package flightproxy

import (
	"context"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

// fakeNode answers Flight calls as a script says, with the raw codec: no proto anywhere in the test.
type fakeNode struct {
	label string
	port  int32
	// answer to one call: the messages the client sent so far, and the stream to answer on
	answer func(n *fakeNode, name string, got []string, s grpc.ServerStream) error
	mu     sync.Mutex
	calls  []string // method:first message
	srv    *grpc.Server
}

func (n *fakeNode) handle(_ any, s grpc.ServerStream) error {
	method, _ := grpc.MethodFromServerStream(s)
	name := strings.TrimPrefix(method, service)
	var f frame
	if err := s.RecvMsg(&f); err != nil && err != io.EOF {
		return err
	}
	first := string(f.data.Materialize())
	f.free()
	n.mu.Lock()
	n.calls = append(n.calls, name+":"+first)
	n.mu.Unlock()
	return n.answer(n, name, []string{first}, s)
}

func (n *fakeNode) callCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.calls)
}

func startNode(t *testing.T, label string, answer func(*fakeNode, string, []string, grpc.ServerStream) error) *fakeNode {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	n := &fakeNode{label: label, port: int32(l.Addr().(*net.TCPAddr).Port), answer: answer}
	n.srv = grpc.NewServer(grpc.UnknownServiceHandler(n.handle), grpc.ForceServerCodecV2(codec{direction: "out"}),
		grpc.MaxRecvMsgSize(math.MaxInt32))
	go func() { _ = n.srv.Serve(l) }()
	t.Cleanup(n.srv.Stop)
	return n
}

func send(s grpc.ServerStream, text string) error {
	return s.SendMsg(&frame{data: mem.BufferSlice{mem.SliceBuffer([]byte(text))}})
}

// ok answers with the node's cookie, its label as the message, and a trailer
func ok(n *fakeNode, _ string, got []string, s grpc.ServerStream) error {
	_ = s.SendHeader(metadata.Pairs("set-cookie", "acl_node="+n.label+"; HttpOnly; Path=/; SameSite=Strict"))
	if err := send(s, n.label+" got "+got[0]); err != nil {
		return err
	}
	s.SetTrailer(metadata.Pairs("x-node", n.label))
	return nil
}

func wrongGroup(groups string) func(*fakeNode, string, []string, grpc.ServerStream) error {
	return func(n *fakeNode, _ string, _ []string, s grpc.ServerStream) error {
		s.SetTrailer(metadata.Pairs("x-acl-resource-groups", groups))
		return status.Error(codes.Unavailable, `acl: this node serves resource group "x" - your roles are served by "`+groups+`"`)
	}
}

func draining(*fakeNode, string, []string, grpc.ServerStream) error {
	return status.Error(codes.Unavailable, "acl: node is draining - not accepting new sessions")
}

func member(n *fakeNode, group string, dflt, admit bool) fleet.Node {
	return fleet.Node{Label: n.label, Service: "hugr-node-x", Address: "127.0.0.1", FlightPort: n.port, Ready: true,
		Serving: true, Lease: &fleet.Lease{Until: time.Now().Add(time.Hour), Summary: nodeproto.Summary{
			Group: group, GroupDefault: dflt, AdmitSession: admit, SeatsLeft: -1}}}
}

type rig struct {
	fleet *fleet.Fleet
	conn  *grpc.ClientConn
}

func start(t *testing.T, nodes ...fleet.Node) *rig {
	t.Helper()
	f := fleet.New()
	f.Set(fleet.NewView(nodes))
	p := &Proxy{Fleet: f, Namespace: "hugr", NoNodeWait: 200 * time.Millisecond, Insecure: true}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := p.Server(nil)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(l.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodecV2(codec{direction: "in"}), grpc.MaxCallRecvMsgSize(math.MaxInt32)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &rig{fleet: f, conn: conn}
}

type answer struct {
	messages []string
	header   metadata.MD
	trailer  metadata.MD
	err      error
}

// call a method with messages, reading everything the node answers
func (r *rig) call(t *testing.T, name string, md metadata.MD, messages ...string) answer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if md != nil {
		ctx = metadata.NewOutgoingContext(ctx, md)
	}
	s, err := r.conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, service+name)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		if err := s.SendMsg(&frame{data: mem.BufferSlice{mem.SliceBuffer([]byte(m))}}); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.CloseSend()
	var a answer
	a.header, _ = s.Header()
	for {
		var f frame
		if err := s.RecvMsg(&f); err != nil {
			if err != io.EOF {
				a.err = err
			}
			break
		}
		a.messages = append(a.messages, string(f.data.Materialize()))
		f.free()
	}
	a.trailer = s.Trailer()
	return a
}

