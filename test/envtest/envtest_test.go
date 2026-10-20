//go:build envtest

// envtest (spec 001): the operator against a real API server and etcd, no kubelet - the reconcile creates, updates
// and recreates a group's objects, the webhook refuses through the API server, a Lease that stops being renewed makes
// its node lost, the pod-deletion cost follows the reported sessions, and a node's entry runs on a real bound token
// (TokenRequest bound to the pod, TokenReview). Run with `make test-envtest`.
package envtest

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/yaml"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/build"
	"github.com/hugr-lab/hugr-platform/internal/controller"
	"github.com/hugr-lab/hugr-platform/internal/nodeapi"
	groupwebhook "github.com/hugr-lab/hugr-platform/internal/webhook"
	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

const ns = "hugr"

var skipNameValidation = true

type cluster struct {
	client    client.Client
	clientset *kubernetes.Clientset
	api       http.Handler
}

func start(t *testing.T) *cluster {
	t.Helper()
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{Paths: []string{filepath.Join("..", "..", "config", "webhook")}},
	}
	config, err := env.Start()
	if err != nil {
		t.Fatalf("envtest: %v (run through make test-envtest: it fetches the API server)", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	hooks := env.WebhookInstallOptions
	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		// every test starts its own manager in this one process
		Controller: ctrlconfig.Controller{SkipNameValidation: &skipNameValidation},
		WebhookServer: webhook.NewServer(webhook.Options{Host: hooks.LocalServingHost, Port: hooks.LocalServingPort,
			CertDir: hooks.LocalServingCertDir}),
	})
	if err != nil {
		t.Fatal(err)
	}
	reconciler := &controller.GroupReconciler{Client: mgr.GetClient(), Direct: mgr.GetAPIReader(), Scheme: scheme,
		Settings: build.Settings{OrchestratorURL: "https://hugr-platform.hugr.svc:8443"}}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	if err := (&groupwebhook.GroupValidator{Reader: mgr.GetAPIReader()}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	front := &controller.FrontReconciler{Client: mgr.GetClient(), Scheme: scheme,
		Settings: build.Settings{FrontImage: "ghcr.io/hugr-lab/hugr-platform:dev"}}
	if err := front.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("no cache")
	}
	// the webhook serves before a group is written
	dialer := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // envtest's own certificate
	addr := fmt.Sprintf("%s:%d", hooks.LocalServingHost, hooks.LocalServingPort)
	eventually(t, 10*time.Second, "the webhook serves", func() bool {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
		}
		return err == nil
	})

	clientset := kubernetes.NewForConfigOrDie(config)
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	api := &nodeapi.Server{
		Auth:   &nodeapi.Authenticator{Reviewer: nodeapi.KubeReviewer{Client: clientset}, Reader: c, Direct: c, Namespace: ns},
		Leases: c, Direct: c, Log: logr.Discard(),
	}
	cl := &cluster{client: c, clientset: clientset, api: api.Handler()}
	must(t, c.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	must(t, c.Create(context.Background(), platform()))
	return cl
}

func platform() *v1alpha1.HugrPlatform {
	size := v1alpha1.SizeSpec{
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")}},
		Temp: v1alpha1.TempSpec{Medium: "local", Size: resource.MustParse("10Gi")},
	}
	return &v1alpha1.HugrPlatform{ObjectMeta: metav1.ObjectMeta{Name: "hugr", Namespace: ns},
		Spec: v1alpha1.HugrPlatformSpec{
			Node:       v1alpha1.NodeSpec{Image: "ghcr.io/hugr-lab/hugr-node:dev"},
			IdP:        v1alpha1.IdPSpec{Issuer: "https://login.example/t/v2.0"},
			Tresor:     v1alpha1.TresorSpec{Host: "tresor.example:443"},
			Repository: v1alpha1.RepositorySpec{Prefix: "https://kista.example/t/prod"},
			Heartbeat:  v1alpha1.HeartbeatSpec{IntervalSeconds: 1},
			Sizes:      map[string]v1alpha1.SizeSpec{"small": size},
			Plan:       v1alpha1.PlanSpec{MaxNodes: 4, Sizes: []string{"small"}},
		}}
}

func group(name string, replicas int32) *v1alpha1.HugrNodeGroup {
	return &v1alpha1.HugrNodeGroup{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.HugrNodeGroupSpec{Platform: "hugr", Replicas: replicas, Size: "small",
			Identity: v1alpha1.WorkloadIdentity{ClientID: name + "-app"},
			TLS:      v1alpha1.TLSSpec{SecretRef: &v1alpha1.SecretRef{Name: "hugr-node-" + name + "-tls"}}}}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, within time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("not within %v: %s", within, what)
}

