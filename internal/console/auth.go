package console

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
)

// rightsTTL is how long the console keeps what the ACL answered for a token (spec 003: 60 s).
const rightsTTL = 60 * time.Second

// rightsMax bounds the cache: past it, expired entries go first, then the oldest.
const rightsMax = 4096

// rightsTries is how many nodes the console asks before it says no node answers.
const rightsTries = 3

var (
	// errNoNode: no node answered, so the console cannot learn anyone's rights (fail closed, spec 003 Q2).
	errNoNode = errors.New("no node answers")
	// errRefused: the nodes refused the token - the sign-in's problem.
	errRefused = errors.New("the nodes refused the token")
	// errNodeFailed: a node answered, but with a failure of its own (its policy source, a schema it does not read):
	// nothing is learnt of the person, and nothing is cached.
	errNodeFailed = errors.New("the nodes failed reading the rights")
)

// tokenSkew is how long past its `exp` the console still takes a token: the clocks of the IdP and the operator.
const tokenSkew = 30 * time.Second

// person is what the console knows of a token: what the ACL said, and who it is.
type person struct {
	rights Rights
	// subject is who the token names, `<iss>|<sub>` (duckdb-acl's author form, spec 120.5), for the audit's
	// changed-by and the log: read once a node has verified the token
	subject string
	until   time.Time
	used    time.Time
}

// rightsCache keeps a token's rights by the token's hash, never the token.
type rightsCache struct {
	mu      sync.Mutex
	entries map[string]*person
}

func (c *rightsCache) get(hash string, now time.Time) (*person, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.entries[hash]
	if !ok || now.After(p.until) {
		return nil, false
	}
	p.used = now
	return p, true
}

func (c *rightsCache) put(hash string, p *person, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]*person{}
	}
	if len(c.entries) >= rightsMax {
		for h, e := range c.entries {
			if now.After(e.until) {
				delete(c.entries, h)
			}
		}
		for len(c.entries) >= rightsMax {
			var oldest string
			for h, e := range c.entries {
				if oldest == "" || e.used.Before(c.entries[oldest].used) {
					oldest = h
				}
			}
			delete(c.entries, oldest)
		}
	}
	c.entries[hash] = p
}

func (c *rightsCache) forget(hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, hash)
}

// answering are the nodes a statement may go to: ready, serving, with a Flight door and a current Lease.
func answering(view *fleet.View, now time.Time) []*fleet.Node {
	var out []*fleet.Node
	if view == nil {
		return out
	}
	for _, n := range view.Nodes() {
		if n.Ready && n.Serving && n.FlightPort != 0 && n.Current(now) {
			out = append(out, n)
		}
	}
	return out
}

