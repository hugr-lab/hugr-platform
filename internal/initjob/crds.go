package initjob

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"
)

// CRDFieldManager is the name hugr-init applies the CRDs under.
const CRDFieldManager = "hugr-init"

// CRDs is what the crds phase asks of the API server.
type CRDs interface {
	// Get answers the CRD of a name, or nil when there is none
	Get(ctx context.Context, name string) (*apiextensionsv1.CustomResourceDefinition, error)
	// Apply a CRD's JSON, server-side
	Apply(ctx context.Context, name string, body []byte) error
}

// RunCRDs is the crds phase (spec 008): before an upgrade, the new release's CRDs - the ones its operator's image
// carries - applied server-side. Within a version the fields only grow; a CRD that would drop a version the cluster
// serves or has stored objects in is refused, and nothing is applied then.
func RunCRDs(ctx context.Context, manifests [][]byte, crds CRDs, log *slog.Logger) error {
	type plan struct {
		name string
		body []byte
	}
	var plans []plan
	for _, manifest := range manifests {
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.UnmarshalStrict(manifest, &crd); err != nil {
			return fmt.Errorf("hugr-init: a CRD of the image does not read: %w", err)
		}
		existing, err := crds.Get(ctx, crd.Name)
		if err != nil {
			return fmt.Errorf("hugr-init: the CRD %s: %w", crd.Name, err)
		}
		if existing != nil {
			if dropped := droppedVersions(existing, &crd); len(dropped) > 0 {
				return fmt.Errorf("hugr-init: the release's CRD %s drops the version(s) %v that the cluster serves or "+
					"stores: an API version goes only through a conversion - nothing was applied", crd.Name, dropped)
			}
		}
		body, err := yaml.YAMLToJSON(manifest)
		if err != nil {
			return err
		}
		plans = append(plans, plan{name: crd.Name, body: body})
	}
	for _, p := range plans {
		if err := crds.Apply(ctx, p.name, p.body); err != nil {
			return fmt.Errorf("hugr-init: the CRD %s not applied: %w", p.name, err)
		}
		log.Info("the release's CRD is applied", "crd", p.name)
	}
	return nil
}

// droppedVersions: the versions the cluster serves or stores that the new CRD does not name
func droppedVersions(existing, next *apiextensionsv1.CustomResourceDefinition) []string {
	names := map[string]bool{}
	for _, v := range next.Spec.Versions {
		names[v.Name] = true
	}
	var dropped []string
	for _, v := range existing.Spec.Versions {
		if v.Served && !names[v.Name] && !slices.Contains(dropped, v.Name) {
			dropped = append(dropped, v.Name)
		}
	}
	for _, v := range existing.Status.StoredVersions {
		if !names[v] && !slices.Contains(dropped, v) {
			dropped = append(dropped, v)
		}
	}
	return dropped
}

// KubeCRDs is CRDs on the API server.
type KubeCRDs struct {
	Client apiextensionsclient.CustomResourceDefinitionInterface
}

// Get a CRD, nil when there is none.
func (k KubeCRDs) Get(ctx context.Context, name string) (*apiextensionsv1.CustomResourceDefinition, error) {
	crd, err := k.Client.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return crd, err
}

// Apply a CRD server-side, taking its fields over from whoever wrote them (Helm's install of crds/, a kubectl apply).
func (k KubeCRDs) Apply(ctx context.Context, name string, body []byte) error {
	force := true
	_, err := k.Client.Patch(ctx, name, types.ApplyPatchType, body,
		metav1.PatchOptions{FieldManager: CRDFieldManager, Force: &force})
	return err
}
