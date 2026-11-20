package console

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

var testNow = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// a token of the IdP's naming "ops@example.org", an hour from testNow
var opsToken = tokenOf(testIssuer, "ops@example.org", testNow.Add(time.Hour))

const testIssuer = "https://login.example.org"

// tokenOf is a JWT's shape with these claims: the console reads them, the node (here a fake) verifies
func tokenOf(iss, sub string, exp time.Time) string {
	claims, _ := json.Marshal(map[string]any{"iss": iss, "sub": sub, "exp": exp.Unix()})
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(claims) + ".c2ln"
}

type call struct{ label, token, statement string }

type fakeNodes struct {
	mu     sync.Mutex
	calls  []call
	answer func(node fleet.Node, statement string) (*Result, error)
}

func (f *fakeNodes) Query(_ context.Context, node fleet.Node, token, statement string) (*Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{node.Label, token, statement})
	f.mu.Unlock()
	if node.FlightPort == 0 {
		return nil, errUnreachable // as the Pool: a node with no door is dialled nowhere
	}
	return f.answer(node, statement)
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
	created []*v1alpha1.HugrNodeGroup
	deleted []string
	err     error
}

func (w *fakeWriter) Change(_ context.Context, change GroupChange) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.changes = append(w.changes, change)
	return w.err
}

func (w *fakeWriter) Create(_ context.Context, group *v1alpha1.HugrNodeGroup) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.created = append(w.created, group)
	return w.err
}

func (w *fakeWriter) Delete(_ context.Context, namespace, name string, _ types.UID) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deleted = append(w.deleted, namespace+"/"+name)
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
	kube   client.Client // the platform and its groups, as a test changes them
}

func newHarness(t *testing.T, info *Result, nodes ...fleet.Node) *harness {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	platform := &v1alpha1.HugrPlatform{ObjectMeta: metav1.ObjectMeta{Name: "hugr", Namespace: "hugr"},
		Spec: v1alpha1.HugrPlatformSpec{IdP: v1alpha1.IdPSpec{Issuer: "https://login.example.org"},
			Repository: v1alpha1.RepositorySpec{Prefix: "https://kista.example.org/ext/corp/prod"},
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
			return said(true, nil), nil
		}
		if strings.HasPrefix(statement, "KILL SESSION") {
			return said(false, "no session has that id"), nil
		}
		if statement == stmtDrain {
			return said(true, "3 sessions still open"), nil
		}
		if statement == stmtResume {
			return said(false, "the node was not draining"), nil
		}
		return said(true, nil), nil
	}}
	w := &fakeWriter{}
	s := &Server{
		Platform: PlatformFinder{Reader: reader, Namespace: "hugr"}.Find,
		Reader:   reader, Groups: w, Fleet: f, Nodes: fn, Log: logr.Discard(),
		Now: func() time.Time { return testNow },
	}
	return &harness{server: s, nodes: fn, writer: w, http: s.Handler(), kube: reader}
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
		{"GET", "groups/etl/bootstrap", "", observeInfo},
		{"GET", "nodes/hugr-node-etl-a", "", observeInfo},
		{"GET", "nodes/hugr-node-etl-a/sessions", "", observeInfo},
		{"POST", "nodes/hugr-node-etl-a/sessions/0A1B2C3D4E5F/end", "", operateInfo},
		{"POST", "nodes/hugr-node-etl-a/drain", "", operateInfo},
		{"POST", "nodes/hugr-node-etl-a/resume", "", operateInfo},
		{"PATCH", "groups/etl", `{"replicas": 3}`, clusterInfo},
		{"POST", "cluster/profile", `{"action": "reset", "setting": "threads"}`, clusterInfo},
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

