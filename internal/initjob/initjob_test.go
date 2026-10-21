package initjob

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testConfig() *Config {
	return &Config{
		Tresor: TresorConfig{URL: "https://tresor.example", TokenFile: "/t", Grants: []string{"group:hugr-nodes", "role:Seeder"}},
		Catalog: CatalogConfig{Secret: "hugr_policy_catalog", Engine: "postgres", Host: "pg.db.svc", Port: 5432,
			Database: "hugr", Login: "hugr_catalog", SSLMode: "disable",
			Admin: AdminConfig{SecretName: "hugr-init-db-admin", Dir: "/a", Database: "postgres"}},
	}
}

// fakeTresor records what the Job asks
type fakeTresor struct {
	exists    bool
	createErr error
	grantErr  error
	created   *Secret
	grants    map[string]string // id -> principal
	calls     []string
}

func (f *fakeTresor) Discover(context.Context) error {
	f.calls = append(f.calls, "discover")
	return nil
}
func (f *fakeTresor) Exists(context.Context, string) (bool, error) {
	f.calls = append(f.calls, "exists")
	return f.exists, nil
}
func (f *fakeTresor) Create(_ context.Context, _ string, s Secret) error {
	f.calls = append(f.calls, "create")
	if f.createErr != nil {
		return f.createErr
	}
	f.created = &s
	return nil
}
func (f *fakeTresor) Grant(_ context.Context, _, id, principal string) error {
	f.calls = append(f.calls, "grant")
	if f.grantErr != nil {
		return f.grantErr
	}
	if f.grants == nil {
		f.grants = map[string]string{}
	}
	f.grants[id] = principal
	return nil
}

type fakeDatabase struct {
	err                       error
	login, password, database string
	called                    bool
}

func (f *fakeDatabase) Ensure(_ context.Context, login, password, database string) error {
	f.called, f.login, f.password, f.database = true, login, password, database
	return f.err
}

type fakeAdmin struct{ deleted bool }

func (f *fakeAdmin) Delete(context.Context) error { f.deleted = true; return nil }

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestRunFresh: an install with no secret makes the login, writes it once, grants it, and deletes the
// administrator's Secret
func TestRunFresh(t *testing.T) {
	c := testConfig()
	tr, db, admin := &fakeTresor{}, &fakeDatabase{}, &fakeAdmin{}
	if err := Run(context.Background(), c, tr, func() (Database, error) { return db, nil }, admin, quiet()); err != nil {
		t.Fatal(err)
	}
	if !db.called || db.login != "hugr_catalog" || db.database != "hugr" {
		t.Fatalf("the database step: %+v", db)
	}
	raw, err := base64.RawURLEncoding.DecodeString(db.password)
	if err != nil || len(raw) != 32 {
		t.Errorf("the login's password is not 32 random bytes, base64url")
	}
	s := tr.created
	if s == nil || s.Type != "postgres" || s.Params["password"] != db.password || s.Params["user"] != "hugr_catalog" ||
		s.Params["host"] != "pg.db.svc" || s.Params["port"] != "5432" || s.Params["dbname"] != "hugr" ||
		s.Params["sslmode"] != "disable" || s.Params["sslrootcert"] != nil || len(s.RedactKeys) != 1 ||
		s.RedactKeys[0] != "password" {
		t.Errorf("the secret written: %+v", s)
	}
	if len(tr.grants) != 2 {
		t.Errorf("the grants: %v", tr.grants)
	}
	for id, p := range tr.grants {
		if id != grantID(p) || !strings.HasPrefix(id, "hugr-init-") {
			t.Errorf("grant %q for %q is not the Job's id", id, p)
		}
	}
	if !admin.deleted {
		t.Error("the administrator's Secret was kept")
	}
	if strings.Join(tr.calls, ",") != "discover,exists,create,grant,grant" {
		t.Errorf("the order: %v", tr.calls)
	}
	// a verified connection: the node's CA bundle is the secret's root
	c.Catalog.SSLMode, c.Catalog.NodeCAFile = "verify-full", "/etc/hugr/ca/ca.crt"
	if s := catalogSecret(c, "pw"); s.Params["sslmode"] != "verify-full" || s.Params["sslrootcert"] != "/etc/hugr/ca/ca.crt" {
		t.Errorf("a verified catalog's secret: %v", s.Params)
	}
}

