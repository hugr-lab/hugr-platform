package autoscale

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

var scaling = Scaling{Min: 0, Max: 4, Idle: DefaultIdle, For: DefaultFor, Cooldown: DefaultCooldown,
	MemoryUsed: DefaultMemoryUsed, TempUsed: DefaultTempUsed, SessionsUsed: DefaultSessionsUsed,
	SeatsUsed: DefaultSeatsUsed, StreamsWaiting: DefaultStreamsWaiting}

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

func TestWakeOnDemand(t *testing.T) {
	d := Decide(scaling, 0, nil, true, State{LastChange: now.Add(-time.Second)}, now)
	if d.Replicas != 1 || !strings.Contains(d.Reason, "client waits") {
		t.Errorf("a client waits for a group at 0: at once, past the cooldown: %+v", d)
	}
	withMin := scaling
	withMin.Min = 2
	if d := Decide(withMin, 0, nil, true, State{}, now); d.Replicas != 2 {
		t.Errorf("a group below its min goes to its min: %+v", d)
	}
	if d := Decide(scaling, 0, nil, false, State{}, now); d.Replicas != 0 || d.Reason != "" {
		t.Errorf("no client, no node: %+v", d)
	}
}

func TestBounds(t *testing.T) {
	if d := Decide(scaling, 6, []Node{busy("a")}, false, State{}, now); d.Replicas != 4 || !strings.Contains(d.Reason, "maximum") {
		t.Errorf("past a max just written: %+v", d)
	}
	// the policy's default group keeps one node whatever min says
	def := idle("a", time.Hour)
	def.Default = true
	if d := Decide(scaling, 1, []Node{def}, false, State{}, now); d.Replicas != 1 {
		t.Errorf("the default group's last node never stops: %+v", d)
	}
	if d := Decide(scaling, 0, []Node{def}, false, State{}, now); d.Replicas != 1 {
		t.Errorf("the default group below 1: %+v", d)
	}
}

func TestGrowth(t *testing.T) {
	nodes := []Node{hot("a"), hot("b")}
	d := Decide(scaling, 2, nodes, false, State{}, now)
	if d.Replicas != 2 || !d.State.PressureSince.Equal(now) {
		t.Fatalf("the pressure starts now, no step yet: %+v", d)
	}
	d = Decide(scaling, 2, nodes, false, d.State, now.Add(59*time.Second))
	if d.Replicas != 2 {
		t.Errorf("before `for`: %+v", d)
	}
	d = Decide(scaling, 2, nodes, false, d.State, now.Add(60*time.Second))
	if d.Replicas != 3 || !strings.Contains(d.Reason, "working memory") || !d.State.LastChange.Equal(now.Add(60*time.Second)) {
		t.Fatalf("every node past the memory threshold for `for`: one more: %+v", d)
	}
	// the new node is starting (3 asked, 2 reported): no second step, whatever the pressure
	after := Decide(scaling, 3, nodes, false, State{PressureSince: now, LastChange: now.Add(-time.Hour)}, now.Add(5*time.Minute))
	if after.Replicas != 3 {
		t.Errorf("a node on its way is capacity: %+v", after)
	}
	// cooldown after the step
	if d := Decide(scaling, 3, []Node{hot("a"), hot("b"), hot("c")}, false,
		State{PressureSince: now.Add(-time.Hour), LastChange: now.Add(-time.Minute)}, now); d.Replicas != 3 {
		t.Errorf("within the cooldown: %+v", d)
	}
	// never past max
	full := []Node{hot("a"), hot("b"), hot("c"), hot("d")}
	if d := Decide(scaling, 4, full, false, State{PressureSince: now.Add(-time.Hour)}, now); d.Replicas != 4 {
		t.Errorf("at max: %+v", d)
	}
	// one node with room: no pressure, and the clock restarts
	if d := Decide(scaling, 2, []Node{hot("a"), busy("b")}, false, State{PressureSince: now.Add(-time.Hour)}, now); d.Replicas != 2 ||
		!d.State.PressureSince.IsZero() {
		t.Errorf("a node with room relieves the group: %+v", d)
	}
	// a draining node is no capacity: the placeable ones decide
	draining := busy("b")
	draining.Draining = true
	if d := Decide(scaling, 2, []Node{hot("a"), draining}, false, State{PressureSince: now.Add(-time.Hour)}, now); d.Replicas != 3 {
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
		d := Decide(scaling, 1, []Node{n}, false, State{PressureSince: now.Add(-time.Hour)}, now)
		if d.Replicas != 2 {
			t.Errorf("%s: %+v", name, d)
		}
	}
	// nothing reported is no pressure (an older node, no limits)
	unknown := Node{Name: "a", Sessions: 1, MemoryUsed: -1, TempUsed: -1, SeatsUsed: -1}
	if d := Decide(scaling, 1, []Node{unknown}, false, State{PressureSince: now.Add(-time.Hour)}, now); d.Replicas != 1 {
		t.Errorf("an unreported load: %+v", d)
	}
}