// TestDrainAndResumeAnswer: the node's answer reaches the page as it is - drain's count, resume's not ok.
func TestDrainAndResumeAnswer(t *testing.T) {
	h := newHarness(t, operateInfo, node("hugr-node-etl-a", "etl"))
	code, out, _ := h.do(t, "POST", APIPrefix+"nodes/hugr-node-etl-a/drain", opsToken, "")
	if code != 200 || out["ok"] != true || out["message"] != "3 sessions still open" {
		t.Errorf("drain: %d %v", code, out)
	}
	code, out, _ = h.do(t, "POST", APIPrefix+"nodes/hugr-node-etl-a/resume", opsToken, "")
	if code != 200 || out["ok"] != false || out["message"] != "the node was not draining" {
		t.Errorf("resume: %d %v", code, out)
	}
	// an answer of another shape is said as such, never read as done
	h.nodes.answer = func(_ fleet.Node, statement string) (*Result, error) {
		if statement == stmtInfo {
			return operateInfo, nil
		}
		return &Result{Columns: []string{"acl_drain"}, Rows: [][]any{{int64(3)}}}, nil
	}
	if code, _, _ := h.do(t, "POST", APIPrefix+"nodes/hugr-node-etl-a/drain", opsToken, ""); code != http.StatusBadGateway {
		t.Errorf("an old answer: %d", code)
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
	for _, body := range []string{`{}`, `{"replicas": 3, "restart": true}`, `{"replicas": -1}`, `{"size": ""}`,
		`{"size": "small", "quack": true}`, `{"drainSeconds": -1}`, `{"drainSeconds": 86401}`, `{"tls": {}}`,
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
	if *scale.Replicas != 3 || scale.RestartedAt != "" || scale.ChangedBy != testIssuer+"|ops@example.org" ||
		scale.Name != "etl" || scale.Namespace != "hugr" {
		t.Errorf("scale: %+v", scale)
	}
	if restart.Replicas != nil || restart.RestartedAt != "2026-10-19T12:00:00Z" {
		t.Errorf("restart: %+v", restart)
	}
	// spec 006: the size, the doors, the drain time - one field a call
	for _, body := range []string{`{"size": "large"}`, `{"quack": false}`, `{"drainSeconds": 0}`} {
		if code, _, _ := h.do(t, "PATCH", APIPrefix+"groups/etl", opsToken, body); code != 204 {
			t.Fatalf("%s: %d", body, code)
		}
	}
	size, doors, drain := h.writer.changes[2], h.writer.changes[3], h.writer.changes[4]
	if size.Size != "large" || size.Owner() != SizeFieldOwner || doors.Quack == nil || *doors.Quack ||
		doors.Owner() != DoorsFieldOwner || drain.DrainSeconds == nil || *drain.DrainSeconds != 0 ||
		drain.Owner() != DrainFieldOwner {
		t.Errorf("size, doors, drain: %+v %+v %+v", size, doors, drain)
	}
	// the webhook's refusal is the person's to read
	h.writer.err = apierrors.NewForbidden(schema.GroupResource{Group: "platform.hugr.io", Resource: "hugrnodegroups"},
		"etl", errors.New(`admission webhook "vgroup.platform.hugr.io" denied the request: the size "huge" is not one the platform lists`))
	if code, out, _ := h.do(t, "PATCH", APIPrefix+"groups/etl", opsToken, `{"size": "huge"}`); code != 422 ||
		out["message"] != `the size "huge" is not one the platform lists` {
		t.Errorf("a refused size: %d %v", code, out)
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
	calls := len(h.nodes.calls)
	if code, _, _ := h.do(t, "POST", APIPrefix+"sign-out", opsToken, ""); code != http.StatusNoContent {
		t.Fatalf("sign-out: %d", code)
	}
	if len(h.nodes.calls) != calls {
		t.Errorf("sign-out asks no node: the console holds no session on any")
	}
	if _, ok := h.server.rights.get(tokenHash(opsToken), testNow); ok {
		t.Error("the rights of a token signed out are forgotten")
	}
}

// A token past its exp is refused before any node is asked, and the rights cached for a token end with it.
func TestTokenPastItsTime(t *testing.T) {
	h := newHarness(t, observeInfo, node("hugr-node-etl-a", "etl"))
	for name, token := range map[string]string{
		"expired":   tokenOf(testIssuer, "ops@example.org", testNow.Add(-time.Minute)),
		"no exp":    "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJvcHNAZXhhbXBsZS5vcmcifQ.c2ln",
		"not a JWT": "opaque-token",
	} {
		if code, _, _ := h.do(t, "GET", APIPrefix+"me", token, ""); code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, code)
		}
	}
	if len(h.nodes.calls) != 0 {
		t.Errorf("no node is asked for a token the console does not take: %v", h.nodes.calls)
	}
	// within the clocks' skew it is taken, and its rights are kept no longer than it lives
	soon := tokenOf(testIssuer, "ops@example.org", testNow.Add(10*time.Second))
	if code, _, _ := h.do(t, "GET", APIPrefix+"me", soon, ""); code != 200 {
		t.Fatalf("a token about to expire: %d", code)
	}
	if p, ok := h.server.rights.get(tokenHash(soon), testNow.Add(10*time.Second+tokenSkew+time.Second)); ok {
		t.Errorf("rights cached past the token's exp: %+v", p)
	}
	h.server.Now = func() time.Time { return testNow.Add(time.Minute) }
	if code, _, _ := h.do(t, "GET", APIPrefix+"me", soon, ""); code != http.StatusUnauthorized {
		t.Errorf("the same token a minute on: %d", code)
	}
}

// duckdb-acl's door answers a session it does not seat with UNKNOWN "acl: authentication failed" - a token it does not
// verify, a node at its cap, a policy source down - so the console asks the next node, and refuses the token only when
// every node that answered refused it so.
func TestNodesRefuseLikeTheDoor(t *testing.T) {
	failed := &NodeError{Code: codes.Unknown, Message: "acl: authentication failed"}
	h := newHarness(t, observeInfo, node("hugr-node-etl-a", "etl"), node("hugr-node-etl-b", "etl"))
	h.nodes.answer = func(fleet.Node, string) (*Result, error) { return nil, failed }
	if code, _, _ := h.do(t, "GET", APIPrefix+"me", opsToken, ""); code != http.StatusUnauthorized {
		t.Errorf("every node refused the token: %d", code)
	}
	if n := h.nodes.statements(stmtInfo); n != 2 {
		t.Errorf("each node asked once: %d", n)
	}
	full := newHarness(t, observeInfo, node("hugr-node-etl-a", "etl"), node("hugr-node-etl-b", "etl"))
	full.nodes.answer = func(n fleet.Node, statement string) (*Result, error) {
		if n.Label == "hugr-node-etl-a" {
			return nil, failed // at its session cap
		}
		return observeInfo, nil
	}
	for range 4 { // whichever node the shuffle asks first
		full.server.rights = rightsCache{}
		if code, _, _ := full.do(t, "GET", APIPrefix+"me", opsToken, ""); code != 200 {
			t.Fatalf("another node seats the person: %d", code)
		}
	}
	// a node action whose session was not seated is the node's state, not a sign-in to redo
	full.nodes.answer = func(_ fleet.Node, statement string) (*Result, error) {
		if statement == stmtInfo {
			return observeInfo, nil
		}
		return nil, failed
	}
	if code, out, _ := full.do(t, "GET", APIPrefix+"nodes/hugr-node-etl-a/sessions", opsToken, ""); code !=
		http.StatusServiceUnavailable || out["message"] != "acl: authentication failed" {
		t.Errorf("a node that seats no session for an action: %d %v", code, out)
	}
}

// Only the ACL's refusal of console_info means "no rights": any other engine error is the node's failure, answered as
// such and never cached.
func TestNodeFailureIsNoVerdict(t *testing.T) {
	h := newHarness(t, observeInfo, node("hugr-node-etl-a", "etl"))
	h.nodes.answer = func(fleet.Node, string) (*Result, error) {
		return nil, &NodeError{Code: codes.InvalidArgument, Message: "acl: IO Error: the policy catalog does not answer"}
	}
	if code, _, _ := h.do(t, "GET", APIPrefix+"me", opsToken, ""); code != http.StatusBadGateway {
		t.Errorf("a node that fails reading the policy: %d", code)
	}
	h.nodes.answer = func(fleet.Node, string) (*Result, error) { return observeInfo, nil }
	code, out, _ := h.do(t, "GET", APIPrefix+"me", opsToken, "")
	if code != 200 || out["rights"].(map[string]any)["fleet"] != true {
		t.Errorf("nothing of the failure was cached: %d %v", code, out)
	}
}

// The watch sees every node of the namespace; a node of another platform's group is no node of this console's.
func TestAnotherPlatformsNodes(t *testing.T) {
	h := newHarness(t, clusterInfo, node("hugr-node-theirs-a", "theirs"))
	if code, out, _ := h.do(t, "GET", APIPrefix+"me", opsToken, ""); code != http.StatusServiceUnavailable ||
		out["error"] != "no_node" {
		t.Errorf("only another platform's node: %d %v", code, out)
	}
	if len(h.nodes.calls) != 0 {
		t.Errorf("another platform's node is never asked: %v", h.nodes.calls)
	}
	both := newHarness(t, clusterInfo, node("hugr-node-theirs-a", "theirs"), node("hugr-node-etl-a", "etl"))
	for _, path := range []string{"nodes/hugr-node-theirs-a", "nodes/hugr-node-theirs-a/sessions"} {
		if code, _, _ := both.do(t, "GET", APIPrefix+path, opsToken, ""); code != http.StatusNotFound {
			t.Errorf("%s: %d", path, code)
		}
	}
	for _, c := range both.nodes.calls {
		if c.label == "hugr-node-theirs-a" {
			t.Errorf("another platform's node was called: %v", c)
		}
	}
}

// The webhook's refusal is the person's to read; the operator's own RBAC refusal is not.
func TestWebhookOrRBAC(t *testing.T) {
	h := newHarness(t, clusterInfo, node("hugr-node-etl-a", "etl"))
	h.writer.err = apierrors.NewForbidden(schema.GroupResource{Group: "platform.hugr.io", Resource: "hugrnodegroups"},
		"etl", errors.New(`admission webhook "vgroup.platform.hugr.io" denied the request: the plan runs at most 4 nodes`))
	code, out, _ := h.do(t, "PATCH", APIPrefix+"groups/etl", opsToken, `{"replicas": 9}`)
	if code != http.StatusUnprocessableEntity || !strings.HasPrefix(out["message"].(string), "the plan runs at most 4") {
		t.Errorf("the webhook's refusal: %d %v", code, out)
	}
	h.writer.err = apierrors.NewForbidden(schema.GroupResource{Group: "platform.hugr.io", Resource: "hugrnodegroups"},
		"etl", errors.New(`User "system:serviceaccount:hugr:hugr-platform" cannot patch resource`))
	code, out, _ = h.do(t, "PATCH", APIPrefix+"groups/etl", opsToken, `{"replicas": 3}`)
	if code != http.StatusServiceUnavailable || strings.Contains(out["message"].(string), "serviceaccount") {
		t.Errorf("the operator's RBAC refusal: %d %v", code, out)
	}
}

func TestHeaders(t *testing.T) {
	h := newHarness(t, observeInfo, node("hugr-node-etl-a", "etl"))
	r := httptest.NewRequest("GET", APIPrefix+"config", nil)
	w := httptest.NewRecorder()
	h.http.ServeHTTP(w, r)
	if !strings.HasPrefix(w.Header().Get("Strict-Transport-Security"), "max-age=") ||
		w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("the headers every answer carries: %v", w.Header())
	}
}

