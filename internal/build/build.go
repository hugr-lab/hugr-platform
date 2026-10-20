// Package build expands a node group into the objects the operator keeps for it (spec 001): its ServiceAccount, its
// Deployment, a headless Service, a PodDisruptionBudget and a NetworkPolicy - and the bootstrap document its pods
// get. Pure: a platform and a group in, objects out; the controller owns and applies them.
package build

import (
	"fmt"
	"slices"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

// Labels and annotations the operator writes.
const (
	LabelGroup        = "platform.hugr.io/group"
	LabelName         = "app.kubernetes.io/name"
	LabelInstance     = "app.kubernetes.io/instance"
	LabelManagedBy    = "app.kubernetes.io/managed-by"
	LabelPartOf       = "app.kubernetes.io/part-of"
	LabelComponent    = "app.kubernetes.io/component"
	AnnotationRestart = "platform.hugr.io/restartedAt"
	// AnnotationHeartbeat is the interval a pod's document carries and its Lease is judged by: in the pod template, so
	// a new interval rolls the pods - never a pod beating at one interval and judged at another
	AnnotationHeartbeat = "platform.hugr.io/heartbeat-seconds"
	AnnotationAzureID   = "azure.workload.identity/client-id"
	ManagedBy           = "hugr-platform"
	NodeName            = "hugr-node"
)

// The doors' ports, and where the pod's files are.
const (
	FlightPort       = 32010
	QuackPort        = 9494
	TokensDir        = "/var/run/secrets/hugr/tokens"
	OrchestratorFile = "orchestrator"
	IdentityFile     = "identity"
	TLSDir           = "/etc/hugr/tls"
	CADir            = "/etc/hugr/ca"
	HomeDir          = "/var/lib/hugr-node" // the image's HOME: what duckdb installs from the repository
	TempDir          = "/var/lib/hugr-node/tmp"
	OrchestratorAud  = "hugr-platform"
	HomeSize         = "2Gi" // the extensions a node installs
	TokenSeconds     = 3600
	NodeUID          = 10001
)

// Settings are the operator's own: where the nodes reach it, and the front's image (its own, spec 002 Q1).
type Settings struct {
	// OrchestratorURL is the operator's in-cluster https URL (HUGR_ORCHESTRATOR).
	OrchestratorURL string
	// FrontImage is the image the front runs (HUGR_FRONT_IMAGE): the operator's own, whose `front` binary it starts.
	FrontImage           string
	FrontImagePullPolicy corev1.PullPolicy
}

// Name is the name of every object of a group.
func Name(group *v1alpha1.HugrNodeGroup) string {
	return NodeName + "-" + group.Name
}

// SelectorLabels select a group's pods.
func SelectorLabels(group *v1alpha1.HugrNodeGroup) map[string]string {
	return map[string]string{LabelName: NodeName, LabelInstance: group.Name, LabelGroup: group.Name}
}

func labels(group *v1alpha1.HugrNodeGroup) map[string]string {
	out := SelectorLabels(group)
	out[LabelManagedBy] = ManagedBy
	out[LabelPartOf] = ManagedBy
	return out
}

func meta(group *v1alpha1.HugrNodeGroup) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: Name(group), Namespace: group.Namespace, Labels: labels(group)}
}

// ServiceAccount is the group's identity: a node is of the group whose ServiceAccount it runs as.
func ServiceAccount(group *v1alpha1.HugrNodeGroup) *corev1.ServiceAccount {
	sa := &corev1.ServiceAccount{ObjectMeta: meta(group), AutomountServiceAccountToken: ptr(false)}
	if group.Spec.Identity.ClientID != "" {
		sa.Annotations = map[string]string{AnnotationAzureID: group.Spec.Identity.ClientID}
	}
	return sa
}

