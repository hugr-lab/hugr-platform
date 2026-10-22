package webconsole

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func serve(t *testing.T, h *Handler, method, path string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestServing(t *testing.T) {
	files := fstest.MapFS{
		"index.html":          {Data: []byte("<!doctype html><title>hugr console</title>")},
		"assets/index-abc.js": {Data: []byte("console.log(1)")},
	}
	h, err := New(func() string { return "https://login.example.org/realms/hugr" }, files)
	if err != nil || !h.Built() {
		t.Fatal(err)
	}
	w := serve(t, h, "GET", "/assets/index-abc.js")
	if w.Code != 200 || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") ||
		!strings.Contains(w.Header().Get("Content-Type"), "javascript") {
		t.Errorf("an asset: %d %v", w.Code, w.Header())
	}
	if w := serve(t, h, "GET", "/assets/missing.js"); w.Code != 404 {
		t.Errorf("a missing asset is no page: %d", w.Code)
	}
	for _, p := range []string{"/", "/groups/etl", "/nodes/hugr-node-etl-a/sessions", "/callback?code=x"} {
		w := serve(t, h, "GET", p)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "hugr console") ||
			w.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s answers the router's page: %d", p, w.Code)
		}
	}
	etag := serve(t, h, "GET", "/").Header().Get("ETag")
	if w := serve(t, h, "GET", "/", "If-None-Match", etag); w.Code != http.StatusNotModified {
		t.Errorf("an unchanged page: %d", w.Code)
	}
	csp := serve(t, h, "GET", "/").Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "script-src 'self' 'wasm-unsafe-eval';", "worker-src 'self';",
		"style-src 'self'", "connect-src 'self' https://login.example.org;",
		"form-action 'self' https://login.example.org;", "frame-ancestors 'none'", "object-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("the policy lacks %q: %s", want, csp)
		}
	}
	// WebAssembly compiles; nothing inline, no eval of JavaScript, no data:, no frame, no blob: worker
	if strings.Contains(strings.ReplaceAll(csp, "'wasm-unsafe-eval'", ""), "unsafe") || strings.Contains(csp, "data:") ||
		strings.Contains(csp, "frame-src") || strings.Contains(csp, "blob:") {
		t.Errorf("no inline, no eval, no data:, no frame: %s", csp)
	}
	if w := serve(t, h, "POST", "/"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("a POST to the page: %d", w.Code)
	}
}

// TestCompressed: the SQL page's DuckDB goes out gzipped to a browser that takes gzip, as is to one that does not; a
// file that is already compressed (a font) or too small goes out as is.
func TestCompressed(t *testing.T) {
	wasm := bytes.Repeat([]byte("duckdb wasm "), 4096)
	files := fstest.MapFS{
		"index.html":                           {Data: []byte("<!doctype html>")},
		"assets/duckdb-base-abc.wasm":          {Data: wasm},
		"assets/manrope-abc.woff2":             {Data: bytes.Repeat([]byte("font"), 1024)},
		"assets/x/quack.duckdb_extension.wasm": {Data: []byte("small")},
	}
	h, err := New(func() string { return "" }, files)
	if err != nil {
		t.Fatal(err)
	}
	for _, accept := range []string{"gzip, deflate, br, zstd", "br;q=1.0, GZIP ; q=0.5"} {
		w := serve(t, h, "GET", "/assets/duckdb-base-abc.wasm", "Accept-Encoding", accept)
		if w.Code != 200 || w.Header().Get("Content-Encoding") != "gzip" || w.Header().Get("Vary") != "Accept-Encoding" ||
			w.Header().Get("Content-Type") != "application/wasm" || w.Body.Len() >= len(wasm) {
			t.Fatalf("%q: %d %v, %d bytes", accept, w.Code, w.Header(), w.Body.Len())
		}
		zr, err := gzip.NewReader(w.Body)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := io.ReadAll(zr); err != nil || !bytes.Equal(got, wasm) {
			t.Errorf("%q: the file decompressed differs (%v)", accept, err)
		}
	}
	for _, accept := range []string{"", "identity", "br", "gzip;q=0", "gzip; q=0.000", "gzipx"} {
		w := serve(t, h, "GET", "/assets/duckdb-base-abc.wasm", "Accept-Encoding", accept)
		if w.Header().Get("Content-Encoding") != "" || !bytes.Equal(w.Body.Bytes(), wasm) ||
			w.Header().Get("Vary") != "Accept-Encoding" {
			t.Errorf("%q: %v", accept, w.Header())
		}
	}
	for _, name := range []string{"/assets/manrope-abc.woff2", "/assets/x/quack.duckdb_extension.wasm"} {
		if w := serve(t, h, "GET", name, "Accept-Encoding", "gzip"); w.Header().Get("Content-Encoding") != "" ||
			w.Header().Get("Vary") != "" {
			t.Errorf("%s goes out as is: %v", name, w.Header())
		}
	}
}

func TestPlaceholder(t *testing.T) {
	h, err := New(func() string { return "" }, fstest.MapFS{})
	if err != nil || h.Built() {
		t.Fatal(err)
	}
	w := serve(t, h, "GET", "/anything")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "make console") {
		t.Errorf("without a build, the page says how to make one: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Header().Get("Content-Security-Policy"), "https://") {
		t.Error("no issuer, no other origin")
	}
}

func TestEmbedded(t *testing.T) {
	if _, err := New(nil, nil); err != nil {
		t.Errorf("the embedded build, or the placeholder: %v", err)
	}
}