// A draining node seats only the call of a person who may resume it: the rights are asked of a node that serves first,
// and of a draining one only when no other answers - an operator still signs in when every node drains.
func TestRightsAskAServingNodeFirst(t *testing.T) {
	draining := node("hugr-node-etl-a", "etl")
	draining.Lease.Summary.Draining = true
	h := newHarness(t, observeInfo, draining, node("hugr-node-etl-b", "etl"))
	for range 4 { // whichever the shuffle
		h.server.rights = rightsCache{}
		h.nodes.calls = nil
		if code, _, _ := h.do(t, "GET", APIPrefix+"me", opsToken, ""); code != 200 {
			t.Fatalf("me: %d", code)
		}
		if len(h.nodes.calls) != 1 || h.nodes.calls[0].label != "hugr-node-etl-b" {
			t.Fatalf("the serving node is asked: %v", h.nodes.calls)
		}
	}
	only := newHarness(t, observeInfo, draining)
	if code, _, _ := only.do(t, "GET", APIPrefix+"me", opsToken, ""); code != 200 || only.nodes.calls[0].label != "hugr-node-etl-a" {
		t.Errorf("every node drains: the draining one is asked: %d %v", code, only.nodes.calls)
	}
}

// loadingNodes is a Caller that also reads the load report without a statement, as the Pool does (spec 012).
type loadingNodes struct {
	*fakeNodes
	loads []string
	err   error
}

