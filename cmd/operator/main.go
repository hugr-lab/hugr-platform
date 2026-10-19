// The hugr platform's operator (spec 001): it keeps each node group's objects, answers a node's entry and records its
// heartbeat. Every replica answers HTTP (the node API, the webhook, the probes); the leader reconciles.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/build"
	"github.com/hugr-lab/hugr-platform/internal/console"
	"github.com/hugr-lab/hugr-platform/internal/controller"
	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
	"github.com/hugr-lab/hugr-platform/internal/front/watch"
	"github.com/hugr-lab/hugr-platform/internal/nodeapi"
	groupwebhook "github.com/hugr-lab/hugr-platform/internal/webhook"
	webconsole "github.com/hugr-lab/hugr-platform/web/console"
)

func main() {
	var (
		namespace       string
		orchestratorURL string
		nodeAPIAddr     string
		certDir         string
		metricsAddr     string
		probeAddr       string
		leaderElect     bool
		webhooks        bool
		frontImage      string
		frontPull       string
		consoleAddr     string
		consoleCertDir  string
		consolePlatform string
	)
	flag.StringVar(&namespace, "namespace", os.Getenv("POD_NAMESPACE"), "the namespace the operator serves")
	flag.StringVar(&orchestratorURL, "orchestrator-url", "", "the operator's in-cluster https URL the nodes reach (HUGR_ORCHESTRATOR)")
	flag.StringVar(&nodeAPIAddr, "node-api-address", ":8443", "where the node API listens (https)")
	flag.StringVar(&certDir, "cert-dir", "/etc/hugr-platform/tls", "the serving certificate (tls.crt, tls.key) of the node API and the webhook")
	flag.StringVar(&metricsAddr, "metrics-address", ":8080", "where the metrics listen, https behind TokenReview + SubjectAccessReview (0 = off)")
	flag.StringVar(&probeAddr, "health-probe-address", ":8081", "where /healthz and /readyz listen")
	flag.BoolVar(&leaderElect, "leader-elect", true, "elect a leader to reconcile")
	flag.BoolVar(&webhooks, "webhooks", true, "serve the validating webhook")
	flag.StringVar(&frontImage, "front-image", os.Getenv("HUGR_FRONT_IMAGE"), "the image the front runs: the operator's own (spec 002)")
	flag.StringVar(&frontPull, "front-image-pull-policy", os.Getenv("HUGR_FRONT_IMAGE_PULL_POLICY"), "the front's image pull policy")
	flag.StringVar(&consoleAddr, "console-address", ":8444", "where the web console listens (https, spec 003)")
	flag.StringVar(&consoleCertDir, "console-cert-dir", "/etc/hugr-console/tls", "the console's public certificate (tls.crt, tls.key); none: no console")
	flag.StringVar(&consolePlatform, "console-platform", "", "the HugrPlatform the console serves (empty: the only one of the namespace)")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	logger := ctrl.Log.WithName("hugr-platform")

	if namespace == "" || orchestratorURL == "" {
		logger.Error(errors.New("--namespace and --orchestrator-url are required"), "not starting")
		os.Exit(2)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	options := ctrl.Options{
		Scheme: scheme,
		Cache:  cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}},
		// the metrics say how the fleet is reconciled: https, and only for a caller allowed to get /metrics
		Metrics: metricsserver.Options{BindAddress: metricsAddr, SecureServing: true,
			FilterProvider: filters.WithAuthenticationAndAuthorization},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          leaderElect,
		LeaderElectionID:        "hugr-platform-operator",
		LeaderElectionNamespace: namespace,
	}
	if webhooks {
		options.WebhookServer = webhook.NewServer(webhook.Options{Port: 9443, CertDir: certDir})
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), options)
	if err != nil {
		logger.Error(err, "no manager")
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		logger.Error(err, "no clientset")
		os.Exit(1)
	}
	if frontImage == "" {
		// the front runs the operator's own image (spec 002 Q1): read from this pod, so an install that renames the
		// image (kustomize, a Managed Application) needs nothing more
		frontImage, frontPull = ownImage(clientset, namespace)
	}
	settings := build.Settings{OrchestratorURL: orchestratorURL, FrontImage: frontImage,
		FrontImagePullPolicy: corev1.PullPolicy(frontPull)}
	reconciler := &controller.GroupReconciler{Client: mgr.GetClient(), Direct: mgr.GetAPIReader(), Scheme: mgr.GetScheme(),
		Settings: settings}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		logger.Error(err, "no group controller")
		os.Exit(1)
	}
	if err := (&controller.FrontReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Settings: settings}).
		SetupWithManager(mgr); err != nil {
		logger.Error(err, "no front controller")
		os.Exit(1)
	}
	if webhooks {
		if err := (&groupwebhook.GroupValidator{Reader: mgr.GetAPIReader()}).SetupWithManager(mgr); err != nil {
			logger.Error(err, "no webhook")
			os.Exit(1)
		}
	}

	api := &nodeapi.Server{
		Auth: &nodeapi.Authenticator{Reviewer: nodeapi.KubeReviewer{Client: clientset}, Reader: mgr.GetClient(),
			Direct: mgr.GetAPIReader(), Namespace: namespace},
		Leases: mgr.GetClient(),
		Direct: mgr.GetAPIReader(),
		Log:    logger.WithName("node-api"),
	}
	door := &nodeAPI{addr: nodeAPIAddr, certDir: certDir, handler: api.Handler()}
	if err := mgr.Add(door); err != nil {
		logger.Error(err, "no node API")
		os.Exit(1)
	}

	// the console (spec 003): every replica serves it when its certificate is mounted
	platforms := console.PlatformFinder{Reader: mgr.GetClient(), Namespace: namespace, Name: consolePlatform}
	roots := &console.Roots{Platform: platforms.Find, Direct: mgr.GetAPIReader()}
	pool := &console.Pool{Namespace: namespace, Roots: roots.Pool}
	nodes := fleet.New()
	web, err := webconsole.New(func() string {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if p, err := platforms.Find(ctx); err == nil {
			return p.Spec.IdP.Issuer
		}
		return ""
	}, nil)
	if err != nil {
		logger.Error(err, "no web console")
		os.Exit(1)
	}
	if !web.Built() {
		logger.Info("the web console is not built into this binary: its page says how to build it")
	}
	consoleDoor := &console.Door{
		Addr:    consoleAddr,
		CertDir: consoleCertDir,
		Server: &console.Server{Platform: platforms.Find, Reader: mgr.GetClient(),
			Groups: console.ApplyWriter{Client: mgr.GetClient()}, Fleet: nodes, Nodes: pool, Web: web,
			Log: logger.WithName("console")},
		Pool:  pool,
		Watch: watch.New(clientset, namespace, nodes, 15*time.Second).Run,
	}
	if consoleDoor.Configured() {
		if err := mgr.Add(consoleDoor); err != nil {
			logger.Error(err, "no console")
			os.Exit(1)
		}
		utilruntime.Must(mgr.AddReadyzCheck("console", consoleDoor.Ready))
	} else {
		logger.Info("no console: no certificate", "dir", consoleCertDir)
	}

	utilruntime.Must(mgr.AddHealthzCheck("healthz", healthz.Ping))
	// ready once what a node calls answers: a node that finds no orchestrator stops, so a replica is not ready before
	utilruntime.Must(mgr.AddReadyzCheck("node-api", door.Ready))
	if webhooks {
		utilruntime.Must(mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker()))
	}
	logger.Info("starting", "namespace", namespace)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "stopped")
		os.Exit(1)
	}
}

