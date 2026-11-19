package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/build"
	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

var t0 = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func lease(renewed time.Time, seconds int32, s nodeproto.Summary) *coordinationv1.Lease {
	at := metav1.NewMicroTime(renewed)
	return &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Annotations: s.Annotations()},
		Spec: coordinationv1.LeaseSpec{RenewTime: &at, LeaseDurationSeconds: &seconds}}
}

func TestNodeStateOf(t *testing.T) {
	l := lease(t0, 30, nodeproto.Summary{Sessions: 2})
	if NodeStateOf("a", l, t0.Add(29*time.Second)).Lost {
		t.Error("a Lease within its duration is lost")
	}
	if !NodeStateOf("a", l, t0.Add(31*time.Second)).Lost {
		t.Error("an expired Lease is not lost")
	}
	if !NodeStateOf("a", &coordinationv1.Lease{}, t0).Lost {
		t.Error("a Lease never renewed is not lost")
	}
}

func TestAggregate(t *testing.T) {
	yes, no := true, false
	fleet := Aggregate([]NodeState{
		{Name: "a", Summary: nodeproto.Summary{Target: 12, Applied: 12, GroupKnown: &yes}},
		{Name: "b", Summary: nodeproto.Summary{Target: 12, Applied: 11, NeedsRestart: true, GroupKnown: &yes}},
		{Name: "c", Summary: nodeproto.Summary{Target: 11, Applied: 11}},
		{Name: "d", Lost: true, Summary: nodeproto.Summary{Target: 99, Applied: 99, GroupKnown: &no}},
	})
	if fleet.Profile.Target != 12 || fleet.Profile.Applied != 1 {
		t.Errorf("target %d applied %d, want 12 and 1 (a lost node counts for neither)", fleet.Profile.Target, fleet.Profile.Applied)
	}
	if len(fleet.Profile.NeedsRestart) != 1 || fleet.Profile.NeedsRestart[0] != "b" {
		t.Errorf("needs restart: %v", fleet.Profile.NeedsRestart)
	}
	if len(fleet.Lost) != 1 || fleet.Lost[0] != "d" {
		t.Errorf("lost: %v", fleet.Lost)
	}
	if fleet.GroupKnown == nil || !*fleet.GroupKnown {
		t.Errorf("the group is known by every live node that says")
	}
	unknown := Aggregate([]NodeState{
		{Name: "a", Summary: nodeproto.Summary{GroupKnown: &yes}},
		{Name: "b", Summary: nodeproto.Summary{GroupKnown: &no}},
	})
	if unknown.GroupKnown == nil || *unknown.GroupKnown {
		t.Error("one node that finds its resource group gone makes the group unknown")
	}
	if Aggregate(nil).GroupKnown != nil {
		t.Error("no node reported: known stays unsaid")
	}
}

// spec 008: the live nodes counted per build, the newest catalog stamp any reads; a lost node and a node of an older pin
// (no build reported) count for neither
func TestAggregateBuilds(t *testing.T) {
	fleet := Aggregate([]NodeState{
		{Name: "a", Summary: nodeproto.Summary{Build: "v0.2.0", DuckDBVersion: "v2.0.0", CatalogSchema: 22, CatalogMinReader: 21}},
		{Name: "b", Summary: nodeproto.Summary{Build: "v0.3.0", DuckDBVersion: "v2.0.1", CatalogSchema: 23, CatalogMinReader: 22}},
		{Name: "c", Summary: nodeproto.Summary{Build: "v0.2.0", DuckDBVersion: "v2.0.0", CatalogSchema: 22, CatalogMinReader: 21}},
		{Name: "d", Summary: nodeproto.Summary{}},
		{Name: "e", Lost: true, Summary: nodeproto.Summary{Build: "v0.4.0", CatalogSchema: 30}},
	})
	want := []v1alpha1.BuildCount{{Build: "v0.2.0", DuckDB: "v2.0.0", Nodes: 2}, {Build: "v0.3.0", DuckDB: "v2.0.1", Nodes: 1}}
	if !equality.Semantic.DeepEqual(fleet.Builds, want) {
		t.Errorf("builds %+v, want %+v", fleet.Builds, want)
	}
	if fleet.Catalog == nil || fleet.Catalog.Schema != 23 || fleet.Catalog.MinReader != 22 {
		t.Errorf("the catalog's stamp is the newest a live node read: %+v", fleet.Catalog)
	}
	if none := Aggregate([]NodeState{{Name: "d"}}); none.Builds != nil || none.Catalog != nil {
		t.Errorf("a node of an older pin says no build: %+v", none)
	}
}