func (l *loadingNodes) Load(_ context.Context, node fleet.Node, token string) (map[string]any, error) {
	l.loads = append(l.loads, node.Label+" "+token)
	if l.err != nil {
		return nil, l.err
	}
	return map[string]any{"sessions_live": int64(5), "memory_working": int64(40960)}, nil
}

func TestNodePageReadsTheLoadWithoutAStatement(t *testing.T) {
	h := newHarness(t, observeInfo, node("hugr-node-etl-a", "etl"))
	nodes := &loadingNodes{fakeNodes: h.nodes}
	h.server.Nodes = nodes
	code, out, _ := h.do(t, "GET", APIPrefix+"nodes/hugr-node-etl-a", opsToken, "")
	load, _ := out["load"].(map[string]any)
	if code != http.StatusOK || load["sessions_live"] != float64(5) || load["memory_working"] != float64(40960) {
		t.Fatalf("the report: %d %v", code, out)
	}
	if len(nodes.loads) != 1 || nodes.loads[0] != "hugr-node-etl-a "+opsToken || h.nodes.statements(stmtLoad) != 0 {
		t.Errorf("read once with the person's token, no statement: %q, %d", nodes.loads, h.nodes.statements(stmtLoad))
	}
	nodes.err = &NodeError{Code: codes.Unauthenticated, Message: "acl: the bearer token did not verify"}
	_, out, _ = h.do(t, "GET", APIPrefix+"nodes/hugr-node-etl-a", opsToken, "")
	if out["loadError"] != "acl: the bearer token did not verify" || out["load"] != nil {
		t.Errorf("the node's refusal, as its message: %v", out)
	}
	nodes.err = errNoLoad
	_, out, _ = h.do(t, "GET", APIPrefix+"nodes/hugr-node-etl-a", opsToken, "")
	if out["loadError"] != "the node answered no load report" {
		t.Errorf("no report: %v", out)
	}
}