// askRights asks `platform.console_info()` with the token on an answering node. A node of another resource group
// refuses the person and names the groups that serve it (duckdb-acl spec 096); the next try goes to one of those. A
// node that seats no session for the token answers "authentication failed" - for a token it does not verify, but also
// at its session cap or with its policy source failing (duckdb-acl specs 040, 044) - so the next node is asked too, and
// the token is refused only when every node that answered refused it so.
func askRights(ctx context.Context, nodes Caller, view *fleet.View, token string, now time.Time,
	rnd *rand.Rand) (Info, error) {
	// a draining node seats only the call of a person who may resume it (duckdb-acl spec 066's addendum): it is asked
	// last, so everyone else is answered by a node that serves - and an operator still is when all of them drain
	var serving, draining []*fleet.Node
	for _, n := range answering(view, now) {
		if n.Lease != nil && n.Lease.Summary.Draining {
			draining = append(draining, n)
		} else {
			serving = append(serving, n)
		}
	}
	rnd.Shuffle(len(serving), func(i, j int) { serving[i], serving[j] = serving[j], serving[i] })
	rnd.Shuffle(len(draining), func(i, j int) { draining[i], draining[j] = draining[j], draining[i] })
	candidates := append(serving, draining...)
	tried := map[string]bool{}
	var groups []string
	refusedToken, failed := 0, 0
	for attempt := 0; attempt < rightsTries; attempt++ {
		var pick *fleet.Node
		for _, n := range candidates {
			if tried[n.Label] {
				continue
			}
			if len(groups) == 0 || slices.Contains(groups, n.Group()) {
				pick = n
				break
			}
		}
		if pick == nil {
			break
		}
		tried[pick.Label] = true
		result, err := nodes.Query(ctx, *pick, token, stmtInfo)
		if err == nil {
			return infoOf(result)
		}
		var refused *NodeError
		switch {
		case authFailed(err):
			refusedToken++
		case errors.As(err, &refused) && noAdministration(refused):
			// the node verified the token and refused the read: a principal with no administration at all, whose
			// rights are none (the console shows it nothing)
			return Info{}, nil
		case errors.As(err, &refused) && len(refused.Groups) > 0:
			groups = refused.Groups
		case errors.As(err, &refused) && refused.Code == codes.Unavailable:
			// draining, at capacity: another node
		case errors.As(err, &refused):
			// the node's own failure (duckdb-acl answers any engine error as INVALID_ARGUMENT): another node
			failed++
		case errors.Is(err, errUnreachable):
		default:
			return Info{}, err
		}
	}
	switch {
	case refusedToken > 0 && failed == 0:
		return Info{}, errRefused
	case failed > 0:
		return Info{}, errNodeFailed
	}
	return Info{}, errNoNode
}

// authFailed: the node seated no session for the token (duckdb-acl answers UNKNOWN "acl: authentication failed").
func authFailed(err error) bool {
	var refused *NodeError
	return errors.As(err, &refused) && (refused.Code == codes.Unauthenticated ||
		refused.Code == codes.Unknown && strings.Contains(refused.Message, "authentication failed"))
}

// noAdministration is the ACL's refusal of console_info to a principal that administers nothing.
func noAdministration(refused *NodeError) bool {
	return refused.Code == codes.InvalidArgument && strings.Contains(refused.Message, "no access to table function") &&
		strings.Contains(refused.Message, "console_info")
}

// infoOf the single row console_info answers.
func infoOf(result *Result) (Info, error) {
	records := result.Records()
	if len(records) != 1 {
		return Info{}, errors.New("platform.console_info answered no row")
	}
	raw, err := json.Marshal(records[0])
	if err != nil {
		return Info{}, err
	}
	var info Info
	if err := json.Unmarshal(raw, &info); err != nil {
		return Info{}, err
	}
	return info, nil
}

// subjectOf a token the node has verified: `<iss>|<sub>` (the issuer left out when the token names none). The
// console verifies nothing itself - the node accepted this very token, so its claims are the IdP's.
func subjectOf(token string) string {
	c := claimsOf(token)
	if c.iss == "" {
		return c.sub
	}
	return c.iss + "|" + c.sub
}

// tokenClaims are what the console reads of a token, never verified - the node does that.
type tokenClaims struct {
	iss, sub string
	exp      time.Time // zero when the token states none
}

// claimsOf a JWT: its `iss`, `sub` (each cut at 256 bytes) and `exp`; nothing for another shape.
func claimsOf(token string) tokenClaims {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return tokenClaims{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return tokenClaims{}
	}
	var claims struct {
		Iss string  `json:"iss"`
		Sub string  `json:"sub"`
		Exp float64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return tokenClaims{}
	}
	out := tokenClaims{iss: cut(claims.Iss, 256), sub: cut(claims.Sub, 256)}
	if claims.Exp > 0 && claims.Exp < 1<<40 {
		out.exp = time.Unix(int64(claims.Exp), 0)
	}
	return out
}

// current says whether the console takes the token at `now`: a JWT whose `exp` has not passed. Read unverified, it can
// only refuse: no node is asked for a token past its time (each call's session opens anew, where the node judges it
// too), and the rights cached for a token end with it.
func current(token string, now time.Time) bool {
	exp := claimsOf(token).exp
	return !exp.IsZero() && now.Before(exp.Add(tokenSkew))
}

func cut(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
