package console

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
)

// sourcesInfo is a cluster bundle holder's: the profile's items and the group's check offered.
var sourcesInfo = infoRow([]string{"observe", "operate", "cluster"}, []string{"node_load", "sessions", "cluster_items"},
	[]string{"kill_session", "drain", "resume", "group_sources"})

const etlCheck = `SELECT "group", source, roles, listed, repair FROM platform.group_sources('etl')`

const etlRepair = `CLUSTER SET SOURCES (sales, crm) IN GROUP etl`

// sourcesNode answers the group's check and the profile's items as hugr-node does (spec 035 stage 3): a listed source,
// one the set leaves out with its repair, another group's own source; the set's spec a MAP of texts.
func sourcesNode(h *harness, asked *[]string) {
	answer := h.nodes.answer
	h.nodes.answer = func(n fleet.Node, statement string) (*Result, error) {
		switch {
		case statement == etlCheck:
			return &Result{Columns: []string{"group", "source", "roles", "listed", "repair"}, Rows: [][]any{
				{"etl", "sales", []any{"analyst"}, true, nil},
				{"etl", "crm", []any{"analyst", "ops"}, false, etlRepair},
				{"etl", "only_other", []any{"ops"}, false, `CLUSTER ATTACH ':memory:' AS "only_other" (TYPE duckdb) IN GROUP etl`},
			}}, nil
		case statement == stmtItems:
			return &Result{Columns: []string{"scope", "kind", "name", "spec", "class", "version", "depends_on", "comment"},
				Rows: [][]any{
					{"", "source", "sales", []any{map[string]any{"key": "type", "value": "duckdb"}}, "drain", int64(3), nil, nil},
					{"", "source", "crm", []any{}, "drain", int64(3), nil, nil},
					{"etl", "source", "Sales", []any{}, "drain", int64(3), nil, nil},
					{"other", "source", "only_other", []any{}, "drain", int64(3), nil, nil},
					{"", "setting", "threads", []any{}, "hot", int64(3), nil, nil},
					{"etl", "sources", "sources", []any{
						map[string]any{"key": "names", "value": `["sales"]`},
						map[string]any{"key": "lazy", "value": "true"},
						map[string]any{"key": "idle_seconds", "value": "1800"},
					}, "drain", int64(4), nil, nil},
				}}, nil
		case strings.HasPrefix(statement, "CLUSTER "):
			*asked = append(*asked, statement)
			return &Result{Columns: []string{"ok", "message", "version", "class", "applied_here"},
				Rows: [][]any{{true, nil, int64(8), "drain", false}}}, nil
		}
		return answer(n, statement)
	}
}

// TestGroupSources: the group's set read from its profile item, the check's rows as the node gave them - a listed
// source, one left out with its repair; the check only for the cluster bundle's function.
func TestGroupSources(t *testing.T) {
	h := newHarness(t, sourcesInfo, node("hugr-node-etl-a", "etl"))
	var asked []string
	sourcesNode(h, &asked)
	code, out, body := h.do(t, "GET", APIPrefix+"groups/etl/sources", opsToken, "")
	if code != 200 || out["resourceGroup"] != "etl" {
		t.Fatalf("sources: %d %s", code, body)
	}
	set := out["set"].(map[string]any)
	if set["all"] != false || set["lazy"] != true || set["idleSeconds"] != float64(1800) ||
		!reflect.DeepEqual(set["names"], []any{"sales"}) {
		t.Errorf("the set: %v", set)
	}
	// the profile's sources for the group: the cluster's and its own, a re-point once, another group's none
	if !reflect.DeepEqual(out["profile"], []any{"sales", "crm"}) {
		t.Errorf("the profile's sources: %v", out["profile"])
	}
	rows := out["rows"].([]any)
	crm := rows[1].(map[string]any)
	if len(rows) != 3 || crm["source"] != "crm" || crm["listed"] != false || crm["repair"] != etlRepair ||
		!reflect.DeepEqual(crm["roles"], []any{"analyst", "ops"}) || rows[0].(map[string]any)["repair"] != nil ||
		crm["elsewhere"] != nil {
		t.Errorf("the rows: %v", rows)
	}
	// another group's own source: not the profile's for etl
	if other := rows[2].(map[string]any); other["elsewhere"] != true {
		t.Errorf("another group's source: %v", other)
	}
	if len(asked) != 0 {
		t.Errorf("a read wrote: %q", asked)
	}

	// without the cluster bundle's function: refused, the node not asked
	o := newHarness(t, operateInfo, node("hugr-node-etl-a", "etl"))
	if code, _, _ := o.do(t, "GET", APIPrefix+"groups/etl/sources", opsToken, ""); code != http.StatusForbidden {
		t.Errorf("without group_sources: %d", code)
	}
	if o.nodes.statements(etlCheck) != 0 {
		t.Error("the check ran without the function")
	}
}

