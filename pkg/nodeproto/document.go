// Package nodeproto is the protocol between a node and the orchestrator (spec 001, hugr-node spec 010): the bootstrap
// document a node fetches at its entry, the heartbeat it sends, and the small summary of a heartbeat the fleet reads.
// It has no Kubernetes dependency, so a bare-metal service can import it when one is needed.
package nodeproto

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// DocumentVersion is the document a node of an orchestrated cluster reads.
const DocumentVersion = 2

// Document is the bootstrap document, version 2. It holds pointers and names only, never a credential.
type Document struct {
	Version    int        `json:"version"`
	NodeGroup  string     `json:"node_group"`
	NodeLabel  string     `json:"node_label"`
	Tresor     Tresor     `json:"tresor"`
	Repository Repository `json:"repository"`
	Catalog    Catalog    `json:"catalog"`
	Heartbeat  Heartbeat  `json:"heartbeat"`
	// FirstPolicy is written by the node that attaches a catalog nobody administers yet (hugr-node spec 015).
	FirstPolicy *FirstPolicy `json:"first_policy,omitempty"`
}

// FirstPolicy is the platform's first policy: names only, never SQL nor a secret.
type FirstPolicy struct {
	Issuer         FirstPolicyIssuer   `json:"issuer"`
	Clients        []FirstPolicyClient `json:"clients"`
	Administrators string              `json:"administrators"`
	ResourceGroups []FirstPolicyGroup  `json:"resource_groups"`
}

// FirstPolicyIssuer is the IdP the people's tokens come from.
type FirstPolicyIssuer struct {
	Name       string   `json:"name"`
	URL        string   `json:"url"`
	Audiences  []string `json:"audiences"`
	RolesClaim string   `json:"roles_claim"`
}

// FirstPolicyClient is an application whose tokens count.
type FirstPolicyClient struct {
	Name     string `json:"name"`
	ClientID string `json:"client_id"`
}

// FirstPolicyGroup is a resource group of the deployment.
type FirstPolicyGroup struct {
	Name    string `json:"name"`
	Default bool   `json:"default,omitempty"`
}

// Tresor is where a node logs in, and as whom.
type Tresor struct {
	Host     string   `json:"host"`
	Identity Identity `json:"identity"`
}

// Identity is the node's workload identity in the organisation's IdP.
type Identity struct {
	Issuer   string `json:"issuer"`
	ClientID string `json:"client_id"`
	Scope    string `json:"scope,omitempty"`
}

// Repository is the extension repository the node trusts by its discovery document.
type Repository struct {
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
}

// Catalog names the policy catalog's reserved secret.
type Catalog struct {
	Secret string `json:"secret"`
	Schema string `json:"schema"`
	// Migrate tells the upgrade's canary to migrate the catalog at its step 5 (spec 008, hugr-node spec 017)
	Migrate bool `json:"migrate,omitempty"`
}

// Heartbeat is how often the node reports.
type Heartbeat struct {
	IntervalSeconds int `json:"interval_seconds"`
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// plainName is what the node takes as a name in the first policy: it goes into a management statement as is
var plainName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,63}$`)

// ValidPlainName says whether a name is one the first policy carries as is - a group's resource group among them:
// every node's bootstrap document lists every group's, so one name it refuses stops every node from starting.
func ValidPlainName(name string) bool { return plainName.MatchString(name) }

// ValidLabel says whether a name is a DNS label of at most max characters (RFC 1123): what a node's label must be
// (duckdb-acl spec 134), and what a group's name must be so that its pods' names are labels too.
func ValidLabel(name string, max int) bool {
	return len(name) <= max && dnsLabel.MatchString(name)
}

// Validate checks what the node checks (hugr-node spec 010), so that the orchestrator never serves a document the
// node would refuse.
func (d Document) Validate() error {
	var problems []string
	if d.Version != DocumentVersion {
		problems = append(problems, fmt.Sprintf("version %d is not %d", d.Version, DocumentVersion))
	}
	if d.NodeGroup == "" {
		problems = append(problems, "no node_group")
	}
	if !ValidLabel(d.NodeLabel, 63) {
		problems = append(problems, "node_label is not a DNS label")
	}
	if d.Tresor.Host == "" || d.Tresor.Identity.Issuer == "" || d.Tresor.Identity.ClientID == "" {
		problems = append(problems, "tresor needs a host, an issuer and a client_id")
	}
	if !strings.HasPrefix(d.Repository.Prefix, "https://") || strings.Contains(d.Repository.Prefix, "@") {
		problems = append(problems, "the repository's prefix is an https URL without userinfo")
	}
	if d.Catalog.Secret == "" {
		problems = append(problems, "no catalog secret")
	}
	if d.Heartbeat.IntervalSeconds < 1 || d.Heartbeat.IntervalSeconds > 3600 {
		problems = append(problems, "heartbeat interval_seconds is 1..3600")
	}
	if d.FirstPolicy != nil {
		problems = append(problems, d.FirstPolicy.problems(d.NodeGroup)...)
	}
	if len(problems) > 0 {
		return errors.New("the bootstrap document: " + strings.Join(problems, "; "))
	}
	return nil
}

// problems are what the node refuses in a first policy (hugr-node spec 015), for a node of nodeGroup
func (p *FirstPolicy) problems(nodeGroup string) []string {
	var problems []string
	add := func(problem string) {
		problems = append(problems, "first_policy: "+problem)
	}
	if !plainName.MatchString(p.Issuer.Name) {
		add("the issuer's name is not a plain name ([A-Za-z0-9_-], at most 63)")
	}
	if !strings.HasPrefix(p.Issuer.URL, "https://") || strings.Contains(p.Issuer.URL, "@") {
		add("the issuer's url is not an https URL without userinfo")
	}
	if p.Issuer.RolesClaim == "" {
		add("no roles_claim")
	}
	if len(p.Issuer.Audiences) == 0 || slices.Contains(p.Issuer.Audiences, "") {
		add("the audiences are not one or more non-empty strings")
	}
	names, ids := map[string]bool{}, map[string]bool{}
	for _, c := range p.Clients {
		if !plainName.MatchString(c.Name) || c.ClientID == "" || names[c.Name] || ids[c.ClientID] {
			add(fmt.Sprintf("the client %q is not a plain name with a client_id, both its own", c.Name))
		}
		names[c.Name], ids[c.ClientID] = true, true
	}
	if len(p.Clients) == 0 {
		add("no client")
	}
	if p.Administrators == "" {
		add("no administrators")
	}
	groups, defaults := map[string]bool{}, 0
	for _, g := range p.ResourceGroups {
		if !plainName.MatchString(g.Name) || groups[g.Name] {
			add(fmt.Sprintf("the resource group %q is not a plain name of its own", g.Name))
		}
		groups[g.Name] = true
		if g.Default {
			defaults++
		}
	}
	if defaults > 1 {
		add("more than one default resource group")
	}
	if !groups[nodeGroup] {
		add("no resource group for the node's own group")
	}
	return problems
}