type world struct {
	client     client.Client
	reconciler *GroupReconciler
	now        time.Time
}

func size() v1alpha1.SizeSpec {
	return v1alpha1.SizeSpec{
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")}},
		Temp: v1alpha1.TempSpec{Size: resource.MustParse("10Gi")},
	}
}

func group(name string, replicas int32, created time.Time) *v1alpha1.HugrNodeGroup {
	return &v1alpha1.HugrNodeGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "hugr", UID: types.UID("uid-" + name),
			CreationTimestamp: metav1.NewTime(created)},
		Spec: v1alpha1.HugrNodeGroupSpec{Platform: "hugr", Replicas: replicas, Size: "small",
			Identity: v1alpha1.WorkloadIdentity{ClientID: name}, TLS: v1alpha1.TLSSpec{SecretRef: &v1alpha1.SecretRef{Name: name + "-tls"}}},
	}
}

func newWorld(t *testing.T, objs ...client.Object) *world {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	platform := &v1alpha1.HugrPlatform{ObjectMeta: metav1.ObjectMeta{Name: "hugr", Namespace: "hugr"},
		Spec: v1alpha1.HugrPlatformSpec{Node: v1alpha1.NodeSpec{Image: "img"},
			Sizes: map[string]v1alpha1.SizeSpec{"small": size()},
			Plan:  v1alpha1.PlanSpec{MaxNodes: 4, Sizes: []string{"small"}}}}
	// server-side apply over typed and our own resources alike: the deduced converter (the API server types them for
	// real in envtest)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, platform)...).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).
		WithStatusSubresource(&v1alpha1.HugrNodeGroup{}, &v1alpha1.HugrPlatform{}).Build()
	w := &world{client: c, now: t0}
	w.reconciler = &GroupReconciler{Client: c, Scheme: scheme, Settings: build.Settings{OrchestratorURL: "https://op"},
		Now: func() time.Time { return w.now }}
	return w
}

func (w *world) reconcile(t *testing.T, name string) *v1alpha1.HugrNodeGroup {
	t.Helper()
	ctx := context.Background()
	result, err := w.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "hugr", Name: name}})
	if err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
	if result.RequeueAfter != StatusEvery {
		t.Errorf("requeue %v, want the status every %v", result.RequeueAfter, StatusEvery)
	}
	var g v1alpha1.HugrNodeGroup
	if err := w.client.Get(ctx, types.NamespacedName{Namespace: "hugr", Name: name}, &g); err != nil {
		t.Fatal(err)
	}
	return &g
}

func (w *world) deployment(t *testing.T, name string) *appsv1.Deployment {
	t.Helper()
	var d appsv1.Deployment
	if err := w.client.Get(context.Background(), types.NamespacedName{Namespace: "hugr", Name: "hugr-node-" + name}, &d); err != nil {
		return nil
	}
	return &d
}

func TestReconcileBuildsTheGroup(t *testing.T) {
	w := newWorld(t, group("etl", 2, t0))
	g := w.reconcile(t, "etl")
	d := w.deployment(t, "etl")
	if d == nil || *d.Spec.Replicas != 2 {
		t.Fatalf("the Deployment is not the group's: %+v", d)
	}
	if owner := metav1.GetControllerOf(d); owner == nil || owner.Kind != "HugrNodeGroup" || owner.Name != "etl" {
		t.Errorf("the Deployment is not owned by its group: %+v", owner)
	}
	for _, obj := range []client.Object{&corev1.ServiceAccount{}, &corev1.Service{}} {
		if err := w.client.Get(context.Background(), types.NamespacedName{Namespace: "hugr", Name: "hugr-node-etl"}, obj); err != nil {
			t.Errorf("%T missing: %v", obj, err)
		}
	}
	if meta.IsStatusConditionTrue(g.Status.Conditions, ConditionRefused) {
		t.Errorf("a fitting group is refused: %v", g.Status.Conditions)
	}
}

// TestReconcileRechecksThePlan: two groups that fit alone and not together - the later one is refused, and what it
// runs is kept.
func TestReconcileRechecksThePlan(t *testing.T) {
	late := group("late", 1, t0.Add(time.Minute))
	w := newWorld(t, group("early", 3, t0), late)
	w.reconcile(t, "early")
	w.reconcile(t, "late") // 3 + 1 fits
	ctx := context.Background()
	var current v1alpha1.HugrNodeGroup
	_ = w.client.Get(ctx, types.NamespacedName{Namespace: "hugr", Name: "late"}, &current)
	current.Spec.Replicas = 3 // passed the webhook alone, not with early's 3
	if err := w.client.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	g := w.reconcile(t, "late")
	if !meta.IsStatusConditionTrue(g.Status.Conditions, ConditionRefused) {
		t.Fatalf("a group past the plan is not refused: %v", g.Status.Conditions)
	}
	if d := w.deployment(t, "late"); d == nil || *d.Spec.Replicas != 1 {
		t.Errorf("a refused group's running node is taken away or grown: %+v", d)
	}
	if early := w.reconcile(t, "early"); meta.IsStatusConditionTrue(early.Status.Conditions, ConditionRefused) {
		t.Error("the earlier group is refused for the later one's change")
	}
}

