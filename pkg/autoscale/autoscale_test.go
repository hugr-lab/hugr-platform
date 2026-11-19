package autoscale

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// scaling wakes from 0 (stage 2's demand): a min-0 group may stop
var scaling = Scaling{Min: 0, Max: 4, Idle: DefaultIdle, For: DefaultFor, Cooldown: DefaultCooldown,
	MemoryUsed: DefaultMemoryUsed, TempUsed: DefaultTempUsed, SessionsUsed: DefaultSessionsUsed,
	SeatsUsed: DefaultSeatsUsed, StreamsWaiting: DefaultStreamsWaiting, WakeOnDemand: true,
	ScaleDownLoad: DefaultScaleDownLoad, ScaleDownFor: DefaultScaleDownFor, SurplusIdle: DefaultSurplusIdle}

// busy is a node with a session and room to spare
func busy(name string) Node {
	return Node{Name: name, Sessions: 3, SessionsMax: 1000, MemoryUsed: 0.2, TempUsed: -1, SeatsUsed: -1}
}

func idle(name string, since time.Duration) Node {
	n := busy(name)
	n.Sessions, n.IdleSince = 0, now.Add(-since)
	return n
}

func hot(name string) Node {
	n := busy(name)
	n.MemoryUsed = 0.85
	return n
}

// look is a settled group: every pod asked for runs and has reported, the plan admits growth
func look(replicas int32, nodes ...Node) Input {
	return Input{Replicas: replicas, Running: replicas, Pods: len(nodes), Nodes: nodes, MayGrow: true, Now: now}
}

func (in Input) with(f func(*Input)) Input {
	f(&in)
	return in
}

func under(since time.Duration) func(*Input) {
	return func(in *Input) { in.State.PressureSince = now.Add(-since) }
}

func TestWakeOnDemand(t *testing.T) {
	d := Decide(scaling, look(0).with(func(in *Input) { in.Demand, in.State.LastChange = true, now.Add(-time.Second) }))
	if d.Replicas != 1 || !strings.Contains(d.Reason, "client waits") {
		t.Errorf("a client waits for a group at 0: at once, past the cooldown: %+v", d)
	}
	withMin := scaling
	withMin.Min = 2
	if d := Decide(withMin, look(0)); d.Replicas != 2 {
		t.Errorf("a group below its min goes to its min: %+v", d)
	}
	if d := Decide(scaling, look(0)); d.Replicas != 0 || d.Reason != "" {
		t.Errorf("no client, no node: %+v", d)
	}
	// without the front's demand nothing wakes a group at 0: it keeps one node
	stage1 := scaling
	stage1.WakeOnDemand = false
	if d := Decide(stage1, look(1, idle("a", time.Hour))); d.Replicas != 1 {
		t.Errorf("nothing would wake it: the last node stays: %+v", d)
	}
	if d := Decide(stage1, look(0)); d.Replicas != 1 {
		t.Errorf("a group at 0 nothing wakes: %+v", d)
	}
}

func TestBounds(t *testing.T) {
	if d := Decide(scaling, look(6, busy("a"))); d.Replicas != 4 || !strings.Contains(d.Reason, "maximum") {
		t.Errorf("past a max just written: %+v", d)
	}
	// the policy's default group keeps one node whatever min says: by the platform's word, or a node's Lease
	def := idle("a", time.Hour)
	def.Default = true
	if d := Decide(scaling, look(1, def)); d.Replicas != 1 {
		t.Errorf("the default group's last node never stops (its Lease): %+v", d)
	}
	platformDefault := look(1, idle("a", time.Hour)).with(func(in *Input) { in.Default = true })
	if d := Decide(scaling, platformDefault); d.Replicas != 1 {
		t.Errorf("the default group's last node never stops (the platform says, a Lease does not): %+v", d)
	}
	if d := Decide(scaling, look(0).with(func(in *Input) { in.Default = true })); d.Replicas != 1 {
		t.Errorf("the default group below 1: %+v", d)
	}
}

