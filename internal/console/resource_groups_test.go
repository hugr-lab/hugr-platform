package console

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
)

func i64(n int64) *int64 { return &n }

// TestResourceGroupStatements: a new group is created whole; an existing one is written again only when its limits or
// its comment change (a re-create keeps its mark and its bindings), its mark and roles change one by one; a drop is
// refused for the default group; values and names the grammar does not take are refused.
func TestResourceGroupStatements(t *testing.T) {
	roles := []string{"analyst", "ops", "Odd role"}
	current := &ResourceGroupForm{Name: "batch", Limits: ResourceGroupLimits{WindowStart: i64(8), MaxSessions: i64(5)},
		Comment: "nightly", Roles: []string{"analyst"}}
	cases := []struct {
		name    string
		form    ResourceGroupForm
		current *ResourceGroupForm
		want    []string
	}{
		{"new", ResourceGroupForm{Name: "batch", Limits: ResourceGroupLimits{QueuePriority: i64(-1), BatchBytes: i64(33554432)},
			Default: true, Comment: "the night's", Roles: []string{"ops", "analyst", "ops"}}, nil,
			[]string{`CREATE RESOURCE GROUP "batch" (batch_bytes 33554432, queue_priority -1) DEFAULT COMMENT 'the night''s'`,
				`GRANT RESOURCE GROUP "batch" TO ROLE "analyst"`, `GRANT RESOURCE GROUP "batch" TO ROLE "ops"`}},
		{"new, no limits", ResourceGroupForm{Name: "batch"}, nil, []string{`CREATE RESOURCE GROUP "batch"`}},
		{"unchanged", ResourceGroupForm{Name: "batch", Limits: current.Limits, Comment: "nightly", Roles: []string{"analyst"}},
			current, nil},
		// a limit cleared: written again whole (ALTER … SET names a limit, never clears one)
		{"a limit cleared", ResourceGroupForm{Name: "batch", Limits: ResourceGroupLimits{WindowStart: i64(8)},
			Comment: "nightly", Roles: []string{"analyst"}}, current,
			[]string{`CREATE OR REPLACE RESOURCE GROUP "batch" (window_start 8) COMMENT 'nightly'`}},
		{"the comment cleared", ResourceGroupForm{Name: "batch", Limits: current.Limits, Roles: []string{"analyst"}}, current,
			[]string{`CREATE OR REPLACE RESOURCE GROUP "batch" (window_start 8, max_sessions 5)`}},
		{"the mark and the roles", ResourceGroupForm{Name: "batch", Limits: current.Limits, Comment: "nightly",
			Default: true, Roles: []string{"Odd role"}}, current,
			[]string{`ALTER RESOURCE GROUP "batch" SET DEFAULT`, `GRANT RESOURCE GROUP "batch" TO ROLE "Odd role"`,
				`REVOKE RESOURCE GROUP "batch" FROM ROLE "analyst"`}},
		{"the mark dropped", ResourceGroupForm{Name: "batch", Default: false},
			&ResourceGroupForm{Name: "batch", Default: true},
			[]string{`ALTER RESOURCE GROUP "batch" DROP DEFAULT`}},
		{"dropped", ResourceGroupForm{Name: "batch", Op: "drop"}, current, []string{`DROP RESOURCE GROUP "batch"`}},
	}
	for _, c := range cases {
		got, err := c.form.statements(c.current, roles)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %q %v", c.name, got, err)
		}
	}
	for name, c := range map[string]struct {
		form    ResourceGroupForm
		current *ResourceGroupForm
	}{
		"a negative limit":    {ResourceGroupForm{Name: "batch", Limits: ResourceGroupLimits{MaxSessions: i64(-1)}}, nil},
		"a role nobody made":  {ResourceGroupForm{Name: "batch", Roles: []string{"nobody"}}, nil},
		"no name":             {ResourceGroupForm{}, nil},
		"a comment's newline": {ResourceGroupForm{Name: "batch", Comment: "a\nb"}, nil},
		"the default dropped": {ResourceGroupForm{Name: "batch", Op: "drop"}, &ResourceGroupForm{Name: "batch", Default: true}},
		"nothing to drop":     {ResourceGroupForm{Name: "batch", Op: "drop"}, nil},
		"another op":          {ResourceGroupForm{Name: "batch", Op: "rename"}, nil},
	} {
		if got, err := c.form.statements(c.current, roles); err == nil {
			t.Errorf("%s: %q", name, got)
		}
	}
}

// resourceGroupsNode answers the policy's roles and resource groups: etl (served by the group etl, bound to analyst),
// batch (no group of nodes serves it, the default one); a write is recorded.
func resourceGroupsNode(h *harness, wrote *[]string) {
	answer := h.nodes.answer
	h.nodes.answer = func(n fleet.Node, statement string) (*Result, error) {
		switch statement {
		case stmtRoles:
			return &Result{Columns: []string{"role"}, Rows: [][]any{{"analyst"}, {"ops"}}}, nil
		case stmtResourceGroupRows:
			return &Result{Columns: []string{"group", "window_start", "window_max", "batch_bytes", "max_result_rows",
				"queue_priority", "max_sessions", "comment", "is_default"}, Rows: [][]any{
				{"etl", int64(8), nil, nil, nil, nil, nil, "", false},
				{"batch", nil, nil, nil, int64(1000), nil, int64(4), "nightly", true}}}, nil
		case stmtRoleResourceGroups:
			return &Result{Columns: []string{"role", "group"}, Rows: [][]any{{"analyst", "etl"}}}, nil
		}
		if strings.Contains(statement, "RESOURCE GROUP ") {
			*wrote = append(*wrote, statement)
			return said(true, nil), nil
		}
		return answer(n, statement)
	}
}

