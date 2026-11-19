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
	// WakeOnDemand: something wakes a group at 0 (the front's demand, stage 2). Without it a group keeps one node.
	WakeOnDemand bool
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
	// Default: the node's Lease says its group is the policy's default.
	Default bool
}

// State is what the autoscaler keeps between its looks.
type State struct {
	// PressureSince: every placeable node has been past a threshold since then (zero: not now).
	PressureSince time.Time
	// LastChange: when it last changed replicas (zero: never).
	LastChange time.Time
}

// Input is one look at a group.
type Input struct {
	// Replicas the group asks; Running what its Deployment runs now (the plan keeps it when it refuses a growth).
	Replicas, Running int32
	// Pods are the group's pods that run (not going, not ended; a rollout's surge pod included); Unreported those of
	// them with no Lease yet - a node on its way, or one that does not come up.
	Pods, Unreported int
	// Nodes are the reported pods.
	Nodes []Node
	// Demand: a client of the group is held waiting for a node.
	Demand bool
	// Default: the group is the policy's default (the platform says so; a node's Lease may too).
	Default bool
	// MayGrow: the plan admits a growth (false while it refuses the group).
	MayGrow bool
	State   State
	Now     time.Time
}

// Decision is the next size, why, and the state to keep.
type Decision struct {
	Replicas int32
	// Reason is empty when replicas do not change.
	Reason string
	State  State
}

// Decide the group's next replicas.
func Decide(s Scaling, in Input) Decision {
	replicas, now := in.Replicas, in.Now
	next := Decision{Replicas: replicas, State: in.State}
	change := func(to int32, reason string) Decision {
		return Decision{Replicas: to, Reason: reason, State: State{LastChange: now}}
	}
	floor := s.Min
	isDefault := in.Default
	for _, n := range in.Nodes {
		isDefault = isDefault || n.Default
	}
	if floor < 1 && (isDefault || !s.WakeOnDemand) {
		// the policy's default group seats every new connection first: its entry never goes; and a group nothing
		// can wake from 0 keeps one node
		floor = 1
	}
	// the bounds first, whatever else holds: a max just lowered, a min just raised (a growth: only if admitted)
	if replicas > s.Max {
		return change(s.Max, fmt.Sprintf("above the maximum of %d", s.Max))
	}
	if !in.MayGrow {
		// the plan refuses the group: what it asked past what runs goes back - never below its floor, which it asks
		// like a manual group asks its replicas (a revert below it would be raised again at the next look, admitted,
		// and refused: a flap) - and it may only shrink
		next.State.PressureSince = time.Time{}
		if keep := max(in.Running, min(floor, replicas)); replicas > keep {
			return change(keep, fmt.Sprintf("the plan admits no growth past the %d that run", in.Running))
		}
	} else if replicas < floor {
		return change(floor, fmt.Sprintf("below the minimum of %d", floor))
	}
	// a client waits for a group with no node: at once, no `for`, no cooldown
	if in.MayGrow && in.Demand && replicas == 0 {
		return change(max(1, floor), "a client waits for a node of the group")
	}
	var placeable []Node
	for _, n := range in.Nodes {
		if !n.Lost && !n.Draining {
			placeable = append(placeable, n)
		}
	}
	// growth: every placeable node past a threshold for `for`
	pressure := ""
	if in.MayGrow && len(placeable) > 0 {
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
	if !in.State.LastChange.IsZero() && now.Sub(in.State.LastChange) < s.Cooldown {
		return next
	}
	// the last write has not reached the Deployment (its apply fails, or the look is the one that applies it): its
	// effect is not seen yet - a step now would remove another node for the same idle one (the re-review). A surge
	// pod, a pod that does not schedule, a quota: none of them change what the Deployment is asked
	if in.Running != replicas {
		return next
	}
	// a pod not created or not reported yet is a node on its way - or one that does not come up (a surge pod a
	// rollout cannot schedule, a quota): no more nodes until it is here. A shrink still goes: a scale-down takes an
	// unready pod first, and the cooldown - kept in the same write as the replicas - holds a second step until the
	// first is made
	coming := in.Unreported > 0 || in.Pods < int(replicas)
	if pressure != "" && !coming && replicas < s.Max && now.Sub(next.State.PressureSince) >= s.For {
		return change(replicas+1, fmt.Sprintf("every node under load for %s (%s)", s.For, pressure))
	}

	// shrinking: a node idle past `idle` stops while above the floor - never a busy one, never the last with a client
	// waiting
	if replicas > floor && !(in.Demand && replicas == 1) {
		for _, n := range in.Nodes {
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
