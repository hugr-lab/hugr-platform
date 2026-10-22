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
	// TLS is the doors' certificate, one per group, for the name hugr-node-<group>.<namespace>.svc: a
	// kubernetes.io/tls Secret, or one cert-manager's csi-driver issues into each pod (spec 005).
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

// TLSSpec names the doors' certificate: exactly one of a Secret and the csi-driver.
//
// +kubebuilder:validation:XValidation:rule="has(self.secretRef) != has(self.csi)",message="tls names exactly one of secretRef and csi"
type TLSSpec struct {
	// SecretRef is a kubernetes.io/tls Secret of the namespace, named hugr-node-…: its key is kept in etcd.
	// +optional
	SecretRef *SecretRef `json:"secretRef,omitempty"`
	// CSI has cert-manager's csi-driver issue each pod its own certificate: the key is made on the pod's node, held
	// in memory and never written to a Secret (spec 005, the owner's decision 6).
	// +optional
	CSI *CSICertificate `json:"csi,omitempty"`
}

// CSICertificate is what the csi-driver asks its issuer for; the names are the operator's (the group's Service).
type CSICertificate struct {
	IssuerRef IssuerRef `json:"issuerRef"`
	// Duration of a certificate, a Go duration ("720h"). Default: the csi-driver's (720h); it renews at two
	// thirds.
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(h|m|s))+$`
	// +optional
	Duration string `json:"duration,omitempty"`
}

// IssuerRef names a cert-manager issuer.
type IssuerRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Kind is Issuer (the namespace's, the default), ClusterIssuer, or an external issuer's kind.
	// +optional
	Kind string `json:"kind,omitempty"`
	// Group is the issuer's API group: cert-manager.io, the default, or an external issuer's.
	// +optional
	Group string `json:"group,omitempty"`
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

// BuildCount is how many of a group's live nodes report one build.
type BuildCount struct {
	// Build is the hugr_node build the nodes report.
	Build string `json:"build"`
	// DuckDB is the DuckDB release they run.
	// +optional
	DuckDB string `json:"duckdb,omitempty"`
	// Nodes reporting it.
	Nodes int32 `json:"nodes"`
}

// CatalogStamp is the policy catalog's schema version and the oldest build that may read it (duckdb-acl spec 094).
type CatalogStamp struct {
	Schema    int64 `json:"schema"`
	MinReader int64 `json:"minReader"`
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
	// Builds counts the live nodes per build they report (spec 008): during an upgrade, two.
	// +optional
	Builds []BuildCount `json:"builds,omitempty"`
	// Catalog is the policy catalog's schema as the live nodes report it: the newest stamp any of them read.
	// +optional
	Catalog *CatalogStamp `json:"catalog,omitempty"`
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
	DefaultFrontReplicas      = 2
	DefaultFrontSecret        = "hugr-front"
	DefaultFrontFlightPort    = 32010
	DefaultFrontQuackPort     = 9494
	DefaultFrontNoNodeWait    = 5
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

// EffectiveReplicas of the front.
func (f *FrontSpec) EffectiveReplicas() int32 {
	if f.Replicas != nil {
		return *f.Replicas
	}
	return DefaultFrontReplicas
}

// EffectiveSecret of the front's public certificate.
func (f *FrontSpec) EffectiveSecret() string {
	if f.Certificate.SecretName != "" {
		return f.Certificate.SecretName
	}
	return DefaultFrontSecret
}

// EffectivePorts of the front's Service: Flight, quack.
func (f *FrontSpec) EffectivePorts() (int32, int32) {
	flight, quack := f.FlightPort, f.QuackPort
	if flight == 0 {
		flight = DefaultFrontFlightPort
	}
	if quack == 0 {
		quack = DefaultFrontQuackPort
	}
	return flight, quack
}

// EffectiveNoNodeWaitSeconds of the front.
func (f *FrontSpec) EffectiveNoNodeWaitSeconds() int32 {
	if f.NoNodeWaitSeconds != nil {
		return *f.NoNodeWaitSeconds
	}
	return DefaultFrontNoNodeWait
}
