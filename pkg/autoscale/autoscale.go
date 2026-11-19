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
	// the scale-down (the owner, 2026-11-19): a group whose load fits in one node fewer drains one, which goes once
	// its sessions have ended
	DefaultScaleDownLoad = 0.6
	DefaultScaleDownFor  = 5 * time.Minute
	DefaultSurplusIdle   = 5 * time.Minute
	// ResumeMargin: the drained node is resumed once the load, on one node fewer, is this far past the scale-down's
	ResumeMargin = 0.1
	// AckWait: a node marked for the scale-down that does not say it drains within this (a build before hugr-node
	// spec 045, a RESUME NODE by hand) is no longer the drained node, and is not picked again; ResumeWait: growth
	// waits at most this long for a resumed node to say it takes connections again. Both are at least 3 heartbeats
	// and a look (a node acts on its answer a heartbeat after it, and says so in the next)
	AckWait    = 2 * time.Minute
	ResumeWait = 2 * time.Minute
	statusLook = 5 * time.Second
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
	// ScaleDownLoad: the group's load, on one node fewer, below which a node is drained for the scale-down, after
	// ScaleDownFor; the drained node goes once it has had no session for SurplusIdle
	ScaleDownLoad float64
	ScaleDownFor  time.Duration
	SurplusIdle   time.Duration
	// Heartbeat: the nodes' heartbeat interval - what a node's answer to the scale-down takes to show
	Heartbeat time.Duration
}

// wait for a node's word: at least d, and 3 heartbeats and a look
func (s Scaling) wait(d time.Duration) time.Duration {
	return max(d, 3*s.Heartbeat+statusLook)
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
	// Started: its first heartbeat (its Lease's creation) - of two equally empty nodes the younger is drained
	Started time.Time
}

// State is what the autoscaler keeps between its looks.
type State struct {
	// PressureSince: every placeable node has been past a threshold since then (zero: not now).
	PressureSince time.Time
	// LastChange: when it last changed replicas (zero: never).
	LastChange time.Time
	// UnderloadSince: the group's load has fitted in one node fewer since then (zero: not now).
	UnderloadSince time.Time
	// Surplus: the node drained for the scale-down ("" none): it takes no new connection, and goes once idle.
	Surplus string
	// MarkedAt: when it was picked; DrainedSince: when it first said it drains (zero: not yet) - its idle time counts
	// from then, never from before the mark
	MarkedAt, DrainedSince time.Time
	// Resuming: the node resumed from the scale-down while it still says it drains (growth waits for it), since when
	Resuming      string
	ResumingSince time.Time
	// Ignored: a node that did not drain when marked - not picked again
	Ignored string
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
		// a step keeps the scale-down's state (the drained node stays drained) - the pressure clock restarts
		st := next.State
		st.LastChange, st.PressureSince, st.UnderloadSince = now, time.Time{}, time.Time{}
		return Decision{Replicas: to, Reason: reason, State: st}
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
	resuming := s.scaleDown(in, floor, pressure != "" && now.Sub(next.State.PressureSince) >= s.For, &next.State)
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
	// a node drained for the scale-down is resumed first: it is capacity the group has, no node is added while it
	// still says it drains (ResumeWait at most)
	if pressure != "" && !coming && !resuming && replicas < s.Max && now.Sub(next.State.PressureSince) >= s.For {
		return change(replicas+1, fmt.Sprintf("every node under load for %s (%s)", s.For, pressure))
	}

	// shrinking: a node idle past `idle` stops while above the floor - never a busy one, never the last with a client
	// waiting. More pods than the Deployment is asked - a rollout's surge pod, a ReplicaSet behind - hold it: mid-rollout
	// a scale-down is split between the ReplicaSets, and the deletion cost ranks only within one, so a busy pod of the
	// old one could go while the idle node stays (the re-review); a stuck rollout then holds the shrink - visible, and
	// better than stopping busy nodes
	if replicas > floor && !(in.Demand && replicas == 1) && in.Pods <= int(in.Running) {
		// the node drained for the scale-down goes first, once its sessions have ended
		for _, n := range in.Nodes {
			if SurplusGoes(n, next.State, s.SurplusIdle, now) {
				// kept as the drained node until its pod is gone: its deletion cost (-2) is what makes the scale-down
				// take it, not a busy one (change keeps it)
				return change(replicas-1, fmt.Sprintf("%s, drained for the scale-down, has had no session for %s",
					n.Name, now.Sub(drainedIdleSince(n, next.State)).Truncate(time.Second)))
			}
		}
		for _, n := range in.Nodes {
			if IdlePast(n, s.Idle, now) {
				return change(replicas-1, fmt.Sprintf("%s has had no session for %s", n.Name,
					now.Sub(n.IdleSince).Truncate(time.Second)))
			}
		}
	}
	return next
}

