package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
)

// APIPrefix of the console's API (spec 003).
const APIPrefix = "/api/v1/console/"

// Server is the console's HTTP side: the API under APIPrefix and the web build beside it.
type Server struct {
	// Platform is the HugrPlatform this console serves.
	Platform func(ctx context.Context) (*v1alpha1.HugrPlatform, error)
	// Reader reads the platform's groups (the manager's cache).
	Reader client.Reader
	// Groups writes a group's count and restart (server-side apply as FieldOwner).
	Groups GroupWriter
	// Fleet is the nodes as their EndpointSlices and Leases show them (the front's watcher, spec 002).
	Fleet *fleet.Fleet
	// Nodes runs a statement on a node with the person's token.
	Nodes Caller
	// Web serves the web build; nil serves nothing but the API.
	Web http.Handler
	Log logr.Logger
	// Now is time.Now unless a test sets it.
	Now func() time.Time

	rights rightsCache
	rndMu  sync.Mutex
	rnd    *rand.Rand
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) random() *rand.Rand {
	s.rndMu.Lock()
	defer s.rndMu.Unlock()
	if s.rnd == nil {
		s.rnd = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}
	return rand.New(rand.NewPCG(s.rnd.Uint64(), s.rnd.Uint64()))
}

func (s *Server) view() *fleet.View {
	if s.Fleet == nil {
		return nil
	}
	v, _ := s.Fleet.View()
	return v
}

// Handler of the console: the API, then the web build.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+APIPrefix+"config", s.config)
	mux.Handle("GET "+APIPrefix+"me", s.person(nil, s.me))
	mux.Handle("GET "+APIPrefix+"groups", s.person(func(r Rights) bool { return r.Fleet }, s.groups))
	mux.Handle("PATCH "+APIPrefix+"groups/{group}", s.person(func(r Rights) bool { return r.Scale }, s.patchGroup))
	mux.Handle("GET "+APIPrefix+"nodes/{label}", s.person(func(r Rights) bool { return r.Fleet }, s.node))
	mux.Handle("GET "+APIPrefix+"nodes/{label}/sessions",
		s.person(func(r Rights) bool { return r.Sessions }, s.sessions))
	mux.Handle("POST "+APIPrefix+"nodes/{label}/sessions/{id}/end",
		s.person(func(r Rights) bool { return r.EndSession }, s.endSession))
	mux.Handle("POST "+APIPrefix+"nodes/{label}/drain",
		s.person(func(r Rights) bool { return r.Drain }, s.action(stmtDrain)))
	mux.Handle("POST "+APIPrefix+"nodes/{label}/resume",
		s.person(func(r Rights) bool { return r.Resume }, s.action(stmtResume)))
	mux.Handle("POST "+APIPrefix+"sign-out", s.bearer(s.signOut))
	mux.HandleFunc(APIPrefix, func(w http.ResponseWriter, _ *http.Request) {
		problem(w, http.StatusNotFound, "not_found", "no such route")
	})
	if s.Web != nil {
		mux.Handle("/", s.Web)
	}
	return secured(mux)
}

// secured adds the headers every answer carries; the API's are never cached.
func secured(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, APIPrefix) {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// problem answers an error as JSON: a fixed code and a message for the person.
func problem(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message})
}

func answer(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// bearer of the request, or a 401.
func bearer(r *http.Request) (string, bool) {
	value := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}

type personHandler func(w http.ResponseWriter, r *http.Request, token string, p *person)

func (s *Server) bearer(next func(w http.ResponseWriter, r *http.Request, token string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearer(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			problem(w, http.StatusUnauthorized, "unauthenticated", "sign in")
			return
		}
		next(w, r, token)
	})
}

// person resolves the token's rights (cached by its hash) and lets the request through when `allowed` says so.
func (s *Server) person(allowed func(Rights) bool, next personHandler) http.Handler {
	return s.bearer(func(w http.ResponseWriter, r *http.Request, token string) {
		p, err := s.lookup(r.Context(), token)
		switch {
		case errors.Is(err, errRefused):
			w.Header().Set("WWW-Authenticate", "Bearer")
			problem(w, http.StatusUnauthorized, "unauthenticated", "the nodes refused your sign-in; sign in again")
			return
		case errors.Is(err, errNoNode):
			problem(w, http.StatusServiceUnavailable, "no_node",
				"no node answers, so the console cannot learn what you may see; the cluster's state is in Kubernetes")
			return
		case err != nil:
			s.Log.Info("rights not read", "error", err.Error())
			problem(w, http.StatusBadGateway, "node_error", "the node did not answer as expected")
			return
		}
		if allowed != nil && !allowed(p.rights) {
			problem(w, http.StatusForbidden, "forbidden", "your rights do not include this")
			return
		}
		next(w, r, token, p)
	})
}

