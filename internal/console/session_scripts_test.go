package console

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
)

// TestSessionScriptStatements (spec 016): each level's SET and DROP, the policy always written, the SQL one literal as
// written; what the grammar does not take, a drop of none and a script as stored are refused.
func TestSessionScriptStatements(t *testing.T) {
	stored := storedScript{sql: "SELECT 1", onError: "continue"}
	cases := []struct {
		name    string
		form    SessionScriptForm
		current storedScript
		want    string
	}{
		{"a group's", SessionScriptForm{Group: "etl", SQL: "SET VARIABLE region = 'etl'"}, storedScript{},
			`ALTER RESOURCE GROUP "etl" SET SESSION SCRIPT 'SET VARIABLE region = ''etl''' ON ERROR FAIL SESSION`},
		{"a role's, two lines and a tab", SessionScriptForm{Role: "analyst", SQL: "SET TimeZone = 'UTC';\n\tSELECT 1", OnError: "continue"},
			storedScript{}, "ALTER ROLE \"analyst\" SET SESSION SCRIPT 'SET TimeZone = ''UTC'';\n\tSELECT 1' ON ERROR CONTINUE"},
		{"a grant's", SessionScriptForm{Group: "etl", Role: "Odd role", SQL: "SELECT 2", OnError: "fail"}, stored,
			`ALTER RESOURCE GROUP "etl" FOR ROLE "Odd role" SET SESSION SCRIPT 'SELECT 2' ON ERROR FAIL SESSION`},
		// the same text, another policy: written
		{"the policy changed", SessionScriptForm{Role: "analyst", SQL: "SELECT 1"}, stored,
			`ALTER ROLE "analyst" SET SESSION SCRIPT 'SELECT 1' ON ERROR FAIL SESSION`},
		{"a group's dropped", SessionScriptForm{Group: "etl", Op: "drop"}, stored, `ALTER RESOURCE GROUP "etl" DROP SESSION SCRIPT`},
		{"a grant's dropped", SessionScriptForm{Group: "etl", Role: "analyst", Op: "drop"}, stored,
			`ALTER RESOURCE GROUP "etl" FOR ROLE "analyst" DROP SESSION SCRIPT`},
	}
	for _, c := range cases {
		got, err := c.form.statement(c.current)
		if err != nil || got != c.want {
			t.Errorf("%s: %q %v", c.name, got, err)
		}
	}
	for name, c := range map[string]struct {
		form    SessionScriptForm
		current storedScript
	}{
		"blank":                {SessionScriptForm{Role: "analyst", SQL: " \n "}, storedScript{}},
		"a control character":  {SessionScriptForm{Role: "analyst", SQL: "SELECT '\x01'"}, storedScript{}},
		"past 64 KiB":          {SessionScriptForm{Role: "analyst", SQL: strings.Repeat("-", MaxScriptBytes+1)}, storedScript{}},
		"another policy":       {SessionScriptForm{Role: "analyst", SQL: "SELECT 1", OnError: "ignore"}, storedScript{}},
		"another op":           {SessionScriptForm{Role: "analyst", SQL: "SELECT 1", Op: "rename"}, storedScript{}},
		"nothing to drop":      {SessionScriptForm{Role: "analyst", Op: "drop"}, storedScript{}},
		"as stored":            {SessionScriptForm{Role: "analyst", SQL: "SELECT 1", OnError: "continue"}, stored},
		"as stored, fail":      {SessionScriptForm{Role: "analyst", SQL: "SELECT 1"}, storedScript{sql: "SELECT 1"}},
		"a name it cannot say": {SessionScriptForm{Role: "a\nb", SQL: "SELECT 1"}, storedScript{}},
	} {
		if got, err := c.form.statement(c.current); err == nil {
			t.Errorf("%s: %q", name, got)
		}
	}
}

// sessionScriptsNode answers the policy's scripts: etl (a script, bound to analyst, whose grant has one too), general
// (none, the default group), the roles analyst (a script) and auditor (none, in no group); a write and a trial are
// recorded, and a trial on a node of another group than etl is refused as the node would place the person.
func sessionScriptsNode(h *harness, wrote, tried *[]string) {
	answer := h.nodes.answer
	h.nodes.answer = func(n fleet.Node, statement string) (*Result, error) {
		switch statement {
		case stmtGroupScripts:
			return &Result{Columns: []string{"group", "session_script", "session_on_error", "is_default"}, Rows: [][]any{
				{"etl", "SET VARIABLE region = 'etl'", "fail", false}, {"general", nil, nil, true}}}, nil
		case stmtRoleScripts:
			return &Result{Columns: []string{"role", "session_script", "session_on_error"}, Rows: [][]any{
				{"analyst", "SET TimeZone = 'UTC'", "continue"}, {"auditor", nil, nil}}}, nil
		case stmtGrantScripts:
			return &Result{Columns: []string{"role", "group", "session_script", "session_on_error"}, Rows: [][]any{
				{"analyst", "etl", "SELECT 1", "fail"}}}, nil
		}
		if strings.Contains(statement, "SESSION SCRIPT") {
			*wrote = append(*wrote, statement)
			return said(true, nil), nil
		}
		if strings.HasPrefix(statement, "SELECT * FROM platform.try_session_script(") {
			*tried = append(*tried, n.Label+": "+statement)
			return &Result{Columns: []string{"pos", "level", "ok", "message", "line"}, Rows: [][]any{
				{int64(1), "role", false, "syntax error at or near \"=\"", int64(2)},
				{nil, "session", true, "served", nil}}}, nil
		}
		return answer(n, statement)
	}
}

