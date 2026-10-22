package console

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// The policy's pages (spec 007): the platform views and read functions the DBeaver client reads, on a node under the
// person's own token. A view is one of the policy's (policyViews) and one the person reads (console_info's my_views);
// it is read whole and filtered here, so no value from the browser becomes SQL. A read function's arguments are names
// the console read first from the node, written as literals.

// policyViews are the views the policy's pages read: the catalogs, the roles, the identity, the function categories,
// the resource groups - never the node's own (sessions, the load report: their pages are the node's).
var policyViews = map[string]bool{
	"catalogs": true, "relations": true, "relation_columns": true, "schemas": true, "functions": true,
	"function_columns": true, "references": true, "keys": true,
	"grants": true, "schema_grants": true, "object_grants": true, "grant_columns": true,
	"roles": true, "role_claims": true, "admins": true, "platform_grants": true,
	"issuers": true, "clients": true, "role_mappings": true,
	"function_categories": true, "function_category_members": true, "function_grants": true,
	"resource_groups": true, "role_resource_groups": true,
}

// policyFunctions are the read functions the pages call, each with the lists its arguments come from.
var policyFunctions = map[string][]string{
	"effective_rights": {"role", "catalog"},
	"check_catalog":    {"catalog"},
	"exposures":        {"source"},
}

// slowFunctions take the node long (a catalog's check probes every stored fact against its sources; what exposes a
// source reads the attached tree): their own deadline, under the console server's write timeout.
var slowFunctions = map[string]bool{"check_catalog": true, "exposures": true}

// slowCall is that deadline.
const slowCall = 45 * time.Second

// the lists an argument is read from, and the views each reads: a role of the policy's or one a grant names (a grant
// may name a role the policy never created - an IdP's role granted directly), a catalog likewise, a source the node
// has attached
var argumentLists = map[string]string{
	"role":    "SELECT \"role\" AS name FROM platform.roles UNION SELECT \"role\" FROM platform.grants",
	"catalog": "SELECT \"catalog\" AS name FROM platform.catalogs UNION SELECT \"catalog\" FROM platform.grants",
	"source":  stmtSources,
}

var argumentViews = map[string][]string{"role": {"roles", "grants"}, "catalog": {"catalogs", "grants"}}

// foldsCase: the names the node compares with an ASCII fold (duckdb-acl spec 116: catalogs, schemas, sources);
// roles compare exactly
var foldsCase = map[string]bool{"catalog": true, "source": true}

