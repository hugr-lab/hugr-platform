package console

import (
	"context"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestMount(t *testing.T) {
	var seen *http.Request
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Clone(context.Background())
		switch r.URL.Path {
		case "/v1/redirect":
			http.Redirect(w, r, "https://elsewhere.example/", http.StatusFound)
		default:
			w.Header().Set("Set-Cookie", "tresor=1")
			_, _ = io.WriteString(w, "answered "+r.URL.Path)
		}
	}))
	defer upstream.Close()
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	target, _ := url.Parse(upstream.URL)
	m := &Mount{Prefix: "/tresor", Paths: tresorPaths,
		Upstream: func(context.Context) (*url.URL, error) { return target, nil },
		Roots:    func(context.Context) (*x509.CertPool, error) { return roots, nil }}
	h := m.Handler()
	do := func(path string, header ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		for i := 0; i+1 < len(header); i += 2 {
			r.Header.Set(header[i], header[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	w := do("/tresor/v1/whoami", "Authorization", "Bearer tresor-token", "Cookie", "console=x", "X-Forwarded-For", "1.2.3.4")
	if w.Code != 200 || w.Body.String() != "answered /v1/whoami" {
		t.Fatalf("an allowed path, its prefix taken off: %d %q", w.Code, w.Body)
	}
	if seen.Header.Get("Authorization") != "Bearer tresor-token" {
		t.Error("the person's bearer for the service reaches it as it came")
	}
	if seen.Header.Get("Cookie") != "" || seen.Header.Get("X-Forwarded-For") != "" {
		t.Errorf("the console origin's cookies and the client's address stay here: %v", seen.Header)
	}
	if seen.Host != target.Host {
		t.Errorf("the service's own host: %q", seen.Host)
	}
	if w.Header().Get("Set-Cookie") != "" {
		t.Error("the service's cookies are not the console origin's")
	}
	if w := do("/tresor/ui/mfe/tresor.js"); w.Code != 200 {
		t.Errorf("the module: %d", w.Code)
	}

	seen = nil
	for _, path := range []string{"/tresor/metrics", "/tresor/", "/tresor/v1/../metrics", "/tresor/ui/other.js",
		"/tresor/v1/..", "/tresorx/v1/whoami"} {
		if w := do(path); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
	if seen != nil {
		t.Errorf("a refused path reached the service: %s", seen.URL.Path)
	}

	w = do("/tresor/v1/redirect")
	if w.Code != http.StatusFound || !strings.Contains(w.Header().Get("Location"), "elsewhere") {
		t.Errorf("a redirect is the browser's to see, never followed here: %d", w.Code)
	}

	untrusted := &Mount{Prefix: "/tresor", Paths: tresorPaths,
		Upstream: func(context.Context) (*url.URL, error) { return target, nil },
		Roots:    func(context.Context) (*x509.CertPool, error) { return x509.NewCertPool(), nil }}
	r := httptest.NewRequest("GET", "/tresor/v1/whoami", nil)
	rec := httptest.NewRecorder()
	untrusted.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("a service whose certificate the bundle does not sign: %d", rec.Code)
	}
}

func TestUpstreams(t *testing.T) {
	h := newHarness(t, observeInfo)
	tresor, err := TresorUpstream(h.server.Platform)(context.Background())
	if err != nil || tresor.Scheme != "https" || tresor.Host != "" {
		t.Fatalf("a platform naming no tresor host has no tresor origin: %v %v", tresor, err)
	}
	m := TresorMount(h.server.Platform, nil)
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/tresor/v1/whoami", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("no host, nothing proxied: %d", w.Code)
	}
	kista, err := KistaUpstream(h.server.Platform)(context.Background())
	if err != nil || kista.String() != "https://kista.example.org/ext" {
		t.Errorf("kista's origin from the repository prefix: %v %v", kista, err)
	}
}

func TestKistaPrefix(t *testing.T) {
	for prefix, want := range map[string][2]string{
		"https://kista.example.com/acme/prod":         {"https://kista.example.com", "acme"},
		"https://example.com/kista/acme/prod/":        {"https://example.com/kista", "acme"},
		"https://extensions.example.com/a/b/acme/dev": {"https://extensions.example.com/a/b", "acme"},
	} {
		base, tenant, err := kistaPrefix(prefix)
		if err != nil || base.String() != want[0] || tenant != want[1] {
			t.Errorf("%s: %v %q %v", prefix, base, tenant, err)
		}
	}
	for _, bad := range []string{"https://kista.example.com/", "https://kista.example.com/prod", "::"} {
		if _, _, err := kistaPrefix(bad); err == nil {
			t.Errorf("%s names no tenant and channel", bad)
		}
	}
}