func (s *Server) lookup(ctx context.Context, token string) (*person, error) {
	hash := tokenHash(token)
	now := s.now()
	if p, ok := s.rights.get(hash, now); ok {
		return p, nil
	}
	info, err := askRights(ctx, s.Nodes, s.view(), token, now, s.random())
	if err != nil {
		return nil, err
	}
	p := &person{rights: RightsOf(info), subject: subjectOf(token), until: now.Add(rightsTTL), used: now}
	s.rights.put(hash, p, now)
	return p, nil
}

// ConfigAnswer is what the browser needs before the sign-in: nothing in it is a secret.
type ConfigAnswer struct {
	Platform  string                    `json:"platform"`
	Issuer    string                    `json:"issuer"`
	ClientID  string                    `json:"clientID"`
	Scopes    []string                  `json:"scopes"`
	Audiences v1alpha1.ConsoleAudiences `json:"audiences"`
	Mounts    map[string]MountAnswer    `json:"mounts"`
	Links     []v1alpha1.ConsoleLink    `json:"links"`
}

// MountAnswer is where a mounted console is served from (same origin, spec 003).
type MountAnswer struct {
	Base     string `json:"base"`
	Audience string `json:"audience,omitempty"`
	Tenant   string `json:"tenant,omitempty"`
}

func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	platform, err := s.Platform(r.Context())
	if err != nil || platform.Spec.Console == nil {
		problem(w, http.StatusNotFound, "no_console", "this platform has no console configured")
		return
	}
	c := platform.Spec.Console
	out := ConfigAnswer{Platform: platform.Name, Issuer: platform.Spec.IdP.Issuer, ClientID: c.ClientID,
		Scopes: append([]string{}, c.Scopes...), Audiences: c.Audiences, Mounts: map[string]MountAnswer{},
		Links: append([]v1alpha1.ConsoleLink{}, c.Links...)}
	if c.Audiences.Tresor != "" {
		out.Mounts["tresor"] = MountAnswer{Base: "/tresor", Audience: c.Audiences.Tresor}
	}
	if c.Audiences.Kista != "" {
		out.Mounts["kista"] = MountAnswer{Base: "/kista", Audience: c.Audiences.Kista, Tenant: kistaTenant(platform)}
	}
	answer(w, out)
}

// kistaTenant the repository's prefix names: https://<host>/<tenant>/... (kista's channel URLs), empty when none.
func kistaTenant(platform *v1alpha1.HugrPlatform) string {
	u, err := url.Parse(platform.Spec.Repository.Prefix)
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(strings.Trim(u.Path, "/"), "/")
	return first
}

// MeAnswer is the person's rights.
type MeAnswer struct {
	Rights Rights `json:"rights"`
}

func (s *Server) me(w http.ResponseWriter, _ *http.Request, _ string, p *person) {
	answer(w, MeAnswer{Rights: p.rights})
}

func (s *Server) groups(w http.ResponseWriter, r *http.Request, _ string, _ *person) {
	platform, err := s.Platform(r.Context())
	if err != nil {
		problem(w, http.StatusServiceUnavailable, "no_platform", "the platform is not read yet")
		return
	}
	var list v1alpha1.HugrNodeGroupList
	if err := s.Reader.List(r.Context(), &list, client.InNamespace(platform.Namespace)); err != nil {
		problem(w, http.StatusServiceUnavailable, "kubernetes", "the groups could not be read")
		return
	}
	answer(w, FleetOf(platform, list.Items, s.view(), s.now()))
}

// GroupPatch is `PATCH groups/{g}`: one of a count or a restart.
type GroupPatch struct {
	Replicas *int32 `json:"replicas,omitempty"`
	Restart  bool   `json:"restart,omitempty"`
}

func (s *Server) patchGroup(w http.ResponseWriter, r *http.Request, _ string, p *person) {
	var patch GroupPatch
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&patch); err != nil || (patch.Replicas == nil) == !patch.Restart {
		problem(w, http.StatusBadRequest, "bad_request", "send {\"replicas\": n} or {\"restart\": true}")
		return
	}
	if patch.Replicas != nil && *patch.Replicas < 0 {
		problem(w, http.StatusBadRequest, "bad_request", "replicas cannot be negative")
		return
	}
	platform, err := s.Platform(r.Context())
	if err != nil {
		problem(w, http.StatusServiceUnavailable, "no_platform", "the platform is not read yet")
		return
	}
	name := r.PathValue("group")
	var group v1alpha1.HugrNodeGroup
	if err := s.Reader.Get(r.Context(), client.ObjectKey{Namespace: platform.Namespace, Name: name}, &group); err != nil ||
		group.Spec.Platform != platform.Name {
		problem(w, http.StatusNotFound, "not_found", "no such group")
		return
	}
	// always named: an apply of the console's that left the annotation out would remove what its owner set before
	changedBy := p.subject
	if changedBy == "" {
		changedBy = "unknown"
	}
	change := GroupChange{Namespace: platform.Namespace, Name: name, ChangedBy: changedBy, Replicas: patch.Replicas}
	if patch.Restart {
		change.RestartedAt = s.now().UTC().Format(time.RFC3339)
	}
	if err := s.Groups.Change(r.Context(), change); err != nil {
		if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apierrors.IsBadRequest(err) {
			// the webhook's reason (past the plan, a size not allowed) is the person's to read
			problem(w, http.StatusUnprocessableEntity, "refused", webhookMessage(err))
			return
		}
		s.Log.Info("group not changed", "group", name, "error", err.Error())
		problem(w, http.StatusServiceUnavailable, "kubernetes", "the group could not be changed")
		return
	}
	s.Log.Info("group changed", "group", name, "by", p.subject, "replicas", patch.Replicas, "restart", patch.Restart)
	w.WriteHeader(http.StatusNoContent)
}

