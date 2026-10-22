package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HugrPlatformSpec is the installation's: the pointers every node needs, the sizes and the plan's bounds. No field
// holds a credential (spec 001).
// +kubebuilder:validation:XValidation:rule="!has(self.firstPolicy) || has(self.console)",message="firstPolicy needs console: the console's client is how the administrator gets in"
type HugrPlatformSpec struct {
	// Node is what every node runs.
	Node NodeSpec `json:"node"`
	// Upgrade is the administrator's say over a rollout of a new node image (spec 008).
	// +optional
	Upgrade UpgradeSpec `json:"upgrade,omitempty"`
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
	// Console is the web console's sign-in (spec 003); without it the operator serves no console.
	// +optional
	Console *ConsoleSpec `json:"console,omitempty"`
	// FirstPolicy is what the first node writes into a policy catalog nobody administers yet (spec 005, hugr-node
	// spec 015): the IdP, the applications whose tokens count, the administrators, the default group. Names only,
	// never SQL nor a secret; a catalog that has its policy is never written again, whatever this says later.
	// +optional
	FirstPolicy *FirstPolicySpec `json:"firstPolicy,omitempty"`
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
	// DuckDBVersion is the DuckDB release the image runs (the release names it): an upgrade checks the repository's
	// channel serves the catalog's extension for it before any node moves, and every new node must report it (spec
	// 008). Empty: neither is checked.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._+-]{1,64}$`
	// +optional
	DuckDBVersion string `json:"duckdbVersion,omitempty"`
	// CatalogSchema is the policy catalog's schema version the image's build reads and writes, and
	// CatalogMinReader the oldest build that reads what it writes (duckdb-acl spec 094; the release names both). An
	// upgrade to a newer schema has its canary migrate the catalog; 0: the catalog is not judged.
	// +kubebuilder:validation:Minimum=0
	// +optional
	CatalogSchema int64 `json:"catalogSchema,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	CatalogMinReader int64 `json:"catalogMinReader,omitempty"`
}

// UpgradeSpec is the administrator's say over a rollout (spec 008).
type UpgradeSpec struct {
	// Paused holds the rollout where it is; set back to false, a blocked rollout tries its step again.
	// +optional
	Paused bool `json:"paused,omitempty"`
	// AllowCatalogDowntime lets the canary take a strict catalog step: one past the old nodes' window, after which
	// they stop serving at their next check until they move too.
	// +optional
	AllowCatalogDowntime bool `json:"allowCatalogDowntime,omitempty"`
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

// ConsoleSpec is the web console's sign-in (spec 003). The browser signs the person in at spec.idp.issuer with PKCE;
// nothing here is a secret - the client is public.
type ConsoleSpec struct {
	// ClientID is the console's public client at the IdP.
	// +kubebuilder:validation:MinLength=1
	ClientID string `json:"clientID"`
	// Audiences are what each token the browser asks for is meant for.
	Audiences ConsoleAudiences `json:"audiences"`
	// Scopes asked at sign-in besides openid and profile.
	// +optional
	Scopes []string `json:"scopes,omitempty"`
	// Links are the backends the console links out to - telemetry and lineage (spec 003 round 2); the console
	// queries none of them.
	// +optional
	// +kubebuilder:validation:MaxItems=8
	Links []ConsoleLink `json:"links,omitempty"`
}

// ConsoleAudiences names a token's audience per service.
type ConsoleAudiences struct {
	// Nodes is the audience of the policy's client for the console: its API and every node-side action.
	// +kubebuilder:validation:MinLength=1
	Nodes string `json:"nodes"`
	// Tresor is tresor-server's: its mounted console, and the SQL page's way to the doors (spec 009) - a platform
	// always has tresor.
	// +kubebuilder:validation:MinLength=1
	Tresor string `json:"tresor"`
	// Kista is kista's, for its mounted console.
	// +optional
	Kista string `json:"kista,omitempty"`
}

// ConsoleLink is a backend the console links out to.
type ConsoleLink struct {
	// Kind of backend.
	// +kubebuilder:validation:Enum=telemetry;lineage
	Kind string `json:"kind"`
	// Name shown as "Open in <name>".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Name string `json:"name"`
	// URL template: https, no userinfo; {node}, {group}, {session} and {trace} are replaced, URL-escaped.
	// +kubebuilder:validation:Pattern=`^https://[^@/\s]+(/[^\s]*)?$`
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url"`
}

