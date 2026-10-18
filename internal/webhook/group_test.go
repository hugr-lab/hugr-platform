package webhook

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
)

func validator(t *testing.T, objs ...client.Object) *GroupValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	sized := v1alpha1.SizeSpec{
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Gi")}},
		Temp: v1alpha1.TempSpec{Size: resource.MustParse("100Gi")},
	}
	unsized := v1alpha1.SizeSpec{Temp: v1alpha1.TempSpec{Size: resource.MustParse("1Gi")}}
	platform := &v1alpha1.HugrPlatform{ObjectMeta: metav1.ObjectMeta{Name: "hugr", Namespace: "hugr"},
		Spec: v1alpha1.HugrPlatformSpec{
			Sizes: map[string]v1alpha1.SizeSpec{"large": sized, "small": sized, "bare": unsized},
			Plan:  v1alpha1.PlanSpec{MaxNodes: 6, Sizes: []string{"large", "bare"}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, platform)...).Build()
	return &GroupValidator{Reader: c}
}

func group(name, size string, replicas int32) *v1alpha1.HugrNodeGroup {
	return &v1alpha1.HugrNodeGroup{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "hugr"},
		Spec: v1alpha1.HugrNodeGroupSpec{Platform: "hugr", Size: size, Replicas: replicas,
			TLS: v1alpha1.TLSSpec{SecretRef: v1alpha1.SecretRef{Name: "hugr-node-" + name + "-tls"}}}}
}

func TestWebhook(t *testing.T) {
	ctx := context.Background()
	v := validator(t, group("bi", "large", 3))
	cases := []struct {
		name  string
		group *v1alpha1.HugrNodeGroup
		want  string // "" = admitted
	}{
		{"a fitting change", group("etl", "large", 3), ""},
		{"a total past maxNodes over every group", group("etl", "large", 4), "at most 6"},
		{"a size not in the plan", group("etl", "small", 1), "not in the plan"},
		{"a size nobody lists", group("etl", "huge", 1), "not one the platform lists"},
		{"a size the node cannot size its engine from", group("etl", "bare", 1), "no memory limit"},
		{"a name that makes no labels", group("etl.prod", "large", 1), "DNS label"},
		{"a certificate that is another Secret of the namespace", func() *v1alpha1.HugrNodeGroup {
			g := group("etl", "large", 1)
			g.Spec.TLS.SecretRef.Name = "hugr-platform-tls"
			return g
		}(), "a group mounts no other Secret"},
		{"a platform that is not there", func() *v1alpha1.HugrNodeGroup { g := group("etl", "large", 1); g.Spec.Platform = "x"; return g }(), "is not in the namespace"},
	}
	for _, c := range cases {
		_, err := v.ValidateCreate(ctx, c.group)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	// a change that does not grow a group passes while the namespace is past the plan: scaled down, restarted
	past := validator(t, group("bi", "large", 5), group("etl", "large", 4))
	down := group("etl", "large", 2)
	if _, err := past.ValidateUpdate(ctx, group("etl", "large", 4), down); err != nil {
		t.Errorf("a scale-down past the plan: %v", err)
	}
	restarted := group("etl", "large", 4)
	restarted.Spec.RestartedAt = "2026-10-19T08:00:00Z"
	if _, err := past.ValidateUpdate(ctx, group("etl", "large", 4), restarted); err != nil {
		t.Errorf("a restart past the plan: %v", err)
	}
	if _, err := past.ValidateUpdate(ctx, group("etl", "large", 4), group("etl", "large", 5)); err == nil {
		t.Error("growing past the plan is let through")
	}
	// an update counts the group once: bi grows from 3 to 6 alone
	if _, err := v.ValidateUpdate(ctx, group("bi", "large", 3), group("bi", "large", 6)); err != nil {
		t.Errorf("a group growing into the whole plan: %v", err)
	}
	if _, err := v.ValidateDelete(ctx, group("bi", "large", 3)); err != nil {
		t.Errorf("a delete is refused: %v", err)
	}
}
