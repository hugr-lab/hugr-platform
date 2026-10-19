// Package flightproxy is the front's Flight SQL door (spec 002): a gRPC proxy that decodes no message. A call that
// carries the acl_node cookie of a node the front sees goes to that node; any other call is seated on a node that
// may take a new session, and seated again when the node refuses it by its placement (another resource group) or
// because it is draining - only for a call whose client sent one message before the node answered, which the front
// keeps to send again.
package flightproxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
	"github.com/hugr-lab/hugr-platform/internal/front/flightwire"
	"github.com/hugr-lab/hugr-platform/internal/front/metrics"
	"github.com/hugr-lab/hugr-platform/internal/front/route"
)

// The Flight service's methods (arrow.flight.protocol.FlightService).
const service = "/arrow.flight.protocol.FlightService/"

// The limits a call meets at the front, which reads before any node or token has judged the caller. A first message
// is kept to send again (a ticket, a command, a handshake: kilobytes); a frame streamed through is a batch of rows.
const (
	MaxFirstMessage   = 4 << 20
	MaxMessage        = 256 << 20
	MaxStreamsPerConn = 128
	// FirstWait: how long a call may take to bring its first message, which the front reads before anyone judged it
	FirstWait = 30 * time.Second
)

// methods of the Flight service: anything else is logged as "other", never as the client wrote it
var methods = map[string]bool{"Handshake": true, "ListFlights": true, "GetFlightInfo": true, "PollFlightInfo": true,
	"GetSchema": true, "DoGet": true, "DoPut": true, "DoExchange": true, "DoAction": true, "ListActions": true}

// oneRequest are the methods whose client sends one message and half-closes: replayable, and the front closes its
// side after it. Handshake is replayable too, but its client may go on sending.
var oneRequest = map[string]bool{
	"ListFlights": true, "GetFlightInfo": true, "PollFlightInfo": true, "GetSchema": true, "DoGet": true,
	"DoAction": true, "ListActions": true,
}

// Proxy is the door.
type Proxy struct {
	Fleet *fleet.Fleet
	// Namespace of the platform: a node's TLS name is <its Service>.<namespace>.svc.
	Namespace string
	// Roots verify the nodes (the cluster CA bundle, spec 001 decision 3).
	Roots *x509.CertPool
	// NoNodeWait is how long a new connection waits for a placeable node of its group.
	NoNodeWait time.Duration
	Log        *slog.Logger
	// Insecure dials the nodes in the clear - a test's only.
	Insecure bool

	mu    sync.Mutex
	conns map[string]*upstream // by label
}

type upstream struct {
	address, service string
	conn             *grpc.ClientConn
}

// Server is the gRPC server of the door; `creds` terminate the client's TLS (nil: cleartext, a test's only).
func (p *Proxy) Server(creds credentials.TransportCredentials) *grpc.Server {
	opts := []grpc.ServerOption{
		grpc.UnknownServiceHandler(p.handle),
		grpc.ForceServerCodecV2(codec{direction: "in"}),
		grpc.MaxRecvMsgSize(MaxMessage), grpc.MaxSendMsgSize(math.MaxInt32), grpc.MaxConcurrentStreams(MaxStreamsPerConn),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
	}
	if creds != nil {
		opts = append(opts, grpc.Creds(creds))
	}
	return grpc.NewServer(opts...)
}

// Forget the connections to nodes the fleet no longer has; run at each change of the fleet. A node that is only not
// ready for a while keeps its connection, and the streams on it: closing it would cut them.
func (p *Proxy) Forget(view *fleet.View) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for label, u := range p.conns {
		n := view.Node(label)
		if n == nil || n.Address != u.address || n.Service != u.service {
			_ = u.conn.Close()
			delete(p.conns, label)
		}
	}
}

