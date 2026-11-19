//go:build envtest

// envtest (spec 011 stage 2): the policy that keeps the front to its demand Leases, against a real API server - the
// front's Role may create and patch every Lease of the namespace, the policy refuses all but its demand Leases.
package envtest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/hugr-lab/hugr-platform/internal/build"
	"github.com/hugr-lab/hugr-platform/internal/front/demand"
	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

func TestFrontWritesItsDemandLeasesOnly(t *testing.T) {
	env := &envtest.Environment{}
	config, err := env.Start()
	if err != nil {
		t.Fatalf("envtest: %v (run through make test-envtest: it fetches the API server)", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx := context.Background()
	admin := kubernetes.NewForConfigOrDie(config)
	_, err = admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	must(t, err)
	// the front's own Role and binding, as the operator writes them
	role := build.FrontRole(platform())
	role.Namespace = ns
	_, err = admin.RbacV1().Roles(ns).Create(ctx, role, metav1.CreateOptions{})
	must(t, err)
	binding := build.FrontRoleBinding(platform())
	binding.Namespace, binding.Subjects[0].Namespace = ns, ns
	_, err = admin.RbacV1().RoleBindings(ns).Create(ctx, binding, metav1.CreateOptions{})
	must(t, err)
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "policy", "front-leases.yaml"))
	must(t, err)
	for _, document := range strings.Split(string(raw), "\n---\n") {
		if strings.Contains(document, "kind: ValidatingAdmissionPolicyBinding") {
			var b admissionregistrationv1.ValidatingAdmissionPolicyBinding
			must(t, yaml.Unmarshal([]byte(document), &b))
			_, err = admin.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, &b, metav1.CreateOptions{})
		} else {
			var p admissionregistrationv1.ValidatingAdmissionPolicy
			must(t, yaml.Unmarshal([]byte(document), &p))
			_, err = admin.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, &p, metav1.CreateOptions{})
		}
		must(t, err)
	}
	// a node's Lease and the operator's leader Lease, the admin's
	seconds := int32(30)
	for _, name := range []string{"hugr-node-etl-a", "hugr-platform-operator"} {
		_, err = admin.CoordinationV1().Leases(ns).Create(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: coordinationv1.LeaseSpec{LeaseDurationSeconds: &seconds}}, metav1.CreateOptions{})
		must(t, err)
	}

	asFront := rest.CopyConfig(config)
	asFront.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + ns + ":" + build.FrontName,
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + ns, "system:authenticated"}}
	front := kubernetes.NewForConfigOrDie(asFront)
	// the policy takes a moment to be enforced: the front's patch of a node's Lease is refused by it
	eventually(t, 20*time.Second, "the front's patch of a node's Lease refused", func() bool {
		_, err := front.CoordinationV1().Leases(ns).Patch(ctx, "hugr-node-etl-a", types.MergePatchType,
			[]byte(`{"metadata":{"annotations":{"platform.hugr.io/sessions":"0"}}}`), metav1.PatchOptions{})
		return err != nil && strings.Contains(err.Error(), "the front writes its demand Leases only")
	})
	if _, err := front.CoordinationV1().Leases(ns).Patch(ctx, "hugr-platform-operator", types.MergePatchType,
		[]byte(`{"spec":{"holderIdentity":"the-front"}}`), metav1.PatchOptions{}); err == nil {
		t.Error("the front took the operator's leader Lease")
	}
	if _, err := front.CoordinationV1().Leases(ns).Create(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Name: "hugr-node-etl-b"}}, metav1.CreateOptions{}); err == nil {
		t.Error("the front made a node's Lease")
	}
	// its demand Lease, written by the front's own code, passes - created, then renewed by a patch
	d := &demand.Demand{Client: front, Namespace: ns, Holder: "hugr-front-0"}
	release := d.Hold([]string{"etl"})
	defer release()
	eventually(t, 20*time.Second, "the front's demand Lease written", func() bool {
		l, err := admin.CoordinationV1().Leases(ns).Get(ctx, nodeproto.DemandLeaseName("etl"), metav1.GetOptions{})
		return err == nil && l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity == "hugr-front-0"
	})
	if _, err := front.CoordinationV1().Leases(ns).Patch(ctx, nodeproto.DemandLeaseName("etl"), types.MergePatchType,
		[]byte(`{"spec":{"holderIdentity":"hugr-front-1"}}`), metav1.PatchOptions{}); err != nil {
		t.Errorf("the front's renew of its demand Lease: %v", err)
	}
	// ...but not a demand Lease stripped of its label, nor its delete (it has no such verb, and the policy refuses it)
	if _, err := front.CoordinationV1().Leases(ns).Patch(ctx, nodeproto.DemandLeaseName("etl"), types.MergePatchType,
		[]byte(`{"metadata":{"labels":{"platform.hugr.io/demand":null}}}`), metav1.PatchOptions{}); err == nil {
		t.Error("the front unmarked a demand Lease")
	}
}
