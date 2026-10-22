package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/build"
	"github.com/hugr-lab/hugr-platform/internal/upgrade"
)

func (w *world) platform(t *testing.T) *v1alpha1.HugrPlatform {
	t.Helper()
	var p v1alpha1.HugrPlatform
	if err := w.client.Get(context.Background(), types.NamespacedName{Namespace: "hugr", Name: "hugr"}, &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

func (w *world) image(t *testing.T, group string) string {
	t.Helper()
	d := w.deployment(t, group)
	if d == nil {
		t.Fatalf("no Deployment for %s", group)
	}
	return build.NodeImage(&d.Spec.Template.Spec)
}

// spec 008: a new node image moves no group until the rollout decides, and then only the canary
func TestRolloutMovesTheCanaryFirst(t *testing.T) {
	w := newWorld(t, group("a", 1, t0), group("b", 1, t0))
	w.reconcile(t, "a")
	w.reconcile(t, "b")
	up := &UpgradeReconciler{Client: w.client, Now: func() time.Time { return w.now },
		Repository: func(context.Context, *v1alpha1.HugrPlatform, []string, []string) (string, error) { return "", nil }}
	run := func() {
		t.Helper()
		if _, err := up.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: "hugr", Name: "hugr"}}); err != nil {
			t.Fatal(err)
		}
	}
	run()
	if p := w.platform(t); p.Status.Image != "img" || p.Status.Upgrade != nil {
		t.Fatalf("a fresh install rolls nothing: %+v", p.Status)
	}
	p := w.platform(t)
	p.Spec.Node.Image = "img2"
	if err := w.client.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	w.reconcile(t, "a")
	if got := w.image(t, "a"); got != "img" {
		t.Fatalf("a group moved before the rollout decided: %s", got)
	}
	run()
	status := w.platform(t).Status
	if status.Upgrade == nil || status.Upgrade.Phase != upgrade.PhaseCanary || status.Upgrade.Group != "a" ||
		status.Image != "img" {
		t.Fatalf("the rollout's start: %+v", status)
	}
	w.reconcile(t, "a")
	w.reconcile(t, "b")
	if a, b := w.image(t, "a"), w.image(t, "b"); a != "img2" || b != "img" {
		t.Errorf("the canary takes the target and the others wait: a %s, b %s", a, b)
	}
}

func TestDeploymentRolled(t *testing.T) {
	two := int32(2)
	d := func(gen, observed int64, updated, available, total int32) *appsv1.Deployment {
		x := &appsv1.Deployment{}
		x.Generation, x.Spec.Replicas = gen, &two
		x.Status = appsv1.DeploymentStatus{ObservedGeneration: observed, UpdatedReplicas: updated,
			AvailableReplicas: available, Replicas: total}
		return x
	}
	if ok, _ := DeploymentRolled(d(3, 3, 2, 2, 2)); !ok {
		t.Error("every replica updated and available is not rolled")
	}
	for name, x := range map[string]*appsv1.Deployment{
		"its generation not observed": d(3, 2, 2, 2, 2),
		"an old pod left":             d(3, 3, 2, 2, 3),
		"a new pod not available":     d(3, 3, 2, 1, 2),
	} {
		if ok, _ := DeploymentRolled(x); ok {
			t.Errorf("%s: rolled", name)
		}
	}
	stuck := d(3, 3, 1, 2, 3)
	stuck.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing,
		Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded", Message: "timed out progressing"}}
	if ok, failed := DeploymentRolled(stuck); ok || failed != "timed out progressing" {
		t.Errorf("past its deadline: %v %q", ok, failed)
	}
}
