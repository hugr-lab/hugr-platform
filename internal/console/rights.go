// Package console is the web console's server (spec 003): the person's rights as the ACL states them, the console's
// API over Kubernetes and the nodes, and the web build it serves. The ACL is the only judge: every page and action is
// shown and checked by what `platform.console_info()` answers for the person's own token, and a node judges each of its
// actions again.
package console

import "slices"

// Info is what `platform.console_info()` answers (duckdb-acl spec 117/125) - the columns the console reads.
type Info struct {
	Build       string   `json:"build"`
	Bundles     []string `json:"bundles"`
	MyViews     []string `json:"my_views"`
	MyFunctions []string `json:"my_functions"`
	Deployment  string   `json:"deployment"`
}

// Rights are what a person may see and do in the console (spec 003, C3).
type Rights struct {
	// Fleet: the groups, their nodes and a node's load report.
	Fleet bool `json:"fleet"`
	// Sessions: a node's sessions.
	Sessions bool `json:"sessions"`
	// EndSession, Drain, Resume: a node's own actions, judged again by the node.
	EndSession bool `json:"endSession"`
	Drain      bool `json:"drain"`
	Resume     bool `json:"resume"`
	// Tracing: a session's audit and profile levels (spec 003 round 2).
	Tracing bool `json:"tracing"`
	// Scale and restart a group: the infrastructure's right, the cluster bundle (spec 003 Q5).
	Scale bool `json:"scale"`
	// Infrastructure: the cluster's profile, drift and a node's logs (the cluster bundle, Q6).
	Infrastructure bool `json:"infrastructure"`
	// Events: a node's audit ring.
	Events bool `json:"events"`
	// Bundles as the node named them, for the person to read.
	Bundles []string `json:"bundles"`
}

// RightsOf the person `info` was answered for. A view or a function the answer does not name is not the person's;
// the cluster bundle comes with passthrough (duckdb-acl spec 118).
func RightsOf(info Info) Rights {
	view := func(name string) bool { return slices.Contains(info.MyViews, name) }
	function := func(name string) bool { return slices.Contains(info.MyFunctions, name) }
	cluster := slices.Contains(info.Bundles, "cluster") || slices.Contains(info.Bundles, "passthrough")
	return Rights{
		Fleet:          view("node_load"),
		Sessions:       view("sessions"),
		EndSession:     function("kill_session"),
		Drain:          function("drain"),
		Resume:         function("resume"),
		Tracing:        function("session_audit_level") && function("session_profile"),
		Scale:          cluster,
		Infrastructure: cluster,
		Events:         view("audit_events"),
		Bundles:        append([]string{}, info.Bundles...), // a list in JSON, never null
	}
}

// Any says whether the person may see anything of the console's own pages.
func (r Rights) Any() bool {
	return r.Fleet || r.Sessions || r.Scale || r.Events
}
