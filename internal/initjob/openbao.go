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
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// OpenBaoFile is the configuration of the internal key store's phase (spec 005 step 2: the umbrella's keys.mode
// internal): what OpenBao must hold for the services, never a credential.
type OpenBaoFile struct {
	OpenBao OpenBaoConfig `json:"openbao"`
}

// OpenBaoConfig is OpenBao's address and what the Job makes there.
type OpenBaoConfig struct {
	URL    string `json:"url"`
	CAFile string `json:"caFile,omitempty"`
	// TokenFile is the Job's projected ServiceAccount token, bound to Audience: its login on every run after the first.
	TokenFile string `json:"tokenFile"`
	// Namespace is OpenBao's and the Job's: the recovery key's Secret is there.
	Namespace      string `json:"namespace"`
	RecoverySecret string `json:"recoverySecret"`
	TransitMount   string `json:"transitMount"`
	AuthMount      string `json:"authMount"`
	Audience       string `json:"audience"`
	// Init is the Job's own role and its ServiceAccount (in Namespace): what a later run logs in as.
	Init     OpenBaoBinding   `json:"init"`
	Services []OpenBaoService `json:"services"`
}

// OpenBaoBinding is a Kubernetes-auth role bound to one ServiceAccount.
type OpenBaoBinding struct {
	Role           string `json:"role"`
	ServiceAccount string `json:"serviceAccount"`
}

// OpenBaoService is a service's role, its policy (of the same name) and the Transit key it uses.
type OpenBaoService struct {
	Name string `json:"name"`
	// Use is what the key is for: sign (kista's channel, RSA - a DuckDB extension's signature) or kek (tresor's data
	// keys: encrypt, decrypt and the root's HMAC).
	Use            string `json:"use"`
	Key            string `json:"key"`
	ServiceAccount string `json:"serviceAccount"`
	Namespace      string `json:"namespace"`
}

var baoName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// LoadOpenBao reads and checks the phase's configuration.
func LoadOpenBao(path string) (*OpenBaoConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("hugr-init: the configuration: %w", err)
	}
	var f OpenBaoFile
	if err := yaml.UnmarshalStrict(raw, &f); err != nil {
		return nil, fmt.Errorf("hugr-init: the configuration: %w", err)
	}
	return &f.OpenBao, f.OpenBao.Validate()
}

// Validate refuses what the phase cannot run on, naming the field.
func (c *OpenBaoConfig) Validate() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if u, err := url.Parse(c.URL); err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" {
		add("openbao.url is an https URL without userinfo")
	}
	if c.TokenFile == "" {
		add("openbao.tokenFile is required")
	}
	for field, v := range map[string]string{"namespace": c.Namespace, "recoverySecret": c.RecoverySecret,
		"transitMount": c.TransitMount, "authMount": c.AuthMount, "audience": c.Audience, "init.role": c.Init.Role,
		"init.serviceAccount": c.Init.ServiceAccount} {
		if !baoName.MatchString(v) {
			add("openbao.%s is a name ([a-z0-9-])", field)
		}
	}
	if len(c.Services) == 0 {
		add("openbao.services names no service")
	}
	seen := map[string]bool{c.Init.Role: true}
	for _, s := range c.Services {
		if !baoName.MatchString(s.Name) || !baoName.MatchString(s.Key) || !baoName.MatchString(s.ServiceAccount) ||
			!baoName.MatchString(s.Namespace) {
			add("openbao.services: %q's name, key, serviceAccount and namespace are names", s.Name)
		}
		if s.Use != "sign" && s.Use != "kek" {
			add("openbao.services: %q's use is sign or kek", s.Name)
		}
		if seen[s.Name] {
			add("openbao.services: %q is named twice (or is the Job's own role)", s.Name)
		}
		seen[s.Name] = true
	}
	if len(problems) > 0 {
		return fmt.Errorf("hugr-init: the configuration: %s", strings.Join(problems, "; "))
	}
	return nil
}

// keyType is the Transit key a use needs.
func keyType(use string) string {
	if use == "sign" {
		return "rsa-2048"
	}
	return "aes256-gcm96"
}

// servicePolicy is what a service may do with its key, and nothing else (tresor-server's documented KEK policy;
// kista's signer).
func servicePolicy(mount string, s OpenBaoService) string {
	k := mount + "/keys/" + s.Key
	var b strings.Builder
	fmt.Fprintf(&b, "path %q {\n  capabilities = [\"read\"]\n}\n", k)
	ops := []string{mount + "/sign/" + s.Key + "/sha2-256"}
	if s.Use == "kek" {
		ops = []string{mount + "/encrypt/" + s.Key, mount + "/decrypt/" + s.Key, mount + "/hmac/" + s.Key + "/sha2-256"}
	}
	for _, p := range ops {
		fmt.Fprintf(&b, "path %q {\n  capabilities = [\"update\"]\n}\n", p)
	}
	return b.String()
}