func TestSeatedThenRouted(t *testing.T) {
	a := startNode(t, "node-a", ok)
	b := startNode(t, "node-b", ok)
	r := start(t, member(a, "general", true, true), member(b, "etl", false, true))
	got := r.call(t, "GetFlightInfo", nil, "q1")
	if got.err != nil || len(got.messages) != 1 || got.messages[0] != "node-a got q1" {
		t.Fatalf("a new connection is seated on the default group's node: %+v", got)
	}
	if c := got.header.Get("set-cookie"); len(c) != 1 || !strings.HasPrefix(c[0], "acl_node=node-a") {
		t.Errorf("the node's cookie reaches the client: %q", c)
	}
	if got.trailer.Get("x-node")[0] != "node-a" {
		t.Errorf("the node's trailer reaches the client: %v", got.trailer)
	}
	// the client carries the cookie back: its calls go to its node, even one no longer placeable
	r.fleet.Set(fleet.NewView([]fleet.Node{member(a, "general", true, false), member(b, "etl", false, true)}))
	routed := r.call(t, "DoGet", metadata.Pairs("cookie", "acl_flight_session_id=s; acl_node=node-b"), "ticket")
	if routed.err != nil || routed.messages[0] != "node-b got ticket" {
		t.Errorf("a call with node-b's cookie: %+v", routed)
	}
	draining := r.call(t, "DoGet", metadata.Pairs("cookie", "acl_node=node-a"), "t2")
	if draining.err != nil || draining.messages[0] != "node-a got t2" {
		t.Errorf("a node that admits nobody new keeps its connections: %+v", draining)
	}
}

func TestReseatByGroup(t *testing.T) {
	a := startNode(t, "node-a", wrongGroup("etl"))
	b := startNode(t, "node-b", ok)
	r := start(t, member(a, "general", true, true), member(b, "etl", false, true))
	got := r.call(t, "Handshake", nil, "hello")
	if got.err != nil || len(got.messages) != 1 || got.messages[0] != "node-b got hello" {
		t.Fatalf("seated again on the group the node named: %+v", got)
	}
	if a.callCount() != 1 || b.callCount() != 1 {
		t.Errorf("each node got the call once: a %d, b %d", a.callCount(), b.callCount())
	}
	if c := got.header.Get("set-cookie"); len(c) != 1 || !strings.Contains(c[0], "node-b") {
		t.Errorf("the cookie is the node's that took it: %q", c)
	}
}

func TestReseatBudget(t *testing.T) {
	// three groups that each send the principal on: two re-seats, then the third node's refusal as it was
	a := startNode(t, "node-a", wrongGroup("g2"))
	b := startNode(t, "node-b", wrongGroup("g3"))
	c := startNode(t, "node-c", wrongGroup("g4"))
	d := startNode(t, "node-d", ok)
	r := start(t, member(a, "g1", true, true), member(b, "g2", false, true), member(c, "g3", false, true),
		member(d, "g4", false, true))
	got := r.call(t, "GetFlightInfo", nil, "q")
	if status.Code(got.err) != codes.Unavailable || !strings.Contains(status.Convert(got.err).Message(), "acl: this node serves") {
		t.Fatalf("the third refusal goes to the client: %+v", got)
	}
	if v := got.trailer.Get("x-acl-resource-groups"); len(v) != 1 || v[0] != "g4" {
		t.Errorf("with its trailer: %v", got.trailer)
	}
	if d.callCount() != 0 {
		t.Error("no fourth node")
	}
}

func TestReseatDrainingInTheGroup(t *testing.T) {
	a := startNode(t, "node-a", draining)
	b := startNode(t, "node-b", ok)
	other := startNode(t, "node-o", ok)
	r := start(t, member(a, "general", true, true), member(b, "general", true, true), member(other, "etl", false, true))
	for i := 0; i < 10; i++ {
		got := r.call(t, "GetFlightInfo", nil, "q"+strconv.Itoa(i))
		if got.err != nil || !strings.HasPrefix(got.messages[0], "node-b") {
			t.Fatalf("a draining node's refusal: another node of the same group, never another group: %+v", got)
		}
	}
	if other.callCount() != 0 {
		t.Error("another group's node took a call")
	}
}