// TestRefusedGroupScalesDown: a group refused by the plan keeps what it runs, but a scale-down is applied - the one
// change that brings it back within the plan
func TestRefusedGroupScalesDown(t *testing.T) {
	g := group("etl", 2, t0)
	w := newWorld(t, g)
	w.reconcile(t, "etl")
	ctx := context.Background()
	var platform v1alpha1.HugrPlatform
	_ = w.client.Get(ctx, types.NamespacedName{Namespace: "hugr", Name: "hugr"}, &platform)
	platform.Spec.Plan.Sizes = []string{"other"} // the group's size leaves the plan
	if err := w.client.Update(ctx, &platform); err != nil {
		t.Fatal(err)
	}
	var current v1alpha1.HugrNodeGroup
	_ = w.client.Get(ctx, types.NamespacedName{Namespace: "hugr", Name: "etl"}, &current)
	current.Spec.Replicas = 1
	_ = w.client.Update(ctx, &current)
	status := w.reconcile(t, "etl").Status
	if !meta.IsStatusConditionTrue(status.Conditions, ConditionRefused) {
		t.Fatalf("a size out of the plan is not refused: %v", status.Conditions)
	}
	if d := w.deployment(t, "etl"); d == nil || *d.Spec.Replicas != 1 {
		t.Errorf("the refused group's scale-down is not applied: %+v", d)
	}
}

// TestOrphanedDeploymentCounts: a Deployment whose group is gone still runs its nodes, and the plan counts them
func TestOrphanedDeploymentCounts(t *testing.T) {
	two := int32(3)
	orphan := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "hugr-node-gone", Namespace: "hugr",
		Labels: map[string]string{build.LabelGroup: "gone"}}, Spec: appsv1.DeploymentSpec{Replicas: &two}}
	w := newWorld(t, orphan, group("etl", 2, t0))
	status := w.reconcile(t, "etl").Status // 3 orphaned + 2 asked > 4
	if !meta.IsStatusConditionTrue(status.Conditions, ConditionRefused) {
		t.Errorf("a group growing into an orphaned Deployment's nodes: %v", status.Conditions)
	}
}

func TestReconcileStatusFromLeases(t *testing.T) {
	g := group("etl", 2, t0)
	pods := []client.Object{}
	for _, name := range []string{"hugr-node-etl-a", "hugr-node-etl-b", "hugr-node-etl-c"} {
		pods = append(pods, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "hugr", Labels: build.SelectorLabels(g)}})
	}
	no := false
	live := lease(t0, 30, nodeproto.Summary{Target: 5, Applied: 5, Sessions: 7, GroupKnown: &no})
	live.Name, live.Namespace, live.Labels = "hugr-node-etl-a", "hugr", map[string]string{build.LabelGroup: "etl"}
	restart := lease(t0, 30, nodeproto.Summary{Target: 5, Applied: 4, NeedsRestart: true, Sessions: 1})
	restart.Name, restart.Namespace, restart.Labels = "hugr-node-etl-b", "hugr", map[string]string{build.LabelGroup: "etl"}
	gone := lease(t0.Add(-time.Minute), 30, nodeproto.Summary{Target: 5, Applied: 5})
	gone.Name, gone.Namespace, gone.Labels = "hugr-node-etl-c", "hugr", map[string]string{build.LabelGroup: "etl"}
	w := newWorld(t, append(pods, g, live, restart, gone)...)
	w.now = t0.Add(10 * time.Second)
	status := w.reconcile(t, "etl").Status
	if status.Profile.Target != 5 || status.Profile.Applied != 1 {
		t.Errorf("profile %+v", status.Profile)
	}
	if len(status.Profile.NeedsRestart) != 1 || status.Profile.NeedsRestart[0] != "hugr-node-etl-b" {
		t.Errorf("needs restart %v", status.Profile.NeedsRestart)
	}
	if len(status.Lost) != 1 || status.Lost[0] != "hugr-node-etl-c" {
		t.Errorf("lost %v - a Lease that stops being renewed makes its node lost", status.Lost)
	}
	if status.ResourceGroupKnown == nil || *status.ResourceGroupKnown ||
		!meta.IsStatusConditionTrue(status.Conditions, ConditionDraining) {
		t.Errorf("a node without its resource group: known %v, conditions %v", status.ResourceGroupKnown, status.Conditions)
	}
	// the pod-deletion cost follows the reported sessions
	var a, b, c corev1.Pod
	ctx := context.Background()
	_ = w.client.Get(ctx, types.NamespacedName{Namespace: "hugr", Name: "hugr-node-etl-a"}, &a)
	_ = w.client.Get(ctx, types.NamespacedName{Namespace: "hugr", Name: "hugr-node-etl-b"}, &b)
	_ = w.client.Get(ctx, types.NamespacedName{Namespace: "hugr", Name: "hugr-node-etl-c"}, &c)
	if a.Annotations[AnnotationDeletionCost] != "4" || b.Annotations[AnnotationDeletionCost] != "1" {
		t.Errorf("deletion costs %q %q, want 4 (7 sessions) and 1", a.Annotations[AnnotationDeletionCost], b.Annotations[AnnotationDeletionCost])
	}
	if c.Annotations[AnnotationDeletionCost] != "-3" {
		t.Errorf("a lost node's cost %q, want -3: a scale-down takes it first", c.Annotations[AnnotationDeletionCost])
	}
}

