package initjob

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestPostgresEnsure runs against a real server when HUGR_INIT_TEST_PG names one (host:port:superuser password; a
// PostgreSQL 18 - with HUGR_INIT_TEST_PG_OLD naming a 17 too, the refusal of an older server is checked):
// the administrator a managed server gives - CREATEROLE and CREATEDB, no superuser - makes the login and the
// database, a run again sets the password anew, and the login then makes its own schema there, as a node's init does.
func TestPostgresEnsure(t *testing.T) {
	target := os.Getenv("HUGR_INIT_TEST_PG")
	if target == "" {
		t.Skip("HUGR_INIT_TEST_PG (host:port:password) names no server")
	}
	parts := strings.SplitN(target, ":", 3)
	port, _ := strconv.Atoi(parts[1])
	ctx := context.Background()
	super := func(db string) *pgx.Conn {
		cfg, err := pgx.ParseConfig("host=" + parts[0] + " port=" + parts[1] + " dbname=" + db + " sslmode=disable")
		if err != nil {
			t.Fatal(err)
		}
		cfg.User, cfg.Password = "postgres", parts[2]
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	root := super("postgres")
	defer root.Close(ctx)
	for _, sql := range []string{
		"DROP DATABASE IF EXISTS hugr_test WITH (FORCE)",
		"DROP ROLE IF EXISTS hugr_test_login",
		"DROP ROLE IF EXISTS hugr_test_admin",
		"CREATE ROLE hugr_test_admin LOGIN CREATEROLE CREATEDB PASSWORD 'admin-pw'",
	} {
		if _, err := root.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "username"), []byte("hugr_test_admin"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "password"), []byte("admin-pw\n"), 0o600)
	c := CatalogConfig{Engine: "postgres", Host: parts[0], Port: port, Database: "hugr_test", Login: "hugr_test_login",
		SSLMode: "disable", Admin: AdminConfig{Dir: dir, Database: "postgres"}}
	pg, err := NewPostgres(c, "")
	if err != nil {
		t.Fatal(err)
	}
	for run, password := range []string{"first-pw-AbC_1", "second-pw-XyZ-2"} {
		if err := pg.Ensure(ctx, c.Login, password, c.Database); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}
	// the login, with the second run's password, makes its own schema and a table in it
	cfg, _ := pgx.ParseConfig("host=" + parts[0] + " port=" + parts[1] + " dbname=hugr_test sslmode=disable")
	cfg.User, cfg.Password = c.Login, "second-pw-XyZ-2"
	login, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("the login with the password of the last run: %v", err)
	}
	defer login.Close(ctx)
	// its own schema (the catalog's acl) and the public one (tresor's and kista's tables)
	for _, sql := range []string{"CREATE SCHEMA acl", "CREATE TABLE acl.meta (key text PRIMARY KEY, value text)",
		"CREATE TABLE public.secrets (name text PRIMARY KEY)"} {
		if _, err := login.Exec(ctx, sql); err != nil {
			t.Fatalf("the login: %s: %v", sql, err)
		}
	}
	cfg.Password = "first-pw-AbC_1"
	if old, err := pgx.ConnectConfig(ctx, cfg); err == nil {
		old.Close(ctx)
		t.Error("the first run's password still logs in")
	}
	if older := os.Getenv("HUGR_INIT_TEST_PG_OLD"); older != "" {
		p := strings.SplitN(older, ":", 3)
		port, _ := strconv.Atoi(p[1])
		_ = os.WriteFile(filepath.Join(dir, "username"), []byte("postgres"), 0o600)
		_ = os.WriteFile(filepath.Join(dir, "password"), []byte(p[2]), 0o600)
		c.Host, c.Port = p[0], port
		pg, err := NewPostgres(c, "")
		if err != nil {
			t.Fatal(err)
		}
		err = pg.Ensure(ctx, c.Login, "pw", c.Database)
		if err == nil || !strings.Contains(err.Error(), "needs 18 or later") {
			t.Errorf("an older server: %v", err)
		}
	}
}