func TestGrowth(t *testing.T) {
	in := look(2, hot("a"), hot("b"))
	d := Decide(scaling, in)
	if d.Replicas != 2 || !d.State.PressureSince.Equal(now) {
		t.Fatalf("the pressure starts now, no step yet: %+v", d)
	}
	in.State, in.Now = d.State, now.Add(59*time.Second)
	if d := Decide(scaling, in); d.Replicas != 2 {
		t.Errorf("before `for`: %+v", d)
	}
	in.Now = now.Add(60 * time.Second)
	d = Decide(scaling, in)
	if d.Replicas != 3 || !strings.Contains(d.Reason, "working memory") || !d.State.LastChange.Equal(in.Now) {
		t.Fatalf("every node past the memory threshold for `for`: one more: %+v", d)
	}
	// the new pod is not created yet (3 asked, 2 pods), then created and not reported: no second step
	notYet := look(3, hot("a"), hot("b")).with(under(time.Hour)).with(func(in *Input) { in.Running = 3 })
	if d := Decide(scaling, notYet); d.Replicas != 3 {
		t.Errorf("the step is still being made: %+v", d)
	}
	starting := notYet.with(func(in *Input) { in.Pods, in.Unreported = 3, 1 })
	if d := Decide(scaling, starting); d.Replicas != 3 {
		t.Errorf("a node on its way is capacity: %+v", d)
	}
	cooling := look(3, hot("a"), hot("b"), hot("c")).with(under(time.Hour)).
		with(func(in *Input) { in.State.LastChange = now.Add(-time.Minute) })
	if d := Decide(scaling, cooling); d.Replicas != 3 {
		t.Errorf("within the cooldown: %+v", d)
	}
	if d := Decide(scaling, look(4, hot("a"), hot("b"), hot("c"), hot("d")).with(under(time.Hour))); d.Replicas != 4 {
		t.Errorf("at max: %+v", d)
	}
	// one node with room: no pressure, and the clock restarts
	if d := Decide(scaling, look(2, hot("a"), busy("b")).with(under(time.Hour))); d.Replicas != 2 ||
		!d.State.PressureSince.IsZero() {
		t.Errorf("a node with room relieves the group: %+v", d)
	}
	// a draining node is no capacity: the placeable ones decide
	draining := busy("b")
	draining.Draining = true
	if d := Decide(scaling, look(2, hot("a"), draining).with(under(time.Hour))); d.Replicas != 3 {
		t.Errorf("only placeable nodes count: %+v", d)
	}
}

func TestEverySignal(t *testing.T) {
	for name, n := range map[string]Node{
		"spill":    {Name: "a", MemoryUsed: 0.1, TempUsed: 0.9, SeatsUsed: -1},
		"sessions": {Name: "a", Sessions: 950, SessionsMax: 1000, MemoryUsed: 0.1, TempUsed: -1, SeatsUsed: -1},
		"seats":    {Name: "a", Sessions: 1, MemoryUsed: 0.1, TempUsed: -1, SeatsUsed: 0.95},
		"streams":  {Name: "a", Sessions: 1, MemoryUsed: 0.1, TempUsed: -1, SeatsUsed: -1, StreamsWaiting: 1},
	} {
		if d := Decide(scaling, look(1, n).with(under(time.Hour))); d.Replicas != 2 {
			t.Errorf("%s: %+v", name, d)
		}
	}
	// nothing reported is no pressure (an older node, no limits)
	unknown := Node{Name: "a", Sessions: 1, MemoryUsed: -1, TempUsed: -1, SeatsUsed: -1}
	if d := Decide(scaling, look(1, unknown).with(under(time.Hour))); d.Replicas != 1 {
		t.Errorf("an unreported load: %+v", d)
	}
}

