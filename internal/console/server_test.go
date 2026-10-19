package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

var testNow = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// a token whose payload says sub "ops@example.org"
const opsToken = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJvcHNAZXhhbXBsZS5vcmcifQ.c2ln"

type call struct{ label, token, statement string }

type fakeNodes struct {
	mu        sync.Mutex
	calls     []call
	answer    func(node fleet.Node, statement string) (*Result, error)
	signedOut []string
}

func (f *fakeNodes) Query(_ context.Context, node fleet.Node, token, statement string) (*Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{node.Label, token, statement})
	f.mu.Unlock()
	return f.answer(node, statement)
}

func (f *fakeNodes) SignOut(_ context.Context, token string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signedOut = append(f.signedOut, token)
	return 1
}

func (f *fakeNodes) statements(statement string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.statement == statement {
			n++
		}
	}
	return n
}

type fakeWriter struct {
	mu      sync.Mutex
	changes []GroupChange
	err     error
}

func (w *fakeWriter) Change(_ context.Context, change GroupChange) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.changes = append(w.changes, change)
	return w.err
}

// infoRow is console_info's answer for the bundles and names.
func infoRow(bundles, views, functions []string) *Result {
	list := func(items []string) []any {
		out := []any{}
		for _, i := range items {
			out = append(out, i)
		}
		return out
	}
	return &Result{Columns: []string{"build", "bundles", "my_views", "my_functions", "deployment"},
		Rows: [][]any{{"dev", list(bundles), list(views), list(functions), "cluster"}}}
}

var (
	observeInfo = infoRow([]string{"observe"}, []string{"node_load", "sessions"}, nil)
	operateInfo = infoRow([]string{"observe", "operate"}, []string{"node_load", "sessions"},
		[]string{"kill_session", "drain", "resume"})
	clusterInfo = infoRow([]string{"observe", "operate", "cluster"}, []string{"node_load", "sessions"},
		[]string{"kill_session", "drain", "resume"})
)

func node(label, group string) fleet.Node {
	return fleet.Node{Label: label, Service: "hugr-node-" + group, Address: "10.0.0.1", FlightPort: 32010,
		Ready: true, Serving: true, Lease: &fleet.Lease{Renewed: testNow.Add(-4 * time.Second),
			Until: testNow.Add(time.Minute), Summary: nodeproto.Summary{Group: group, AdmitSession: true,
				Sessions: 3, SeatsLeft: 9}}}
}

type harness struct {
	server *Server
	nodes  *fakeNodes
	writer *fakeWriter
	http   http.Handler
}

func newHarness(t *testing.T, info *Result, nodes ...fleet.Node) *harness {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	platform := &v1alpha1.HugrPlatform{ObjectMeta: metav1.ObjectMeta{Name: "hugr", Namespace: "hugr"},
		Spec: v1alpha1.HugrPlatformSpec{IdP: v1alpha1.IdPSpec{Issuer: "https://login.example.org"},
			Repository: v1alpha1.RepositorySpec{Prefix: "https://kista.example.org/corp/"},
			Plan:       v1alpha1.PlanSpec{MaxNodes: 12, Sizes: []string{"small"}},
			Console: &v1alpha1.ConsoleSpec{ClientID: "console", Audiences: v1alpha1.ConsoleAudiences{
				Nodes: "hugr-nodes", Tresor: "duckdb-secrets", Kista: "kista"}}}}
	group := func(name, platform string) *v1alpha1.HugrNodeGroup {
		return &v1alpha1.HugrNodeGroup{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "hugr"},
			Spec: v1alpha1.HugrNodeGroupSpec{Platform: platform, Replicas: 2, Size: "small"}}
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(platform, group("etl", "hugr"), group("general", "hugr"), group("theirs", "other")).Build()
	f := fleet.New()
	f.Set(fleet.NewView(nodes))
	fn := &fakeNodes{answer: func(_ fleet.Node, statement string) (*Result, error) {
		switch statement {
		case stmtInfo:
			return info, nil
		case stmtLoad:
			return &Result{Columns: []string{"draining", "sessions_live"}, Rows: [][]any{{false, int64(3)}}}, nil
		case stmtSessions:
			return &Result{Columns: []string{"id", "subject"}, Rows: [][]any{{"0A1B2C3D4E5F", "svc@example.org"}}}, nil
		}
		if strings.HasPrefix(statement, "KILL SESSION '0A1B2C3D4E5F'") {
			return &Result{Columns: []string{"kill_session"}, Rows: [][]any{{true}}}, nil
		}
		if strings.HasPrefix(statement, "KILL SESSION") {
			return &Result{Columns: []string{"kill_session"}, Rows: [][]any{{false}}}, nil
		}
		return &Result{Columns: []string{"ok"}, Rows: [][]any{{true}}}, nil
	}}
	w := &fakeWriter{}
	s := &Server{
		Platform: PlatformFinder{Reader: reader, Namespace: "hugr"}.Find,
		Reader:   reader, Groups: w, Fleet: f, Nodes: fn, Log: logr.Discard(),
		Now: func() time.Time { return testNow },
	}
	return &harness{server: s, nodes: fn, writer: w, http: s.Handler()}
}