func key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: ns, Name: name}
}

func TestOperator(t *testing.T) {
	c := start(t)
	ctx := context.Background()

	// the reconcile creates the group's objects
	must(t, c.client.Create(ctx, group("etl", 2)))
	var deployment appsv1.Deployment
	eventually(t, 15*time.Second, "the group's Deployment", func() bool {
		return c.client.Get(ctx, key("hugr-node-etl"), &deployment) == nil
	})
	for _, obj := range []client.Object{&corev1.ServiceAccount{}, &corev1.Service{}} {
		eventually(t, 10*time.Second, fmt.Sprintf("the group's %T", obj), func() bool {
			return c.client.Get(ctx, key("hugr-node-etl"), obj) == nil
		})
	}

	// ...updates them when the group changes
	var etl v1alpha1.HugrNodeGroup
	must(t, c.client.Get(ctx, key("etl"), &etl))
	etl.Spec.Replicas = 3
	etl.Spec.RestartedAt = "2026-10-18T12:00:00Z"
	must(t, c.client.Update(ctx, &etl))
	eventually(t, 15*time.Second, "the Deployment follows the group", func() bool {
		if err := c.client.Get(ctx, key("hugr-node-etl"), &deployment); err != nil {
			return false
		}
		return *deployment.Spec.Replicas == 3 &&
			deployment.Spec.Template.Annotations[build.AnnotationRestart] == "2026-10-18T12:00:00Z"
	})

	// ...and recreates one that went
	var service corev1.Service
	must(t, c.client.Get(ctx, key("hugr-node-etl"), &service))
	must(t, c.client.Delete(ctx, &service))
	eventually(t, 15*time.Second, "the Service recreated", func() bool {
		var again corev1.Service
		return c.client.Get(ctx, key("hugr-node-etl"), &again) == nil && again.UID != service.UID
	})

	// the webhook refuses through the API server: 3 + 2 past the plan's 4
	err := c.client.Create(ctx, group("bi", 2))
	if err == nil || !strings.Contains(err.Error(), "at most 4") {
		t.Fatalf("a group past the plan: %v", err)
	}
	// names the API server accepts (DNS subdomains) and the webhook does not: its pods' names would be no labels
	for _, name := range []string{"etl.prod", strings.Repeat("g", 37)} {
		if err := c.client.Create(ctx, group(name, 0)); err == nil || !strings.Contains(err.Error(), "DNS label") {
			t.Errorf("a group named %q: %v", name, err)
		}
	}

	// a node's entry on a real bound token: the group's ReplicaSet and pod (no kube-controller-manager here)
	must(t, c.client.Get(ctx, key("hugr-node-etl"), &deployment))
	yes := true
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "hugr-node-etl-7f9c", Namespace: ns,
		Labels: deployment.Spec.Selector.MatchLabels,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: deployment.Name,
			UID: deployment.UID, Controller: &yes}}},
		Spec: appsv1.ReplicaSetSpec{Selector: deployment.Spec.Selector, Template: deployment.Spec.Template}}
	must(t, c.client.Create(ctx, rs))
	pods := map[string]*corev1.Pod{}
	for _, name := range []string{"hugr-node-etl-7f9c-a", "hugr-node-etl-7f9c-b"} {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: deployment.Spec.Template.Labels,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID,
				Controller: &yes}}},
			Spec: deployment.Spec.Template.Spec}
		must(t, c.client.Create(ctx, pod))
		pods[name] = pod
	}
	token := func(pod *corev1.Pod) string {
		request, err := c.clientset.CoreV1().ServiceAccounts(ns).CreateToken(ctx, "hugr-node-etl", &authenticationv1.TokenRequest{
			Spec: authenticationv1.TokenRequestSpec{Audiences: []string{build.OrchestratorAud},
				BoundObjectRef: &authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID}},
		}, metav1.CreateOptions{})
		must(t, err)
		return request.Status.Token
	}
	call := func(method, path, bearer, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		c.api.ServeHTTP(rec, req)
		return rec
	}
	a, b := pods["hugr-node-etl-7f9c-a"], pods["hugr-node-etl-7f9c-b"]
	tokenA := token(a)
	rec := call(http.MethodGet, "/v1/bootstrap", tokenA, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("the entry on a bound token: %d %s", rec.Code, rec.Body)
	}
	var document nodeproto.Document
	must(t, json.Unmarshal(rec.Body.Bytes(), &document))
	if document.NodeLabel != a.Name || document.NodeGroup != "etl" || document.Tresor.Identity.ClientID != "etl-app" {
		t.Errorf("the document: %+v", document)
	}
	// a token for another audience (the IdP's federation one) is no entry
	other, err := c.clientset.CoreV1().ServiceAccounts(ns).CreateToken(ctx, "hugr-node-etl", &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{Audiences: []string{v1alpha1.DefaultFederationAudience},
			BoundObjectRef: &authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: a.Name, UID: a.UID}},
	}, metav1.CreateOptions{})
	must(t, err)
	if rec := call(http.MethodGet, "/v1/bootstrap", other.Status.Token, ""); rec.Code != http.StatusForbidden {
		t.Errorf("a token of the federation audience: %d, want 403", rec.Code)
	}

	// heartbeats: a reports 5 sessions and the target applied; b once, then never again
	beatOnce := func(pod *corev1.Pod, bearer string, sessions int) int {
		body := fmt.Sprintf(`{"node_id": %q, "config": {"target": 3, "applied": 3}, "load": {"group_known": true,
			"sessions": {"live": %d}, "admit": {"new_session": true}}}`, pod.Name, sessions)
		return call(http.MethodPost, "/v1/heartbeat", bearer, body).Code
	}
	beat := func(pod *corev1.Pod, bearer string, sessions int) {
		if code := beatOnce(pod, bearer, sessions); code != http.StatusNoContent {
			t.Fatalf("heartbeat of %s: %d", pod.Name, code)
		}
	}
	tokenB := token(b)
	beat(a, tokenA, 5)
	beat(b, tokenB, 1)
	// b speaking for a is refused
	if rec := call(http.MethodPost, "/v1/heartbeat", tokenB, `{"node_id": "`+a.Name+`"}`); rec.Code != http.StatusConflict {
		t.Errorf("a node beating for another: %d, want 409", rec.Code)
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(500 * time.Millisecond):
				beatOnce(a, tokenA, 5) // never t.Fatalf off the test's goroutine
			}
		}
	}()
	// the pod-deletion cost follows the reported sessions (5, in powers of two: 4)
	eventually(t, 20*time.Second, "a's deletion cost from its sessions", func() bool {
		var pod corev1.Pod
		return c.client.Get(ctx, key(a.Name), &pod) == nil && pod.Annotations[controller.AnnotationDeletionCost] == "4"
	})
	// b's Lease (3 × 1 s) expires: b is lost, a is not
	eventually(t, 30*time.Second, "b lost, a live and applied", func() bool {
		var g v1alpha1.HugrNodeGroup
		if c.client.Get(ctx, key("etl"), &g) != nil {
			return false
		}
		return len(g.Status.Lost) == 1 && g.Status.Lost[0] == b.Name && g.Status.Profile.Target == 3 &&
			g.Status.Profile.Applied == 1 && g.Status.ResourceGroupKnown != nil && *g.Status.ResourceGroupKnown &&
			!meta.IsStatusConditionTrue(g.Status.Conditions, controller.ConditionDraining)
	})
	var lease coordinationv1.Lease
	must(t, c.client.Get(ctx, key(a.Name), &lease))
	if len(lease.OwnerReferences) != 1 || lease.OwnerReferences[0].UID != a.UID {
		t.Errorf("a's Lease is not the pod's: %+v", lease.OwnerReferences)
	}
	// a pod that is gone is no node any more
	must(t, c.client.Delete(ctx, b))
	eventually(t, 10*time.Second, "b's pod gone", func() bool {
		return apierrors.IsNotFound(c.client.Get(ctx, key(b.Name), &corev1.Pod{}))
	})
	if rec := call(http.MethodPost, "/v1/heartbeat", tokenB, `{"node_id": "`+b.Name+`"}`); rec.Code != http.StatusForbidden {
		t.Errorf("a heartbeat from a deleted pod's token: %d, want 403", rec.Code)
	}

	// the optional admission policies (config/policy): only the chain that owns them makes a group's pods (the
	// operator its Deployment, the controllers its ReplicaSets and pods) - the test's own above were made by hand,
	// which they now refuse
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "policy", "group-pods.yaml"))
	must(t, err)
	for _, document := range strings.Split(string(raw), "\n---\n") {
		var kind struct{ Kind string }
		must(t, yaml.Unmarshal([]byte(document), &kind))
		var obj client.Object = &admissionregistrationv1.ValidatingAdmissionPolicy{}
		if kind.Kind == "ValidatingAdmissionPolicyBinding" {
			obj = &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
		}
		must(t, yaml.Unmarshal([]byte(document), obj))
		must(t, c.client.Create(ctx, obj))
	}
	must(t, c.client.Patch(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns,
		Labels: map[string]string{"kubernetes.io/metadata.name": ns}}}, client.Merge))
	eventually(t, 20*time.Second, "a hand-made pod of the group refused by the policy", func() bool {
		hand := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{GenerateName: "by-hand-", Namespace: ns,
			Labels: deployment.Spec.Template.Labels}, Spec: deployment.Spec.Template.Spec}
		err := c.client.Create(ctx, hand)
		if err == nil {
			_ = c.client.Delete(ctx, hand)
			return false
		}
		return strings.Contains(err.Error(), "created by its Deployment only")
	})
	// ...nor a ReplicaSet made by hand to make them
	eventually(t, 20*time.Second, "a hand-made ReplicaSet of the group refused by the policy", func() bool {
		hand := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{GenerateName: "by-hand-", Namespace: ns},
			Spec: appsv1.ReplicaSetSpec{Selector: deployment.Spec.Selector, Template: deployment.Spec.Template}}
		err := c.client.Create(ctx, hand)
		if err == nil {
			_ = c.client.Delete(ctx, hand)
			return false
		}
		return strings.Contains(err.Error(), "is its Deployment's only")
	})
	// ...and a pod of another account is none of its business
	unrelated := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: ns},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}}}
	must(t, c.client.Create(ctx, unrelated))
}