// nodeAPI serves the node API over TLS on every replica, its certificate re-read when it rotates.
type nodeAPI struct {
	addr      string
	certDir   string
	handler   http.Handler
	listening atomic.Bool
}

// Ready says whether the node API listens.
func (n *nodeAPI) Ready(*http.Request) error {
	if !n.listening.Load() {
		return errors.New("the node API does not listen yet")
	}
	return nil
}

var _ manager.LeaderElectionRunnable = &nodeAPI{}

func (n *nodeAPI) NeedLeaderElection() bool {
	return false
}

func (n *nodeAPI) Start(ctx context.Context) error {
	watcher, err := certwatcher.New(filepath.Join(n.certDir, "tls.crt"), filepath.Join(n.certDir, "tls.key"))
	if err != nil {
		return err
	}
	go func() {
		_ = watcher.Start(ctx)
	}()
	server := &http.Server{
		Addr:              n.addr,
		Handler:           n.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: watcher.GetCertificate},
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	listener, err := net.Listen("tcp", n.addr)
	if err != nil {
		return err
	}
	n.listening.Store(true)
	defer n.listening.Store(false)
	if err := server.ServeTLS(listener, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ownImage is the image and pull policy of this pod's operator container (empty when it cannot be read: the front's
// condition then says so).
func ownImage(clientset kubernetes.Interface, namespace string) (string, string) {
	name := os.Getenv("POD_NAME")
	if name == "" {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", ""
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == "operator" {
			return c.Image, string(c.ImagePullPolicy)
		}
	}
	return "", ""
}
