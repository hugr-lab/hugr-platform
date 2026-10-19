// Package fleet is the nodes as the front sees them (spec 002): each a pod of a group's headless Service, with the
// Lease the operator keeps for it. Pure: a View is built from what the informers hold and replaced whole on every
// change; nothing here reads Kubernetes.
package fleet

import (
	"context"
	"math/rand/v2"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

// Protocol is a door of the node.
type Protocol int

// The two doors.
const (
	Flight Protocol = iota
	Quack
)

// Node is one pod of a group.
type Node struct {
	// Label is the pod's name, which the node writes into its connections' identifiers (duckdb-acl spec 134).
	Label string
	// Service is the group's headless Service: its name is the doors' TLS name (hugr-node-<group>.<ns>.svc).
	Service    string
	Address    string
	FlightPort int32 // 0: the Service has no such port
	QuackPort  int32
	Ready      bool
	Serving    bool
	// Lease is nil before the node's first heartbeat.
	Lease *Lease
}

// Lease is what the node's Lease says.
type Lease struct {
	Until   time.Time // renewTime + leaseDurationSeconds
	Summary nodeproto.Summary
}

// Port of a door, 0 when the node does not open it.
func (n *Node) Port(p Protocol) int32 {
	if p == Quack {
		return n.QuackPort
	}
	return n.FlightPort
}

// Current says whether the Lease is still renewed.
func (n *Node) Current(now time.Time) bool {
	return n.Lease != nil && now.Before(n.Lease.Until)
}

// Placeable: a new connection may be seated here - the endpoint ready, the Lease current and admitting.
func (n *Node) Placeable(p Protocol, now time.Time) bool {
	return n.Ready && n.Port(p) != 0 && n.Current(now) && n.Lease.Summary.AdmitSession
}

// Group is the policy's resource group the node serves, as its Lease says (empty before its first heartbeat, or when
// it serves everyone).
func (n *Node) Group() string {
	if n.Lease == nil {
		return ""
	}
	return n.Lease.Summary.Group
}

// View is one snapshot of the fleet; it is never changed once built.
type View struct {
	nodes   []*Node // by label
	byLabel map[string]*Node
}

// NewView of the nodes; a label seen twice keeps its first node.
func NewView(nodes []Node) *View {
	v := &View{byLabel: map[string]*Node{}}
	for i := range nodes {
		n := nodes[i]
		if n.Label == "" || v.byLabel[n.Label] != nil {
			continue
		}
		v.byLabel[n.Label] = &n
		v.nodes = append(v.nodes, &n)
	}
	sort.Slice(v.nodes, func(i, j int) bool { return v.nodes[i].Label < v.nodes[j].Label })
	return v
}

// Routable is the node a label names, while its endpoint serves: a call of a connection it holds goes there,
// placeable or not (a draining node keeps its connections).
func (v *View) Routable(label string, p Protocol) *Node {
	n := v.byLabel[label]
	if n == nil || !(n.Serving || n.Ready) || n.Port(p) == 0 {
		return nil
	}
	return n
}

// Nodes of the view, by label.
func (v *View) Nodes() []*Node { return v.nodes }

// DefaultGroups are the resource groups whose nodes' current Leases say they are the policy's default - one in a
// consistent policy, more while a change of the default reaches the nodes.
func (v *View) DefaultGroups(now time.Time) []string {
	var out []string
	for _, n := range v.nodes {
		if n.Current(now) && n.Lease.Summary.GroupDefault && !slices.Contains(out, n.Group()) {
			out = append(out, n.Group())
		}
	}
	sort.Strings(out)
	return out
}

// Choose a placeable node of the first of the groups that has one, not one of `tried`: of two at random, the one
// with more seats left, then fewer sessions - replicas that never coordinate do not all pile onto one node. No
// groups means every group's nodes. Nil when no group has a placeable node.
func (v *View) Choose(p Protocol, groups []string, tried map[string]bool, now time.Time, rnd *rand.Rand) *Node {
	return v.choose(groups, tried, rnd, func(n *Node) bool { return n.Placeable(p, now) })
}

// ChooseAnswering is Choose over the nodes that answer at all - ready, the door open, the Lease current - whether or
// not they take a new session: for what any node answers alike (quack's discovery document, a CORS preflight).
func (v *View) ChooseAnswering(p Protocol, groups []string, tried map[string]bool, now time.Time, rnd *rand.Rand) *Node {
	return v.choose(groups, tried, rnd, func(n *Node) bool { return n.Ready && n.Port(p) != 0 && n.Current(now) })
}

func (v *View) choose(groups []string, tried map[string]bool, rnd *rand.Rand, ok func(*Node) bool) *Node {
	pick := func(match func(*Node) bool) *Node {
		var candidates []*Node
		for _, n := range v.nodes {
			if !tried[n.Label] && ok(n) && match(n) {
				candidates = append(candidates, n)
			}
		}
		switch len(candidates) {
		case 0:
			return nil
		case 1:
			return candidates[0]
		}
		i := rnd.IntN(len(candidates))
		j := rnd.IntN(len(candidates) - 1)
		if j >= i {
			j++
		}
		return better(candidates[i], candidates[j])
	}
	if len(groups) == 0 {
		return pick(func(*Node) bool { return true })
	}
	for _, g := range groups {
		if n := pick(func(n *Node) bool { return n.Group() == g }); n != nil {
			return n
		}
	}
	return nil
}

func better(a, b *Node) *Node {
	sa, sb := a.Lease.Summary, b.Lease.Summary
	if sa.SeatsLeft >= 0 && sb.SeatsLeft >= 0 && sa.SeatsLeft != sb.SeatsLeft {
		if sa.SeatsLeft > sb.SeatsLeft {
			return a
		}
		return b
	}
	if sb.Sessions < sa.Sessions {
		return b
	}
	return a
}

// Fleet holds the current view; the watcher replaces it, the proxies read it.
type Fleet struct {
	mu      sync.Mutex
	view    *View
	changed chan struct{}
}

// New fleet, empty until the first Set.
func New() *Fleet {
	return &Fleet{view: NewView(nil), changed: make(chan struct{})}
}

// Set the view; whoever waits for a node looks again.
func (f *Fleet) Set(v *View) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.view = v
	close(f.changed)
	f.changed = make(chan struct{})
}

// View now, and a channel closed at the next change.
func (f *Fleet) View() (*View, <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.view, f.changed
}

// Wait for `pick` to find a node, up to `wait` (a node coming up, a drain ending): nil when none came in time, or
// the call ended. `pick` is asked again at every change of the view.
func (f *Fleet) Wait(ctx context.Context, wait time.Duration, pick func(*View) *Node) *Node {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		view, changed := f.View()
		if n := pick(view); n != nil {
			return n
		}
		select {
		case <-changed:
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}