func TestShrink(t *testing.T) {
	d := Decide(scaling, look(2, busy("a"), idle("b", 31*time.Minute)))
	if d.Replicas != 1 || !strings.Contains(d.Reason, "b has had no session") {
		t.Errorf("an idle node past `idle` goes: %+v", d)
	}
	if d := Decide(scaling, look(2, busy("a"), idle("b", 29*time.Minute))); d.Replicas != 2 {
		t.Errorf("not yet idle long enough: %+v", d)
	}
	if d := Decide(scaling, look(2, busy("a"), busy("b"))); d.Replicas != 2 {
		t.Errorf("busy nodes are never stopped to shrink: %+v", d)
	}
	withMin := scaling
	withMin.Min = 2
	if d := Decide(withMin, look(2, idle("a", time.Hour), idle("b", time.Hour))); d.Replicas != 2 {
		t.Errorf("at min: %+v", d)
	}
	// the last node of a min-0 group stops once idle, unless a client waits
	if d := Decide(scaling, look(1, idle("a", time.Hour))); d.Replicas != 0 {
		t.Errorf("the last idle node of a min-0 group: %+v", d)
	}
	if d := Decide(scaling, look(1, idle("a", time.Hour)).with(func(in *Input) { in.Demand = true })); d.Replicas != 1 {
		t.Errorf("a client waits: the last node stays: %+v", d)
	}
	// a node with no session that reports no idle time (an older pin) never counts as idle
	unknown := busy("b")
	unknown.Sessions = 0
	if d := Decide(scaling, look(2, busy("a"), unknown)); d.Replicas != 2 {
		t.Errorf("no idle time reported: %+v", d)
	}
	if d := Decide(scaling, look(2, busy("a"), idle("b", time.Hour)).with(func(in *Input) {
		in.State.LastChange = now.Add(-time.Minute)
	})); d.Replicas != 2 {
		t.Errorf("within the cooldown: %+v", d)
	}
}

// TestARolloutHoldsTheShrink (the re-review): during a rollout the surge pod makes the pods one more than the
// Deployment is asked, and a scale-down then lands on either ReplicaSet - the deletion cost ranks only within one, so
// a busy old pod could go for the idle new one; the shrink waits for the rollout, growth for the pod on its way
func TestARolloutHoldsTheShrink(t *testing.T) {
	surge := look(2, busy("a"), idle("b", time.Hour)).with(func(in *Input) { in.Pods, in.Unreported = 3, 1 })
	if d := Decide(scaling, surge); d.Replicas != 2 {
		t.Errorf("a shrink during a rollout: %+v", d)
	}
	hotSurge := look(2, hot("a"), hot("b")).with(func(in *Input) { in.Pods, in.Unreported = 3, 1 }).with(under(time.Hour))
	if d := Decide(scaling, hotSurge); d.Replicas != 2 {
		t.Errorf("growth past a pod on its way: %+v", d)
	}
	// a ReplicaSet behind the Deployment (it says 2, 3 pods still run): the same
	behind := look(2, busy("a"), idle("b", time.Hour), busy("c")).with(func(in *Input) { in.Pods = 3 })
	if d := Decide(scaling, behind); d.Replicas != 2 {
		t.Errorf("a second shrink before the ReplicaSet took the first: %+v", d)
	}
}

func TestAnUnreportedPodHoldsOnlyGrowth(t *testing.T) {
	// a pod that never comes up (unschedulable, an image it cannot pull) holds the growth, never the shrink - the
	// scale-down takes an unready pod first
	stuck := look(3, hot("a"), hot("b")).with(func(in *Input) { in.Pods, in.Unreported = 3, 1 }).with(under(time.Hour))
	if d := Decide(scaling, stuck); d.Replicas != 3 {
		t.Errorf("growth past a pod that does not come up: %+v", d)
	}
	// within what the Deployment is asked (no surge): the shrink goes - the scale-down takes the unready pod first
	idleStuck := look(3, busy("a"), idle("b", time.Hour)).with(func(in *Input) { in.Pods, in.Unreported = 3, 1 })
	if d := Decide(scaling, idleStuck); d.Replicas != 2 {
		t.Errorf("a shrink held by a pod that does not come up: %+v", d)
	}
}

