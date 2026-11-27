package console

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"

	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
)

// spec 016: a session's scripts (duckdb-acl spec 181) - a resource group's, a role's, a role's grant of a group - written
// and dropped from the console, and tried first (duckdb-acl spec 183): the node runs what a session would run with the
// form's script in place of the stored one. The editor reads the script with a version, the server composes the
// statement, and the write runs only the statement shown while the script is as it was read (spec 013's way). The
// script's text is never logged.

// SessionScriptForm is a script as the editor writes it: its owner (a group's: Role ""; a role's: Group ""; a grant's:
// both), its SQL and its policy on error.
type SessionScriptForm struct {
	Group   string `json:"group"`
	Role    string `json:"role"`
	SQL     string `json:"sql"`
	OnError string `json:"onError"` // fail (the node's default) | continue
	Op      string `json:"op,omitempty"`
}

// SessionScriptRead is `GET policy/session-script?group=&role=`: the script as stored (none: Exists false) and its
// version, and the sessions a trial may be of - for a group's script the roles bound to the group, for a role's the
// groups it is served on (its own, or else the default group).
type SessionScriptRead struct {
	Exists  bool     `json:"exists"`
	SQL     string   `json:"sql"`
	OnError string   `json:"onError"`
	Version string   `json:"version"`
	Roles   []string `json:"roles"`
	Groups  []string `json:"groups"`
}

// SessionScriptEdit is what the editor writes: the form, the version it was made from, and - to write - the statement
// it was shown.
type SessionScriptEdit struct {
	Form       SessionScriptForm `json:"form"`
	Version    string            `json:"version"`
	Statements []string          `json:"statements,omitempty"`
}

// SessionScriptTry is `POST policy/session-script/try`: the form's script, and the session to try it in.
type SessionScriptTry struct {
	Form    SessionScriptForm `json:"form"`
	Session struct {
		Role  string `json:"role"`
		Group string `json:"group"`
	} `json:"session"`
}

// SessionScriptTried is the node's answer to a trial, with the node that ran it: on a node of another group than the
// session's, its sources and settings are that node's.
type SessionScriptTried struct {
	Node  string           `json:"node"`
	Group string           `json:"group"`
	Rows  []map[string]any `json:"rows"`
}

const (
	stmtGroupScripts = `SELECT "group", session_script, session_on_error, is_default FROM platform.resource_groups`
	stmtRoleScripts  = `SELECT "role", session_script, session_on_error FROM platform.roles`
	stmtGrantScripts = `SELECT "role", "group", session_script, session_on_error FROM platform.role_resource_groups`
)

var errScriptChanged = errors.New("the session script changed since it was read: read it again")

// storedScript is a script as the node holds it ("" = none set).
type storedScript struct {
	sql, onError string
}

// sessionScriptState is what the node holds: each owner with its script, read on one node.
type sessionScriptState struct {
	groups   map[string]storedScript
	roles    map[string]storedScript
	grants   map[[2]string]storedScript // [group, role]
	defaults string
}

func (s *Server) readSessionScripts(ctx context.Context, token string) (*sessionScriptState, error) {
	results, _, err := s.onNode(ctx, token, stmtGroupScripts, stmtRoleScripts, stmtGrantScripts)
	if err != nil {
		return nil, err
	}
	st := &sessionScriptState{groups: map[string]storedScript{}, roles: map[string]storedScript{},
		grants: map[[2]string]storedScript{}}
	script := func(row map[string]any) storedScript {
		return storedScript{sql: str(row["session_script"]), onError: str(row["session_on_error"])}
	}
	for _, row := range results[0].Records() {
		st.groups[str(row["group"])] = script(row)
		if d, _ := row["is_default"].(bool); d {
			st.defaults = str(row["group"])
		}
	}
	for _, row := range results[1].Records() {
		st.roles[str(row["role"])] = script(row)
	}
	for _, row := range results[2].Records() {
		st.grants[[2]string{str(row["group"]), str(row["role"])}] = script(row)
	}
	return st, nil
}

