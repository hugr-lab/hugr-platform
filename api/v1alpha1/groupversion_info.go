// Package v1alpha1 holds the platform's resources (spec 001): HugrPlatform, the installation's pointers, sizes and
// plan, and HugrNodeGroup, one pool of nodes.
//
// +kubebuilder:object:generate=true
// +groupName=platform.hugr.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the resources' group and version.
	GroupVersion = schema.GroupVersion{Group: "platform.hugr.io", Version: "v1alpha1"}

	// SchemeBuilder adds the resources to a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the resources to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion, &HugrPlatform{}, &HugrPlatformList{}, &HugrNodeGroup{}, &HugrNodeGroupList{})
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