func TestRefusedByThePlan(t *testing.T) {
	// the plan refuses the growth (its maxNodes lowered): what was asked past what runs goes back...
	refused := look(3, hot("a"), hot("b")).with(func(in *Input) { in.Running, in.MayGrow, in.Pods = 2, false, 2 }).
		with(under(time.Hour))
	if d := Decide(scaling, refused); d.Replicas != 2 || !strings.Contains(d.Reason, "plan") {
		t.Errorf("a refused growth goes back to what runs: %+v", d)
	}
	// ...never below its floor (a new group nothing runs yet keeps asking its one node, refused, no flap)...
	newGroup := look(1).with(func(in *Input) { in.Running, in.MayGrow = 0, false })
	minOne := scaling
	minOne.Min = 1
	if d := Decide(minOne, newGroup); d.Replicas != 1 || d.Reason != "" {
		t.Errorf("a refused group's floor is kept: %+v", d)
	}
	// ...it never grows, and keeps no pressure clock...
	atRunning := look(2, hot("a"), hot("b")).with(func(in *Input) { in.MayGrow = false }).with(under(time.Hour))
	if d := Decide(scaling, atRunning); d.Replicas != 2 || !d.State.PressureSince.IsZero() {
		t.Errorf("a refused group under load: %+v", d)
	}
	// ...a max lowered is applied, and an idle node still stops
	if d := Decide(scaling, look(5, busy("a")).with(func(in *Input) { in.MayGrow = false })); d.Replicas != 4 {
		t.Errorf("a max lowered on a refused group: %+v", d)
	}
	if d := Decide(scaling, look(2, busy("a"), idle("b", time.Hour)).with(func(in *Input) { in.MayGrow = false })); d.Replicas != 1 {
		t.Errorf("a refused group shrinks: %+v", d)
	}
	// and it is not raised to its min, nor woken: both are growth
	withMin := scaling
	withMin.Min = 3
	if d := Decide(withMin, look(2, busy("a"), busy("b")).with(func(in *Input) { in.MayGrow = false })); d.Replicas != 2 {
		t.Errorf("a refused group raised to its min: %+v", d)
	}
	if d := Decide(scaling, look(0).with(func(in *Input) { in.MayGrow, in.Demand = false, true })); d.Replicas != 0 {
		t.Errorf("a refused group woken: %+v", d)
	}
}

func TestALostNodeBlocksNothing(t *testing.T) {
	lost := busy("b")
	lost.Lost = true
	if d := Decide(scaling, look(2, hot("a"), lost).with(under(time.Hour))); d.Replicas != 3 {
		t.Errorf("a lost node is no capacity and not on its way: %+v", d)
	}
	if d := Decide(scaling, look(2, idle("a", time.Hour), lost)); d.Replicas != 1 {
		t.Errorf("a lost node does not hold the shrink: %+v", d)
	}
}

func TestIdlePast(t *testing.T) {
	if !IdlePast(idle("a", time.Hour), DefaultIdle, now) || IdlePast(idle("a", time.Minute), DefaultIdle, now) ||
		IdlePast(busy("a"), DefaultIdle, now) {
		t.Error("idle past the group's idle")
	}
	lost := idle("a", time.Hour)
	lost.Lost = true
	if IdlePast(lost, DefaultIdle, now) {
		t.Error("a lost node is lost, not idle")
	}
}

// TestNoStepBeforeTheDeploymentTakesTheLast (the re-review): a Deployment that has not taken the last write (its
// apply fails, or the look is the one that applies it) still runs the idle node - a second shrink would take a busy one
func TestNoStepBeforeTheDeploymentTakesTheLast(t *testing.T) {
	behind := look(2, idle("a", time.Hour), busy("b"), busy("c")).with(func(in *Input) { in.Running, in.Pods = 3, 3 })
	if d := Decide(scaling, behind); d.Replicas != 2 || d.Reason != "" {
		t.Errorf("a second shrink before the Deployment took the first: %+v", d)
	}
	hotBehind := look(3, hot("a"), hot("b")).with(func(in *Input) { in.Running = 2 }).with(under(time.Hour))
	if d := Decide(scaling, hotBehind); d.Replicas != 3 {
		t.Errorf("a growth before the Deployment took the last: %+v", d)
	}
}

