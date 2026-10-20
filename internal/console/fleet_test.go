package console

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"reflect"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
)

func TestFleetOf(t *testing.T) {
	known := true
	platform := &v1alpha1.HugrPlatform{ObjectMeta: metav1.ObjectMeta{Name: "hugr"},
		Spec: v1alpha1.HugrPlatformSpec{Plan: v1alpha1.PlanSpec{MaxNodes: 12, Sizes: []string{"small", "large"}}}}
	groups := []v1alpha1.HugrNodeGroup{
		{ObjectMeta: metav1.ObjectMeta{Name: "general"}, Spec: v1alpha1.HugrNodeGroupSpec{Platform: "hugr", Replicas: 1}},
		{ObjectMeta: metav1.ObjectMeta{Name: "etl"}, Spec: v1alpha1.HugrNodeGroupSpec{Platform: "hugr", Replicas: 3,
			Size: "large", ResourceGroup: "batch", RestartedAt: "2026-10-19T10:00:00Z"},
			Status: v1alpha1.HugrNodeGroupStatus{ReadyReplicas: 2, ResourceGroupKnown: &known,
				Profile: v1alpha1.ProfileStatus{Target: 14, Applied: 2, NeedsRestart: []string{"hugr-node-etl-b"}},
				Lost:    []string{"hugr-node-etl-c"},
				Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Scaling",
					Message: "2 of 3"}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "elsewhere"}, Spec: v1alpha1.HugrNodeGroupSpec{Platform: "other"}},
	}
	starting := node("hugr-node-etl-c", "etl")
	starting.Lease = nil
	starting.Ready = false
	view := fleet.NewView([]fleet.Node{node("hugr-node-etl-b", "etl"), node("hugr-node-etl-a", "etl"), starting})
	got := FleetOf(platform, groups, view, testNow, KubeState{Usage: map[string]Usage{"hugr-node-etl-a": {MemoryBytes: 42}}})
	if got.Plan.MaxNodes != 12 || !reflect.DeepEqual(got.Plan.Sizes, []string{"small", "large"}) {
		t.Errorf("the plan: %+v", got.Plan)
	}
	if len(got.Groups) != 2 || got.Groups[0].Name != "etl" || got.Groups[1].Name != "general" {
		t.Fatalf("this platform's groups by name: %+v", got.Groups)
	}
	etl := got.Groups[0]
	if etl.ResourceGroup != "batch" || etl.ResourceGroupKnown == nil || !*etl.ResourceGroupKnown ||
		etl.Profile.Target != 14 || etl.ReadyReplicas != 2 || len(etl.Lost) != 1 || etl.RestartedAt == "" ||
		len(etl.Conditions) != 1 || etl.Conditions[0].Reason != "Scaling" || etl.Scaling.Mode != "manual" {
		t.Errorf("the group's spec and status: %+v", etl)
	}
	if len(etl.Nodes) != 3 || etl.Nodes[0].Label != "hugr-node-etl-a" || etl.Nodes[2].Label != "hugr-node-etl-c" {
		t.Fatalf("its nodes in order: %+v", etl.Nodes)
	}
	a, c := etl.Nodes[0], etl.Nodes[2]
	if a.LeaseAge == nil || *a.LeaseAge != 4 || !a.Current || a.Sessions != 3 || a.SeatsLeft != 9 || !a.Admitting ||
		a.Usage == nil || a.Usage.MemoryBytes != 42 || etl.Nodes[1].Usage != nil {
		t.Errorf("a node from its Lease: %+v", a)
	}
	if c.LeaseAge != nil || c.Current || c.SeatsLeft != -1 || c.Ready {
		t.Errorf("a node before its first heartbeat: %+v", c)
	}
	if general := got.Groups[1]; general.ResourceGroup != "general" || len(general.Nodes) != 0 || general.Nodes == nil {
		t.Errorf("a group without nodes answers an empty list, its resource group its name: %+v", general)
	}
}

func TestGroupChangeContent(t *testing.T) {
	three := int32(3)
	scale := GroupChange{Namespace: "hugr", Name: "etl", ChangedBy: "ops@example.org", Replicas: &three}.Content()
	want := map[string]any{
		"apiVersion": "platform.hugr.io/v1alpha1", "kind": "HugrNodeGroup",
		"metadata": map[string]any{"name": "etl", "namespace": "hugr",
			"annotations": map[string]any{ChangedByAnnotation: "ops@example.org"}},
		"spec": map[string]any{"replicas": int64(3)},
	}
	if !reflect.DeepEqual(scale, want) {
		t.Errorf("a scale applies the count and who, nothing else of the group:\n%v\n%v", scale, want)
	}
	restart := GroupChange{Namespace: "hugr", Name: "etl", RestartedAt: "2026-10-19T12:00:00Z"}.Content()
	if spec := restart["spec"].(map[string]any); len(spec) != 1 || spec["restartedAt"] != "2026-10-19T12:00:00Z" {
		t.Errorf("a restart applies restartedAt only: %v", spec)
	}
	if _, ok := restart["metadata"].(map[string]any)["annotations"]; ok {
		t.Error("no changed-by without a subject")
	}
}

