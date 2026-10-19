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
)

// person is what the console knows of a token: what the ACL said, and who it is.
type person struct {
	rights  Rights
	subject string // the token's sub, for the audit's changed-by: read once a node has verified the token
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
// refuses the person and names the groups that serve it (duckdb-acl spec 096); the next try goes to one of those.
func askRights(ctx context.Context, nodes Caller, view *fleet.View, token string, now time.Time,
	rnd *rand.Rand) (Info, error) {
	candidates := answering(view, now)
	rnd.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	tried := map[string]bool{}
	var groups []string
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
		case errors.As(err, &refused) && refused.Code == codes.Unauthenticated:
			return Info{}, errRefused
		case errors.As(err, &refused) && (refused.Code == codes.InvalidArgument || refused.Code == codes.PermissionDenied):
			// the node judged the read and refused it: a principal with no administration at all, whose rights are
			// none (the console shows it nothing)
			return Info{}, nil
		case errors.As(err, &refused) && len(refused.Groups) > 0:
			groups = refused.Groups
		case errors.As(err, &refused) && refused.Code == codes.Unavailable:
			// draining, at capacity: another node
		case errors.Is(err, errUnreachable):
		default:
			return Info{}, err
		}
	}
	return Info{}, errNoNode
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

// subjectOf a token the node has verified: its `sub`. The console verifies nothing itself - the node accepted this
// very token, so its claims are the IdP's.
func subjectOf(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	if len(claims.Sub) > 256 {
		return claims.Sub[:256]
	}
	return claims.Sub
}
