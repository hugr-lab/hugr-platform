// Package webconsole serves the web console's build (spec 003): embedded from dist, every path that is no file of
// the build answered by index.html (the router's), under a strict content security policy - no inline script or
// style, no data: fonts, the IdP's origin the only other one the page talks to.
package webconsole

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// dist holds the build (`make console`). A binary built without it serves the placeholder: the console's API still
// answers.
//
//go:embed all:dist
var dist embed.FS

const placeholder = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>hugr console</title></head>
<body><p>This operator was built without its web console: build it with <code>make console</code>, then
<code>make build</code>.</p></body></html>
`

// Handler serves the build.
type Handler struct {
	files fs.FS
	index []byte
	etag  string
	// Issuer is the IdP's issuer URL now (the platform's spec.idp.issuer): its origin is the sign-in's connect-src
	// and form-action.
	Issuer func() string
	// zipped holds an asset gzipped at its first request that takes gzip: the SQL page's DuckDB is 34 MB, 8 when
	// compressed (spec 007).
	zipped sync.Map // name -> *zippedAsset
}

type zippedAsset struct {
	once sync.Once
	data []byte // nil: compression did not make it smaller
}

// compressible are the extensions whose files compress; the fonts (woff2) already are.
var compressible = map[string]bool{".wasm": true, ".js": true, ".css": true, ".svg": true}

// New handler over the embedded build, or over `files` when a test gives one.
func New(issuer func() string, files fs.FS) (*Handler, error) {
	if files == nil {
		sub, err := fs.Sub(dist, "dist")
		if err != nil {
			return nil, err
		}
		files = sub
	}
	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		index = []byte(placeholder)
		files = nil
	}
	sum := sha256.Sum256(index)
	return &Handler{files: files, index: index, etag: `"` + hex.EncodeToString(sum[:8]) + `"`, Issuer: issuer}, nil
}

// Built says whether the binary carries the build.
func (h *Handler) Built() bool { return h.files != nil }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.policy(w)
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if h.files != nil && name != "" && name != "index.html" {
		if data, err := fs.ReadFile(h.files, name); err == nil {
			if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			if strings.HasPrefix(name, "assets/") {
				// Vite names an asset by its content
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			if zipped := h.gzipped(name, data); zipped != nil {
				w.Header().Add("Vary", "Accept-Encoding")
				if acceptsGzip(r) {
					w.Header().Set("Content-Encoding", "gzip")
					data = zipped
				}
			}
			http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
			return
		}
		if strings.HasPrefix(name, "assets/") {
			http.NotFound(w, r)
			return
		}
	}
	// the router's path: the page itself, revalidated on every load
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", h.etag)
	if r.Header.Get("If-None-Match") == h.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(h.index)
}

// gzipped is the file compressed, once per file, or nil when it does not compress.
func (h *Handler) gzipped(name string, data []byte) []byte {
	if !compressible[path.Ext(name)] || len(data) < 1024 {
		return nil
	}
	v, _ := h.zipped.LoadOrStore(name, &zippedAsset{})
	z := v.(*zippedAsset)
	z.once.Do(func() {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		if _, err := zw.Write(data); err == nil && zw.Close() == nil && b.Len() < len(data) {
			z.data = b.Bytes()
		}
	})
	return z.data
}

// acceptsGzip says whether the request's Accept-Encoding takes gzip (not with q=0).
func acceptsGzip(r *http.Request) bool {
	for _, header := range r.Header.Values("Accept-Encoding") {
		for _, item := range strings.Split(header, ",") {
			coding, params, _ := strings.Cut(strings.TrimSpace(item), ";")
			if !strings.EqualFold(strings.TrimSpace(coding), "gzip") {
				continue
			}
			q := 1.0
			if v, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
				q, _ = strconv.ParseFloat(strings.TrimSpace(v), 64)
			}
			return q > 0
		}
	}
	return false
}

// policy sets the content security policy: the page's own origin, and the IdP's for the sign-in. The token is renewed
// by its refresh token, never in a frame, so no frame-src. The SQL page's DuckDB (spec 007) runs in a worker of the
// page's own origin - the worker's script is served under this same policy, so it holds for the worker too: WebAssembly
// compiled (wasm-unsafe-eval: no eval of JavaScript), quack loaded from this origin.
func (h *Handler) policy(w http.ResponseWriter) {
	idp := ""
	if h.Issuer != nil {
		if u, err := url.Parse(h.Issuer()); err == nil && u.Scheme == "https" && u.Host != "" {
			idp = " https://" + u.Host
		}
	}
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; "+
		"worker-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; connect-src 'self'"+idp+"; "+
		"object-src 'none'; base-uri 'self'; form-action 'self'"+idp+"; frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
}
