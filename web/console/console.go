// Package webconsole serves the web console's build (spec 003): embedded from dist, every path that is no file of
// the build answered by index.html (the router's), under a strict content security policy - no inline script or
// style, no data: fonts, the IdP's origin the only other one the page talks to.
package webconsole

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
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
}

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

// policy sets the content security policy: the page's own origin, and the IdP's for the sign-in. The token is renewed
// by its refresh token, never in a frame, so no frame-src.
func (h *Handler) policy(w http.ResponseWriter) {
	idp := ""
	if h.Issuer != nil {
		if u, err := url.Parse(h.Issuer()); err == nil && u.Scheme == "https" && u.Host != "" {
			idp = " https://" + u.Host
		}
	}
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; "+
		"img-src 'self'; font-src 'self'; connect-src 'self'"+idp+"; object-src 'none'; base-uri 'self'; "+
		"form-action 'self'"+idp+"; frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
}