func (p *Proxy) conn(n *fleet.Node) (*grpc.ClientConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conns == nil {
		p.conns = map[string]*upstream{}
	}
	if u := p.conns[n.Label]; u != nil && u.address == n.Address && u.service == n.Service {
		return u.conn, nil
	} else if u != nil {
		_ = u.conn.Close()
	}
	creds := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: p.Roots,
		ServerName: n.Service + "." + p.Namespace + ".svc"})
	if p.Insecure {
		creds = insecure.NewCredentials()
	}
	conn, err := grpc.NewClient("passthrough:///"+net.JoinHostPort(n.Address, strconv.Itoa(int(n.FlightPort))),
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(grpc.ForceCodecV2(codec{direction: "out"}), grpc.MaxCallRecvMsgSize(math.MaxInt32),
			grpc.MaxCallSendMsgSize(math.MaxInt32)))
	if err != nil {
		return nil, err
	}
	p.conns[n.Label] = &upstream{address: n.Address, service: n.Service, conn: conn}
	return conn, nil
}

// random is a generator of one choice's own: the handlers of both doors choose at once, and a shared one would race.
func (p *Proxy) random() *rand.Rand {
	return rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
}

func (p *Proxy) handle(_ any, client grpc.ServerStream) error {
	metrics.StreamsOpen.WithLabelValues("flight").Inc()
	defer metrics.StreamsOpen.WithLabelValues("flight").Dec()
	method, ok := grpc.MethodFromServerStream(client)
	if !ok || !strings.HasPrefix(method, service) {
		return status.Error(codes.Unimplemented, "hugr: not a Flight call")
	}
	name := strings.TrimPrefix(method, service)
	md, _ := metadata.FromIncomingContext(client.Context())
	view, _ := p.Fleet.View()
	return p.seat(client, name, md, view.Routable(route.CookieLabel(md.Get("cookie")), fleet.Flight))
}

// seat a call on a node: the one its cookie names, else one that may take a new connection (the cookie names no node
// the front sees: a new connection, or one whose node is gone - the node that takes it moves the session by its
// state cookie, duckdb-acl spec 135). A call whose client sent one message before the node answered is seated again
// when the node refuses it before doing any work: another resource group, a drain, no answer at all. That holds for
// a call the cookie routed too - a driver's first call (JDBC's Handshake, ADBC's GetSqlInfo) is answered with the
// node's cookie before any session is seated, so the node's placement is first judged on a routed call.
func (p *Proxy) seat(client grpc.ServerStream, name string, md metadata.MD, routed *fleet.Node) error {
	replay := oneRequest[name] || name == "Handshake"
	// a call without the cookie may name its node in its first message - a ticket, a prepared statement's handle
	// (duckdb-acl spec 134's addendum) - which a DoPut sends before its batches: read it, send it first. (A
	// DoExchange is not read ahead: its server may speak first.)
	peek := replay || (routed == nil && name == "DoPut")
	open := &opening{}
	defer open.free()
	if peek {
		var f frame
		switch err := recvFirst(client, &f); {
		case err == io.EOF:
		case err != nil:
			return err
		default:
			open.first, f.data, open.sent = f.data, nil, true
			if replay && open.first.Len() > MaxFirstMessage {
				return status.Errorf(codes.ResourceExhausted, "hugr: a call's first message is larger than %d bytes",
					MaxFirstMessage)
			}
		}
	}
	view, _ := p.Fleet.View()
	if open.sent {
		// a call that names a handle - a ticket, a prepared statement (duckdb-acl spec 134's addendum) - belongs to
		// the node holding it, cookie or not, and is never seated elsewhere: another node does not have it, and would
		// take it for a stolen ticket (spec 047)
		if held := flightwire.Label(name, prefix(open.first, flightwire.Prefix)); held != "" {
			return p.toHolder(client, view.Node(held), name, md, replay, open)
		}
	}
	if !replay && open.sent {
		// a DoPut that names no handle: one message and the end - Flight SQL's ExecuteUpdate - can be sent again
		replay = open.readSecond(client)
	}
	seat := route.NewSeat(view.DefaultGroups(time.Now()))
	outcome := "seated"
	next := routed
	if routed != nil {
		outcome = "routed"
		if g := routed.Group(); g != "" {
			seat.Groups = []string{g} // another node of the group the connection was on
		}
	}
	var silent error // the last node tried did not answer
	for {
		n := next
		next = nil
		if n == nil {
			n = p.Fleet.Wait(client.Context(), p.NoNodeWait, func(v *fleet.View) *fleet.Node {
				return v.Choose(fleet.Flight, seat.Groups, seat.Tried, time.Now(), p.random())
			})
		}
		if err := client.Context().Err(); err != nil {
			return status.FromContextError(err).Err() // the client went: no node is tried for it
		}
		if n == nil && silent != nil {
			metrics.Calls.WithLabelValues("flight", "upstream_error").Inc()
			return silent
		}
		if n == nil {
			metrics.Calls.WithLabelValues("flight", "no_node").Inc()
			return status.Error(codes.Unavailable, NoNodeText(seat.NamedGroups()))
		}
		err := p.forward(client, n, name, md, replay, open)
		var refused *refusal
		silent = nil
		if errors.As(err, &refused) {
			if refused.kind == route.Unreachable {
				silent = refused.err
			}
			if seat.Next(n.Label, refused.kind, refused.groups) {
				metrics.Reseats.WithLabelValues(refused.reason()).Inc()
				outcome = "re-seated"
				continue
			}
			err = refused.answer(client)
		}
		p.done(name, n, outcome, err)
		return err
	}
}

