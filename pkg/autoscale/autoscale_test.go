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
	SeatsUsed: DefaultSeatsUsed, StreamsWaiting: DefaultStreamsWaiting, WakeOnDemand: true}

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

// TestARolloutHoldsNothingButGrowth (the re-review): during a rollout the surge pod makes the pods one more than the
// replicas, and a surge pod the cluster cannot schedule stays so - an idle node still stops, only growth waits
func TestARolloutHoldsNothingButGrowth(t *testing.T) {
	surge := look(2, busy("a"), idle("b", time.Hour)).with(func(in *Input) { in.Pods, in.Unreported = 3, 1 })
	if d := Decide(scaling, surge); d.Replicas != 1 {
		t.Errorf("an idle node during a rollout: %+v", d)
	}
	hotSurge := look(2, hot("a"), hot("b")).with(func(in *Input) { in.Pods, in.Unreported = 3, 1 }).with(under(time.Hour))
	if d := Decide(scaling, hotSurge); d.Replicas != 2 {
		t.Errorf("growth past a pod on its way: %+v", d)
	}
}

func TestAnUnreportedPodHoldsOnlyGrowth(t *testing.T) {
	// a pod that never comes up (unschedulable, an image it cannot pull) holds the growth, never the shrink - the
	// scale-down takes an unready pod first
	stuck := look(3, hot("a"), hot("b")).with(func(in *Input) { in.Pods, in.Unreported = 3, 1 }).with(under(time.Hour))
	if d := Decide(scaling, stuck); d.Replicas != 3 {
		t.Errorf("growth past a pod that does not come up: %+v", d)
	}
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
