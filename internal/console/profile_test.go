package console

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
)

// TestProfileStatement: each form makes one CLUSTER statement in hugr-node's grammar - names plain or quoted, values
// literals - and nothing a person types becomes SQL.
func TestProfileStatement(t *testing.T) {
	for _, tc := range []struct {
		change ProfileChange
		want   string
	}{
		{ProfileChange{Action: "install", Extension: "postgres_scanner", Version: "v2.0.0", Repository: "hugr",
			Group: "etl", Comment: "the lake's"},
			`CLUSTER INSTALL EXTENSION postgres_scanner VERSION 'v2.0.0' FROM hugr IN GROUP "etl" COMMENT 'the lake''s'`},
		{ProfileChange{Action: "update", Extension: "mssql", Version: "v2.0.1", Repository: "ignored"},
			"CLUSTER UPDATE EXTENSION mssql VERSION 'v2.0.1'"},
		{ProfileChange{Action: "remove", Extension: "mssql", Comment: "an update's or a remove's has none"},
			"CLUSTER REMOVE EXTENSION mssql"},
		{ProfileChange{Action: "attach", Path: "host=pg dbname=lake", Alias: `Lake "eu"`, Type: "postgres",
			Secret: "lake_ro", ReadOnly: true, Lineage: "postgres://pg:5432/lake", DependsOn: []string{"base", "q\"s"},
			Group: "etl", Comment: "c"},
			`CLUSTER ATTACH 'host=pg dbname=lake' AS "Lake ""eu""" (TYPE postgres, SECRET lake_ro, READ_ONLY) ` +
				`LINEAGE 'postgres://pg:5432/lake' DEPENDS ON ("base", "q""s") IN GROUP "etl" COMMENT 'c'`},
		{ProfileChange{Action: "attach", Path: ":memory:", Alias: "scratch", Type: "duckdb"},
			`CLUSTER ATTACH ':memory:' AS "scratch" (TYPE duckdb)`},
		{ProfileChange{Action: "detach", Alias: "scratch", Cascade: true, Force: true, Group: "etl"},
			`CLUSTER DETACH "scratch" CASCADE FORCE IN GROUP "etl"`},
		{ProfileChange{Action: "set", Setting: "threads", Value: "4"}, "CLUSTER SET threads = '4'"},
		{ProfileChange{Action: "set", Setting: "preserve_insertion_order", Value: "false", Group: "etl-eu"},
			`CLUSTER SET preserve_insertion_order = 'false' IN GROUP "etl-eu"`},
		{ProfileChange{Action: "set", Setting: "memory_limit", Value: "8GB"}, "CLUSTER SET memory_limit = '8GB'"},
		{ProfileChange{Action: "set", Setting: "s", Value: "x'; DROP TABLE t; --"}, "CLUSTER SET s = 'x''; DROP TABLE t; --'"},
		{ProfileChange{Action: "reset", Setting: "threads", Group: "etl"}, `CLUSTER RESET threads IN GROUP "etl"`},
	} {
		got, err := tc.change.statement()
		if err != nil || got != tc.want {
			t.Errorf("%+v:\n got %q (%v)\nwant %q", tc.change, got, err, tc.want)
		}
	}
	for name, c := range map[string]ProfileChange{
		"no action":              {},
		"another action":         {Action: "drop"},
		"an extension with SQL":  {Action: "install", Extension: "x VERSION '1'; --", Version: "1"},
		"no version":             {Action: "install", Extension: "x"},
		"a repository path":      {Action: "install", Extension: "x", Version: "1", Repository: "https://evil"},
		"no path":                {Action: "attach", Alias: "a", Type: "duckdb"},
		"no alias":               {Action: "attach", Path: ":memory:", Type: "duckdb"},
		"a type with SQL":        {Action: "attach", Path: ":memory:", Alias: "a", Type: "duckdb) --"},
		"a secret with SQL":      {Action: "attach", Path: ":memory:", Alias: "a", Type: "duckdb", Secret: "s, x 1"},
		"a line break":           {Action: "attach", Path: "a\nb", Alias: "a", Type: "duckdb"},
		"an empty dependency":    {Action: "attach", Path: ":memory:", Alias: "a", Type: "duckdb", DependsOn: []string{""}},
		"a setting with SQL":     {Action: "set", Setting: "threads = 1; SET x", Value: "1"},
		"no value":               {Action: "set", Setting: "threads"},
		"a group with SQL":       {Action: "reset", Setting: "threads", Group: "etl COMMENT 'x'"},
		"a group's quoted name":  {Action: "reset", Setting: "threads", Group: `"etl"`},
		"a group with a space":   {Action: "reset", Setting: "threads", Group: "etl eu"},
		"a comment's line break": {Action: "install", Extension: "x", Version: "1", Comment: "a\nb"},
	} {
		if got, err := c.statement(); err == nil {
			t.Errorf("%s: written %q", name, got)
		}
	}
}

// TestChangeProfile: the change goes to a node that seats the person, once, as the statement built; the node's answer
// comes back as written; a malformed change reaches no node; the cluster bundle's alone.
func TestChangeProfile(t *testing.T) {
	h := newHarness(t, clusterInfo, node("hugr-node-etl-a", "etl"))
	var asked []string
	answer := h.nodes.answer
	h.nodes.answer = func(n fleet.Node, statement string) (*Result, error) {
		if strings.HasPrefix(statement, "CLUSTER ") {
			asked = append(asked, statement)
			return &Result{Columns: []string{"acl_cluster_attach"}, Rows: [][]any{{map[string]any{"version": float64(7),
				"class": "hot", "applied_here": true, "note": ""}}}}, nil
		}
		return answer(n, statement)
	}
	code, out, _ := h.do(t, "POST", APIPrefix+"cluster/profile", opsToken,
		`{"action": "attach", "path": ":memory:", "alias": "scratch", "type": "duckdb", "group": "etl"}`)
	if code != 200 || out["class"] != "hot" || out["version"] != float64(7) || out["appliedHere"] != true ||
		out["node"] != "hugr-node-etl-a" {
		t.Fatalf("a change: %d %v", code, out)
	}
	if len(asked) != 1 || asked[0] != `CLUSTER ATTACH ':memory:' AS "scratch" (TYPE duckdb) IN GROUP "etl"` {
		t.Errorf("the statement the node ran: %q", asked)
	}
	for _, body := range []string{`{}`, `not json`, `{"action": "set", "setting": "x; y", "value": "1"}`,
		`{"action": "attach", "path": ":memory:", "alias": "a", "type": "duckdb", "unknown": 1}`} {
		if code, _, _ := h.do(t, "POST", APIPrefix+"cluster/profile", opsToken, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, code)
		}
	}
	if len(asked) != 1 {
		t.Errorf("a refused change reached a node: %q", asked)
	}
}