func (h *harness) do(t *testing.T, method, path, token, body string) (int, map[string]any, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, reader)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.http.ServeHTTP(w, r)
	raw := w.Body.String()
	var out map[string]any
	_ = json.Unmarshal([]byte(raw), &out)
	if token != "" && strings.Contains(raw, token) {
		t.Errorf("%s %s: the answer carries the token", method, path)
	}
	return w.Code, out, raw
}

func TestConfig(t *testing.T) {
	h := newHarness(t, observeInfo, node("hugr-node-etl-a", "etl"))
	code, out, _ := h.do(t, "GET", APIPrefix+"config", "", "")
	if code != 200 || out["issuer"] != "https://login.example.org" || out["clientID"] != "console" {
		t.Fatalf("the sign-in's config, unauthenticated: %d %v", code, out)
	}
	mounts := out["mounts"].(map[string]any)
	if mounts["kista"].(map[string]any)["tenant"] != "corp" || mounts["tresor"].(map[string]any)["base"] != "/tresor" {
		t.Errorf("the mounts, the tenant the repository's prefix names: %v", mounts)
	}
	if len(h.nodes.calls) != 0 {
		t.Error("the config asks no node")
	}
}

func TestBearerRequired(t *testing.T) {
	h := newHarness(t, clusterInfo, node("hugr-node-etl-a", "etl"))
	for _, route := range [][2]string{{"GET", "me"}, {"GET", "groups"}, {"PATCH", "groups/etl"},
		{"GET", "nodes/hugr-node-etl-a"}, {"POST", "nodes/hugr-node-etl-a/drain"}, {"POST", "sign-out"}} {
		code, _, _ := h.do(t, route[0], APIPrefix+route[1], "", "")
		if code != http.StatusUnauthorized {
			t.Errorf("%s %s without a bearer: %d", route[0], route[1], code)
		}
	}
}

func TestRightsGateEveryRoute(t *testing.T) {
	routes := []struct {
		method, path, body string
		needs              *Result // the least answer that allows it
	}{
		{"GET", "groups", "", observeInfo},
		{"GET", "nodes/hugr-node-etl-a", "", observeInfo},
		{"GET", "nodes/hugr-node-etl-a/sessions", "", observeInfo},
		{"POST", "nodes/hugr-node-etl-a/sessions/0A1B2C3D4E5F/end", "", operateInfo},
		{"POST", "nodes/hugr-node-etl-a/drain", "", operateInfo},
		{"POST", "nodes/hugr-node-etl-a/resume", "", operateInfo},
		{"PATCH", "groups/etl", `{"replicas": 3}`, clusterInfo},
	}
	nothing := infoRow(nil, nil, nil)
	for _, route := range routes {
		for _, info := range []*Result{nothing, observeInfo, operateInfo, clusterInfo} {
			h := newHarness(t, info, node("hugr-node-etl-a", "etl"))
			code, _, _ := h.do(t, route.method, APIPrefix+route.path, opsToken, route.body)
			allowed := rank(info) >= rank(route.needs)
			if allowed && code >= 400 {
				t.Errorf("%s %s with %v: %d, want allowed", route.method, route.path, info.Rows[0][1], code)
			}
			if !allowed && code != http.StatusForbidden {
				t.Errorf("%s %s with %v: %d, want 403", route.method, route.path, info.Rows[0][1], code)
			}
			if !allowed && len(h.writer.changes) != 0 {
				t.Errorf("%s %s refused yet written", route.method, route.path)
			}
		}
	}
}

