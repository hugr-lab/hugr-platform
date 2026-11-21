package console

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
)

func capsOf(c ...string) *[]string { return &c }

// TestGrantStatements: each level's form makes the grammar duckdb-acl reads - names quoted as identifiers, the row
// filter a literal, a mask and a versions clause as written in their clauses; a catalog grant that exists changes
// property by property.
func TestGrantStatements(t *testing.T) {
	for _, tc := range []struct {
		form    GrantForm
		current *GrantForm
		want    []string
	}{
		{GrantForm{Level: "catalog", Role: "analyst", Catalog: "sales", Caps: capsOf("select", "insert"), Main: true,
			RLS: "tenant = acl_claim('tenant')", Versions: "ANY"}, nil,
			[]string{`GRANT CATALOG "sales" TO ROLE "analyst" WITH (select, insert) MAIN RLS 'tenant = acl_claim(''tenant'')' VERSIONS ANY`}},
		{GrantForm{Level: "catalog", Role: "r", Catalog: "c", Caps: capsOf()}, nil,
			[]string{`GRANT CATALOG "c" TO ROLE "r" CAPS '{}'`}},
		{GrantForm{Level: "catalog", Role: "r", Catalog: "c"}, nil, []string{`GRANT CATALOG "c" TO ROLE "r"`}},
		// an existing catalog grant: only what changed, its object grants kept
		{GrantForm{Level: "catalog", Role: "r", Catalog: "c", Caps: capsOf("select", "manage"), RLS: ""},
			&GrantForm{Level: "catalog", Role: "r", Catalog: "c", Caps: capsOf("select"), Main: true, RLS: "a > 1"},
			[]string{`ALTER GRANT CATALOG "c" TO ROLE "r" SET CAPS '{"select": true, "manage": true}'`,
				`ALTER GRANT CATALOG "c" TO ROLE "r" SET MAIN false`, `ALTER GRANT CATALOG "c" TO ROLE "r" SET RLS ''`}},
		{GrantForm{Level: "catalog", Role: "r", Catalog: "c"},
			&GrantForm{Level: "catalog", Role: "r", Catalog: "c", Caps: capsOf("select")},
			[]string{`ALTER GRANT CATALOG "c" TO ROLE "r" SET CAPS ''`}},
		{GrantForm{Level: "catalog", Role: "r", Catalog: "c", Caps: capsOf("select")},
			&GrantForm{Level: "catalog", Role: "r", Catalog: "c", Caps: capsOf("select")}, nil},
		{GrantForm{Level: "schema", Role: "ingest", Catalog: "sales", Target: `raw."eu.x"`, Caps: capsOf("select", "create"),
			Into: "phys.staging", Comment: "the lander's"}, nil,
			[]string{`GRANT SCHEMA "sales"."raw"."eu.x" TO ROLE "ingest" WITH (select, create) INTO "phys"."staging" COMMENT 'the lander''s'`}},
		{GrantForm{Level: "schema", Role: "r", Catalog: "c", Target: "vs", VirtualOnly: true}, nil,
			[]string{`GRANT SCHEMA "c"."vs" TO ROLE "r" VIRTUAL ONLY`}},
		{GrantForm{Level: "object", Role: "analyst", Catalog: "c", Target: "people",
			Columns: &ColumnTest{Mode: "except", Names: []string{"salary"}},
			Masks:   []GrantMask{{Column: "ssn", Mask: "'***-**-' || right(ssn, 4)"}}, Versions: "AT (VERSION => 3)"},
			&GrantForm{Level: "object"},
			[]string{`GRANT OBJECT "c"."people" TO ROLE "analyst" COLUMNS (* EXCLUDE ("salary")) MASK ("ssn" AS '***-**-' || right(ssn, 4)) VERSIONS AT (VERSION => 3)`}},
		{GrantForm{Level: "object", Role: "r", Catalog: "c", Target: "t", Caps: capsOf("select"), RLS: "a = 1",
			Columns: &ColumnTest{Mode: "only", Names: []string{"id", "Odd name"}}}, nil,
			[]string{`GRANT OBJECT "c"."t" TO ROLE "r" WITH (select) RLS 'a = 1' COLUMNS ("id", "Odd name")`}},
		{GrantForm{Level: "object", Op: "revoke", Role: "r", Catalog: "c", Target: "t"}, &GrantForm{},
			[]string{`REVOKE OBJECT "c"."t" FROM ROLE "r"`}},
		{GrantForm{Level: "catalog", Op: "revoke", Role: "r", Catalog: "c"}, &GrantForm{},
			[]string{`REVOKE CATALOG "c" FROM ROLE "r"`}},
		// an existing catalog grant's versions: the grant written again (it keeps its object grants)
		{GrantForm{Level: "catalog", Role: "r", Catalog: "c", Caps: capsOf("select"), Main: true, Versions: "any"},
			&GrantForm{Level: "catalog", Role: "r", Catalog: "c", Caps: capsOf("select"), Main: true},
			[]string{`GRANT CATALOG "c" TO ROLE "r" WITH (select) MAIN VERSIONS any`}},
		// an unreadable grant is still revoked
		{GrantForm{Level: "object", Op: "revoke", Role: "r", Catalog: "c", Target: "t"}, &GrantForm{Unreadable: true},
			[]string{`REVOKE OBJECT "c"."t" FROM ROLE "r"`}},
		{GrantForm{Level: "object", Role: "r", Catalog: "c", Target: "t",
			Versions: "BETWEEN TIMESTAMP '2026-01-01 MASK' AND now()"}, nil,
			[]string{`GRANT OBJECT "c"."t" TO ROLE "r" VERSIONS BETWEEN TIMESTAMP '2026-01-01 MASK' AND now()`}},
		// a name ending in e before a quote is no escape string; a $ inside a string is text
		{GrantForm{Level: "object", Role: "r", Catalog: "c", Target: "t",
			Masks: []GrantMask{{Column: "a", Mask: "concat(name, '$')"}}}, nil,
			[]string{`GRANT OBJECT "c"."t" TO ROLE "r" MASK ("a" AS concat(name, '$'))`}},
		// a catalog grant's versions cleared: written again without the clause (the current version)
		{GrantForm{Level: "catalog", Role: "r", Catalog: "c"},
			&GrantForm{Level: "catalog", Role: "r", Catalog: "c", Versions: "ANY"},
			[]string{`GRANT CATALOG "c" TO ROLE "r"`}},
		// a mask's own quotes and parentheses
		{GrantForm{Level: "object", Role: "r", Catalog: "c", Target: "t",
			Masks: []GrantMask{{Column: "a", Mask: "replace(a, ';', '(')"}}}, nil,
			[]string{`GRANT OBJECT "c"."t" TO ROLE "r" MASK ("a" AS replace(a, ';', '('))`}},
	} {
		got, err := tc.form.statements(tc.current)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%+v:\n got %q (%v)\nwant %q", tc.form, got, err, tc.want)
		}
	}
	for name, tc := range map[string]struct {
		form    GrantForm
		current *GrantForm
	}{
		"a capability with SQL":     {GrantForm{Level: "catalog", Role: "r", Catalog: "c", Caps: capsOf("select) MAIN --")}, nil},
		"a capability's capitals":   {GrantForm{Level: "catalog", Role: "r", Catalog: "c", Caps: capsOf("Select")}, nil},
		"a revoke of nothing":       {GrantForm{Level: "object", Op: "revoke", Role: "r", Catalog: "c", Target: "t"}, nil},
		"another op":                {GrantForm{Level: "catalog", Op: "drop", Role: "r", Catalog: "c"}, nil},
		"a role's line break":       {GrantForm{Level: "catalog", Role: "r\n", Catalog: "c"}, nil},
		"no columns named":          {GrantForm{Level: "object", Role: "r", Catalog: "c", Target: "t", Columns: &ColumnTest{Mode: "only"}}, nil},
		"another column mode":       {GrantForm{Level: "object", Role: "r", Catalog: "c", Target: "t", Columns: &ColumnTest{Mode: "x", Names: []string{"a"}}}, nil},
		"an empty mask":             {GrantForm{Level: "object", Role: "r", Catalog: "c", Target: "t", Masks: []GrantMask{{Column: "a"}}}, nil},
		"into and virtual only":     {GrantForm{Level: "schema", Role: "r", Catalog: "c", Target: "s", Into: "p.s", VirtualOnly: true}, nil},
		"a legacy list's versions":  {GrantForm{Level: "catalog", Role: "r", Catalog: "c", Versions: "ANY"}, &GrantForm{LegacyColumns: true}},
		"an unreadable grant":       {GrantForm{Level: "object", Role: "r", Catalog: "c", Target: "t"}, &GrantForm{Unreadable: true}},
		"a mask's second statement": {GrantForm{Level: "object", Role: "r", Catalog: "c", Target: "t", Masks: []GrantMask{{Column: "a", Mask: "1); DROP ROLE x; --"}}}, nil},
		"a mask's comment":          {GrantForm{Level: "object", Role: "r", Catalog: "c", Target: "t", Masks: []GrantMask{{Column: "a", Mask: "1 -- x"}}}, nil},
		"a mask's open parenthesis": {GrantForm{Level: "object", Role: "r", Catalog: "c", Target: "t", Masks: []GrantMask{{Column: "a", Mask: "upper(a"}}}, nil},
		"a mask's open quote":       {GrantForm{Level: "object", Role: "r", Catalog: "c", Target: "t", Masks: []GrantMask{{Column: "a", Mask: "'x"}}}, nil},
		"versions with a clause":    {GrantForm{Level: "object", Role: "r", Catalog: "c", Target: "t", Versions: "ANY MASK (a AS 1)"}, nil},
		"versions not a version":    {GrantForm{Level: "object", Role: "r", Catalog: "c", Target: "t", Versions: "RLS (true)"}, nil},
		"between and a clause": {GrantForm{Level: "object", Role: "r", Catalog: "c", Target: "t",
			Versions: "BETWEEN '2026-01-01' AND now() COLUMNS (a)"}, nil},
		"at and more": {GrantForm{Level: "object", Role: "r", Catalog: "c", Target: "t", Versions: "AT (VERSION => 1) (x)"}, nil},
		// strings this scan does not read as duckdb's tokenizer does
		"an escape string": {GrantForm{Level: "object", Role: "r", Catalog: "c", Target: "t",
			Masks: []GrantMask{{Column: "a", Mask: `E'\'' ) ; REVOKE CATALOG c FROM ROLE r ; SELECT (E'\''`}}}, nil},
		"a dollar string": {GrantForm{Level: "object", Role: "r", Catalog: "c", Target: "t",
			Masks: []GrantMask{{Column: "a", Mask: `$$'$$ ) ; X ; SELECT ($$'$$`}}}, nil},
		"a backtick name": {GrantForm{Level: "object", Role: "r", Catalog: "c", Target: "t",
			Masks: []GrantMask{{Column: "a", Mask: "`'` ) ; GRANT CATALOG c TO ROLE r MAIN ; GRANT OBJECT c.t TO ROLE r MASK (\"b\" AS `'`"}}}, nil},
		"an escape string after a digit": {GrantForm{Level: "object", Role: "r", Catalog: "c", Target: "t",
			Masks: []GrantMask{{Column: "a", Mask: `1E'\'' ) ; X ; Y (1E'\''`}}}, nil},
		"versions' escape string": {GrantForm{Level: "object", Role: "r", Catalog: "c", Target: "t",
			Versions: `AT (TIMESTAMP => e'\'' ) MASK (a AS 1) --')`}, nil},
	} {
		if got, err := tc.form.statements(tc.current); err == nil {
			t.Errorf("%s: written %q", name, got)
		}
	}
}

