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
		case statement == `SELECT * FROM platform."grants" LIMIT 100001`:
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
		if c.statement != stmtInfo && c.statement != `SELECT * FROM platform."grants" LIMIT 100001` {
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
	// a catalog as the node compares it (an ASCII fold), written in the node's spelling
	h.do(t, "GET", APIPrefix+"policy/functions/effective_rights?role=analyst&catalog=SALES", opsToken, "")
	if h.nodes.statements(`SELECT * FROM platform."effective_rights"('analyst', 'sales')`) != 1 {
		t.Errorf("a catalog's spelling is the node's: %v", h.nodes.calls)
	}
	// a role exactly
	if code, _, _ := h.do(t, "GET", APIPrefix+"policy/functions/effective_rights?role=ANALYST&catalog=sales", opsToken, ""); code != 422 {
		t.Errorf("a role compares exactly: %d", code)
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

func TestPolicyViewBounds(t *testing.T) {
	h := newHarness(t, policyReaderInfo, node("hugr-node-etl-a", "etl"))
	h.nodes.answer = func(_ fleet.Node, statement string) (*Result, error) {
		if statement == stmtInfo {
			return policyReaderInfo, nil
		}
		rows := make([][]any, viewRowsMax+1)
		for i := range rows {
			rows[i] = []any{"r", int64(i % 2), true}
		}
		return &Result{Columns: []string{"role", "pos", "flag"}, Rows: rows}, nil
	}
	code, out, _ := h.do(t, "GET", APIPrefix+"policy/views/grants?limit=1", opsToken, "")
	if code != 200 || out["truncated"] != true || out["total"] != float64(viewRowsMax) {
		t.Errorf("a view past the bound is cut there and says so: %d %v", code, out["total"])
	}
	// a filter on a number or a flag compares its text
	if _, out, _ := h.do(t, "GET", APIPrefix+"policy/views/grants?pos=1&flag=true&limit=1", opsToken, ""); out["total"] != float64(viewRowsMax/2) {
		t.Errorf("a filter on a number: %v", out["total"])
	}
}

func TestPolicyCallNeedsItsLists(t *testing.T) {
	// effective_rights held by name, the views its arguments are checked against not read: said, not the node's refusal
	info := infoRow([]string{"observe"}, []string{"grants"}, []string{"effective_rights"})
	h := newHarness(t, info, node("hugr-node-etl-a", "etl"))
	policyReader(h, info)
	code, out, _ := h.do(t, "GET", APIPrefix+"policy/functions/effective_rights?role=analyst&catalog=sales", opsToken, "")
	if code != 403 || !strings.Contains(out["message"].(string), "platform.roles") {
		t.Errorf("%d %v", code, out)
	}
}
