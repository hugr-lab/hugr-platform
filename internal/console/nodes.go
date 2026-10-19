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

// Caller runs a statement on a node with a person's token.
type Caller interface {
	Query(ctx context.Context, node fleet.Node, token, statement string) (*Result, error)
	// SignOut closes the person's session on every node holding one; it answers how many it closed.
	SignOut(ctx context.Context, token string) int
}

// Pool is the console's Caller over Flight SQL (spec 003): one gRPC channel per node, one session per node and
// token - the first call opens it and the node's cookie keeps it (duckdb-acl spec 050). Each call carries the token as
// its bearer; the pool keeps a token's hash, never the token.
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
	sessions map[sessionKey]session
}

type channel struct {
	address string
	roots   *x509.CertPool
	client  *flightsql.Client
}

type sessionKey struct {
	label string
	token string // the token's hash
}

type session struct {
	cookie string // "name=value; name=value" of the node's cookies
	used   time.Time
}

// sessionIdle is how long a session the pool keeps for a token stays unused before it is forgotten: the node's own
// idle timeout (duckdb-acl spec 044, 900 s by default) ends it there.
const sessionIdle = 15 * time.Minute

// The cookies a node sets that the pool keeps: its session and its label (duckdb-acl specs 050 and 134).
var keptCookies = map[string]bool{"acl_flight_session_id": true, "acl_node": true}

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

// Query runs one statement on the node and reads its whole result.
func (p *Pool) Query(ctx context.Context, node fleet.Node, token, statement string) (*Result, error) {
	if node.FlightPort == 0 || node.Address == "" {
		return nil, errUnreachable
	}
	client, err := p.client(ctx, node)
	if err != nil {
		return nil, err
	}
	key := sessionKey{label: node.Label, token: tokenHash(token)}
	ctx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()
	var header, trailer metadata.MD
	info, err := client.Execute(p.outgoing(ctx, key, token), statement, grpc.Header(&header), grpc.Trailer(&trailer))
	p.keep(key, header)
	if err != nil {
		return nil, withGroups(nodeError(err), trailer)
	}
	result := &Result{}
	for _, endpoint := range info.Endpoint {
		if err := p.read(ctx, client, key, token, endpoint.Ticket, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (p *Pool) read(ctx context.Context, client *flightsql.Client, key sessionKey, token string, ticket *flight.Ticket,
	result *Result) error {
	var header metadata.MD
	reader, err := client.DoGet(p.outgoing(ctx, key, token), ticket, grpc.Header(&header))
	if err != nil {
		return nodeError(err)
	}
	defer reader.Release()
	p.keep(key, header)
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

// SignOut closes the sessions the pool holds for the token, on every node.
func (p *Pool) SignOut(ctx context.Context, token string) int {
	hash := tokenHash(token)
	type held struct {
		client *flightsql.Client
		cookie string
	}
	var closing []held
	p.mu.Lock()
	for key, s := range p.sessions {
		if key.token != hash {
			continue
		}
		if c := p.channels[key.label]; c != nil && s.cookie != "" {
			closing = append(closing, held{c.client, s.cookie})
		}
		delete(p.sessions, key)
	}
	p.mu.Unlock()
	closed := 0
	for _, h := range closing {
		callCtx, cancel := context.WithTimeout(ctx, p.timeout())
		md := metadata.Pairs("authorization", "Bearer "+token, "cookie", h.cookie)
		_, err := h.client.CloseSession(metadata.NewOutgoingContext(callCtx, md), &flight.CloseSessionRequest{})
		cancel()
		if err == nil {
			closed++
		}
	}
	return closed
}

// Prune closes the channels of nodes that are gone and forgets the sessions unused for long.
func (p *Pool) Prune(live func(label string) bool, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for label, c := range p.channels {
		if !live(label) {
			_ = c.client.Close()
			delete(p.channels, label)
		}
	}
	for key, s := range p.sessions {
		if now.Sub(s.used) > sessionIdle || p.channels[key.label] == nil {
			delete(p.sessions, key)
		}
	}
}

func (p *Pool) outgoing(ctx context.Context, key sessionKey, token string) context.Context {
	md := metadata.Pairs("authorization", "Bearer "+token)
	p.mu.Lock()
	if s, ok := p.sessions[key]; ok {
		md.Append("cookie", s.cookie)
	}
	p.mu.Unlock()
	return metadata.NewOutgoingContext(ctx, md)
}

// keep the cookies a node set on a response.
func (p *Pool) keep(key sessionKey, header metadata.MD) {
	set := header.Get("set-cookie")
	if len(set) == 0 {
		p.mu.Lock()
		if s, ok := p.sessions[key]; ok {
			s.used = time.Now()
			p.sessions[key] = s
		}
		p.mu.Unlock()
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessions == nil {
		p.sessions = map[sessionKey]session{}
	}
	s := p.sessions[key]
	s.cookie = mergeCookies(s.cookie, set)
	s.used = time.Now()
	p.sessions[key] = s
}

// mergeCookies adds the kept cookies of Set-Cookie lines to a Cookie header's value, a newer value replacing an older.
func mergeCookies(header string, set []string) string {
	var order []string
	values := map[string]string{}
	for _, pair := range strings.Split(header, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || name == "" {
			continue
		}
		if _, seen := values[name]; !seen {
			order = append(order, name)
		}
		values[name] = value
	}
	for _, line := range set {
		first, _, _ := strings.Cut(line, ";")
		name, value, ok := strings.Cut(strings.TrimSpace(first), "=")
		if !ok || !keptCookies[name] {
			continue
		}
		if _, seen := values[name]; !seen {
			order = append(order, name)
		}
		values[name] = value
	}
	parts := make([]string, 0, len(order))
	for _, name := range order {
		parts = append(parts, name+"="+values[name])
	}
	return strings.Join(parts, "; ")
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
		_ = c.client.Close()
		delete(p.channels, node.Label)
	}
	creds := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots,
		ServerName: node.Service + "." + p.Namespace + ".svc"})
	client, err := flightsql.NewClient(address, nil, nil, grpc.WithTransportCredentials(creds))
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
		return errUnreachable
	}
	s, ok := status.FromError(err)
	if !ok {
		return errUnreachable
	}
	switch s.Code() {
	case codes.DeadlineExceeded, codes.Canceled:
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