func TestNoNode(t *testing.T) {
	a := startNode(t, "node-a", ok)
	r := start(t, member(a, "general", true, false), member(startNode(t, "node-e", ok), "etl", false, true))
	started := time.Now()
	got := r.call(t, "GetFlightInfo", nil, "q")
	if status.Code(got.err) != codes.Unavailable || status.Convert(got.err).Message() != NoNodeText(nil) {
		t.Fatalf("no placeable node in the group: %+v", got)
	}
	if time.Since(started) < 200*time.Millisecond {
		t.Error("the call did not wait for a node")
	}
	// a node of the group coming up during the wait takes it
	go func() {
		time.Sleep(50 * time.Millisecond)
		r.fleet.Set(fleet.NewView([]fleet.Node{member(a, "general", true, true)}))
	}()
	if late := r.call(t, "GetFlightInfo", nil, "q"); late.err != nil || late.messages[0] != "node-a got q" {
		t.Errorf("a node coming up during the wait: %+v", late)
	}
}

func TestUnreachableNode(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := &fakeNode{label: "node-dead", port: int32(l.Addr().(*net.TCPAddr).Port)}
	_ = l.Close()
	b := startNode(t, "node-b", ok)
	r := start(t, member(dead, "general", true, true), member(b, "general", true, true))
	for i := 0; i < 5; i++ {
		if got := r.call(t, "GetFlightInfo", nil, "q"); got.err != nil || got.messages[0] != "node-b got q" {
			t.Fatalf("a node that is gone: another node of the group: %+v", got)
		}
	}
}

func TestStreamsBothWays(t *testing.T) {
	echo := startNode(t, "node-a", func(n *fakeNode, name string, got []string, s grpc.ServerStream) error {
		for _, m := range got {
			if err := send(s, "echo "+m); err != nil {
				return err
			}
		}
		for {
			var f frame
			if err := s.RecvMsg(&f); err != nil {
				return nil
			}
			if err := send(s, "echo "+string(f.data.Materialize())); err != nil {
				return err
			}
			f.free()
		}
	})
	r := start(t, member(echo, "general", true, true))
	// DoPut is not replayable: it is streamed from the start; Handshake replays its first message, then streams
	for _, name := range []string{"DoPut", "Handshake"} {
		got := r.call(t, name, nil, "1", "2", "3")
		if got.err != nil || strings.Join(got.messages, ",") != "echo 1,echo 2,echo 3" {
			t.Errorf("%s: %+v", name, got)
		}
	}
	// a server stream of many messages
	many := startNode(t, "node-m", func(n *fakeNode, _ string, _ []string, s grpc.ServerStream) error {
		for i := 0; i < 500; i++ {
			if err := send(s, strconv.Itoa(i)); err != nil {
				return err
			}
		}
		return nil
	})
	r2 := start(t, member(many, "general", true, true))
	if got := r2.call(t, "DoGet", nil, "t"); got.err != nil || len(got.messages) != 500 || got.messages[499] != "499" {
		t.Errorf("a DoGet of 500 batches: %d %v", len(got.messages), got.err)
	}
}

func TestDoPutRefusalIsTheClients(t *testing.T) {
	a := startNode(t, "node-a", wrongGroup("etl"))
	b := startNode(t, "node-b", ok)
	r := start(t, member(a, "general", true, true), member(b, "etl", false, true))
	// a DoPut of batches is streamed on: once its node read some of it, it is not sent again
	got := r.call(t, "DoPut", nil, "descriptor", "batch")
	if status.Code(got.err) != codes.Unavailable || b.callCount() != 0 {
		t.Errorf("a call that streamed is not replayed: %+v, node-b %d", got, b.callCount())
	}
}

func TestEveryNodeSilent(t *testing.T) {
	var nodes []fleet.Node
	for _, label := range []string{"node-x", "node-y"} {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		n := &fakeNode{label: label, port: int32(l.Addr().(*net.TCPAddr).Port)}
		_ = l.Close()
		nodes = append(nodes, member(n, "general", true, true))
	}
	r := start(t, nodes...)
	got := r.call(t, "GetFlightInfo", nil, "q")
	if status.Code(got.err) != codes.Unavailable || status.Convert(got.err).Message() != "hugr: the node did not answer" {
		t.Errorf("nodes that do not answer are not 'no node': %v", got.err)
	}
}

