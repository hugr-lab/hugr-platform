package console

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
)

// policyReader is a node whose policy has two catalogs, two roles, a source, and a grant per catalog.
func policyReader(h *harness, info *Result) {
	h.nodes.answer = func(_ fleet.Node, statement string) (*Result, error) {
		switch {
		case statement == stmtInfo:
			return info, nil
		case statement == `SELECT * FROM platform."grants"`:
			return &Result{Columns: []string{"role", "catalog", "caps"}, Rows: [][]any{
				{"analyst", "sales", map[string]any{"select": true}}, {"analyst", "hr", nil},
				{"admin", "sales", nil}}}, nil
		case statement == argumentLists["role"]:
			return &Result{Columns: []string{"name"}, Rows: [][]any{{"analyst"}, {"o'hara"}}}, nil
		case statement == argumentLists["catalog"]:
			return &Result{Columns: []string{"name"}, Rows: [][]any{{"sales"}, {"hr"}}}, nil
		case statement == stmtSources:
			return &Result{Columns: []string{"name"}, Rows: [][]any{{"attached.pg"}, {"attached.pg.public"},
				{`attached."a.b"`}}}, nil
		case strings.HasPrefix(statement, "SELECT * FROM platform.\"effective_rights\""),
			strings.HasPrefix(statement, "SELECT * FROM platform.\"exposures\""):
			return &Result{Columns: []string{"object", "caps"}, Rows: [][]any{{"orders", "select"}}}, nil
		}
		return &Result{Columns: []string{"ok"}, Rows: [][]any{{true}}}, nil
	}
}

var policyReaderInfo = infoRow([]string{"policy"}, []string{"grants", "roles", "catalogs"},
	[]string{"effective_rights", "exposures"})

func TestPolicyView(t *testing.T) {
	h := newHarness(t, policyReaderInfo, node("hugr-node-etl-a", "etl"))
	policyReader(h, policyReaderInfo)
	code, out, _ := h.do(t, "GET", APIPrefix+"policy/views/grants?role=analyst", opsToken, "")
	if code != 200 || out["total"] != float64(2) || len(out["rows"].([]any)) != 2 {
		t.Fatalf("a role's grants, filtered here: %d %v", code, out)
	}
	code, out, _ = h.do(t, "GET", APIPrefix+"policy/views/grants?offset=1&limit=1", opsToken, "")
	if rows := out["rows"].([]any); code != 200 || out["total"] != float64(3) || len(rows) != 1 ||
		rows[0].(map[string]any)["catalog"] != "hr" {
		t.Fatalf("a page of them: %d %v", code, out)
	}
	for path, want := range map[string]int{
		"policy/views/sessions":                     404, // the node's own: its page, not the policy's
		"policy/views/admins":                       403, // the policy's, not read by this person
		"policy/views/grants?nope=x":                400,
		"policy/views/grants?role=a&role=b":         400,
		"policy/views/grants?limit=0":               400,
		"policy/views/grants?limit=5001":            400,
		"policy/views/grants?offset=-1":             400,
		`policy/views/grants%22%3B%20DROP%20x%20--`: 404,
	} {
		if code, _, _ := h.do(t, "GET", APIPrefix+path, opsToken, ""); code != want {
			t.Errorf("%s: %d, want %d", path, code, want)
		}
	}
	for _, c := range h.nodes.calls {
		if c.statement != stmtInfo && c.statement != `SELECT * FROM platform."grants"` {
			t.Errorf("a statement not asked for: %q", c.statement)
		}
	}
}

func TestPolicyCall(t *testing.T) {
	h := newHarness(t, policyReaderInfo, node("hugr-node-etl-a", "etl"))
	policyReader(h, policyReaderInfo)
	code, out, _ := h.do(t, "GET", APIPrefix+"policy/functions/effective_rights?role=o%27hara&catalog=sales", opsToken, "")
	if code != 200 || out["total"] != float64(1) {
		t.Fatalf("effective rights: %d %v", code, out)
	}
	if h.nodes.statements(`SELECT * FROM platform."effective_rights"('o''hara', 'sales')`) != 1 {
		t.Errorf("the call's arguments, the node's own names as literals: %v", h.nodes.calls)
	}
	for path, want := range map[string]int{
		"policy/functions/effective_rights?role=analyst":                        400, // the catalog missing
		"policy/functions/effective_rights?role=nobody&catalog=sales":           422, // no such role
		"policy/functions/effective_rights?role=analyst&catalog=sales%27)%20--": 422,
		"policy/functions/check_catalog?catalog=sales":                          403, // not this person's function
		"policy/functions/acl_create_catalog?catalog=x":                         404,
	} {
		if code, _, _ := h.do(t, "GET", APIPrefix+path, opsToken, ""); code != want {
			t.Errorf("%s: %d, want %d", path, code, want)
		}
	}
	code, out, _ = h.do(t, "GET", APIPrefix+"policy/functions/exposures?source=a.b", opsToken, "")
	if code != 200 || h.nodes.statements(`SELECT * FROM platform."exposures"('a.b')`) != 1 {
		t.Errorf("a source by its alias, a quoted part read whole: %d %v", code, out)
	}
	if code, _, _ := h.do(t, "GET", APIPrefix+"policy/functions/exposures?source=public", opsToken, ""); code != 422 {
		t.Errorf("a schema of a source is not a source: %d", code)
	}
	code, out, _ = h.do(t, "GET", APIPrefix+"policy/sources", opsToken, "")
	if code != 200 || !reflect.DeepEqual(out["sources"], []any{"pg", "a.b"}) {
		t.Errorf("the sources: %d %v", code, out)
	}
	o := newHarness(t, observeInfo, node("hugr-node-etl-a", "etl"))
	if code, _, _ := o.do(t, "GET", APIPrefix+"policy/sources", opsToken, ""); code != http.StatusForbidden {
		t.Errorf("observe reads no source: %d", code)
	}
}

func TestKeyParts(t *testing.T) {
	for key, want := range map[string][]string{
		"attached.pg":       {"attached", "pg"},
		`attached."a.b"`:    {"attached", "a.b"},
		`attached."a""b".x`: {"attached", `a"b`, "x"},
		"a":                 {"a"},
		"":                  nil,
		"a..b":              nil,
		"a.":                nil,
		`attached."open`:    nil,
		`attached."x"y`:     nil,
	} {
		if got := keyParts(key); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %q, want %q", key, got, want)
		}
	}
	for in, want := range map[string]string{"sales": "'sales'", "o'hara": "'o''hara'"} {
		if got, ok := literal(in); !ok || got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
	for _, in := range []string{"", "a\nb", strings.Repeat("x", 1025)} {
		if _, ok := literal(in); ok {
			t.Errorf("%q is not written", in)
		}
	}
}