// TestStatusOverAStaleRead: the group the reconciler read is a version behind (kubectl changed its spec since) - the
// status is written all the same, not refused as a conflict and retried with a growing backoff.
func TestStatusOverAStaleRead(t *testing.T) {
	w := newWorld(t, group("etl", 1, t0))
	ctx := context.Background()
	var stale v1alpha1.HugrNodeGroup
	key := types.NamespacedName{Namespace: "hugr", Name: "etl"}
	if err := w.client.Get(ctx, key, &stale); err != nil {
		t.Fatal(err)
	}
	fresh := stale.DeepCopy()
	fresh.Spec.Replicas = 2
	if err := w.client.Update(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	status := stale.Status.DeepCopy()
	status.ReadyReplicas = 1
	if err := w.reconciler.writeStatus(ctx, &stale, status); err != nil {
		t.Fatalf("the status over a stale read: %v", err)
	}
	if err := w.client.Get(ctx, key, fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.Status.ReadyReplicas != 1 || fresh.Spec.Replicas != 2 {
		t.Errorf("ready %d, replicas %d: want the status written and kubectl's spec kept", fresh.Status.ReadyReplicas, fresh.Spec.Replicas)
	}
}

func TestReconcileWithoutPlatform(t *testing.T) {
	g := group("etl", 1, t0)
	g.Spec.Platform = "other"
	w := newWorld(t, g)
	status := w.reconcile(t, "etl").Status
	if c := meta.FindStatusCondition(status.Conditions, ConditionReady); c == nil || c.Reason != "PlatformMissing" {
		t.Errorf("conditions %v", status.Conditions)
	}
	if w.deployment(t, "etl") != nil {
		t.Error("a group without its platform runs nodes")
	}
}

func TestDeletionCost(t *testing.T) {
	for sessions, want := range map[int64]int64{0: 0, 1: 1, 2: 2, 3: 2, 7: 4, 8: 8, 1000: 512} {
		if got := DeletionCost(NodeState{Summary: nodeproto.Summary{Sessions: sessions}}, 0, t0); got != want {
			t.Errorf("%d sessions cost %d, want %d", sessions, got, want)
		}
	}
	if DeletionCost(NodeState{Lost: true, Summary: nodeproto.Summary{Sessions: 900}}, 0, t0) != -3 {
		t.Error("a lost node keeps its last cost")
	}
	// spec 011: in an auto group a node idle past its idle time goes before any live one, after a lost one
	idle := NodeState{Summary: nodeproto.Summary{IdleSince: t0.Add(-time.Hour).Unix()}}
	if got := DeletionCost(idle, 30*time.Minute, t0); got != -2 {
		t.Errorf("an idle node of an auto group costs %d, want -2", got)
	}
	if got := DeletionCost(idle, 0, t0); got != 0 {
		t.Errorf("a manual group has no idle time: %d", got)
	}
	recent := NodeState{Summary: nodeproto.Summary{IdleSince: t0.Add(-time.Minute).Unix()}}
	if got := DeletionCost(recent, 30*time.Minute, t0); got != 0 {
		t.Errorf("idle, but not for long: %d", got)
	}
}

// TestAutoscaleShrinksAnIdleNode: an auto group (spec 011) whose node has had no session past its idle time loses a
// replica, that node costs -2 so the scale-down takes it, and the decision is in the status; a manual group is left as
// it is
func TestAutoscaleShrinksAnIdleNode(t *testing.T) {
	g := group("etl", 2, t0)
	g.Spec.Scaling = &v1alpha1.ScalingSpec{Mode: "auto", Min: 1, Max: 3}
	var objs []client.Object
	for _, name := range []string{"hugr-node-etl-a", "hugr-node-etl-b"} {
		objs = append(objs, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "hugr", Labels: build.SelectorLabels(g)}})
	}
	busy := lease(t0, 30, nodeproto.Summary{Sessions: 3, SessionsMax: 1000, MemoryUsed: 0.2, TempUsed: -1, SeatsUsed: -1})
	busy.Name, busy.Namespace, busy.Labels = "hugr-node-etl-a", "hugr", map[string]string{build.LabelGroup: "etl"}
	idle := lease(t0, 30, nodeproto.Summary{SessionsMax: 1000, MemoryUsed: 0.1, TempUsed: -1, SeatsUsed: -1,
		IdleSince: t0.Add(-time.Hour).Unix()})
	idle.Name, idle.Namespace, idle.Labels = "hugr-node-etl-b", "hugr", map[string]string{build.LabelGroup: "etl"}
	// an evicted pod (Failed, kept until pod GC) with its Lease long expired: no node, no pod of the group
	evicted := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "hugr-node-etl-old", Namespace: "hugr",
		Labels: build.SelectorLabels(g)}, Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"}}
	ghost := lease(t0.Add(-time.Hour), 30, nodeproto.Summary{})
	ghost.Name, ghost.Namespace, ghost.Labels = "hugr-node-etl-old", "hugr", map[string]string{build.LabelGroup: "etl"}
	w := newWorld(t, append(objs, g, busy, idle, evicted, ghost)...)
	w.now = t0.Add(10 * time.Second)
	// the first look builds the Deployment: nothing runs the group's 2 yet, no step before it does
	if first := w.reconcile(t, "etl"); first.Spec.Replicas != 2 {
		t.Fatalf("a step before the Deployment runs the replicas: %d", first.Spec.Replicas)
	}
	got := w.reconcile(t, "etl")
	if got.Spec.Replicas != 1 {
		t.Fatalf("the idle node's replica stays: %d", got.Spec.Replicas)
	}
	last := got.Status.Scaling.Last
	if last == nil || last.From != 2 || last.To != 1 || !strings.Contains(last.Reason, "hugr-node-etl-b") {
		t.Errorf("the decision in the status: %+v", last)
	}
	var b corev1.Pod
	_ = w.client.Get(context.Background(), types.NamespacedName{Namespace: "hugr", Name: "hugr-node-etl-b"}, &b)
	if b.Annotations[AnnotationDeletionCost] != "-2" {
		t.Errorf("the idle node costs %q, want -2: the scale-down takes it", b.Annotations[AnnotationDeletionCost])
	}
	// the next look is within the cooldown: nothing more, whatever the leases say
	if len(got.Status.Lost) != 0 {
		t.Errorf("an evicted pod is not a lost node: %v", got.Status.Lost)
	}
	// the next look: both pods still run (the ReplicaSet has not taken one yet) - the step is being made, nothing more
	if again := w.reconcile(t, "etl"); again.Spec.Replicas != 1 {
		t.Errorf("a second step before the first is made: %d", again.Spec.Replicas)
	}
	// the Deployment follows the group
	if d := w.deployment(t, "etl"); d == nil || *d.Spec.Replicas != 1 {
		t.Errorf("the Deployment does not follow the autoscaler: %+v", d)
	}

	manual := group("bi", 2, t0)
	m := newWorld(t, manual, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "hugr-node-bi-a", Namespace: "hugr",
		Labels: build.SelectorLabels(manual)}}, func() client.Object {
		l := lease(t0, 30, nodeproto.Summary{IdleSince: t0.Add(-time.Hour).Unix()})
		l.Name, l.Namespace, l.Labels = "hugr-node-bi-a", "hugr", map[string]string{build.LabelGroup: "bi"}
		return l
	}())
	m.now = t0.Add(10 * time.Second)
	if bi := m.reconcile(t, "bi"); bi.Spec.Replicas != 2 || bi.Status.Scaling != nil {
		t.Errorf("a manual group is the operator's number: %d %+v", bi.Spec.Replicas, bi.Status.Scaling)
	}
}

