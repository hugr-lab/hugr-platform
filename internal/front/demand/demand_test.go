package demand

import (
	"context"
	"math/rand/v2"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

func node(label, group string) fleet.Node {
	return fleet.Node{Label: label, Service: "hugr-node-" + group, Address: "10.0.0.1", FlightPort: 32010, Ready: true,
		Serving: true, Lease: &fleet.Lease{Renewed: time.Now(), Until: time.Now().Add(time.Minute),
			Summary: nodeproto.Summary{Group: group, AdmitSession: true}}}
}

func pick(group string) func(*fleet.View) *fleet.Node {
	return func(v *fleet.View) *fleet.Node {
		return v.Choose(fleet.Flight, []string{group}, nil, time.Now(), rand.New(rand.NewPCG(1, 1)))
	}
}

func demandOf() (*Demand, *fake.Clientset) {
	client := fake.NewClientset()
	return &Demand{Client: client, Namespace: "hugr", Holder: "hugr-front-a", Answer: 300 * time.Millisecond,
		Poll: 50 * time.Millisecond, Max: 5 * time.Second}, client
}

func lease(t *testing.T, c *fake.Clientset, group string) (ok bool, starting string) {
	t.Helper()
	l, err := c.CoordinationV1().Leases("hugr").Get(context.Background(), nodeproto.DemandLeaseName(group), metav1.GetOptions{})
	if err != nil {
		return false, ""
	}
	if l.Annotations[nodeproto.AnnotationDemandGroup] != group || l.Labels[nodeproto.LabelDemand] != "true" ||
		l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != "hugr-front-a" || !nodeproto.DemandLive(l, time.Now()) {
		t.Errorf("the demand Lease: %+v", l)
	}
	return true, l.Annotations[nodeproto.AnnotationStarting]
}

func TestAPlaceableNodeWritesNoDemand(t *testing.T) {
	d, c := demandOf()
	f := fleet.New()
	f.Set(fleet.NewView([]fleet.Node{node("hugr-node-etl-a", "etl")}))
	if n := d.Wait(context.Background(), f, time.Second, []string{"etl"}, pick("etl")); n == nil {
		t.Fatal("a placeable node is not chosen")
	}
	if ok, _ := lease(t, c, "etl"); ok {
		t.Error("a group with a node is no demand")
	}
}

func TestNobodyStartsANode(t *testing.T) {
	// a group nobody scales (manual, or no group of the platform's): the Lease says so for the operator, no answer
	// comes, the client hears "no node" once the operator had time to answer
	d, c := demandOf()
	f := fleet.New()
	f.Set(fleet.NewView(nil))
	start := time.Now()
	if n := d.Wait(context.Background(), f, 50*time.Millisecond, []string{"etl"}, pick("etl")); n != nil {
		t.Fatalf("a node from nowhere: %v", n)
	}
	if took := time.Since(start); took < 300*time.Millisecond || took > 2*time.Second {
		t.Errorf("held %v, want the answer's wait", took)
	}
	if ok, starting := lease(t, c, "etl"); !ok || starting != "" {
		t.Errorf("the demand was not written: %v %q", ok, starting)
	}
	// ungrouped (the Leases' defaults, nobody verified): no demand, NoNodeWait only
	d2, c2 := demandOf()
	if n := d2.Wait(context.Background(), f, 10*time.Millisecond, nil, pick("etl")); n != nil {
		t.Fatal("a node from nowhere")
	}
	if l, _ := c2.CoordinationV1().Leases("hugr").List(context.Background(), metav1.ListOptions{}); len(l.Items) != 0 {
		t.Errorf("a demand for groups nobody named: %v", l.Items)
	}
}

func TestHeldWhileANodeStarts(t *testing.T) {
	d, c := demandOf()
	f := fleet.New()
	f.Set(fleet.NewView(nil))
	done := make(chan *fleet.Node)
	go func() { done <- d.Wait(context.Background(), f, 50*time.Millisecond, []string{"etl"}, pick("etl")) }()
	// the operator answers, and the node comes up past the answer's wait
	for i := 0; i < 100; i++ {
		if ok, _ := lease(t, c, "etl"); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	l, _ := c.CoordinationV1().Leases("hugr").Get(context.Background(), nodeproto.DemandLeaseName("etl"), metav1.GetOptions{})
	l.Annotations[nodeproto.AnnotationStarting] = "etl"
	if _, err := c.CoordinationV1().Leases("hugr").Update(context.Background(), l, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	f.Set(fleet.NewView([]fleet.Node{node("hugr-node-etl-a", "etl")}))
	select {
	case n := <-done:
		if n == nil || n.Label != "hugr-node-etl-a" {
			t.Errorf("seated on %v", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("still held after the node came up")
	}
	// the renew kept the operator's answer
	if _, starting := lease(t, c, "etl"); starting != "etl" {
		t.Errorf("the front wrote over the operator's answer: %q", starting)
	}
}

func TestTheClientsEndEndsTheHold(t *testing.T) {
	d, c := demandOf()
	f := fleet.New()
	f.Set(fleet.NewView(nil))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for i := 0; i < 100; i++ {
			if ok, _ := lease(t, c, "etl"); ok {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		l, _ := c.CoordinationV1().Leases("hugr").Get(context.Background(), nodeproto.DemandLeaseName("etl"), metav1.GetOptions{})
		l.Annotations[nodeproto.AnnotationStarting] = "etl"
		_, _ = c.CoordinationV1().Leases("hugr").Update(context.Background(), l, metav1.UpdateOptions{})
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if n := d.Wait(ctx, f, 50*time.Millisecond, []string{"etl"}, pick("etl")); n != nil {
		t.Fatal("a node from nowhere")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("held %v past the client's end", took)
	}
}

func TestHoldsShareOneRenewer(t *testing.T) {
	d, _ := demandOf()
	a := d.Hold([]string{"etl"})
	b := d.Hold([]string{"etl", "bi"})
	d.mu.Lock()
	if d.held["etl"].count != 2 || d.held["bi"].count != 1 {
		t.Errorf("holders: %+v", d.held)
	}
	d.mu.Unlock()
	a()
	a() // a release is once
	d.mu.Lock()
	if d.held["etl"] == nil || d.held["etl"].count != 1 {
		t.Errorf("etl still held by b: %+v", d.held)
	}
	d.mu.Unlock()
	b()
	d.mu.Lock()
	if len(d.held) != 0 {
		t.Errorf("nobody holds: %+v", d.held)
	}
	d.mu.Unlock()
}
