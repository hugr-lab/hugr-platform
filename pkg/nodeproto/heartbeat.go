package nodeproto

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// MaxReportBytes is the largest heartbeat body accepted.
const MaxReportBytes = 256 * 1024

// Report is the heartbeat's body (hugr-node spec 010). Unknown fields are ignored.
type Report struct {
	NodeID string          `json:"node_id"`
	Image  string          `json:"image"`
	DuckDB string          `json:"duckdb"`
	Config Config          `json:"config"`
	Drift  map[string]int  `json:"drift"`
	Load   json.RawMessage `json:"load"`
}

// Config is the node's convergence on the profile.
type Config struct {
	Target       int64     `json:"target"`
	Applied      int64     `json:"applied"`
	NeedsRestart bool      `json:"needs_restart"`
	Pending      []Pending `json:"pending"`
}

// Pending is an item that waits: its kind, name and class (restart, drain, or hot with the error's class).
type Pending struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Class string `json:"class"`
	Error string `json:"error,omitempty"`
}

// Load is the part of acl_node_load() the fleet reads.
type Load struct {
	Draining     bool   `json:"draining"`
	Group        string `json:"group"` // the policy's resource group the node serves (empty: none, it serves everyone)
	GroupKnown   *bool  `json:"group_known"`
	GroupDefault bool   `json:"group_default"` // the node's group is the policy's default (duckdb-acl spec 096 addendum)
	Sessions     struct {
		Live int64 `json:"live"`
		Max  int64 `json:"max"`
	} `json:"sessions"`
	Quack *struct {
		Doors []struct {
			SeatsLeft int64 `json:"seats_left"`
		} `json:"doors"`
	} `json:"quack"`
	Admit struct {
		NewSession bool `json:"new_session"`
	} `json:"admit"`
}

// ErrTooLarge is a report past MaxReportBytes.
var ErrTooLarge = errors.New("the heartbeat is larger than 256 KiB")

// ErrForeignNode is a report that names another node than the one whose token sent it: a node speaks only for
// itself (answered 409).
var ErrForeignNode = errors.New("the heartbeat names another node than its token's")

// ReadReport reads one report of at most MaxReportBytes and checks it is the token's node's.
func ReadReport(body io.Reader, tokenNode string) (Report, Load, error) {
	raw, err := io.ReadAll(io.LimitReader(body, MaxReportBytes+1))
	if err != nil {
		return Report{}, Load{}, err
	}
	if len(raw) > MaxReportBytes {
		return Report{}, Load{}, ErrTooLarge
	}
	var report Report
	if err := json.Unmarshal(raw, &report); err != nil {
		return Report{}, Load{}, fmt.Errorf("the heartbeat is not a JSON report: %w", err)
	}
	if report.NodeID != tokenNode {
		return Report{}, Load{}, ErrForeignNode
	}
	var load Load
	if len(report.Load) > 0 && string(report.Load) != "null" {
		if err := json.Unmarshal(report.Load, &load); err != nil {
			return Report{}, Load{}, fmt.Errorf("the heartbeat's load is not acl_node_load(): %w", err)
		}
	}
	return report, load, nil
}

// Summary is what the fleet keeps of a report: the Lease's annotations (spec 001). The front seats by it, the
// console shows it; the full report is not stored.
type Summary struct {
	Target       int64
	Applied      int64
	NeedsRestart bool
	AdmitSession bool
	Draining     bool
	Group        string // the resource group the node serves, as it reported (the front seats by it, spec 002)
	GroupKnown   *bool
	GroupDefault bool // where the front seats a principal whose group it does not know yet (spec 002)
	Sessions     int64
	SeatsLeft    int64 // -1: the node opens no quack door
}

// SummaryOf a report and its load.
func SummaryOf(report Report, load Load) Summary {
	seats := int64(-1)
	if load.Quack != nil {
		seats = 0
		for _, door := range load.Quack.Doors {
			seats += door.SeatsLeft
		}
	}
	return Summary{
		Target:       report.Config.Target,
		Applied:      report.Config.Applied,
		NeedsRestart: report.Config.NeedsRestart,
		AdmitSession: load.Admit.NewSession,
		Draining:     load.Draining,
		Group:        load.Group,
		GroupKnown:   load.GroupKnown,
		GroupDefault: load.GroupDefault,
		Sessions:     load.Sessions.Live,
		SeatsLeft:    seats,
	}
}

// The Lease's annotation keys.
const (
	AnnotationTarget       = "platform.hugr.io/profile-target"
	AnnotationApplied      = "platform.hugr.io/profile-applied"
	AnnotationNeedsRestart = "platform.hugr.io/needs-restart"
	AnnotationAdmitSession = "platform.hugr.io/admit-new-session"
	AnnotationDraining     = "platform.hugr.io/draining"
	AnnotationGroup        = "platform.hugr.io/resource-group"
	AnnotationGroupKnown   = "platform.hugr.io/group-known"
	AnnotationGroupDefault = "platform.hugr.io/group-default"
	AnnotationSessions     = "platform.hugr.io/sessions"
	AnnotationSeatsLeft    = "platform.hugr.io/seats-left"
)

// Annotations of a summary, as the Lease carries them.
func (s Summary) Annotations() map[string]string {
	out := map[string]string{
		AnnotationTarget:       strconv.FormatInt(s.Target, 10),
		AnnotationApplied:      strconv.FormatInt(s.Applied, 10),
		AnnotationNeedsRestart: strconv.FormatBool(s.NeedsRestart),
		AnnotationAdmitSession: strconv.FormatBool(s.AdmitSession),
		AnnotationDraining:     strconv.FormatBool(s.Draining),
		AnnotationGroup:        s.Group,
		AnnotationGroupDefault: strconv.FormatBool(s.GroupDefault),
		AnnotationSessions:     strconv.FormatInt(s.Sessions, 10),
		AnnotationSeatsLeft:    strconv.FormatInt(s.SeatsLeft, 10),
	}
	if s.GroupKnown != nil {
		out[AnnotationGroupKnown] = strconv.FormatBool(*s.GroupKnown)
	}
	return out
}

// ParseSummary reads a Lease's annotations back; a missing or unreadable value reads as its zero (a node that never
// reported admits nobody).
func ParseSummary(annotations map[string]string) Summary {
	num := func(key string) int64 {
		v, _ := strconv.ParseInt(annotations[key], 10, 64)
		return v
	}
	flag := func(key string) bool {
		v, _ := strconv.ParseBool(annotations[key])
		return v
	}
	s := Summary{
		Target:       num(AnnotationTarget),
		Applied:      num(AnnotationApplied),
		NeedsRestart: flag(AnnotationNeedsRestart),
		AdmitSession: flag(AnnotationAdmitSession),
		Draining:     flag(AnnotationDraining),
		Group:        annotations[AnnotationGroup],
		GroupDefault: flag(AnnotationGroupDefault),
		Sessions:     num(AnnotationSessions),
		SeatsLeft:    -1,
	}
	if v, ok := annotations[AnnotationSeatsLeft]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			s.SeatsLeft = n
		}
	}
	if v, ok := annotations[AnnotationGroupKnown]; ok {
		if b, err := strconv.ParseBool(v); err == nil {
			s.GroupKnown = &b
		}
	}
	return s
}