// grantsNode answers the editor's reads as a node does: the roles and catalogs, the grant views, the names.
func grantsNode(h *harness, wrote *[]string, refuse string) {
	answer := h.nodes.answer
	h.nodes.answer = func(n fleet.Node, statement string) (*Result, error) {
		switch statement {
		case stmtGrantRoles:
			return &Result{Columns: []string{"name"}, Rows: [][]any{{"analyst"}, {"ops"}}}, nil
		case argumentLists["catalog"]:
			return &Result{Columns: []string{"name"}, Rows: [][]any{{"Sales"}}}, nil
		case stmtGrantRows:
			return &Result{Columns: []string{"role", "catalog", "is_main", "caps", "rls", "versions"}, Rows: [][]any{
				{"analyst", "Sales", true, map[string]any{"select": true, "insert": false}, "a > 1", nil}}}, nil
		case stmtObjectGrantRows:
			return &Result{Columns: []string{"role", "catalog", "object", "caps", "rls", "columns", "masks", "versions"},
				Rows: [][]any{{"analyst", "Sales", "people", nil, nil,
					map[string]any{"mode": "except", "names": []any{"salary"}},
					[]any{map[string]any{"column": "ssn", "mask": "CAST(('***') AS VARCHAR)"}}, nil},
					// a grant whose masks the node could not read: the view answers NULL
					{"ops", "Sales", "orders", nil, nil, map[string]any{"mode": "all", "names": []any{}}, nil, nil}}}, nil
		case stmtSchemaNames:
			return &Result{Columns: []string{"catalog", "name", "phys_path"}, Rows: [][]any{
				{"Sales", "pg", "pg.public"}, {"Sales", "raw", nil}}}, nil
		case stmtMappedRoles:
			if refuse == "mappings" {
				return nil, &NodeError{Message: "acl: platform.role_mappings is the policy bundle's"}
			}
			return &Result{Columns: []string{"name"}, Rows: [][]any{{"from_idp"}}}, nil
		case stmtObjectNames:
			return &Result{Columns: []string{"catalog", "name"}, Rows: [][]any{{"Sales", "people"}, {"Sales", "orders"}}}, nil
		}
		if strings.HasPrefix(statement, "GRANT ") || strings.HasPrefix(statement, "ALTER GRANT") ||
			strings.HasPrefix(statement, "REVOKE ") {
			if refuse != "" && strings.Contains(statement, refuse) {
				return nil, &NodeError{Message: "acl: refused " + refuse}
			}
			*wrote = append(*wrote, statement)
			return said(true, "group etl does not list source pg, which role analyst reads - CLUSTER SET SOURCES (sales, pg) IN GROUP etl"), nil
		}
		if strings.HasPrefix(statement, "SELECT * FROM platform.check_grant(") {
			*wrote = append(*wrote, statement)
			return &Result{Columns: []string{"item", "name", "type", "status", "message"},
				Rows: [][]any{{"rls", "a = 1", nil, "ok", nil}}}, nil
		}
		return answer(n, statement)
	}
}