func TestEmptyFirstMessageIsSent(t *testing.T) {
	// ListActions' request is Empty: zero bytes, still the one message the node waits for
	a := startNode(t, "node-a", ok)
	r := start(t, member(a, "general", true, true))
	if got := r.call(t, "ListActions", nil, ""); got.err != nil || len(got.messages) != 1 || got.messages[0] != "node-a got " {
		t.Errorf("an empty first message: %+v", got)
	}
}

func TestLimits(t *testing.T) {
	a := startNode(t, "node-a", ok)
	r := start(t, member(a, "general", true, true))
	got := r.call(t, "GetFlightInfo", nil, strings.Repeat("x", MaxFirstMessage+1))
	if status.Code(got.err) != codes.ResourceExhausted || a.callCount() != 0 {
		t.Errorf("a first message past the limit is refused before a node is asked: %v (node calls %d)", got.err, a.callCount())
	}
	if got := r.call(t, "GetFlightInfo", nil, strings.Repeat("x", MaxFirstMessage)); got.err != nil {
		t.Errorf("a first message at the limit: %v", got.err)
	}
	// no node: the caller, whose token nobody verified, is not told the default group's name
	r2 := start(t, member(a, "general", true, false))
	if got := r2.call(t, "GetFlightInfo", nil, "q"); status.Convert(got.err).Message() != NoNodeText(nil) {
		t.Errorf("the default group is named to a caller nobody verified: %v", got.err)
	}
}

func TestRoutedCallReseated(t *testing.T) {
	// JDBC opens with a Handshake, which the node answers with its cookie before any session is seated: the next
	// call carries node-a's cookie, and only then does node-a judge the principal's group
	a := startNode(t, "node-a", func(n *fakeNode, name string, got []string, s grpc.ServerStream) error {
		if name == "Handshake" {
			return ok(n, name, got, s)
		}
		return wrongGroup("etl")(n, name, got, s)
	})
	b := startNode(t, "node-b", ok)
	r := start(t, member(a, "general", true, true), member(b, "etl", false, true))
	hs := r.call(t, "Handshake", nil, "")
	if hs.err != nil || !strings.HasPrefix(hs.header.Get("set-cookie")[0], "acl_node=node-a") {
		t.Fatalf("the handshake: %+v", hs)
	}
	got := r.call(t, "GetFlightInfo", metadata.Pairs("cookie", "acl_node=node-a"), "q")
	if got.err != nil || got.messages[0] != "node-b got q" || !strings.Contains(got.header.Get("set-cookie")[0], "node-b") {
		t.Errorf("a routed call refused by its node's placement is seated in the group the node named: %+v", got)
	}
}

func TestHandshakeWithoutMessage(t *testing.T) {
	// a BasicAuth handshake (arrow-go, pyarrow): headers, then the end of the stream, no message
	a := startNode(t, "node-a", ok)
	r := start(t, member(a, "general", true, true))
	if got := r.call(t, "Handshake", metadata.Pairs("authorization", "Basic dTpw")); got.err != nil ||
		len(got.messages) != 1 || got.messages[0] != "node-a got " {
		t.Errorf("a handshake that sends no message: %+v", got)
	}
}

func TestRoutedDeadNode(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := &fakeNode{label: "node-dead", port: int32(l.Addr().(*net.TCPAddr).Port)}
	_ = l.Close()
	b := startNode(t, "node-b", ok)
	r := start(t, member(dead, "etl", false, true), member(b, "etl", false, true))
	// a call routed to a node that is gone: another node of its group, the call never reached the dead one
	if got := r.call(t, "GetFlightInfo", metadata.Pairs("cookie", "acl_node=node-dead"), "q"); got.err != nil ||
		got.messages[0] != "node-b got q" {
		t.Errorf("routed to a dead node: %+v", got)
	}
	// a DoPut whose stream to the node never opened: nothing of it was read yet, so another node takes it
	if got := r.call(t, "DoPut", metadata.Pairs("cookie", "acl_node=node-dead"), "batch"); got.err != nil ||
		got.messages[0] != "node-b got batch" {
		t.Errorf("a DoPut routed to a dead node: %+v", got)
	}
	// no other node: UNAVAILABLE, naming no address
	only := start(t, member(dead, "etl", false, true))
	got := only.call(t, "DoPut", metadata.Pairs("cookie", "acl_node=node-dead"), "batch")
	if status.Code(got.err) != codes.Unavailable || strings.Contains(status.Convert(got.err).Message(), "127.0.0.1") {
		t.Errorf("a DoPut to the one dead node: %v", got.err)
	}
}