func TestCandidatesAskABusyNodeFirst(t *testing.T) {
	// spec 012: a statement's per-call session ends, which moves an idle node's idle_since - the console's reads go to a
	// node with a live session first, an idle one next, a draining one last
	idle := node("hugr-node-etl-a", "etl")
	idle.Lease.Summary.Sessions = 0
	busy := node("hugr-node-etl-b", "etl")
	draining := node("hugr-node-etl-c", "etl")
	draining.Lease.Summary.Draining = true
	view := fleet.NewView([]fleet.Node{draining, idle, busy})
	for seed := uint64(0); seed < 20; seed++ {
		got := candidates(view, testNow, rand.New(rand.NewPCG(seed, seed)))
		if len(got) != 3 || got[0].Label != "hugr-node-etl-b" || got[1].Label != "hugr-node-etl-a" ||
			got[2].Label != "hugr-node-etl-c" {
			t.Fatalf("seed %d: %v, %v, %v", seed, got[0].Label, got[1].Label, got[2].Label)
		}
	}
}

func TestAutoGroupScalesItself(t *testing.T) {
	// spec 011: an auto group's replicas are the autoscaler's - the console shows its min, max and last change, and
	// a person's replicas are refused (its min and max are what to change)
	h := newHarness(t, clusterInfo, node("hugr-node-etl-a", "etl"))
	ctx := context.Background()
	var g v1alpha1.HugrNodeGroup
	if err := h.kube.Get(ctx, client.ObjectKey{Namespace: "hugr", Name: "etl"}, &g); err != nil {
		t.Fatal(err)
	}
	g.Spec.Scaling = &v1alpha1.ScalingSpec{Mode: "auto", Min: 0, Max: 3}
	g.Status.Scaling = &v1alpha1.ScalingStatus{Last: &v1alpha1.ScalingDecision{At: metav1.NewTime(testNow), From: 2,
		To: 1, Reason: "hugr-node-etl-b has had no session for 31m0s"}}
	if err := h.kube.Update(ctx, &g); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := h.do(t, "PATCH", APIPrefix+"groups/etl", opsToken, `{"replicas": 3}`); code != http.StatusConflict ||
		out["error"] != "auto_scaled" {
		t.Errorf("a person's replicas on an auto group: %d %v", code, out)
	}
	if len(h.writer.changes) != 0 {
		t.Errorf("refused, yet written: %+v", h.writer.changes)
	}
	_, out, _ := h.do(t, "GET", APIPrefix+"groups", opsToken, "")
	for _, raw := range out["groups"].([]any) {
		group := raw.(map[string]any)
		if group["name"] != "etl" {
			continue
		}
		scaling := group["scaling"].(map[string]any)
		last, _ := scaling["last"].(map[string]any)
		if scaling["mode"] != "auto" || scaling["min"] != float64(0) || scaling["max"] != float64(3) || last == nil ||
			last["to"] != float64(1) || !strings.Contains(last["reason"].(string), "no session") {
			t.Errorf("the group's scaling: %v", scaling)
		}
	}
}