func TestRootsAndPlatform(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	bundle := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	platform := &v1alpha1.HugrPlatform{ObjectMeta: metav1.ObjectMeta{Name: "hugr", Namespace: "hugr"},
		Spec: v1alpha1.HugrPlatformSpec{CABundle: &v1alpha1.CABundleSpec{
			ConfigMapRef: v1alpha1.ConfigMapKeyRef{Name: "hugr-ca", Key: "ca.crt"}}}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "hugr-ca", Namespace: "hugr"},
		Data: map[string]string{"ca.crt": bundle}}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(platform, cm).Build()
	finder := PlatformFinder{Reader: reader, Namespace: "hugr"}
	found, err := finder.Find(context.Background())
	if err != nil || found.Name != "hugr" {
		t.Fatalf("the only platform of the namespace: %v %v", found, err)
	}
	roots := &Roots{Platform: finder.Find, Direct: reader}
	first, err := roots.Pool(context.Background())
	if err != nil || first == nil {
		t.Fatalf("the bundle: %v", err)
	}
	second, _ := roots.Pool(context.Background())
	if first != second {
		t.Error("an unchanged bundle is the same pool: the channels are not restarted")
	}
	two := platform.DeepCopy()
	two.Name, two.ResourceVersion = "second", ""
	if err := reader.Create(context.Background(), two); err != nil {
		t.Fatal(err)
	}
	if _, err := finder.Find(context.Background()); err == nil {
		t.Error("two platforms and none named: the console says which to serve")
	}
	if p, err := (PlatformFinder{Reader: reader, Namespace: "hugr", Name: "second"}).Find(context.Background()); err != nil ||
		p.Name != "second" {
		t.Errorf("the one named: %v %v", p, err)
	}
	bare := &Roots{Platform: func(context.Context) (*v1alpha1.HugrPlatform, error) {
		return &v1alpha1.HugrPlatform{}, nil
	}, Direct: reader}
	if _, err := bare.Pool(context.Background()); err == nil {
		t.Error("no CA bundle: the console speaks to no node")
	}
}

func TestPodState(t *testing.T) {
	waiting := func(reason string) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: "hugr-node", State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: reason + " says why"}}}
	}
	ready := corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue}
	now := metav1.NewTime(testNow)
	crash := waiting("CrashLoopBackOff")
	crash.RestartCount = 4
	crash.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}
	for name, c := range map[string]struct {
		pod                    corev1.Pod
		state, reason, message string
	}{
		"placed nowhere": {pod: corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
				Message: "0/1 nodes are available: 1 Insufficient cpu."}}}},
			state: "failing", reason: "Unschedulable", message: "0/1 nodes are available: 1 Insufficient cpu."},
		"being made": {pod: corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{waiting("ContainerCreating")}}}, state: "starting", reason: "ContainerCreating"},
		"no image": {pod: corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{waiting("ImagePullBackOff")}}},
			state: "failing", reason: "ImagePullBackOff", message: "ImagePullBackOff says why"},
		"crashing": {pod: corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{crash}}},
			state: "failing", reason: "CrashLoopBackOff", message: "the container exited with 137 (OOMKilled)"},
		"bootstrapping": {pod: corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}, state: "starting", reason: "NotReady"},
		"serving":       {pod: corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{ready}}}, state: "running"},
		"going": {pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{ready}}}, state: "stopping", reason: "Terminating"},
		"evicted": {pod: corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted",
			Message: "The node was low on resource: memory."}}, state: "failing", reason: "Evicted", message: "The node was low on resource: memory."},
	} {
		got := podState(&c.pod)
		if got.State != c.state || got.Reason != c.reason || (c.message != "" && got.Message != c.message) {
			t.Errorf("%s: %+v", name, got)
		}
	}
	if got := podState(&corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{crash}}}); got.Restarts != 4 {
		t.Errorf("the restarts counted: %+v", got)
	}
}

// A scale's new node is in its group from its pod on: before its endpoint and Lease, starting or failing to; a
// Deployment that cannot make a pod says so on the group.
func TestFleetOfAScale(t *testing.T) {
	platform := &v1alpha1.HugrPlatform{ObjectMeta: metav1.ObjectMeta{Name: "hugr"}}
	groups := []v1alpha1.HugrNodeGroup{{ObjectMeta: metav1.ObjectMeta{Name: "etl"},
		Spec: v1alpha1.HugrNodeGroupSpec{Platform: "hugr", Replicas: 3}, Status: v1alpha1.HugrNodeGroupStatus{ReadyReplicas: 1}}}
	pod := func(name string, status corev1.PodStatus) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"platform.hugr.io/group": "etl"}},
			Status: status}
	}
	kube := KubeState{
		Pods: []corev1.Pod{
			pod("hugr-node-etl-a", corev1.PodStatus{Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}),
			pod("hugr-node-etl-new", corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "no room"}}}),
		},
		Deployments: []appsv1.Deployment{{ObjectMeta: metav1.ObjectMeta{Name: "hugr-node-etl",
			Labels: map[string]string{"platform.hugr.io/group": "etl"}}, Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate",
				Message: `pods "hugr-node-etl-x" is forbidden: exceeded quota`}}}}},
	}
	got := FleetOf(platform, groups, fleet.NewView([]fleet.Node{node("hugr-node-etl-a", "etl")}), testNow, kube)
	etl := got.Groups[0]
	if len(etl.Nodes) != 2 || etl.Nodes[0].Pod == nil || etl.Nodes[0].Pod.State != "running" {
		t.Fatalf("the serving node with its pod: %+v", etl.Nodes)
	}
	if n := etl.Nodes[1]; n.Label != "hugr-node-etl-new" || n.Ready || n.LeaseAge != nil || n.Pod == nil ||
		n.Pod.State != "failing" || n.Pod.Message != "no room" {
		t.Errorf("the scale's pod, not placed: %+v %+v", n, n.Pod)
	}
	if etl.Problem != `a pod cannot be made: pods "hugr-node-etl-x" is forbidden: exceeded quota` {
		t.Errorf("the Deployment's failure on the group: %q", etl.Problem)
	}
}
