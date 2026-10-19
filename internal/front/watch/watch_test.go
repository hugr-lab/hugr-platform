package watch

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func slice(name string, kind discoveryv1.AddressType, address string, ready bool) *discoveryv1.EndpointSlice {
	port, portName := int32(32010), "flight"
	return &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: name, Labels: map[string]string{discoveryv1.LabelServiceName: "hugr-node-etl"}},
		AddressType: kind,
		Ports:       []discoveryv1.EndpointPort{{Name: &portName, Port: &port}},
		Endpoints: []discoveryv1.Endpoint{{Addresses: []string{address}, Conditions: discoveryv1.EndpointConditions{Ready: &ready},
			TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "hugr-node-etl-abc"}}},
	}
}

func TestViewIsOneReading(t *testing.T) {
	// a dual-stack pod is in two slices; an update may list a pod in two IPv4 slices for a while: whatever order the
	// store lists them in, the node is the same one - its connections are not flipped between two addresses
	v6 := slice("a-v6", discoveryv1.AddressTypeIPv6, "fd00::7", true)
	v4 := slice("z-v4", discoveryv1.AddressTypeIPv4, "10.0.0.7", true)
	older := slice("b-old", discoveryv1.AddressTypeIPv4, "10.0.0.7", false)
	for _, order := range [][]any{{v6, v4, older}, {older, v4, v6}, {v4, v6, older}} {
		n := View(order, nil).Node("hugr-node-etl-abc")
		if n == nil || n.Address != "10.0.0.7" || n.Ready {
			t.Errorf("order %v: %+v", order, n)
		}
	}
}
