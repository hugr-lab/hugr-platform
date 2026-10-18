// Package plan judges node groups against the installation's plan (spec 001): which sizes it allows and how many
// nodes it runs in all. The webhook checks one change; the reconcile checks every group again, since two changes may
// each fit alone and not together. No Kubernetes dependency.
package plan

import (
	"fmt"
	"sort"
	"time"

	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

// MaxGroupName keeps a group's pods' names (hugr-node-<group>-<hash>-<id>, at most 10 + 1 + 5 after the group's)
// within a DNS label: a pod's name is the node's label (duckdb-acl spec 134).
const MaxGroupName = 36

// Plan is what the installation allows.
type Plan struct {
	MaxNodes int32
	Sizes    []string        // the plan's sizes
	Known    map[string]bool // the platform's sizes
}

// Group is one group as the plan sees it.
type Group struct {
	Name     string
	Size     string
	Replicas int32     // what the group asks for
	Running  int32     // what its Deployment runs now (kept when the group is refused)
	Created  time.Time // the order the reconcile admits groups in
}

// CheckName says whether a group's name makes nodes' labels.
func CheckName(name string) error {
	if !nodeproto.ValidLabel(name, MaxGroupName) {
		return fmt.Errorf("the group's name %q is not a DNS label of at most %d characters - its pods' names are the "+
			"nodes' labels", name, MaxGroupName)
	}
	return nil
}

// CheckSize says whether the plan allows a size.
func (p Plan) CheckSize(size string) error {
	if !p.Known[size] {
		return fmt.Errorf("the size %q is not one the platform lists", size)
	}
	for _, s := range p.Sizes {
		if s == size {
			return nil
		}
	}
	return fmt.Errorf("the size %q is not in the plan (%v)", size, p.Sizes)
}

// CheckChange is the webhook's: the changed group against every other group as it stands, summed.
func (p Plan) CheckChange(change Group, others []Group) error {
	if err := CheckName(change.Name); err != nil {
		return err
	}
	if err := p.CheckSize(change.Size); err != nil {
		return err
	}
	total := change.Replicas
	for _, g := range others {
		if g.Name != change.Name {
			total += g.Replicas
		}
	}
	if total > p.MaxNodes {
		return fmt.Errorf("the plan runs at most %d nodes, and the groups would ask %d", p.MaxNodes, total)
	}
	return nil
}

// Admit is the reconcile's: every group of the namespace again, since two changes may each pass the webhook alone and
// not together. What runs counts, as it runs: a group that does not grow fits; the growing ones, in the order of their
// creation (the name breaks a tie), each only while everything running plus its growth stays within MaxNodes - so the
// reconcile never takes the namespace past the plan by itself. A scale-down frees its nodes when its Deployment runs
// fewer, never before (its own apply may not happen): the growth after it waits a reconcile. A group that does not
// fit, or whose name or size the installation does not allow, keeps what it runs. Answers the refused groups with
// their reasons.
func (p Plan) Admit(groups []Group) map[string]string {
	ordered := append([]Group(nil), groups...)
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].Created.Equal(ordered[j].Created) {
			return ordered[i].Created.Before(ordered[j].Created)
		}
		return ordered[i].Name < ordered[j].Name
	})
	refused := map[string]string{}
	var committed int32 // the nodes the namespace runs, or will once the admitted changes are made
	var growing []Group
	for _, g := range ordered {
		committed += g.Running
	}
	for _, g := range ordered {
		if err := CheckName(g.Name); err != nil {
			refused[g.Name] = err.Error()
			continue
		}
		if err := p.CheckSize(g.Size); err != nil {
			refused[g.Name] = err.Error() + " - it keeps the " + fmt.Sprint(g.Running) + " it runs"
			continue
		}
		if g.Replicas <= g.Running {
			continue
		}
		growing = append(growing, g)
	}
	for _, g := range growing {
		if committed+g.Replicas-g.Running > p.MaxNodes {
			refused[g.Name] = fmt.Sprintf("the plan runs at most %d nodes: %d run or are admitted, and growing this "+
				"group from %d to %d would take %d - it keeps the %d it runs", p.MaxNodes, committed, g.Running,
				g.Replicas, committed+g.Replicas-g.Running, g.Running)
			continue
		}
		committed += g.Replicas - g.Running
	}
	return refused
}