// scaleDown keeps the scale-down's state (the owner, 2026-11-19) and says whether growth waits for a resumed node.
// What serves new connections is the placeable nodes and the drained one (capacity the group has) - never a node
// drained for another reason (an operator's DRAIN NODE, its resource group gone): that is no capacity, and is never
// picked (the review). When the load of what serves, on one node fewer, has stayed below ScaleDownLoad for
// ScaleDownFor - no node under pressure, no client waiting - one placeable node is drained: the fewest sessions, then
// the youngest. It is resumed when no other node takes connections, a client waits, a node is under pressure, or the
// load is back ResumeMargin past; a node that does not say it drains within AckWait is dropped and not picked again.
func (s Scaling) scaleDown(in Input, floor int32, grows bool, state *State) (resuming bool) {
	now := in.Now
	var live []Node
	for _, n := range in.Nodes {
		if !n.Lost {
			live = append(live, n)
		}
	}
	find := func(name string) (Node, bool) {
		for _, n := range live {
			if n.Name == name {
				return n, true
			}
		}
		return Node{}, false
	}
	clearSurplus := func() {
		state.Surplus, state.MarkedAt, state.DrainedSince, state.UnderloadSince = "", time.Time{}, time.Time{}, time.Time{}
	}
	// a resume in progress: the node still says it drains (ResumeWait at most)
	if state.Resuming != "" {
		if n, ok := find(state.Resuming); !ok || !n.Draining || now.Sub(state.ResumingSince) >= s.wait(ResumeWait) {
			state.Resuming, state.ResumingSince = "", time.Time{}
		}
	}
	if _, ok := find(state.Ignored); !ok {
		state.Ignored = "" // gone: nothing to skip
	}
	if state.Surplus != "" {
		n, ok := find(state.Surplus)
		switch {
		case !ok:
			clearSurplus() // gone: the shrink took it, or it was lost
		case n.Draining && state.DrainedSince.IsZero():
			state.DrainedSince = now // it drains: its idle time counts from now
		case !n.Draining && !state.DrainedSince.IsZero():
			state.Ignored = n.Name // it drained and takes connections again: a RESUME NODE by hand
			clearSurplus()
		case !n.Draining && now.Sub(state.MarkedAt) >= s.wait(AckWait):
			state.Ignored = n.Name // it does not drain (an older build): not ours to wait for
			clearSurplus()
		}
		// on its way out: kept until its pod is gone - while a shrink can take it, or one already asked has not reached
		// the Deployment yet; otherwise (the floor raised, a shrink held by a rollout or a waiting client) the resume
		// rules below apply (the re-review)
		shrinkable := in.Replicas > floor && in.Pods <= int(in.Running) && !(in.Demand && in.Replicas == 1)
		if ok && state.Surplus != "" && SurplusGoes(n, *state, s.SurplusIdle, now) &&
			(shrinkable || in.Running > in.Replicas) {
			return state.Resuming != ""
		}
	}
	var serving []Node
	others, pressured := 0, false
	for _, n := range live {
		if n.Draining && n.Name != state.Surplus {
			continue // drained for another reason: no capacity
		}
		serving = append(serving, n)
		if !n.Draining && n.Name != state.Surplus {
			others++
			pressured = pressured || s.pressure(n) != ""
		}
	}
	load, known := groupLoad(serving)
	onFewer := 2.0 // with one node serving, none fewer
	if len(serving) >= 2 {
		onFewer = load * float64(len(serving)) / float64(len(serving)-1)
	}
	// resumed by what growth would act on (every other node under pressure for `for`), not a blip on one node - that
	// only holds a new pick (the review); and at the floor, where it would never go
	if state.Surplus != "" && (others == 0 || in.Demand || grows || in.Replicas <= floor ||
		known && onFewer >= s.ScaleDownLoad+ResumeMargin) {
		state.Resuming, state.ResumingSince = state.Surplus, now // the load came back: it takes connections again
		clearSurplus()
		return true
	}
	if !known || len(serving) < 2 || in.Replicas <= floor || pressured || in.Demand || onFewer >= s.ScaleDownLoad {
		state.UnderloadSince = time.Time{} // nothing new is drained; what is drained stays (between the thresholds)
		return state.Resuming != ""
	}
	if state.UnderloadSince.IsZero() {
		state.UnderloadSince = now
	}
	settled := in.Running == in.Replicas && in.Pods == int(in.Replicas) && in.Unreported == 0
	if state.Surplus != "" || !settled || now.Sub(state.UnderloadSince) < s.ScaleDownFor {
		return state.Resuming != ""
	}
	var pick *Node
	for i := range serving {
		n := &serving[i]
		if n.Draining || n.Name == state.Ignored {
			continue
		}
		if pick == nil || n.Sessions < pick.Sessions || n.Sessions == pick.Sessions && n.Started.After(pick.Started) {
			pick = n
		}
	}
	if pick != nil {
		state.Surplus, state.MarkedAt, state.DrainedSince = pick.Name, now, time.Time{}
	}
	return state.Resuming != ""
}

