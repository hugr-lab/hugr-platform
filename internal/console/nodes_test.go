package console

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/flight"
	"github.com/apache/arrow-go/v18/arrow/flight/flightsql"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
)

// fakeDoor is a node's Flight SQL door as the console sees it: a bearer on every call, a session cookie from the
// first, a statement's answer at DoGet.
type fakeDoor struct {
	flightsql.BaseServer
	mu       sync.Mutex
	sessions map[string]string // cookie value -> principal
	seen     []string          // "<method> <token> <cookie>"
	closed   []string
	issued   int
	// load is how the door answers the Handshake `node-load`: "" the report, "off" its endpoint off, "point" a person
	// who reads the view by a point grant alone (no observe)
	load string
}

// loadReport is a node's `acl_node_load()` as duckdb-acl writes it (spec 079, 096, 175).
const loadReport = `{"draining":false,"observe":"token","group":"etl","group_known":true,"group_default":false,` +
	`"label":"hugr-node-etl-a","config":{"target":7,"applied":6},` +
	`"sessions":{"live":0,"max":1000,"by_door":{"flight":0,"quack":0},"by_group":{"etl":{"live":0,"max":0}},` +
	`"idle_since":1763546400},` +
	`"memory":{"used":62914560,"working":40960,"limit":null,"temp_used":0,"temp_limit":null,"critical":false},` +
	`"admit":{"new_session":true,"new_quack_client":true,"new_stream":true}}`

// doorService is the door's Flight service: Flight SQL's, and its own Handshake.
type doorService struct {
	flight.FlightServer
	door *fakeDoor
}

func (s doorService) Handshake(stream flight.FlightService_HandshakeServer) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	token := strings.TrimPrefix(strings.Join(md.Get("authorization"), ""), "Bearer ")
	d := s.door
	d.mu.Lock()
	d.seen = append(d.seen, "handshake "+token+" "+strings.Join(md.Get("cookie"), "; "))
	mode := d.load
	d.mu.Unlock()
	request, err := stream.Recv()
	if err != nil {
		return err
	}
	if string(request.GetPayload()) != "node-load" {
		return nil // the drivers' payload-less handshake: a no-op
	}
	switch {
	case mode == "silent":
		return nil // an acl before spec 079: the payload is not one it knows, the exchange ends with nothing
	case mode == "off":
		return status.Error(codes.Unimplemented, "acl: the load report is off (acl_metrics_endpoint)")
	case token == "refused":
		return status.Error(codes.Unauthenticated, "acl: the bearer token did not verify")
	case mode == "point":
		return status.Error(codes.PermissionDenied, "acl: reading the load report needs the observe scope")
	}
	// the Handshake seats no session: no cookie is set
	return stream.Send(&flight.HandshakeResponse{Payload: []byte(loadReport)})
}

func (d *fakeDoor) authorize(ctx context.Context, method string) (string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	token := strings.TrimPrefix(strings.Join(md.Get("authorization"), ""), "Bearer ")
	cookie := strings.Join(md.Get("cookie"), "; ")
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = append(d.seen, method+" "+token+" "+cookie)
	if token == "refused" || strings.HasSuffix(token, ".forged") {
		// as duckdb-acl's door: its cookie middleware gives a cookie-less call one, refused or not, and the session
		// refuses a token it does not verify before the cookie counts - with UNKNOWN, not UNAUTHENTICATED
		if cookie == "" {
			_ = grpc.SetHeader(ctx, metadata.Pairs("set-cookie", "acl_flight_session_id=refused; HttpOnly; Path=/"))
		}
		return "", status.Error(codes.Unknown, "acl: authentication failed")
	}
	if token == "elsewhere" {
		_ = grpc.SetTrailer(ctx, metadata.Pairs("x-acl-resource-groups", "gen%20eral,etl"))
		return "", status.Error(codes.Unavailable, "acl: this node serves another resource group")
	}
	for _, part := range strings.Split(cookie, ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(part), "=")
		if name == "acl_flight_session_id" && d.sessions[value] == principalOf(token) {
			return value, nil
		}
	}
	d.issued++
	id := "s" + strconv.Itoa(d.issued)
	d.sessions[id] = principalOf(token)
	_ = grpc.SetHeader(ctx, metadata.Pairs("set-cookie", "acl_flight_session_id="+id+"; HttpOnly; Path=/",
		"set-cookie", "acl_node=hugr-node-etl-a; HttpOnly", "set-cookie", "other=x"))
	return id, nil
}

