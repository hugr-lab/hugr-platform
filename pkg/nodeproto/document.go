// Package nodeproto is the protocol between a node and the orchestrator (spec 001, hugr-node spec 010): the bootstrap
// document a node fetches at its entry, the heartbeat it sends, and the small summary of a heartbeat the fleet reads.
// It has no Kubernetes dependency, so a bare-metal service can import it when one is needed.
package nodeproto

import (
	"errors"
	"fmt"
	"regexp"
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
}

// Heartbeat is how often the node reports.
type Heartbeat struct {
	IntervalSeconds int `json:"interval_seconds"`
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

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
	if len(problems) > 0 {
		return errors.New("the bootstrap document: " + strings.Join(problems, "; "))
	}
	return nil
}
