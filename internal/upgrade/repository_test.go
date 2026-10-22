package upgrade

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRepository(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/hugr/platform/v2.0.1/linux_amd64/postgres_scanner.duckdb_extension.gz",
			"/hugr/platform/v2.0.1/linux_arm64/postgres_scanner.duckdb_extension.gz":
			w.WriteHeader(http.StatusOK)
		case "/broken/platform/v2.0.1/linux_amd64/postgres_scanner.duckdb_extension.gz":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	// a node's word goes into a path: an empty one, or one that would climb, is not asked
	platforms := []string{"linux_arm64", "linux_amd64", "linux_amd64", "", "../../x", "a/b"}

	missing, err := Repository(ctx, srv.Client(), srv.URL+"/hugr/platform/", "v2.0.1", platforms, []string{"postgres_scanner"})
	if err != nil || missing != "" {
		t.Fatalf("every build there: %q %v", missing, err)
	}
	if len(asked) != 2 || !strings.HasPrefix(asked[0], "HEAD ") {
		t.Errorf("each platform asked once, by HEAD: %v", asked)
	}
	missing, err = Repository(ctx, srv.Client(), srv.URL+"/hugr/platform", "v2.0.1", platforms,
		[]string{"postgres_scanner", "mssql"})
	if err != nil || !strings.Contains(missing, "mssql for linux_amd64, mssql for linux_arm64") ||
		!strings.Contains(missing, "DuckDB v2.0.1") {
		t.Errorf("a missing extension: %q %v", missing, err)
	}
	if _, err = Repository(ctx, srv.Client(), srv.URL+"/broken/platform", "v2.0.1", []string{"linux_amd64"},
		[]string{"postgres_scanner"}); err == nil {
		t.Error("a repository answering 503 is waited for, not judged")
	}
	srv.Close()
	if _, err = Repository(ctx, srv.Client(), srv.URL+"/hugr/platform", "v2.0.1", []string{"linux_amd64"},
		[]string{"postgres_scanner"}); err == nil {
		t.Error("a repository that does not answer is waited for, not judged")
	}
}
