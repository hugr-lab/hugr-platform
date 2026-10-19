//go:build envtest

// envtest (spec 002): the front's objects from spec.front, and the front's view of the fleet from a real API
// server's EndpointSlices and Leases.
package envtest

import (
	"context"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/build"
	"github.com/hugr-lab/hugr-platform/internal/controller"
	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
	"github.com/hugr-lab/hugr-platform/internal/front/watch"
	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

func TestFrontObjects(t *testing.T) {
	c := start(t)
	ctx := context.Background()
	var p v1alpha1.HugrPlatform
	must(t, c.client.Get(ctx, key("hugr"), &p))
	// the front mounts no Secret but its own: another's name is refused by the API server
	p.Spec.Front = &v1alpha1.FrontSpec{Certificate: v1alpha1.FrontCertificateSpec{SecretName: "hugr-platform-tls"}}
	if err := c.client.Update(ctx, &p); err == nil || !apierrors.IsInvalid(err) {
		t.Fatalf("a front naming another Secret: %v", err)
	}
	must(t, c.client.Get(ctx, key("hugr"), &p))
	replicas := int32(3)
	p.Spec.Front = &v1alpha1.FrontSpec{Replicas: &replicas, ServiceType: corev1.ServiceTypeLoadBalancer,
		Certificate: v1alpha1.FrontCertificateSpec{SecretName: "hugr-front-public"},
		CABundle:    &v1alpha1.CABundleSpec{ConfigMapRef: v1alpha1.ConfigMapKeyRef{Name: "hugr-ca", Key: "bundle.pem"}}}
	must(t, c.client.Update(ctx, &p))

	var deployment appsv1.Deployment
	eventually(t, 15*time.Second, "the front's Deployment", func() bool {
		return c.client.Get(ctx, key(build.FrontName), &deployment) == nil
	})
	pod := deployment.Spec.Template.Spec
	if *deployment.Spec.Replicas != 3 || pod.Containers[0].Image != "ghcr.io/hugr-lab/hugr-platform:dev" ||
		pod.Containers[0].Command[0] != "/front" || pod.ServiceAccountName != build.FrontName {
		t.Errorf("the Deployment: %+v", deployment.Spec)
	}
	if !slices.Contains(pod.Containers[0].Args, "--heartbeat=1s") || !slices.Contains(pod.Containers[0].Args, "--no-node-wait=5s") {
		t.Errorf("the front's arguments: %q", pod.Containers[0].Args)
	}
	if l := deployment.Spec.Template.Labels; l[build.LabelPartOf] != build.ManagedBy || l[build.LabelComponent] != "front" {
		t.Errorf("the pods are not the ones the nodes' NetworkPolicy admits: %v", l)
	}
	var service corev1.Service
	must(t, c.client.Get(ctx, key(build.FrontName), &service))
	if service.Spec.Type != corev1.ServiceTypeLoadBalancer || len(service.Spec.Ports) != 2 ||
		service.Spec.Ports[0].Port != 32010 || service.Spec.Ports[1].Port != 9494 {
		t.Errorf("the Service: %+v", service.Spec)
	}
	var role rbacv1.Role
	must(t, c.client.Get(ctx, key(build.FrontName), &role))
	for _, rule := range role.Rules {
		if slices.ContainsFunc(rule.Verbs, func(v string) bool { return v != "get" && v != "list" && v != "watch" }) ||
			len(rule.Resources) != 1 || (rule.Resources[0] != "endpointslices" && rule.Resources[0] != "leases") {
			t.Errorf("the front's Role reads more than the fleet: %+v", rule)
		}
	}
	for _, obj := range []client.Object{&rbacv1.RoleBinding{}, &corev1.ServiceAccount{}, &policyv1.PodDisruptionBudget{}} {
		must(t, c.client.Get(ctx, key(build.FrontName), obj))
	}
	eventually(t, 10*time.Second, "the platform says its front is applied", func() bool {
		_ = c.client.Get(ctx, key("hugr"), &p)
		return meta.IsStatusConditionTrue(p.Status.Conditions, controller.ConditionFront)
	})

	// no spec.front: no front
	p.Spec.Front = nil
	must(t, c.client.Update(ctx, &p))
	eventually(t, 15*time.Second, "the front's objects are gone", func() bool {
		for _, obj := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}, &rbacv1.Role{},
			&rbacv1.RoleBinding{}, &corev1.ServiceAccount{}, &policyv1.PodDisruptionBudget{}} {
			if err := c.client.Get(ctx, key(build.FrontName), obj); !apierrors.IsNotFound(err) {
				return false
			}
		}
		return true
	})
}

