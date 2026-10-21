package initjob

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Tresor speaks tresor's protocol (duckdb-secrets/1) as an administrator, with the Job's projected token.
type Tresor struct {
	base      string
	tokenFile string
	client    *http.Client
	api       string
}

// NewTresor is a client of tresor at base, verifying it with caFile (empty: the system roots).
func NewTresor(base, tokenFile, caFile string) (*Tresor, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("hugr-init: the CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("hugr-init: the CA bundle %s holds no certificate", caFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &Tresor{base: strings.TrimRight(base, "/"), tokenFile: tokenFile,
		client: &http.Client{Transport: transport, Timeout: 30 * time.Second,
			// the bearer is the Job's identity: never carried to wherever a redirect points
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// ErrExists is a secret that is there already (a 412 on If-None-Match: *).
var ErrExists = errors.New("the secret exists")

// Discover reads the service's API base from its discovery document.
func (t *Tresor) Discover(ctx context.Context) error {
	var doc struct {
		Protocol string `json:"protocol"`
		API      string `json:"api"`
	}
	if err := t.do(ctx, http.MethodGet, t.base+"/.well-known/duckdb-secrets", nil, nil, false, &doc); err != nil {
		return fmt.Errorf("tresor's discovery: %w", err)
	}
	if !strings.HasPrefix(doc.Protocol, "duckdb-secrets/") {
		return fmt.Errorf("tresor's discovery names no duckdb-secrets protocol")
	}
	u, err := url.Parse(doc.API)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" {
		return fmt.Errorf("tresor's discovery names an api that is not an https URL without userinfo")
	}
	t.api = strings.TrimRight(doc.API, "/")
	return nil
}

// Exists says whether a secret of that name is among those the caller (an administrator: all of them) may see.
func (t *Tresor) Exists(ctx context.Context, name string) (bool, error) {
	var descriptors []struct {
		Name string `json:"name"`
	}
	if err := t.do(ctx, http.MethodGet, t.api+"/v1/secrets", nil, nil, true, &descriptors); err != nil {
		return false, fmt.Errorf("tresor's secrets: %w", err)
	}
	for _, d := range descriptors {
		if d.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// Secret is what a PUT carries.
type Secret struct {
	Type       string         `json:"type"`
	Provider   string         `json:"provider"`
	Scope      []string       `json:"scope"`
	Params     map[string]any `json:"params"`
	RedactKeys []string       `json:"redact_keys"`
	Comment    string         `json:"comment,omitempty"`
}

// Create writes a secret only when the name is free: ErrExists otherwise.
func (t *Tresor) Create(ctx context.Context, name string, s Secret) error {
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	err = t.do(ctx, http.MethodPut, t.api+"/v1/secrets/"+url.PathEscape(name), body,
		map[string]string{"If-None-Match": "*"}, true, nil)
	var status statusError
	if errors.As(err, &status) && status.code == http.StatusPreconditionFailed {
		return ErrExists
	}
	if err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	return nil
}

// Grant gives principal use of the secret, under an id of the Job's (a replace of the same grant is the same).
func (t *Tresor) Grant(ctx context.Context, name, id, principal string) error {
	body, _ := json.Marshal(map[string]any{"principal": principal, "verbs": []string{"use"}})
	if err := t.do(ctx, http.MethodPut, t.api+"/v1/secrets/"+url.PathEscape(name)+"/grants/"+url.PathEscape(id),
		body, nil, true, nil); err != nil {
		return fmt.Errorf("granting %s to %s: %w", name, principal, err)
	}
	return nil
}

// statusError is a refusal: the status and the problem's type, never the body's text beyond its detail.
type statusError struct {
	code   int
	kind   string
	detail string
}

func (e statusError) Error() string {
	if e.kind == "" {
		return fmt.Sprintf("tresor answered %d", e.code)
	}
	if e.detail == "" {
		return fmt.Sprintf("tresor answered %d %s", e.code, e.kind)
	}
	return fmt.Sprintf("tresor answered %d %s: %s", e.code, e.kind, e.detail)
}

func (t *Tresor) do(ctx context.Context, method, target string, body []byte, headers map[string]string, auth bool,
	out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if auth {
		token, err := readToken(t.tokenFile)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var problem struct {
			Type   string `json:"type"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(data, &problem)
		if len(problem.Detail) > 300 {
			problem.Detail = problem.Detail[:300]
		}
		return statusError{code: resp.StatusCode, kind: problem.Type, detail: problem.Detail}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("an answer that is not the protocol's: %w", err)
		}
	}
	return nil
}

// readToken reads the projected token at each request (kubelet renews it); never logged.
func readToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("hugr-init: the projected token cannot be read")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", fmt.Errorf("hugr-init: the projected token file holds no token")
	}
	return token, nil
}
