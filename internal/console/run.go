package console

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
)

// The console reads the platform's CA bundle and writes a group's count and restart (spec 003).
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get
// +kubebuilder:rbac:groups=platform.hugr.io,resources=hugrnodegroups,verbs=patch

// Door serves the console over TLS on every replica (spec 003: the leader only reconciles), its certificate - the
// Secret hugr-console, the administrator's or cert-manager's - re-read when it rotates.
type Door struct {
	Addr    string
	CertDir string
	Server  *Server
	// Pool is pruned of gone nodes and idle sessions every minute.
	Pool *Pool
	// Watch keeps the Server's fleet current until its context ends.
	Watch func(ctx context.Context)

	listening atomic.Bool
}

var _ manager.LeaderElectionRunnable = &Door{}

// NeedLeaderElection is false: every replica answers.
func (d *Door) NeedLeaderElection() bool { return false }

// Configured says whether the console's certificate is there: without it the operator serves no console.
func (d *Door) Configured() bool {
	_, err := os.Stat(filepath.Join(d.CertDir, "tls.crt"))
	return err == nil
}

// Ready says whether the console listens (a readiness check only when Configured).
func (d *Door) Ready(*http.Request) error {
	if !d.listening.Load() {
		return errors.New("the console does not listen yet")
	}
	return nil
}

// Start the watcher, the pruning and the listener.
func (d *Door) Start(ctx context.Context) error {
	watcher, err := certwatcher.New(filepath.Join(d.CertDir, "tls.crt"), filepath.Join(d.CertDir, "tls.key"))
	if err != nil {
		return err
	}
	go func() { _ = watcher.Start(ctx) }()
	if d.Watch != nil {
		go d.Watch(ctx)
	}
	if d.Pool != nil {
		go d.prune(ctx)
	}
	server := &http.Server{
		Addr:              d.Addr,
		Handler:           d.Server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: watcher.GetCertificate},
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	listener, err := net.Listen("tcp", d.Addr)
	if err != nil {
		return err
	}
	d.listening.Store(true)
	defer d.listening.Store(false)
	if err := server.ServeTLS(listener, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (d *Door) prune(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			v := d.Server.view()
			d.Pool.Prune(func(label string) bool { return v != nil && v.Node(label) != nil }, now)
		}
	}
}

// PlatformFinder finds the HugrPlatform the console serves: the one named, else the only one of the namespace.
type PlatformFinder struct {
	Reader    client.Reader
	Namespace string
	Name      string
}

// Find the platform.
func (f PlatformFinder) Find(ctx context.Context) (*v1alpha1.HugrPlatform, error) {
	if f.Name != "" {
		var p v1alpha1.HugrPlatform
		if err := f.Reader.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: f.Name}, &p); err != nil {
			return nil, err
		}
		return &p, nil
	}
	var list v1alpha1.HugrPlatformList
	if err := f.Reader.List(ctx, &list, client.InNamespace(f.Namespace)); err != nil {
		return nil, err
	}
	if len(list.Items) != 1 {
		return nil, fmt.Errorf("the namespace has %d platforms: name the console's with --console-platform",
			len(list.Items))
	}
	return &list.Items[0], nil
}

// Roots read the platform's CA bundle (spec.caBundle's ConfigMap) directly, once per resource version: the nodes'
// certificates are verified by it, as the front verifies them (spec 002).
type Roots struct {
	Platform func(context.Context) (*v1alpha1.HugrPlatform, error)
	Direct   client.Reader

	mu      sync.Mutex
	version string
	pool    *x509.CertPool
	read    time.Time
}

// rootsFresh is how long a read bundle serves before it is read again: a rotated CA reaches new channels within it.
const rootsFresh = 30 * time.Second

// Pool of the bundle's certificates.
func (r *Roots) Pool(ctx context.Context) (*x509.CertPool, error) {
	r.mu.Lock()
	if r.pool != nil && time.Since(r.read) < rootsFresh {
		pool := r.pool
		r.mu.Unlock()
		return pool, nil
	}
	r.mu.Unlock()
	platform, err := r.Platform(ctx)
	if err != nil {
		return nil, err
	}
	if platform.Spec.CABundle == nil {
		return nil, errors.New("the console verifies the nodes by the platform's CA bundle: set spec.caBundle")
	}
	ref := platform.Spec.CABundle.ConfigMapRef
	var cm corev1.ConfigMap
	if err := r.Direct.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: ref.Name}, &cm); err != nil {
		return nil, fmt.Errorf("the CA bundle %s: %w", ref.Name, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := cm.Name + "/" + ref.Key + "@" + cm.ResourceVersion
	if r.pool != nil && r.version == key {
		r.read = time.Now()
		return r.pool, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(cm.Data[ref.Key])) {
		return nil, fmt.Errorf("the CA bundle %s has no certificate under %s", ref.Name, ref.Key)
	}
	r.version, r.pool, r.read = key, pool, time.Now()
	return pool, nil
}
