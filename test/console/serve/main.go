// The console without Kubernetes (spec 003's end to end): its real server, pool and web build over a static fleet of
// one node, a platform and a group kept in memory, and a pod made up for the node's page. Only for test/console/e2e.
//
//	go run ./test/console/serve -listen 127.0.0.1:18644 -cert c.crt -key c.key -ca ca.crt -node etl=127.0.0.1:41101 \
//	  -issuer https://127.0.0.1:18691/realms/hugr -client-id hugr-console -audience hugr-nodes -dist web/console/dist
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/build"
	"github.com/hugr-lab/hugr-platform/internal/console"
	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
	webconsole "github.com/hugr-lab/hugr-platform/web/console"
)

func main() {
	var listen, certFile, keyFile, caFile, nodeFlag, issuer, clientID, audience, dist string
	flag.StringVar(&listen, "listen", "127.0.0.1:18644", "where the console listens (https)")
	flag.StringVar(&certFile, "cert", "", "the console's certificate")
	flag.StringVar(&keyFile, "key", "", "its key")
	flag.StringVar(&caFile, "ca", "", "the CA the node's certificate is signed by")
	flag.StringVar(&nodeFlag, "node", "", "<group>=<host>:<flight port>")
	flag.StringVar(&issuer, "issuer", "", "the IdP's issuer")
	flag.StringVar(&clientID, "client-id", "hugr-console", "the console's public client")
	flag.StringVar(&audience, "audience", "hugr-nodes", "the nodes' audience")
	flag.StringVar(&dist, "dist", "web/console/dist", "the web build")
	flag.Parse()
	group, address, _ := strings.Cut(nodeFlag, "=")
	host, portText, _ := strings.Cut(address, ":")
	port, err := strconv.Atoi(portText)
	if group == "" || err != nil {
		fail(fmt.Errorf("-node is <group>=<host>:<port>"))
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		fail(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem)

	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	platform := &v1alpha1.HugrPlatform{ObjectMeta: metav1.ObjectMeta{Name: "e2e", Namespace: "hugr"},
		Spec: v1alpha1.HugrPlatformSpec{IdP: v1alpha1.IdPSpec{Issuer: issuer},
			Tresor:     v1alpha1.TresorSpec{Host: "tresor.invalid:443"},
			Repository: v1alpha1.RepositorySpec{Prefix: "https://kista.invalid/corp/prod"},
			Sizes: map[string]v1alpha1.SizeSpec{"small": {Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")}}}},
			Plan: v1alpha1.PlanSpec{MaxNodes: 4, Sizes: []string{"small"}},
			Console: &v1alpha1.ConsoleSpec{ClientID: clientID, Audiences: v1alpha1.ConsoleAudiences{Nodes: audience},
				Links: []v1alpha1.ConsoleLink{{Kind: "telemetry", Name: "Grafana (an example link)",
					URL: "https://grafana.example.org/explore?node={node}"}}}}}
	g := &v1alpha1.HugrNodeGroup{ObjectMeta: metav1.ObjectMeta{Name: group, Namespace: "hugr"},
		Spec: v1alpha1.HugrNodeGroupSpec{Platform: "e2e", Replicas: 1, Size: "small",
			Identity: v1alpha1.WorkloadIdentity{ClientID: "e2e-" + group}},
		Status: v1alpha1.HugrNodeGroupStatus{Replicas: 1, ReadyReplicas: 1}}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(platform, g).WithStatusSubresource(g).Build()

	label := build.NodeName + "-" + group + "-a"
	nodes := fleet.New()
	nodes.Set(fleet.NewView([]fleet.Node{{Label: label, Service: build.NodeName + "-" + group, Address: host,
		FlightPort: int32(port), Ready: true, Serving: true, Lease: &fleet.Lease{Renewed: time.Now(),
			Until: time.Now().Add(24 * time.Hour), Summary: nodeproto.Summary{AdmitSession: true, SeatsLeft: -1}}}}))
	web, err := webconsole.New(func() string { return issuer }, os.DirFS(dist))
	if err != nil {
		fail(err)
	}
	log := funcr.New(func(prefix, args string) { fmt.Fprintln(os.Stderr, prefix, args) }, funcr.Options{})
	server := &console.Server{
		Platform: func(context.Context) (*v1alpha1.HugrPlatform, error) { return platform.DeepCopy(), nil },
		Reader:   reader,
		Groups:   &memoryWriter{reader: reader, log: log},
		// spec 006: a group the console makes - kept in memory, as every write of this server
		NewGroupTLS: console.GroupTLS("hugr-internal", "720h"),
		Fleet:       nodes,
		Nodes:       &console.Pool{Namespace: "hugr", Roots: func(context.Context) (*x509.CertPool, error) { return roots, nil }},
		Kube:        madeUpKube{label: label, group: group},
		Web:         web,
		Log:         log,
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		fail(err)
	}
	s := &http.Server{Addr: listen, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}}
	fmt.Fprintln(os.Stderr, "console on https://"+listen)
	fail(s.ListenAndServeTLS("", ""))
}

// memoryWriter applies a group change to the in-memory group: the page shows it, nothing is applied anywhere.
type memoryWriter struct {
	mu     sync.Mutex
	reader client.Client
	log    logr.Logger
}

func (w *memoryWriter) Change(ctx context.Context, change console.GroupChange) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var g v1alpha1.HugrNodeGroup
	if err := w.reader.Get(ctx, client.ObjectKey{Namespace: change.Namespace, Name: change.Name}, &g); err != nil {
		return err
	}
	if change.Replicas != nil {
		g.Spec.Replicas = *change.Replicas
	}
	if change.RestartedAt != "" {
		g.Spec.RestartedAt = change.RestartedAt
	}
	if change.Size != "" {
		g.Spec.Size = change.Size
	}
	if change.Quack != nil {
		g.Spec.Doors.Quack = *change.Quack
	}
	if change.DrainSeconds != nil {
		g.Spec.DrainSeconds = change.DrainSeconds
	}
	w.log.Info("group changed", "group", change.Name, "by", change.ChangedBy)
	return w.reader.Update(ctx, &g)
}

func (w *memoryWriter) Create(ctx context.Context, group *v1alpha1.HugrNodeGroup) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.log.Info("group made", "group", group.Name)
	return w.reader.Create(ctx, group)
}

