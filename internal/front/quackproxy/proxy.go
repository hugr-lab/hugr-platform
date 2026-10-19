// Package quackproxy is the front's quack door (spec 002): an HTTP/1.1 reverse proxy for `POST /quack`, each request
// routed on its own, and `GET /.well-known/quack-auth`. A request goes to the node its label names - the acl_node
// cookie, the X-Quack-Connection-Id header, else the connection id in the message's own header - and a CONNECT is
// seated on a node that may take it, seated again when the node refuses it by its placement or because it is full.
// The front reads a message's header and an error answer's text; it writes no quack message and forwards every body
// untouched.
package quackproxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
	"github.com/hugr-lab/hugr-platform/internal/front/flightproxy"
	"github.com/hugr-lab/hugr-platform/internal/front/metrics"
	"github.com/hugr-lab/hugr-platform/internal/front/quackwire"
	"github.com/hugr-lab/hugr-platform/internal/front/route"
)

// ConnectionIDHeader is the header a quack client with duckdb/duckdb-quack#297 names its connection by.
const ConnectionIDHeader = "X-Quack-Connection-Id"

// MaxConnect is the largest CONNECT the front keeps to send again (it carries a token).
const MaxConnect = 1 << 20

// HeaderWait is how long a request's body may take to bring the message's header: the front reads it before any node
// or token has judged the caller.
const HeaderWait = 30 * time.Second

// MaxConnsPerNode bounds the front's connections to one node (a node's quack server holds 1024 by default).
const MaxConnsPerNode = 1024

// Proxy is the door.
type Proxy struct {
	Fleet *fleet.Fleet
	// Namespace of the platform: a node's TLS name is <its Service>.<namespace>.svc.
	Namespace string
	// Roots verify the nodes (the cluster CA bundle).
	Roots *x509.CertPool
	// NoNodeWait is how long a new connection waits for a placeable node of its group.
	NoNodeWait time.Duration
	Log        *slog.Logger
	// Insecure speaks plain HTTP to the nodes - a test's only.
	Insecure bool

	mu         sync.Mutex
	transports map[string]*http.Transport // by Service
	rnd        *rand.Rand
}

// Handler of the door.
func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /quack", p.quack)
	mux.HandleFunc("GET /.well-known/quack-auth", p.discovery)
	mux.HandleFunc("OPTIONS /quack", p.discovery) // a browser client's preflight: any node answers it alike
	return mux
}

func (p *Proxy) transport(service string) *http.Transport {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.transports == nil {
		p.transports = map[string]*http.Transport{}
	}
	if t := p.transports[service]; t != nil {
		return t
	}
	t := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: p.Roots, ServerName: service + "." + p.Namespace + ".svc"},
		TLSHandshakeTimeout: 5 * time.Second,
		// a quack client keeps 64 requests in flight; the body is the protocol's, never re-encoded
		MaxIdleConnsPerHost: 256,
		MaxConnsPerHost:     MaxConnsPerNode,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
	p.transports[service] = t
	return t
}

func (p *Proxy) random() *rand.Rand {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rnd == nil {
		p.rnd = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}
	return p.rnd
}

func (p *Proxy) target(n *fleet.Node) *url.URL {
	scheme := "https"
	if p.Insecure {
		scheme = "http"
	}
	return &url.URL{Scheme: scheme, Host: net.JoinHostPort(n.Address, strconv.Itoa(int(n.QuackPort)))}
}