// principalOf a token as the fake door judges it: a JWT's issuer and subject, another token as itself (the node's own
// fingerprint holds more, and the same for one person's refreshed token)
func principalOf(token string) string {
	if c := claimsOf(token); c.iss != "" {
		return c.iss + "|" + c.sub
	}
	return token
}

func (d *fakeDoor) GetFlightInfoStatement(ctx context.Context, cmd flightsql.StatementQuery,
	_ *flight.FlightDescriptor) (*flight.FlightInfo, error) {
	if _, err := d.authorize(ctx, "info"); err != nil {
		return nil, err
	}
	ticket, err := flightsql.CreateStatementQueryTicket([]byte(cmd.GetQuery()))
	if err != nil {
		return nil, err
	}
	return &flight.FlightInfo{Endpoint: []*flight.FlightEndpoint{{Ticket: &flight.Ticket{Ticket: ticket}}},
		Schema: flight.SerializeSchema(answerSchema, memory.DefaultAllocator)}, nil
}

var loadSchema = arrow.NewSchema([]arrow.Field{
	{Name: "draining", Type: arrow.FixedWidthTypes.Boolean},
	{Name: "sessions_live", Type: arrow.PrimitiveTypes.Int64},
}, nil)

var answerSchema = arrow.NewSchema([]arrow.Field{
	{Name: "id", Type: arrow.BinaryTypes.String},
	{Name: "idle_seconds", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
	{Name: "roles", Type: arrow.ListOf(arrow.BinaryTypes.String)},
}, nil)

func (d *fakeDoor) DoGetStatement(ctx context.Context, ticket flightsql.StatementQueryTicket) (*arrow.Schema,
	<-chan flight.StreamChunk, error) {
	if _, err := d.authorize(ctx, "get"); err != nil {
		return nil, nil, err
	}
	if string(ticket.GetStatementHandle()) == stmtLoad {
		b := array.NewRecordBuilder(memory.DefaultAllocator, loadSchema)
		defer b.Release()
		b.Field(0).(*array.BooleanBuilder).Append(false)
		b.Field(1).(*array.Int64Builder).Append(3)
		ch := make(chan flight.StreamChunk, 1)
		ch <- flight.StreamChunk{Data: b.NewRecordBatch()}
		close(ch)
		return loadSchema, ch, nil
	}
	b := array.NewRecordBuilder(memory.DefaultAllocator, answerSchema)
	defer b.Release()
	b.Field(0).(*array.StringBuilder).AppendValues([]string{"0A1B2C3D4E5F", string(ticket.GetStatementHandle())}, nil)
	b.Field(1).(*array.Int64Builder).AppendValues([]int64{4, 0}, []bool{true, false})
	roles := b.Field(2).(*array.ListBuilder)
	roles.Append(true)
	roles.ValueBuilder().(*array.StringBuilder).AppendValues([]string{"etl_writer", "etl_reader"}, nil)
	roles.Append(true)
	ch := make(chan flight.StreamChunk, 1)
	ch <- flight.StreamChunk{Data: b.NewRecordBatch()}
	close(ch)
	return answerSchema, ch, nil
}

func (d *fakeDoor) CloseSession(ctx context.Context, _ *flight.CloseSessionRequest) (*flight.CloseSessionResult, error) {
	id, err := d.authorize(ctx, "close")
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.closed = append(d.closed, id)
	delete(d.sessions, id)
	d.mu.Unlock()
	return &flight.CloseSessionResult{Status: flight.CloseSessionResultClosed}, nil
}

// certificate for the group's TLS name, signed by a CA the test trusts.
func certificate(t *testing.T, name string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	return tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}, roots
}

func startDoor(t *testing.T) (*fakeDoor, fleet.Node, *x509.CertPool) {
	t.Helper()
	cert, roots := certificate(t, "hugr-node-etl.hugr.svc")
	door := &fakeDoor{sessions: map[string]string{}}
	server := flight.NewServerWithMiddleware(nil,
		grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}})))
	server.RegisterFlightService(doorService{FlightServer: flightsql.NewFlightServer(door), door: door})
	if err := server.Init("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve() }()
	t.Cleanup(server.Shutdown)
	host, port, _ := net.SplitHostPort(server.Addr().String())
	p, _ := strconv.Atoi(port)
	return door, fleet.Node{Label: "hugr-node-etl-a", Service: "hugr-node-etl", Address: host, FlightPort: int32(p)},
		roots
}