// TestRepairGroupSource: the repair is the node's text, read again where it is written - the browser names a source,
// never a statement; a source the set lists has nothing to repair, an unknown one no row.
func TestRepairGroupSource(t *testing.T) {
	h := newHarness(t, sourcesInfo, node("hugr-node-etl-a", "etl"))
	var asked []string
	sourcesNode(h, &asked)
	shown := `{"source": "crm", "repair": "` + etlRepair + `"}`
	code, out, body := h.do(t, "POST", APIPrefix+"groups/etl/sources/repair", opsToken, shown)
	if code != 200 || out["version"] != float64(8) || out["ok"] != true || out["node"] != "hugr-node-etl-a" {
		t.Fatalf("repair: %d %s", code, body)
	}
	if !reflect.DeepEqual(asked, []string{etlRepair}) {
		t.Errorf("the statement the node ran: %q", asked)
	}
	for body, want := range map[string]int{
		`{"source": "sales", "repair": "x"}`:   http.StatusConflict,
		`{"source": "nowhere", "repair": "x"}`: http.StatusNotFound,
		// the repair the person saw is not the node's now: refused, never the node's new text run unseen
		`{"source": "crm", "repair": "CLUSTER SET SOURCES (sales) IN GROUP etl"}`: http.StatusConflict,
		`{"source": "crm"}`:                                  http.StatusBadRequest,
		`{"source": "", "repair": "x"}`:                      http.StatusBadRequest,
		`{"source": "crm", "repair": "x", "statement": "x"}`: http.StatusBadRequest,
	} {
		if code, _, _ := h.do(t, "POST", APIPrefix+"groups/etl/sources/repair", opsToken, body); code != want {
			t.Errorf("%s: %d, want %d", body, code, want)
		}
	}
	if len(asked) != 1 {
		t.Errorf("a refused repair reached a node: %q", asked)
	}
	// the cluster bundle's: an operator without it is refused before any node is asked
	o := newHarness(t, operateInfo, node("hugr-node-etl-a", "etl"))
	if code, _, _ := o.do(t, "POST", APIPrefix+"groups/etl/sources/repair", opsToken, shown); code != http.StatusForbidden {
		t.Errorf("without the cluster bundle: %d", code)
	}
}

// TestGroupSourcesCheckFails: a check the node refuses (a resource group not in the policy, a source whose exposures
// fail) leaves the set answered - it is still to see and to change.
func TestGroupSourcesCheckFails(t *testing.T) {
	h := newHarness(t, sourcesInfo, node("hugr-node-etl-a", "etl"))
	var asked []string
	sourcesNode(h, &asked)
	answer := h.nodes.answer
	h.nodes.answer = func(n fleet.Node, statement string) (*Result, error) {
		if statement == etlCheck {
			return nil, &NodeError{Code: codes.InvalidArgument, Message: `acl: no resource group "etl"`}
		}
		return answer(n, statement)
	}
	code, out, body := h.do(t, "GET", APIPrefix+"groups/etl/sources", opsToken, "")
	if code != 200 || out["checkError"] != `acl: no resource group "etl"` || out["set"] == nil ||
		len(out["rows"].([]any)) != 0 {
		t.Fatalf("a failed check: %d %s", code, body)
	}
}

// TestWriteAvoidsPartialNodes: a node whose group's source set is a list refuses every write before it runs
// (duckdb-acl spec 176), so the write goes to a node of another group; when every node that seats the person is such
// a node, its refusal is the answer.
func TestWriteAvoidsPartialNodes(t *testing.T) {
	partial := func(group string) error {
		return &NodeError{Code: codes.InvalidArgument, Message: `acl: this node attaches the sources of group "` + group +
			`" only - run it on a node of a group that attaches every source`}
	}
	h := newHarness(t, clusterInfo, node("hugr-node-etl-a", "etl"), node("hugr-node-general-a", "general"))
	var wrote []string
	answer := h.nodes.answer
	h.nodes.answer = func(n fleet.Node, statement string) (*Result, error) {
		if strings.HasPrefix(statement, "CLUSTER ") {
			if n.Group() == "etl" {
				return nil, partial("etl")
			}
			wrote = append(wrote, n.Label)
			return &Result{Columns: []string{"ok", "message", "version", "class", "applied_here"},
				Rows: [][]any{{true, nil, int64(5), "hot", true}}}, nil
		}
		return answer(n, statement)
	}
	change := `{"action": "reset-sources", "group": "etl"}`
	for i := 0; i < 8; i++ { // whichever node a try picks first, the write lands on general's
		code, out, body := h.do(t, "POST", APIPrefix+"cluster/profile", opsToken, change)
		if code != 200 || out["node"] != "hugr-node-general-a" {
			t.Fatalf("try %d: %d %s", i, code, body)
		}
	}
	if len(wrote) != 8 {
		t.Errorf("writes: %v", wrote)
	}
	// only partial nodes: the node's refusal, which says where to run it
	p := newHarness(t, clusterInfo, node("hugr-node-etl-a", "etl"))
	answer = p.nodes.answer
	p.nodes.answer = func(n fleet.Node, statement string) (*Result, error) {
		if strings.HasPrefix(statement, "CLUSTER ") {
			return nil, partial("etl")
		}
		return answer(n, statement)
	}
	code, out, _ := p.do(t, "POST", APIPrefix+"cluster/profile", opsToken, change)
	if code != http.StatusForbidden || !strings.Contains(out["message"].(string), "attaches the sources of group") {
		t.Errorf("only partial nodes: %d %v", code, out)
	}
}