// Deployment runs the group's nodes. Not a StatefulSet: a scale-down removes the pod with the lowest deletion cost
// (the fewest sessions), not the highest ordinal.
func Deployment(platform *v1alpha1.HugrPlatform, group *v1alpha1.HugrNodeGroup, settings Settings) (*appsv1.Deployment, error) {
	size, ok := platform.Spec.Sizes[group.Spec.Size]
	if !ok {
		return nil, fmt.Errorf("the size %q is not one the platform lists", group.Spec.Size)
	}
	drain := group.EffectiveDrainSeconds()
	env := []corev1.EnvVar{
		{Name: "HUGR_ORCHESTRATOR", Value: settings.OrchestratorURL},
		{Name: "HUGR_ORCHESTRATOR_TOKEN_FILE", Value: TokensDir + "/" + OrchestratorFile},
		{Name: "HUGR_IDENTITY_ASSERTION_FILE", Value: TokensDir + "/" + IdentityFile},
		{Name: "HUGR_FLIGHT", Value: "grpc+tls://0.0.0.0:" + strconv.Itoa(FlightPort)},
		{Name: "HUGR_FLIGHT_CERT", Value: TLSDir + "/tls.crt"},
		{Name: "HUGR_FLIGHT_KEY", Value: TLSDir + "/tls.key"},
	}
	ports := []corev1.ContainerPort{{Name: "flight", ContainerPort: FlightPort, Protocol: corev1.ProtocolTCP}}
	if group.Spec.Doors.Quack {
		env = append(env,
			corev1.EnvVar{Name: "HUGR_QUACK", Value: "quack:0.0.0.0:" + strconv.Itoa(QuackPort)},
			corev1.EnvVar{Name: "HUGR_QUACK_CERT", Value: TLSDir + "/tls.crt"},
			corev1.EnvVar{Name: "HUGR_QUACK_KEY", Value: TLSDir + "/tls.key"})
		ports = append(ports, corev1.ContainerPort{Name: "quack", ContainerPort: QuackPort, Protocol: corev1.ProtocolTCP})
	}
	env = append(env, corev1.EnvVar{Name: "HUGR_DRAIN_DEADLINE", Value: strconv.Itoa(int(drain))})
	if platform.Spec.CABundle != nil {
		env = append(env, corev1.EnvVar{Name: "HUGR_CA_FILE", Value: CADir + "/" + platform.Spec.CABundle.ConfigMapRef.Key})
	}
	// the container's own numbers, divisor 1: the memory limit in bytes, the CPU request in whole cores (hugr-node
	// spec 010 sizes the engine from them)
	env = append(env,
		corev1.EnvVar{Name: "HUGR_POD_MEMORY", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{
			ContainerName: NodeName, Resource: "limits.memory", Divisor: resource.MustParse("1")}}},
		corev1.EnvVar{Name: "HUGR_POD_CPU", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{
			ContainerName: NodeName, Resource: "requests.cpu", Divisor: resource.MustParse("1")}}},
		corev1.EnvVar{Name: "HUGR_TEMP_DIRECTORY", Value: TempDir},
		corev1.EnvVar{Name: "HUGR_TEMP_SIZE", Value: strconv.FormatInt(size.Temp.Size.Value(), 10)})

	federation := platform.Spec.IdP.FederationAudience
	if federation == "" {
		federation = v1alpha1.DefaultFederationAudience
	}
	tempSize := size.Temp.Size.DeepCopy()
	doors, err := DoorsCertificate(group)
	if err != nil {
		return nil, err
	}
	volumes := []corev1.Volume{
		{Name: "tokens", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
					Audience: OrchestratorAud, ExpirationSeconds: ptr(int64(TokenSeconds)), Path: OrchestratorFile}},
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
					Audience: federation, ExpirationSeconds: ptr(int64(TokenSeconds)), Path: IdentityFile}},
			}}}},
		{Name: "tls", VolumeSource: doors},
		{Name: "home", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr(resource.MustParse(HomeSize))}}},
		{Name: "temp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &tempSize}}},
	}
	mounts := []corev1.VolumeMount{
		{Name: "tokens", MountPath: TokensDir, ReadOnly: true},
		{Name: "tls", MountPath: TLSDir, ReadOnly: true},
		{Name: "home", MountPath: HomeDir},
		{Name: "temp", MountPath: TempDir},
	}
	if platform.Spec.CABundle != nil {
		ref := platform.Spec.CABundle.ConfigMapRef
		volumes = append(volumes, corev1.Volume{Name: "ca", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: ref.Name},
			Items:                []corev1.KeyToPath{{Key: ref.Key, Path: ref.Key}}}}})
		mounts = append(mounts, corev1.VolumeMount{Name: "ca", MountPath: CADir, ReadOnly: true})
	}

	podLabels := labels(group)
	annotations := map[string]string{AnnotationHeartbeat: strconv.Itoa(int(platform.EffectiveHeartbeatSeconds()))}
	if group.Spec.RestartedAt != "" {
		annotations[AnnotationRestart] = group.Spec.RestartedAt
	}
	one := intstr.FromInt32(1)
	zero := intstr.FromInt32(0)
	deployment := &appsv1.Deployment{
		ObjectMeta: meta(group),
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(group.Spec.Replicas),
			Selector: &metav1.LabelSelector{MatchLabels: SelectorLabels(group)},
			// new nodes up before old ones drain
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &one, MaxUnavailable: &zero}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels, Annotations: annotations},
				Spec: corev1.PodSpec{
					ServiceAccountName:            Name(group),
					AutomountServiceAccountToken:  ptr(false),
					TerminationGracePeriodSeconds: ptr(int64(drain) + 30),
					NodeSelector:                  size.Scheduling.NodeSelector,
					Tolerations:                   size.Scheduling.Tolerations,
					Affinity:                      size.Scheduling.Affinity,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr(true), RunAsUser: ptr(int64(NodeUID)), RunAsGroup: ptr(int64(NodeUID)),
						FSGroup: ptr(int64(NodeUID)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					Containers: []corev1.Container{{
						Name:            NodeName,
						Image:           platform.Spec.Node.Image,
						ImagePullPolicy: platform.Spec.Node.ImagePullPolicy,
						Env:             env,
						Ports:           ports,
						Resources:       size.Resources,
						VolumeMounts:    mounts,
						// the process serves, nothing more: the front seats by the Lease's summary, never by readiness
						ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
							TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("flight")}},
							PeriodSeconds: 5, FailureThreshold: 3},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
					}},
					Volumes: volumes,
				},
			},
		},
	}
	return deployment, nil
}