// asciiFold is duckdb-acl's one fold: A-Z to a-z, nothing else (never Unicode's)
func asciiFold(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

// listed finds the value among the node's names, as the node compares them; the answer is the node's own spelling.
func listed(names []string, value string, fold bool) (string, bool) {
	for _, name := range names {
		if name == value || fold && asciiFold(name) == asciiFold(value) {
			return name, true
		}
	}
	return "", false
}

// stmtSources lists the node's attached sources as an administrator reads them (duckdb-acl spec 118.3:
// `platform.attached.<alias>` - never the policy store nor a secrets service), as the DBeaver client does.
const stmtSources = "SELECT schema_name AS name FROM information_schema.schemata WHERE catalog_name = 'platform' " +
	"AND schema_name LIKE 'attached.%' ORDER BY schema_name"

const (
	pageDefault = 500
	pageMax     = 5000
	// viewRowsMax bounds a view the console reads whole: the node bounds no platform view (acl_metadata_max_rows is
	// its metadata surfaces'), so the statement does - a constant, past which the answer says it read the first rows
	viewRowsMax = 100000
)

// literal is a value as an SQL string literal: single quotes, a quote doubled. A value with a control character, or
// none, is not written at all.
func literal(value string) (string, bool) {
	if value == "" || len(value) > 1024 || strings.ContainsFunc(value, unicode.IsControl) {
		return "", false
	}
	return "'" + strings.ReplaceAll(value, "'", "''") + "'", true
}

// keyParts splits a canonical key (duckdb-acl spec 116): parts joined by '.', a part double-quoted when it holds '.'
// or '"' (a quote doubled inside). An unreadable key has no parts.
func keyParts(key string) []string {
	var parts []string
	for len(key) > 0 {
		var part string
		if key[0] == '"' {
			i, closed := 1, false
			for i < len(key) {
				if key[i] == '"' {
					if i+1 < len(key) && key[i+1] == '"' {
						part += `"`
						i += 2
						continue
					}
					closed = true
					i++
					break
				}
				part += string(key[i])
				i++
			}
			if !closed {
				return nil
			}
			key = key[i:]
		} else {
			end := strings.IndexByte(key, '.')
			if end < 0 {
				end = len(key)
			}
			part, key = key[:end], key[end:]
		}
		if part == "" {
			return nil
		}
		parts = append(parts, part)
		if key == "" {
			break
		}
		if key[0] != '.' {
			return nil
		}
		key = key[1:]
		if key == "" {
			return nil
		}
	}
	return parts
}

// sourcesOf the listing: the aliases of `attached.<alias>`.
func sourcesOf(result *Result) []string {
	out := []string{}
	for _, row := range result.Records() {
		key, _ := row["name"].(string)
		if parts := keyParts(key); len(parts) == 2 && parts[0] == "attached" {
			out = append(out, parts[1])
		}
	}
	return out
}

// ViewAnswer is a page of a view's rows (or a read function's answer), filtered and paged here. Truncated: the view held
// more than the console reads, and Total counts the rows it read.
type ViewAnswer struct {
	Columns   []string         `json:"columns"`
	Rows      []map[string]any `json:"rows"`
	Total     int              `json:"total"`
	Truncated bool             `json:"truncated,omitempty"`
}

// text of a scalar, as a filter compares it; a list or a structure has none
func text(v any) (string, bool) {
	switch v := v.(type) {
	case string:
		return v, true
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return fmt.Sprint(v), true
	}
	return "", false
}

// page of the request: its offset and limit, bounded.
func page(r *http.Request) (int, int, bool) {
	offset, limit := 0, pageDefault
	q := r.URL.Query()
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, 0, false
		}
		offset = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > pageMax {
			return 0, 0, false
		}
		limit = n
	}
	return offset, limit, true
}

// answerPage answers the rows that pass the filters (a column equal to a text), a page of them; only the page's rows
// become objects. A result past max rows (0: none) is cut there, and says so.
func answerPage(w http.ResponseWriter, result *Result, filters map[string]string, offset, limit, max int) {
	at := map[string]int{}
	for column := range filters {
		i := slices.Index(result.Columns, column)
		if i < 0 {
			problem(w, http.StatusBadRequest, "bad_request", "no column "+column+" to filter by")
			return
		}
		at[column] = i
	}
	rows := result.Rows
	out := ViewAnswer{Columns: append([]string{}, result.Columns...), Rows: []map[string]any{}}
	if max > 0 && len(rows) > max {
		rows, out.Truncated = rows[:max], true
	}
	for _, row := range rows {
		kept := true
		for column, value := range filters {
			if i := at[column]; i >= len(row) {
				kept = false
			} else if t, ok := text(row[i]); !ok || t != value {
				kept = false
			}
			if !kept {
				break
			}
		}
		if !kept {
			continue
		}
		if out.Total >= offset && len(out.Rows) < limit {
			record := make(map[string]any, len(result.Columns))
			for i, c := range result.Columns {
				if i < len(row) {
					record[c] = row[i]
				}
			}
			out.Rows = append(out.Rows, record)
		}
		out.Total++
	}
	answer(w, out)
}

// filtersOf the request: every query parameter but the page's, each once.
func filtersOf(r *http.Request) (map[string]string, bool) {
	filters := map[string]string{}
	for key, values := range r.URL.Query() {
		if key == "offset" || key == "limit" {
			continue
		}
		if len(values) != 1 {
			return nil, false
		}
		filters[key] = values[0]
	}
	return filters, true
}

