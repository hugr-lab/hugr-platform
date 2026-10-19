package build

import (
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
)

// The front's objects (spec 002) share one name.
const (
	FrontName       = "hugr-front"
	FrontTLSDir     = "/etc/hugr-front/tls"
	FrontCADir      = "/etc/hugr-front/ca"
	FrontCAFileName = "ca.crt"
	FrontInternal   = 8081
	FrontDrain      = 30 // the streams in flight after SIGTERM
	FrontUnready    = 5  // not ready before it stops accepting
	FrontUID        = 65532
	FrontComponent  = "front"
	FrontBinary     = "/front"
)

// FrontSelectorLabels select the front's pods.
func FrontSelectorLabels(platform *v1alpha1.HugrPlatform) map[string]string {
	return map[string]string{LabelName: FrontName, LabelInstance: platform.Name}
}

func frontMeta(platform *v1alpha1.HugrPlatform) metav1.ObjectMeta {
	l := FrontSelectorLabels(platform)
	l[LabelManagedBy], l[LabelPartOf], l[LabelComponent] = ManagedBy, ManagedBy, FrontComponent
	return metav1.ObjectMeta{Name: FrontName, Namespace: platform.Namespace, Labels: l}
}

// FrontServiceAccount of the front's pods.
func FrontServiceAccount(platform *v1alpha1.HugrPlatform) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: frontMeta(platform), AutomountServiceAccountToken: ptr(true)}
}

// FrontRole reads the fleet and nothing else: the groups' EndpointSlices and the nodes' Leases of the namespace.
func FrontRole(platform *v1alpha1.HugrPlatform) *rbacv1.Role {
	read := []string{"get", "list", "watch"}
	return &rbacv1.Role{ObjectMeta: frontMeta(platform), Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, Verbs: read},
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: read},
	}}
}

// FrontRoleBinding gives the front's ServiceAccount its Role.
func FrontRoleBinding(platform *v1alpha1.HugrPlatform) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{ObjectMeta: frontMeta(platform),
		RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: FrontName},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: FrontName, Namespace: platform.Namespace}}}
}

// FrontCABundle is the bundle the front verifies the nodes by: its own, else the platform's.
func FrontCABundle(platform *v1alpha1.HugrPlatform) (*v1alpha1.CABundleSpec, error) {
	if platform.Spec.Front.CABundle != nil {
		return platform.Spec.Front.CABundle, nil
	}
	if platform.Spec.CABundle != nil {
		return platform.Spec.CABundle, nil
	}
	return nil, fmt.Errorf("the front verifies the nodes by a CA bundle: set spec.front.caBundle or spec.caBundle")
}

// FrontDeployment of the front: the operator's image, its `front` binary.
func FrontDeployment(platform *v1alpha1.HugrPlatform, settings Settings) (*appsv1.Deployment, error) {
	front := platform.Spec.Front
	if settings.FrontImage == "" {
		return nil, fmt.Errorf("the operator has no image for the front (HUGR_FRONT_IMAGE)")
	}
	ca, err := FrontCABundle(platform)
	if err != nil {
		return nil, err
	}
	// in the pod the doors listen on the nodes' own ports; the Service maps spec.front's onto them
	flight, quack := int32(FlightPort), int32(QuackPort)
	args := []string{
		"--cert-dir=" + FrontTLSDir,
		"--ca-file=" + FrontCADir + "/" + FrontCAFileName,
		"--flight-address=:" + strconv.Itoa(int(flight)),
		"--quack-address=:" + strconv.Itoa(int(quack)),
		"--internal-address=:" + strconv.Itoa(FrontInternal),
		"--heartbeat=" + strconv.Itoa(int(platform.EffectiveHeartbeatSeconds())) + "s",
		"--no-node-wait=" + strconv.Itoa(int(front.EffectiveNoNodeWaitSeconds())) + "s",
	}
	probe := func(path string) *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path,
			Port: intstr.FromString("internal")}}, PeriodSeconds: 5}
	}
	container := corev1.Container{
		Name: FrontComponent, Image: settings.FrontImage, ImagePullPolicy: settings.FrontImagePullPolicy,
		Command: []string{FrontBinary}, Args: args,
		Env: []corev1.EnvVar{{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}}},
		Ports: []corev1.ContainerPort{
			{Name: "flight", ContainerPort: flight, Protocol: corev1.ProtocolTCP},
			{Name: "quack", ContainerPort: quack, Protocol: corev1.ProtocolTCP},
			{Name: "internal", ContainerPort: FrontInternal, Protocol: corev1.ProtocolTCP},
		},
		Resources:      front.Resources,
		ReadinessProbe: probe("/readyz"),
		LivenessProbe:  probe("/healthz"),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "tls", MountPath: FrontTLSDir, ReadOnly: true},
			{Name: "ca", MountPath: FrontCADir, ReadOnly: true},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(true), RunAsNonRoot: ptr(true),
			Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
	replicas := front.EffectiveReplicas()
	meta := frontMeta(platform)
	return &appsv1.Deployment{ObjectMeta: meta, Spec: appsv1.DeploymentSpec{
		Replicas: &replicas,
		Selector: &metav1.LabelSelector{MatchLabels: FrontSelectorLabels(platform)},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: meta.Labels},
			Spec: corev1.PodSpec{
				ServiceAccountName:            FrontName,
				TerminationGracePeriodSeconds: ptr(int64(FrontDrain + FrontUnready + 5)),
				SecurityContext: &corev1.PodSecurityContext{RunAsUser: ptr(int64(FrontUID)), RunAsNonRoot: ptr(true),
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
				Containers: []corev1.Container{container},
				Volumes: []corev1.Volume{
					{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
						SecretName: front.EffectiveSecret()}}},
					{Name: "ca", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: ca.ConfigMapRef.Name},
						Items:                []corev1.KeyToPath{{Key: ca.ConfigMapRef.Key, Path: FrontCAFileName}}}}},
				},
			},
		},
	}}, nil
}

// FrontService of the clients: the two doors, of the platform's service type. The internal port is not in it.
func FrontService(platform *v1alpha1.HugrPlatform) *corev1.Service {
	flight, quack := platform.Spec.Front.EffectivePorts()
	serviceType := platform.Spec.Front.ServiceType
	if serviceType == "" {
		serviceType = corev1.ServiceTypeClusterIP
	}
	return &corev1.Service{ObjectMeta: frontMeta(platform), Spec: corev1.ServiceSpec{
		Type: serviceType, Selector: FrontSelectorLabels(platform),
		Ports: []corev1.ServicePort{
			{Name: "flight", Port: flight, TargetPort: intstr.FromString("flight"), Protocol: corev1.ProtocolTCP},
			{Name: "quack", Port: quack, TargetPort: intstr.FromString("quack"), Protocol: corev1.ProtocolTCP},
		}}}
}

// FrontPodDisruptionBudget keeps one replica up.
func FrontPodDisruptionBudget(platform *v1alpha1.HugrPlatform) *policyv1.PodDisruptionBudget {
	one := intstr.FromInt32(1)
	return &policyv1.PodDisruptionBudget{ObjectMeta: frontMeta(platform), Spec: policyv1.PodDisruptionBudgetSpec{
		MinAvailable: &one, Selector: &metav1.LabelSelector{MatchLabels: FrontSelectorLabels(platform)}}}
}
