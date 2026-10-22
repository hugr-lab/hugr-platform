package initjob

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/hugr-lab/hugr-platform/internal/crds"
)

type fakeCRDs struct {
	existing map[string]*apiextensionsv1.CustomResourceDefinition
	applied  []string
}

func (f *fakeCRDs) Get(_ context.Context, name string) (*apiextensionsv1.CustomResourceDefinition, error) {
	return f.existing[name], nil
}

func (f *fakeCRDs) Apply(_ context.Context, name string, body []byte) error {
	var crd apiextensionsv1.CustomResourceDefinition
	if err := json.Unmarshal(body, &crd); err != nil || crd.Name != name {
		return err
	}
	f.applied = append(f.applied, name)
	return nil
}

func installed(name string, versions []string, stored ...string) *apiextensionsv1.CustomResourceDefinition {
	crd := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: name}}
	for _, v := range versions {
		crd.Spec.Versions = append(crd.Spec.Versions, apiextensionsv1.CustomResourceDefinitionVersion{Name: v, Served: true})
	}
	crd.Status.StoredVersions = stored
	return crd
}

func TestRunCRDs(t *testing.T) {
	manifests, err := crds.Manifests()
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	// a cluster with the same versions (an older schema of them): both applied
	same := &fakeCRDs{existing: map[string]*apiextensionsv1.CustomResourceDefinition{
		"hugrplatforms.platform.hugr.io":  installed("hugrplatforms.platform.hugr.io", []string{"v1alpha1"}, "v1alpha1"),
		"hugrnodegroups.platform.hugr.io": installed("hugrnodegroups.platform.hugr.io", []string{"v1alpha1"}, "v1alpha1"),
	}}
	if err := RunCRDs(ctx, manifests, same, log); err != nil || len(same.applied) != 2 {
		t.Fatalf("the release's CRDs over the same versions: %v, applied %v", err, same.applied)
	}

	// none there yet (a cluster whose crds/ were skipped): applied
	none := &fakeCRDs{existing: map[string]*apiextensionsv1.CustomResourceDefinition{}}
	if err := RunCRDs(ctx, manifests, none, log); err != nil || len(none.applied) != 2 {
		t.Fatalf("no CRDs yet: %v, applied %v", err, none.applied)
	}

	// a version the cluster serves, or has stored objects in, that the release drops: refused, nothing applied
	for name, existing := range map[string]*apiextensionsv1.CustomResourceDefinition{
		"served": installed("hugrnodegroups.platform.hugr.io", []string{"v1alpha1", "v1beta1"}, "v1alpha1"),
		"stored": installed("hugrnodegroups.platform.hugr.io", []string{"v1alpha1"}, "v1alpha1", "v1beta1"),
	} {
		dropping := &fakeCRDs{existing: map[string]*apiextensionsv1.CustomResourceDefinition{existing.Name: existing}}
		err := RunCRDs(ctx, manifests, dropping, log)
		if err == nil || !strings.Contains(err.Error(), "v1beta1") || len(dropping.applied) != 0 {
			t.Errorf("a %s version dropped: %v, applied %v", name, err, dropping.applied)
		}
	}
}
