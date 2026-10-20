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
	server.RegisterFlightService(flightsql.NewFlightServer(door))
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
