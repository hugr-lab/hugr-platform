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
		// a source its secret names: no path
		{ProfileChange{Action: "attach", Alias: "sales", Type: "postgres", Secret: "sales"},
			`CLUSTER ATTACH '' AS "sales" (TYPE postgres, SECRET sales)`},
		{ProfileChange{Action: "detach", Alias: "scratch", Cascade: true, Force: true, Group: "etl"},
			`CLUSTER DETACH "scratch" CASCADE FORCE IN GROUP "etl"`},
		{ProfileChange{Action: "set", Setting: "threads", Value: "4"}, "CLUSTER SET threads = '4'"},
		{ProfileChange{Action: "set", Setting: "preserve_insertion_order", Value: "false", Group: "etl-eu"},
			`CLUSTER SET preserve_insertion_order = 'false' IN GROUP "etl-eu"`},
		{ProfileChange{Action: "set", Setting: "memory_limit", Value: "8GB"}, "CLUSTER SET memory_limit = '8GB'"},
		{ProfileChange{Action: "set", Setting: "s", Value: "x'; DROP TABLE t; --"}, "CLUSTER SET s = 'x''; DROP TABLE t; --'"},
		{ProfileChange{Action: "reset", Setting: "threads", Group: "etl"}, `CLUSTER RESET threads IN GROUP "etl"`},
		// spec 007 stage 2b (hugr-node spec 035): a lazy source, a group's source set
		{ProfileChange{Action: "attach", Path: "s3://lake", Alias: "archive", Type: "ducklake", Lazy: true,
			Idle: "30 min", Lineage: "s3://lake"},
			`CLUSTER ATTACH 's3://lake' AS "archive" (TYPE ducklake) LAZY IDLE '30 min' LINEAGE 's3://lake'`},
		{ProfileChange{Action: "attach", Path: ":memory:", Alias: "a", Type: "duckdb", Lazy: true},
			`CLUSTER ATTACH ':memory:' AS "a" (TYPE duckdb) LAZY`},
		{ProfileChange{Action: "sources", Sources: []string{"sales", "Crm"}, Group: "etl"},
			`CLUSTER SET SOURCES ("sales", "Crm") IN GROUP "etl"`},
		{ProfileChange{Action: "sources", Sources: []string{"sales"}, Lazy: true, Idle: "1 hour", Group: "etl"},
			`CLUSTER SET SOURCES ("sales") LAZY IDLE '1 hour' IN GROUP "etl"`},
		{ProfileChange{Action: "sources", All: true, Lazy: true, Group: "apps"},
			`CLUSTER SET SOURCES ALL LAZY IN GROUP "apps"`},
		{ProfileChange{Action: "reset-sources", Group: "etl"}, `CLUSTER RESET SOURCES IN GROUP "etl"`},
	} {
		got, err := tc.change.statement()
		if err != nil || got != tc.want {
			t.Errorf("%+v:\n got %q (%v)\nwant %q", tc.change, got, err, tc.want)
		}
	}
	for name, c := range map[string]ProfileChange{
		"no action":             {},
		"another action":        {Action: "drop"},
		"an extension with SQL": {Action: "install", Extension: "x VERSION '1'; --", Version: "1"},
		"no version":            {Action: "install", Extension: "x"},
		"a repository path":     {Action: "install", Extension: "x", Version: "1", Repository: "https://evil"},
		"no path":               {Action: "attach", Alias: "a", Type: "duckdb"},
		"no alias":              {Action: "attach", Path: ":memory:", Type: "duckdb"},
		"a type with SQL":       {Action: "attach", Path: ":memory:", Alias: "a", Type: "duckdb) --"},
		"a secret with SQL":     {Action: "attach", Path: ":memory:", Alias: "a", Type: "duckdb", Secret: "s, x 1"},
		"a line break":          {Action: "attach", Path: "a\nb", Alias: "a", Type: "duckdb"},
		"an empty dependency":   {Action: "attach", Path: ":memory:", Alias: "a", Type: "duckdb", DependsOn: []string{""}},
		"a comma in a dependency": {Action: "attach", Path: ":memory:", Alias: "a", Type: "duckdb",
			DependsOn: []string{"x,y"}},
		"a comma in an alias":    {Action: "attach", Path: ":memory:", Alias: "a,b", Type: "duckdb"},
		"an alias's edge space":  {Action: "detach", Alias: " a"},
		"a dependency's space":   {Action: "attach", Path: ":memory:", Alias: "a", Type: "duckdb", DependsOn: []string{"x "}},
		"a setting with SQL":     {Action: "set", Setting: "threads = 1; SET x", Value: "1"},
		"no value":               {Action: "set", Setting: "threads"},
		"a group with SQL":       {Action: "reset", Setting: "threads", Group: "etl COMMENT 'x'"},
		"a group's quoted name":  {Action: "reset", Setting: "threads", Group: `"etl"`},
		"a group with a space":   {Action: "reset", Setting: "threads", Group: "etl eu"},
		"a comment's line break": {Action: "install", Extension: "x", Version: "1", Comment: "a\nb"},
		"an idle time alone":     {Action: "attach", Path: ":memory:", Alias: "a", Type: "duckdb", Idle: "30 min"},
		"an idle line break":     {Action: "attach", Path: ":memory:", Alias: "a", Type: "duckdb", Lazy: true, Idle: "1\n"},
		"a cluster's source set": {Action: "sources", Sources: []string{"sales"}},
		"a cluster's reset":      {Action: "reset-sources"},
		"a set of nothing":       {Action: "sources", Group: "etl"},
		"all, not lazy":          {Action: "sources", All: true, Group: "etl"},
		"all and a list":         {Action: "sources", All: true, Lazy: true, Sources: []string{"a"}, Group: "etl"},
		"a set's comma":          {Action: "sources", Sources: []string{"a,b"}, Group: "etl"},
		"a set's idle alone":     {Action: "sources", Sources: []string{"a"}, Idle: "1 hour", Group: "etl"},
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
			// duckdb-acl spec 179: the pair, then the CLUSTER answer's own fields as columns - each its own type, the
			// BIGINT an int64
			return &Result{Columns: []string{"ok", "message", "version", "class", "applied_here"},
				Rows: [][]any{{true, "applied on this node", int64(7), "hot", true}}}, nil
		}
		return answer(n, statement)
	}
	code, out, _ := h.do(t, "POST", APIPrefix+"cluster/profile", opsToken,
		`{"action": "attach", "path": ":memory:", "alias": "scratch", "type": "duckdb", "group": "etl"}`)
	if code != 200 || out["class"] != "hot" || out["version"] != float64(7) || out["appliedHere"] != true ||
		out["node"] != "hugr-node-etl-a" || out["ok"] != true || out["message"] != "applied on this node" {
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
