package console

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

// spec 007 stage 2b: a group's sources (hugr-node spec 035) - the set its nodes attach, and what its roles reach that
// the set leaves out (`platform.group_sources(g)`, the cluster bundle's), with the repair the node writes. Nothing
// changes the set by itself (the owner, 2026-11-20): a repair runs when the person asks for it, its text the node's.

// GroupSourcesAnswer is `GET groups/{group}/sources`: the group's set (none: every source of the cluster and of the
// group) and the check's rows.
type GroupSourcesAnswer struct {
	ResourceGroup string        `json:"resourceGroup"`
	Set           *SourceSet    `json:"set"`
	Rows          []GroupSource `json:"rows"`
	// CheckError: the check could not be read (a resource group the policy lacks, a source whose exposures fail, a
	// node too slow) - the set is still answered, and may still be changed
	CheckError string `json:"checkError,omitempty"`
	// Profile: the sources the profile has for the group's nodes - the cluster's and the group's own ATTACH items,
	// what a set may list
	Profile []string `json:"profile"`
}

// SourceSet is the group's `sources` item: the aliases it lists, or all; and the lazy marks that replace its sources'
// own.
type SourceSet struct {
	All         bool     `json:"all"`
	Names       []string `json:"names"`
	Lazy        bool     `json:"lazy"`
	IdleSeconds int64    `json:"idleSeconds"`
}

// GroupSource is one row of the check: a source the group's roles reach or its set lists. Listed is nil when the
// group's set lists none (no set, or all lazy), false when the group's nodes do not attach it - Repair is then what to
// run. Elsewhere: another group's own source, which the profile does not attach for this group at all (its repair an
// ATTACH … IN GROUP).
type GroupSource struct {
	Source    string   `json:"source"`
	Roles     []string `json:"roles"`
	Listed    *bool    `json:"listed"`
	Elsewhere bool     `json:"elsewhere,omitempty"`
	Repair    string   `json:"repair,omitempty"`
}

// SourceRepair is `POST groups/{group}/sources/repair`: the source whose row's repair to run, and the repair as the
// person saw it - run only while the node still writes the same.
type SourceRepair struct {
	Source string `json:"source"`
	Repair string `json:"repair"`
}

// sourcesBudget bounds a request of the section under the console server's write timeout (60 s): the check is slow
// (it reads what exposes each source), and a repair still has its write to run after it.
const (
	sourcesBudget = 55 * time.Second
	repairCheck   = 25 * time.Second
)

// stmtGroupSources is the check of one group: its name a literal of a name the platform's group carries (a plain
// name, nodeproto.ValidPlainName).
func stmtGroupSources(group string) (string, bool) {
	if !nodeproto.ValidPlainName(group) {
		return "", false
	}
	lit, _ := literal(group)
	return `SELECT "group", source, roles, listed, repair FROM platform.group_sources(` + lit + `)`, true
}

func (s *Server) groupSources(w http.ResponseWriter, r *http.Request, token string, p *person) {
	if !slices.Contains(p.rights.Functions, "group_sources") {
		problem(w, http.StatusForbidden, "forbidden", "reading a group's sources is the cluster bundle's")
		return
	}
	_, group, ok := s.platformGroup(w, r)
	if !ok {
		return
	}
	name := resourceGroupOf(group)
	check, ok := stmtGroupSources(name)
	if !ok {
		problem(w, http.StatusUnprocessableEntity, "refused", "the group's resource group has no name the policy takes")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sourcesBudget)
	defer cancel()
	// the set first, on its own: a check that fails leaves the set to see and to change
	results, node, err := s.onNode(ctx, token, stmtItems)
	if err != nil {
		policyProblem(w, err)
		return
	}
	out := GroupSourcesAnswer{ResourceGroup: name, Rows: []GroupSource{}, Set: sourceSetOf(results[0], name),
		Profile: profileSources(results[0], name)}
	// the check reads what exposes each source: a slow read, on the same node, never retried elsewhere
	if result, err := s.Nodes.Query(withCallTimeout(ctx, slowCall), *node, token, check); err != nil {
		out.CheckError = personMessage(err)
	} else {
		out.Rows = groupSourceRows(result, out.Profile)
	}
	answer(w, out)
}

