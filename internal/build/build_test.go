package build

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// Fixture is the platform and group of the tests: a large size with a spill volume, a CA bundle, the quack door.
func Fixture() (*v1alpha1.HugrPlatform, *v1alpha1.HugrNodeGroup) {
	platform := &v1alpha1.HugrPlatform{
		ObjectMeta: metav1.ObjectMeta{Name: "hugr", Namespace: "hugr"},
		Spec: v1alpha1.HugrPlatformSpec{
			Node:       v1alpha1.NodeSpec{Image: "ghcr.io/hugr-lab/hugr-node:v0.1.0"},
			IdP:        v1alpha1.IdPSpec{Issuer: "https://login.example/t/v2.0"},
			Tresor:     v1alpha1.TresorSpec{Host: "tresor.example:443", Scope: "api://tresor/.default"},
			Repository: v1alpha1.RepositorySpec{Prefix: "https://kista.example/t/prod"},
			CABundle:   &v1alpha1.CABundleSpec{ConfigMapRef: v1alpha1.ConfigMapKeyRef{Name: "hugr-ca", Key: "ca.crt"}},
			Sizes: map[string]v1alpha1.SizeSpec{"large": {
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("16"), corev1.ResourceMemory: resource.MustParse("128Gi")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Gi")}},
				Scheduling: v1alpha1.SchedulingSpec{NodeSelector: map[string]string{"agentpool": "e16ds"}},
				Temp:       v1alpha1.TempSpec{Medium: "local", Size: resource.MustParse("600Gi")},
			}},
			Plan: v1alpha1.PlanSpec{MaxNodes: 12, Sizes: []string{"large"}},
		},
	}
	group := &v1alpha1.HugrNodeGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "etl", Namespace: "hugr"},
		Spec: v1alpha1.HugrNodeGroupSpec{
			Platform: "hugr", Replicas: 3, Size: "large",
			Identity:    v1alpha1.WorkloadIdentity{ClientID: "etl-app"},
			Doors:       v1alpha1.DoorsSpec{Quack: true},
			TLS:         v1alpha1.TLSSpec{SecretRef: &v1alpha1.SecretRef{Name: "hugr-node-etl-tls"}},
			RestartedAt: "2026-10-18T12:00:00Z",
		},
	}
	return platform, group
}

func golden(t *testing.T, name string, obj interface{}) {
	t.Helper()
	got, err := yaml.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", name+".golden.yaml")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/build -update)", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs from %s:\n%s", name, path, got)
	}
}

