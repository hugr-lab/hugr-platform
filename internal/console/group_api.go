package console

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
	"github.com/hugr-lab/hugr-platform/pkg/plan"
)

// The groups the console writes (spec 006): a new group, one field of a group, a group's deletion - each a write of the
// operator's ServiceAccount, judged by the webhook as a kubectl write is, and offered to the cluster bundle.

// GroupTLS is the doors' certificate of a group the console makes: the csi-driver's, from the namespace's Issuer -
// the chart's own groups' (spec 005); none without an issuer.
func GroupTLS(issuer, duration string) v1alpha1.TLSSpec {
	if issuer == "" {
		return v1alpha1.TLSSpec{}
	}
	return v1alpha1.TLSSpec{CSI: &v1alpha1.CSICertificate{IssuerRef: v1alpha1.IssuerRef{Name: issuer, Kind: "Issuer",
		Group: "cert-manager.io"}, Duration: duration}}
}

// GroupCreate is `POST groups`: a group as the person describes it. Its identity is given, never made: the client id
// of an identity the IdP already has, which trusts the group's ServiceAccount (the page shows its subject).
type GroupCreate struct {
	Name          string `json:"name"`
	Size          string `json:"size"`
	Replicas      int32  `json:"replicas"`
	ClientID      string `json:"clientID"`
	Quack         bool   `json:"quack"`
	DrainSeconds  *int32 `json:"drainSeconds,omitempty"`
	ResourceGroup string `json:"resourceGroup,omitempty"`
}

// GroupPatch is `PATCH groups/{g}`: exactly one of a count, a restart, a size, the doors, the drain time.
type GroupPatch struct {
	Replicas     *int32  `json:"replicas,omitempty"`
	Restart      bool    `json:"restart,omitempty"`
	Size         *string `json:"size,omitempty"`
	Quack        *bool   `json:"quack,omitempty"`
	DrainSeconds *int32  `json:"drainSeconds,omitempty"`
}

// fields the patch sets.
func (p GroupPatch) fields() int {
	n := 0
	for _, set := range []bool{p.Replicas != nil, p.Restart, p.Size != nil, p.Quack != nil, p.DrainSeconds != nil} {
		if set {
			n++
		}
	}
	return n
}

// drainSecondsMax bounds a stopping node's wait: past a day a stuck session holds a pod - and its place in the plan.
const drainSecondsMax = 86400

// clientIDPattern is an IdP's client id as the console takes it: printable, no space (a UUID, a name, a URI).
var clientIDPattern = regexp.MustCompile(`^[\x21-\x7e]{1,255}$`)

func decodeStrict(r *http.Request, v any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4<<10))
	decoder.DisallowUnknownFields()
	return decoder.Decode(v)
}

// changedBy names the person on what the console writes - always: an apply of the console's that left the
// annotation out would remove what its owner set before.
func changedBy(p *person) string {
	if p.subject == "" {
		return "unknown"
	}
	return p.subject
}

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request, token string, p *person) {
	var in GroupCreate
	if err := decodeStrict(r, &in); err != nil {
		problem(w, http.StatusBadRequest, "bad_request",
			"send {\"name\", \"size\", \"replicas\", \"clientID\"[, \"quack\", \"drainSeconds\", \"resourceGroup\"]}")
		return
	}
	switch err := plan.CheckName(in.Name); {
	case err != nil:
		problem(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	case in.Size == "":
		problem(w, http.StatusBadRequest, "bad_request", "a group needs a size")
		return
	case in.Replicas < 0:
		problem(w, http.StatusBadRequest, "bad_request", "replicas cannot be negative")
		return
	case !clientIDPattern.MatchString(in.ClientID):
		problem(w, http.StatusBadRequest, "bad_request",
			"a group needs its identity's client id: printable characters, no space, at most 255")
		return
	case in.DrainSeconds != nil && (*in.DrainSeconds < 0 || *in.DrainSeconds > drainSecondsMax):
		problem(w, http.StatusBadRequest, "bad_request", "the drain time is 0 to 86400 seconds")
		return
	case in.ResourceGroup != "" && !nodeproto.ValidPlainName(in.ResourceGroup):
		// the first policy every node's document carries takes plain names only
		problem(w, http.StatusBadRequest, "bad_request", "a resource group's name is letters, digits, '_' and '-', at most 63")
		return
	}
	if s.NewGroupTLS.CSI == nil && s.NewGroupTLS.SecretRef == nil {
		problem(w, http.StatusServiceUnavailable, "no_certificate",
			"this console has no certificate issuer for a new group's doors; the chart names it")
		return
	}
	if !s.rightsNow(w, r, token) {
		return
	}
	platform, err := s.Platform(r.Context())
	if err != nil {
		problem(w, http.StatusServiceUnavailable, "no_platform", "the platform is not read yet")
		return
	}
	group := &v1alpha1.HugrNodeGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: platform.Namespace, Name: in.Name,
			Annotations: map[string]string{ChangedByAnnotation: changedBy(p)}},
		Spec: v1alpha1.HugrNodeGroupSpec{Platform: platform.Name, Replicas: in.Replicas, Size: in.Size,
			ResourceGroup: in.ResourceGroup, Identity: v1alpha1.WorkloadIdentity{ClientID: in.ClientID},
			DrainSeconds: in.DrainSeconds, Doors: v1alpha1.DoorsSpec{Quack: in.Quack}, TLS: *s.NewGroupTLS.DeepCopy()},
	}
	if err := s.Groups.Create(r.Context(), group); err != nil {
		if apierrors.IsAlreadyExists(err) {
			problem(w, http.StatusConflict, "exists", "a group of this name exists in the namespace")
			return
		}
		s.writeProblem(w, err, in.Name, "the group could not be made")
		return
	}
	s.Log.Info("group made", "group", in.Name, "by", p.subject, "size", in.Size, "replicas", in.Replicas)
	w.WriteHeader(http.StatusCreated)
}

