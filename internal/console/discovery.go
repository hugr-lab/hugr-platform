package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// DiscoveryHandler answers a mount's discovery document (spec 009): tresor's duckdb-secrets discovery names its api,
// tresor-server's public_url - an address the nodes use and the page cannot reach (the console's CSP: its own
// origin). The console answers it under its prefix from the service's own document with only api replaced by its own
// base, the host the client asked, so every call that follows goes through the mount.
func (m *Mount) DiscoveryHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base, err := url.Parse("https://" + r.Host + m.Prefix)
		if r.Host == "" || err != nil || base.Host != r.Host || base.User != nil || base.Path != m.Prefix {
			problem(w, http.StatusBadRequest, "bad_host", "the request names no host")
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
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		target := *upstream
		target.Path = upstream.Path + m.Discovery
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			problem(w, http.StatusServiceUnavailable, "no_service", "this platform names no such service")
			return
		}
		req.Header.Set("Accept", "application/json")
		client := &http.Client{Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		res, err := client.Do(req)
		if err != nil {
			m.Log.Info("a mounted service's discovery not read", "prefix", m.Prefix, "error", err.Error())
			problem(w, http.StatusBadGateway, "service_unreachable", "the service does not answer")
			return
		}
		defer res.Body.Close()
		var doc map[string]json.RawMessage
		if res.StatusCode != http.StatusOK {
			m.Log.Info("a mounted service's discovery not read", "prefix", m.Prefix, "status", res.StatusCode)
			problem(w, http.StatusBadGateway, "service_unreachable",
				fmt.Sprintf("the service's discovery answered %d", res.StatusCode))
			return
		}
		if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&doc); err != nil || doc == nil {
			problem(w, http.StatusBadGateway, "service_unreachable", "the service's discovery is no JSON object")
			return
		}
		doc["api"], _ = json.Marshal(base.String())
		w.Header().Set("Cache-Control", "no-store")
		answer(w, doc)
	})
}
