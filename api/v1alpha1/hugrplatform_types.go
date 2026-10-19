package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HugrPlatformSpec is the installation's: the pointers every node needs, the sizes and the plan's bounds. No field
// holds a credential (spec 001).
type HugrPlatformSpec struct {
	// Node is what every node runs.
	Node NodeSpec `json:"node"`
	// IdP is the organisation's IdP: users, nodes, the operator, tresor and kista trust it.
	IdP IdPSpec `json:"idp"`
	// Tresor is the secrets service, a product of its own, reached directly.
	Tresor TresorSpec `json:"tresor"`
	// Operator is the operator's own identity as a client of the nodes (reads only: the observe bundle).
	// +optional
	Operator OperatorSpec `json:"operator,omitempty"`
	// Repository is the extension repository the nodes trust by its discovery document.
	Repository RepositorySpec `json:"repository"`
	// Catalog names the policy catalog's reserved secret; its type is the catalog's.
	// +optional
	Catalog CatalogSpec `json:"catalog,omitempty"`
	// CABundle is the nodes' HUGR_CA_FILE. It replaces their system roots (hugr-node spec 006): it holds them, plus
	// the CA of the operator's certificate.
	// +optional
	CABundle *CABundleSpec `json:"caBundle,omitempty"`
	// Heartbeat is how often a node reports.
	// +optional
	Heartbeat HeartbeatSpec `json:"heartbeat,omitempty"`
	// Front is the clients' one address for the nodes' doors (spec 002); without it the operator builds no front.
	// +optional
	Front *FrontSpec `json:"front,omitempty"`
	// Sizes says what a group's size means in this cluster.
	// +kubebuilder:validation:MinProperties=1
	Sizes map[string]SizeSpec `json:"sizes"`
	// Plan is what the operator will run: the sizes allowed and the nodes in all.
	Plan PlanSpec `json:"plan"`
}

// NodeSpec is what every node runs.
type NodeSpec struct {
	// Image is the hugr-node image.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`
	// ImagePullPolicy of the nodes' container.
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
}

// IdPSpec is the organisation's IdP.
type IdPSpec struct {
	// Issuer is the IdP's issuer URL, the one the nodes' tokens for tresor come from.
	// +kubebuilder:validation:Pattern=`^https://`
	Issuer string `json:"issuer"`
	// FederationAudience is the audience of the ServiceAccount token a node presents as its client assertion
	// (FLOW federated). Entra's is api://AzureADTokenExchange, the default.
	// +optional
	FederationAudience string `json:"federationAudience,omitempty"`
}

// TresorSpec points at the secrets service.
type TresorSpec struct {
	// Host is tresor's host:port.
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`
	// Scope is the OAuth scope a node asks the IdP for, if the IdP needs one.
	// +optional
	Scope string `json:"scope,omitempty"`
}

// OperatorSpec is the operator's identity as a client of the nodes.
type OperatorSpec struct {
	// Identity is the operator's workload identity, federated from its ServiceAccount.
	// +optional
	Identity WorkloadIdentity `json:"identity,omitempty"`
}

// WorkloadIdentity is an identity in the organisation's IdP that a ServiceAccount is federated into.
type WorkloadIdentity struct {
	// ClientID is the identity's client id in the IdP.
	// +optional
	ClientID string `json:"clientID,omitempty"`
}

// RepositorySpec points at the extension repository.
type RepositorySpec struct {
	// Name is the repository's name on the nodes (CREATE EXTENSION REPOSITORY). Default hugr.
	// +optional
	Name string `json:"name,omitempty"`
	// Prefix is the repository's https prefix; its signing keys are read from it.
	// +kubebuilder:validation:Pattern=`^https://[^@]+$`
	Prefix string `json:"prefix"`
}

// CatalogSpec names the policy catalog.
type CatalogSpec struct {
	// Secret is the reserved secret in tresor that holds the catalog's login. Default hugr_policy_catalog.
	// +optional
	Secret string `json:"secret,omitempty"`
	// Schema is the catalog's schema. Default acl.
	// +optional
	Schema string `json:"schema,omitempty"`
}

// CABundleSpec is a CA bundle in a ConfigMap of the namespace.
type CABundleSpec struct {
	ConfigMapRef ConfigMapKeyRef `json:"configMapRef"`
}