// TestGrantEditor: the grant is read with its version; a preview composes, a write runs the statements and answers
// what each said; a grant changed since it was read is refused; names are the node's, in its spelling.
func TestGrantEditor(t *testing.T) {
	h := newHarness(t, clusterInfo, node("hugr-node-etl-a", "etl"))
	var wrote []string
	grantsNode(h, &wrote, "")
	code, out, body := h.do(t, "GET", APIPrefix+"policy/grant?level=catalog&role=analyst&catalog=sales", opsToken, "")
	if code != 200 || out["exists"] != true {
		t.Fatalf("read: %d %s", code, body)
	}
	g := out["grant"].(map[string]any)
	v := out["version"].(string)
	if g["catalog"] != "Sales" || g["rls"] != "a > 1" || g["main"] != true || !reflect.DeepEqual(g["caps"], []any{"select"}) || v == "" {
		t.Errorf("the grant: %v %q", g, v)
	}
	edit := `{"form": {"level": "catalog", "role": "analyst", "catalog": "sales", "caps": ["select"], "main": true, "rls": "a > 2"}, "version": "` + v + `"}`
	code, out, body = h.do(t, "POST", APIPrefix+"policy/grant/preview", opsToken, edit)
	if code != 200 || !reflect.DeepEqual(out["statements"], []any{`ALTER GRANT CATALOG "Sales" TO ROLE "analyst" SET RLS 'a > 2'`}) {
		t.Fatalf("preview: %d %s", code, body)
	}
	if len(wrote) != 0 {
		t.Errorf("a preview wrote: %q", wrote)
	}
	// the write carries the statements shown: none, or others, and nothing is written
	if code, _, _ := h.do(t, "POST", APIPrefix+"policy/grant", opsToken, edit); code != http.StatusConflict {
		t.Errorf("a write without the statements shown: %d", code)
	}
	other := strings.Replace(edit, `"version"`, `"statements": ["ALTER GRANT CATALOG \"Sales\" TO ROLE \"analyst\" SET RLS 'true'"], "version"`, 1)
	if code, _, _ := h.do(t, "POST", APIPrefix+"policy/grant", opsToken, other); code != http.StatusConflict {
		t.Errorf("a write of other statements: %d", code)
	}
	shown := strings.Replace(edit, `"version"`, `"statements": ["ALTER GRANT CATALOG \"Sales\" TO ROLE \"analyst\" SET RLS 'a > 2'"], "version"`, 1)
	code, out, body = h.do(t, "POST", APIPrefix+"policy/grant", opsToken, shown)
	answers := out["answers"].([]any)
	if code != 200 || len(wrote) != 1 || len(answers) != 1 || answers[0].(map[string]any)["ok"] != true ||
		!strings.Contains(answers[0].(map[string]any)["message"].(string), "CLUSTER SET SOURCES") || out["refused"] != nil {
		t.Fatalf("write: %d %s %q", code, body, wrote)
	}
	// a form made from another version of the grant: refused, nothing written
	stale := strings.Replace(shown, v, "0123", 1)
	if code, _, _ := h.do(t, "POST", APIPrefix+"policy/grant", opsToken, stale); code != http.StatusConflict {
		t.Errorf("a stale form: %d", code)
	}
	// a new grant where one exists: the same refusal
	if code, out, _ := h.do(t, "POST", APIPrefix+"policy/grant", opsToken, strings.Replace(shown, v, "", 1)); code != http.StatusConflict ||
		out["error"] != "grant_exists" || !strings.Contains(out["message"].(string), "holds this grant already") {
		t.Errorf("a new grant over one: %d", code)
	}
	// names the node does not list
	for _, bad := range []string{
		`{"form": {"level": "catalog", "role": "nobody", "catalog": "sales"}, "version": ""}`,
		`{"form": {"level": "catalog", "role": "analyst", "catalog": "nowhere"}, "version": ""}`,
		`{"form": {"level": "object", "role": "analyst", "catalog": "sales", "target": "secret"}, "version": ""}`,
		`{"form": {"level": "table", "role": "analyst", "catalog": "sales"}, "version": ""}`,
	} {
		if code, _, _ := h.do(t, "POST", APIPrefix+"policy/grant", opsToken, bad); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d", bad, code)
		}
	}
	if len(wrote) != 1 {
		t.Errorf("a refused form reached the node: %q", wrote)
	}
	// an object grant read back as the editor writes it: its COLUMNS test and its masks
	code, out, _ = h.do(t, "GET", APIPrefix+"policy/grant?level=object&role=analyst&catalog=sales&target=PEOPLE", opsToken, "")
	o := out["grant"].(map[string]any)
	if code != 200 || o["target"] != "people" || o["caps"] != nil ||
		!reflect.DeepEqual(o["columns"], map[string]any{"mode": "except", "names": []any{"salary"}}) ||
		!reflect.DeepEqual(o["masks"], []any{map[string]any{"column": "ssn", "mask": "CAST(('***') AS VARCHAR)"}}) {
		t.Errorf("an object grant: %d %v", code, o)
	}
	// one whose masks the node could not read: read as such, and only revoked - written anew it would show them
	code, out, _ = h.do(t, "GET", APIPrefix+"policy/grant?level=object&role=ops&catalog=sales&target=orders", opsToken, "")
	if code != 200 || out["grant"].(map[string]any)["unreadable"] != true {
		t.Fatalf("an unreadable grant: %d %v", code, out)
	}
	uv := out["version"].(string)
	caps := `{"form": {"level": "object", "role": "ops", "catalog": "Sales", "target": "orders", "caps": ["select"]}, "version": "` + uv + `"}`
	if code, out, _ := h.do(t, "POST", APIPrefix+"policy/grant/preview", opsToken, caps); code != http.StatusUnprocessableEntity ||
		!strings.Contains(out["message"].(string), "could not read") {
		t.Errorf("an unreadable grant written anew: %d %v", code, out)
	}
	revoke := `{"form": {"level": "object", "op": "revoke", "role": "ops", "catalog": "Sales", "target": "orders", "caps": null}, "version": "` + uv + `"}`
	if code, out, _ := h.do(t, "POST", APIPrefix+"policy/grant/preview", opsToken, revoke); code != 200 ||
		!reflect.DeepEqual(out["statements"], []any{`REVOKE OBJECT "Sales"."orders" FROM ROLE "ops"`}) {
		t.Errorf("an unreadable grant revoked: %d %v", code, out)
	}
	// a table under a live alias schema is an object the node takes; under a declared schema it is not one
	alias := `{"form": {"level": "object", "role": "analyst", "catalog": "Sales", "target": "PG.orders", "caps": null}, "version": ""}`
	if code, out, _ := h.do(t, "POST", APIPrefix+"policy/grant/preview", opsToken, alias); code != 200 ||
		!reflect.DeepEqual(out["statements"], []any{`GRANT OBJECT "Sales"."pg"."orders" TO ROLE "analyst"`}) {
		t.Errorf("a table under an alias: %d %v", code, out)
	}
	notAlias := strings.Replace(alias, "PG.orders", "raw.orders", 1)
	if code, _, _ := h.do(t, "POST", APIPrefix+"policy/grant/preview", opsToken, notAlias); code != http.StatusUnprocessableEntity {
		t.Errorf("a table under a declared schema: %d", code)
	}
}

