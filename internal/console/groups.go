package console

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
)

// The console's names in server-side apply, one per field it sets: an apply removes the fields its owner set before
// and does not name now, so each field is owned apart - a restart never drops the count a scale set, a new size never
// the doors (spec 006). All are apart from the operator's (hugr-platform) and the administrator's.
const (
	FieldOwner        = "hugr-console"
	RestartFieldOwner = "hugr-console-restart"
	SizeFieldOwner    = "hugr-console-size"
	DoorsFieldOwner   = "hugr-console-doors"
	DrainFieldOwner   = "hugr-console-drain"
	// CreateFieldOwner made the group (a create, not an apply: a later apply of one field takes that field over and
	// leaves the rest as they were made)
	CreateFieldOwner = "hugr-console-create"
)

// ChangedByAnnotation names, on a group, who last changed it from the console: the person's `sub` at the IdP, never a
// token (spec 003).
const ChangedByAnnotation = "platform.hugr.io/changed-by"

// GroupChange is one write of a group: exactly one of its count, its restart, its size, its doors, its drain time.
type GroupChange struct {
	Namespace, Name string
	ChangedBy       string
	Replicas        *int32
	RestartedAt     string
	Size            string
	Quack           *bool
	DrainSeconds    *int32
}

// GroupWriter writes a group: a change of one field, a new group, a group's deletion.
type GroupWriter interface {
	Change(ctx context.Context, change GroupChange) error
	Create(ctx context.Context, group *v1alpha1.HugrNodeGroup) error
	Delete(ctx context.Context, namespace, name string) error
}

// ApplyWriter writes by server-side apply, taking the fields it sets: the count a person sets in the console is
// the console's until someone sets it otherwise (a later apply with another owner conflicts and has to force).
type ApplyWriter struct {
	Client client.Client
}

// Owner of the change's field.
func (c GroupChange) Owner() string {
	switch {
	case c.Replicas != nil:
		return FieldOwner
	case c.Size != "":
		return SizeFieldOwner
	case c.Quack != nil:
		return DoorsFieldOwner
	case c.DrainSeconds != nil:
		return DrainFieldOwner
	}
	return RestartFieldOwner
}

// Content of the apply: only the field the change sets, so nothing else of the group changes owner.
func (c GroupChange) Content() map[string]any {
	spec := map[string]any{}
	switch {
	case c.Replicas != nil:
		spec["replicas"] = int64(*c.Replicas)
	case c.Size != "":
		spec["size"] = c.Size
	case c.Quack != nil:
		spec["doors"] = map[string]any{"quack": *c.Quack}
	case c.DrainSeconds != nil:
		spec["drainSeconds"] = int64(*c.DrainSeconds)
	case c.RestartedAt != "":
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

// Create makes the group; one that exists already is the API server's conflict.
func (w ApplyWriter) Create(ctx context.Context, group *v1alpha1.HugrNodeGroup) error {
	return w.Client.Create(ctx, group, client.FieldOwner(CreateFieldOwner))
}

// Delete deletes the group: its Deployment goes with it (the owner reference), each pod drains at its SIGTERM.
func (w ApplyWriter) Delete(ctx context.Context, namespace, name string) error {
	group := &v1alpha1.HugrNodeGroup{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	return w.Client.Delete(ctx, group, client.PropagationPolicy(metav1.DeletePropagationBackground))
}
