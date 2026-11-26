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
	"strconv"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

// spec 013: a resource group as the policy holds it - its limits, its comment, whether it is the default group and the
// roles bound to it - edited from the console, whether a group of nodes serves it or not (duckdb-acl spec 085). The
// server composes the statements from the form, shows them, and runs them only while the group is as it was read, as
// the grant editor does (spec 007 stage 2c).

// ResourceGroupLimits are the limits a resource group may name, each a whole number as text (a BIGINT: a browser's
// number rounds past 2^53); nil is unnamed - the node's setting.
type ResourceGroupLimits struct {
	WindowStart   *string `json:"window_start"`
	WindowMax     *string `json:"window_max"`
	BatchBytes    *string `json:"batch_bytes"`
	MaxResultRows *string `json:"max_result_rows"`
	QueuePriority *string `json:"queue_priority"`
	MaxSessions   *string `json:"max_sessions"`
}

// named are the limits in the grammar's order, each with its name.
func (l ResourceGroupLimits) named() []struct {
	name  string
	value *string
} {
	return []struct {
		name  string
		value *string
	}{{"window_start", l.WindowStart}, {"window_max", l.WindowMax}, {"batch_bytes", l.BatchBytes},
		{"max_result_rows", l.MaxResultRows}, {"queue_priority", l.QueuePriority}, {"max_sessions", l.MaxSessions}}
}

// ResourceGroupForm is a resource group as the editor writes it and as the console reads one back.
type ResourceGroupForm struct {
	Name    string              `json:"name"`
	Limits  ResourceGroupLimits `json:"limits"`
	Comment string              `json:"comment"`
	Default bool                `json:"default"`
	Roles   []string            `json:"roles"`
	// Op: write (the default) or drop
	Op string `json:"op,omitempty"`
}

// ResourceGroupRead is `GET policy/resource-group?name=`: the group as the node holds it now (none: Exists false) and
// its version, the roles the policy has (what may be bound), the current default group, and the groups of nodes that
// serve it.
type ResourceGroupRead struct {
	Exists     bool              `json:"exists"`
	Group      ResourceGroupForm `json:"group"`
	Version    string            `json:"version"`
	Roles      []string          `json:"roles"`
	DefaultIs  string            `json:"defaultIs,omitempty"`
	NodeGroups []string          `json:"nodeGroups"`
}

// ResourceGroupEdit is what the editor sends: the form, the version it was made from ("" = a new group), and - to write
// - the statements it was shown.
type ResourceGroupEdit struct {
	Form       ResourceGroupForm `json:"form"`
	Version    string            `json:"version"`
	Statements []string          `json:"statements,omitempty"`
}

var (
	errGroupChanged = errors.New("the resource group changed since it was read: read it again")
	errGroupExists  = errors.New("the policy has a resource group of this name already: open it from its row")
)

const stmtResourceGroupRows = `SELECT "group", window_start, window_max, batch_bytes, max_result_rows, queue_priority, ` +
	`max_sessions, comment, is_default FROM platform.resource_groups`

// resourceGroupState is what the node holds: every group, the bindings, the roles - read on one node.
type resourceGroupState struct {
	groups   map[string]*ResourceGroupForm // by name, as the node spells it
	roles    []string
	defaults string
}