// CSIDriver is cert-manager's csi-driver, which issues a doors' certificate into each pod (spec 005).
const CSIDriver = "csi.cert-manager.io"

// DoorsCertificate is the volume of the doors' pair, as the group names it: its Secret, or a certificate the
// csi-driver issues into the pod for the group's Service - the key made on the pod's node, held in memory, never
// written to a Secret (spec 005). The names are the operator's, never the group's editor's.
func DoorsCertificate(group *v1alpha1.HugrNodeGroup) (corev1.VolumeSource, error) {
	tls := group.Spec.TLS
	switch {
	case tls.SecretRef != nil && tls.CSI == nil:
		return corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: tls.SecretRef.Name, DefaultMode: ptr(int32(0o440))}}, nil
	case tls.CSI != nil && tls.SecretRef == nil:
		service := Name(group) + "." + group.Namespace + ".svc"
		attributes := map[string]string{
			CSIDriver + "/issuer-name": tls.CSI.IssuerRef.Name,
			CSIDriver + "/dns-names":   service + "," + service + ".cluster.local",
			// the files are the node's group's: the pod's fsGroup reaches them too (csi-driver v0.16, measured), the
			// attribute says so whatever a driver's version does by default
			CSIDriver + "/fs-group": strconv.Itoa(NodeUID),
		}
		if tls.CSI.IssuerRef.Kind != "" {
			attributes[CSIDriver+"/issuer-kind"] = tls.CSI.IssuerRef.Kind
		}
		if tls.CSI.IssuerRef.Group != "" {
			attributes[CSIDriver+"/issuer-group"] = tls.CSI.IssuerRef.Group
		}
		if tls.CSI.Duration != "" {
			attributes[CSIDriver+"/duration"] = tls.CSI.Duration
		}
		return corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
			Driver: CSIDriver, ReadOnly: ptr(true), VolumeAttributes: attributes}}, nil
	}
	return corev1.VolumeSource{}, fmt.Errorf("the group %q names exactly one doors' certificate: tls.secretRef or tls.csi",
		group.Name)
}