func TestConcurrentSeats(t *testing.T) {
	a := startNode(t, "node-a", ok)
	b := startNode(t, "node-b", ok)
	r := start(t, member(a, "general", true, true), member(b, "general", true, true))
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := r.call(t, "GetFlightInfo", nil, "q"); got.err != nil {
				t.Errorf("a concurrent seat: %v", got.err)
			}
		}()
	}
	wg.Wait()
}

func TestForgetKeepsANodeThatIsOnlyNotReady(t *testing.T) {
	a := startNode(t, "node-a", ok)
	p := &Proxy{Insecure: true}
	n := member(a, "general", true, true)
	if _, err := p.conn(&n); err != nil {
		t.Fatal(err)
	}
	notReady := n
	notReady.Ready, notReady.Serving = false, false
	p.Forget(fleet.NewView([]fleet.Node{notReady}))
	if p.conns["node-a"] == nil {
		t.Error("a node that failed its readiness probe lost its connection, and the streams on it")
	}
	p.Forget(fleet.NewView(nil))
	if p.conns["node-a"] != nil {
		t.Error("a node the fleet no longer has keeps its connection")
	}
}

// protobuf by hand, as a client sends it: Any-wrapped Flight SQL messages carrying a handle
func pbField(number uint64, payload []byte) []byte {
	head := []byte{}
	for v := number<<3 | 2; ; v >>= 7 {
		if v < 0x80 {
			head = append(head, byte(v))
			break
		}
		head = append(head, byte(v)|0x80)
	}
	for v := uint64(len(payload)); ; v >>= 7 {
		if v < 0x80 {
			head = append(head, byte(v))
			break
		}
		head = append(head, byte(v)|0x80)
	}
	return append(head, payload...)
}

func pbAny(kind string, value []byte) []byte {
	return append(pbField(1, []byte("type.googleapis.com/arrow.flight.protocol.sql."+kind)), pbField(2, value)...)
}

func TestCookielessByHandle(t *testing.T) {
	// a client that keeps no cookie (ADBC's default): the node named itself in the ticket and the prepared handle
	echo := func(n *fakeNode, name string, got []string, s grpc.ServerStream) error {
		if err := send(s, n.label+" "+name); err != nil {
			return err
		}
		for {
			var f frame
			if err := s.RecvMsg(&f); err != nil {
				return nil
			}
			f.free()
			if err := send(s, n.label+" more"); err != nil {
				return err
			}
		}
	}
	a := startNode(t, "node-a", echo)
	b := startNode(t, "node-b", echo)
	r := start(t, member(a, "general", true, true), member(b, "general", true, true))
	handle := []byte("node-b.65BC150114D603160B8B3BDEF240A4F6")
	ticket := string(pbField(1, pbAny("TicketStatementQuery", pbField(1, handle))))
	prepared := string(append([]byte{0x08, 0x02}, pbField(2, pbAny("CommandPreparedStatementQuery", pbField(1, handle)))...))
	for i := 0; i < 8; i++ { // two random choices would put some on node-a
		if got := r.call(t, "DoGet", nil, ticket); got.err != nil || got.messages[0] != "node-b DoGet" {
			t.Fatalf("a ticket node-b minted: %+v", got)
		}
		if got := r.call(t, "GetFlightInfo", nil, prepared); got.err != nil || got.messages[0] != "node-b GetFlightInfo" {
			t.Fatalf("a prepared statement node-b holds: %+v", got)
		}
		// a prepared statement's ticket is its command
		preparedTicket := string(pbField(1, pbAny("CommandPreparedStatementQuery", pbField(1, handle))))
		if got := r.call(t, "DoGet", nil, preparedTicket); got.err != nil || got.messages[0] != "node-b DoGet" {
			t.Fatalf("a prepared statement's ticket: %+v", got)
		}
		// a DoPut binding the prepared statement's parameters: its descriptor first, its batches after
		put := string(pbField(1, []byte(prepared)))
		got := r.call(t, "DoPut", nil, put, "batch-1", "batch-2")
		if got.err != nil || strings.Join(got.messages, ",") != "node-b DoPut,node-b more,node-b more" {
			t.Fatalf("a DoPut on node-b's prepared statement: %+v", got)
		}
	}
	// an ad-hoc statement names no node: seated as any new call
	adhoc := string(append([]byte{0x08, 0x02}, pbField(2, pbAny("CommandStatementQuery", pbField(1, []byte("SELECT 1"))))...))
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		got := r.call(t, "GetFlightInfo", nil, adhoc)
		seen[strings.Fields(got.messages[0])[0]] = true
	}
	if !seen["node-a"] || !seen["node-b"] {
		t.Errorf("an ad-hoc statement is seated by two random choices: %v", seen)
	}
}

