package initjob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBao is OpenBao as far as the phase sees it: its state in memory, every call but init and login checked against a
// live token, and an existing mount refused as OpenBao refuses it.
type fakeBao struct {
	mu          sync.Mutex
	initialized bool
	tokens      map[string]bool
	mounts      map[string]bool
	auths       map[string]bool
	keys        map[string]string
	policies    map[string]string
	roles       map[string]map[string]any
	config      map[string]any
	jwt         string // the login's accepted token
	calls       []string
	// notActive: the writes after init that answer as a node not yet holding the HA lock
	notActive int
	// leaderless: the sys/leader reads after init that still name no active node
	leaderless int
	// unavailable: the logins answered 503 (an OpenBao still unsealing after a restart)
	unavailable int
}

func newFakeBao() *fakeBao {
	return &fakeBao{tokens: map[string]bool{}, mounts: map[string]bool{"sys/": true}, auths: map[string]bool{"token/": true},
		keys: map[string]string{}, policies: map[string]string{}, roles: map[string]map[string]any{}, jwt: "the-jwt"}
}

func (f *fakeBao) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v1/")
	f.calls = append(f.calls, r.Method+" "+path)
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	answer := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if v != nil {
			_ = json.NewEncoder(w).Encode(v)
		}
	}
	refuse := func(code int, text string) { answer(code, map[string]any{"errors": []string{text}}) }
	switch {
	case path == "sys/init" && r.Method == http.MethodGet:
		answer(200, map[string]any{"initialized": f.initialized})
		return
	case path == "sys/init" && r.Method == http.MethodPut:
		if f.initialized {
			refuse(400, "already initialized")
			return
		}
		f.initialized = true
		f.tokens["root-token"] = true
		answer(200, map[string]any{"root_token": "root-token", "recovery_keys_base64": []string{"recovery-key"},
			"keys": []string{}, "keys_base64": []string{}})
		return
	case path == "sys/leader":
		if !f.initialized || f.leaderless > 0 {
			f.leaderless--
			answer(200, map[string]any{"ha_enabled": true, "leader_address": ""})
			return
		}
		answer(200, map[string]any{"ha_enabled": true, "is_self": true, "leader_address": "https://openbao:8200"})
		return
	case strings.HasPrefix(path, "auth/") && strings.HasSuffix(path, "/login") && f.unavailable > 0:
		f.unavailable--
		refuse(503, "Vault is sealed")
		return
	case strings.HasPrefix(path, "auth/") && strings.HasSuffix(path, "/login"):
		if _, ok := f.roles[strings.TrimSuffix(strings.TrimPrefix(path, "auth/"), "/login")+"/"+body["role"].(string)]; !ok ||
			body["jwt"] != f.jwt {
			refuse(403, "permission denied")
			return
		}
		f.tokens["login-token"] = true
		answer(200, map[string]any{"auth": map[string]any{"client_token": "login-token"}})
		return
	}
	token := r.Header.Get("X-Vault-Token")
	if !f.tokens[token] {
		refuse(403, "permission denied")
		return
	}
	if r.Method != http.MethodGet && f.notActive > 0 {
		f.notActive--
		refuse(500, "local node not active but active cluster node not found")
		return
	}
	switch {
	case path == "auth/token/revoke-self":
		delete(f.tokens, token)
		answer(204, nil)
	case path == "sys/mounts" && r.Method == http.MethodGet:
		answer(200, map[string]any{"data": setOf(f.mounts)})
	case strings.HasPrefix(path, "sys/mounts/"):
		name := strings.TrimPrefix(path, "sys/mounts/") + "/"
		if f.mounts[name] {
			refuse(400, "path is already in use at "+name)
			return
		}
		f.mounts[name] = true
		answer(204, nil)
	case path == "sys/auth" && r.Method == http.MethodGet:
		answer(200, map[string]any{"data": setOf(f.auths)})
	case strings.HasPrefix(path, "sys/auth/"):
		name := strings.TrimPrefix(path, "sys/auth/") + "/"
		if f.auths[name] {
			refuse(400, "path is already in use at "+name)
			return
		}
		f.auths[name] = true
		answer(204, nil)
	case strings.HasPrefix(path, "sys/policies/acl/"):
		f.policies[strings.TrimPrefix(path, "sys/policies/acl/")] = body["policy"].(string)
		answer(204, nil)
	case strings.Contains(path, "/keys/"):
		mount, key, _ := strings.Cut(path, "/keys/")
		if !f.mounts[mount+"/"] {
			refuse(404, "no handler for route")
			return
		}
		if r.Method == http.MethodGet {
			if t, ok := f.keys[key]; ok {
				answer(200, map[string]any{"data": map[string]any{"type": t}})
			} else {
				answer(404, map[string]any{"errors": []string{}})
			}
			return
		}
		f.keys[key] = body["type"].(string)
		answer(200, map[string]any{"data": map[string]any{"type": f.keys[key]}})
	case strings.HasSuffix(path, "/config"):
		f.config = body
		answer(204, nil)
	case strings.Contains(path, "/role/"):
		mount, role, _ := strings.Cut(strings.TrimPrefix(path, "auth/"), "/role/")
		if !f.auths[mount+"/"] {
			refuse(404, "no handler for route")
			return
		}
		f.roles[mount+"/"+role] = body
		answer(204, nil)
	default:
		refuse(404, "no handler for route "+path)
	}
}