// Service is the group's headless Service: its EndpointSlice is the front's node list (spec 002).
func Service(group *v1alpha1.HugrNodeGroup) *corev1.Service {
	ports := []corev1.ServicePort{{Name: "flight", Port: FlightPort, TargetPort: intstr.FromString("flight"), Protocol: corev1.ProtocolTCP}}
	if group.Spec.Doors.Quack {
		ports = append(ports, corev1.ServicePort{Name: "quack", Port: QuackPort, TargetPort: intstr.FromString("quack"), Protocol: corev1.ProtocolTCP})
	}
	return &corev1.Service{ObjectMeta: meta(group), Spec: corev1.ServiceSpec{
		ClusterIP: corev1.ClusterIPNone, Selector: SelectorLabels(group), Ports: ports}}
}

// PodDisruptionBudget lets one node go at a time.
func PodDisruptionBudget(group *v1alpha1.HugrNodeGroup) *policyv1.PodDisruptionBudget {
	one := intstr.FromInt32(1)
	return &policyv1.PodDisruptionBudget{ObjectMeta: meta(group), Spec: policyv1.PodDisruptionBudgetSpec{
		MaxUnavailable: &one, Selector: &metav1.LabelSelector{MatchLabels: SelectorLabels(group)}}}
}

// NetworkPolicy opens the doors to the front and the operator only; egress stays open (the sources, the repository,
// tresor and the IdP).
func NetworkPolicy(group *v1alpha1.HugrNodeGroup) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	ports := []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: ptr(intstr.FromInt32(FlightPort))}}
	if group.Spec.Doors.Quack {
		ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: ptr(intstr.FromInt32(QuackPort))})
	}
	peers := []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{
		MatchLabels: map[string]string{LabelPartOf: ManagedBy},
		MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key: LabelComponent, Operator: metav1.LabelSelectorOpIn, Values: []string{"operator", "front"}}},
	}}}
	return &networkingv1.NetworkPolicy{ObjectMeta: meta(group), Spec: networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: SelectorLabels(group)},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: peers, Ports: ports}},
	}}
}

// HeartbeatSeconds of a pod: the interval its template was built with, else the platform's.
func HeartbeatSeconds(platform *v1alpha1.HugrPlatform, annotations map[string]string) int32 {
	if v, err := strconv.Atoi(annotations[AnnotationHeartbeat]); err == nil && v >= 1 && v <= 3600 {
		return int32(v)
	}
	return platform.EffectiveHeartbeatSeconds()
}

// Document is the bootstrap document a pod of the group gets: pointers and names only, its interval the pod's own.
func Document(platform *v1alpha1.HugrPlatform, group *v1alpha1.HugrNodeGroup, pod string, interval int32) nodeproto.Document {
	repository := platform.Spec.Repository.Name
	if repository == "" {
		repository = v1alpha1.DefaultRepositoryName
	}
	secret := platform.Spec.Catalog.Secret
	if secret == "" {
		secret = v1alpha1.DefaultCatalogSecret
	}
	schema := platform.Spec.Catalog.Schema
	if schema == "" {
		schema = v1alpha1.DefaultCatalogSchema
	}
	return nodeproto.Document{
		Version:   nodeproto.DocumentVersion,
		NodeGroup: group.EffectiveResourceGroup(),
		NodeLabel: pod,
		Tresor: nodeproto.Tresor{Host: platform.Spec.Tresor.Host, Identity: nodeproto.Identity{
			Issuer: platform.Spec.IdP.Issuer, ClientID: group.Spec.Identity.ClientID, Scope: platform.Spec.Tresor.Scope}},
		Repository: nodeproto.Repository{Name: repository, Prefix: platform.Spec.Repository.Prefix},
		Catalog:    nodeproto.Catalog{Secret: secret, Schema: schema},
		Heartbeat:  nodeproto.Heartbeat{IntervalSeconds: int(interval)},
	}
}