// toHolder sends a call that names a handle to the node that holds it, once: its refusal, whatever it is, goes to the
// client as the node said it. A holder the front no longer sees is gone with the handle.
func (p *Proxy) toHolder(client grpc.ServerStream, n *fleet.Node, name string, md metadata.MD, replay bool,
	open *opening) error {
	if n == nil || n.Port(fleet.Flight) == 0 {
		// the holder is not in the fleet the front sees (gone, or not yet seen): another node could only refuse it
		metrics.Calls.WithLabelValues("flight", "holder_gone").Inc()
		return status.Error(codes.Unavailable, "hugr: the node that holds this call's ticket or prepared statement is "+
			"not in the fleet")
	}
	err := p.forward(client, n, name, md, replay, open)
	var refused *refusal
	if errors.As(err, &refused) {
		err = refused.answer(client)
	}
	p.done(name, n, "routed", err)
	return err
}

// opening is what the front read of a call before it chose a node.
type opening struct {
	first   mem.BufferSlice // the client's first message: kept, never copied - each send takes a reference
	sent    bool            // the client sent one (a BasicAuth Handshake sends none)
	closed  bool            // and half-closed right after it (a DoPut that is one message: ExecuteUpdate)
	second  *frame          // a DoPut's second message, read to learn the first was not alone
	pending <-chan received // a receive of it still running when the front stopped waiting
}

type received struct {
	f   frame
	err error
}

// SecondWait is how long the front waits to learn whether a DoPut is one message and the end - a client that
// closes after its descriptor does so at once; one that streams batches goes on as a stream, the receive handed on.
const SecondWait = time.Second

// readSecond reads a DoPut's next message: the end makes it a call that can be sent again (true).
func (o *opening) readSecond(client grpc.ServerStream) bool {
	got := make(chan received, 1)
	go func() {
		var f frame
		err := client.RecvMsg(&f)
		got <- received{f, err}
	}()
	timer := time.NewTimer(SecondWait)
	defer timer.Stop()
	select {
	case r := <-got:
		switch {
		case r.err == io.EOF:
			o.closed = true
			return true
		case r.err != nil:
			// the client's error is the pump's to meet: it ends the call there
			ch := make(chan received, 1)
			ch <- r
			o.pending = ch
		default:
			o.second = &r.f
		}
	case <-timer.C:
		o.pending = got
	}
	return false
}

func (o *opening) free() {
	o.first.Free()
	o.first = nil
	if o.second != nil {
		o.second.free()
		o.second = nil
	}
}

