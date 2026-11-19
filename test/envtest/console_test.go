//go:build envtest

package envtest

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/console"
)

// The console's writes of a group (spec 003): a server-side apply of the count or the restart alone, as its own field
// owner, the webhook's refusal readable by the person.
func TestConsoleGroupWrites(t *testing.T) {
	c := start(t)
	ctx := context.Background()
	must(t, c.client.Create(ctx, group("etl", 1)))
	writer := console.ApplyWriter{Client: c.client}

	three := int32(3)
	must(t, writer.Change(ctx, console.GroupChange{Namespace: ns, Name: "etl", ChangedBy: "ops@example.org",
		Replicas: &three}))
	var g v1alpha1.HugrNodeGroup
	must(t, c.client.Get(ctx, key("etl"), &g))
	if g.Spec.Replicas != 3 || g.Annotations[console.ChangedByAnnotation] != "ops@example.org" ||
		g.Spec.Identity.ClientID != "etl-app" || g.Spec.Size != "small" {
		t.Fatalf("the count and who, nothing else changed: %+v %v", g.Spec, g.Annotations)
	}
	owned := false
	for _, f := range g.ManagedFields {
		if f.Manager == console.FieldOwner {
			owned = true
		}
	}
	if !owned {
		t.Error("the console owns the fields it set")
	}

	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	must(t, writer.Change(ctx, console.GroupChange{Namespace: ns, Name: "etl", ChangedBy: "ops@example.org",
		RestartedAt: at}))
	must(t, c.client.Get(ctx, key("etl"), &g))
	if g.Spec.RestartedAt != at || g.Spec.Replicas != 3 {
		t.Errorf("a restart sets restartedAt and keeps the count it does not name: %+v", g.Spec)
	}
	// and a later scale keeps the restart
	two := int32(2)
	must(t, writer.Change(ctx, console.GroupChange{Namespace: ns, Name: "etl", ChangedBy: "ops@example.org",
		Replicas: &two}))
	must(t, c.client.Get(ctx, key("etl"), &g))
	if g.Spec.RestartedAt != at || g.Spec.Replicas != 2 {
		t.Errorf("a scale keeps the restart it does not name: %+v", g.Spec)
	}

	// past the plan (4 nodes in all): the webhook refuses through the API server
	five := int32(5)
	err := writer.Change(ctx, console.GroupChange{Namespace: ns, Name: "etl", ChangedBy: "ops@example.org",
		Replicas: &five})
	if err == nil || !(apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apierrors.IsBadRequest(err)) {
		t.Fatalf("the webhook refuses past the plan: %v", err)
	}
	must(t, c.client.Get(ctx, key("etl"), &g))
	if g.Spec.Replicas != 2 {
		t.Errorf("a refused change changes nothing: %d", g.Spec.Replicas)
	}

	// spec 011 stage 4: the scaling's mode, bounds and idle - the thresholds someone set where the group is defined
	// stay theirs
	patch := client.MergeFrom(g.DeepCopy())
	g.Spec.Scaling = &v1alpha1.ScalingSpec{Mode: "manual", ScaleUp: v1alpha1.ScaleUpSpec{MemoryUsed: 70}}
	must(t, c.client.Patch(ctx, &g, patch, client.FieldOwner("kubectl")))
	must(t, writer.Change(ctx, console.GroupChange{Namespace: ns, Name: "etl", ChangedBy: "ops@example.org",
		Scaling: &console.ScalingChange{Mode: "auto", Min: 0, Max: 3, Idle: 45 * time.Minute}}))
	must(t, c.client.Get(ctx, key("etl"), &g))
	if sc := g.Spec.Scaling; sc == nil || sc.Mode != "auto" || sc.Max != 3 || sc.Idle == nil ||
		sc.Idle.Duration != 45*time.Minute || sc.ScaleUp.MemoryUsed != 70 {
		t.Fatalf("auto from the console, the threshold kept: %+v", g.Spec.Scaling)
	}
	// past the plan at its max (4 in all): refused
	if err := writer.Change(ctx, console.GroupChange{Namespace: ns, Name: "etl",
		Scaling: &console.ScalingChange{Mode: "auto", Max: 5}}); err == nil {
		t.Error("an auto group's max past the plan")
	}
	must(t, writer.Change(ctx, console.GroupChange{Namespace: ns, Name: "etl", ChangedBy: "ops@example.org",
		Scaling: &console.ScalingChange{Mode: "manual"}}))
	must(t, c.client.Get(ctx, key("etl"), &g))
	if sc := g.Spec.Scaling; sc == nil || sc.Mode != "manual" || sc.ScaleUp.MemoryUsed != 70 {
		t.Errorf("back to manual, the threshold kept: %+v", g.Spec.Scaling)
	}
}