func rank(info *Result) int {
	return map[*Result]int{observeInfo: 1, operateInfo: 2, clusterInfo: 3}[info]
}

func TestNoNodeFailsClosed(t *testing.T) {
	h := newHarness(t, clusterInfo) // no node at all
	code, out, _ := h.do(t, "GET", APIPrefix+"me", opsToken, "")
	if code != http.StatusServiceUnavailable || out["error"] != "no_node" {
		t.Fatalf("no node: %d %v", code, out)
	}
	unreachable := newHarness(t, clusterInfo, node("hugr-node-etl-a", "etl"), node("hugr-node-etl-b", "etl"))
	unreachable.nodes.answer = func(fleet.Node, string) (*Result, error) { return nil, errUnreachable }
	code, out, _ = unreachable.do(t, "GET", APIPrefix+"groups", opsToken, "")
	if code != http.StatusServiceUnavailable || out["error"] != "no_node" {
		t.Fatalf("every node unreachable: %d %v", code, out)
	}
	expired := node("hugr-node-etl-a", "etl")
	expired.Lease.Until = testNow.Add(-time.Second)
	stale := newHarness(t, clusterInfo, expired)
	if code, _, _ := stale.do(t, "GET", APIPrefix+"me", opsToken, ""); code != http.StatusServiceUnavailable {
		t.Errorf("a node whose Lease expired is not asked: %d", code)
	}
}

func TestRightsCached(t *testing.T) {
	n := node("hugr-node-etl-a", "etl")
	n.Lease.Until = testNow.Add(time.Hour)
	h := newHarness(t, observeInfo, n)
	for i := 0; i < 3; i++ {
		if code, _, _ := h.do(t, "GET", APIPrefix+"me", opsToken, ""); code != 200 {
			t.Fatalf("me: %d", code)
		}
	}
	if n := h.nodes.statements(stmtInfo); n != 1 {
		t.Errorf("console_info asked %d times in the TTL", n)
	}
	h.server.Now = func() time.Time { return testNow.Add(rightsTTL + time.Second) }
	h.do(t, "GET", APIPrefix+"me", opsToken, "")
	if n := h.nodes.statements(stmtInfo); n != 2 {
		t.Errorf("past the TTL the rights are asked again: %d", n)
	}
}

func TestRefusedToken(t *testing.T) {
	h := newHarness(t, observeInfo, node("hugr-node-etl-a", "etl"))
	h.nodes.answer = func(fleet.Node, string) (*Result, error) {
		return nil, &NodeError{Code: codes.Unauthenticated, Message: "acl: token rejected"}
	}
	code, _, _ := h.do(t, "GET", APIPrefix+"me", opsToken, "")
	if code != http.StatusUnauthorized {
		t.Errorf("a token the nodes refuse is the sign-in's problem: %d", code)
	}
}

func TestNoAdministration(t *testing.T) {
	h := newHarness(t, observeInfo, node("hugr-node-etl-a", "etl"))
	h.nodes.answer = func(fleet.Node, string) (*Result, error) {
		return nil, &NodeError{Code: codes.InvalidArgument,
			Message: `acl: Binder Error: acl_rewrite: no access to table function "platform.console_info"`}
	}
	code, out, _ := h.do(t, "GET", APIPrefix+"me", opsToken, "")
	if code != 200 || out["rights"].(map[string]any)["fleet"] != false {
		t.Fatalf("a principal the node refuses console_info has no rights, and sees nothing: %d %v", code, out)
	}
	if code, _, _ := h.do(t, "GET", APIPrefix+"groups", opsToken, ""); code != http.StatusForbidden {
		t.Errorf("and every page is refused: %d", code)
	}
}