// FirstPolicySpec is the platform's first policy. The console's client (console.clientID, its tokens for
// console.audiences.nodes) is always one of its clients, and its resource groups are the namespace's groups'.
type FirstPolicySpec struct {
	// Issuer is the IdP the people's tokens come from.
	Issuer FirstPolicyIssuer `json:"issuer"`
	// Clients are applications besides the console whose tokens count: a tool's registration (DBeaver, a driver).
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Clients []FirstPolicyClient `json:"clients,omitempty"`
	// Administrators is the roles claim's value that makes a person the platform's administrator.
	// +kubebuilder:validation:MinLength=1
	Administrators string `json:"administrators"`
	// DefaultGroup is the resource group of a principal bound to none: one of the namespace's groups' resource
	// groups. Without it, a principal is served only by the groups the administrator binds it to.
	// +optional
	DefaultGroup string `json:"defaultGroup,omitempty"`
}

// FirstPolicyIssuer is the first policy's IdP.
type FirstPolicyIssuer struct {
	// Name of the issuer in the policy.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_-]{1,63}$`
	Name string `json:"name"`
	// URL is the issuer of the people's tokens (their iss); its keys are read from it by discovery. Default:
	// spec.idp.issuer.
	// +kubebuilder:validation:Pattern=`^https://[^@]+$`
	// +optional
	URL string `json:"url,omitempty"`
	// Audiences the people's tokens may carry besides the console's (console.audiences.nodes).
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MinLength=1
	// +optional
	Audiences []string `json:"audiences,omitempty"`
	// RolesClaim is the claim that carries a person's roles: roles for Entra ID, realm_access.roles for Keycloak.
	// +kubebuilder:validation:MinLength=1
	RolesClaim string `json:"rolesClaim"`
}

// FirstPolicyClient is an application whose tokens count.
// +kubebuilder:validation:XValidation:rule="self.name != 'console'",message="console is the console's own client, made from console.clientID"
type FirstPolicyClient struct {
	// Name of the client in the policy.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_-]{1,63}$`
	Name string `json:"name"`
	// ClientID is the application's id at the IdP: its tokens' azp.
	// +kubebuilder:validation:MinLength=1
	ClientID string `json:"clientID"`
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
	// Image is the node image every group runs when no rollout is under way (spec 008).
	// +optional
	Image string `json:"image,omitempty"`
	// Upgrade is the latest rollout of a node image: where it stands, or how it ended.
	// +optional
	Upgrade *UpgradeStatus `json:"upgrade,omitempty"`
}

// UpgradeStatus is a rollout of a node image across the groups (spec 008): the preconditions, a canary that migrates
// the catalog when the image's build needs it, then the groups one at a time.
type UpgradeStatus struct {
	// From is the image the groups ran when the rollout began; To is the one it moves them to.
	From string `json:"from"`
	To   string `json:"to"`
	// Phase: Preconditions, Canary, Groups, Complete, Blocked (stopped on a failure, until the administrator pauses
	// and resumes it, or sets another image) or Paused (spec.upgrade.paused).
	Phase string `json:"phase"`
	// Group rolls now: the canary, then each group in turn.
	// +optional
	Group string `json:"group,omitempty"`
	// Moved lists the groups that run To.
	// +optional
	Moved []string `json:"moved,omitempty"`
	// Migrate: the canary's new nodes migrate the catalog (their document's catalog.migrate).
	// +optional
	Migrate bool `json:"migrate,omitempty"`
	// Resume is the phase a Blocked or Paused rollout goes back to.
	// +optional
	Resume string `json:"resume,omitempty"`
	// Reason the rollout is Blocked or Paused.
	// +optional
	Reason    string      `json:"reason,omitempty"`
	StartedAt metav1.Time `json:"startedAt"`
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
