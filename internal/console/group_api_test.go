package console

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
)

var (
	policyInfo = infoRow([]string{"observe", "policy"}, []string{"node_load", "sessions"}, nil)
	newTLS     = v1alpha1.TLSSpec{CSI: &v1alpha1.CSICertificate{IssuerRef: v1alpha1.IssuerRef{Name: "hugr-internal",
		Kind: "Issuer", Group: "cert-manager.io"}, Duration: "720h"}}
)

func TestCreateGroup(t *testing.T) {
	h := newHarness(t, clusterInfo, node("hugr-node-etl-a", "etl"))
	h.server.NewGroupTLS = newTLS
	good := `{"name": "reports", "size": "small", "replicas": 2, "clientID": "hugr-node-reports", "quack": true, ` +
		`"drainSeconds": 60, "resourceGroup": "reports"}`
	for _, body := range []string{`{}`, `not json`, `{"name": "Reports", "size": "small", "clientID": "x"}`,
		`{"name": "reports", "clientID": "x"}`, `{"name": "reports", "size": "small"}`,
		`{"name": "reports", "size": "small", "clientID": "a b"}`,
		`{"name": "reports", "size": "small", "clientID": "x", "replicas": -1}`,
		`{"name": "reports", "size": "small", "clientID": "x", "drainSeconds": 90000}`,
		`{"name": "reports", "size": "small", "clientID": "x", "resourceGroup": "a\"b"}`,
		`{"name": "reports", "size": "small", "clientID": "x", "tls": {"secretRef": {"name": "hugr-operator"}}}`} {
		if code, _, _ := h.do(t, "POST", APIPrefix+"groups", opsToken, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, code)
		}
	}
	if len(h.writer.created) != 0 {
		t.Fatalf("nothing refused is made: %+v", h.writer.created)
	}
	if code, _, _ := h.do(t, "POST", APIPrefix+"groups", opsToken, good); code != http.StatusCreated {
		t.Fatalf("a group: %d", code)
	}
	g := h.writer.created[0]
	if g.Name != "reports" || g.Namespace != "hugr" || g.Spec.Platform != "hugr" || g.Spec.Size != "small" ||
		g.Spec.Replicas != 2 || g.Spec.Identity.ClientID != "hugr-node-reports" || !g.Spec.Doors.Quack ||
		*g.Spec.DrainSeconds != 60 || g.Spec.ResourceGroup != "reports" ||
		g.Annotations[ChangedByAnnotation] != testIssuer+"|ops@example.org" {
		t.Errorf("the group as described, this platform's, named for who made it: %+v", g)
	}
	if g.Spec.TLS.CSI == nil || g.Spec.TLS.CSI.IssuerRef.Name != "hugr-internal" || g.Spec.TLS.SecretRef != nil {
		t.Errorf("the doors' certificate is the chart's groups' own, never the browser's: %+v", g.Spec.TLS)
	}
	g.Spec.TLS.CSI.IssuerRef.Name = "changed"
	if h.server.NewGroupTLS.CSI.IssuerRef.Name != "hugr-internal" {
		t.Error("a made group shares nothing with the console's configuration")
	}
	h.writer.err = apierrors.NewAlreadyExists(schema.GroupResource{Resource: "hugrnodegroups"}, "reports")
	if code, _, _ := h.do(t, "POST", APIPrefix+"groups", opsToken, good); code != http.StatusConflict {
		t.Errorf("a group of the name exists: %d", code)
	}
	h.writer.err = apierrors.NewForbidden(schema.GroupResource{Group: "platform.hugr.io", Resource: "hugrnodegroups"},
		"reports", errors.New(`admission webhook "vgroup.platform.hugr.io" denied the request: the plan runs at most 12 nodes`))
	if code, out, _ := h.do(t, "POST", APIPrefix+"groups", opsToken, good); code != 422 ||
		out["message"] != "the plan runs at most 12 nodes" {
		t.Errorf("past the plan, the webhook's reason: %d %v", code, out)
	}
	h.writer.err = apierrors.NewForbidden(schema.GroupResource{Group: "platform.hugr.io", Resource: "hugrnodegroups"},
		"reports", errors.New(`User "system:serviceaccount:hugr:hugr-platform" cannot create resource`))
	if code, out, raw := h.do(t, "POST", APIPrefix+"groups", opsToken, good); code != 503 ||
		strings.Contains(raw, "serviceaccount") {
		t.Errorf("the operator's own refusal is logged, not shown: %d %v", code, out)
	}
	// with no issuer configured the console makes no group
	h.writer.err = nil
	h.server.NewGroupTLS = v1alpha1.TLSSpec{}
	if code, _, _ := h.do(t, "POST", APIPrefix+"groups", opsToken, good); code != http.StatusServiceUnavailable {
		t.Errorf("no certificate issuer: %d", code)
	}
	// below the cluster bundle: refused before anything
	o := newHarness(t, operateInfo, node("hugr-node-etl-a", "etl"))
	o.server.NewGroupTLS = newTLS
	if code, _, _ := o.do(t, "POST", APIPrefix+"groups", opsToken, good); code != http.StatusForbidden ||
		len(o.writer.created) != 0 {
		t.Errorf("operate makes no group: %d", code)
	}
}

