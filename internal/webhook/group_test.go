package webhook

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
			TLS: v1alpha1.TLSSpec{SecretRef: &v1alpha1.SecretRef{Name: "hugr-node-" + name + "-tls"}}}}
}

// csi is a group whose doors' certificate the csi-driver issues, from the namespace's Issuer hugr-internal.
func csi(name, duration string) *v1alpha1.HugrNodeGroup {
	g := group(name, "large", 1)
	g.Spec.TLS = v1alpha1.TLSSpec{CSI: &v1alpha1.CSICertificate{
		IssuerRef: v1alpha1.IssuerRef{Name: "hugr-internal"}, Duration: duration}}
	return g
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
		{"a certificate the csi-driver issues", csi("etl", ""), ""},
		{"a csi certificate of a day", csi("etl", "24h"), ""},
		{"a csi certificate shorter than an hour", csi("etl", "30m"), "at least 1h"},
		{"both a Secret and the csi-driver", func() *v1alpha1.HugrNodeGroup {
			g := csi("etl", "")
			g.Spec.TLS.SecretRef = &v1alpha1.SecretRef{Name: "hugr-node-etl-tls"}
			return g
		}(), "exactly one"},
		{"no certificate at all", func() *v1alpha1.HugrNodeGroup {
			g := group("etl", "large", 1)
			g.Spec.TLS.SecretRef = nil
			return g
		}(), "exactly one"},
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

// a read the webhook cannot make answers a fixed message: the API server's own names the operator's ServiceAccount, and
// a person reads the answer in the console (spec 006)
func TestWebhookReadFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	forbidden := errors.New(`hugrplatforms.platform.hugr.io "hugr" is forbidden: User "system:serviceaccount:hugr:hugr-platform" cannot get resource`)
	for _, fail := range []interceptor.Funcs{
		{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return forbidden
		}},
		{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return forbidden
		}},
	} {
		platform := &v1alpha1.HugrPlatform{ObjectMeta: metav1.ObjectMeta{Name: "hugr", Namespace: "hugr"},
			Spec: v1alpha1.HugrPlatformSpec{Sizes: map[string]v1alpha1.SizeSpec{"large": {Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Gi")}},
				Temp: v1alpha1.TempSpec{Size: resource.MustParse("100Gi")}}},
				Plan: v1alpha1.PlanSpec{MaxNodes: 6, Sizes: []string{"large"}}}}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(platform).WithInterceptorFuncs(fail).Build()
		_, err := (&GroupValidator{Reader: c}).ValidateCreate(context.Background(), group("bi", "large", 1))
		if !errors.Is(err, errNotRead) || strings.Contains(err.Error(), "serviceaccount") {
			t.Errorf("a failed read answers: %v", err)
		}
	}
}

// a resource group the first policy refuses would stop every node's bootstrap: refused where it is written, and a group
// that names one already keeps it until the name changes (spec 006)
func TestWebhookResourceGroup(t *testing.T) {
	ctx := context.Background()
	v := validator(t)
	dotted := csi("reports", "720h")
	dotted.Spec.ResourceGroup = "reports.eu"
	if _, err := v.ValidateCreate(ctx, dotted); err == nil || !strings.Contains(err.Error(), "not a plain name") {
		t.Errorf("a dotted resource group made: %v", err)
	}
	plain := csi("reports", "720h")
	plain.Spec.ResourceGroup = "reports_eu"
	if _, err := v.ValidateCreate(ctx, plain); err != nil {
		t.Errorf("a plain resource group refused: %v", err)
	}
	if _, err := v.ValidateUpdate(ctx, plain, dotted); err == nil {
		t.Error("a resource group changed to a dotted one")
	}
	kept := dotted.DeepCopy()
	kept.Spec.Replicas = 0
	if _, err := v.ValidateUpdate(ctx, dotted, kept); err != nil {
		t.Errorf("a group keeping its resource group is refused a scale-down: %v", err)
	}
}