func TestPatchScaling(t *testing.T) {
	// spec 011 stage 4: the console sets an auto group's mode, bounds and idle time - and only those fields, its own
	h := newHarness(t, clusterInfo, node("hugr-node-etl-a", "etl"))
	for _, body := range []string{`{"scaling": {"mode": "hpa"}}`, `{"scaling": {"mode": "auto", "min": 3, "max": 2}}`,
		`{"scaling": {"mode": "auto", "min": 0, "max": 0}}`, `{"scaling": {"mode": "auto", "min": 0, "max": 2, "idle": "5s"}}`,
		`{"scaling": {"mode": "auto", "min": 0, "max": 2, "idle": "forever"}}`, `{"scaling": {"mode": "auto", "max": 2}, "restart": true}`} {
		if code, _, _ := h.do(t, "PATCH", APIPrefix+"groups/etl", opsToken, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, code)
		}
	}
	if len(h.writer.changes) != 0 {
		t.Fatalf("nothing refused is written: %+v", h.writer.changes)
	}
	if code, _, _ := h.do(t, "PATCH", APIPrefix+"groups/etl", opsToken,
		`{"scaling": {"mode": "auto", "min": 0, "max": 3, "idle": "45m"}}`); code != 204 {
		t.Fatalf("auto: %d", code)
	}
	change := h.writer.changes[0]
	if change.Scaling == nil || change.Scaling.Mode != "auto" || change.Scaling.Max != 3 || change.Scaling.Idle != 45*time.Minute ||
		change.Owner() != ScalingFieldOwner {
		t.Fatalf("the change: %+v", change)
	}
	spec := change.Content()["spec"].(map[string]any)
	scaling := spec["scaling"].(map[string]any)
	if len(spec) != 1 || scaling["mode"] != "auto" || scaling["min"] != int64(0) || scaling["max"] != int64(3) ||
		scaling["idle"] != "45m0s" {
		t.Errorf("only the scaling's mode, bounds and idle are applied: %v", spec)
	}
	if _, thresholds := scaling["scaleUp"]; thresholds {
		t.Error("the thresholds are the definition's")
	}
	if code, _, _ := h.do(t, "PATCH", APIPrefix+"groups/etl", opsToken, `{"scaling": {"mode": "manual"}}`); code != 204 {
		t.Fatalf("manual: %d", code)
	}
	manual := h.writer.changes[1].Content()["spec"].(map[string]any)["scaling"].(map[string]any)
	if len(manual) != 1 || manual["mode"] != "manual" {
		t.Errorf("manual names the mode alone: %v", manual)
	}
	// manual keeping its bounds for a switch back (the review)
	if code, _, _ := h.do(t, "PATCH", APIPrefix+"groups/etl", opsToken,
		`{"scaling": {"mode": "manual", "min": 1, "max": 3, "idle": "1m30s"}}`); code != 204 {
		t.Fatalf("manual with its bounds: %d", code)
	}
	kept := h.writer.changes[2].Content()["spec"].(map[string]any)["scaling"].(map[string]any)
	if kept["mode"] != "manual" || kept["min"] != int64(1) || kept["max"] != int64(3) || kept["idle"] != "1m30s" {
		t.Errorf("manual keeps its bounds and idle: %v", kept)
	}
	observe := newHarness(t, observeInfo, node("hugr-node-etl-a", "etl"))
	if code, _, _ := observe.do(t, "PATCH", APIPrefix+"groups/etl", opsToken, `{"scaling": {"mode": "manual"}}`); code != http.StatusForbidden {
		t.Errorf("observe sets a group's scaling: %d", code)
	}
}