func TestExecuteUpdateReseated(t *testing.T) {
	// Flight SQL's ExecuteUpdate: a DoPut of its descriptor, then the end - a call the front can send again
	a := startNode(t, "node-a", wrongGroup("etl"))
	b := startNode(t, "node-b", ok)
	r := start(t, member(a, "general", true, true), member(b, "etl", false, true))
	update := string(append([]byte{0x08, 0x02}, pbField(2, pbAny("CommandStatementUpdate", pbField(1, []byte("DELETE"))))...))
	if got := r.call(t, "DoPut", nil, string(pbField(1, []byte(update)))); got.err != nil || got.messages[0] != "node-b got "+string(pbField(1, []byte(update))) {
		t.Errorf("a one-message DoPut refused by its node's placement is seated again: %+v", got)
	}
}

func TestHolderAnswersAlone(t *testing.T) {
	handle := []byte("node-b.65BC150114D603160B8B3BDEF240A4F6")
	ticket := string(pbField(1, pbAny("TicketStatementQuery", pbField(1, handle))))
	a := startNode(t, "node-a", ok)
	b := startNode(t, "node-b", draining)
	r := start(t, member(a, "general", true, true), member(b, "general", true, true))
	got := r.call(t, "DoGet", nil, ticket)
	if status.Code(got.err) != codes.Unavailable || !strings.Contains(status.Convert(got.err).Message(), "draining") ||
		a.callCount() != 0 {
		t.Errorf("the holder's refusal is the client's, no other node is asked: %v (node-a %d)", got.err, a.callCount())
	}
	// a holder the fleet does not have: refused by the front, no node asked
	r2 := start(t, member(a, "general", true, true))
	got = r2.call(t, "DoGet", nil, ticket)
	if status.Code(got.err) != codes.Unavailable || !strings.Contains(status.Convert(got.err).Message(), "not in the fleet") ||
		a.callCount() != 0 {
		t.Errorf("a holder gone: %v (node-a %d)", got.err, a.callCount())
	}
}

func TestSlowStreamKeepsItsOrder(t *testing.T) {
	// a DoPut whose second message comes after the front stopped waiting for it: the pump takes that receive
	echo := startNode(t, "node-a", func(n *fakeNode, _ string, got []string, s grpc.ServerStream) error {
		if err := send(s, "got "+got[0]); err != nil {
			return err
		}
		for {
			var f frame
			if err := s.RecvMsg(&f); err != nil {
				return nil
			}
			if err := send(s, "got "+string(f.data.Materialize())); err != nil {
				return err
			}
			f.free()
		}
	})
	r := start(t, member(echo, "general", true, true))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := r.conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, service+"DoPut")
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range []string{"descriptor", "batch-1", "batch-2"} {
		if i == 1 {
			time.Sleep(SecondWait + 300*time.Millisecond)
		}
		if err := st.SendMsg(&frame{data: mem.BufferSlice{mem.SliceBuffer([]byte(m))}}); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.CloseSend()
	var got []string
	for {
		var f frame
		if err := st.RecvMsg(&f); err != nil {
			break
		}
		got = append(got, string(f.data.Materialize()))
	}
	if strings.Join(got, ",") != "got descriptor,got batch-1,got batch-2" {
		t.Errorf("a slow stream through the front: %q", got)
	}
}