// light is a node with a few sessions: the group's load, on one node fewer, fits well
func light(name string, sessions int64) Node {
	return Node{Name: name, Sessions: sessions, SessionsMax: 1000, MemoryUsed: 0.2, TempUsed: -1, SeatsUsed: -1,
		Started: now.Add(-time.Hour)}
}

// TestScaleDownDrainsANode (the owner, 2026-11-19): a group whose load fits in one node fewer drains one - the fewest
// sessions - for ScaleDownFor; it goes once idle; the load back resumes it before any node is added
func TestScaleDownDrainsANode(t *testing.T) {
	in := look(3, light("a", 40), light("b", 3), light("c", 20))
	d := Decide(scaling, in)
	if d.State.Surplus != "" || !d.State.UnderloadSince.Equal(now) {
		t.Fatalf("the underload starts now, nothing drained yet: %+v", d.State)
	}
	in.State, in.Now = d.State, now.Add(DefaultScaleDownFor)
	d = Decide(scaling, in)
	if d.State.Surplus != "b" || d.Replicas != 3 {
		t.Fatalf("after ScaleDownFor the node with the fewest sessions is drained, nothing stops yet: %+v", d)
	}
	// it says it drains (its idle time counts from then, not from before the mark); its sessions end; it goes after
	// SurplusIdle from its drain
	b := light("b", 0)
	at := now.Add(DefaultScaleDownFor + time.Minute)
	b.Draining, b.IdleSince = true, at.Add(-4*time.Minute)
	in = look(3, light("a", 40), b, light("c", 20))
	in.State, in.Now = d.State, at
	d = Decide(scaling, in)
	if d.State.Surplus != "b" || d.Replicas != 3 || !d.State.DrainedSince.Equal(at) {
		t.Fatalf("it drains: its drain noted, it stays: %+v", d)
	}
	later := at.Add(DefaultSurplusIdle)
	in = look(3, light("a", 40), b, light("c", 20)).with(func(x *Input) { x.State, x.Now = d.State, later })
	d2 := Decide(scaling, in)
	if d2.Replicas != 2 || !strings.Contains(d2.Reason, "b, drained for the scale-down") || d2.State.Surplus != "b" {
		t.Errorf("drained and idle past SurplusIdle since its drain: it goes, kept as the drained one: %+v", d2)
	}
	// the next look, its pod still listed: kept, whatever the load
	after := look(2, light("a", 40), b, light("c", 20)).with(func(x *Input) { x.State, x.Now, x.Pods = d2.State, later.Add(5*time.Second), 3 })
	if d3 := Decide(scaling, after); d3.State.Surplus != "b" {
		t.Errorf("the drained node is forgotten before its pod is gone: %+v", d3.State)
	}
}

func TestScaleDownResumesWhenTheLoadComesBack(t *testing.T) {
	drained := State{Surplus: "b", UnderloadSince: now.Add(-time.Hour)}
	b := light("b", 5)
	b.Draining = true
	// the load on two nodes is back past the scale-down's 60% plus the margin: b takes connections again
	busy := look(3, Node{Name: "a", Sessions: 10, SessionsMax: 1000, MemoryUsed: 0.6, TempUsed: -1, SeatsUsed: -1}, b,
		Node{Name: "c", Sessions: 10, SessionsMax: 1000, MemoryUsed: 0.6, TempUsed: -1, SeatsUsed: -1}).
		with(func(in *Input) { in.State = drained })
	if d := Decide(scaling, busy); d.State.Surplus != "" || d.Replicas != 3 {
		t.Errorf("the load came back: b is resumed, no node added yet: %+v", d)
	}
	// every placeable node under pressure: the drained one is resumed before growing
	hotPair := look(3, hot("a"), b, hot("c")).with(func(in *Input) { in.State = drained }).with(under(time.Hour))
	if d := Decide(scaling, hotPair); d.State.Surplus != "" || d.Replicas != 3 {
		t.Errorf("under pressure: the drained node first, then a new one: %+v", d)
	}
	// between the two thresholds: what is drained stays drained
	between := look(3, Node{Name: "a", Sessions: 10, SessionsMax: 1000, MemoryUsed: 0.42, TempUsed: -1, SeatsUsed: -1}, b,
		Node{Name: "c", Sessions: 10, SessionsMax: 1000, MemoryUsed: 0.42, TempUsed: -1, SeatsUsed: -1}).
		with(func(in *Input) { in.State = drained })
	if d := Decide(scaling, between); d.State.Surplus != "b" {
		t.Errorf("between the scale-down and the resume: b stays drained: %+v", d)
	}
}

