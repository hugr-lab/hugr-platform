// Package autoscale decides a node group's size from its nodes' load (spec 011). Pure: the group's scaling, its
// nodes as their Leases say, the demand and what was decided before in; the next replicas, the reason and what to
// keep out. The operator's leader asks it at every status look of an auto group and writes what it answers.
package autoscale

import (
	"fmt"
	"time"
)

// Defaults of a group's scaling (spec 011, the owner's decisions of 2026-11-16).
const (
	DefaultIdle           = 30 * time.Minute
	DefaultFor            = 60 * time.Second
	DefaultCooldown       = 2 * time.Minute
	DefaultMemoryUsed     = 0.8
	DefaultTempUsed       = 0.8
	DefaultSessionsUsed   = 0.9
	DefaultSeatsUsed      = 0.9
	DefaultStreamsWaiting = 1
)

// Scaling is a group's scaling, its defaults filled in.
type Scaling struct {
	Min, Max int32
	Idle     time.Duration
	For      time.Duration
	Cooldown time.Duration
	// the shares (0..1) and the count past which a node is under pressure
	MemoryUsed, TempUsed, SessionsUsed, SeatsUsed float64
	StreamsWaiting                                int64
}

// Node is one node of the group as its Lease says. A share is 0..1, or below 0 when the node did not report it.
type Node struct {
	Name string
	// Lost: the Lease expired. Draining: the node seats no one new.
	Lost, Draining bool
	Sessions       int64
	SessionsMax    int64
	MemoryUsed     float64
	TempUsed       float64
	SeatsUsed      float64
	StreamsWaiting int64
	// IdleSince is since when the node has had no live session; zero while it has one (or did not say).
	IdleSince time.Time
	// Default: the node's group is the policy's default, which keeps at least one node.
	Default bool
}

// State is what the autoscaler keeps between its looks.
type State struct {
	// PressureSince: every placeable node has been past a threshold since then (zero: not now).
	PressureSince time.Time
	// LastChange: when it last changed replicas (zero: never).
	LastChange time.Time
}

// Decision is the next size, why, and the state to keep.
type Decision struct {
	Replicas int32
	// Reason is empty when replicas do not change.
	Reason string
	State  State
}

// Decide the group's next replicas. replicas is what the group asks now; nodes are the reported pods (a pod not yet
// reported is not among them - it is starting); demand: a client of the group is held waiting for a node.
func Decide(s Scaling, replicas int32, nodes []Node, demand bool, state State, now time.Time) Decision {
	next := Decision{Replicas: replicas, State: state}
	change := func(to int32, reason string) Decision {
		return Decision{Replicas: to, Reason: reason, State: State{LastChange: now}}
	}
	floor := s.Min
	for _, n := range nodes {
		if n.Default && floor < 1 {
			floor = 1 // the policy's default group seats every new connection first: its entry never goes
		}
	}
	// the bounds first, whatever else holds: a min or max just written
	if replicas < floor {
		return change(floor, fmt.Sprintf("below the minimum of %d", floor))
	}
	if replicas > s.Max {
		return change(s.Max, fmt.Sprintf("above the maximum of %d", s.Max))
	}
	// a client waits for a group with no node: at once, no `for`, no cooldown
	if demand && replicas == 0 {
		return change(max(1, floor), "a client waits for a node of the group")
	}

	var live, placeable []Node
	for _, n := range nodes {
		if n.Lost {
			continue
		}
		live = append(live, n)
		if !n.Draining {
			placeable = append(placeable, n)
		}
	}
	// a node on its way (asked for, not reported yet) is capacity coming: no step until it is here. A lost node has
	// reported - it is no capacity, and it is not on its way either (the scale-down takes it first, cost -3)
	starting := int(replicas) > len(nodes)

	// growth: every placeable node past a threshold for `for`
	pressure := ""
	if len(placeable) > 0 {
		if first := s.pressure(placeable[0]); first != "" {
			pressure = placeable[0].Name + ": " + first
		}
		for _, n := range placeable[1:] {
			if s.pressure(n) == "" {
				pressure = ""
				break
			}
		}
	}
	if pressure == "" {
		next.State.PressureSince = time.Time{}
	} else if next.State.PressureSince.IsZero() {
		next.State.PressureSince = now
	}
	cooling := !state.LastChange.IsZero() && now.Sub(state.LastChange) < s.Cooldown
	if starting || cooling {
		return next
	}
	if pressure != "" && replicas < s.Max && now.Sub(next.State.PressureSince) >= s.For {
		return change(replicas+1, fmt.Sprintf("every node under load for %s (%s)", s.For, pressure))
	}

	// shrinking: a node idle past `idle` stops while above the floor - never a busy one, never the last with a client
	// waiting
	if replicas > floor && !(demand && replicas == 1) {
		for _, n := range live {
			if IdlePast(n, s.Idle, now) {
				return change(replicas-1, fmt.Sprintf("%s has had no session for %s", n.Name,
					now.Sub(n.IdleSince).Truncate(time.Second)))
			}
		}
	}
	return next
}

// pressure names the threshold a node is past, or "".
func (s Scaling) pressure(n Node) string {
	switch {
	case n.MemoryUsed >= 0 && n.MemoryUsed >= s.MemoryUsed:
		return fmt.Sprintf("%.0f%% of its working memory", s.MemoryUsed*100)
	case n.TempUsed >= 0 && n.TempUsed >= s.TempUsed:
		return fmt.Sprintf("%.0f%% of its spill", s.TempUsed*100)
	case n.SessionsMax > 0 && float64(n.Sessions)/float64(n.SessionsMax) >= s.SessionsUsed:
		return fmt.Sprintf("%.0f%% of its sessions", s.SessionsUsed*100)
	case n.SeatsUsed >= 0 && n.SeatsUsed >= s.SeatsUsed:
		return fmt.Sprintf("%.0f%% of its quack seats", s.SeatsUsed*100)
	case s.StreamsWaiting > 0 && n.StreamsWaiting >= s.StreamsWaiting:
		return fmt.Sprintf("%d streams waiting", s.StreamsWaiting)
	}
	return ""
}

// IdlePast says whether a node has had no session for at least idle (the pod-deletion cost's idle).
func IdlePast(n Node, idle time.Duration, now time.Time) bool {
	return !n.Lost && n.Sessions == 0 && !n.IdleSince.IsZero() && now.Sub(n.IdleSince) >= idle
}
