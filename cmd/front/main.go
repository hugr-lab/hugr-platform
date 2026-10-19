// The hugr platform's front (spec 002): the clients' one address for the nodes' two doors, Flight SQL and quack. It
// terminates TLS, seats a new connection on a node that may take it, and sends every later call of the connection to
// that node by the label the node wrote into it. It keeps no table of sessions: any replica routes any call.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc/credentials"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"

	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
	"github.com/hugr-lab/hugr-platform/internal/front/flightproxy"
	"github.com/hugr-lab/hugr-platform/internal/front/metrics"
	"github.com/hugr-lab/hugr-platform/internal/front/quackproxy"
	"github.com/hugr-lab/hugr-platform/internal/front/watch"
)

// Drain is how long streams in flight get after SIGTERM before they are cut.
const Drain = 30 * time.Second

// Unready is how long the front stays up, not ready, before it stops accepting: the Service's endpoints move first.
const Unready = 5 * time.Second

func main() {
	var (
		namespace, certDir, caFile, flightAddr, quackAddr, internalAddr string
		heartbeat, noNodeWait                                           time.Duration
	)
	flag.StringVar(&namespace, "namespace", os.Getenv("POD_NAMESPACE"), "the platform's namespace")
	flag.StringVar(&certDir, "cert-dir", "/etc/hugr-front/tls", "the public certificate (tls.crt, tls.key)")
	flag.StringVar(&caFile, "ca-file", "/etc/hugr-front/ca/ca.crt", "the cluster CA bundle the nodes' certificates are verified by")
	flag.StringVar(&flightAddr, "flight-address", ":32010", "where the Flight SQL door listens")
	flag.StringVar(&quackAddr, "quack-address", ":9494", "where the quack door listens")
	flag.StringVar(&internalAddr, "internal-address", ":8081", "where /metrics, /healthz and /readyz listen")
	flag.DurationVar(&heartbeat, "heartbeat", 15*time.Second, "the nodes' heartbeat interval: a watch lost for 3 makes the front not ready")
	flag.DurationVar(&noNodeWait, "no-node-wait", 5*time.Second, "how long a new connection waits for a node of its group")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log, namespace, certDir, caFile, flightAddr, quackAddr, internalAddr, heartbeat, noNodeWait); err != nil {
		log.Error("the front stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger, namespace, certDir, caFile, flightAddr, quackAddr, internalAddr string, heartbeat,
	noNodeWait time.Duration) error {
	if namespace == "" {
		return errors.New("--namespace is required")
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return errors.New("the CA bundle holds no certificate")
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	certs, err := certwatcher.New(filepath.Join(certDir, "tls.crt"), filepath.Join(certDir, "tls.key"))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	run, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = certs.Start(run) }()

	nodes := fleet.New()
	watcher := watch.New(client, namespace, nodes, heartbeat)
	go watcher.Run(run)
	flight := &flightproxy.Proxy{Fleet: nodes, Namespace: namespace, Roots: roots, NoNodeWait: noNodeWait, Log: log}
	quack := &quackproxy.Proxy{Fleet: nodes, Namespace: namespace, Roots: roots, NoNodeWait: noNodeWait, Log: log}
	go follow(run, nodes, flight)

	public := &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certs.GetCertificate}
	grpcServer := flight.Server(credentials.NewTLS(public))
	flightListener, err := net.Listen("tcp", flightAddr)
	if err != nil {
		return err
	}
	quackServer := &http.Server{Addr: quackAddr, Handler: quack.Handler(), TLSConfig: public.Clone(),
		ReadHeaderTimeout: 10 * time.Second}

	var stopping atomic.Bool
	internal := http.NewServeMux()
	internal.Handle("/metrics", promhttp.HandlerFor(metrics.Registry(), promhttp.HandlerOpts{}))
	internal.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	internal.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if stopping.Load() {
			http.Error(w, "stopping", http.StatusServiceUnavailable)
			return
		}
		if err := watcher.Ready(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	internalServer := &http.Server{Addr: internalAddr, Handler: internal, ReadHeaderTimeout: 10 * time.Second}

	failed := make(chan error, 3)
	go func() { failed <- grpcServer.Serve(flightListener) }()
	go func() { failed <- quackServer.ListenAndServeTLS("", "") }()
	go func() { failed <- internalServer.ListenAndServe() }()
	log.Info("serving", "flight", flightAddr, "quack", quackAddr, "internal", internalAddr)

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}
	// not ready at once; the endpoints move; then GOAWAY to every client and up to Drain for streams in flight
	stopping.Store(true)
	time.Sleep(Unready)
	drained := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(drained)
	}()
	shutdown, done := context.WithTimeout(context.Background(), Drain)
	defer done()
	_ = quackServer.Shutdown(shutdown)
	select {
	case <-drained:
	case <-shutdown.Done():
		grpcServer.Stop()
	}
	_ = internalServer.Close()
	return nil
}

// follow the fleet: connections to nodes it no longer has are closed, and its gauge kept.
func follow(ctx context.Context, nodes *fleet.Fleet, flight *flightproxy.Proxy) {
	for {
		view, changed := nodes.View()
		flight.Forget(view)
		metrics.Nodes.Reset()
		now := time.Now()
		for _, n := range view.Nodes() {
			state := "unknown"
			switch {
			case n.Lease == nil || !n.Current(now):
			case n.Placeable(fleet.Flight, now):
				state = "placeable"
			case n.Serving:
				state = "serving"
			}
			metrics.Nodes.WithLabelValues(n.Group(), state).Inc()
		}
		select {
		case <-changed:
		case <-time.After(time.Minute): // a Lease that expired changes no object
		case <-ctx.Done():
			return
		}
	}
}