func TestScaleDownHoldsAtTheFloorAndUnsettled(t *testing.T) {
	withMin := scaling
	withMin.Min = 3
	in := look(3, light("a", 1), light("b", 1), light("c", 1)).with(func(in *Input) {
		in.State.UnderloadSince = now.Add(-time.Hour)
	})
	if d := Decide(withMin, in); d.State.Surplus != "" {
		t.Errorf("at the floor nothing is drained: %+v", d.State)
	}
	if d := Decide(scaling, look(1, light("a", 0))); d.State.Surplus != "" {
		t.Errorf("one node is never drained for a scale-down (its idle time stops it): %+v", d.State)
	}
	coming := in.with(func(in *Input) { in.Unreported = 1 })
	if d := Decide(scaling, coming); d.State.Surplus != "" {
		t.Errorf("a node on its way: no scale-down: %+v", d.State)
	}
	// the drained node gone (the shrink took it): forgotten
	gone := look(2, light("a", 1), light("c", 1)).with(func(in *Input) { in.State.Surplus = "b" })
	if d := Decide(scaling, gone); d.State.Surplus != "" {
		t.Errorf("a gone node stays drained in the state: %+v", d.State)
	}
	// of two empty nodes the younger
	young := light("b", 0)
	young.Started = now.Add(-time.Minute)
	pair := look(3, light("a", 0), young, light("c", 9)).with(func(in *Input) { in.State.UnderloadSince = now.Add(-time.Hour) })
	if d := Decide(scaling, pair); d.State.Surplus != "b" {
		t.Errorf("of two empty nodes the younger is drained: %+v", d.State)
	}
}

// TestScaleDownNeverLeavesNoNode (the review): a node drained for another reason (an operator's DRAIN NODE) is no
// capacity and never picked; the last node taking connections is never drained; a waiting client resumes the drained one
func TestScaleDownNeverLeavesNoNode(t *testing.T) {
	ordered := light("a", 10)
	ordered.Draining = true // the operator's
	in := look(2, ordered, light("b", 3)).with(func(in *Input) { in.State.UnderloadSince = now.Add(-time.Hour) })
	if d := Decide(scaling, in); d.State.Surplus != "" {
		t.Errorf("the last node taking connections is drained: %+v", d.State)
	}
	three := look(3, ordered, light("b", 3), light("c", 3)).with(func(in *Input) { in.State.UnderloadSince = now.Add(-time.Hour) })
	if d := Decide(scaling, three); d.State.Surplus == "a" {
		t.Errorf("a node the operator drains is picked: %+v", d.State)
	}
	// drained b, and a client waits (no node placeable for it): b is resumed
	b := light("b", 3)
	b.Draining = true
	waiting := look(3, light("a", 3), b, light("c", 3)).with(func(in *Input) {
		in.State = State{Surplus: "b", MarkedAt: now.Add(-time.Hour), DrainedSince: now.Add(-time.Hour)}
		in.Demand = true
	})
	if d := Decide(scaling, waiting); d.State.Surplus != "" || d.State.Resuming != "b" {
		t.Errorf("a waiting client does not resume the drained node: %+v", d.State)
	}
	// the only other node gone to the operator's drain: b resumed
	alone := look(2, ordered, b).with(func(in *Input) {
		in.State = State{Surplus: "b", MarkedAt: now.Add(-time.Hour), DrainedSince: now.Add(-time.Hour)}
	})
	if d := Decide(scaling, alone); d.State.Surplus != "" {
		t.Errorf("no node takes connections and the drained one stays drained: %+v", d.State)
	}
}