// groupSourceRows are the check's rows as the page reads them; a source the profile does not attach for the group is
// another group's own.
func groupSourceRows(result *Result, profile []string) []GroupSource {
	out := []GroupSource{}
	for _, row := range result.Records() {
		source, _ := row["source"].(string)
		g := GroupSource{Source: source, Roles: []string{}}
		if roles, ok := row["roles"].([]any); ok {
			for _, role := range roles {
				if role, ok := role.(string); ok {
					g.Roles = append(g.Roles, role)
				}
			}
		}
		if listed, ok := row["listed"].(bool); ok {
			g.Listed = &listed
		}
		g.Repair, _ = row["repair"].(string)
		if _, ours := listed(profile, source, true); g.Listed != nil && !*g.Listed && !ours {
			g.Elsewhere = true
		}
		out = append(out, g)
	}
	return out
}

// sourceSetOf the profile's items: the group's `sources` item (scope = the group, kind and name `sources`), its spec
// a MAP of texts ({"names": "[…]"} or {"all": "true"}, plus "lazy" and "idle_seconds"); none when the group has none.
func sourceSetOf(items *Result, group string) *SourceSet {
	for _, row := range items.Records() {
		if row["scope"] != group || row["kind"] != "sources" {
			continue
		}
		spec := specMap(row["spec"])
		set := &SourceSet{All: spec["all"] == "true", Names: []string{}, Lazy: spec["lazy"] == "true"}
		if names := spec["names"]; names != "" {
			_ = json.Unmarshal([]byte(names), &set.Names)
		}
		set.IdleSeconds, _ = strconv.ParseInt(spec["idle_seconds"], 10, 64)
		return set
	}
	return nil
}

// profileSources are the profile's ATTACH items the group's nodes may attach: the cluster's and the group's own, each
// name once (a group's item of a cluster source's name re-points it).
func profileSources(items *Result, group string) []string {
	out := []string{}
	for _, row := range items.Records() {
		name, _ := row["name"].(string)
		if row["kind"] != "source" || name == "" || (row["scope"] != "" && row["scope"] != group) {
			continue
		}
		if _, seen := listed(out, name, true); !seen {
			out = append(out, name)
		}
	}
	return out
}

// specMap reads a profile item's spec: Arrow's MAP comes as [{key, value}], a structure as an object.
func specMap(v any) map[string]string {
	out := map[string]string{}
	switch v := v.(type) {
	case []any:
		for _, e := range v {
			if e, ok := e.(map[string]any); ok {
				k, _ := e["key"].(string)
				out[k], _ = text(e["value"])
			}
		}
	case map[string]any:
		for k, e := range v {
			out[k], _ = text(e)
		}
	}
	return out
}

// repairGroupSource runs the repair of one row of the group's check: the check read again, so the statement is the
// node's as it stands now - never a text from the browser - and run only while it is the one the person saw.
func (s *Server) repairGroupSource(w http.ResponseWriter, r *http.Request, token string, _ *person) {
	var in SourceRepair
	if err := decodeStrict(r, &in); err != nil || in.Source == "" || in.Repair == "" {
		problem(w, http.StatusBadRequest, "bad_request", "send {\"source\": alias, \"repair\": the repair shown}")
		return
	}
	_, group, ok := s.platformGroup(w, r)
	if !ok {
		return
	}
	check, ok := stmtGroupSources(resourceGroupOf(group))
	if !ok {
		problem(w, http.StatusUnprocessableEntity, "refused", "the group's resource group has no name the policy takes")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sourcesBudget)
	defer cancel()
	// the check within its own part of the budget: the write keeps the rest
	results, _, err := s.onNode(withCallTimeout(ctx, repairCheck), token, check)
	if err != nil {
		policyProblem(w, err)
		return
	}
	for _, row := range groupSourceRows(results[0], nil) {
		if row.Source != in.Source {
			continue
		}
		switch {
		case row.Repair == "":
			problem(w, http.StatusConflict, "nothing_to_repair", "the check has no repair for "+in.Source+" now")
		case row.Repair != in.Repair:
			problem(w, http.StatusConflict, "repair_changed", "the node's repair for "+in.Source+" changed since "+
				"it was read: read the check again")
		default:
			s.writeProfile(w, ctx, token, row.Repair)
		}
		return
	}
	problem(w, http.StatusNotFound, "not_found", "the group's check has no row for "+in.Source)
}