// TestSessionScriptEditor (spec 016): a script is read with its version and the sessions a trial may be of; a preview
// composes, a write runs what was shown; a stale or an unshown edit and an owner the policy lacks are refused with
// nothing written; a trial sends the form and the session as literals, to a node of the session's group; below the
// policy bundle, none of it.
func TestSessionScriptEditor(t *testing.T) {
	info := infoRow([]string{"observe", "policy"}, []string{"node_load", "sessions"}, []string{"try_session_script"})
	h := newHarness(t, info, node("hugr-node-general-a", "general"), node("hugr-node-etl-a", "etl"))
	var wrote, tried []string
	sessionScriptsNode(h, &wrote, &tried)

	code, out, body := h.do(t, "GET", APIPrefix+"policy/session-script?role=analyst", opsToken, "")
	if code != 200 || out["exists"] != true || out["sql"] != "SET TimeZone = 'UTC'" || out["onError"] != "continue" ||
		out["version"] == "" || !reflect.DeepEqual(out["groups"], []any{"etl"}) {
		t.Fatalf("a role's: %d %s", code, body)
	}
	v := out["version"].(string)
	// a role in no group is served by the default group's nodes; a group's script is tried with its bound roles
	if _, out, body = h.do(t, "GET", APIPrefix+"policy/session-script?role=auditor", opsToken, ""); out["exists"] != false ||
		out["version"] != "" || !reflect.DeepEqual(out["groups"], []any{"general"}) {
		t.Errorf("auditor's: %s", body)
	}
	if _, out, body = h.do(t, "GET", APIPrefix+"policy/session-script?group=etl", opsToken, ""); out["onError"] != "fail" ||
		!reflect.DeepEqual(out["roles"], []any{"analyst"}) {
		t.Errorf("etl's: %s", body)
	}
	for _, q := range []string{"role=nobody", "group=nowhere", "group=general&role=analyst", ""} {
		if code, _, _ := h.do(t, "GET", APIPrefix+"policy/session-script?"+q, opsToken, ""); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d", q, code)
		}
	}

	edit := `{"form": {"role": "analyst", "sql": "SET TimeZone = 'Asia/Tokyo';\nSET VARIABLE layer = 'role'", "onError": "fail"}, "version": "` + v + `"}`
	want := "ALTER ROLE \"analyst\" SET SESSION SCRIPT 'SET TimeZone = ''Asia/Tokyo'';\nSET VARIABLE layer = ''role''' ON ERROR FAIL SESSION"
	code, out, body = h.do(t, "POST", APIPrefix+"policy/session-script/preview", opsToken, edit)
	if code != 200 || !reflect.DeepEqual(out["statements"], []any{want}) || len(wrote) != 0 {
		t.Fatalf("preview: %d %s %q", code, body, wrote)
	}
	if code, out, _ := h.do(t, "POST", APIPrefix+"policy/session-script", opsToken, edit); code != http.StatusConflict ||
		out["error"] != "not_shown" {
		t.Errorf("a write without the statement shown: %d %v", code, out)
	}
	shown := strings.Replace(edit, `"version"`, `"statements": [`+jsonString(want)+`], "version"`, 1)
	code, out, body = h.do(t, "POST", APIPrefix+"policy/session-script", opsToken, shown)
	if code != 200 || !reflect.DeepEqual(wrote, []string{want}) || out["refused"] != nil {
		t.Fatalf("write: %d %s %q", code, body, wrote)
	}
	if code, out, _ := h.do(t, "POST", APIPrefix+"policy/session-script", opsToken, strings.Replace(shown, v, "0123", 1)); code != http.StatusConflict ||
		out["error"] != "script_changed" {
		t.Errorf("stale: %d %v", code, out)
	}
	// a grant that is none: the console says so, nothing is sent
	if code, _, _ := h.do(t, "POST", APIPrefix+"policy/session-script/preview", opsToken,
		`{"form": {"group": "general", "role": "analyst", "sql": "SELECT 1"}, "version": ""}`); code != http.StatusUnprocessableEntity {
		t.Errorf("a grant that is none: %d", code)
	}
	// a long script: 64 KiB of quotes is longer in JSON, and still read
	long := strings.Repeat("'", MaxScriptBytes/4)
	if code, _, body := h.do(t, "POST", APIPrefix+"policy/session-script/preview", opsToken,
		`{"form": {"role": "auditor", "sql": `+jsonString(long)+`}, "version": ""}`); code != 200 {
		t.Errorf("a long script: %d %s", code, body)
	}
	if len(wrote) != 1 {
		t.Errorf("a refused edit reached the node: %q", wrote)
	}

	// the trial: on etl's node, every argument a literal, the node's rows answered
	code, out, body = h.do(t, "POST", APIPrefix+"policy/session-script/try", opsToken,
		`{"form": {"role": "analyst", "sql": "SET a = 1;\nSET b =", "onError": "fail"}, "session": {"group": "etl"}}`)
	if code != 200 || out["node"] != "hugr-node-etl-a" || out["group"] != "etl" || len(out["rows"].([]any)) != 2 ||
		!reflect.DeepEqual(tried, []string{"hugr-node-etl-a: SELECT * FROM platform.try_session_script('', 'analyst', " +
			"'SET a = 1;\nSET b =', 'fail', '', 'etl')"}) {
		t.Fatalf("try: %d %s %q", code, body, tried)
	}
	for name, b := range map[string]string{
		"a role's with no group picked": `{"form": {"role": "analyst", "sql": "SELECT 1"}, "session": {}}`,
		"an unlisted session role":      `{"form": {"group": "etl", "sql": "SELECT 1"}, "session": {"role": "nobody"}}`,
		"an unlisted session group":     `{"form": {"role": "analyst", "sql": "SELECT 1"}, "session": {"group": "nowhere"}}`,
		"blank":                         `{"form": {"role": "analyst", "sql": "  "}, "session": {"group": "etl"}}`,
	} {
		if code, _, _ := h.do(t, "POST", APIPrefix+"policy/session-script/try", opsToken, b); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d", name, code)
		}
	}
	if len(tried) != 1 || len(wrote) != 1 {
		t.Errorf("a refused trial reached the node: %q %q", tried, wrote)
	}
	// a node without the function: the trial is not offered
	h3 := newHarness(t, policyInfo, node("hugr-node-etl-a", "etl"))
	sessionScriptsNode(h3, &wrote, &tried)
	if code, _, _ := h3.do(t, "POST", APIPrefix+"policy/session-script/try", opsToken,
		`{"form": {"role": "analyst", "sql": "SELECT 1"}, "session": {"group": "etl"}}`); code != http.StatusForbidden {
		t.Errorf("a node without try_session_script: %d", code)
	}
	// below the policy bundle: no part of it
	h2 := newHarness(t, clusterInfo, node("hugr-node-etl-a", "etl"))
	if code, _, _ := h2.do(t, "GET", APIPrefix+"policy/session-script?role=analyst", opsToken, ""); code != http.StatusForbidden {
		t.Errorf("the cluster bundle read a session script: %d", code)
	}
}