// TestScaleDownWaitsForTheNodesWord (the review): the drained node goes only once it said it drains; a node that does
// not within AckWait (an older build) is dropped and not picked again; a streams-waiting node is load, not underload
func TestScaleDownWaitsForTheNodesWord(t *testing.T) {
	old := light("b", 0)
	old.IdleSince = now.Add(-10 * time.Minute) // idle before the mark (under the plain idle), never says it drains
	marked := State{Surplus: "b", MarkedAt: now.Add(-time.Minute), UnderloadSince: now.Add(-time.Hour)}
	in := look(3, light("a", 40), old, light("c", 20)).with(func(in *Input) { in.State = marked })
	if d := Decide(scaling, in); d.Replicas != 3 || d.State.Surplus != "b" {
		t.Errorf("a marked node that has not said it drains goes: %+v", d)
	}
	marked.MarkedAt = now.Add(-AckWait)
	in.State = marked
	d := Decide(scaling, in)
	if d.State.Surplus != "" || d.State.Ignored != "b" || d.Replicas != 3 {
		t.Fatalf("a node that never drained is dropped: %+v", d)
	}
	in.State = d.State
	in.State.UnderloadSince = now.Add(-time.Hour)
	if d := Decide(scaling, in); d.State.Surplus == "b" {
		t.Errorf("the node that did not drain is picked again: %+v", d.State)
	}
	// a stream waiting on a node is pressure: no underload, a drained node resumed
	streams := light("c", 1)
	streams.StreamsWaiting = 1
	drained := light("b", 1)
	drained.Draining = true
	flip := look(3, light("a", 1), drained, streams).with(func(in *Input) {
		in.State = State{Surplus: "b", MarkedAt: now.Add(-time.Hour), DrainedSince: now.Add(-time.Hour)}
	})
	if d := Decide(scaling, flip); d.State.Surplus != "b" {
		t.Errorf("a blip on one node (a stream waiting) resumes the drained node - the re-review: %+v", d.State)
	}
	// every other node under pressure for `for`: what growth would act on resumes it
	hotA, hotC := hot("a"), hot("c")
	pressedAll := look(3, hotA, drained, hotC).with(func(in *Input) {
		in.State = State{Surplus: "b", MarkedAt: now.Add(-time.Hour), DrainedSince: now.Add(-time.Hour),
			PressureSince: now.Add(-time.Hour)}
	})
	if d := Decide(scaling, pressedAll); d.State.Surplus != "" || d.State.Resuming != "b" || d.Replicas != 3 {
		t.Errorf("every other node under pressure: resumed, no node added yet: %+v", d)
	}
	if d := Decide(scaling, look(3, light("a", 1), light("b", 1), streams).with(func(in *Input) {
		in.State.UnderloadSince = now.Add(-time.Hour)
	})); d.State.Surplus != "" || !d.State.UnderloadSince.IsZero() {
		t.Errorf("streams waiting count as underload: %+v", d.State)
	}
}

// TestGrowthWaitsForTheResumedNode (the review): a node resumed from the scale-down that still says it drains holds
// the growth, ResumeWait at most
func TestGrowthWaitsForTheResumedNode(t *testing.T) {
	b := hot("b")
	b.Draining = true
	in := look(3, hot("a"), b, hot("c")).with(under(time.Hour)).with(func(in *Input) {
		in.State.Resuming, in.State.ResumingSince = "b", now.Add(-time.Minute)
	})
	if d := Decide(scaling, in); d.Replicas != 3 {
		t.Errorf("growth while the resumed node still drains: %+v", d)
	}
	in.State.ResumingSince = now.Add(-ResumeWait)
	if d := Decide(scaling, in); d.Replicas != 4 {
		t.Errorf("past ResumeWait the group grows: %+v", d)
	}
}

