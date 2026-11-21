package console

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// spec 007 stage 2c: the grant editor - a catalog, a schema or an object grant written from a form, as the DBeaver
// client's editors write them (acl-clients spec 008), with the columns of duckdb-acl spec 158 (a test on names and
// masks). The server composes the grammar from the form's fields (the owner, 2026-11-21): names the node listed,
// quoted as identifiers; the row filter a literal; a mask's expression and a versions clause the person's SQL in its
// clause, which the node judges. An edit reads the grant first and is written only while it is as read (its version).

// GrantForm is a grant as the editor writes it, and as the console reads one back.
type GrantForm struct {
	// Level: catalog, schema or object. Op: write (the default) or revoke.
	Level   string `json:"level"`
	Op      string `json:"op,omitempty"`
	Role    string `json:"role"`
	Catalog string `json:"catalog"`
	// Target: the schema's path (schema), the object's name in the catalog (object) - canonical keys as the node
	// lists them
	Target string `json:"target,omitempty"`
	// Caps: nil is unstated (a catalog grant: every data capability; a schema or object grant: inherited), empty none
	Caps *[]string `json:"caps"`
	// a catalog grant's
	Main bool `json:"main,omitempty"`
	// a catalog or object grant's row filter, as written
	RLS string `json:"rls,omitempty"`
	// a catalog or object grant's VERSIONS clause as written (CURRENT, ANY, AT (…), BETWEEN … AND …); "" unstated
	Versions string `json:"versions,omitempty"`
	// an object grant's COLUMNS test and MASK items
	Columns *ColumnTest `json:"columns,omitempty"`
	Masks   []GrantMask `json:"masks,omitempty"`
	// a schema grant's: where the role's CREATE lands, and its comment
	Into        string `json:"into,omitempty"`
	VirtualOnly bool   `json:"virtualOnly,omitempty"`
	Comment     string `json:"comment,omitempty"`
}

// ColumnTest is an object grant's COLUMNS: all (no clause), only these, or every one but these.
type ColumnTest struct {
	Mode  string   `json:"mode"`
	Names []string `json:"names"`
}

// GrantMask is one MASK item: what the role reads in place of the column.
type GrantMask struct {
	Column string `json:"column"`
	Mask   string `json:"mask"`
}

// GrantEdit is what the editor sends: the form, and the version of the grant it was made from ("" = a new grant).
type GrantEdit struct {
	Form    GrantForm `json:"form"`
	Version string    `json:"version"`
}

// GrantRead is `GET policy/grant`: the grant as the node holds it now (none: Exists false), and its version.
type GrantRead struct {
	Exists  bool      `json:"exists"`
	Grant   GrantForm `json:"grant"`
	Version string    `json:"version"`
}

// GrantWritten is what the statements answered, up to the one the node refused.
type GrantWritten struct {
	Statements []string `json:"statements"`
	Answers    []Answer `json:"answers"`
	// Refused: the statement the node refused (its index) and why; the ones before it were written
	Refused *GrantRefusal `json:"refused,omitempty"`
}

// GrantRefusal is the statement the node refused and its message.
type GrantRefusal struct {
	Index   int    `json:"index"`
	Message string `json:"message"`
}

var (
	errGrantChanged = errors.New("the grant changed since it was read: read it again")
	// a capability is a word (duckdb-acl spec 012)
	capWord = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
)

// the views the editor reads: the grants of each level, and the names a form may name
const (
	stmtGrantRows       = `SELECT "role", catalog, is_main, caps, rls, versions FROM platform.grants LIMIT 100001`
	stmtSchemaGrantRows = `SELECT "role", catalog, schema_path, caps, inherited, "into", virtual_only, comment ` +
		`FROM platform.schema_grants LIMIT 100001`
	stmtObjectGrantRows = `SELECT "role", catalog, object, caps, rls, columns, masks, versions FROM platform.object_grants ` +
		`LIMIT 100001`
	stmtSchemaNames = `SELECT catalog, path AS name FROM platform.schemas LIMIT 100001`
	stmtObjectNames = `SELECT catalog, name FROM platform.relations UNION ALL SELECT catalog, name FROM platform.functions ` +
		`LIMIT 100001`
)

// grantBody reads the editor's request: a form with its masks may pass decodeStrict's 4 KiB.
func grantBody(r *http.Request, v any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 256<<10))
	decoder.DisallowUnknownFields()
	return decoder.Decode(v)
}

