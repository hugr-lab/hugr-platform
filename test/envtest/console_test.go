//go:build envtest

package envtest

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

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
}
