// Package route is the front's decisions that need no network (spec 002): the node label a call carries, what a
// node's refusal to seat a connection says, and how many times a connection is seated again.
package route

import (
	"net/url"
	"strings"
)

// CookieName is the cookie both doors of a node set (duckdb-acl spec 134).
const CookieName = "acl_node"

// GroupsHeader names, on a node's wrong_resource_group refusal, the groups that serve the principal (duckdb-acl spec
// 096's addendum): a Flight trailer, a header of quack's CONNECT answer.
const GroupsHeader = "x-acl-resource-groups"

// ValidLabel is a node's label as duckdb-acl writes one: a DNS-1123 label, 1..63 of [a-z0-9-], a letter or a digit
// at both ends. Anything else a client sends is no label.
func ValidLabel(label string) bool {
	if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// CookieLabel is the label of the acl_node cookie in a call's Cookie headers (HTTP/2 may split them into several),
// read by the cookie's name - never a substring of another cookie's value. empty when none or not a label.
func CookieLabel(headers []string) string {
	for _, header := range headers {
		for _, piece := range strings.Split(header, ";") {
			name, value, ok := strings.Cut(piece, "=")
			if ok && strings.TrimSpace(name) == CookieName {
				if label := strings.TrimSpace(value); ValidLabel(label) {
					return label
				}
				return ""
			}
		}
	}
	return ""
}

// ConnectionLabel is the label a quack connection id starts with (`<label>.<random>`), empty when it has none.
func ConnectionLabel(id string) string {
	label, _, ok := strings.Cut(id, ".")
	if !ok || !ValidLabel(label) {
		return ""
	}
	return label
}

// Groups of a GroupsHeader value: percent-encoded names, joined by commas. A part that does not decode is dropped.
func Groups(header string) []string {
	var out []string
	for _, part := range strings.Split(header, ",") {
		name, err := url.PathUnescape(strings.TrimSpace(part))
		if err == nil && name != "" {
			out = append(out, name)
		}
	}
	return out
}

// Refusal is what a node's answer to a new connection says the front may act on.
type Refusal int

// The refusals. Anything else a node answers is the client's.
const (
	None Refusal = iota
	// WrongGroup: the node serves another resource group; Groups names the ones that serve the principal.
	WrongGroup
	// Busy: draining, or at capacity - another node of the same group may take it.
	Busy
	// Unreachable: the node did not answer at all (it went between the fleet's view and the dial).
	Unreachable
)

// Messages of the refusals a node answers in words only (duckdb-acl specs 066 and 079).
const (
	drainingText   = "acl: node is draining"
	atCapacityText = "acl: node at capacity"
	wrongGroupText = "acl: this node serves resource group"
)

// FlightRefusal of a status answered to a call that seats a session: UNAVAILABLE with the node's words, and the
// groups trailer on a wrong group.
func FlightRefusal(unavailable bool, message string, groupsTrailer []string) (Refusal, []string) {
	if !unavailable {
		return None, nil
	}
	switch {
	case strings.Contains(message, wrongGroupText):
		var groups []string
		for _, value := range groupsTrailer {
			groups = append(groups, Groups(value)...)
		}
		return WrongGroup, groups
	case strings.Contains(message, drainingText), strings.Contains(message, atCapacityText):
		return Busy, nil
	}
	return None, nil
}

// QuackRefusal of a node's answer to a CONNECT: its groups header, or the text of its error answer.
func QuackRefusal(groupsHeader string, errorText string) (Refusal, []string) {
	if groupsHeader != "" {
		if groups := Groups(groupsHeader); len(groups) > 0 {
			return WrongGroup, groups
		}
	}
	if strings.Contains(errorText, atCapacityText) || strings.Contains(errorText, drainingText) {
		return Busy, nil
	}
	return None, nil
}

// NamedGroups of the seat for a refusal's text: the groups a node named, never the Leases' defaults.
func (s *Seat) NamedGroups() []string {
	if s.Named {
		return s.Groups
	}
	return nil
}

// MaxReseats of each kind: at most two re-seats in another group, and two on another node of the same group.
const MaxReseats = 2

// Seat is one connection's seating: the groups it is seated in, and what was tried.
type Seat struct {
	Groups []string        // nil: every group's nodes
	Tried  map[string]bool // labels
	// Named: Groups came from a node's refusal, which it sends only to a principal whose token verified. The default
	// groups from the Leases are never named to a caller: nobody verified it yet.
	Named              bool
	byGroup, sameGroup int
}

// NewSeat of a new connection in these groups (the default ones; nil = every group).
func NewSeat(groups []string) *Seat {
	return &Seat{Groups: groups, Tried: map[string]bool{}}
}

// Next says whether to seat the connection again after the node `label` answered with `refusal`, and moves the
// seat to the groups it names. False: the node's answer goes to the client as it was.
func (s *Seat) Next(label string, refusal Refusal, groups []string) bool {
	s.Tried[label] = true
	switch refusal {
	case WrongGroup:
		if s.byGroup >= MaxReseats || len(groups) == 0 {
			return false
		}
		s.byGroup++
		s.Groups, s.Named = groups, true
		return true
	case Busy, Unreachable:
		if s.sameGroup >= MaxReseats {
			return false
		}
		s.sameGroup++
		return true
	}
	return false
}