// TestCooldownRidesOnTheReplicasWrite (the re-review): the status patch that records the last change may be lost (a
// leader's end between the two writes) - the look after must still keep the cooldown, from the change's time written
// with the replicas, or a second idle node goes at once
func TestCooldownRidesOnTheReplicasWrite(t *testing.T) {
	g := group("etl", 3, t0)
	g.Spec.Scaling = &v1alpha1.ScalingSpec{Mode: "auto", Min: 1, Max: 3}
	objs := []client.Object{g}
	for i, name := range []string{"hugr-node-etl-a", "hugr-node-etl-b", "hugr-node-etl-c"} {
		objs = append(objs, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "hugr", Labels: build.SelectorLabels(g)}})
		summary := nodeproto.Summary{SessionsMax: 1000, MemoryUsed: 0.1, TempUsed: -1, SeatsUsed: -1,
			IdleSince: t0.Add(-time.Hour).Unix()}
		if i == 0 {
			summary.Sessions, summary.IdleSince = 2, 0
		}
		l := lease(t0, 600, summary) // renewed through the test's minutes
		l.Name, l.Namespace, l.Labels = name, "hugr", map[string]string{build.LabelGroup: "etl"}
		objs = append(objs, l)
	}
	w := newWorld(t, objs...)
	w.now = t0.Add(10 * time.Second)
	w.reconcile(t, "etl") // builds the Deployment
	if got := w.reconcile(t, "etl"); got.Spec.Replicas != 2 || got.Annotations[AnnotationScaledAt] == "" {
		t.Fatalf("the first idle node goes, its time with it: %d %v", got.Spec.Replicas, got.Annotations)
	}
	// the next look applies 2 to the Deployment; then the status that said so is gone, and the pod the step took too
	w.now = t0.Add(15 * time.Second)
	w.reconcile(t, "etl")
	ctx := context.Background()
	var current v1alpha1.HugrNodeGroup
	_ = w.client.Get(ctx, types.NamespacedName{Namespace: "hugr", Name: "etl"}, &current)
	current.Status.Scaling = nil
	if err := w.client.Status().Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	var taken corev1.Pod
	_ = w.client.Get(ctx, types.NamespacedName{Namespace: "hugr", Name: "hugr-node-etl-b"}, &taken)
	_ = w.client.Delete(ctx, &taken)
	w.now = t0.Add(20 * time.Second)
	if got := w.reconcile(t, "etl"); got.Spec.Replicas != 2 {
		t.Errorf("a second idle node within the cooldown, its status lost: %d", got.Spec.Replicas)
	}
	w.now = t0.Add(3 * time.Minute)
	if got := w.reconcile(t, "etl"); got.Spec.Replicas != 1 {
		t.Errorf("past the cooldown the next idle node goes: %d", got.Spec.Replicas)
	}
}