func TestFrontWatch(t *testing.T) {
	c := start(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	nodes := fleet.New()
	w := watch.New(c.clientset, ns, nodes, time.Second)
	if w.Ready() == nil {
		t.Fatal("ready before the informers synced")
	}
	go w.Run(ctx)
	eventually(t, 10*time.Second, "the front is ready once the fleet is read", func() bool { return w.Ready() == nil })

	ready, port, flightPort := true, int32(9494), int32(32010)
	quack, flight := "quack", "flight"
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "hugr-node-etl-x1", Namespace: ns,
			Labels: map[string]string{discoveryv1.LabelServiceName: "hugr-node-etl"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: &flight, Port: &flightPort}, {Name: &quack, Port: &port}},
		Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.7"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready},
			TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "hugr-node-etl-abc", Namespace: ns}}},
	}
	must(t, c.client.Create(ctx, slice))
	// a Service of something else is no node
	other := slice.DeepCopy()
	other.Name, other.Labels = "web-x1", map[string]string{discoveryv1.LabelServiceName: "web"}
	other.ResourceVersion, other.UID = "", ""
	other.Endpoints[0].TargetRef.Name = "web-abc"
	must(t, c.client.Create(ctx, other))

	pick := func(v *fleet.View) *fleet.Node {
		return v.Choose(fleet.Quack, []string{"etl"}, nil, time.Now(), rand.New(rand.NewPCG(1, 1)))
	}
	eventually(t, 10*time.Second, "the endpoint is a node, not placeable without a Lease", func() bool {
		v, _ := nodes.View()
		n := v.Routable("hugr-node-etl-abc", fleet.Quack)
		return n != nil && n.Address == "10.0.0.7" && n.Service == "hugr-node-etl" && pick(v) == nil &&
			v.Routable("web-abc", fleet.Quack) == nil
	})

	now := metav1.NewMicroTime(time.Now())
	seconds := int32(60)
	summary := nodeproto.Summary{Group: "etl", AdmitSession: true, GroupDefault: true, SeatsLeft: 3}
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "hugr-node-etl-abc", Namespace: ns, Annotations: summary.Annotations(),
			Labels: map[string]string{build.LabelManagedBy: build.ManagedBy}},
		Spec: coordinationv1.LeaseSpec{RenewTime: &now, LeaseDurationSeconds: &seconds},
	}
	must(t, c.client.Create(ctx, lease))
	eventually(t, 10*time.Second, "a current, admitting Lease: placeable, its group the default", func() bool {
		v, _ := nodes.View()
		return pick(v) != nil && slices.Equal(v.DefaultGroups(time.Now()), []string{"etl"})
	})

	// the node stops admitting: still routable, no longer placeable
	summary.AdmitSession = false
	lease.Annotations = summary.Annotations()
	must(t, c.client.Update(ctx, lease))
	eventually(t, 10*time.Second, "not admitting", func() bool {
		v, _ := nodes.View()
		return pick(v) == nil && v.Routable("hugr-node-etl-abc", fleet.Quack) != nil
	})

	// a Lease that is not renewed: not placeable again, whatever it says
	summary.AdmitSession = true
	old := metav1.NewMicroTime(time.Now().Add(-2 * time.Minute))
	lease.Annotations, lease.Spec.RenewTime = summary.Annotations(), &old
	must(t, c.client.Update(ctx, lease))
	eventually(t, 10*time.Second, "an expired Lease", func() bool {
		v, _ := nodes.View()
		n := v.Routable("hugr-node-etl-abc", fleet.Quack)
		return n != nil && n.Lease != nil && !n.Current(time.Now()) && pick(v) == nil
	})

	// a Lease the operator does not keep is not read: a second pod with one, current and admitting, is no seat
	must(t, c.client.Get(ctx, client.ObjectKeyFromObject(slice), slice))
	slice.Endpoints = append(slice.Endpoints, discoveryv1.Endpoint{Addresses: []string{"10.0.0.8"},
		Conditions: discoveryv1.EndpointConditions{Ready: &ready},
		TargetRef:  &corev1.ObjectReference{Kind: "Pod", Name: "hugr-node-etl-def", Namespace: ns}})
	must(t, c.client.Update(ctx, slice))
	stray := lease.DeepCopy()
	stray.ResourceVersion, stray.UID, stray.Name, stray.Labels = "", "", "hugr-node-etl-def", nil
	stray.Spec.RenewTime = &now
	must(t, c.client.Create(ctx, stray))
	eventually(t, 10*time.Second, "the second pod is seen, its Lease is not", func() bool {
		v, _ := nodes.View()
		n := v.Routable("hugr-node-etl-def", fleet.Quack)
		return n != nil && n.Lease == nil && pick(v) == nil
	})

	must(t, c.client.Delete(ctx, slice))
	eventually(t, 10*time.Second, "the endpoint gone, the node gone", func() bool {
		v, _ := nodes.View()
		return v.Routable("hugr-node-etl-abc", fleet.Quack) == nil && len(v.Nodes()) == 0
	})
}