// TestGroupObjects: a group's expansion, golden - the engine's numbers from the Downward API (divisor 1), the spill
// volume, the two projected tokens, restartedAt in the pod template, the doors' certificate.
func TestGroupObjects(t *testing.T) {
	platform, group := Fixture()
	deployment, err := Deployment(platform, group, Settings{OrchestratorURL: "https://hugr-platform.hugr.svc:8443"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "serviceaccount", ServiceAccount(group))
	golden(t, "deployment", deployment)
	golden(t, "service", Service(group))
	golden(t, "pdb", PodDisruptionBudget(group))
	golden(t, "networkpolicy", NetworkPolicy(group))
}

// TestDoorsCertificateByCSI: a group whose doors' certificate the csi-driver issues mounts no Secret; the driver is
// asked for the group's Service names (the operator's, never the editor's), the files are the node's group's, and the
// issuer, its kind and the duration are passed as written.
func TestDoorsCertificateByCSI(t *testing.T) {
	platform, group := Fixture()
	group.Spec.TLS = v1alpha1.TLSSpec{CSI: &v1alpha1.CSICertificate{
		IssuerRef: v1alpha1.IssuerRef{Name: "hugr-internal", Kind: "ClusterIssuer"}, Duration: "720h"}}
	deployment, err := Deployment(platform, group, Settings{})
	if err != nil {
		t.Fatal(err)
	}
	var tls *corev1.Volume
	for i, v := range deployment.Spec.Template.Spec.Volumes {
		if v.Secret != nil {
			t.Errorf("the pod mounts the Secret %q", v.Secret.SecretName)
		}
		if v.Name == "tls" {
			tls = &deployment.Spec.Template.Spec.Volumes[i]
		}
	}
	if tls == nil || tls.CSI == nil {
		t.Fatalf("no csi volume for the doors: %+v", tls)
	}
	if tls.CSI.Driver != CSIDriver || tls.CSI.ReadOnly == nil || !*tls.CSI.ReadOnly {
		t.Errorf("driver %q, read-only %v", tls.CSI.Driver, tls.CSI.ReadOnly)
	}
	want := map[string]string{
		"csi.cert-manager.io/issuer-name": "hugr-internal",
		"csi.cert-manager.io/issuer-kind": "ClusterIssuer",
		"csi.cert-manager.io/dns-names":   "hugr-node-etl.hugr.svc,hugr-node-etl.hugr.svc.cluster.local",
		"csi.cert-manager.io/fs-group":    "10001",
		"csi.cert-manager.io/duration":    "720h",
	}
	for k, v := range want {
		if got := tls.CSI.VolumeAttributes[k]; got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if len(tls.CSI.VolumeAttributes) != len(want) {
		t.Errorf("attributes %v, want exactly %v", tls.CSI.VolumeAttributes, want)
	}
	// neither form, or both: no Deployment
	group.Spec.TLS = v1alpha1.TLSSpec{}
	if _, err := Deployment(platform, group, Settings{}); err == nil {
		t.Error("a group with no doors' certificate is expanded")
	}
	group.Spec.TLS = v1alpha1.TLSSpec{SecretRef: &v1alpha1.SecretRef{Name: "hugr-node-etl-tls"},
		CSI: &v1alpha1.CSICertificate{IssuerRef: v1alpha1.IssuerRef{Name: "hugr-internal"}}}
	if _, err := Deployment(platform, group, Settings{}); err == nil {
		t.Error("a group with two doors' certificates is expanded")
	}
}

func TestDeploymentRefusesAnUnknownSize(t *testing.T) {
	platform, group := Fixture()
	group.Spec.Size = "huge"
	if _, err := Deployment(platform, group, Settings{}); err == nil {
		t.Error("a size the platform does not list is expanded")
	}
}

func TestRestartedAtRollsThePods(t *testing.T) {
	platform, group := Fixture()
	before, _ := Deployment(platform, group, Settings{})
	group.Spec.RestartedAt = "2026-10-19T08:00:00Z"
	after, _ := Deployment(platform, group, Settings{})
	if before.Spec.Template.Annotations[AnnotationRestart] == after.Spec.Template.Annotations[AnnotationRestart] {
		t.Error("a new restartedAt leaves the pod template as it was: nothing would roll")
	}
	group.Spec.RestartedAt = ""
	none, _ := Deployment(platform, group, Settings{})
	if _, ok := none.Spec.Template.Annotations[AnnotationRestart]; ok {
		t.Error("an empty restartedAt is written into the template")
	}
}

func TestDocument(t *testing.T) {
	platform, group := Fixture()
	doc := Document(platform, group, "hugr-node-etl-7f9c-x2k8p", platform.EffectiveHeartbeatSeconds())
	if err := doc.Validate(); err != nil {
		t.Fatalf("the document the operator builds is one the node refuses: %v", err)
	}
	golden(t, "document", doc)
	group.Spec.ResourceGroup = "etl-prod"
	if got := Document(platform, group, "p", 15).NodeGroup; got != "etl-prod" {
		t.Errorf("node_group = %q, want the group's resource group", got)
	}
	// the interval is the pod's own (its template's), the platform's only for a pod without one
	platform.Spec.Heartbeat.IntervalSeconds = 2
	if got := HeartbeatSeconds(platform, map[string]string{AnnotationHeartbeat: "15"}); got != 15 {
		t.Errorf("interval = %d, want the pod's 15", got)
	}
	if got := HeartbeatSeconds(platform, nil); got != 2 {
		t.Errorf("interval = %d, want the platform's 2", got)
	}
	before, _ := Deployment(platform, group, Settings{})
	platform.Spec.Heartbeat.IntervalSeconds = 30
	after, _ := Deployment(platform, group, Settings{})
	if before.Spec.Template.Annotations[AnnotationHeartbeat] == after.Spec.Template.Annotations[AnnotationHeartbeat] {
		t.Error("a new interval leaves the pod template as it was: pods would beat at one interval, judged at another")
	}
}

// TestFirstPolicy: the console's client and audience always, the platform's groups' resource groups once each, the
// IdP's URL by default (spec 005, hugr-node spec 015)
func TestFirstPolicy(t *testing.T) {
	platform, group := Fixture()
	if policy, err := FirstPolicy(platform, nil); policy != nil || err != nil {
		t.Fatalf("a platform without a first policy: %v, %v", policy, err)
	}
	platform.Spec.FirstPolicy = &v1alpha1.FirstPolicySpec{
		Issuer: v1alpha1.FirstPolicyIssuer{Name: "corp", Audiences: []string{"api://hugr-tools", "api://hugr"},
			RolesClaim: "roles"},
		Clients:        []v1alpha1.FirstPolicyClient{{Name: "dbeaver", ClientID: "dbeaver-app"}},
		Administrators: "hugr-admins",
		DefaultGroup:   "etl",
	}
	if _, err := FirstPolicy(platform, nil); err == nil {
		t.Error("a first policy without the console is built")
	}
	platform.Spec.Console = &v1alpha1.ConsoleSpec{ClientID: "console-app",
		Audiences: v1alpha1.ConsoleAudiences{Nodes: "api://hugr"}}
	bi := *group
	bi.Name, bi.Spec.ResourceGroup = "bi", "bi-prod"
	twin := *group
	twin.Name, twin.Spec.ResourceGroup = "etl-twin", "etl" // the same resource group as etl's
	foreign := *group
	foreign.Name, foreign.Spec.Platform = "other", "another-platform"
	policy, err := FirstPolicy(platform, []v1alpha1.HugrNodeGroup{bi, *group, twin, foreign})
	if err != nil {
		t.Fatal(err)
	}
	doc := Document(platform, group, "hugr-node-etl-7f9c-x2k8p", platform.EffectiveHeartbeatSeconds())
	doc.FirstPolicy = policy
	if err := doc.Validate(); err != nil {
		t.Fatalf("the first policy the operator builds is one the node refuses: %v", err)
	}
	golden(t, "document-first-policy", doc)
	platform.Spec.FirstPolicy.DefaultGroup = "missing"
	if _, err := FirstPolicy(platform, []v1alpha1.HugrNodeGroup{*group}); err == nil {
		t.Error("a default group no group serves is built")
	}
}

func TestCheckSize(t *testing.T) {
	platform, _ := Fixture()
	if err := CheckSize(platform, "large"); err != nil {
		t.Errorf("a complete size: %v", err)
	}
	bare := platform.Spec.Sizes["large"]
	bare.Resources.Requests = nil
	platform.Spec.Sizes["bare"] = bare
	if err := CheckSize(platform, "bare"); err == nil {
		t.Error("a size without a CPU request: the node would get HUGR_POD_CPU 0")
	}
}