func TestRightsFromThePersonsGroup(t *testing.T) {
	h := newHarness(t, observeInfo, node("hugr-node-etl-a", "etl"), node("hugr-node-general-a", "general"))
	h.nodes.answer = func(n fleet.Node, statement string) (*Result, error) {
		if n.Group() != "general" {
			return nil, &NodeError{Code: codes.Unavailable, Message: "acl: wrong resource group",
				Groups: []string{"general"}}
		}
		return observeInfo, nil
	}
	if code, _, _ := h.do(t, "GET", APIPrefix+"me", opsToken, ""); code != 200 {
		t.Fatalf("the rights read on a node of the group the refusal named: %d", code)
	}
}

func TestEndSessionChecksTheID(t *testing.T) {
	h := newHarness(t, operateInfo, node("hugr-node-etl-a", "etl"))
	code, _, _ := h.do(t, "POST", APIPrefix+"nodes/hugr-node-etl-a/sessions/x'%3B%20DROP/end", opsToken, "")
	if code != http.StatusBadRequest {
		t.Errorf("an id of another form: %d", code)
	}
	for _, c := range h.nodes.calls {
		if strings.Contains(c.statement, "DROP") {
			t.Fatalf("the browser's text reached a node: %q", c.statement)
		}
	}
	code, _, _ = h.do(t, "POST", APIPrefix+"nodes/hugr-node-etl-a/sessions/0a1b2c3d4e5f/end", opsToken, "")
	if code != 200 || h.nodes.statements("KILL SESSION '0A1B2C3D4E5F'") != 1 {
		t.Errorf("an ops id ends the session: %d %v", code, h.nodes.calls)
	}
	// the node answers false for a session it does not hold: a 404, never a success
	code, out, _ := h.do(t, "POST", APIPrefix+"nodes/hugr-node-etl-a/sessions/FFFFFFFFFFFF/end", opsToken, "")
	if code != http.StatusNotFound || out["error"] != "not_found" {
		t.Errorf("no such session on the node: %d %v", code, out)
	}
	for _, c := range h.nodes.calls {
		if c.token != opsToken || c.label != "hugr-node-etl-a" {
			t.Errorf("every call carries the person's token to the node named: %+v", c)
		}
	}
}

func TestNodeRefusalPassedThrough(t *testing.T) {
	h := newHarness(t, operateInfo, node("hugr-node-etl-a", "etl"))
	h.nodes.answer = func(_ fleet.Node, statement string) (*Result, error) {
		if statement == stmtInfo {
			return operateInfo, nil
		}
		return nil, &NodeError{Code: codes.PermissionDenied, Message: "acl: not your operation"}
	}
	code, out, _ := h.do(t, "POST", APIPrefix+"nodes/hugr-node-etl-a/drain", opsToken, "")
	if code != http.StatusForbidden || out["message"] != "acl: not your operation" {
		t.Errorf("the node judges again, its message reaches the person: %d %v", code, out)
	}
	if code, _, _ := h.do(t, "POST", APIPrefix+"nodes/nobody/drain", opsToken, ""); code != http.StatusNotFound {
		t.Errorf("a node the fleet does not know: %d", code)
	}
}

func TestNodePageWhenTheNodeDoesNotAnswer(t *testing.T) {
	h := newHarness(t, observeInfo, node("hugr-node-etl-a", "etl"), node("hugr-node-etl-b", "etl"))
	h.nodes.answer = func(n fleet.Node, statement string) (*Result, error) {
		if n.Label == "hugr-node-etl-b" && statement == stmtLoad {
			return nil, errUnreachable
		}
		if statement == stmtInfo {
			return observeInfo, nil
		}
		return &Result{Columns: []string{"draining"}, Rows: [][]any{{false}}}, nil
	}
	h.server.rights.put(tokenHash(opsToken), &person{rights: RightsOf(Info{MyViews: []string{"node_load"}}),
		until: testNow.Add(time.Minute)}, testNow)
	code, out, _ := h.do(t, "GET", APIPrefix+"nodes/hugr-node-etl-b", opsToken, "")
	if code != 200 || out["loadError"] != "the node does not answer" || out["kubernetes"] == nil {
		t.Errorf("Kubernetes' part stays, the node's says it does not answer: %d %v", code, out)
	}
}