func (s *Server) policyView(w http.ResponseWriter, r *http.Request, token string, p *person) {
	view := r.PathValue("view")
	if !policyViews[view] {
		problem(w, http.StatusNotFound, "not_found", "no such view of the policy")
		return
	}
	if !slices.Contains(p.rights.Views, view) {
		problem(w, http.StatusForbidden, "forbidden", "your rights do not read this part of the policy")
		return
	}
	offset, limit, ok := page(r)
	filters, fine := filtersOf(r)
	if !ok || !fine {
		problem(w, http.StatusBadRequest, "bad_request", "a filter once each, offset >= 0, limit 1 to 5000")
		return
	}
	ident, _ := quoted(view)
	results, _, err := s.onNode(r.Context(), token,
		fmt.Sprintf("SELECT * FROM platform.%s LIMIT %d", ident, viewRowsMax+1))
	if err != nil {
		policyProblem(w, err)
		return
	}
	answerPage(w, results[0], filters, offset, limit, viewRowsMax)
}

// policySources lists the node's attached sources, for the exposures' page.
func (s *Server) policySources(w http.ResponseWriter, r *http.Request, token string, p *person) {
	if !slices.Contains(p.rights.Functions, "exposures") {
		problem(w, http.StatusForbidden, "forbidden", "your rights do not read what exposes a source")
		return
	}
	results, _, err := s.onNode(withCallTimeout(r.Context(), slowCall), token, stmtSources)
	if err != nil {
		policyProblem(w, err)
		return
	}
	answer(w, map[string]any{"sources": sourcesOf(results[0])})
}

// policyCall calls a read function with arguments the node listed: each is read first on the node the call then goes
// to, and written as a literal only when the list has it.
func (s *Server) policyCall(w http.ResponseWriter, r *http.Request, token string, p *person) {
	function := r.PathValue("function")
	params, known := policyFunctions[function]
	if !known {
		problem(w, http.StatusNotFound, "not_found", "no such read of the policy")
		return
	}
	if !slices.Contains(p.rights.Functions, function) {
		problem(w, http.StatusForbidden, "forbidden", "your rights do not include this read")
		return
	}
	offset, limit, ok := page(r)
	if !ok {
		problem(w, http.StatusBadRequest, "bad_request", "offset >= 0, limit 1 to 5000")
		return
	}
	q := r.URL.Query()
	lists := make([]string, len(params))
	for i, param := range params {
		if q.Get(param) == "" {
			problem(w, http.StatusBadRequest, "bad_request", "send "+strings.Join(params, ", "))
			return
		}
		for _, view := range argumentViews[param] {
			if !slices.Contains(p.rights.Views, view) {
				problem(w, http.StatusForbidden, "forbidden", "this read takes a "+param+
					" the console checks against platform."+view+", which your rights do not read")
				return
			}
		}
		lists[i] = argumentLists[param]
	}
	ctx := r.Context()
	if slowFunctions[function] {
		ctx = withCallTimeout(ctx, slowCall)
	}
	results, node, err := s.onNode(ctx, token, lists...)
	if err != nil {
		policyProblem(w, err)
		return
	}
	args := make([]string, len(params))
	for i, param := range params {
		value := q.Get(param)
		names := []string{}
		if param == "source" {
			names = sourcesOf(results[i])
		} else {
			for _, row := range results[i].Records() {
				if name, ok := row["name"].(string); ok {
					names = append(names, name)
				}
			}
		}
		// the node's own spelling is written, never the browser's text
		name, found := listed(names, value, foldsCase[param])
		lit, ok := literal(name)
		if !found || !ok {
			problem(w, http.StatusUnprocessableEntity, "refused", "the policy has no "+param+" "+value)
			return
		}
		args[i] = lit
	}
	ident, _ := quoted(function)
	result, err := s.Nodes.Query(ctx, *node, token, "SELECT * FROM platform."+ident+"("+strings.Join(args, ", ")+")")
	if err != nil {
		policyProblem(w, err)
		return
	}
	answerPage(w, result, nil, offset, limit, 0)
}
