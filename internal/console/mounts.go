package console

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
)

// The mounted consoles' paths the shell proxies (spec 003): their micro-frontend modules and the APIs those call -
// nothing else of a service is reached through the console's origin.
var (
	tresorPaths = []string{"/ui/mfe/", "/v1/", "/admin/v1/"}
	kistaPaths  = []string{"/ui/mfe/", "/api/v1/"}
)

// Mount is a service a mounted console talks to, proxied under the console's origin.
type Mount struct {
	// Prefix the console serves it under: /tresor or /kista.
	Prefix string
	// Upstream is the service's https origin, from the platform (nil: the platform names none).
	Upstream func(ctx context.Context) (*url.URL, error)
	// Paths of the service that may be reached.
	Paths []string
	// Roots verify the service's certificate (the platform's CA bundle; nil: the system's).
	Roots func(ctx context.Context) (*x509.CertPool, error)
	// Unbounded: the service answers when its work is done (the quack door: a statement's first batch takes as long as
	// the statement), so no deadline for its answer's headers.
	Unbounded bool
	// Log takes a failed forward (the service down, its certificate refused); the zero Logger drops it.
	Log logr.Logger

	mu        sync.Mutex
	transport *http.Transport
	roots     *x509.CertPool
}

// client is the mount's transport, kept while the bundle is the same pool: a module's dozens of assets share its
// connections.
func (m *Mount) client(ctx context.Context) (*http.Transport, error) {
	var roots *x509.CertPool
	if m.Roots != nil {
		r, err := m.Roots(ctx)
		if err != nil {
			return nil, err
		}
		roots = r
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.transport != nil && m.roots == roots {
		return m.transport, nil
	}
	if m.transport != nil {
		m.transport.CloseIdleConnections()
	}
	m.transport = &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: m.responseTimeout(),
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   16,
		ForceAttemptHTTP2:     true,
	}
	if roots != nil {
		m.transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	}
	m.roots = roots
	return m.transport, nil
}

// TresorUpstream is tresor-server's origin: spec.tresor.host is its host:port.
func TresorUpstream(platform func(context.Context) (*v1alpha1.HugrPlatform, error)) func(context.Context) (*url.URL, error) {
	return func(ctx context.Context) (*url.URL, error) {
		p, err := platform(ctx)
		if err != nil {
			return nil, err
		}
		return &url.URL{Scheme: "https", Host: p.Spec.Tresor.Host}, nil
	}
}

// KistaUpstream is kista's public URL: the repository prefix is <public_url>/<tenant>/<channel> (kista's docs), and
// the public URL may carry a path of its own.
func KistaUpstream(platform func(context.Context) (*v1alpha1.HugrPlatform, error)) func(context.Context) (*url.URL, error) {
	return func(ctx context.Context) (*url.URL, error) {
		p, err := platform(ctx)
		if err != nil {
			return nil, err
		}
		base, _, err := kistaPrefix(p.Spec.Repository.Prefix)
		return base, err
	}
}

// kistaPrefix splits a channel's prefix into kista's public URL and the tenant.
func kistaPrefix(prefix string) (*url.URL, string, error) {
	u, err := url.Parse(prefix)
	if err != nil {
		return nil, "", err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-1] == "" || parts[len(parts)-2] == "" {
		return nil, "", errors.New("the repository prefix is no <kista>/<tenant>/<channel>")
	}
	base := &url.URL{Scheme: "https", Host: u.Host, Path: strings.Join(parts[:len(parts)-2], "/")}
	if base.Path != "" {
		base.Path = "/" + base.Path
	}
	return base, parts[len(parts)-2], nil
}

// Handler of the mount: the request as it came - the person's bearer for that service included - minus the console
// origin's cookies and anything that says where it came from; no redirect followed.
func (m *Mount) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, m.Prefix)
		if !strings.HasPrefix(rest, "/") || strings.Contains(rest, "/../") || strings.HasSuffix(rest, "/..") || !m.allowed(rest) {
			problem(w, http.StatusNotFound, "not_found", "no such path of the service")
			return
		}
		upstream, err := m.Upstream(r.Context())
		if err != nil || upstream == nil || upstream.Host == "" {
			problem(w, http.StatusServiceUnavailable, "no_service", "this platform names no such service")
			return
		}
		transport, err := m.client(r.Context())
		if err != nil {
			problem(w, http.StatusServiceUnavailable, "no_service", "the service's certificate cannot be verified")
			return
		}
		proxy := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Scheme = upstream.Scheme
				pr.Out.URL.Host = upstream.Host
				pr.Out.URL.Path = upstream.Path + rest
				pr.Out.URL.RawPath = ""
				pr.Out.Host = upstream.Host
				pr.Out.Header.Del("Cookie")
				pr.Out.Header.Del("Forwarded")
				pr.Out.Header.Del("X-Forwarded-For")
				pr.Out.Header.Del("X-Forwarded-Host")
				pr.Out.Header.Del("X-Forwarded-Proto")
			},
			Transport: transport,
			ModifyResponse: func(res *http.Response) error {
				res.Header.Del("Set-Cookie") // the service's cookies are not the console's origin's
				// nor its whole origin: no service worker scoped past the service's own paths, no clearing of the
				// console's storage
				res.Header.Del("Service-Worker-Allowed")
				res.Header.Del("Clear-Site-Data")
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				m.Log.Info("a mounted service not reached", "prefix", m.Prefix, "error", err.Error())
				problem(w, http.StatusBadGateway, "service_unreachable", "the service does not answer")
			},
		}
		// the console's own policy on the page; a module's answer keeps the service's headers but these two
		w.Header().Set("X-Content-Type-Options", "nosniff")
		proxy.ServeHTTP(w, r)
	})
}

func (m *Mount) allowed(path string) bool {
	for _, p := range m.Paths {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func (m *Mount) responseTimeout() time.Duration {
	if m.Unbounded {
		return 0
	}
	return 60 * time.Second
}

// TresorMount and KistaMount with the platform's paths.
func TresorMount(platform func(context.Context) (*v1alpha1.HugrPlatform, error),
	roots func(context.Context) (*x509.CertPool, error)) *Mount {
	return &Mount{Prefix: "/tresor", Upstream: TresorUpstream(platform), Paths: tresorPaths, Roots: roots}
}

// KistaMount is kista's.
func KistaMount(platform func(context.Context) (*v1alpha1.HugrPlatform, error),
	roots func(context.Context) (*x509.CertPool, error)) *Mount {
	return &Mount{Prefix: "/kista", Upstream: KistaUpstream(platform), Paths: kistaPaths, Roots: roots}
}