// recvFirst reads a call's first message within FirstWait: a client that brings none in time holds nothing longer.
func recvFirst(client grpc.ServerStream, f *frame) error {
	got := make(chan error, 1)
	go func() { got <- client.RecvMsg(f) }()
	timer := time.NewTimer(FirstWait)
	defer timer.Stop()
	select {
	case err := <-got:
		return err
	case <-timer.C:
		return status.Error(codes.DeadlineExceeded, "hugr: the call's first message did not come in time")
	}
}

// NoNodeText is the refusal when no node of the groups can take a new session.
func NoNodeText(groups []string) string {
	if len(groups) == 0 {
		return "hugr: no node can take a new session - retry"
	}
	return "hugr: no node of group " + strings.Join(groups, ", ") + " can take a new session - retry"
}

func (p *Proxy) done(name string, n *fleet.Node, outcome string, err error) {
	if _, ok := err.(upstreamError); ok {
		outcome = "upstream_error"
	}
	metrics.Calls.WithLabelValues("flight", outcome).Inc()
	if !methods[name] {
		name = "other"
	}
	if p.Log != nil {
		p.Log.Info("call", "protocol", "flight", "method", name, "node", n.Label, "outcome", outcome,
			"code", status.Code(err).String())
	}
}

// refusal is a node's answer to a seated call that the front may act on, before anything reached the client.
type refusal struct {
	kind    route.Refusal
	groups  []string
	header  metadata.MD
	trailer metadata.MD
	err     error
}

func (r *refusal) Error() string { return r.err.Error() }

// GRPCStatus is the node's status, or the front's own when the node did not answer.
func (r *refusal) GRPCStatus() *status.Status { return status.Convert(r.err) }

func (r *refusal) reason() string {
	switch r.kind {
	case route.WrongGroup:
		return "wrong_resource_group"
	case route.Busy:
		return "draining"
	}
	return "unreachable"
}

// answer sends the refusal to the client as the node said it.
func (r *refusal) answer(client grpc.ServerStream) error {
	if len(r.header) > 0 {
		_ = client.SetHeader(r.header)
	}
	client.SetTrailer(r.trailer)
	return r.err
}

// upstreamError: the node did not answer. Its cause (a dial's text names the pod's address) never reaches a client.
type upstreamError struct{ cause error }

func (e upstreamError) Error() string { return "hugr: the node did not answer" }

func (e upstreamError) GRPCStatus() *status.Status { return status.New(codes.Unavailable, e.Error()) }

// forward the call to the node. With `replay`, `first` (the client's first message, already read) is sent first, and
// the rest of the client's messages only after the node's first answer; `seating` makes a refusal of the node's that came before anything
// else a *refusal.
func (p *Proxy) forward(client grpc.ServerStream, n *fleet.Node, name string, md metadata.MD, replay bool,
	open *opening) error {
	conn, err := p.conn(n)
	if err != nil {
		return &refusal{kind: route.Unreachable, err: upstreamError{err}}
	}
	ctx, cancel := context.WithCancel(client.Context())
	defer cancel()
	ctx = metadata.NewOutgoingContext(ctx, forwarded(md))
	up, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, service+name)
	if err != nil {
		// nothing reached the node: another node may take the call, whatever it is
		return &refusal{kind: route.Unreachable, err: upstreamError{err}}
	}
	if open.sent {
		open.first.Ref() // this send's own: gRPC frees it once sent
		if err := up.SendMsg(&frame{data: open.first}); err != nil && err != io.EOF {
			return upstreamError{err}
		}
		if !replay { // a stream's first message, sent: never sent again, so not held for the call's length
			open.first.Free()
			open.first = nil
		}
	}
	if open.second != nil { // a stream's second message, read with the first: sent once, gRPC frees it
		second := open.second
		open.second = nil
		if err := up.SendMsg(second); err != nil && err != io.EOF {
			return upstreamError{err}
		}
	}
	if !replay {
		pending := open.pending // a receive the front stopped waiting for: the pump takes its message first
		open.pending = nil
		go pump(client, up, cancel, pending) // the rest of a stream
	} else if oneRequest[name] || !open.sent || open.closed {
		_ = up.CloseSend() // the client sent all it will
	}
	// the node's first answer: its header, then its first message or its status
	header, err := up.Header()
	if err != nil {
		return upstreamError{err}
	}
	var f frame
	err = up.RecvMsg(&f)
	if err != nil && err != io.EOF {
		trailer := up.Trailer()
		if replay {
			st := status.Convert(err)
			kind, groups := route.FlightRefusal(st.Code() == codes.Unavailable, st.Message(),
				trailer.Get(route.GroupsHeader))
			if kind != route.None {
				return &refusal{kind: kind, groups: groups, header: forwarded(header), trailer: forwarded(trailer),
					err: err}
			}
		}
		if len(header) > 0 {
			_ = client.SetHeader(forwarded(header))
		}
		client.SetTrailer(forwarded(trailer))
		return err
	}
	if sendErr := client.SendHeader(forwarded(header)); sendErr != nil {
		f.free()
		return sendErr
	}
	if replay && open.sent && !open.closed && !oneRequest[name] {
		go pump(client, up, cancel, nil) // a Handshake: the client's next messages
	}
	// the node's messages to the client until its status
	for err == nil {
		if sendErr := client.SendMsg(&f); sendErr != nil {
			return sendErr // the client went: cancel() ends the node's side, whose trailer is not read while it is open
		}
		f = frame{}
		err = up.RecvMsg(&f)
	}
	client.SetTrailer(forwarded(up.Trailer()))
	if err == io.EOF {
		return nil
	}
	return err
}