func TestShrink(t *testing.T) {
	nodes := []Node{busy("a"), idle("b", 31*time.Minute)}
	d := Decide(scaling, 2, nodes, false, State{}, now)
	if d.Replicas != 1 || !strings.Contains(d.Reason, "b has had no session") {
		t.Errorf("an idle node past `idle` goes: %+v", d)
	}
	if d := Decide(scaling, 2, []Node{busy("a"), idle("b", 29*time.Minute)}, false, State{}, now); d.Replicas != 2 {
		t.Errorf("not yet idle long enough: %+v", d)
	}
	// busy nodes are never stopped to shrink
	if d := Decide(scaling, 2, []Node{busy("a"), busy("b")}, false, State{}, now); d.Replicas != 2 {
		t.Errorf("busy nodes: %+v", d)
	}
	// the floor
	withMin := scaling
	withMin.Min = 2
	if d := Decide(withMin, 2, []Node{idle("a", time.Hour), idle("b", time.Hour)}, false, State{}, now); d.Replicas != 2 {
		t.Errorf("at min: %+v", d)
	}
	// the last node of a min-0 group stops once idle, unless a client waits
	if d := Decide(scaling, 1, []Node{idle("a", time.Hour)}, false, State{}, now); d.Replicas != 0 {
		t.Errorf("the last idle node of a min-0 group: %+v", d)
	}
	if d := Decide(scaling, 1, []Node{idle("a", time.Hour)}, true, State{}, now); d.Replicas != 1 {
		t.Errorf("a client waits: the last node stays: %+v", d)
	}
	// a node live but not reporting idle_since (an older pin, or a session) never counts as idle
	unknown := busy("b")
	unknown.Sessions = 0
	if d := Decide(scaling, 2, []Node{busy("a"), unknown}, false, State{}, now); d.Replicas != 2 {
		t.Errorf("no idle time reported: %+v", d)
	}
	// within the cooldown nothing shrinks either
	if d := Decide(scaling, 2, nodes, false, State{LastChange: now.Add(-time.Minute)}, now); d.Replicas != 2 {
		t.Errorf("within the cooldown: %+v", d)
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

func TestALostNodeBlocksNothing(t *testing.T) {
	lost := busy("b")
	lost.Lost = true
	// 2 asked, 2 reported - one lost: nobody is starting, the live one decides alone
	if d := Decide(scaling, 2, []Node{hot("a"), lost}, false, State{PressureSince: now.Add(-time.Hour)}, now); d.Replicas != 3 {
		t.Errorf("a lost node is no capacity and not on its way: %+v", d)
	}
	if d := Decide(scaling, 2, []Node{idle("a", time.Hour), lost}, false, State{}, now); d.Replicas != 1 {
		t.Errorf("a lost node does not hold the shrink: %+v", d)
	}
}