// initPolicy is the Job's own: what the configuration needs, each path by name - it may write the services' policies
// and roles, never another policy or role, and it reads no key's material.
func initPolicy(c *OpenBaoConfig) string {
	paths := map[string][]string{
		"sys/mounts":                                   {"read"},
		"sys/mounts/" + c.TransitMount:                 {"create", "read", "update"},
		"sys/auth":                                     {"read"},
		"sys/auth/" + c.AuthMount:                      {"create", "read", "update", "sudo"},
		"auth/" + c.AuthMount + "/config":              {"create", "read", "update"},
		"sys/policies/acl/" + c.Init.Role:              {"create", "read", "update"},
		"auth/" + c.AuthMount + "/role/" + c.Init.Role: {"create", "read", "update"},
	}
	for _, s := range c.Services {
		paths[c.TransitMount+"/keys/"+s.Key] = []string{"create", "read", "update"}
		paths["sys/policies/acl/"+s.Name] = []string{"create", "read", "update"}
		paths["auth/"+c.AuthMount+"/role/"+s.Name] = []string{"create", "read", "update"}
	}
	names := make([]string, 0, len(paths))
	for p := range paths {
		names = append(names, p)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, p := range names {
		quoted := make([]string, len(paths[p]))
		for i, c := range paths[p] {
			quoted[i] = fmt.Sprintf("%q", c)
		}
		fmt.Fprintf(&b, "path %q {\n  capabilities = [%s]\n}\n", p, strings.Join(quoted, ", "))
	}
	return b.String()
}

// RecoveryStore keeps OpenBao's recovery key: the break-glass of an instance whose tokens are all gone.
type RecoveryStore interface {
	// Present says whether the key is kept.
	Present(ctx context.Context) (bool, error)
	// Put keeps the key, replacing one of an OpenBao made before (its database dropped).
	Put(ctx context.Context, key string) error
}

// RunOpenBao is the phase: the first run initialises OpenBao (static auto-unseal: one recovery key, kept in a Secret
// of its namespace) and configures it with the root token it got, which it then revokes; every later run logs in as
// its own role and configures again - what the release asks for is made, nothing is removed. No token outlives the
// run.
func RunOpenBao(ctx context.Context, c *OpenBaoConfig, bao *OpenBao, recovery RecoveryStore, log *slog.Logger) error {
	initialized, err := bao.WaitInitialized(ctx)
	if err != nil {
		return err
	}
	var token string
	if !initialized {
		root, key, err := bao.Initialize(ctx)
		if err != nil {
			return err
		}
		token = root
		// the root token dies with this run whatever happens next
		defer func() { _ = bao.RevokeSelf(context.WithoutCancel(ctx), root) }()
		// the node just initialised is active only once it has taken the HA lock: a write before that is refused
		if err := bao.WaitActive(ctx); err != nil {
			return err
		}
		if err := recovery.Put(ctx, key); err != nil {
			return fmt.Errorf("hugr-init: OpenBao is initialised, but its recovery key could not be kept in %s/%s: %w"+
				" - without it there is no break-glass; drop OpenBao's database and run again", c.Namespace,
				c.RecoverySecret, err)
		}
		log.Info("OpenBao initialised; its recovery key is kept", "secret", c.Namespace+"/"+c.RecoverySecret)
	} else {
		jwt, err := readToken(c.TokenFile)
		if err != nil {
			return err
		}
		if token, err = bao.Login(ctx, c.AuthMount, c.Init.Role, jwt); err != nil {
			return fmt.Errorf("hugr-init: OpenBao is initialised, and the Job's login (role %s) failed: %w", c.Init.Role,
				err)
		}
		defer func() { _ = bao.RevokeSelf(context.WithoutCancel(ctx), token) }()
		if present, err := recovery.Present(ctx); err == nil && !present {
			log.Warn("OpenBao's recovery key is not kept: its break-glass needs it",
				"secret", c.Namespace+"/"+c.RecoverySecret)
		}
	}
	if err := configureOpenBao(ctx, c, bao, token); err != nil {
		return err
	}
	log.Info("OpenBao holds the services' keys, policies and roles", "keys", len(c.Services))
	return nil
}

func configureOpenBao(ctx context.Context, c *OpenBaoConfig, bao *OpenBao, token string) error {
	// the Job's own policy and its login first: a release that adds a service widens the policy, which OpenBao applies
	// to the tokens holding it at once; and a first run that stops after this still leaves the next one a login
	if err := bao.write(ctx, token, http.MethodPut, "sys/policies/acl/"+c.Init.Role,
		map[string]any{"policy": initPolicy(c)}); err != nil {
		return fmt.Errorf("hugr-init: the Job's own policy %s: %w", c.Init.Role, err)
	}
	auths, err := bao.list(ctx, token, "sys/auth")
	if err != nil {
		return fmt.Errorf("hugr-init: OpenBao's auth methods: %w", err)
	}
	if !auths[c.AuthMount+"/"] {
		if err := bao.write(ctx, token, http.MethodPost, "sys/auth/"+c.AuthMount,
			map[string]any{"type": "kubernetes"}); err != nil {
			return fmt.Errorf("hugr-init: enabling Kubernetes auth at %s: %w", c.AuthMount, err)
		}
	}
	// OpenBao reviews a token with its own ServiceAccount and the cluster's CA, both read in its pod
	if err := bao.write(ctx, token, http.MethodPost, "auth/"+c.AuthMount+"/config",
		map[string]any{"kubernetes_host": "https://kubernetes.default.svc"}); err != nil {
		return fmt.Errorf("hugr-init: configuring Kubernetes auth: %w", err)
	}
	roles := []struct {
		name, sa, ns, ttl string
	}{{c.Init.Role, c.Init.ServiceAccount, c.Namespace, "15m"}}
	for _, s := range c.Services {
		roles = append(roles, struct{ name, sa, ns, ttl string }{s.Name, s.ServiceAccount, s.Namespace, "1h"})
	}
	putRole := func(name, sa, ns, ttl string) error {
		if err := bao.write(ctx, token, http.MethodPost, "auth/"+c.AuthMount+"/role/"+name, map[string]any{
			"bound_service_account_names": []string{sa}, "bound_service_account_namespaces": []string{ns},
			"audience": c.Audience, "token_policies": []string{name}, "token_ttl": ttl,
		}); err != nil {
			return fmt.Errorf("hugr-init: the role %s: %w", name, err)
		}
		return nil
	}
	if err := putRole(roles[0].name, roles[0].sa, roles[0].ns, roles[0].ttl); err != nil {
		return err
	}
	mounts, err := bao.list(ctx, token, "sys/mounts")
	if err != nil {
		return fmt.Errorf("hugr-init: OpenBao's mounts: %w", err)
	}
	if !mounts[c.TransitMount+"/"] {
		if err := bao.write(ctx, token, http.MethodPost, "sys/mounts/"+c.TransitMount,
			map[string]any{"type": "transit"}); err != nil {
			return fmt.Errorf("hugr-init: mounting Transit at %s: %w", c.TransitMount, err)
		}
	}
	for _, s := range c.Services {
		want := keyType(s.Use)
		have, err := bao.keyType(ctx, token, c.TransitMount, s.Key)
		if err != nil {
			return fmt.Errorf("hugr-init: the key %s: %w", s.Key, err)
		}
		switch {
		case have == "":
			if err := bao.write(ctx, token, http.MethodPost, c.TransitMount+"/keys/"+s.Key,
				map[string]any{"type": want}); err != nil {
				return fmt.Errorf("hugr-init: making the key %s: %w", s.Key, err)
			}
		case have != want:
			return fmt.Errorf("hugr-init: the key %s is %s, and %s's use needs %s: never changed by the Job", s.Key, have,
				s.Name, want)
		}
	}
	for _, s := range c.Services {
		if err := bao.write(ctx, token, http.MethodPut, "sys/policies/acl/"+s.Name,
			map[string]any{"policy": servicePolicy(c.TransitMount, s)}); err != nil {
			return fmt.Errorf("hugr-init: the policy %s: %w", s.Name, err)
		}
	}
	for _, r := range roles[1:] {
		if err := putRole(r.name, r.sa, r.ns, r.ttl); err != nil {
			return err
		}
	}
	return nil
}

// OpenBao speaks OpenBao's HTTP API: the few calls the phase makes. A token is a request's header, never in an
// error or a log.
type OpenBao struct {
	base   string
	client *http.Client
	// Wait is how long WaitInitialized waits for OpenBao to answer at all (a pod still starting), asking every Poll
	Wait time.Duration
	Poll time.Duration
}

// NewOpenBao is a client of OpenBao at base, verifying it with caFile (empty: the system roots).
func NewOpenBao(base, caFile string) (*OpenBao, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("hugr-init: OpenBao's CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("hugr-init: OpenBao's CA %s holds no certificate", caFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &OpenBao{base: strings.TrimRight(base, "/"), Wait: 5 * time.Minute, Poll: 2 * time.Second,
		client: &http.Client{Transport: transport, Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// WaitInitialized waits until OpenBao answers, then says whether it is initialised.
func (b *OpenBao) WaitInitialized(ctx context.Context) (bool, error) {
	deadline := time.Now().Add(b.Wait)
	for {
		var out struct {
			Initialized bool `json:"initialized"`
		}
		_, err := b.do(ctx, "", http.MethodGet, "sys/init", nil, &out)
		if err == nil {
			return out.Initialized, nil
		}
		if time.Now().After(deadline) {
			return false, fmt.Errorf("hugr-init: OpenBao did not answer within %s: %w", b.Wait, err)
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(b.Poll):
		}
	}
}

// Initialize initialises OpenBao under its auto-unseal seal: one recovery key share, the root token.
func (b *OpenBao) Initialize(ctx context.Context) (root, recoveryKey string, err error) {
	var out struct {
		RootToken          string   `json:"root_token"`
		RecoveryKeysBase64 []string `json:"recovery_keys_base64"`
	}
	if _, err := b.do(ctx, "", http.MethodPut, "sys/init",
		map[string]any{"recovery_shares": 1, "recovery_threshold": 1}, &out); err != nil {
		return "", "", fmt.Errorf("hugr-init: initialising OpenBao: %w", err)
	}
	if out.RootToken == "" || len(out.RecoveryKeysBase64) != 1 {
		return "", "", errors.New("hugr-init: OpenBao's init answered no root token or not one recovery key")
	}
	return out.RootToken, out.RecoveryKeysBase64[0], nil
}

// WaitActive waits until the cluster has an active node: the one OpenBao just initialised takes the HA lock a moment
// after, and until then a write answers "local node not active but active cluster node not found".
func (b *OpenBao) WaitActive(ctx context.Context) error {
	deadline := time.Now().Add(b.Wait)
	for {
		var out struct {
			LeaderAddress string `json:"leader_address"`
		}
		if _, err := b.do(ctx, "", http.MethodGet, "sys/leader", nil, &out); err == nil && out.LeaderAddress != "" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("hugr-init: OpenBao has no active node within %s", b.Wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.Poll):
		}
	}
}

// Login is the Job's Kubernetes-auth login: a token of its own role.
func (b *OpenBao) Login(ctx context.Context, mount, role, jwt string) (string, error) {
	var out struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if _, err := b.do(ctx, "", http.MethodPost, "auth/"+mount+"/login",
		map[string]any{"role": role, "jwt": jwt}, &out); err != nil {
		return "", err
	}
	if out.Auth.ClientToken == "" {
		return "", errors.New("the login answered no token")
	}
	return out.Auth.ClientToken, nil
}

// RevokeSelf ends the token.
func (b *OpenBao) RevokeSelf(ctx context.Context, token string) error {
	_, err := b.do(ctx, token, http.MethodPost, "auth/token/revoke-self", map[string]any{}, nil)
	return err
}

// write is a configuration call, each idempotent: one answered 5xx (a node handing its HA lock over, a standby that
// lost the active) is asked again, a few times
func (b *OpenBao) write(ctx context.Context, token, method, path string, body any) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		var status int
		if status, err = b.do(ctx, token, method, path, body, nil); err == nil || status < 500 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.Poll):
		}
	}
	return err
}

