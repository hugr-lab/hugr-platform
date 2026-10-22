package console

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
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
	"function_columns": true, "references": true, "reference_columns": true, "keys": true,
	"grants": true, "schema_grants": true, "object_grants": true, "grant_columns": true,
	"roles": true, "role_claims": true, "admins": true, "platform_grants": true,
	"issuers": true, "clients": true, "role_mappings": true,
	"function_categories": true, "function_category_members": true, "function_grants": true,
	"resource_groups": true, "role_resource_groups": true, "catalog_schema": true,
}

// policyFunctions are the read functions the pages call, each with the lists its arguments come from.
var policyFunctions = map[string][]string{
	"effective_rights": {"role", "catalog"},
	"check_catalog":    {"catalog"},
	"exposures":        {"source"},
}

// the lists an argument is read from: a role of the policy's, a catalog, a source the node has attached
var argumentLists = map[string]string{
	"role":    "SELECT \"role\" AS name FROM platform.roles",
	"catalog": "SELECT \"catalog\" AS name FROM platform.catalogs",
	"source":  stmtSources,
}

// stmtSources lists the node's attached sources as an administrator reads them (duckdb-acl spec 118.3:
// `platform.attached.<alias>` - never the policy store nor a secrets service), as the DBeaver client does.
const stmtSources = "SELECT schema_name AS name FROM information_schema.schemata WHERE catalog_name = 'platform' " +
	"AND schema_name LIKE 'attached.%' ORDER BY schema_name"

const (
	pageDefault = 500
	pageMax     = 5000
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

// ViewAnswer is a page of a view's rows (or a read function's answer), filtered and paged here.
type ViewAnswer struct {
	Columns []string         `json:"columns"`
	Rows    []map[string]any `json:"rows"`
	Total   int              `json:"total"`
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

// answerPage answers the rows that pass the filters (a column equal to a text), a page of them.
func answerPage(w http.ResponseWriter, result *Result, filters map[string]string, offset, limit int) {
	for column := range filters {
		if !slices.Contains(result.Columns, column) {
			problem(w, http.StatusBadRequest, "bad_request", "no column "+column+" to filter by")
			return
		}
	}
	out := ViewAnswer{Columns: append([]string{}, result.Columns...), Rows: []map[string]any{}}
	for _, row := range result.Records() {
		kept := true
		for column, value := range filters {
			if text, ok := row[column].(string); !ok || text != value {
				kept = false
				break
			}
		}
		if !kept {
			continue
		}
		if out.Total >= offset && len(out.Rows) < limit {
			out.Rows = append(out.Rows, row)
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
	results, _, err := s.onNode(r.Context(), token, "SELECT * FROM platform."+ident)
	if err != nil {
		policyProblem(w, err)
		return
	}
	answerPage(w, results[0], filters, offset, limit)
}

// policySources lists the node's attached sources, for the exposures' page.
func (s *Server) policySources(w http.ResponseWriter, r *http.Request, token string, p *person) {
	if !slices.Contains(p.rights.Functions, "exposures") {
		problem(w, http.StatusForbidden, "forbidden", "your rights do not read what exposes a source")
		return
	}
	results, _, err := s.onNode(r.Context(), token, stmtSources)
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
		lists[i] = argumentLists[param]
	}
	results, node, err := s.onNode(r.Context(), token, lists...)
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
		lit, ok := literal(value)
		if !slices.Contains(names, value) || !ok {
			problem(w, http.StatusUnprocessableEntity, "refused", "the policy has no "+param+" "+value)
			return
		}
		args[i] = lit
	}
	ident, _ := quoted(function)
	result, err := s.Nodes.Query(r.Context(), *node, token,
		"SELECT * FROM platform."+ident+"("+strings.Join(args, ", ")+")")
	if err != nil {
		policyProblem(w, err)
		return
	}
	answerPage(w, result, nil, offset, limit)
}
