package console

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"unicode"

	"google.golang.org/grpc/codes"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
)

// A group in the policy (spec 006): the policy's resource group its nodes serve (acl_node_group), and the roles bound
// to it. Until the policy has it the group's nodes drain themselves; the console offers the statements a person would
// write in the DBeaver client - on a node, under the person's own token, judged by the node (the policy bundle).
const (
	stmtRoles              = "SELECT \"role\" FROM platform.roles ORDER BY \"role\""
	stmtResourceGroups     = "SELECT \"group\", is_default FROM platform.resource_groups"
	stmtRoleResourceGroups = "SELECT \"role\", \"group\" FROM platform.role_resource_groups"
)

// quoted is a name as an SQL identifier of the ACL's grammar: double quotes, a quote doubled. A name with a control
// character, or none, is not written at all.
func quoted(name string) (string, bool) {
	if name == "" || len(name) > 256 || strings.ContainsFunc(name, unicode.IsControl) {
		return "", false
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`, true
}

// GroupPolicyAnswer is `GET groups/{g}/policy`: whether the policy has the group's resource group, and the roles.
type GroupPolicyAnswer struct {
	ResourceGroup string   `json:"resourceGroup"`
	InPolicy      bool     `json:"inPolicy"`
	Default       bool     `json:"default"`
	Roles         []string `json:"roles"`
	Bound         []string `json:"bound"`
}

// GroupPolicyAdd is `POST groups/{g}/policy`: the roles to bind, and whether a new resource group is the default one.
type GroupPolicyAdd struct {
	Roles   []string `json:"roles"`
	Default bool     `json:"default,omitempty"`
}

// resourceGroupOf the group: its spec's, else its name (the operator's default).
func resourceGroupOf(group *v1alpha1.HugrNodeGroup) string {
	if group.Spec.ResourceGroup != "" {
		return group.Spec.ResourceGroup
	}
	return group.Name
}

// onNode runs the statements on one node, in order, under the person's token: the first answering node that seats
// the person (one of the groups a refusal names, as for the rights), the rest on that same node.
func (s *Server) onNode(ctx context.Context, token string, statements ...string) ([]*Result, error) {
	view, err := s.platformView(ctx)
	if err != nil {
		return nil, err
	}
	var groups []string
	var last error = errNoNode
	var pick *fleet.Node
	var first *Result
	for _, n := range candidates(view, s.now(), s.random()) {
		if len(groups) > 0 && !slices.Contains(groups, n.Group()) {
			continue
		}
		result, err := s.Nodes.Query(ctx, *n, token, statements[0])
		var refused *NodeError
		switch {
		case err == nil:
			pick, first = n, result
		case errors.As(err, &refused) && len(refused.Groups) > 0:
			groups, last = refused.Groups, err
			continue
		case errors.Is(err, errUnreachable), authFailed(err),
			errors.As(err, &refused) && refused.Code == codes.Unavailable:
			last = err
			continue
		default:
			return nil, err // the node judged the statement
		}
		break
	}
	if pick == nil {
		return nil, last
	}
	out := []*Result{first}
	for _, statement := range statements[1:] {
		result, err := s.Nodes.Query(ctx, *pick, token, statement)
		if err != nil {
			return out, err
		}
		out = append(out, result)
	}
	return out, nil
}

// policyState reads the group's place in the policy: the roles, the resource groups, their bindings.
func (s *Server) policyState(ctx context.Context, token, group string) (*GroupPolicyAnswer, error) {
	results, err := s.onNode(ctx, token, stmtRoles, stmtResourceGroups, stmtRoleResourceGroups)
	if err != nil {
		return nil, err
	}
	out := &GroupPolicyAnswer{ResourceGroup: group, Roles: []string{}, Bound: []string{}}
	for _, row := range results[0].Records() {
		if role, ok := row["role"].(string); ok {
			out.Roles = append(out.Roles, role)
		}
	}
	for _, row := range results[1].Records() {
		if row["group"] == group {
			out.InPolicy = true
			out.Default, _ = row["is_default"].(bool)
		}
	}
	for _, row := range results[2].Records() {
		if role, ok := row["role"].(string); ok && row["group"] == group {
			out.Bound = append(out.Bound, role)
		}
	}
	slices.Sort(out.Bound)
	return out, nil
}

// policyProblem answers a failed policy call: a node's refusal as nodeProblem does, no node as the rights' lookup.
func policyProblem(w http.ResponseWriter, err error) {
	if errors.Is(err, errNoNode) {
		problem(w, http.StatusServiceUnavailable, "no_node", "no node answers: the policy cannot be read or written now")
		return
	}
	nodeProblem(w, err)
}

func (s *Server) groupPolicy(w http.ResponseWriter, r *http.Request, token string, _ *person) {
	_, group, ok := s.platformGroup(w, r)
	if !ok {
		return
	}
	state, err := s.policyState(r.Context(), token, resourceGroupOf(group))
	if err != nil {
		policyProblem(w, err)
		return
	}
	answer(w, state)
}

// addGroupToPolicy writes the group's resource group (when the policy has none of that name) and binds the roles
// asked for: names the node listed, each written as an identifier. A repeat binds again (the node's write replaces
// the binding), so a call cut half way is finished by the next.
func (s *Server) addGroupToPolicy(w http.ResponseWriter, r *http.Request, token string, p *person) {
	var in GroupPolicyAdd
	if err := decodeStrict(r, &in); err != nil {
		problem(w, http.StatusBadRequest, "bad_request", "send {\"roles\": [...][, \"default\": true]}")
		return
	}
	_, group, ok := s.platformGroup(w, r)
	if !ok {
		return
	}
	name := resourceGroupOf(group)
	target, ok := quoted(name)
	if !ok {
		problem(w, http.StatusUnprocessableEntity, "refused", "the group's resource group has no name the policy takes")
		return
	}
	state, err := s.policyState(r.Context(), token, name)
	if err != nil {
		policyProblem(w, err)
		return
	}
	var statements []string
	if !state.InPolicy {
		create := "CREATE RESOURCE GROUP " + target
		if in.Default {
			create += " DEFAULT"
		}
		statements = append(statements, create)
	}
	for _, role := range in.Roles {
		if !slices.Contains(state.Roles, role) {
			problem(w, http.StatusUnprocessableEntity, "refused", "the policy has no role "+role)
			return
		}
		ident, ok := quoted(role)
		if !ok {
			problem(w, http.StatusUnprocessableEntity, "refused", "a role's name the console does not write")
			return
		}
		statements = append(statements, "GRANT RESOURCE GROUP "+target+" TO ROLE "+ident)
	}
	if len(statements) == 0 {
		answer(w, state)
		return
	}
	if _, err := s.onNode(r.Context(), token, statements...); err != nil {
		s.Log.Info("group not added to the policy", "group", group.Name, "by", p.subject, "error", err.Error())
		policyProblem(w, err)
		return
	}
	s.Log.Info("group added to the policy", "group", group.Name, "resourceGroup", name, "by", p.subject,
		"created", !state.InPolicy, "roles", len(in.Roles))
	if state, err = s.policyState(r.Context(), token, name); err != nil {
		policyProblem(w, err)
		return
	}
	answer(w, state)
}

// removeGroupFromPolicy drops the group's resource group: its role bindings go too, and the group's nodes drain
// themselves (they serve no group the policy has).
func (s *Server) removeGroupFromPolicy(w http.ResponseWriter, r *http.Request, token string, p *person) {
	_, group, ok := s.platformGroup(w, r)
	if !ok {
		return
	}
	name := resourceGroupOf(group)
	target, ok := quoted(name)
	if !ok {
		problem(w, http.StatusUnprocessableEntity, "refused", "the group's resource group has no name the policy takes")
		return
	}
	if _, err := s.onNode(r.Context(), token, "DROP RESOURCE GROUP IF EXISTS "+target); err != nil {
		policyProblem(w, err)
		return
	}
	s.Log.Info("group removed from the policy", "group", group.Name, "resourceGroup", name, "by", p.subject)
	w.WriteHeader(http.StatusNoContent)
}
