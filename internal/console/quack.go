package console

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/build"
)

// The platform's quack door under the console's own origin (spec 007): the SQL page's DuckDB, in the browser, attaches
// the platform as a quack client does. A quack URI has no path - its client posts to https://<host>/quack - so the
// console serves /quack and forwards it to the front, which seats the person on a node of their group (spec 002). The
// browser reaches nothing but the console: the front and the nodes stay in-cluster, their certificates the internal
// CA's. The door authenticates the quack session by the token in its CONNECT, as for any client; the console lets a
// request through only from a person signed in to it (the page's quack secret sends their token as a header too).

// quackPaths are the door's: its messages and its discovery document.
var quackPaths = []string{"/quack", "/.well-known/quack-auth"}

// FrontQuackUpstream is the front's quack door in the platform's namespace.
func FrontQuackUpstream(platform func(context.Context) (*v1alpha1.HugrPlatform, error)) func(context.Context) (*url.URL, error) {
	return func(ctx context.Context) (*url.URL, error) {
		p, err := platform(ctx)
		if err != nil {
			return nil, err
		}
		if p.Spec.Front == nil {
			return nil, errors.New("the platform runs no front")
		}
		_, quack := p.Spec.Front.EffectivePorts()
		return &url.URL{Scheme: "https", Host: net.JoinHostPort(build.FrontName+"."+p.Namespace+".svc",
			strconv.Itoa(int(quack)))}, nil
	}
}

// QuackMount forwards the door's paths to the front (or, for a stand, to a node's own door). A quack request is answered
// when its work is done - a PREPARE when the statement's first batch is - so neither its answer nor its write has a
// deadline here; the client bounds its own wait.
func QuackMount(upstream func(context.Context) (*url.URL, error),
	roots func(context.Context) (*x509.CertPool, error)) *Mount {
	return &Mount{Prefix: "", Upstream: upstream, Paths: quackPaths, Roots: roots, Unbounded: true}
}

// quackDoor lets the door's requests through for a person signed in to the console, the console's token taken off
// before the front sees the request: the quack session's token is the one in its CONNECT.
func (s *Server) quackDoor(w http.ResponseWriter, r *http.Request, _ string, _ *person) {
	if s.Quack == nil {
		problem(w, http.StatusNotFound, "no_door", "this console serves no quack door")
		return
	}
	r.Header.Del("Authorization")
	// the console's server writes within 60 s: not a quack answer, which waits for its statement
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	s.Quack.Handler().ServeHTTP(w, r)
}