// owner is the stored script of the form's owner, or why the owner is none the policy has. Names compare exactly, as
// duckdb-acl keeps roles and groups.
func (st *sessionScriptState) owner(f SessionScriptForm) (storedScript, error) {
	switch {
	case f.Group == "" && f.Role == "":
		return storedScript{}, errors.New("name a resource group, a role, or both (a role's grant of the group)")
	case f.Group != "" && f.Role != "":
		if _, ok := st.groups[f.Group]; !ok {
			return storedScript{}, fmt.Errorf("the policy has no resource group %s", f.Group)
		}
		if _, ok := st.roles[f.Role]; !ok {
			return storedScript{}, fmt.Errorf("the policy has no role %s", f.Role)
		}
		got, ok := st.grants[[2]string{f.Group, f.Role}]
		if !ok {
			return storedScript{}, fmt.Errorf("role %s is not in resource group %s: a grant's script hangs on the grant",
				f.Role, f.Group)
		}
		return got, nil
	case f.Group != "":
		got, ok := st.groups[f.Group]
		if !ok {
			return storedScript{}, fmt.Errorf("the policy has no resource group %s", f.Group)
		}
		return got, nil
	default:
		got, ok := st.roles[f.Role]
		if !ok {
			return storedScript{}, fmt.Errorf("the policy has no role %s", f.Role)
		}
		return got, nil
	}
}

// sessions a trial of the form's script may be of: for a group's script the roles bound to it, for a role's the groups
// it is in (else the default group)
func (st *sessionScriptState) sessions(f SessionScriptForm) (roles, groups []string) {
	roles, groups = []string{}, []string{}
	for key := range st.grants {
		if f.Group != "" && key[0] == f.Group {
			roles = append(roles, key[1])
		}
		if f.Role != "" && key[1] == f.Role {
			groups = append(groups, key[0])
		}
	}
	if f.Role != "" && len(groups) == 0 && st.defaults != "" {
		groups = append(groups, st.defaults)
	}
	slices.Sort(roles)
	slices.Sort(groups)
	return roles, groups
}

// scriptVersion of a script as read: its digest ("" = none set).
func scriptVersion(s storedScript) string {
	if s.sql == "" {
		return ""
	}
	b, _ := json.Marshal([]string{s.sql, s.onError})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}

// target is the statement's head for the form's owner, each name quoted
func (f SessionScriptForm) target() (string, error) {
	var group, role string
	var ok bool
	if f.Group != "" {
		if group, ok = quoted(f.Group); !ok {
			return "", fmt.Errorf("a resource group's name the console does not write: %s", f.Group)
		}
	}
	if f.Role != "" {
		if role, ok = quoted(f.Role); !ok {
			return "", fmt.Errorf("a role's name the console does not write: %s", f.Role)
		}
	}
	switch {
	case group != "" && role != "":
		return "ALTER RESOURCE GROUP " + group + " FOR ROLE " + role, nil
	case group != "":
		return "ALTER RESOURCE GROUP " + group, nil
	default:
		return "ALTER ROLE " + role, nil
	}
}

// onErrorOf the form: fail unless continue is asked
func (f SessionScriptForm) onErrorOf() (string, error) {
	switch f.OnError {
	case "", "fail":
		return "fail", nil
	case "continue":
		return "continue", nil
	}
	return "", errors.New("on error is fail or continue")
}

// errScriptText says what a script's text must be
var errScriptText = fmt.Errorf("the script is not blank, at most %d KiB of UTF-8, with no control character but a "+
	"line break and a tab", MaxScriptBytes>>10)

// statement the form writes over the script as stored: a SET with its policy always written, or a DROP.
func (f SessionScriptForm) statement(current storedScript) (string, error) {
	head, err := f.target()
	if err != nil {
		return "", err
	}
	switch f.Op {
	case "drop":
		if current.sql == "" {
			return "", errors.New("no session script is set there")
		}
		return head + " DROP SESSION SCRIPT", nil
	case "", "set":
	default:
		return "", errors.New("the op is set or drop")
	}
	onError, err := f.onErrorOf()
	if err != nil {
		return "", err
	}
	lit, ok := scriptLiteral(f.SQL)
	if !ok {
		return "", errScriptText
	}
	stored := current.onError
	if stored == "" {
		stored = "fail"
	}
	if current.sql == f.SQL && stored == onError {
		return "", errors.New("the script is as stored: nothing to write")
	}
	policy := " ON ERROR FAIL SESSION"
	if onError == "continue" {
		policy = " ON ERROR CONTINUE"
	}
	return head + " SET SESSION SCRIPT " + lit + policy, nil
}