func (s *Server) readResourceGroups(ctx context.Context, token string) (*resourceGroupState, error) {
	results, _, err := s.onNode(ctx, token, stmtRoles, stmtResourceGroupRows, stmtRoleResourceGroups)
	if err != nil {
		return nil, err
	}
	st := &resourceGroupState{groups: map[string]*ResourceGroupForm{}, roles: []string{}}
	for _, row := range results[0].Records() {
		if role, ok := row["role"].(string); ok {
			st.roles = append(st.roles, role)
		}
	}
	limit := func(v any) *string {
		if n, ok := v.(int64); ok {
			s := strconv.FormatInt(n, 10)
			return &s
		}
		return nil
	}
	for _, row := range results[1].Records() {
		name := str(row["group"])
		g := &ResourceGroupForm{Name: name, Comment: str(row["comment"]), Roles: []string{}, Limits: ResourceGroupLimits{
			WindowStart: limit(row["window_start"]), WindowMax: limit(row["window_max"]),
			BatchBytes: limit(row["batch_bytes"]), MaxResultRows: limit(row["max_result_rows"]),
			QueuePriority: limit(row["queue_priority"]), MaxSessions: limit(row["max_sessions"])}}
		g.Default, _ = row["is_default"].(bool)
		if g.Default {
			st.defaults = name
		}
		st.groups[name] = g
	}
	for _, row := range results[2].Records() {
		if g := st.groups[str(row["group"])]; g != nil {
			g.Roles = append(g.Roles, str(row["role"]))
		}
	}
	for _, g := range st.groups {
		slices.Sort(g.Roles)
	}
	return st, nil
}

// groupVersion of a resource group as read: its form's digest ("" = none).
func groupVersion(g *ResourceGroupForm) string {
	if g == nil {
		return ""
	}
	b, _ := json.Marshal(g)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}