// TestDemandWakesAGroupAtZero (spec 011 stage 2): the front's demand Lease for an auto group's resource group starts
// its node at once and is answered "starting"; an expired one is no demand; and with nobody holding a client the
// group's last idle node stops, its min being 0
func TestDemandWakesAGroupAtZero(t *testing.T) {
	g := group("etl", 0, t0)
	g.Spec.Scaling = &v1alpha1.ScalingSpec{Mode: "auto", Min: 0, Max: 2}
	demand := func(renewed time.Time) *coordinationv1.Lease {
		l := lease(renewed, nodeproto.DemandSeconds, nodeproto.Summary{})
		l.Name, l.Namespace = nodeproto.DemandLeaseName("etl"), "hugr"
		l.Labels = map[string]string{nodeproto.LabelDemand: "true"}
		l.Annotations = map[string]string{nodeproto.AnnotationDemandGroup: "etl"}
		return l
	}
	stale := newWorld(t, g.DeepCopy(), demand(t0.Add(-time.Minute)))
	stale.now = t0
	if got := stale.reconcile(t, "etl"); got.Spec.Replicas != 0 {
		t.Errorf("an expired demand starts a node: %d", got.Spec.Replicas)
	}

	w := newWorld(t, g, demand(t0))
	w.now = t0.Add(5 * time.Second)
	got := w.reconcile(t, "etl")
	if got.Spec.Replicas != 1 || !strings.Contains(got.Status.Scaling.Last.Reason, "client waits") {
		t.Fatalf("a held client wakes the group: %d %+v", got.Spec.Replicas, got.Status.Scaling)
	}
	var answered coordinationv1.Lease
	_ = w.client.Get(context.Background(), types.NamespacedName{Namespace: "hugr", Name: nodeproto.DemandLeaseName("etl")}, &answered)
	if answered.Annotations[nodeproto.AnnotationStarting] != "etl" {
		t.Errorf("the demand is not answered: %v", answered.Annotations)
	}

	// later: the node serves, nobody waits, it has been idle past idle - the last node of a min-0 group stops
	idle := group("bi", 1, t0)
	idle.Spec.Scaling = &v1alpha1.ScalingSpec{Mode: "auto", Min: 0, Max: 2}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "hugr-node-bi-a", Namespace: "hugr", Labels: build.SelectorLabels(idle)}}
	beat := lease(t0, 600, nodeproto.Summary{SessionsMax: 1000, MemoryUsed: 0.1, TempUsed: -1, SeatsUsed: -1,
		IdleSince: t0.Add(-time.Hour).Unix()})
	beat.Name, beat.Namespace, beat.Labels = "hugr-node-bi-a", "hugr", map[string]string{build.LabelGroup: "bi"}
	b := newWorld(t, idle, pod, beat)
	b.now = t0.Add(10 * time.Second)
	b.reconcile(t, "bi") // builds the Deployment
	if got := b.reconcile(t, "bi"); got.Spec.Replicas != 0 {
		t.Errorf("the last idle node of a min-0 group stays: %d", got.Spec.Replicas)
	}
}