func TestPatchGroup(t *testing.T) {
	h := newHarness(t, clusterInfo, node("hugr-node-etl-a", "etl"))
	for _, body := range []string{`{}`, `{"replicas": 3, "restart": true}`, `{"replicas": -1}`, `{"size": "large"}`,
		`not json`} {
		if code, _, _ := h.do(t, "PATCH", APIPrefix+"groups/etl", opsToken, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, code)
		}
	}
	if code, _, _ := h.do(t, "PATCH", APIPrefix+"groups/theirs", opsToken, `{"replicas": 3}`); code != 404 {
		t.Errorf("another platform's group: %d", code)
	}
	if code, _, _ := h.do(t, "PATCH", APIPrefix+"groups/nobody", opsToken, `{"replicas": 3}`); code != 404 {
		t.Errorf("no such group: %d", code)
	}
	if len(h.writer.changes) != 0 {
		t.Fatalf("nothing refused is written: %+v", h.writer.changes)
	}
	if code, _, _ := h.do(t, "PATCH", APIPrefix+"groups/etl", opsToken, `{"replicas": 3}`); code != 204 {
		t.Fatalf("scale: %d", code)
	}
	if code, _, _ := h.do(t, "PATCH", APIPrefix+"groups/etl", opsToken, `{"restart": true}`); code != 204 {
		t.Fatalf("restart: %d", code)
	}
	scale, restart := h.writer.changes[0], h.writer.changes[1]
	if *scale.Replicas != 3 || scale.RestartedAt != "" || scale.ChangedBy != "ops@example.org" ||
		scale.Name != "etl" || scale.Namespace != "hugr" {
		t.Errorf("scale: %+v", scale)
	}
	if restart.Replicas != nil || restart.RestartedAt != "2026-10-19T12:00:00Z" {
		t.Errorf("restart: %+v", restart)
	}
}

func TestGroupsAnswer(t *testing.T) {
	h := newHarness(t, observeInfo, node("hugr-node-etl-b", "etl"), node("hugr-node-etl-a", "etl"),
		node("hugr-node-general-a", "general"))
	code, out, _ := h.do(t, "GET", APIPrefix+"groups", opsToken, "")
	if code != 200 {
		t.Fatalf("groups: %d", code)
	}
	groups := out["groups"].([]any)
	if len(groups) != 2 {
		t.Fatalf("this platform's groups only: %v", groups)
	}
	etl := groups[0].(map[string]any)
	nodes := etl["nodes"].([]any)
	if etl["name"] != "etl" || len(nodes) != 2 || nodes[0].(map[string]any)["label"] != "hugr-node-etl-a" ||
		nodes[0].(map[string]any)["leaseAge"] != float64(4) || etl["scaling"].(map[string]any)["mode"] != "manual" {
		t.Errorf("a group's nodes by its Service, in order, with the Lease's age: %v", etl)
	}
}

func TestSignOut(t *testing.T) {
	h := newHarness(t, observeInfo, node("hugr-node-etl-a", "etl"))
	h.do(t, "GET", APIPrefix+"me", opsToken, "")
	if code, _, _ := h.do(t, "POST", APIPrefix+"sign-out", opsToken, ""); code != 200 {
		t.Fatalf("sign-out: %d", code)
	}
	if len(h.nodes.signedOut) != 1 {
		t.Errorf("the pool closes the person's sessions")
	}
	if _, ok := h.server.rights.get(tokenHash(opsToken), testNow); ok {
		t.Error("the rights of a token signed out are forgotten")
	}
}