// prefix of a message, at most n bytes, copied (the message is not).
func prefix(b mem.BufferSlice, n int) []byte {
	out := make([]byte, 0, min(n, b.Len()))
	for _, buf := range b {
		data := buf.ReadOnlyData()
		if len(out)+len(data) > n {
			data = data[:n-len(out)]
		}
		if out = append(out, data...); len(out) >= n {
			break
		}
	}
	return out
}

// pump the client's messages to the node until the client half-closes; a client that fails ends the call.
func pump(client grpc.ServerStream, up grpc.ClientStream, cancel context.CancelFunc, pending <-chan received) {
	for {
		var f frame
		var err error
		if pending != nil {
			r := <-pending
			pending = nil
			f, err = r.f, r.err
		} else {
			err = client.RecvMsg(&f)
		}
		if err != nil {
			if err == io.EOF {
				_ = up.CloseSend()
			} else {
				cancel()
			}
			return
		}
		if err := up.SendMsg(&f); err != nil {
			return // the node's status comes on the other side
		}
	}
}

// forwarded is the metadata as the other side may receive it: gRPC's own keys are its transport's, not a call's.
func forwarded(md metadata.MD) metadata.MD {
	out := metadata.MD{}
	for k, v := range md {
		if strings.HasPrefix(k, ":") || strings.HasPrefix(k, "grpc-") || k == "content-type" || k == "user-agent" ||
			k == "te" || k == "connection" {
			continue
		}
		out[k] = v
	}
	return out
}

// frame is one message, never decoded: its buffers are passed on without a copy.
type frame struct{ data mem.BufferSlice }

func (f *frame) free() {
	f.data.Free()
	f.data = nil
}

// codec passes frames through, counting what it receives: from the client (in) or the node (out).
type codec struct{ direction string }

func (codec) Marshal(v any) (mem.BufferSlice, error) {
	f, ok := v.(*frame)
	if !ok {
		return nil, errors.New("hugr: not a frame")
	}
	out := f.data // gRPC frees these once sent
	f.data = nil
	return out, nil
}

func (c codec) Unmarshal(data mem.BufferSlice, v any) error {
	f, ok := v.(*frame)
	if !ok {
		return errors.New("hugr: not a frame")
	}
	data.Ref() // gRPC frees `data` when this returns
	f.data = data
	metrics.Bytes.WithLabelValues("flight", c.direction).Add(float64(data.Len()))
	return nil
}

// Name is proto's: the content type stays the one a Flight server expects.
func (codec) Name() string { return "proto" }
