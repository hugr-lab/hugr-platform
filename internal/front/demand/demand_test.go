package demand

import (
	"context"
	"math/rand/v2"
	"testing"
	"time"

	"errors"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

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

// soon is a connection's hold deadline in a test
func soon() time.Time { return time.Now().Add(5 * time.Second) }

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
	if n := d.Wait(context.Background(), f, time.Second, []string{"etl"}, soon(), pick("etl")); n == nil {
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
	if n := d.Wait(context.Background(), f, 50*time.Millisecond, []string{"etl"}, soon(), pick("etl")); n != nil {
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
	if n := d2.Wait(context.Background(), f, 10*time.Millisecond, nil, soon(), pick("etl")); n != nil {
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
	go func() {
		done <- d.Wait(context.Background(), f, 50*time.Millisecond, []string{"etl"}, soon(), pick("etl"))
	}()
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
	if n := d.Wait(ctx, f, 50*time.Millisecond, []string{"etl"}, soon(), pick("etl")); n != nil {
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

// TestAnOldAnswerIsDropped (the review): an answer left on a Lease that expired - its group switched to manual, or
// gone, before the operator cleared it - is not this demand's: a new demand drops it, and the client hears "no node"
// once nobody answers, not after the whole hold
func TestAnOldAnswerIsDropped(t *testing.T) {
	d, c := demandOf()
	seconds := int32(nodeproto.DemandSeconds)
	old := metav1.NewMicroTime(time.Now().Add(-time.Hour))
	_, err := c.CoordinationV1().Leases("hugr").Create(context.Background(), &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: nodeproto.DemandLeaseName("etl"), Namespace: "hugr",
			Labels:      map[string]string{nodeproto.LabelDemand: "true"},
			Annotations: map[string]string{nodeproto.AnnotationDemandGroup: "etl", nodeproto.AnnotationStarting: "etl"}},
		Spec: coordinationv1.LeaseSpec{RenewTime: &old, LeaseDurationSeconds: &seconds}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f := fleet.New()
	f.Set(fleet.NewView(nil))
	start := time.Now()
	if n := d.Wait(context.Background(), f, 50*time.Millisecond, []string{"etl"}, soon(), pick("etl")); n != nil {
		t.Fatal("a node from nowhere")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("held %v by an old answer", took)
	}
	if ok, starting := lease(t, c, "etl"); !ok || starting != "" {
		t.Errorf("the old answer stays: %q", starting)
	}
}

// TestOneHoldPerConnection (the review): a connection re-seated after a node came up and refused it again is held
// again only within its first hold's deadline
func TestOneHoldPerConnection(t *testing.T) {
	d, c := demandOf()
	f := fleet.New()
	f.Set(fleet.NewView(nil))
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
	}()
	until := time.Now().Add(700 * time.Millisecond)
	start := time.Now()
	if n := d.Wait(context.Background(), f, 50*time.Millisecond, []string{"etl"}, until, pick("etl")); n != nil {
		t.Fatal("a node from nowhere")
	}
	if took := time.Since(start); took < 600*time.Millisecond || took > 2*time.Second {
		t.Errorf("held %v, want the connection's deadline", took)
	}
	// the re-seat's wait: past the deadline, no second hold
	again := time.Now()
	if n := d.Wait(context.Background(), f, 50*time.Millisecond, []string{"etl"}, until, pick("etl")); n != nil {
		t.Fatal("a node from nowhere")
	}
	if took := time.Since(again); took > 500*time.Millisecond {
		t.Errorf("a second hold past the connection's deadline: %v", took)
	}
}

// TestAnOldAnswerIsDroppedAfterAFailedWrite (the re-review): the write that drops an old answer is the demand's first
// one the API takes, not merely its first try
func TestAnOldAnswerIsDroppedAfterAFailedWrite(t *testing.T) {
	d, c := demandOf()
	seconds := int32(nodeproto.DemandSeconds)
	old := metav1.NewMicroTime(time.Now().Add(-time.Hour))
	_, err := c.CoordinationV1().Leases("hugr").Create(context.Background(), &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: nodeproto.DemandLeaseName("etl"), Namespace: "hugr",
			Labels:      map[string]string{nodeproto.LabelDemand: "true"},
			Annotations: map[string]string{nodeproto.AnnotationDemandGroup: "etl", nodeproto.AnnotationStarting: "etl"}},
		Spec: coordinationv1.LeaseSpec{RenewTime: &old, LeaseDurationSeconds: &seconds}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	failed := false
	c.PrependReactor("patch", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		if !failed {
			failed = true
			return true, nil, apierrors.NewInternalError(errors.New("the API server's hiccup"))
		}
		return false, nil, nil
	})
	f := fleet.New()
	f.Set(fleet.NewView(nil))
	start := time.Now()
	if n := d.Wait(context.Background(), f, 50*time.Millisecond, []string{"etl"}, soon(), pick("etl")); n != nil {
		t.Fatal("a node from nowhere")
	}
	if took := time.Since(start); took > 4*time.Second {
		t.Errorf("held %v by an old answer after a failed write", took)
	}
	if ok, starting := lease(t, c, "etl"); !ok || starting != "" {
		t.Errorf("the old answer stays: %q", starting)
	}
}