func (p *Proxy) quack(w http.ResponseWriter, r *http.Request) {
	metrics.StreamsOpen.WithLabelValues("quack").Inc()
	defer metrics.StreamsOpen.WithLabelValues("quack").Dec()
	body := bufio.NewReaderSize(r.Body, quackwire.HeaderLimit)
	control := http.NewResponseController(w)
	_ = control.SetReadDeadline(time.Now().Add(HeaderWait))
	header, err := quackwire.PeekHeader(body)
	_ = control.SetReadDeadline(time.Time{}) // the body itself is the node's to wait for
	if err != nil {
		http.Error(w, "hugr: not a quack message", http.StatusBadRequest)
		return
	}
	r.Body = struct {
		io.Reader
		io.Closer
	}{body, r.Body}
	if header.Type == quackwire.Connect {
		p.connect(w, r)
		return
	}
	// the message's own connection id first: a cookie is one per jar, and a client holding connections on two nodes
	// sends one of them in every request
	label := route.ConnectionLabel(header.ConnectionID)
	if label == "" {
		label = route.ConnectionLabel(r.Header.Get(ConnectionIDHeader))
	}
	if label == "" {
		label = route.CookieLabel(r.Header.Values("Cookie"))
	}
	view, _ := p.Fleet.View()
	outcome := "routed"
	n := view.Routable(label, fleet.Quack)
	if n == nil {
		// a node the front does not see: any node answers that it does not know the connection, and the client
		// connects again - the front writes no quack message of its own
		outcome = "unknown_label"
		if n = p.choose(r, view.DefaultGroups(time.Now()), nil); n == nil {
			p.refuseNoNode(w, nil)
			return
		}
	}
	p.forward(w, r, n)
	p.done(header.Type, n, outcome)
}

// choose a placeable node of the groups, waiting up to NoNodeWait for one.
func (p *Proxy) choose(r *http.Request, groups []string, tried map[string]bool) *fleet.Node {
	return p.Fleet.Wait(r.Context(), p.NoNodeWait, func(v *fleet.View) *fleet.Node {
		return v.Choose(fleet.Quack, groups, tried, time.Now(), p.random())
	})
}

// unanswered: the nodes tried did not answer at all (gone between the fleet's view and the dial).
func (p *Proxy) unanswered(w http.ResponseWriter) {
	metrics.Calls.WithLabelValues("quack", "upstream_error").Inc()
	http.Error(w, "hugr: the node did not answer", http.StatusBadGateway)
}

func (p *Proxy) refuseNoNode(w http.ResponseWriter, groups []string) {
	metrics.Calls.WithLabelValues("quack", "no_node").Inc()
	http.Error(w, flightproxy.NoNodeText(groups), http.StatusServiceUnavailable)
}

// forward one request to the node, its body streamed both ways.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, n *fleet.Node) {
	target := p.target(n)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = target.Scheme, target.Host
			pr.Out.Host = pr.In.Host // the client's, as it sent it
		},
		Transport: p.transport(n.Service),
		ModifyResponse: func(resp *http.Response) error {
			resp.Body = counting{resp.Body, "out"}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			metrics.Calls.WithLabelValues("quack", "upstream_error").Inc()
			http.Error(w, "hugr: the node did not answer", http.StatusBadGateway)
		},
	}
	r.Body = counting{r.Body, "in"}
	proxy.ServeHTTP(w, r)
}

// connect seats a CONNECT: on a node that may take a new connection, again when the node refuses it by its
// placement or because it is full. The message is small and kept to send again.
func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxConnect+1))
	if err != nil || len(body) > MaxConnect {
		http.Error(w, "hugr: the CONNECT is not readable", http.StatusBadRequest)
		return
	}
	metrics.Bytes.WithLabelValues("quack", "in").Add(float64(len(body)))
	view, _ := p.Fleet.View()
	seat := route.NewSeat(view.DefaultGroups(time.Now()))
	outcome := "seated"
	silent := false // the last node tried did not answer
	for {
		n := p.choose(r, seat.Groups, seat.Tried)
		if n == nil && silent {
			p.unanswered(w)
			return
		}
		if n == nil {
			p.refuseNoNode(w, seat.NamedGroups())
			return
		}
		resp, answer, wrote, err := p.send(r, n, body)
		if err != nil {
			// only a CONNECT that never reached the node is sent again: one it received may have opened a session
			silent = true
			if !wrote && seat.Next(n.Label, route.Unreachable, nil) {
				metrics.Reseats.WithLabelValues("unreachable").Inc()
				outcome = "re-seated"
				continue
			}
			p.unanswered(w)
			return
		}
		silent = false
		text, _ := quackwire.ErrorText(answer)
		kind, groups := route.QuackRefusal(resp.Header.Get(route.GroupsHeader), text)
		if kind != route.None && seat.Next(n.Label, kind, groups) {
			reason := "wrong_resource_group"
			if kind == route.Busy {
				reason = "at_capacity"
			}
			metrics.Reseats.WithLabelValues(reason).Inc()
			outcome = "re-seated"
			continue
		}
		p.reply(w, resp, answer)
		p.done(quackwire.Connect, n, outcome)
		return
	}
}