// webhookMessage is the admission's reason without the API server's framing.
func webhookMessage(err error) string {
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		if m := status.Status().Message; m != "" {
			if _, reason, ok := strings.Cut(m, "denied the request: "); ok {
				return reason
			}
			return m
		}
	}
	return "the change was refused"
}

// nodeOf the request's label in the fleet, or a 404.
func (s *Server) nodeOf(w http.ResponseWriter, r *http.Request) (*fleet.Node, bool) {
	v := s.view()
	label := r.PathValue("label")
	if v == nil || v.Node(label) == nil {
		problem(w, http.StatusNotFound, "not_found", "no such node")
		return nil, false
	}
	return v.Node(label), true
}

// NodeAnswerFull is `GET nodes/{label}`: Kubernetes' part always, the node's own when it answers.
type NodeAnswerFull struct {
	Kubernetes NodeAnswer     `json:"kubernetes"`
	Group      string         `json:"group"`
	Load       map[string]any `json:"load"`
	LoadError  string         `json:"loadError,omitempty"`
}

func (s *Server) node(w http.ResponseWriter, r *http.Request, token string, _ *person) {
	n, ok := s.nodeOf(w, r)
	if !ok {
		return
	}
	out := NodeAnswerFull{Kubernetes: nodeAnswer(n, s.now()), Group: groupOf(n)}
	result, err := s.Nodes.Query(r.Context(), *n, token, stmtLoad)
	switch {
	case err != nil:
		out.LoadError = personMessage(err)
	case len(result.Rows) == 1:
		out.Load = result.Records()[0]
	default:
		out.LoadError = "the node answered no load report"
	}
	answer(w, out)
}

func (s *Server) sessions(w http.ResponseWriter, r *http.Request, token string, _ *person) {
	n, ok := s.nodeOf(w, r)
	if !ok {
		return
	}
	result, err := s.Nodes.Query(r.Context(), *n, token, stmtSessions)
	if err != nil {
		nodeProblem(w, err)
		return
	}
	answer(w, map[string]any{"sessions": result.Records()})
}

func (s *Server) endSession(w http.ResponseWriter, r *http.Request, token string, p *person) {
	statement, err := killSession(r.PathValue("id"))
	if err != nil {
		problem(w, http.StatusBadRequest, "bad_request", "not a session id")
		return
	}
	s.run(w, r, token, p, statement)
}

func (s *Server) action(statement string) personHandler {
	return func(w http.ResponseWriter, r *http.Request, token string, p *person) {
		s.run(w, r, token, p, statement)
	}
}

func (s *Server) run(w http.ResponseWriter, r *http.Request, token string, p *person, statement string) {
	n, ok := s.nodeOf(w, r)
	if !ok {
		return
	}
	result, err := s.Nodes.Query(r.Context(), *n, token, statement)
	if err != nil {
		nodeProblem(w, err)
		return
	}
	s.Log.Info("node action", "node", n.Label, "by", p.subject, "action", strings.Fields(statement)[0])
	answer(w, map[string]any{"result": result.Records()})
}

func (s *Server) signOut(w http.ResponseWriter, r *http.Request, token string) {
	s.rights.forget(tokenHash(token))
	closed := s.Nodes.SignOut(r.Context(), token)
	answer(w, map[string]int{"closed": closed})
}

// personMessage is what the person reads of a failed node call.
func personMessage(err error) string {
	var refused *NodeError
	if errors.As(err, &refused) {
		return refused.Message
	}
	return "the node does not answer"
}

// nodeProblem answers a failed node call: the node's message, never more.
func nodeProblem(w http.ResponseWriter, err error) {
	var refused *NodeError
	if !errors.As(err, &refused) {
		problem(w, http.StatusServiceUnavailable, "node_unreachable", "the node does not answer")
		return
	}
	status := http.StatusConflict
	switch refused.Code {
	case codes.Unauthenticated:
		status = http.StatusUnauthorized
	case codes.PermissionDenied:
		status = http.StatusForbidden
	case codes.InvalidArgument, codes.NotFound:
		status = http.StatusBadRequest
	case codes.Unavailable:
		status = http.StatusServiceUnavailable
	}
	problem(w, status, "node_refused", refused.Message)
}