// TestGrantRefusedHalfWay: the statements run in order and stop at the one the node refuses; what ran before it is
// answered, the refusal says which.
func TestGrantRefusedHalfWay(t *testing.T) {
	h := newHarness(t, clusterInfo, node("hugr-node-etl-a", "etl"))
	var wrote []string
	grantsNode(h, &wrote, "SET MAIN")
	_, out, _ := h.do(t, "GET", APIPrefix+"policy/grant?level=catalog&role=analyst&catalog=Sales", opsToken, "")
	v := out["version"].(string)
	edit := `{"form": {"level": "catalog", "role": "analyst", "catalog": "Sales", "caps": [], "main": false, "rls": "a > 1"}, ` +
		`"statements": ["ALTER GRANT CATALOG \"Sales\" TO ROLE \"analyst\" SET CAPS '{}'", "ALTER GRANT CATALOG \"Sales\" TO ROLE \"analyst\" SET MAIN false"], ` +
		`"version": "` + v + `"}`
	code, out, body := h.do(t, "POST", APIPrefix+"policy/grant", opsToken, edit)
	refused, _ := out["refused"].(map[string]any)
	if code != 200 || len(wrote) != 1 || len(out["answers"].([]any)) != 1 || refused == nil ||
		refused["index"] != float64(1) || refused["message"] != "acl: refused SET MAIN" ||
		len(out["statements"].([]any)) != 2 {
		t.Fatalf("half way: %d %s", code, body)
	}
}