// TestTheGoingNodeIsResumedWhereItCannotGo (the re-review): a drained idle node is kept only while a shrink can take
// it - at the floor, or with the shrink held, the resume rules apply
func TestTheGoingNodeIsResumedWhereItCannotGo(t *testing.T) {
	b := light("b", 0)
	b.Draining, b.IdleSince = true, now.Add(-time.Hour)
	st := State{Surplus: "b", MarkedAt: now.Add(-2 * time.Hour), DrainedSince: now.Add(-time.Hour)}
	withMin := scaling
	withMin.Min = 2
	atFloor := look(2, light("a", 5), b).with(func(in *Input) { in.State = st })
	if d := Decide(withMin, atFloor); d.State.Surplus != "" || d.State.Resuming != "b" {
		t.Errorf("at the floor the drained node never goes: resume it: %+v", d.State)
	}
	// a stuck rollout (a surge pod) holds the shrink: the drained node is not kept forever - the others under
	// pressure resume it
	hotA := hot("a")
	held := look(2, hotA, b).with(func(in *Input) {
		in.State = st
		in.State.PressureSince = now.Add(-time.Hour)
		in.Pods, in.Unreported = 3, 1
	})
	if d := Decide(scaling, held); d.State.Surplus != "" {
		t.Errorf("a held shrink keeps the drained node from the resume rules: %+v", d.State)
	}
	// a shrink asked and not yet made (more pods than replicas): kept
	pending := look(1, light("a", 5), b).with(func(in *Input) { in.State, in.Pods, in.Running = st, 2, 2 })
	if d := Decide(scaling, pending); d.State.Surplus != "b" {
		t.Errorf("the drained node, its shrink pending, is resumed: %+v", d.State)
	}
}

func TestTheNodesWordWaitsForItsHeartbeat(t *testing.T) {
	slow := scaling
	slow.Heartbeat = time.Minute // a node acts a heartbeat after its answer, says so the next
	marked := light("b", 3)
	in := look(3, light("a", 40), marked, light("c", 20)).with(func(in *Input) {
		in.State = State{Surplus: "b", MarkedAt: now.Add(-150 * time.Second)}
	})
	if d := Decide(slow, in); d.State.Surplus != "b" || d.State.Ignored != "" {
		t.Errorf("a node of a 60 s heartbeat ignored before it could answer: %+v", d.State)
	}
	in.State.MarkedAt = now.Add(-4 * time.Minute)
	if d := Decide(slow, in); d.State.Ignored != "b" {
		t.Errorf("past 3 heartbeats and a look: ignored: %+v", d.State)
	}
	// a RESUME NODE by hand after it drained: not ours any more
	resumed := look(3, light("a", 40), light("b", 3), light("c", 20)).with(func(in *Input) {
		in.State = State{Surplus: "b", MarkedAt: now.Add(-time.Hour), DrainedSince: now.Add(-30 * time.Minute)}
	})
	if d := Decide(scaling, resumed); d.State.Surplus != "" || d.State.Ignored != "b" {
		t.Errorf("a drained node resumed by hand stays the drained one: %+v", d.State)
	}
}

// TestTheGoingNodeServesWhenNoneElseDoes (the last pass): the drained idle node is not kept going while no other node
// takes connections - two nodes lost - or a client waits: it is resumed
func TestTheGoingNodeServesWhenNoneElseDoes(t *testing.T) {
	b := light("b", 0)
	b.Draining, b.IdleSince = true, now.Add(-time.Hour)
	lostA, lostC := light("a", 3), light("c", 3)
	lostA.Lost, lostC.Lost = true, true
	st := State{Surplus: "b", MarkedAt: now.Add(-2 * time.Hour), DrainedSince: now.Add(-time.Hour),
		LastChange: now.Add(-time.Minute)}
	in := look(3, lostA, b, lostC).with(func(in *Input) { in.State, in.Demand = st, true })
	if d := Decide(scaling, in); d.State.Surplus != "" || d.State.Resuming != "b" {
		t.Errorf("no other node serves and a client waits: the drained node stays going: %+v", d.State)
	}
}
