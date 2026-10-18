package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HugrNodeGroupSpec is one pool of nodes: how many, of which size. People edit it with standard tooling.
type HugrNodeGroupSpec struct {
	// Platform is the HugrPlatform of the namespace the group belongs to.
	// +kubebuilder:validation:MinLength=1
	Platform string `json:"platform"`
	// Replicas is the number of nodes.
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas"`
	// Size is one of the platform's sizes, allowed by its plan.
	// +kubebuilder:validation:MinLength=1
	Size string `json:"size"`
	// ResourceGroup is the policy's resource group the nodes serve (acl_node_group). Default: the group's name.
	// +optional
	ResourceGroup string `json:"resourceGroup,omitempty"`
	// Identity is the group's workload identity in the IdP, federated from the group's ServiceAccount: the
	// nodes' login to tresor.
	Identity WorkloadIdentity `json:"identity"`
	// DrainSeconds a stopping node waits for its sessions (HUGR_DRAIN_DEADLINE); the pod's grace is 30 s more.
	// Default 300.
	// +kubebuilder:validation:Minimum=0
	// +optional
	DrainSeconds *int32 `json:"drainSeconds,omitempty"`
	// Doors the nodes open. Flight is always open.
	// +optional
	Doors DoorsSpec `json:"doors,omitempty"`
	// TLS is the doors' certificate, one per group: a kubernetes.io/tls Secret whose SAN is
	// hugr-node-<group>.<namespace>.svc.
	TLS TLSSpec `json:"tls"`
	// RestartedAt rolls the group when it changes: the console's restart button, or a kubectl patch.
	// +optional
	RestartedAt string `json:"restartedAt,omitempty"`
}

// DoorsSpec says which doors the nodes open.
type DoorsSpec struct {
	// Quack opens the quack door beside Flight.
	// +optional
	Quack bool `json:"quack,omitempty"`
}

// TLSSpec names the doors' certificate.
type TLSSpec struct {
	SecretRef SecretRef `json:"secretRef"`
}

// SecretRef names a Secret of the namespace.
type SecretRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// ProfileStatus is the group's convergence on the cluster profile.
type ProfileStatus struct {
	// Target is the highest profile version a live node reports.
	// +optional
	Target int64 `json:"target,omitempty"`
	// Applied is how many live nodes have applied the target.
	// +optional
	Applied int32 `json:"applied,omitempty"`
	// NeedsRestart lists the nodes that report an item they can apply only at a restart or a drain.
	// +optional
	NeedsRestart []string `json:"needsRestart,omitempty"`
}

// HugrNodeGroupStatus is the group's state, aggregated from its Deployment and its nodes' Leases.
type HugrNodeGroupStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// +optional
	Profile ProfileStatus `json:"profile,omitempty"`
	// Lost lists the nodes whose Lease expired.
	// +optional
	Lost []string `json:"lost,omitempty"`
	// ResourceGroupKnown says whether the nodes find their resource group in the policy (nil: no node reported).
	// +optional
	ResourceGroupKnown *bool `json:"resourceGroupKnown,omitempty"`
	// Conditions: Ready, Refused (past the plan), Draining (the resource group is gone).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// HugrNodeGroup is one pool of nodes.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=hng
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Size",type=string,JSONPath=`.spec.size`
// +kubebuilder:printcolumn:name="Target",type=integer,JSONPath=`.status.profile.target`
// +kubebuilder:printcolumn:name="Applied",type=integer,JSONPath=`.status.profile.applied`
type HugrNodeGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HugrNodeGroupSpec   `json:"spec,omitempty"`
	Status HugrNodeGroupStatus `json:"status,omitempty"`
}

// HugrNodeGroupList is a list of groups.
//
// +kubebuilder:object:root=true
type HugrNodeGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HugrNodeGroup `json:"items"`
}

// The group's defaults, in one place.
const (
	DefaultDrainSeconds       = 300
	DefaultHeartbeatSeconds   = 15
	DefaultRepositoryName     = "hugr"
	DefaultCatalogSecret      = "hugr_policy_catalog"
	DefaultCatalogSchema      = "acl"
	DefaultFederationAudience = "api://AzureADTokenExchange"
)

// EffectiveResourceGroup is the policy's resource group the group's nodes serve.
func (g *HugrNodeGroup) EffectiveResourceGroup() string {
	if g.Spec.ResourceGroup != "" {
		return g.Spec.ResourceGroup
	}
	return g.Name
}

// EffectiveDrainSeconds is how long a stopping node waits for its sessions.
func (g *HugrNodeGroup) EffectiveDrainSeconds() int32 {
	if g.Spec.DrainSeconds != nil {
		return *g.Spec.DrainSeconds
	}
	return DefaultDrainSeconds
}

// EffectiveHeartbeatSeconds is the nodes' heartbeat interval.
func (p *HugrPlatform) EffectiveHeartbeatSeconds() int32 {
	if p.Spec.Heartbeat.IntervalSeconds > 0 {
		return p.Spec.Heartbeat.IntervalSeconds
	}
	return DefaultHeartbeatSeconds
}
