package console

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestDiscovery: tresor's discovery through the console names the console's own /tresor as its api, every other
// field the service's own; a service down or answering no document is a 502, a platform naming none a 503.
func TestDiscovery(t *testing.T) {
	status := http.StatusOK
	body := `{"protocol":"duckdb-secrets/1","api":"https://tresor.hugr-tresor.svc:443","capabilities":{"write":true},` +
		`"issuers":[{"issuer":"https://idp.example/realms/hugr","audience":"duckdb-secrets","client_id":"tresor-people"}]}`
	var asked string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	target, _ := url.Parse(upstream.URL)
	named := target
	m := &Mount{Prefix: "/tresor", Paths: tresorPaths, Discovery: "/.well-known/duckdb-secrets",
		Upstream: func(context.Context) (*url.URL, error) { return named, nil },
		Roots:    func(context.Context) (*x509.CertPool, error) { return roots, nil }}
	s := &Server{Mounts: []*Mount{m}}
	h := s.Handler()
	do := func(host string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/tresor/.well-known/duckdb-secrets", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	w := do("console.example.org")
	if w.Code != 200 || asked != "/.well-known/duckdb-secrets" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("the service's discovery read: %d %q %q", w.Code, asked, w.Body)
	}
	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["api"] != "https://console.example.org/tresor" {
		t.Errorf("api is the console's /tresor: %v", doc["api"])
	}
	var want map[string]any
	_ = json.Unmarshal([]byte(body), &want)
	for k, v := range want {
		if k == "api" {
			continue
		}
		got, _ := json.Marshal(doc[k])
		exp, _ := json.Marshal(v)
		if string(got) != string(exp) {
			t.Errorf("%s is the service's own: %s, not %s", k, got, exp)
		}
	}
	if len(doc) != len(want) {
		t.Errorf("nothing added: %v", doc)
	}
	if w := do("127.0.0.1:18644"); !jsonHas(w, "api", "https://127.0.0.1:18644/tresor") {
		t.Errorf("the host and port the client asked: %s", w.Body)
	}
	for _, host := range []string{"", "a@b", "x/y"} {
		if w := do(host); w.Code != http.StatusBadRequest {
			t.Errorf("host %q: %d", host, w.Code)
		}
	}
	// a POST is the mount's, which has no such path
	r := httptest.NewRequest("POST", "/tresor/.well-known/duckdb-secrets", nil)
	r.Host = "console.example.org"
	pw := httptest.NewRecorder()
	h.ServeHTTP(pw, r)
	if pw.Code != http.StatusNotFound {
		t.Errorf("a POST: %d", pw.Code)
	}

	status = http.StatusInternalServerError
	if w := do("console.example.org"); w.Code != http.StatusBadGateway {
		t.Errorf("the service failing: %d", w.Code)
	}
	status, body = http.StatusOK, `["not an object"]`
	if w := do("console.example.org"); w.Code != http.StatusBadGateway {
		t.Errorf("no document: %d", w.Code)
	}
	upstream.Close()
	status, body = http.StatusOK, `{}`
	if w := do("console.example.org"); w.Code != http.StatusBadGateway {
		t.Errorf("the service down: %d", w.Code)
	}
	named = nil
	if w := do("console.example.org"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("no service named: %d", w.Code)
	}
}

func jsonHas(w *httptest.ResponseRecorder, key, value string) bool {
	var doc map[string]any
	return w.Code == 200 && json.Unmarshal(w.Body.Bytes(), &doc) == nil && doc[key] == value
}