// ConfigMapKeyRef names a key of a ConfigMap.
type ConfigMapKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// HeartbeatSpec is how often a node reports.
type HeartbeatSpec struct {
	// IntervalSeconds between two heartbeats. A node is lost after three. Default 15.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	// +optional
	IntervalSeconds int32 `json:"intervalSeconds,omitempty"`
}

// FrontSpec is the front (spec 002): a stateless proxy for the nodes' Flight SQL and quack doors.
type FrontSpec struct {
	// Replicas of the front. Default 2.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`
	// ServiceType of the front's Service: ClusterIP (the default), or LoadBalancer where the clients come from
	// outside the cluster.
	// +kubebuilder:validation:Enum=ClusterIP;LoadBalancer
	// +optional
	ServiceType corev1.ServiceType `json:"serviceType,omitempty"`
	// Certificate is the Secret (kubernetes.io/tls) of the public certificate the clients see - the administrator's,
	// or the secretName of a cert-manager Certificate. Default hugr-front.
	// +optional
	Certificate FrontCertificateSpec `json:"certificate,omitempty"`
	// CABundle verifies the nodes' doors (their group's certificate, spec 001 decision 3). Default: the platform's
	// caBundle.
	// +optional
	CABundle *CABundleSpec `json:"caBundle,omitempty"`
	// FlightPort of the Service. Default 32010, the nodes' own.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	FlightPort int32 `json:"flightPort,omitempty"`
	// QuackPort of the Service. Default 9494.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	QuackPort int32 `json:"quackPort,omitempty"`
	// NoNodeWaitSeconds is how long a new connection waits for a node of its group before it is refused. Default 5.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=300
	// +optional
	NoNodeWaitSeconds *int32 `json:"noNodeWaitSeconds,omitempty"`
	// Resources of the front's container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// FrontCertificateSpec names the public certificate's Secret.
type FrontCertificateSpec struct {
	// SecretName is hugr-front or starts with hugr-front-: the front mounts no other Secret of the namespace (the
	// operator's own, a group's door key), as a group mounts only hugr-node-... ones.
	// +kubebuilder:validation:Pattern=`^hugr-front(-[a-z0-9]([-a-z0-9]*[a-z0-9])?)?$`
	// +kubebuilder:validation:MaxLength=253
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// SizeSpec is what one size means: the container's resources, where it runs, and its spill volume.
type SizeSpec struct {
	// Resources of the node's container. The node sizes its engine from limits.memory and requests.cpu; a size
	// sets no CPU limit, which would only throttle.
	Resources corev1.ResourceRequirements `json:"resources"`
	// Scheduling places the pods: one pod per VM of a pool.
	// +optional
	Scheduling SchedulingSpec `json:"scheduling,omitempty"`
	// Temp is the spill volume.
	Temp TempSpec `json:"temp"`
}

// SchedulingSpec places a size's pods.
type SchedulingSpec struct {
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
}

// TempSpec is the spill volume: an emptyDir on the VM's local disk, of a size.
type TempSpec struct {
	// Medium is where the volume lives: local, the VM's own disk (an emptyDir).
	// +kubebuilder:validation:Enum=local
	// +optional
	Medium string `json:"medium,omitempty"`
	// Size is the volume's sizeLimit; the node keeps its spill under 90% of it.
	Size resource.Quantity `json:"size"`
}

// PlanSpec is what the operator will run.
type PlanSpec struct {
	// MaxNodes is the most nodes over every group of the namespace.
	// +kubebuilder:validation:Minimum=0
	MaxNodes int32 `json:"maxNodes"`
	// Sizes the plan allows.
	// +kubebuilder:validation:MinItems=1
	Sizes []string `json:"sizes"`
}

// HugrPlatformStatus is the installation's state.
type HugrPlatformStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// HugrPlatform is one installation of the platform, one per namespace.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=hp
type HugrPlatform struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HugrPlatformSpec   `json:"spec,omitempty"`
	Status HugrPlatformStatus `json:"status,omitempty"`
}

// HugrPlatformList is a list of installations.
//
// +kubebuilder:object:root=true
type HugrPlatformList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HugrPlatform `json:"items"`
}