// TestDemandAnswerAndFloor (the review): "starting" only while a node is on its way - never for a group whose node
// runs and is not placeable (a client should hear "no node" soon); another group's answer is not ours to clear; and
// the policy's default group keeps a floor of 1 though a heartbeat no longer says it is the default
func TestDemandAnswerAndFloor(t *testing.T) {
	demand := func(starting string) *coordinationv1.Lease {
		l := lease(t0, nodeproto.DemandSeconds, nodeproto.Summary{})
		l.Name, l.Namespace = nodeproto.DemandLeaseName("etl"), "hugr"
		l.Labels = map[string]string{nodeproto.LabelDemand: "true"}
		l.Annotations = map[string]string{nodeproto.AnnotationDemandGroup: "etl"}
		if starting != "" {
			l.Annotations[nodeproto.AnnotationStarting] = starting
		}
		return l
	}
	answer := func(w *world) string {
		var l coordinationv1.Lease
		_ = w.client.Get(context.Background(), types.NamespacedName{Namespace: "hugr", Name: nodeproto.DemandLeaseName("etl")}, &l)
		return l.Annotations[nodeproto.AnnotationStarting]
	}
	// a group at its max whose one node runs, at capacity (admits nobody): no node is coming
	full := group("etl", 1, t0)
	full.Spec.Scaling = &v1alpha1.ScalingSpec{Mode: "auto", Min: 0, Max: 1}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "hugr-node-etl-a", Namespace: "hugr", Labels: build.SelectorLabels(full)}}
	beat := lease(t0, 600, nodeproto.Summary{Sessions: 1000, SessionsMax: 1000, MemoryUsed: 0.2, TempUsed: -1, SeatsUsed: -1})
	beat.Name, beat.Namespace, beat.Labels = "hugr-node-etl-a", "hugr", map[string]string{build.LabelGroup: "etl"}
	w := newWorld(t, full, pod, beat, demand(""))
	w.now = t0.Add(5 * time.Second)
	w.reconcile(t, "etl")
	w.reconcile(t, "etl")
	if got := answer(w); got != "" {
		t.Errorf("a full group at its max answered %q: its client would be held for nothing", got)
	}

	// a new node that reported and does not admit yet (its readiness, a heartbeat more) is still on its way: the held
	// client keeps waiting for it (the review)
	waking := group("etl", 1, t0)
	waking.Spec.Scaling = &v1alpha1.ScalingSpec{Mode: "auto", Min: 0, Max: 1}
	young := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "hugr-node-etl-a", Namespace: "hugr",
		Labels: build.SelectorLabels(waking), CreationTimestamp: metav1.NewTime(t0)}}
	first := lease(t0, 600, nodeproto.Summary{SessionsMax: 1000, MemoryUsed: 0.1, TempUsed: -1, SeatsUsed: -1})
	first.Name, first.Namespace, first.Labels = "hugr-node-etl-a", "hugr", map[string]string{build.LabelGroup: "etl"}
	// its pod waited long for a VM and an image: what is young is its first heartbeat, its Lease
	young.CreationTimestamp = metav1.NewTime(t0.Add(-time.Hour))
	first.CreationTimestamp = metav1.NewTime(t0)
	y := newWorld(t, waking, young, first, demand(""))
	y.now = t0.Add(20 * time.Second)
	y.reconcile(t, "etl")
	if got := answer(y); got != "etl" {
		t.Errorf("a young node not admitting yet is not on its way: %q", got)
	}

	// two groups of one resource group: the other's answer stays...
	other := group("etl", 0, t0)
	other.Spec.Scaling = &v1alpha1.ScalingSpec{Mode: "auto", Min: 0, Max: 1}
	blue := group("etl-blue", 1, t0)
	blue.Spec.ResourceGroup = "etl"
	blue.Spec.Scaling = &v1alpha1.ScalingSpec{Mode: "auto", Min: 0, Max: 1}
	o := newWorld(t, other, blue, demand("etl-blue"))
	o.now = t0.Add(5 * time.Second)
	o.reconcile(t, "etl")
	if got := answer(o); got != "etl-blue" {
		t.Errorf("another group's answer was replaced or cleared: %q", got)
	}
	// ...but not an answer of a group that is gone - or no longer auto: no node comes from it (the review); a gone
	// group's answer on another resource group's demand too (a deleted group's own look never comes)
	elsewhere := demand("etl-gone")
	elsewhere.Name = nodeproto.DemandLeaseName("bi")
	elsewhere.Annotations[nodeproto.AnnotationDemandGroup] = "bi"
	e := newWorld(t, other.DeepCopy(), elsewhere)
	e.now = t0.Add(5 * time.Second)
	e.reconcile(t, "etl")
	var dropped coordinationv1.Lease
	_ = e.client.Get(context.Background(), types.NamespacedName{Namespace: "hugr", Name: nodeproto.DemandLeaseName("bi")}, &dropped)
	if got := dropped.Annotations[nodeproto.AnnotationStarting]; got != "" {
		t.Errorf("a gone group's answer on another resource group's demand stays: %q", got)
	}
	gone := newWorld(t, other.DeepCopy(), demand("etl-gone"))
	gone.now = t0.Add(5 * time.Second)
	gone.reconcile(t, "etl")
	if got := answer(gone); got == "etl-gone" {
		t.Error("a gone group's answer stays")
	}
	manualBlue := blue.DeepCopy()
	manualBlue.Spec.Scaling = nil
	m := newWorld(t, manualBlue, demand("etl-blue"))
	m.now = t0.Add(5 * time.Second)
	m.reconcile(t, "etl-blue")
	if got := answer(m); got != "" {
		t.Errorf("a group switched to manual keeps its answer: %q", got)
	}

	// the default group, once known as such, keeps one node when a heartbeat no longer says so
	def := group("etl", 1, t0)
	def.Spec.Scaling = &v1alpha1.ScalingSpec{Mode: "auto", Min: 0, Max: 2}
	defPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "hugr-node-etl-a", Namespace: "hugr", Labels: build.SelectorLabels(def)}}
	said := lease(t0, 600, nodeproto.Summary{GroupDefault: true, SessionsMax: 1000, MemoryUsed: 0.1, TempUsed: -1,
		SeatsUsed: -1, IdleSince: t0.Add(-time.Hour).Unix()})
	said.Name, said.Namespace, said.Labels = "hugr-node-etl-a", "hugr", map[string]string{build.LabelGroup: "etl"}
	d := newWorld(t, def, defPod, said)
	d.now = t0.Add(5 * time.Second)
	if got := d.reconcile(t, "etl"); got.Status.Scaling == nil || got.Status.Scaling.DefaultGroup != "etl" {
		t.Fatalf("the default group is not kept: %+v", got.Status.Scaling)
	}
	var beatNow coordinationv1.Lease
	_ = d.client.Get(context.Background(), types.NamespacedName{Namespace: "hugr", Name: "hugr-node-etl-a"}, &beatNow)
	beatNow.Annotations[nodeproto.AnnotationGroupDefault] = "false" // a heartbeat during a catalog hiccup
	_ = d.client.Update(context.Background(), &beatNow)
	if got := d.reconcile(t, "etl"); got.Spec.Replicas != 1 {
		t.Errorf("the default group's last idle node stopped on one heartbeat: %d", got.Spec.Replicas)
	}
	// the policy moves its default to bi (ALTER RESOURCE GROUP bi SET DEFAULT): bi's node says so, and etl's floor goes
	moved := lease(t0, 600, nodeproto.Summary{Group: "bi", GroupDefault: true, AdmitSession: true})
	moved.Name, moved.Namespace, moved.Labels = "hugr-node-bi-a", "hugr", map[string]string{build.LabelGroup: "bi"}
	if err := d.client.Create(context.Background(), moved); err != nil {
		t.Fatal(err)
	}
	if got := d.reconcile(t, "etl"); got.Status.Scaling.DefaultGroup != "" {
		t.Errorf("the default moved, etl keeps its floor: %q", got.Status.Scaling.DefaultGroup)
	}
}