func TestPoolAgainstADoor(t *testing.T) {
	door, n, roots := startDoor(t)
	pool := &Pool{Namespace: "hugr", Roots: func(context.Context) (*x509.CertPool, error) { return roots, nil }}
	ctx := context.Background()

	result, err := pool.Query(ctx, n, "alice-token", "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	records := result.Records()
	if len(records) != 2 || records[0]["id"] != "0A1B2C3D4E5F" || records[0]["idle_seconds"] != int64(4) ||
		records[1]["idle_seconds"] != nil || records[1]["id"] != "SELECT 1" {
		t.Fatalf("the rows, a NULL as nil: %v", records)
	}
	if roles, ok := records[0]["roles"].([]any); !ok || len(roles) != 2 || roles[0] != "etl_writer" {
		t.Errorf("a list column: %#v", records[0]["roles"])
	}
	if _, err := pool.Query(ctx, n, "alice-token", "SELECT 2"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Query(ctx, n, "bob-token", "SELECT 3"); err != nil {
		t.Fatal(err)
	}
	door.mu.Lock()
	seen := append([]string{}, door.seen...)
	issued := door.issued
	door.mu.Unlock()
	// no session kept on the node: every call - a statement's GetFlightInfo, its DoGet - comes without a cookie, so the
	// node seats a session for that call alone, and the cookie it sets is never sent back
	if issued != len(seen) || len(seen) != 6 {
		t.Errorf("a session per call: %d issued for %d calls %q", issued, len(seen), seen)
	}
	for _, call := range seen {
		if !strings.HasSuffix(call, " ") {
			t.Errorf("a call carried a cookie: %q", call)
		}
	}
	if seen[1] != "get alice-token " {
		t.Errorf("the DoGet redeems its ticket by the bearer alone: %q", seen[1])
	}

	if at, ok := pool.CertificateExpiry(n.Label); !ok || at.Before(time.Now()) {
		t.Errorf("the doors' certificate's expiry, from the handshake: %v %v", at, ok)
	}
	if _, ok := pool.CertificateExpiry("never-spoken-to"); ok {
		t.Error("a node never spoken to has no known certificate")
	}
	_, err = pool.Query(ctx, n, "refused", "SELECT 1")
	var refused *NodeError
	if !errors.As(err, &refused) || !authFailed(err) || refused.Message != "acl: authentication failed" {
		t.Errorf("a refused token is the node's own answer: %v", err)
	}
	_, err = pool.Query(ctx, n, "elsewhere", "SELECT 1")
	if !errors.As(err, &refused) || len(refused.Groups) != 2 || refused.Groups[0] != "gen eral" ||
		refused.Groups[1] != "etl" {
		t.Errorf("the groups the refusal names, decoded: %v %+v", err, refused)
	}
}

func TestPoolUnreachableAndUntrusted(t *testing.T) {
	_, n, _ := startDoor(t)
	_, other := certificate(t, "hugr-node-etl.hugr.svc")
	pool := &Pool{Namespace: "hugr", Timeout: 2 * time.Second,
		Roots: func(context.Context) (*x509.CertPool, error) { return other, nil }}
	if _, err := pool.Query(context.Background(), n, "alice-token", "SELECT 1"); !errors.Is(err, errUnreachable) {
		t.Errorf("a node whose certificate the bundle does not sign is not spoken to: %v", err)
	}
	gone := n
	gone.FlightPort = 1
	gone.Label = "gone"
	if _, err := pool.Query(context.Background(), gone, "alice-token", "SELECT 1"); !errors.Is(err, errUnreachable) {
		t.Errorf("no listener: %v", err)
	}
	noDoor := n
	noDoor.FlightPort = 0
	if _, err := pool.Query(context.Background(), noDoor, "alice-token", "SELECT 1"); !errors.Is(err, errUnreachable) {
		t.Errorf("no Flight door: %v", err)
	}
}

func TestPrune(t *testing.T) {
	_, n, roots := startDoor(t)
	pool := &Pool{Namespace: "hugr", Roots: func(context.Context) (*x509.CertPool, error) { return roots, nil }}
	if _, err := pool.Query(context.Background(), n, "alice-token", "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	pool.Prune(func(string) bool { return true })
	if len(pool.channels) != 1 {
		t.Errorf("a live node's channel stays: %d", len(pool.channels))
	}
	pool.Prune(func(string) bool { return false })
	if len(pool.channels) != 0 {
		t.Errorf("a gone node's channel is closed: %d", len(pool.channels))
	}
}

func TestPoolLoadWithoutSession(t *testing.T) {
	door, n, roots := startDoor(t)
	pool := &Pool{Namespace: "hugr", Roots: func(context.Context) (*x509.CertPool, error) { return roots, nil }}
	ctx := context.Background()

	load, err := pool.Load(ctx, n, "alice-token")
	if err != nil {
		t.Fatal(err)
	}
	door.mu.Lock()
	seen, issued := append([]string{}, door.seen...), door.issued
	door.mu.Unlock()
	// spec 012: one Handshake with the person's bearer and no cookie, no statement - so no session on the node, whose
	// end would move its idle_since
	if len(seen) != 1 || seen[0] != "handshake alice-token " || issued != 0 {
		t.Fatalf("the report without a session: %q, %d sessions", seen, issued)
	}
	want := map[string]any{"draining": false, "observe": "token", "group": "etl", "group_known": true,
		"group_default": false, "config_target": int64(7), "config_applied": int64(6), "sessions_live": int64(0),
		"sessions_max": int64(1000), "sessions_idle_since": "2025-11-19T10:00:00", "memory_used": int64(62914560),
		"memory_working": int64(40960), "memory_limit": nil, "temp_used": int64(0), "temp_limit": nil,
		"memory_critical": false, "admit_new_session": true, "admit_new_quack_client": true, "admit_new_stream": true}
	for k, v := range want {
		if load[k] != v {
			t.Errorf("%s: %#v, want %#v", k, load[k], v)
		}
	}
	if doors, ok := load["sessions_by_door"].(map[string]any); !ok || doors["flight"] != int64(0) || len(doors) != 2 {
		t.Errorf("the sessions per door, an object: %#v", load["sessions_by_door"])
	}
	groups, _ := load["sessions_by_group"].(map[string]any)
	if g, ok := groups["etl"].(map[string]any); !ok || g["live"] != int64(0) || g["max"] != int64(0) {
		t.Errorf("the sessions per group: %#v", load["sessions_by_group"])
	}
	if _, kept := load["label"]; kept || len(load) != 21 {
		t.Errorf("only the view's columns: %d %v", len(load), load)
	}
	if _, ok := pool.CertificateExpiry(n.Label); !ok {
		t.Error("the Handshake shows the doors' certificate too")
	}

	// a node without the endpoint (an older bootstrap), a person who holds only a point grant on the view, and a node
	// that does not know the payload: the statement, as before
	for _, mode := range []string{"off", "point", "silent"} {
		door.mu.Lock()
		door.load, door.seen = mode, nil
		door.mu.Unlock()
		load, err := pool.Load(ctx, n, "alice-token")
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if load["sessions_live"] != int64(3) || load["draining"] != false {
			t.Errorf("%s: the view's row: %v", mode, load)
		}
		door.mu.Lock()
		seen := append([]string{}, door.seen...)
		door.mu.Unlock()
		if len(seen) != 3 || seen[1] != "info alice-token " || seen[2] != "get alice-token " {
			t.Errorf("%s: the Handshake, then the statement: %q", mode, seen)
		}
	}

	// a token the node does not verify is its answer, not a reason to try the statement
	door.mu.Lock()
	door.load, door.seen = "", nil
	door.mu.Unlock()
	_, err = pool.Load(ctx, n, "refused")
	var refused *NodeError
	if !errors.As(err, &refused) || refused.Code != codes.Unauthenticated {
		t.Errorf("a refused token: %v", err)
	}
	door.mu.Lock()
	if len(door.seen) != 1 {
		t.Errorf("no statement after a refused token: %q", door.seen)
	}
	door.mu.Unlock()

	noDoor := n
	noDoor.FlightPort = 0
	if _, err := pool.Load(ctx, noDoor, "alice-token"); !errors.Is(err, errUnreachable) {
		t.Errorf("no Flight door: %v", err)
	}
}

func TestFlatLoadRefusesNoReport(t *testing.T) {
	for _, report := range []string{"", "null", "[1]", "not json"} {
		if _, err := FlatLoad([]byte(report)); !errors.Is(err, errNoLoad) {
			t.Errorf("%q: %v", report, err)
		}
	}
	load, err := FlatLoad([]byte(`{"sessions":{"live":2,"idle_since":null}}`))
	if err != nil || load["sessions_idle_since"] != nil || load["sessions_live"] != int64(2) ||
		load["memory_used"] != nil {
		t.Errorf("a live node's report, an older node's without memory: %v %v", load, err)
	}
}

// said is a management statement's answer as the node gives it (duckdb-acl spec 179): ok and message, a NULL message
// when the call has nothing to say.
func said(ok any, message any) *Result {
	return &Result{Columns: []string{"ok", "message"}, Rows: [][]any{{ok, message}}}
}

// TestAnswerOf: the pair read by name, a STRUCT answer's own columns kept, an ok that is NULL or no boolean not ok,
// anything else no answer.
func TestAnswerOf(t *testing.T) {
	for _, c := range []struct {
		in   *Result
		want Answer
	}{
		{said(true, nil), Answer{OK: true}},
		{said(true, "2 sessions still open"), Answer{OK: true, Message: "2 sessions still open"}},
		{said(false, "the node was not draining"), Answer{Message: "the node was not draining"}},
		{said(nil, nil), Answer{}},
		{said("true", "x"), Answer{Message: "x"}},
	} {
		got, err := AnswerOf(c.in)
		if err != nil || got.OK != c.want.OK || got.Message != c.want.Message {
			t.Errorf("%v: %+v %v", c.in.Rows, got, err)
		}
	}
	cluster := &Result{Columns: []string{"ok", "message", "version", "class"}, Rows: [][]any{{true, nil, int64(3), "hot"}}}
	if got, err := AnswerOf(cluster); err != nil || !got.OK || got.Fields["version"] != int64(3) || got.Fields["class"] != "hot" {
		t.Errorf("a STRUCT answer's fields: %+v %v", got, err)
	}
	// the answers of before spec 179 - one column, the call's value - and no row are no answer: never read as done
	for _, r := range []*Result{
		{Columns: []string{"kill_session"}, Rows: [][]any{{true}}},
		{Columns: []string{"ok", "message"}},
		nil,
	} {
		if _, err := AnswerOf(r); err == nil {
			t.Errorf("%v read as an answer", r)
		}
	}
}

// TestPlainStructWithList: a struct whose field is a list - an object grant's columns, STRUCT(mode, names LIST) - is
// read whole, as arrow-go hands it (a map whose list field is still JSON): the names were lost before.
func TestPlainStructWithList(t *testing.T) {
	names := arrow.ListOf(arrow.BinaryTypes.String)
	b := array.NewStructBuilder(memory.DefaultAllocator, arrow.StructOf(
		arrow.Field{Name: "mode", Type: arrow.BinaryTypes.String}, arrow.Field{Name: "names", Type: names}))
	defer b.Release()
	b.Append(true)
	b.FieldBuilder(0).(*array.StringBuilder).Append("except")
	list := b.FieldBuilder(1).(*array.ListBuilder)
	list.Append(true)
	list.ValueBuilder().(*array.StringBuilder).Append("id")
	columns := b.NewStructArray()
	defer columns.Release()
	got, ok := plain(columns.GetOneForMarshal(0)).(map[string]any)
	if !ok {
		t.Fatalf("the struct: %#v", plain(columns.GetOneForMarshal(0)))
	}
	if n, ok := got["names"].([]any); !ok || len(n) != 1 || n[0] != "id" || got["mode"] != "except" {
		t.Fatalf("the struct's list field: %#v", got)
	}
	f := formOf("object", map[string]any{"role": "r", "catalog": "c", "object": "t", "columns": got, "masks": []any{}})
	if f.Columns == nil || f.Columns.Mode != "except" || len(f.Columns.Names) != 1 || f.Columns.Names[0] != "id" {
		t.Fatalf("the grant read: %#v", f.Columns)
	}
}