// composedScript reads the policy again and composes the edit's statement - only while the script is the one read.
func (s *Server) composedScript(ctx context.Context, token string, edit *SessionScriptEdit) ([]string, error) {
	st, err := s.readSessionScripts(ctx, token)
	if err != nil {
		return nil, err
	}
	current, err := st.owner(edit.Form)
	if err != nil {
		return nil, err
	}
	if scriptVersion(current) != edit.Version {
		return nil, errScriptChanged
	}
	statement, err := edit.Form.statement(current)
	if err != nil {
		return nil, err
	}
	return []string{statement}, nil
}

func (s *Server) sessionScriptRead(w http.ResponseWriter, r *http.Request, token string, _ *person) {
	q := r.URL.Query()
	form := SessionScriptForm{Group: q.Get("group"), Role: q.Get("role")}
	st, err := s.readSessionScripts(r.Context(), token)
	if err != nil {
		policyProblem(w, err)
		return
	}
	current, err := st.owner(form)
	if err != nil {
		scriptProblem(w, err)
		return
	}
	out := SessionScriptRead{Exists: current.sql != "", SQL: current.sql, OnError: current.onError,
		Version: scriptVersion(current)}
	if out.Exists && out.OnError == "" {
		out.OnError = "fail"
	}
	out.Roles, out.Groups = st.sessions(form)
	answer(w, out)
}

func (s *Server) sessionScriptPreview(w http.ResponseWriter, r *http.Request, token string, _ *person) {
	var edit SessionScriptEdit
	if err := decodeStrictUpTo(r, &edit, 3*MaxScriptBytes); err != nil {
		problem(w, http.StatusBadRequest, "bad_request", "send {\"form\": {...}, \"version\": ...}")
		return
	}
	statements, err := s.composedScript(r.Context(), token, &edit)
	if err != nil {
		scriptProblem(w, err)
		return
	}
	answer(w, GrantWritten{Statements: statements, Answers: []*Answer{}})
}

// sessionScriptWrite runs the edit's statement on one node that writes, and answers what it said.
func (s *Server) sessionScriptWrite(w http.ResponseWriter, r *http.Request, token string, p *person) {
	var edit SessionScriptEdit
	if err := decodeStrictUpTo(r, &edit, 3*MaxScriptBytes); err != nil {
		problem(w, http.StatusBadRequest, "bad_request", "send {\"form\": {...}, \"version\": ...}")
		return
	}
	statements, err := s.composedScript(r.Context(), token, &edit)
	if err == nil && !slices.Equal(statements, edit.Statements) {
		err = errNotShown
	}
	if err != nil {
		scriptProblem(w, err)
		return
	}
	out, err := s.writeAnswered(r.Context(), token, statements)
	if err != nil {
		policyProblem(w, err)
		return
	}
	if out.Refused != nil {
		// the owner, never the script's text
		s.Log.Info("a session script not written", "group", edit.Form.Group, "role", edit.Form.Role, "by", p.subject,
			"error", out.Refused.Message)
	}
	answer(w, out)
}