func (w *memoryWriter) Delete(ctx context.Context, namespace, name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.log.Info("group deleted", "group", name)
	return w.reader.Delete(ctx, &v1alpha1.HugrNodeGroup{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}})
}

// madeUpKube is the node's pod as a page shows it: its usage and the pod made up, no events, a log that says where it
// is.
type madeUpKube struct{ label, group string }

func (k madeUpKube) Pod(_ context.Context, name string) (*corev1.Pod, error) {
	start := metav1.NewTime(time.Now().Add(-time.Hour))
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PodSpec{NodeName: "e2e-machine", Containers: []corev1.Container{{Name: build.NodeName, Image: "hugr-node:e2e",
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "127.0.0.1", StartTime: &start,
			ContainerStatuses: []corev1.ContainerStatus{{Name: build.NodeName, ImageID: "sha256:e2e"}}}}, nil
}

func (madeUpKube) Events(context.Context, string) ([]corev1.Event, error) { return nil, nil }

func (madeUpKube) Logs(context.Context, string, bool, int64) (string, error) {
	return "e2e: this node runs outside Kubernetes; its log is the run's node.log\n", nil
}

// GroupPods: the stand's one pod, running (made up, as Pod's) - a scale's new pods are the cluster's to make
func (k madeUpKube) GroupPods(ctx context.Context) ([]corev1.Pod, error) {
	p, _ := k.Pod(ctx, k.label)
	p.Labels = map[string]string{build.LabelGroup: k.group}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	return []corev1.Pod{*p}, nil
}

func (madeUpKube) GroupDeployments(context.Context) ([]appsv1.Deployment, error) { return nil, nil }

func (madeUpKube) PodWarnings(context.Context) ([]corev1.Event, error) { return nil, nil }

// the pod's usage as the metrics API would answer it: made up, as the pod is
var madeUpUsage = console.Usage{CPUMilli: 120, MemoryBytes: 900 << 20}

func (madeUpKube) Usage(context.Context, string) (*console.Usage, error) { return &madeUpUsage, nil }

func (k madeUpKube) UsageAll(context.Context) (map[string]console.Usage, error) {
	return map[string]console.Usage{k.label: madeUpUsage}, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}