// TestSeatsNobody: a placement refusal, an unavailable or unreachable node, a token not taken pass to the next node; the
// node's own judgment does not.
func TestSeatsNobody(t *testing.T) {
	if !seatsNobody(&NodeError{Groups: []string{"a"}}) || !seatsNobody(errUnreachable) {
		t.Error("a placement refusal and an unreachable node seat nobody")
	}
	if seatsNobody(&NodeError{Message: "does not run the role's script"}) || seatsNobody(errors.New("x")) {
		t.Error("the node's own refusal is the answer")
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestSessionScriptsOrder (spec 016): platform.session_scripts takes a role and a group the node listed - the group
// compared exactly, as the node keeps it - written as literals; a group the node lacks is refused.
func TestSessionScriptsOrder(t *testing.T) {
	info := infoRow([]string{"policy"}, []string{"roles", "grants", "resource_groups"}, []string{"session_scripts"})
	h := newHarness(t, info, node("hugr-node-etl-a", "etl"))
	answer := h.nodes.answer
	h.nodes.answer = func(n fleet.Node, statement string) (*Result, error) {
		switch {
		case statement == argumentLists["role"]:
			return &Result{Columns: []string{"name"}, Rows: [][]any{{"analyst"}}}, nil
		case statement == argumentLists["group"]:
			return &Result{Columns: []string{"name"}, Rows: [][]any{{"etl"}}}, nil
		case strings.HasPrefix(statement, "SELECT * FROM platform.\"session_scripts\""):
			return &Result{Columns: []string{"pos", "level"}, Rows: [][]any{{int64(1), "role"}}}, nil
		}
		return answer(n, statement)
	}
	if code, out, _ := h.do(t, "GET", APIPrefix+"policy/functions/session_scripts?role=analyst&group=etl", opsToken, ""); code != 200 ||
		out["total"] != float64(1) || h.nodes.statements(`SELECT * FROM platform."session_scripts"('analyst', 'etl')`) != 1 {
		t.Fatalf("the order: %d %v", code, out)
	}
	for path, want := range map[string]int{
		"policy/functions/session_scripts?role=analyst":           400, // the group is always named
		"policy/functions/session_scripts?role=analyst&group=ETL": 422, // exactly
		"policy/functions/session_scripts?role=analyst&group=x":   422,
	} {
		if code, _, _ := h.do(t, "GET", APIPrefix+path, opsToken, ""); code != want {
			t.Errorf("%s: %d, want %d", path, code, want)
		}
	}
}