// drainedIdleSince: the drained node's idle time counts from its drain, never from before it.
func drainedIdleSince(n Node, st State) time.Time {
	if n.IdleSince.After(st.DrainedSince) {
		return n.IdleSince
	}
	return st.DrainedSince
}

// SurplusGoes says whether the node drained for the scale-down goes: it said it drains, and has had no session for
// idle since then (the pod-deletion cost's -2 too).
func SurplusGoes(n Node, st State, idle time.Duration, now time.Time) bool {
	return n.Name == st.Surplus && st.Surplus != "" && !n.Lost && n.Draining && !st.DrainedSince.IsZero() &&
		n.Sessions == 0 && !n.IdleSince.IsZero() && now.Sub(drainedIdleSince(n, st)) >= idle
}

// groupLoad is the worst share of the nodes' load: working memory and spill (the mean share - a group's nodes share
// one size), sessions (their sum against the sum of the limits), quack seats (the mean share); known when any is.
func groupLoad(nodes []Node) (float64, bool) {
	worst, known := 0.0, false
	mean := func(share func(Node) float64) {
		sum, count := 0.0, 0
		for _, n := range nodes {
			if v := share(n); v >= 0 {
				sum, count = sum+v, count+1
			}
		}
		if count > 0 {
			worst, known = max(worst, sum/float64(count)), true
		}
	}
	mean(func(n Node) float64 { return n.MemoryUsed })
	mean(func(n Node) float64 { return n.TempUsed })
	mean(func(n Node) float64 { return n.SeatsUsed })
	var sessions, limit int64
	for _, n := range nodes {
		if n.SessionsMax > 0 {
			sessions, limit = sessions+n.Sessions, limit+n.SessionsMax
		}
	}
	if limit > 0 {
		worst, known = max(worst, float64(sessions)/float64(limit)), true
	}
	return worst, known
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