func setOf(m map[string]bool) map[string]any {
	out := map[string]any{}
	for k := range m {
		out[k] = map[string]any{}
	}
	return out
}

// fakeRecovery is the recovery key's Secret.
type fakeRecovery struct {
	key    string
	putErr error
}

func (f *fakeRecovery) Present(context.Context) (bool, error) { return f.key != "", nil }
func (f *fakeRecovery) Put(_ context.Context, key string) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.key = key
	return nil
}

func baoConfig(t *testing.T) *OpenBaoConfig {
	t.Helper()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("the-jwt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &OpenBaoConfig{URL: "https://openbao.hugr-openbao.svc:8200", TokenFile: token, Namespace: "hugr-openbao",
		RecoverySecret: "openbao-recovery", TransitMount: "transit", AuthMount: "kubernetes", Audience: "vault",
		Init: OpenBaoBinding{Role: "hugr-openbao-init", ServiceAccount: "hugr-openbao-init"},
		Services: []OpenBaoService{
			{Name: "kista", Use: "sign", Key: "hugr-channel", ServiceAccount: "hugr-kista", Namespace: "hugr-kista"},
			{Name: "tresor", Use: "kek", Key: "hugr-tresor", ServiceAccount: "tresor", Namespace: "hugr-tresor"},
		}}
}

func baoClient(srv *httptest.Server) *OpenBao {
	return &OpenBao{base: srv.URL, client: srv.Client(), Wait: time.Second, Poll: 10 * time.Millisecond}
}

