package console

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
)

// The console's names in server-side apply, one per field it sets: an apply removes the fields its owner set before
// and does not name now, so the count and the restart are owned apart - a restart never drops the count a scale set.
// Both are apart from the operator's (hugr-platform) and the administrator's.
const (
	FieldOwner        = "hugr-console"
	RestartFieldOwner = "hugr-console-restart"
)

// ChangedByAnnotation names, on a group, who last changed it from the console: the person's `sub` at the IdP, never a
// token (spec 003).
const ChangedByAnnotation = "platform.hugr.io/changed-by"

// GroupChange is one write of a group: its count or its restart.
type GroupChange struct {
	Namespace, Name string
	ChangedBy       string
	Replicas        *int32
	RestartedAt     string
}

// GroupWriter writes a group's change.
type GroupWriter interface {
	Change(ctx context.Context, change GroupChange) error
}

// ApplyWriter writes by server-side apply, taking the fields it sets: the count a person sets in the console is
// the console's until someone sets it otherwise (a later apply with another owner conflicts and has to force).
type ApplyWriter struct {
	Client client.Client
}

// Owner of the change's fields.
func (c GroupChange) Owner() string {
	if c.Replicas == nil {
		return RestartFieldOwner
	}
	return FieldOwner
}

// Content of the apply: only the fields the change sets, so nothing else of the group changes owner.
func (c GroupChange) Content() map[string]any {
	spec := map[string]any{}
	if c.Replicas != nil {
		spec["replicas"] = int64(*c.Replicas)
	}
	if c.RestartedAt != "" {
		spec["restartedAt"] = c.RestartedAt
	}
	metadata := map[string]any{"name": c.Name, "namespace": c.Namespace}
	if c.ChangedBy != "" {
		metadata["annotations"] = map[string]any{ChangedByAnnotation: c.ChangedBy}
	}
	return map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "HugrNodeGroup",
		"metadata":   metadata,
		"spec":       spec,
	}
}

// Change applies the change.
func (w ApplyWriter) Change(ctx context.Context, change GroupChange) error {
	applied := client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: change.Content()})
	return w.Client.Apply(ctx, applied, client.FieldOwner(change.Owner()), client.ForceOwnership)
}