func TestDeleteGroup(t *testing.T) {
	h := newHarness(t, clusterInfo, node("hugr-node-etl-a", "etl"))
	if code, _, _ := h.do(t, "DELETE", APIPrefix+"groups/theirs", opsToken, ""); code != 404 {
		t.Errorf("another platform's group: %d", code)
	}
	if code, _, _ := h.do(t, "DELETE", APIPrefix+"groups/etl", opsToken, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if len(h.writer.deleted) != 1 || h.writer.deleted[0] != "hugr/etl" {
		t.Errorf("deleted: %v", h.writer.deleted)
	}
	for _, c := range h.nodes.calls {
		if strings.Contains(c.statement, "RESOURCE GROUP") {
			t.Errorf("a delete leaves the policy alone: %q", c.statement)
		}
	}
	o := newHarness(t, operateInfo, node("hugr-node-etl-a", "etl"))
	if code, _, _ := o.do(t, "DELETE", APIPrefix+"groups/etl", opsToken, ""); code != http.StatusForbidden ||
		len(o.writer.deleted) != 0 {
		t.Errorf("operate deletes no group: %d", code)
	}
}

// policyAnswers is a node's policy: roles, resource groups, bindings - and what it was told to write.
func policyAnswers(h *harness, groups [][]any) {
	h.nodes.answer = func(_ fleet.Node, statement string) (*Result, error) {
		switch statement {
		case stmtInfo:
			return policyInfo, nil
		case stmtRoles:
			return &Result{Columns: []string{"role"}, Rows: [][]any{{"analyst"}, {`odd "role"`}, {"platform_admin"}}}, nil
		case stmtResourceGroups:
			return &Result{Columns: []string{"group", "is_default"}, Rows: groups}, nil
		case stmtRoleResourceGroups:
			return &Result{Columns: []string{"role", "group"}, Rows: [][]any{{"analyst", "general"}}}, nil
		}
		return &Result{Columns: []string{"ok"}, Rows: [][]any{{true}}}, nil
	}
}

func TestGroupPolicy(t *testing.T) {
	h := newHarness(t, policyInfo, node("hugr-node-etl-a", "etl"))
	policyAnswers(h, [][]any{{"general", true}})
	code, out, _ := h.do(t, "GET", APIPrefix+"groups/etl/policy", opsToken, "")
	if code != 200 || out["resourceGroup"] != "etl" || out["inPolicy"] != false || len(out["roles"].([]any)) != 3 ||
		len(out["bound"].([]any)) != 0 {
		t.Fatalf("a group the policy lacks: %d %v", code, out)
	}
	code, out, _ = h.do(t, "GET", APIPrefix+"groups/general/policy", opsToken, "")
	if code != 200 || out["inPolicy"] != true || out["default"] != true || out["bound"].([]any)[0] != "analyst" {
		t.Fatalf("a group the policy has: %d %v", code, out)
	}
	// a role the node does not list is refused, and nothing is written
	if code, _, _ := h.do(t, "POST", APIPrefix+"groups/etl/policy", opsToken, `{"roles": ["nobody"]}`); code != 422 {
		t.Errorf("an unknown role: %d", code)
	}
	if h.nodes.statements("CREATE RESOURCE GROUP \"etl\"") != 0 {
		t.Error("nothing is written before every role is known")
	}
	code, _, _ = h.do(t, "POST", APIPrefix+"groups/etl/policy", opsToken,
		`{"roles": ["analyst", "odd \"role\""], "default": true}`)
	if code != 200 {
		t.Fatalf("add: %d", code)
	}
	for _, want := range []string{`CREATE RESOURCE GROUP "etl" DEFAULT`, `GRANT RESOURCE GROUP "etl" TO ROLE "analyst"`,
		`GRANT RESOURCE GROUP "etl" TO ROLE "odd ""role"""`} {
		if h.nodes.statements(want) != 1 {
			t.Errorf("not written once: %s", want)
		}
	}
	// a resource group the policy has already: the roles are bound, nothing is created
	h.do(t, "POST", APIPrefix+"groups/general/policy", opsToken, `{"roles": ["platform_admin"]}`)
	if h.nodes.statements(`CREATE RESOURCE GROUP "general"`) != 0 ||
		h.nodes.statements(`GRANT RESOURCE GROUP "general" TO ROLE "platform_admin"`) != 1 {
		t.Error("a known resource group is bound to, never created again")
	}
	if code, _, _ := h.do(t, "DELETE", APIPrefix+"groups/etl/policy", opsToken, ""); code != http.StatusNoContent ||
		h.nodes.statements(`DROP RESOURCE GROUP IF EXISTS "etl"`) != 1 {
		t.Errorf("remove: %d", code)
	}
	// below the policy bundle: refused, nothing asked of a node but the rights
	o := newHarness(t, clusterInfo, node("hugr-node-etl-a", "etl"))
	if code, _, _ := o.do(t, "POST", APIPrefix+"groups/etl/policy", opsToken, `{"roles": []}`); code != 403 {
		t.Errorf("cluster is not policy: %d", code)
	}
}

func TestGroupPolicyNode(t *testing.T) {
	// the group's own nodes drain themselves (the policy lacks their group): the statement goes to a serving node,
	// and to one of the groups a placement refusal names
	draining := node("hugr-node-etl-a", "etl")
	draining.Lease.Summary.Draining = true
	h := newHarness(t, policyInfo, draining, node("hugr-node-general-a", "general"), node("hugr-node-other-a", "other"))
	policyAnswers(h, nil)
	inner := h.nodes.answer
	h.nodes.answer = func(n fleet.Node, statement string) (*Result, error) {
		if statement != stmtInfo && n.Label == "hugr-node-other-a" {
			return nil, &NodeError{Code: codes.Unavailable, Message: "not your group", Groups: []string{"general"}}
		}
		return inner(n, statement)
	}
	if code, _, _ := h.do(t, "GET", APIPrefix+"groups/etl/policy", opsToken, ""); code != 200 {
		t.Fatalf("policy: %d", code)
	}
	for _, c := range h.nodes.calls {
		if c.statement == stmtRoles && c.label == "hugr-node-etl-a" {
			t.Errorf("a draining node is asked last: %v", h.nodes.calls)
		}
	}
	// the node's refusal is the person's to read, and no other node is asked
	h.nodes.answer = func(n fleet.Node, statement string) (*Result, error) {
		if statement == stmtInfo {
			return policyInfo, nil
		}
		return nil, &NodeError{Code: codes.InvalidArgument, Message: "acl: this needs the policy bundle"}
	}
	before := len(h.nodes.calls)
	if code, out, _ := h.do(t, "GET", APIPrefix+"groups/etl/policy", opsToken, ""); code != 403 ||
		out["message"] != "acl: this needs the policy bundle" {
		t.Errorf("a refusal: %d %v", code, out)
	}
	if len(h.nodes.calls) != before+1 {
		t.Errorf("a refusal is the node's answer, not a reason to ask another: %d calls", len(h.nodes.calls)-before)
	}
}

func TestQuoted(t *testing.T) {
	for in, want := range map[string]string{"etl": `"etl"`, `a"b`: `"a""b"`, "Grp.1": `"Grp.1"`} {
		if got, ok := quoted(in); !ok || got != want {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "a\nb", "a\x00b", strings.Repeat("x", 257)} {
		if _, ok := quoted(in); ok {
			t.Errorf("%q is not written", in)
		}
	}
}