func TestOpenBaoFirstRunInitialisesConfiguresAndRevokes(t *testing.T) {
	f := newFakeBao()
	srv := httptest.NewServer(f)
	defer srv.Close()
	rec := &fakeRecovery{}
	c := baoConfig(t)
	if err := RunOpenBao(context.Background(), c, baoClient(srv), rec, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if !f.initialized || rec.key != "recovery-key" {
		t.Fatalf("initialised %v, recovery key kept %q", f.initialized, rec.key)
	}
	if len(f.tokens) != 0 {
		t.Fatalf("a token outlived the run: %v", f.tokens)
	}
	if !f.mounts["transit/"] || !f.auths["kubernetes/"] {
		t.Fatalf("mounts %v, auths %v", f.mounts, f.auths)
	}
	if f.keys["hugr-channel"] != "rsa-2048" || f.keys["hugr-tresor"] != "aes256-gcm96" {
		t.Fatalf("keys %v", f.keys)
	}
	for _, p := range []string{"kista", "tresor", "hugr-openbao-init"} {
		if f.policies[p] == "" {
			t.Fatalf("no policy %s: %v", p, f.policies)
		}
	}
	kista := f.roles["kubernetes/kista"]
	if kista == nil || kista["audience"] != "vault" || kista["bound_service_account_names"].([]any)[0] != "hugr-kista" ||
		kista["bound_service_account_namespaces"].([]any)[0] != "hugr-kista" || kista["token_policies"].([]any)[0] != "kista" {
		t.Fatalf("kista's role %v", kista)
	}
	own := f.roles["kubernetes/hugr-openbao-init"]
	if own == nil || own["bound_service_account_namespaces"].([]any)[0] != "hugr-openbao" || own["token_ttl"] != "15m" {
		t.Fatalf("the Job's own role %v", own)
	}
	if f.config["kubernetes_host"] != "https://kubernetes.default.svc" {
		t.Fatalf("the Kubernetes auth's config %v", f.config)
	}
}

func TestOpenBaoLaterRunLogsInAndChangesNothingTwice(t *testing.T) {
	f := newFakeBao()
	srv := httptest.NewServer(f)
	defer srv.Close()
	rec := &fakeRecovery{}
	c := baoConfig(t)
	if err := RunOpenBao(context.Background(), c, baoClient(srv), rec, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	// an upgrade: the same release again, then one more service key
	for i := 0; i < 2; i++ {
		if err := RunOpenBao(context.Background(), c, baoClient(srv), rec, slog.New(slog.DiscardHandler)); err != nil {
			t.Fatalf("run %d: %v", i+2, err)
		}
	}
	for _, call := range f.calls {
		if call == "PUT sys/init" || call == "POST sys/mounts/transit" || call == "POST sys/auth/kubernetes" {
			t.Fatalf("a later run made what existed: %s", call)
		}
	}
	if len(f.tokens) != 0 {
		t.Fatalf("the login token outlived the run: %v", f.tokens)
	}
}

// a release that adds a service: the Job widens its own policy before it makes the key and the role, which OpenBao
// would refuse otherwise (its token holds the policy of the run before)
func TestOpenBaoWidensItsOwnPolicyFirst(t *testing.T) {
	f := newFakeBao()
	srv := httptest.NewServer(f)
	defer srv.Close()
	rec := &fakeRecovery{}
	c := baoConfig(t)
	if err := RunOpenBao(context.Background(), c, baoClient(srv), rec, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	c.Services = append(c.Services, OpenBaoService{Name: "extra", Use: "kek", Key: "hugr-extra", ServiceAccount: "x",
		Namespace: "hugr-x"})
	f.calls = nil
	if err := RunOpenBao(context.Background(), c, baoClient(srv), rec, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	first := -1
	for i, call := range f.calls {
		if call == "PUT sys/policies/acl/hugr-openbao-init" && first < 0 {
			first = i
		}
		if (strings.Contains(call, "hugr-extra") || strings.HasSuffix(call, "/role/extra")) && (first < 0 || i < first) {
			t.Fatalf("%s before the Job's own policy was widened: %v", call, f.calls)
		}
	}
	if !strings.Contains(f.policies["hugr-openbao-init"], "transit/keys/hugr-extra") || f.keys["hugr-extra"] == "" ||
		f.roles["kubernetes/extra"] == nil {
		t.Fatalf("the new service was not made: keys %v", f.keys)
	}
}

// the node just initialised takes the HA lock a moment later: the Job waits for an active node, and a write refused
// as "not active" in between is asked again (live OpenBao 2.7.1 on PostgreSQL answered so)
func TestOpenBaoFirstRunWaitsForTheActiveNode(t *testing.T) {
	f := newFakeBao()
	f.leaderless = 3
	f.notActive = 2
	srv := httptest.NewServer(f)
	defer srv.Close()
	rec := &fakeRecovery{}
	if err := RunOpenBao(context.Background(), baoConfig(t), baoClient(srv), rec, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if f.roles["kubernetes/hugr-openbao-init"] == nil || f.keys["hugr-tresor"] == "" || len(f.tokens) != 0 {
		t.Fatalf("roles %v keys %v tokens %v", f.roles, f.keys, f.tokens)
	}
}

// a first run that stops after its own login is made leaves the next one able to finish
func TestOpenBaoTheJobsLoginComesFirst(t *testing.T) {
	f := newFakeBao()
	srv := httptest.NewServer(f)
	defer srv.Close()
	if err := RunOpenBao(context.Background(), baoConfig(t), baoClient(srv), &fakeRecovery{},
		slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	own, key := -1, -1
	for i, call := range f.calls {
		if call == "POST auth/kubernetes/role/hugr-openbao-init" && own < 0 {
			own = i
		}
		if strings.HasPrefix(call, "POST transit/keys/") && key < 0 {
			key = i
		}
	}
	if own < 0 || key < 0 || own > key {
		t.Fatalf("the Job's own role (%d) is not made before the first key (%d): %v", own, key, f.calls)
	}
}

func TestOpenBaoRefusesAKeyOfAnotherType(t *testing.T) {
	f := newFakeBao()
	srv := httptest.NewServer(f)
	defer srv.Close()
	f.initialized = true
	f.mounts["transit/"] = true
	f.auths["kubernetes/"] = true
	f.roles["kubernetes/hugr-openbao-init"] = map[string]any{}
	f.keys["hugr-channel"] = "aes256-gcm96"
	err := RunOpenBao(context.Background(), baoConfig(t), baoClient(srv), &fakeRecovery{key: "k"},
		slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "hugr-channel is aes256-gcm96") {
		t.Fatalf("want the key's type refused, got %v", err)
	}
	if f.keys["hugr-channel"] != "aes256-gcm96" {
		t.Fatalf("the key was changed")
	}
	if len(f.tokens) != 0 {
		t.Fatalf("the token outlived a failed run")
	}
}

func TestOpenBaoUnkeptRecoveryKeyFailsAndRevokesTheRoot(t *testing.T) {
	f := newFakeBao()
	srv := httptest.NewServer(f)
	defer srv.Close()
	err := RunOpenBao(context.Background(), baoConfig(t), baoClient(srv), &fakeRecovery{putErr: errors.New("forbidden")},
		slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "break-glass") {
		t.Fatalf("want the unkept key named, got %v", err)
	}
	if len(f.tokens) != 0 {
		t.Fatalf("the root token outlived the run")
	}
}

func TestOpenBaoLoginWaitsOutAnUnsealingServer(t *testing.T) {
	f := newFakeBao()
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := baoConfig(t)
	rec := &fakeRecovery{}
	if err := RunOpenBao(context.Background(), c, baoClient(srv), rec, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	f.unavailable = 2
	if err := RunOpenBao(context.Background(), c, baoClient(srv), rec, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("a login answered 503 twice was not asked again: %v", err)
	}
}

func TestOpenBaoFailedLoginNamesTheRole(t *testing.T) {
	f := newFakeBao()
	srv := httptest.NewServer(f)
	defer srv.Close()
	f.initialized = true
	c := baoConfig(t)
	err := RunOpenBao(context.Background(), c, baoClient(srv), &fakeRecovery{}, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "role hugr-openbao-init") {
		t.Fatalf("want the login's role named, got %v", err)
	}
}

func TestOpenBaoWarnsWhenTheRecoveryKeyIsGone(t *testing.T) {
	f := newFakeBao()
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := baoConfig(t)
	rec := &fakeRecovery{}
	if err := RunOpenBao(context.Background(), c, baoClient(srv), rec, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	rec.key = ""
	var logged bytes.Buffer
	if err := RunOpenBao(context.Background(), c, baoClient(srv), rec, slog.New(slog.NewTextHandler(&logged, nil))); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), "recovery key is not kept") {
		t.Fatalf("no warning: %s", logged.String())
	}
}

func TestOpenBaoWaitsForAServerThatStarts(t *testing.T) {
	f := newFakeBao()
	starting := 3
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if starting > 0 {
			starting--
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		f.ServeHTTP(w, r)
	}))
	defer srv.Close()
	ok, err := baoClient(srv).WaitInitialized(context.Background())
	if err != nil || ok || starting != 0 {
		t.Fatalf("want an uninitialised answer after the starts, got %v %v (%d left)", ok, err, starting)
	}
	b := baoClient(srv)
	b.Wait = 50 * time.Millisecond
	starting = 1000
	if _, err := b.WaitInitialized(context.Background()); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("want a server that never answers named, got %v", err)
	}
}

func TestOpenBaoPolicies(t *testing.T) {
	c := baoConfig(t)
	kista := servicePolicy("transit", c.Services[0])
	tresor := servicePolicy("transit", c.Services[1])
	for _, want := range []string{`path "transit/keys/hugr-channel"`, `path "transit/sign/hugr-channel/sha2-256"`} {
		if !strings.Contains(kista, want) {
			t.Fatalf("kista's policy lacks %s:\n%s", want, kista)
		}
	}
	if strings.Contains(kista, "decrypt") {
		t.Fatalf("kista's policy may decrypt:\n%s", kista)
	}
	for _, want := range []string{"transit/encrypt/hugr-tresor", "transit/decrypt/hugr-tresor", "transit/hmac/hugr-tresor/sha2-256"} {
		if !strings.Contains(tresor, want) {
			t.Fatalf("tresor's policy lacks %s:\n%s", want, tresor)
		}
	}
	own := initPolicy(c)
	if strings.Contains(own, "*") || strings.Contains(own, "transit/sign") || strings.Contains(own, "transit/decrypt") {
		t.Fatalf("the Job's policy is wider than its configuration:\n%s", own)
	}
	for _, want := range []string{`path "sys/auth/kubernetes"`, `"sudo"`, `path "sys/policies/acl/tresor"`,
		`path "auth/kubernetes/role/hugr-openbao-init"`} {
		if !strings.Contains(own, want) {
			t.Fatalf("the Job's policy lacks %s:\n%s", want, own)
		}
	}
}

func TestOpenBaoConfigValidate(t *testing.T) {
	c := baoConfig(t)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := *c
	bad.URL = "http://openbao"
	bad.Services = append([]OpenBaoService{}, c.Services...)
	bad.Services[0].Use = "encrypt"
	bad.Services = append(bad.Services, OpenBaoService{Name: "hugr-openbao-init", Use: "kek", Key: "k",
		ServiceAccount: "s", Namespace: "n"})
	err := bad.Validate()
	for _, want := range []string{"https URL", "use is sign or kek", "named twice"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q refused, got %v", want, err)
		}
	}
}

// the client never writes a token into an error
func TestOpenBaoErrorsCarryNoToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
	}))
	defer srv.Close()
	err := baoClient(srv).write(context.Background(), "secret-token-value", http.MethodPost, "sys/mounts/transit",
		map[string]any{"type": "transit"})
	if err == nil || strings.Contains(err.Error(), "secret-token-value") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("got %v", err)
	}
}
