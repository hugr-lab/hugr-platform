package build

import (
	"strings"
	"testing"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
)

func TestFrontObjects(t *testing.T) {
	platform := &v1alpha1.HugrPlatform{}
	platform.Name, platform.Namespace = "hugr", "hugr"
	platform.Spec.Front = &v1alpha1.FrontSpec{FlightPort: 443, QuackPort: 8443}
	if _, err := FrontDeployment(platform, Settings{FrontImage: "img"}); err == nil ||
		!strings.Contains(err.Error(), "CA bundle") {
		t.Errorf("a front without a CA bundle to verify the nodes by: %v", err)
	}
	platform.Spec.CABundle = &v1alpha1.CABundleSpec{ConfigMapRef: v1alpha1.ConfigMapKeyRef{Name: "hugr-ca", Key: "ca.crt"}}
	if _, err := FrontDeployment(platform, Settings{}); err == nil {
		t.Error("a front without an image")
	}
	d, err := FrontDeployment(platform, Settings{FrontImage: "img"})
	if err != nil {
		t.Fatal(err)
	}
	if v := d.Spec.Template.Spec.Volumes; v[0].Secret.SecretName != "hugr-front" || v[1].ConfigMap.Name != "hugr-ca" ||
		v[1].ConfigMap.Items[0].Path != FrontCAFileName {
		t.Errorf("the certificate and the platform's CA bundle: %+v", v)
	}
	if c := d.Spec.Template.Spec.Containers[0]; c.Ports[0].ContainerPort != FlightPort || c.Ports[1].ContainerPort != QuackPort {
		t.Errorf("in the pod the doors listen on the nodes' ports: %+v", c.Ports)
	}
	if *d.Spec.Replicas != 2 || *d.Spec.Template.Spec.TerminationGracePeriodSeconds != FrontDrain+FrontUnready+5 {
		t.Errorf("replicas %d, grace %d", *d.Spec.Replicas, *d.Spec.Template.Spec.TerminationGracePeriodSeconds)
	}
	platform.Spec.Front.CABundle = &v1alpha1.CABundleSpec{ConfigMapRef: v1alpha1.ConfigMapKeyRef{Name: "nodes-ca", Key: "x"}}
	if ca, _ := FrontCABundle(platform); ca.ConfigMapRef.Name != "nodes-ca" {
		t.Error("the front's own bundle wins over the platform's")
	}
	s := FrontService(platform)
	if s.Spec.Type != "ClusterIP" || s.Spec.Ports[0].Port != 443 || s.Spec.Ports[1].Port != 8443 ||
		s.Spec.Ports[0].TargetPort.StrVal != "flight" {
		t.Errorf("the Service maps spec.front's ports onto the pod's: %+v", s.Spec)
	}
	for _, p := range s.Spec.Ports {
		if p.Name == "internal" {
			t.Error("the internal port is in the clients' Service")
		}
	}
}