// path writes a canonical key as a dotted path of identifiers, each part quoted.
func path(key string) (string, bool) {
	parts := keyParts(key)
	if len(parts) == 0 {
		return "", false
	}
	out := make([]string, len(parts))
	for i, p := range parts {
		q, ok := quoted(p)
		if !ok {
			return "", false
		}
		out[i] = q
	}
	return strings.Join(out, "."), true
}

// capsList is a stored caps STRUCT as the capabilities it holds (nil: unstated).
func capsList(v any) *[]string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := []string{}
	for k, held := range m {
		if held == true {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return &out
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// formOf a grant row of its level's view.
func formOf(level string, row map[string]any) GrantForm {
	f := GrantForm{Level: level, Role: str(row["role"]), Catalog: str(row["catalog"]), Caps: capsList(row["caps"])}
	switch level {
	case "catalog":
		f.Main, _ = row["is_main"].(bool)
		f.RLS, f.Versions = str(row["rls"]), str(row["versions"])
	case "schema":
		f.Target, f.Into, f.Comment = str(row["schema_path"]), str(row["into"]), str(row["comment"])
		f.VirtualOnly, _ = row["virtual_only"].(bool)
	case "object":
		f.Target, f.RLS, f.Versions = str(row["object"]), str(row["rls"]), str(row["versions"])
		if c, ok := row["columns"].(map[string]any); ok {
			test := &ColumnTest{Mode: str(c["mode"]), Names: []string{}}
			if names, ok := c["names"].([]any); ok {
				for _, n := range names {
					test.Names = append(test.Names, str(n))
				}
			}
			if test.Mode != "" && test.Mode != "all" {
				f.Columns = test
			}
		}
		if masks, ok := row["masks"].([]any); ok {
			for _, m := range masks {
				if m, ok := m.(map[string]any); ok {
					f.Masks = append(f.Masks, GrantMask{Column: str(m["column"]), Mask: str(m["mask"])})
				}
			}
		}
	}
	return f
}

// version of a grant as read: its form's digest ("" = none).
func version(f *GrantForm) string {
	if f == nil {
		return ""
	}
	b, _ := json.Marshal(f)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}

// grantNames are the node's own lists a form is checked against, and the grants as they stand.
type grantNames struct {
	roles, catalogs []string
	targets         map[string][]string // catalog -> schema paths or objects (the form's level)
	current         *GrantForm
}

func levelStatements(level string) (rows, targets string, ok bool) {
	switch level {
	case "catalog":
		return stmtGrantRows, "", true
	case "schema":
		return stmtSchemaGrantRows, stmtSchemaNames, true
	case "object":
		return stmtObjectGrantRows, stmtObjectNames, true
	}
	return "", "", false
}

// readGrant reads, on one node, the names the form may use and the grant it names as it stands now; the form's
// names are taken in the node's spelling.
func (s *Server) readGrant(ctx context.Context, token string, f *GrantForm) (*grantNames, error) {
	rows, targets, ok := levelStatements(f.Level)
	if !ok {
		return nil, errors.New("the level is catalog, schema or object")
	}
	statements := []string{argumentLists["role"], argumentLists["catalog"], rows}
	if targets != "" {
		statements = append(statements, targets)
	}
	results, _, err := s.onNode(ctx, token, statements...)
	if err != nil {
		return nil, err
	}
	n := &grantNames{targets: map[string][]string{}}
	for _, row := range results[0].Records() {
		n.roles = append(n.roles, str(row["name"]))
	}
	for _, row := range results[1].Records() {
		n.catalogs = append(n.catalogs, str(row["name"]))
	}
	if targets != "" {
		for _, row := range results[3].Records() {
			c := asciiFold(str(row["catalog"]))
			n.targets[c] = append(n.targets[c], str(row["name"]))
		}
	}
	role, ok := listed(n.roles, f.Role, false)
	if !ok {
		return nil, fmt.Errorf("the policy has no role %s", f.Role)
	}
	catalog, ok := listed(n.catalogs, f.Catalog, true)
	if !ok {
		return nil, fmt.Errorf("the policy has no catalog %s", f.Catalog)
	}
	f.Role, f.Catalog = role, catalog
	if targets != "" {
		target, ok := listed(n.targets[asciiFold(catalog)], f.Target, true)
		if !ok {
			return nil, fmt.Errorf("the catalog %s has no %s %s", catalog, f.Level, f.Target)
		}
		f.Target = target
	}
	for _, row := range results[2].Records() {
		if inherited, _ := row["inherited"].(bool); inherited {
			continue // a schema grant's materialised inheritance: no grant of the role's own
		}
		g := formOf(f.Level, row)
		if g.Role == f.Role && asciiFold(g.Catalog) == asciiFold(f.Catalog) &&
			(f.Level == "catalog" || asciiFold(g.Target) == asciiFold(f.Target)) {
			n.current = &g
			break
		}
	}
	return n, nil
}

// capsClause is a GRANT's capabilities: unstated no clause, none CAPS '{}', else WITH (…).
func capsClause(caps *[]string) (string, error) {
	if caps == nil {
		return "", nil
	}
	if len(*caps) == 0 {
		return " CAPS '{}'", nil
	}
	for _, c := range *caps {
		if !capWord.MatchString(c) {
			return "", fmt.Errorf("a capability is a word: %q", c)
		}
	}
	return " WITH (" + strings.Join(*caps, ", ") + ")", nil
}

// capsJSON is ALTER GRANT's SET CAPS value: ” unstated, else the held ones' JSON.
func capsJSON(caps *[]string) (string, error) {
	if caps == nil {
		return "''", nil
	}
	items := []string{}
	for _, c := range *caps {
		if !capWord.MatchString(c) {
			return "", fmt.Errorf("a capability is a word: %q", c)
		}
		items = append(items, strconv.Quote(c)+": true")
	}
	lit, _ := literal("{" + strings.Join(items, ", ") + "}")
	return lit, nil
}

// textLiteral is a form's text as a literal, ” for none.
func textLiteral(field, v string) (string, error) {
	if v == "" {
		return "''", nil
	}
	if len(v) > 64<<10 || strings.ContainsRune(v, 0) {
		return "", fmt.Errorf("%s is too long", field)
	}
	return "'" + strings.ReplaceAll(v, "'", "''") + "'", nil
}

// clause checks a text placed in a clause as the person wrote it: one or more lines, no NUL.
func clause(field, v string) (string, error) {
	v = strings.TrimSpace(v)
	if len(v) > 64<<10 || strings.ContainsRune(v, 0) {
		return "", fmt.Errorf("%s is too long", field)
	}
	return v, nil
}

// columnsClause is an object grant's COLUMNS, and its text as check_grant takes it.
func columnsClause(c *ColumnTest) (clauseText, checkText string, err error) {
	if c == nil || c.Mode == "" || c.Mode == "all" {
		return "", "", nil
	}
	names := []string{}
	for _, n := range c.Names {
		q, ok := quoted(n)
		if !ok {
			return "", "", fmt.Errorf("a column's name: %q", n)
		}
		names = append(names, q)
	}
	if len(names) == 0 {
		return "", "", errors.New("name the columns")
	}
	list := strings.Join(names, ", ")
	switch c.Mode {
	case "only":
		return " COLUMNS (" + list + ")", list, nil
	case "except":
		return " COLUMNS (* EXCLUDE (" + list + "))", "* EXCLUDE (" + list + ")", nil
	}
	return "", "", errors.New("the columns are all, only or except")
}

// masksText is the MASK items as written: `"col" AS <expression>, …`.
func masksText(masks []GrantMask) (string, error) {
	items := []string{}
	for _, m := range masks {
		col, ok := quoted(m.Column)
		if !ok {
			return "", fmt.Errorf("a mask's column: %q", m.Column)
		}
		expr, err := clause("a mask", m.Mask)
		if err != nil {
			return "", err
		}
		if expr == "" {
			return "", fmt.Errorf("the mask of %s is empty", m.Column)
		}
		items = append(items, col+" AS "+expr)
	}
	return strings.Join(items, ", "), nil
}

// statements the form writes, against the grant as it stands (nil: none).
func (f GrantForm) statements(current *GrantForm) ([]string, error) {
	cat, ok := path(f.Catalog)
	if !ok {
		return nil, errors.New("the catalog's name")
	}
	role, ok := quoted(f.Role)
	if !ok {
		return nil, errors.New("the role's name")
	}
	target := cat
	if f.Level != "catalog" {
		t, ok := path(f.Target)
		if !ok {
			return nil, fmt.Errorf("the %s's name", f.Level)
		}
		target = cat + "." + t
	}
	word := map[string]string{"catalog": "CATALOG", "schema": "SCHEMA", "object": "OBJECT"}[f.Level]
	if f.Op == "revoke" {
		if current == nil {
			return nil, errors.New("the role holds no such grant")
		}
		return []string{"REVOKE " + word + " " + target + " FROM ROLE " + role}, nil
	}
	if f.Op != "" && f.Op != "write" {
		return nil, errors.New("the op is write or revoke")
	}
	caps, err := capsClause(f.Caps)
	if err != nil {
		return nil, err
	}
	rls, err := textLiteral("the row filter", strings.TrimSpace(f.RLS))
	if err != nil {
		return nil, err
	}
	versions, err := clause("the versions", f.Versions)
	if err != nil {
		return nil, err
	}
	head := "GRANT " + word + " " + target + " TO ROLE " + role
	switch f.Level {
	case "catalog":
		if current == nil {
			out := head + caps
			if f.Main {
				out += " MAIN"
			}
			if strings.TrimSpace(f.RLS) != "" {
				out += " RLS " + rls
			}
			if versions != "" {
				out += " VERSIONS " + versions
			}
			return []string{out}, nil
		}
		// a catalog grant changes property by property, keeping the rest (its object grants too)
		if versions != strings.TrimSpace(current.Versions) {
			return nil, errors.New("a catalog grant's versions are written with it: revoking it takes its object " +
				"grants too, so change them with the grant written anew")
		}
		alter := "ALTER GRANT CATALOG " + cat + " TO ROLE " + role + " SET "
		var out []string
		if !sameCaps(f.Caps, current.Caps) {
			j, err := capsJSON(f.Caps)
			if err != nil {
				return nil, err
			}
			out = append(out, alter+"CAPS "+j)
		}
		if f.Main != current.Main {
			out = append(out, alter+"MAIN "+strconv.FormatBool(f.Main))
		}
		if strings.TrimSpace(f.RLS) != strings.TrimSpace(current.RLS) {
			out = append(out, alter+"RLS "+rls)
		}
		return out, nil
	case "schema":
		out := head + caps
		if f.Into != "" && f.VirtualOnly {
			return nil, errors.New("a schema grant creates INTO a physical schema or VIRTUAL ONLY, not both")
		}
		if f.Into != "" {
			into, ok := path(f.Into)
			if !ok {
				return nil, errors.New("the physical schema's name")
			}
			out += " INTO " + into
		}
		if f.VirtualOnly {
			out += " VIRTUAL ONLY"
		}
		if f.Comment != "" {
			c, err := textLiteral("the comment", f.Comment)
			if err != nil {
				return nil, err
			}
			out += " COMMENT " + c
		}
		return []string{out}, nil
	default:
		// an object grant is written whole: a GRANT replaces it (caps, RLS, COLUMNS, MASK, VERSIONS)
		out := head + caps
		if strings.TrimSpace(f.RLS) != "" {
			out += " RLS " + rls
		}
		columns, _, err := columnsClause(f.Columns)
		if err != nil {
			return nil, err
		}
		out += columns
		if len(f.Masks) > 0 {
			m, err := masksText(f.Masks)
			if err != nil {
				return nil, err
			}
			out += " MASK (" + m + ")"
		}
		if versions != "" {
			out += " VERSIONS " + versions
		}
		return []string{out}, nil
	}
}

func sameCaps(a, b *[]string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	x, y := slices.Clone(*a), slices.Clone(*b)
	sort.Strings(x)
	sort.Strings(y)
	return slices.Equal(x, y)
}

// grantQuery reads a form's identity from the query: level, role, catalog, target.
func grantQuery(r *http.Request) GrantForm {
	q := r.URL.Query()
	return GrantForm{Level: q.Get("level"), Role: q.Get("role"), Catalog: q.Get("catalog"), Target: q.Get("target")}
}

func (s *Server) grantRead(w http.ResponseWriter, r *http.Request, token string, _ *person) {
	f := grantQuery(r)
	names, err := s.readGrant(r.Context(), token, &f)
	if err != nil {
		grantProblem(w, err)
		return
	}
	out := GrantRead{Grant: f, Version: version(names.current)}
	if names.current != nil {
		out.Exists, out.Grant = true, *names.current
	}
	answer(w, out)
}

// composed reads the grant again and composes the edit's statements - only while the grant is the one the form was
// made from.
func (s *Server) composed(ctx context.Context, token string, edit *GrantEdit) ([]string, error) {
	names, err := s.readGrant(ctx, token, &edit.Form)
	if err != nil {
		return nil, err
	}
	if version(names.current) != edit.Version {
		return nil, errGrantChanged
	}
	return edit.Form.statements(names.current)
}

func (s *Server) grantPreview(w http.ResponseWriter, r *http.Request, token string, _ *person) {
	var edit GrantEdit
	if err := grantBody(r, &edit); err != nil {
		problem(w, http.StatusBadRequest, "bad_request", "send {\"form\": {...}, \"version\": ...}")
		return
	}
	statements, err := s.composed(r.Context(), token, &edit)
	if err != nil {
		grantProblem(w, err)
		return
	}
	answer(w, GrantWritten{Statements: statements, Answers: []Answer{}})
}

// grantWrite runs the edit's statements one at a time on one node, stopping at the first the node refuses, and
// answers what each said (a grant's group-source hint among them, hugr-node spec 035).
func (s *Server) grantWrite(w http.ResponseWriter, r *http.Request, token string, p *person) {
	var edit GrantEdit
	if err := grantBody(r, &edit); err != nil {
		problem(w, http.StatusBadRequest, "bad_request", "send {\"form\": {...}, \"version\": ...}")
		return
	}
	statements, err := s.composed(r.Context(), token, &edit)
	if err != nil {
		grantProblem(w, err)
		return
	}
	out := GrantWritten{Statements: statements, Answers: []Answer{}}
	if len(statements) == 0 {
		answer(w, out)
		return
	}
	results, _, err := s.write(r.Context(), token, statements...)
	for _, result := range results {
		said, aerr := AnswerOf(result)
		if aerr != nil {
			said = Answer{OK: false, Message: "the node answered no (ok, message)"}
		}
		said.Fields = nil
		out.Answers = append(out.Answers, said)
	}
	if err != nil {
		if len(results) == 0 && !errors.As(err, new(*NodeError)) {
			policyProblem(w, err) // nothing ran: the node could not be reached
			return
		}
		s.Log.Info("a grant not written", "level", edit.Form.Level, "by", p.subject, "error", err.Error())
		out.Refused = &GrantRefusal{Index: len(results), Message: personMessage(err)}
	}
	answer(w, out)
}

// grantCheck asks platform.check_grant what writing a catalog or object grant would say, written nowhere.
func (s *Server) grantCheck(w http.ResponseWriter, r *http.Request, token string, p *person) {
	if !slices.Contains(p.rights.Functions, "check_grant") {
		problem(w, http.StatusForbidden, "forbidden", "checking a grant is the policy bundle's")
		return
	}
	var f GrantForm
	if err := grantBody(r, &f); err != nil || f.Level == "schema" {
		problem(w, http.StatusBadRequest, "bad_request", "send a catalog or object grant's form")
		return
	}
	if _, err := s.readGrant(r.Context(), token, &f); err != nil {
		grantProblem(w, err)
		return
	}
	statement, err := f.checkStatement()
	if err != nil {
		problem(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	results, _, err := s.onNode(withCallTimeout(r.Context(), slowCall), token, statement)
	if err != nil {
		policyProblem(w, err)
		return
	}
	answerPage(w, results[0], nil, 0, pageMax, viewRowsMax)
}

// checkStatement is `SELECT * FROM platform.check_grant(role, catalog, object, rls, columns[, caps][, masks])`, every
// argument a literal.
func (f GrantForm) checkStatement() (string, error) {
	lit := textLiteral
	args := []string{}
	for _, a := range [][2]string{{"the role", f.Role}, {"the catalog", f.Catalog}, {"the object", f.Target},
		{"the row filter", strings.TrimSpace(f.RLS)}} {
		v, err := lit(a[0], a[1])
		if err != nil {
			return "", err
		}
		args = append(args, v)
	}
	_, columns, err := columnsClause(f.Columns)
	if err != nil {
		return "", err
	}
	c, _ := lit("the columns", columns)
	args = append(args, c)
	if f.Caps != nil {
		j, err := capsJSON(f.Caps)
		if err != nil {
			return "", err
		}
		args = append(args, "caps := "+j)
	}
	if len(f.Masks) > 0 {
		m, err := masksText(f.Masks)
		if err != nil {
			return "", err
		}
		ml, err := lit("the masks", m)
		if err != nil {
			return "", err
		}
		args = append(args, "masks := "+ml)
	}
	return "SELECT * FROM platform.check_grant(" + strings.Join(args, ", ") + ")", nil
}

// grantProblem answers a form the console refuses, a grant that changed, or the node's own refusal.
func grantProblem(w http.ResponseWriter, err error) {
	var refused *NodeError
	switch {
	case errors.Is(err, errGrantChanged):
		problem(w, http.StatusConflict, "grant_changed", err.Error())
	case errors.As(err, &refused), errors.Is(err, errNoNode), errors.Is(err, errUnreachable):
		policyProblem(w, err)
	default:
		problem(w, http.StatusUnprocessableEntity, "refused", err.Error())
	}
}
