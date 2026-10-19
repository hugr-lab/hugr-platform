package fleet

import (
	"context"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

var now = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func node(label, group string, admit bool, seats, sessions int64) Node {
	return Node{Label: label, Service: "hugr-node-x", Address: "10.0.0.1", FlightPort: 32010, QuackPort: 9494,
		Ready: true, Serving: true, Lease: &Lease{Until: now.Add(time.Minute), Summary: nodeproto.Summary{
			Group: group, AdmitSession: admit, SeatsLeft: seats, Sessions: sessions}}}
}

func TestPlaceable(t *testing.T) {
	ok := node("a", "g", true, 5, 0)
	if !ok.Placeable(Flight, now) || !ok.Placeable(Quack, now) {
		t.Fatal("a ready node with a current, admitting Lease is placeable")
	}
	expired := ok
	expired.Lease = &Lease{Until: now.Add(-time.Second), Summary: ok.Lease.Summary}
	noLease := ok
	noLease.Lease = nil
	notReady := ok
	notReady.Ready = false
	notAdmitting := node("a", "g", false, 5, 0)
	noQuack := ok
	noQuack.QuackPort = 0
	for name, n := range map[string]Node{"expired": expired, "no lease yet": noLease, "not ready": notReady,
		"not admitting (draining, at capacity)": notAdmitting} {
		if n.Placeable(Flight, now) {
			t.Errorf("%s: placeable", name)
		}
	}
	if noQuack.Placeable(Quack, now) || !noQuack.Placeable(Flight, now) {
		t.Error("a node without a quack door is placeable for Flight only")
	}
}

func TestRoutable(t *testing.T) {
	draining := node("d", "g", false, 0, 3)
	terminating := node("t", "g", true, 5, 1)
	terminating.Ready, terminating.Serving = false, true
	gone := node("x", "g", true, 5, 1)
	gone.Ready, gone.Serving = false, false
	v := NewView([]Node{draining, terminating, gone})
	if v.Routable("d", Flight) == nil || v.Routable("t", Quack) == nil {
		t.Error("a draining or terminating node keeps its connections")
	}
	if v.Routable("x", Flight) != nil || v.Routable("nobody", Flight) != nil {
		t.Error("a node that does not serve, or no node, routes nothing")
	}
}

func TestChoose(t *testing.T) {
	rnd := rand.New(rand.NewPCG(1, 2))
	v := NewView([]Node{node("a1", "a", true, 1, 9), node("a2", "a", true, 7, 9), node("b1", "b", true, 9, 0),
		node("a3", "a", false, 99, 0)})
	counts := map[string]int{}
	for i := 0; i < 1000; i++ {
		counts[v.Choose(Quack, []string{"a"}, nil, now, rnd).Label]++
	}
	if counts["b1"] != 0 || counts["a3"] != 0 {
		t.Errorf("another group's node or one not admitting was chosen: %v", counts)
	}
	if counts["a2"] != 1000 {
		t.Errorf("of the two, the node with more seats left: %v", counts)
	}
	if n := v.Choose(Quack, []string{"a"}, map[string]bool{"a2": true}, now, rnd); n == nil || n.Label != "a1" {
		t.Errorf("a node tried is passed over: %v", n)
	}
	if n := v.Choose(Quack, []string{"none", "b"}, nil, now, rnd); n == nil || n.Label != "b1" {
		t.Errorf("the first of the groups that has a placeable node: %v", n)
	}
	if n := v.Choose(Quack, []string{"none"}, nil, now, rnd); n != nil {
		t.Errorf("a group without a placeable node, never another group: %v", n)
	}
	any := map[string]bool{}
	for i := 0; i < 200; i++ {
		any[v.Choose(Flight, nil, nil, now, rnd).Group()] = true
	}
	if !any["a"] || !any["b"] {
		t.Errorf("no groups means every group's nodes: %v", any)
	}
	// the same seats: fewer sessions; two random among many spreads the load
	w := NewView([]Node{node("x", "g", true, -1, 5), node("y", "g", true, -1, 1)})
	if n := w.Choose(Flight, []string{"g"}, nil, now, rnd); n.Label != "y" {
		t.Errorf("without quack seats, fewer sessions: %s", n.Label)
	}
	var many []Node
	for _, l := range []string{"n1", "n2", "n3", "n4", "n5", "n6"} {
		many = append(many, node(l, "g", true, 5, 0))
	}
	spread := map[string]int{}
	for i := 0; i < 600; i++ {
		spread[NewView(many).Choose(Quack, []string{"g"}, nil, now, rnd).Label]++
	}
	if len(spread) < 5 {
		t.Errorf("equal nodes are not spread over: %v", spread)
	}
}

func TestDefaultGroups(t *testing.T) {
	d := node("a", "general", true, 1, 1)
	d.Lease.Summary.GroupDefault = true
	stale := node("b", "old", true, 1, 1)
	stale.Lease.Summary.GroupDefault = true
	stale.Lease.Until = now.Add(-time.Second)
	v := NewView([]Node{d, stale, node("c", "etl", true, 1, 1)})
	if got := v.DefaultGroups(now); len(got) != 1 || got[0] != "general" {
		t.Errorf("the default group from the current Leases: %q", got)
	}
}

func TestWait(t *testing.T) {
	f := New()
	pick := func(v *View) *Node { return v.Choose(Flight, []string{"g"}, nil, now, rand.New(rand.NewPCG(1, 1))) }
	go func() {
		time.Sleep(20 * time.Millisecond)
		f.Set(NewView([]Node{node("late", "g", true, 1, 0)}))
	}()
	if n := f.Wait(context.Background(), 5*time.Second, pick); n == nil || n.Label != "late" {
		t.Fatalf("a node coming up during the wait is taken: %v", n)
	}
	start := time.Now()
	if n := New().Wait(context.Background(), 30*time.Millisecond, pick); n != nil {
		t.Fatal("no node: nil")
	}
	if time.Since(start) < 30*time.Millisecond {
		t.Error("the wait ended early")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n := New().Wait(ctx, time.Hour, pick); n != nil {
		t.Error("a call that ended waits no more")
	}
}