// servingGroups are the platform's groups of nodes whose resource group is this one.
func (s *Server) servingGroups(ctx context.Context, name string) ([]string, error) {
	platform, err := s.Platform(ctx)
	if err != nil {
		return nil, err
	}
	var list v1alpha1.HugrNodeGroupList
	if err := s.Reader.List(ctx, &list, client.InNamespace(platform.Namespace)); err != nil {
		return nil, err
	}
	out := []string{}
	for i := range list.Items {
		g := &list.Items[i]
		if g.Spec.Platform == platform.Name && g.DeletionTimestamp == nil && resourceGroupOf(g) == name {
			out = append(out, g.Name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// limitsClause is ` (window_start 8, …)`, or "" when the form names none; a negative value only for queue_priority.
func (l ResourceGroupLimits) clause() (string, error) {
	var items []string
	for _, x := range l.named() {
		if x.value == nil {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(*x.value), 10, 64)
		if err != nil {
			return "", fmt.Errorf("%s is a whole number", x.name)
		}
		if n < 0 && x.name != "queue_priority" {
			return "", fmt.Errorf("%s is 0 or more (0: unlimited, or none)", x.name)
		}
		items = append(items, x.name+" "+strconv.FormatInt(n, 10))
	}
	if len(items) == 0 {
		return "", nil
	}
	return " (" + strings.Join(items, ", ") + ")", nil
}

// statements are what the form writes over the group as it stands (nil: a new one): a new group is created with its
// limits, its mark and its comment, then its roles bound; an existing one is written again whole when its limits or its
// comment change (a re-create keeps its default mark and its bindings, duckdb-acl spec 085), then its mark and its
// bindings change one by one. Roles are the policy's.
func (f ResourceGroupForm) statements(current *ResourceGroupForm, roles []string) ([]string, error) {
	target, ok := quoted(f.Name)
	if !ok {
		return nil, errors.New("the resource group's name is required")
	}
	if f.Op == "drop" {
		if current == nil {
			return nil, fmt.Errorf("the policy has no resource group %s", f.Name)
		}
		if current.Default {
			return nil, errors.New(f.Name + " is the policy's default group: make another the default, or drop the mark, first")
		}
		return []string{"DROP RESOURCE GROUP " + target}, nil
	}
	if f.Op != "" && f.Op != "write" {
		return nil, errors.New("the op is write or drop")
	}
	limits, err := f.Limits.clause()
	if err != nil {
		return nil, err
	}
	// the comment as written: checked only where a statement writes it (a stored one SQL wrote may be any text)
	comment := func() (string, error) {
		c := f.Comment
		if current == nil || c != current.Comment {
			c = strings.TrimSpace(c)
		}
		if c == "" {
			return "", nil
		}
		lit, ok := literal(c)
		if !ok {
			if current != nil && f.Comment == current.Comment {
				return "", errors.New("the stored comment is not one line of at most 1024 characters: change it here, " +
					"or the limits from SQL")
			}
			return "", errors.New("the comment is one line of at most 1024 characters")
		}
		return " COMMENT " + lit, nil
	}
	wanted := []string{}
	for _, role := range f.Roles {
		if !slices.Contains(wanted, role) {
			wanted = append(wanted, role)
		}
	}
	slices.Sort(wanted)
	var had []string
	if current != nil {
		had = current.Roles
	}
	var out []string
	switch {
	case current == nil:
		create := "CREATE RESOURCE GROUP " + target + limits
		if f.Default {
			create += " DEFAULT"
		}
		c, err := comment()
		if err != nil {
			return nil, err
		}
		out = append(out, create+c)
	default:
		sameLimits, _ := current.Limits.clause()
		if sameLimits != limits || current.Comment != f.Comment {
			c, err := comment()
			if err != nil {
				return nil, err
			}
			out = append(out, "CREATE OR REPLACE RESOURCE GROUP "+target+limits+c)
		}
		if f.Default != current.Default {
			if f.Default {
				out = append(out, "ALTER RESOURCE GROUP "+target+" SET DEFAULT")
			} else {
				out = append(out, "ALTER RESOURCE GROUP "+target+" DROP DEFAULT")
			}
		}
	}
	for _, role := range wanted {
		if slices.Contains(had, role) {
			continue
		}
		if !slices.Contains(roles, role) {
			return nil, fmt.Errorf("the policy has no role %s", role)
		}
		ident, _ := quoted(role)
		out = append(out, "GRANT RESOURCE GROUP "+target+" TO ROLE "+ident)
	}
	for _, role := range had {
		if !slices.Contains(wanted, role) {
			ident, ok := quoted(role)
			if !ok {
				return nil, fmt.Errorf("a role's name the console does not write: %s", role)
			}
			out = append(out, "REVOKE RESOURCE GROUP "+target+" FROM ROLE "+ident)
		}
	}
	return out, nil
}

// composedGroup reads the policy again and composes the edit's statements - only while the group is the one the form
// was made from. A new group's name is a plain name (a group of nodes may serve it: every node's bootstrap names it).
func (s *Server) composedGroup(ctx context.Context, token string, edit *ResourceGroupEdit) ([]string, error) {
	st, err := s.readResourceGroups(ctx, token)
	if err != nil {
		return nil, err
	}
	current, spelled := st.lookup(edit.Form.Name)
	if groupVersion(current) != edit.Version {
		if edit.Version == "" {
			return nil, errGroupExists
		}
		return nil, errGroupChanged
	}
	if current == nil && !nodeproto.ValidPlainName(edit.Form.Name) {
		return nil, errors.New("a resource group's name is a plain name: letters, digits, _ and -, at most 63")
	}
	edit.Form.Name = spelled
	// a second default is the node's refusal: said before anything of the edit is written
	if edit.Form.Op != "drop" && edit.Form.Default && (current == nil || !current.Default) && st.defaults != "" &&
		st.defaults != spelled {
		return nil, errDefaultHeld{name: st.defaults}
	}
	if edit.Form.Op == "drop" {
		// its nodes would serve nobody, and drain
		serving, err := s.servingGroups(ctx, spelled)
		if err != nil {
			return nil, errKubernetes
		}
		if len(serving) > 0 {
			return nil, errGroupServed{groups: serving, name: spelled}
		}
	}
	return edit.Form.statements(current, st.roles)
}

// errKubernetes: the groups of nodes could not be read - the platform's, not the person's edit
var errKubernetes = errors.New("the groups could not be read")

type errDefaultHeld struct{ name string }

func (e errDefaultHeld) Error() string {
	return e.name + " is the default group: drop its mark first, then make this one the default"
}

type errGroupServed struct {
	groups []string
	name   string
}

func (e errGroupServed) Error() string {
	return "the groups " + strings.Join(e.groups, ", ") + " serve " + e.name +
		": their nodes would drain - remove them from the policy on their own pages"
}

// lookup a group by name as the node compares it (exactly, as duckdb-acl keeps a resource group's name); the name as
// asked when there is none.
func (st *resourceGroupState) lookup(name string) (*ResourceGroupForm, string) {
	if g := st.groups[name]; g != nil {
		return g, g.Name
	}
	return nil, name
}

func (s *Server) resourceGroupRead(w http.ResponseWriter, r *http.Request, token string, _ *person) {
	name := r.URL.Query().Get("name")
	st, err := s.readResourceGroups(r.Context(), token)
	if err != nil {
		policyProblem(w, err)
		return
	}
	out := ResourceGroupRead{Roles: st.roles, DefaultIs: st.defaults, NodeGroups: []string{},
		Group: ResourceGroupForm{Name: name, Roles: []string{}}}
	if current, _ := st.lookup(name); current != nil {
		out.Exists, out.Group, out.Version = true, *current, groupVersion(current)
	}
	if name != "" {
		if out.NodeGroups, err = s.servingGroups(r.Context(), name); err != nil {
			problem(w, http.StatusServiceUnavailable, "kubernetes", "the groups could not be read")
			return
		}
	}
	answer(w, out)
}

func (s *Server) resourceGroupPreview(w http.ResponseWriter, r *http.Request, token string, _ *person) {
	var edit ResourceGroupEdit
	if err := grantBody(r, &edit); err != nil {
		problem(w, http.StatusBadRequest, "bad_request", "send {\"form\": {...}, \"version\": ...}")
		return
	}
	statements, err := s.composedGroup(r.Context(), token, &edit)
	if err != nil {
		groupProblem(w, err)
		return
	}
	answer(w, GrantWritten{Statements: statements, Answers: []*Answer{}})
}

// resourceGroupWrite runs the edit's statements one at a time on one node that writes, stopping at the first the node
// refuses, and answers what each said.
func (s *Server) resourceGroupWrite(w http.ResponseWriter, r *http.Request, token string, p *person) {
	var edit ResourceGroupEdit
	if err := grantBody(r, &edit); err != nil {
		problem(w, http.StatusBadRequest, "bad_request", "send {\"form\": {...}, \"version\": ...}")
		return
	}
	statements, err := s.composedGroup(r.Context(), token, &edit)
	if err == nil && !slices.Equal(statements, edit.Statements) {
		err = errNotShown
	}
	if err != nil {
		groupProblem(w, err)
		return
	}
	out, err := s.writeAnswered(r.Context(), token, statements)
	if err != nil {
		policyProblem(w, err)
		return
	}
	if out.Refused != nil {
		s.Log.Info("a resource group not written", "group", edit.Form.Name, "by", p.subject, "error", out.Refused.Message)
	}
	answer(w, out)
}

// groupProblem answers a form the console refuses, a group that changed or is served, or the node's own refusal.
func groupProblem(w http.ResponseWriter, err error) {
	var served errGroupServed
	var held errDefaultHeld
	switch {
	case errors.Is(err, errKubernetes):
		problem(w, http.StatusServiceUnavailable, "kubernetes", err.Error())
	case errors.As(err, &held):
		problem(w, http.StatusConflict, "default_held", err.Error())
	case errors.Is(err, errNoNode), errors.Is(err, errUnreachable), errors.As(err, new(*NodeError)):
		policyProblem(w, err)
	case errors.Is(err, errGroupChanged):
		problem(w, http.StatusConflict, "group_changed", err.Error())
	case errors.Is(err, errGroupExists):
		problem(w, http.StatusConflict, "group_exists", err.Error())
	case errors.Is(err, errNotShown):
		problem(w, http.StatusConflict, "not_shown", err.Error())
	case errors.As(err, &served):
		problem(w, http.StatusConflict, "served", err.Error())
	default:
		grantProblem(w, err)
	}
}