// list is the set of a listing's keys (sys/mounts, sys/auth: "transit/", "kubernetes/").
func (b *OpenBao) list(ctx context.Context, token, path string) (map[string]bool, error) {
	var out struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if _, err := b.do(ctx, token, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for k := range out.Data {
		set[k] = true
	}
	return set, nil
}

// keyType is a Transit key's type, "" when there is no such key.
func (b *OpenBao) keyType(ctx context.Context, token, mount, key string) (string, error) {
	var out struct {
		Data struct {
			Type string `json:"type"`
		} `json:"data"`
	}
	status, err := b.do(ctx, token, http.MethodGet, mount+"/keys/"+key, nil, &out)
	if status == http.StatusNotFound {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return out.Data.Type, nil
}

// do is one request; a refusal is its status and OpenBao's errors (bounded), never a header or a token.
func (b *OpenBao) do(ctx context.Context, token, method, path string, body, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.base+"/v1/"+path, reader)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var problem struct {
			Errors []string `json:"errors"`
		}
		_ = json.Unmarshal(data, &problem)
		text := strings.Join(problem.Errors, "; ")
		if len(text) > 300 {
			text = text[:300]
		}
		return resp.StatusCode, fmt.Errorf("OpenBao answered %d to %s %s: %s", resp.StatusCode, method, path, text)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("an answer that is not OpenBao's: %w", err)
		}
	}
	return resp.StatusCode, nil
}