// rightsNow reads the person's rights again, past the cache: a group made or deleted is a write of the operator's
// ServiceAccount that nothing else judges by the person, so a right revoked a minute ago does not still make one.
func (s *Server) rightsNow(w http.ResponseWriter, r *http.Request, token string) bool {
	view, err := s.platformView(r.Context())
	if err != nil {
		problem(w, http.StatusServiceUnavailable, "kubernetes", "the platform's groups could not be read")
		return false
	}
	info, err := askRights(r.Context(), s.Nodes, view, token, s.now(), s.random())
	switch {
	case errors.Is(err, errRefused):
		problem(w, http.StatusUnauthorized, "unauthenticated", "the nodes refused your sign-in; sign in again")
		return false
	case err != nil:
		problem(w, http.StatusServiceUnavailable, "no_node", "no node answers, so your rights cannot be read again now")
		return false
	case !RightsOf(info).Scale:
		s.rights.forget(tokenHash(token))
		problem(w, http.StatusForbidden, "forbidden", "your rights do not include this")
		return false
	}
	return true
}

// platformGroup is the request's group if it is this platform's, else a 404.
func (s *Server) platformGroup(w http.ResponseWriter, r *http.Request) (*v1alpha1.HugrPlatform, *v1alpha1.HugrNodeGroup,
	bool) {
	platform, err := s.Platform(r.Context())
	if err != nil {
		problem(w, http.StatusServiceUnavailable, "no_platform", "the platform is not read yet")
		return nil, nil, false
	}
	var group v1alpha1.HugrNodeGroup
	if err := s.Reader.Get(r.Context(), client.ObjectKey{Namespace: platform.Namespace, Name: r.PathValue("group")},
		&group); err != nil || group.Spec.Platform != platform.Name {
		problem(w, http.StatusNotFound, "not_found", "no such group")
		return nil, nil, false
	}
	return platform, &group, true
}

func (s *Server) patchGroup(w http.ResponseWriter, r *http.Request, _ string, p *person) {
	var patch GroupPatch
	if err := decodeStrict(r, &patch); err != nil || patch.fields() != 1 {
		problem(w, http.StatusBadRequest, "bad_request", "send one of {\"replicas\": n}, {\"restart\": true}, "+
			"{\"size\": s}, {\"quack\": bool}, {\"drainSeconds\": n}")
		return
	}
	switch {
	case patch.Replicas != nil && *patch.Replicas < 0:
		problem(w, http.StatusBadRequest, "bad_request", "replicas cannot be negative")
		return
	case patch.Size != nil && *patch.Size == "":
		problem(w, http.StatusBadRequest, "bad_request", "a group needs a size")
		return
	case patch.DrainSeconds != nil && (*patch.DrainSeconds < 0 || *patch.DrainSeconds > drainSecondsMax):
		problem(w, http.StatusBadRequest, "bad_request", "the drain time is 0 to 86400 seconds")
		return
	}
	platform, group, ok := s.platformGroup(w, r)
	if !ok {
		return
	}
	change := GroupChange{Namespace: platform.Namespace, Name: group.Name, ChangedBy: changedBy(p),
		Replicas: patch.Replicas, Quack: patch.Quack, DrainSeconds: patch.DrainSeconds}
	if patch.Size != nil {
		change.Size = *patch.Size
	}
	if patch.Restart {
		change.RestartedAt = s.now().UTC().Format(time.RFC3339)
	}
	if err := s.Groups.Change(r.Context(), change); err != nil {
		s.writeProblem(w, err, group.Name, "the group could not be changed")
		return
	}
	s.Log.Info("group changed", "group", group.Name, "by", p.subject, "field", change.Owner())
	w.WriteHeader(http.StatusNoContent)
}

// deleteGroup deletes the group: its pods drain as they stop. Its resource group stays in the policy - taking it out
// is its own action (spec 006).
func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request, token string, p *person) {
	platform, group, ok := s.platformGroup(w, r)
	if !ok || !s.rightsNow(w, r, token) {
		return
	}
	// the group read here, and no other of its name made since (the UID): its platform was judged on this one
	if err := s.Groups.Delete(r.Context(), platform.Namespace, group.Name, group.UID); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			problem(w, http.StatusNotFound, "not_found", "no such group")
			return
		}
		s.writeProblem(w, err, group.Name, "the group could not be deleted")
		return
	}
	s.Log.Info("group deleted", "group", group.Name, "by", p.subject)
	w.WriteHeader(http.StatusNoContent)
}

// writeProblem answers a failed write of a group: the webhook's reason (past the plan, a size not allowed) is the
// person's to read; the operator's own RBAC refusal is not (it names the ServiceAccount) - it is logged.
func (s *Server) writeProblem(w http.ResponseWriter, err error, group, message string) {
	if apierrors.IsInvalid(err) || (apierrors.IsForbidden(err) || apierrors.IsBadRequest(err)) &&
		strings.Contains(err.Error(), "denied the request") {
		problem(w, http.StatusUnprocessableEntity, "refused", webhookMessage(err))
		return
	}
	s.Log.Info("group not written", "group", group, "error", err.Error())
	problem(w, http.StatusServiceUnavailable, "kubernetes", message)
}
