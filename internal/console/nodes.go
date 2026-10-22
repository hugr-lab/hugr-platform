package console

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apache/arrow-go/v18/arrow/flight"
	"github.com/apache/arrow-go/v18/arrow/flight/flightsql"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
	"github.com/hugr-lab/hugr-platform/internal/front/route"
)

// Result of a statement: its columns and rows, each value as encoding/json marshals it.
type Result struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

// Records are the rows as objects by column name.
func (r *Result) Records() []map[string]any {
	out := make([]map[string]any, 0, len(r.Rows))
	for _, row := range r.Rows {
		record := make(map[string]any, len(r.Columns))
		for i, c := range r.Columns {
			if i < len(row) {
				record[c] = row[i]
			}
		}
		out = append(out, record)
	}
	return out
}

// NodeError is a node's refusal: its gRPC code and its message, passed to the person as the message only.
type NodeError struct {
	Code    codes.Code
	Message string
	// Groups are the resource groups that serve the person, when the node refused it as another group's
	// (duckdb-acl spec 096: the trailer x-acl-resource-groups).
	Groups []string
}

func (e *NodeError) Error() string { return e.Message }

// Unreachable is a node that did not answer: no connection, or no answer before the deadline.
var errUnreachable = errors.New("the node did not answer")

// errTimedOut: the node took longer than the call's deadline - it may still be at it, so the call is not sent to
// another node (an unreachable one, still).
var errTimedOut = fmt.Errorf("%w in time", errUnreachable)

// Caller runs a statement on a node with a person's token.
type Caller interface {
	Query(ctx context.Context, node fleet.Node, token, statement string) (*Result, error)
}

// Pool is the console's Caller over Flight SQL (spec 003): one gRPC channel per node, and no session kept on any:
// every call carries the person's token as its bearer and no cookie, so the node seats a session for that call alone
// and closes it on the call's way out (duckdb-acl spec 050) - the console occupies a node's seats only while it asks,
// leaves nothing to idle out, and a draining node seats the call of a person who may resume it (spec 066's
// addendum). The pool keeps no token.
type Pool struct {
	// Namespace of the nodes: a node is dialled by its pod's address with its group's TLS name
	// (hugr-node-<group>.<ns>.svc), as the front does (spec 002).
	Namespace string
	// Roots are the platform's CA bundle; a new pool restarts the channels.
	Roots func(context.Context) (*x509.CertPool, error)
	// Timeout of one statement, 15 s when zero.
	Timeout time.Duration

	mu       sync.Mutex
	channels map[string]*channel
}

type channel struct {
	address  string
	roots    *x509.CertPool
	client   *flightsql.Client
	notAfter time.Time // the doors' certificate, as the last handshake showed it
}

// channelLinger is how long a channel replaced by a new address or CA bundle stays open for the calls still on it.
const channelLinger = time.Minute

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (p *Pool) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return 15 * time.Second
}

// callTimeout carries a statement's own deadline in its context: a read the node takes long over (a catalog's check
// against its sources, what exposes a source) waits longer than a page's quick read.
type callTimeout struct{}

// withCallTimeout gives the statements called with ctx this deadline instead of the pool's.
func withCallTimeout(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, callTimeout{}, d)
}

// maxMessage is the largest Arrow batch the console reads from a node: one DuckDB chunk of long texts (a view's SQL,
// a row filter) passes gRPC's default of 4 MiB.
const maxMessage = 64 << 20