// sessionScriptTry asks the node to run what a session would run with the form's script in place (duckdb-acl spec
// 183): on a node of the session's group when one seats the person, else on one that does.
func (s *Server) sessionScriptTry(w http.ResponseWriter, r *http.Request, token string, p *person) {
	if !slices.Contains(p.rights.Functions, "try_session_script") {
		problem(w, http.StatusForbidden, "forbidden",
			"trying a session script is the policy bundle's, on a node that has platform.try_session_script")
		return
	}
	var in SessionScriptTry
	if err := decodeStrictUpTo(r, &in, 3*MaxScriptBytes); err != nil {
		problem(w, http.StatusBadRequest, "bad_request", "send {\"form\": {...}, \"session\": {\"role\", \"group\"}}")
		return
	}
	ctx := r.Context()
	st, err := s.readSessionScripts(ctx, token)
	if err != nil {
		policyProblem(w, err)
		return
	}
	if _, err := st.owner(in.Form); err != nil {
		scriptProblem(w, err)
		return
	}
	statement, err := in.trial(st)
	if err != nil {
		scriptProblem(w, err)
		return
	}
	group := in.Session.Group
	if group == "" {
		group = in.Form.Group
	}
	result, node, err := s.onNodeOf(withCallTimeout(ctx, slowCall), token, group, statement)
	if err != nil {
		policyProblem(w, err)
		return
	}
	answer(w, SessionScriptTried{Node: node.Label, Group: node.Group(), Rows: result.Records()})
}

// trial is `SELECT * FROM platform.try_session_script(group, role, sql, on_error, session_role, session_group)`, each
// argument a literal: the names the node listed, the script as it is written.
func (in SessionScriptTry) trial(st *sessionScriptState) (string, error) {
	onError, err := in.Form.onErrorOf()
	if err != nil {
		return "", err
	}
	sql, ok := scriptLiteral(in.Form.SQL)
	if !ok {
		return "", errScriptText
	}
	if in.Session.Role != "" {
		if _, ok := st.roles[in.Session.Role]; !ok {
			return "", fmt.Errorf("the policy has no role %s", in.Session.Role)
		}
	}
	if in.Session.Group != "" {
		if _, ok := st.groups[in.Session.Group]; !ok {
			return "", fmt.Errorf("the policy has no resource group %s", in.Session.Group)
		}
	}
	// a role's script is tried on a group the person picked whenever the role has one: otherwise the node answering
	// would decide
	if in.Form.Group == "" && in.Session.Group == "" {
		if _, groups := st.sessions(in.Form); len(groups) > 0 {
			return "", errors.New("pick the group the session is served on")
		}
	}
	args := []string{}
	for _, name := range []string{in.Form.Group, in.Form.Role} {
		args = append(args, nameLiteral(name))
	}
	args = append(args, sql, "'"+onError+"'", nameLiteral(in.Session.Role), nameLiteral(in.Session.Group))
	for _, a := range args {
		if a == "" {
			return "", errors.New("a name the console does not write")
		}
	}
	return "SELECT * FROM platform.try_session_script(" + strings.Join(args, ", ") + ")", nil
}

// nameLiteral is a name the node listed as a literal, ” for none; "" when it cannot be written
func nameLiteral(name string) string {
	if name == "" {
		return "''"
	}
	lit, ok := literal(name)
	if !ok {
		return ""
	}
	return lit
}

// onNodeOf runs one read on a node of the resource group when one seats the person, else as onNode does
func (s *Server) onNodeOf(ctx context.Context, token, group, statement string) (*Result, *fleet.Node, error) {
	if group != "" {
		view, err := s.platformView(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, n := range candidates(view, s.now(), s.random()) {
			if n.Group() != group {
				continue
			}
			result, err := s.Nodes.Query(ctx, *n, token, statement)
			switch {
			case err == nil:
				return result, n, nil
			case seatsNobody(err):
				continue
			default:
				return nil, nil, err // the node judged it, or did not answer in time (it may still be at it)
			}
		}
	}
	results, node, err := s.onNode(ctx, token, statement)
	if err != nil {
		return nil, nil, err
	}
	return results[0], node, nil
}

// seatsNobody: the node did not seat the person (another group's, down, draining, or the token not taken there) - the
// next node may
func seatsNobody(err error) bool {
	var refused *NodeError
	if errors.As(err, &refused) && (len(refused.Groups) > 0 || refused.Code == codes.Unavailable) {
		return true
	}
	return errors.Is(err, errUnreachable) || authFailed(err)
}

// scriptProblem answers a form the console refuses, a script that changed, or the node's own refusal.
func scriptProblem(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errScriptChanged):
		problem(w, http.StatusConflict, "script_changed", err.Error())
	default:
		grantProblem(w, err)
	}
}