// ConsoleClient is the first policy's client for the console (hugr-node spec 015).
const ConsoleClient = "console"

// FirstPolicy is the first policy a node of the platform gets in its document (spec 005, hugr-node spec 015), or nil
// when the platform names none. The console's client is always one, its audience always counts, and the resource
// groups are the platform's groups', one of them the default when the platform says which. An error is a platform
// the node could not write the policy of.
func FirstPolicy(platform *v1alpha1.HugrPlatform, groups []v1alpha1.HugrNodeGroup) (*nodeproto.FirstPolicy, error) {
	spec := platform.Spec.FirstPolicy
	if spec == nil {
		return nil, nil
	}
	console := platform.Spec.Console
	if console == nil {
		return nil, fmt.Errorf("firstPolicy needs console: the console's client is how the administrator gets in")
	}
	url := spec.Issuer.URL
	if url == "" {
		url = platform.Spec.IdP.Issuer
	}
	audiences := []string{console.Audiences.Nodes}
	for _, audience := range spec.Issuer.Audiences {
		if !slices.Contains(audiences, audience) {
			audiences = append(audiences, audience)
		}
	}
	clients := []nodeproto.FirstPolicyClient{{Name: ConsoleClient, ClientID: console.ClientID}}
	for _, client := range spec.Clients {
		clients = append(clients, nodeproto.FirstPolicyClient{Name: client.Name, ClientID: client.ClientID})
	}
	var names []string
	for i := range groups {
		if groups[i].Spec.Platform != platform.Name {
			continue
		}
		if name := groups[i].EffectiveResourceGroup(); !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	if spec.DefaultGroup != "" && !slices.Contains(names, spec.DefaultGroup) {
		return nil, fmt.Errorf("firstPolicy's defaultGroup %q is no group's resource group", spec.DefaultGroup)
	}
	resourceGroups := make([]nodeproto.FirstPolicyGroup, 0, len(names))
	for _, name := range names {
		resourceGroups = append(resourceGroups, nodeproto.FirstPolicyGroup{Name: name, Default: name == spec.DefaultGroup})
	}
	return &nodeproto.FirstPolicy{
		Issuer: nodeproto.FirstPolicyIssuer{Name: spec.Issuer.Name, URL: url, Audiences: audiences,
			RolesClaim: spec.Issuer.RolesClaim},
		Clients: clients, Administrators: spec.Administrators, ResourceGroups: resourceGroups,
	}, nil
}

func ptr[T any](v T) *T {
	return &v
}

// CheckSize says whether a size gives the node what it sizes its engine from: a memory limit (the line past which
// the pod is OOM-killed) and a CPU request.
func CheckSize(platform *v1alpha1.HugrPlatform, name string) error {
	size, ok := platform.Spec.Sizes[name]
	if !ok {
		return fmt.Errorf("the size %q is not one the platform lists", name)
	}
	if _, ok := size.Resources.Limits[corev1.ResourceMemory]; !ok {
		return fmt.Errorf("the size %q sets no memory limit - the node sizes its engine from it", name)
	}
	if _, ok := size.Resources.Requests[corev1.ResourceCPU]; !ok {
		return fmt.Errorf("the size %q requests no CPU - the node sizes its threads from it", name)
	}
	if size.Temp.Size.IsZero() {
		return fmt.Errorf("the size %q has no spill volume", name)
	}
	return nil
}