// Query runs one statement on the node and reads its whole result.
func (p *Pool) Query(ctx context.Context, node fleet.Node, token, statement string) (*Result, error) {
	if node.FlightPort == 0 || node.Address == "" {
		return nil, errUnreachable
	}
	client, err := p.client(ctx, node)
	if err != nil {
		return nil, err
	}
	timeout := p.timeout()
	if d, ok := ctx.Value(callTimeout{}).(time.Duration); ok && d > 0 {
		timeout = d
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var header, trailer metadata.MD
	var remote peer.Peer
	info, err := client.Execute(outgoing(ctx, token), statement, grpc.Header(&header), grpc.Trailer(&trailer),
		grpc.Peer(&remote))
	p.handshake(node.Label, &remote)
	if err != nil {
		return nil, withGroups(nodeError(err), trailer)
	}
	// the cookies the node sets (its session's, its label's) are not sent back: the next call is a session of its own
	result := &Result{}
	for _, endpoint := range info.Endpoint {
		if err := p.read(ctx, client, token, endpoint.Ticket, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (p *Pool) read(ctx context.Context, client *flightsql.Client, token string, ticket *flight.Ticket,
	result *Result) error {
	// a ticket is redeemed by the principal that made it, without a cookie (duckdb-acl spec 134's addendum)
	reader, err := client.DoGet(outgoing(ctx, token), ticket)
	if err != nil {
		return nodeError(err)
	}
	defer reader.Release()
	if result.Columns == nil {
		for _, f := range reader.Schema().Fields() {
			result.Columns = append(result.Columns, f.Name)
		}
	}
	for reader.Next() {
		batch := reader.RecordBatch()
		for r := 0; r < int(batch.NumRows()); r++ {
			row := make([]any, batch.NumCols())
			for c := range row {
				column := batch.Column(c)
				if column.IsNull(r) {
					continue
				}
				row[c] = plain(column.GetOneForMarshal(r))
			}
			result.Rows = append(result.Rows, row)
		}
	}
	if err := reader.Err(); err != nil {
		return nodeError(err)
	}
	return nil
}

// plain turns a nested value (a list, a struct, a map: arrow hands those as JSON) into Go's own.
func plain(v any) any {
	raw, ok := v.(json.RawMessage)
	if !ok {
		return v
	}
	var out any
	if json.Unmarshal(raw, &out) != nil {
		return string(raw)
	}
	return out
}

// handshake keeps the expiry of the certificate the node answered with.
func (p *Pool) handshake(label string, remote *peer.Peer) {
	info, ok := remote.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.PeerCertificates) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.channels[label]; c != nil {
		c.notAfter = info.State.PeerCertificates[0].NotAfter
	}
}

// CertificateExpiry of a node's doors, once the console has spoken to it.
func (p *Pool) CertificateExpiry(label string) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.channels[label]; c != nil && !c.notAfter.IsZero() {
		return c.notAfter, true
	}
	return time.Time{}, false
}

// Prune closes the channels of nodes that are gone.
func (p *Pool) Prune(live func(label string) bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for label, c := range p.channels {
		if !live(label) {
			_ = c.client.Close()
			delete(p.channels, label)
		}
	}
}

// outgoing is a call's metadata: the person's bearer, nothing else.
func outgoing(ctx context.Context, token string) context.Context {
	return metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+token))
}

func (p *Pool) client(ctx context.Context, node fleet.Node) (*flightsql.Client, error) {
	roots, err := p.Roots(ctx)
	if err != nil {
		return nil, err
	}
	address := net.JoinHostPort(node.Address, strconv.Itoa(int(node.FlightPort)))
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.channels == nil {
		p.channels = map[string]*channel{}
	}
	if c := p.channels[node.Label]; c != nil {
		if c.address == address && c.roots == roots {
			return c.client, nil
		}
		// the calls still on it finish: closed once they are past their deadline
		old := c.client
		time.AfterFunc(channelLinger, func() { _ = old.Close() })
		delete(p.channels, node.Label)
	}
	creds := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots,
		ServerName: node.Service + "." + p.Namespace + ".svc"})
	client, err := flightsql.NewClient(address, nil, nil, grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMessage)))
	if err != nil {
		return nil, fmt.Errorf("the node %s: %w", node.Label, err)
	}
	p.channels[node.Label] = &channel{address: address, roots: roots, client: client}
	return client, nil
}

// withGroups adds the resource groups a refusal named.
func withGroups(err error, trailer metadata.MD) error {
	var refused *NodeError
	if errors.As(err, &refused) {
		for _, value := range trailer.Get(route.GroupsHeader) {
			refused.Groups = append(refused.Groups, route.Groups(value)...)
		}
	}
	return err
}

// nodeError is what the person learns of a failed call: an unreachable node, or the node's own message.
func nodeError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errTimedOut
	}
	s, ok := status.FromError(err)
	if !ok {
		return errUnreachable
	}
	switch s.Code() {
	case codes.DeadlineExceeded:
		return errTimedOut
	case codes.Canceled:
		return errUnreachable
	case codes.Unavailable:
		// the transport's own failure, not a node that answered "draining" (duckdb-acl spec 066)
		if s.Message() == "" || strings.Contains(s.Message(), "connection error") ||
			strings.Contains(s.Message(), "transport") {
			return errUnreachable
		}
	}
	return &NodeError{Code: s.Code(), Message: s.Message()}
}