// TestDoorsCertificate (spec 005): the API server itself refuses a group that names both doors' certificates or
// neither (the CRD's rule, whatever the webhook does), and a group whose certificate the csi-driver issues gets a
// Deployment that mounts no Secret.
// TestFirstPolicyFields: the CRD refuses a first policy without the console, and a client named as the console's
func TestFirstPolicyFields(t *testing.T) {
	c := start(t)
	ctx := context.Background()
	first := func() *v1alpha1.HugrPlatform {
		p := platform()
		p.Name = "first"
		p.Spec.Console = &v1alpha1.ConsoleSpec{ClientID: "console-app", Audiences: v1alpha1.ConsoleAudiences{Nodes: "api://hugr"}}
		p.Spec.FirstPolicy = &v1alpha1.FirstPolicySpec{
			Issuer:         v1alpha1.FirstPolicyIssuer{Name: "corp", RolesClaim: "roles"},
			Administrators: "hugr-admins",
		}
		return p
	}
	must(t, c.client.Create(ctx, first(), client.DryRunAll))
	noConsole := first()
	noConsole.Spec.Console = nil
	asConsole := first()
	asConsole.Spec.FirstPolicy.Clients = []v1alpha1.FirstPolicyClient{{Name: "console", ClientID: "x"}}
	badName := first()
	badName.Spec.FirstPolicy.Issuer.Name = "corp.x"
	overHTTP := first()
	overHTTP.Spec.FirstPolicy.Issuer.URL = "http://login.example"
	emptyAudience := first()
	emptyAudience.Spec.FirstPolicy.Issuer.Audiences = []string{""}
	for name, p := range map[string]*v1alpha1.HugrPlatform{"without the console": noConsole,
		"a client named console": asConsole, "an issuer name with a dot": badName, "an issuer over http": overHTTP,
		"an empty audience": emptyAudience} {
		if err := c.client.Create(ctx, p, client.DryRunAll); err == nil {
			t.Errorf("%s: accepted, want the CRD's refusal", name)
		}
	}
}