// send the CONNECT to the node and read its whole answer (a connection's first answer is small). A redirect is never
// followed: the answer is the node's. `wrote`: the request reached the node whole, whatever happened after.
func (p *Proxy) send(r *http.Request, n *fleet.Node, body []byte) (*http.Response, []byte, bool, error) {
	var wrote atomic.Bool
	ctx := httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) { wrote.Store(info.Err == nil) },
	})
	out := r.Clone(ctx)
	out.URL = p.target(n)
	out.URL.Path, out.URL.RawQuery = r.URL.Path, r.URL.RawQuery
	out.RequestURI = ""
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.ContentLength = int64(len(body))
	for k := range out.Header {
		if hop(k) {
			out.Header.Del(k)
		}
	}
	resp, err := p.transport(n.Service).RoundTrip(out)
	if err != nil {
		return nil, nil, wrote.Load(), err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, nil, true, err
	}
	return resp, answer, true, nil
}

// discovery: what every node answers alike - the policy's discovery document, a CORS preflight. Any node that answers
// at all (of the default group first), another when one does not answer or says it is draining (503).
func (p *Proxy) discovery(w http.ResponseWriter, r *http.Request) {
	view, _ := p.Fleet.View()
	seat := route.NewSeat(view.DefaultGroups(time.Now()))
	silent := false
	for {
		n := p.Fleet.Wait(r.Context(), p.NoNodeWait, func(v *fleet.View) *fleet.Node {
			if n := v.ChooseAnswering(fleet.Quack, seat.Groups, seat.Tried, time.Now(), p.random()); n != nil {
				return n
			}
			return v.ChooseAnswering(fleet.Quack, nil, seat.Tried, time.Now(), p.random())
		})
		if n == nil && silent {
			p.unanswered(w)
			return
		}
		if n == nil {
			p.refuseNoNode(w, nil)
			return
		}
		resp, answer, _, err := p.send(r, n, nil)
		if err != nil {
			silent = true
			if seat.Next(n.Label, route.Unreachable, nil) {
				continue
			}
			p.unanswered(w)
			return
		}
		if resp.StatusCode == http.StatusServiceUnavailable && seat.Next(n.Label, route.Busy, nil) {
			continue // a node draining before its Lease says so
		}
		p.reply(w, resp, answer)
		if p.Log != nil {
			p.Log.Info("call", "protocol", "quack", "method", "discovery", "node", n.Label)
		}
		return
	}
}

// reply with a node's whole answer.
func (p *Proxy) reply(w http.ResponseWriter, resp *http.Response, answer []byte) {
	for k, v := range resp.Header {
		if !hop(k) {
			w.Header()[k] = v
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(answer)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(answer)
	metrics.Bytes.WithLabelValues("quack", "out").Add(float64(len(answer)))
}

func (p *Proxy) done(t quackwire.Type, n *fleet.Node, outcome string) {
	metrics.Calls.WithLabelValues("quack", outcome).Inc()
	if p.Log != nil {
		p.Log.Info("call", "protocol", "quack", "message", int(t), "node", n.Label, "outcome", outcome)
	}
}

// hop-by-hop headers are the connection's, never forwarded (RFC 9110 7.6.1)
func hop(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Connection", "Keep-Alive", "Proxy-Connection", "Proxy-Authenticate", "Proxy-Authorization", "Te",
		"Trailer", "Transfer-Encoding", "Upgrade", "Content-Length":
		return true
	}
	return false
}

type counting struct {
	io.ReadCloser
	direction string
}

func (c counting) Read(b []byte) (int, error) {
	n, err := c.ReadCloser.Read(b)
	metrics.Bytes.WithLabelValues("quack", c.direction).Add(float64(n))
	return n, err
}