// TestGrantCheck: check_grant with every argument a literal - the form's text never SQL of the statement; the policy
// bundle's function only.
func TestGrantCheck(t *testing.T) {
	info := infoRow([]string{"observe", "policy"}, []string{"grants", "object_grants"}, []string{"check_grant"})
	h := newHarness(t, info, node("hugr-node-etl-a", "etl"))
	var wrote []string
	grantsNode(h, &wrote, "")
	form := `{"level": "object", "role": "analyst", "catalog": "sales", "target": "orders", "caps": ["select"],
		"rls": "a = 1'); DROP ROLE x; --", "columns": {"mode": "only", "names": ["id"]},
		"masks": [{"column": "id", "mask": "-id"}]}`
	code, out, body := h.do(t, "POST", APIPrefix+"policy/grant/check", opsToken, form)
	if code != 200 || len(out["rows"].([]any)) != 1 {
		t.Fatalf("check: %d %s", code, body)
	}
	want := `SELECT * FROM platform.check_grant('analyst', 'Sales', 'orders', 'a = 1''); DROP ROLE x; --', '"id"', ` +
		`caps := '{"select": true}', masks := '"id" AS -id')`
	if len(wrote) != 1 || wrote[0] != want {
		t.Errorf("the check:\n got %q\nwant %q", wrote, want)
	}
	o := newHarness(t, clusterInfo, node("hugr-node-etl-a", "etl"))
	if code, _, _ := o.do(t, "POST", APIPrefix+"policy/grant/check", opsToken, form); code != http.StatusForbidden {
		t.Errorf("without check_grant: %d", code)
	}
}

// TestGrantMappedRole: a role only a mapping names is taken where the person reads the mappings; where the mappings
// are refused (they are the policy bundle's), the editor still works for the roles it knows.
func TestGrantMappedRole(t *testing.T) {
	for _, tc := range []struct {
		refuse string
		role   string
		want   int
	}{{"", "from_idp", 200}, {"mappings", "from_idp", http.StatusUnprocessableEntity}, {"mappings", "ops", 200}} {
		h := newHarness(t, clusterInfo, node("hugr-node-etl-a", "etl"))
		var wrote []string
		grantsNode(h, &wrote, tc.refuse)
		form := `{"form": {"level": "catalog", "role": "` + tc.role + `", "catalog": "Sales", "caps": null}, "version": ""}`
		if code, out, _ := h.do(t, "POST", APIPrefix+"policy/grant/preview", opsToken, form); code != tc.want {
			t.Errorf("%+v: %d %v", tc, code, out)
		}
	}
}