func TestDoorsCertificate(t *testing.T) {
	c := start(t)
	ctx := context.Background()
	csi := &v1alpha1.CSICertificate{IssuerRef: v1alpha1.IssuerRef{Name: "hugr-internal"}, Duration: "720h"}

	both := group("both", 1)
	both.Spec.TLS.CSI = csi
	neither := group("neither", 1)
	neither.Spec.TLS = v1alpha1.TLSSpec{}
	for _, g := range []*v1alpha1.HugrNodeGroup{both, neither} {
		err := c.client.Create(ctx, g)
		if err == nil || !strings.Contains(err.Error(), "exactly one of secretRef and csi") {
			t.Errorf("group %s: %v, want the CRD's refusal", g.Name, err)
		}
	}

	issued := group("issued", 1)
	issued.Spec.TLS = v1alpha1.TLSSpec{CSI: csi}
	must(t, c.client.Create(ctx, issued))
	var deployment appsv1.Deployment
	eventually(t, 15*time.Second, "the csi group's Deployment", func() bool {
		return c.client.Get(ctx, key("hugr-node-issued"), &deployment) == nil
	})
	for _, v := range deployment.Spec.Template.Spec.Volumes {
		if v.Secret != nil {
			t.Errorf("the csi group's pods mount the Secret %q", v.Secret.SecretName)
		}
		if v.Name == "tls" && (v.CSI == nil || v.CSI.Driver != build.CSIDriver) {
			t.Errorf("the doors' volume is not the csi-driver's: %+v", v.VolumeSource)
		}
	}
}