// TestResourceGroupEditor: a group is read with its version and the groups of nodes serving it; a preview composes, a
// write runs what was shown; a stale or an unshown edit, a new group over one, a drop of a served group and a person
// below the policy bundle are refused - nothing written.
func TestResourceGroupEditor(t *testing.T) {
	h := newHarness(t, policyInfo, node("hugr-node-etl-a", "etl"))
	var wrote []string
	resourceGroupsNode(h, &wrote)
	code, out, body := h.do(t, "GET", APIPrefix+"policy/resource-group?name=etl", opsToken, "")
	if code != 200 || out["exists"] != true || out["defaultIs"] != "batch" ||
		!reflect.DeepEqual(out["nodeGroups"], []any{"etl"}) || !reflect.DeepEqual(out["roles"], []any{"analyst", "ops"}) {
		t.Fatalf("read: %d %s", code, body)
	}
	g := out["group"].(map[string]any)
	v := out["version"].(string)
	if g["limits"].(map[string]any)["window_start"] != float64(8) || g["limits"].(map[string]any)["max_sessions"] != nil ||
		!reflect.DeepEqual(g["roles"], []any{"analyst"}) || v == "" {
		t.Errorf("the group: %v %q", g, v)
	}
	edit := `{"form": {"name": "etl", "limits": {"window_start": 16, "max_sessions": 20}, "comment": "", "roles": ["analyst", "ops"]}, "version": "` + v + `"}`
	want := []any{`CREATE OR REPLACE RESOURCE GROUP "etl" (window_start 16, max_sessions 20)`,
		`GRANT RESOURCE GROUP "etl" TO ROLE "ops"`}
	code, out, body = h.do(t, "POST", APIPrefix+"policy/resource-group/preview", opsToken, edit)
	if code != 200 || !reflect.DeepEqual(out["statements"], want) || len(wrote) != 0 {
		t.Fatalf("preview: %d %s %q", code, body, wrote)
	}
	if code, out, _ := h.do(t, "POST", APIPrefix+"policy/resource-group", opsToken, edit); code != http.StatusConflict || out["error"] != "not_shown" {
		t.Errorf("a write without the statements shown: %d %v", code, out)
	}
	shown := strings.Replace(edit, `"version"`, `"statements": ["CREATE OR REPLACE RESOURCE GROUP \"etl\" (window_start 16, max_sessions 20)", "GRANT RESOURCE GROUP \"etl\" TO ROLE \"ops\""], "version"`, 1)
	code, out, body = h.do(t, "POST", APIPrefix+"policy/resource-group", opsToken, shown)
	if code != 200 || len(wrote) != 2 || len(out["answers"].([]any)) != 2 || out["refused"] != nil {
		t.Fatalf("write: %d %s %q", code, body, wrote)
	}
	for _, c := range []struct {
		body, code string
	}{
		{strings.Replace(shown, v, "0123", 1), "group_changed"},
		{strings.Replace(shown, v, "", 1), "group_exists"},
		// the group etl serves it: its nodes would drain
		{`{"form": {"name": "etl", "op": "drop"}, "version": "` + v + `", "statements": ["DROP RESOURCE GROUP \"etl\""]}`, "served"},
	} {
		if code, out, _ := h.do(t, "POST", APIPrefix+"policy/resource-group", opsToken, c.body); code != http.StatusConflict ||
			out["error"] != c.code {
			t.Errorf("%s: %d %v", c.code, code, out)
		}
	}
	// a new group: a plain name only
	if code, _, _ := h.do(t, "POST", APIPrefix+"policy/resource-group/preview", opsToken,
		`{"form": {"name": "a.b"}, "version": ""}`); code != http.StatusUnprocessableEntity {
		t.Errorf("a dotted name: %d", code)
	}
	code, out, body = h.do(t, "POST", APIPrefix+"policy/resource-group/preview", opsToken,
		`{"form": {"name": "adhoc", "limits": {"max_result_rows": 0}}, "version": ""}`)
	if code != 200 || !reflect.DeepEqual(out["statements"], []any{`CREATE RESOURCE GROUP "adhoc" (max_result_rows 0)`}) {
		t.Errorf("a new group: %d %s", code, body)
	}
	if len(wrote) != 2 {
		t.Errorf("a refused edit reached the node: %q", wrote)
	}
	// below the policy bundle: no part of it
	h2 := newHarness(t, clusterInfo, node("hugr-node-etl-a", "etl"))
	if code, _, _ := h2.do(t, "GET", APIPrefix+"policy/resource-group?name=etl", opsToken, ""); code != http.StatusForbidden {
		t.Errorf("the cluster bundle read a resource group: %d", code)
	}
}