// TestRunAgain: a run after the secret exists touches neither the database nor the secret, and grants again
func TestRunAgain(t *testing.T) {
	tr, admin := &fakeTresor{exists: true}, &fakeAdmin{}
	made := false
	err := Run(context.Background(), testConfig(), tr, func() (Database, error) { made = true; return &fakeDatabase{}, nil },
		admin, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if made || tr.created != nil {
		t.Error("a run after the secret exists made the login again")
	}
	if len(tr.grants) != 2 || !admin.deleted {
		t.Errorf("a run again: grants %v, admin deleted %v", tr.grants, admin.deleted)
	}
}

// TestRunFailures: the administrator's Secret stays while the Job has not done what it was for
func TestRunFailures(t *testing.T) {
	cases := map[string]struct {
		tr *fakeTresor
		db *fakeDatabase
	}{
		"the database refuses":              {&fakeTresor{}, &fakeDatabase{err: errors.New("no")}},
		"the secret appears meanwhile":      {&fakeTresor{createErr: ErrExists}, &fakeDatabase{}},
		"tresor refuses the write":          {&fakeTresor{createErr: errors.New("403")}, &fakeDatabase{}},
		"tresor refuses a grant":            {&fakeTresor{grantErr: errors.New("403")}, &fakeDatabase{}},
		"tresor refuses a grant, run again": {&fakeTresor{exists: true, grantErr: errors.New("403")}, &fakeDatabase{}},
	}
	for name, tc := range cases {
		admin := &fakeAdmin{}
		err := Run(context.Background(), testConfig(), tc.tr, func() (Database, error) { return tc.db, nil }, admin, quiet())
		if err == nil {
			t.Errorf("%s: no error", name)
		}
		if admin.deleted {
			t.Errorf("%s: the administrator's Secret was deleted", name)
		}
		if tc.db.err != nil && tc.tr.created != nil {
			t.Errorf("%s: a secret was written for a login the database refused", name)
		}
	}
}

func TestGrantID(t *testing.T) {
	if grantID("group:A") == grantID("group:a") {
		t.Error("two principals that read alike share a grant id")
	}
	if id := grantID("group:hugr-nodes"); id != "hugr-init-group-hugr-nodes-"+id[len(id)-8:] || len(id) != 35 {
		t.Errorf("a grant id %q is not readable", id)
	}
	ids := map[string]bool{}
	for range 3 {
		ids[grantID("group:hugr-nodes")] = true
	}
	if len(ids) != 1 {
		t.Error("a grant id changes between runs")
	}
	if long := grantID("role:" + strings.Repeat("x", 300)); len(long) > 200 {
		t.Errorf("a grant id of %d characters", len(long))
	}
}

func TestConfigValidate(t *testing.T) {
	if err := testConfig().Validate(); err != nil {
		t.Fatalf("a complete configuration: %v", err)
	}
	cases := map[string]func(*Config){
		"tresor over http":        func(c *Config) { c.Tresor.URL = "http://tresor" },
		"tresor's userinfo":       func(c *Config) { c.Tresor.URL = "https://u:p@tresor" },
		"no token file":           func(c *Config) { c.Tresor.TokenFile = "" },
		"no grant":                func(c *Config) { c.Tresor.Grants = nil },
		"a grant to a subject":    func(c *Config) { c.Tresor.Grants = []string{"subject:x|y"} },
		"another engine":          func(c *Config) { c.Catalog.Engine = "mssql" },
		"a host with a space":     func(c *Config) { c.Catalog.Host = "pg db" },
		"a database with a quote": func(c *Config) { c.Catalog.Database = `hugr"x` },
		"an upper-case login":     func(c *Config) { c.Catalog.Login = "Hugr" },
		"an sslmode of prefer":    func(c *Config) { c.Catalog.SSLMode = "prefer" },
		"no admin dir":            func(c *Config) { c.Catalog.Admin.Dir = "" },
		"no secret name":          func(c *Config) { c.Catalog.Secret = "" },
	}
	for name, change := range cases {
		c := testConfig()
		change(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestTresorClient: the protocol as the Job speaks it - discovery without a token, the rest with the projected one,
// the write conditional, a 412 as ErrExists, a refusal's type and detail, no redirect followed
func TestTresorClient(t *testing.T) {
	var seen []string
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		seen = append(seen, r.Method+" "+r.URL.Path+" "+auth+" "+r.Header.Get("If-None-Match"))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/.well-known/duckdb-secrets":
			_, _ = io.WriteString(w, `{"protocol":"duckdb-secrets/1","api":"`+server.URL+`/"}`)
		case auth != "Bearer tok-1":
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"type":"unauthenticated"}`)
		case r.Method == "GET" && r.URL.Path == "/v1/secrets":
			_, _ = io.WriteString(w, `[{"name":"sales"},{"name":"hugr_policy_catalog"}]`)
		case r.Method == "PUT" && r.URL.Path == "/v1/secrets/taken":
			w.WriteHeader(412)
			_, _ = io.WriteString(w, `{"type":"precondition_failed"}`)
		case r.Method == "PUT" && r.URL.Path == "/v1/secrets/new":
			var s Secret
			if json.NewDecoder(r.Body).Decode(&s) != nil || s.Type != "postgres" {
				w.WriteHeader(422)
				return
			}
			w.WriteHeader(201)
			_, _ = io.WriteString(w, `{}`)
		case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/v1/secrets/new/grants/"):
			var g map[string]any
			_ = json.NewDecoder(r.Body).Decode(&g)
			if g["principal"] != "group:nodes" {
				w.WriteHeader(422)
				_, _ = io.WriteString(w, `{"type":"invalid_secret","detail":"a principal"}`)
				return
			}
			_, _ = io.WriteString(w, `[]`)
		case r.URL.Path == "/v1/secrets/moved":
			http.Redirect(w, r, "https://elsewhere.example/", http.StatusFound)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	_ = os.WriteFile(token, []byte("tok-1\n"), 0o600)
	ca := filepath.Join(dir, "ca.crt")
	_ = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600)

	tr, err := NewTresor(server.URL, token, ca)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := tr.Discover(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(seen[0], "  ") {
		t.Errorf("the discovery carried a token: %q", seen[0])
	}
	if ok, err := tr.Exists(ctx, "hugr_policy_catalog"); err != nil || !ok {
		t.Errorf("exists: %v %v", ok, err)
	}
	if ok, err := tr.Exists(ctx, "other"); err != nil || ok {
		t.Errorf("a missing secret: %v %v", ok, err)
	}
	if err := tr.Create(ctx, "taken", Secret{Type: "postgres"}); !errors.Is(err, ErrExists) {
		t.Errorf("a 412: %v", err)
	}
	if err := tr.Create(ctx, "new", Secret{Type: "postgres"}); err != nil {
		t.Errorf("a create: %v", err)
	}
	if !strings.HasSuffix(seen[len(seen)-1], "Bearer tok-1 *") {
		t.Errorf("the write is not conditional: %q", seen[len(seen)-1])
	}
	if err := tr.Grant(ctx, "new", "g1", "group:nodes"); err != nil {
		t.Errorf("a grant: %v", err)
	}
	err = tr.Grant(ctx, "new", "g1", "group:other")
	if err == nil || !strings.Contains(err.Error(), "422 invalid_secret: a principal") {
		t.Errorf("a refused grant: %v", err)
	}
	if err := tr.do(ctx, "GET", tr.api+"/v1/secrets/moved", nil, nil, true, nil); err == nil {
		t.Error("a redirect was followed with the bearer")
	}
	// the token is read at each request: a renewed one is sent from then on
	_ = os.WriteFile(token, []byte("tok-2"), 0o600)
	if _, err := tr.Exists(ctx, "x"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("the renewed token was not read: %v", err)
	}
}
