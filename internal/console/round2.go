package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
)

// Part is one read of a page made of several: its rows, or what the node said instead (a page shows what it can).
type Part struct {
	Rows  []map[string]any `json:"rows"`
	Error string           `json:"error,omitempty"`
}

func (s *Server) part(ctx context.Context, n fleet.Node, token, statement string) Part {
	result, err := s.Nodes.Query(ctx, n, token, statement)
	if err != nil {
		return Part{Rows: []map[string]any{}, Error: personMessage(err)}
	}
	return Part{Rows: result.Records()}
}

// certificates is the pool's knowledge of a node's certificate (the Pool has it, a test's Caller may not).
type certificates interface {
	CertificateExpiry(label string) (time.Time, bool)
}

func (s *Server) kubernetes(w http.ResponseWriter, r *http.Request, _ string, _ *person) {
	n, ok := s.nodeOf(w, r)
	if !ok {
		return
	}
	if s.Kube == nil {
		problem(w, http.StatusServiceUnavailable, "kubernetes", "the console reads no pod here")
		return
	}
	pod, err := s.Kube.Pod(r.Context(), n.Label)
	if err != nil {
		problem(w, http.StatusServiceUnavailable, "kubernetes", "the node's pod could not be read")
		return
	}
	events, err := s.Kube.Events(r.Context(), n.Label)
	if err != nil {
		events = nil // the page shows the pod; the events are what is missing
	}
	out := podAnswer(pod, events)
	if usage, err := s.Kube.Usage(r.Context(), n.Label); err != nil {
		out.UsageError = "the metrics API did not answer"
	} else if usage == nil {
		out.UsageError = "the cluster has no metrics API (metrics-server), or no sample of the pod yet"
	} else {
		out.Usage = usage
	}
	if c, ok := s.Nodes.(certificates); ok {
		if at, ok := c.CertificateExpiry(n.Label); ok {
			out.CertificateExpiry = &at
		}
	}
	answer(w, out)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request, _ string, p *person) {
	n, ok := s.nodeOf(w, r)
	if !ok {
		return
	}
	if s.Kube == nil {
		problem(w, http.StatusServiceUnavailable, "kubernetes", "the console reads no pod here")
		return
	}
	lines := int64(100)
	if v := r.URL.Query().Get("lines"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil || parsed < 1 || parsed > logLinesMax {
			problem(w, http.StatusBadRequest, "bad_request", "lines is 1 to 500")
			return
		}
		lines = parsed
	}
	previous := r.URL.Query().Get("previous") == "true"
	text, err := s.Kube.Logs(r.Context(), n.Label, previous, lines)
	if err != nil {
		if previous {
			problem(w, http.StatusNotFound, "no_previous", errNoPrevious.Error())
			return
		}
		problem(w, http.StatusServiceUnavailable, "kubernetes", "the node's log could not be read")
		return
	}
	s.Log.Info("node log read", "node", n.Label, "by", p.subject, "previous", previous, "lines", lines)
	answer(w, map[string]any{"text": text, "previous": previous, "lines": lines})
}

// MemoryAnswer is the engine's memory on the node: duckdb's own numbers, read with the person's token.
type MemoryAnswer struct {
	Tags   Part   `json:"tags"`
	Spill  *int64 `json:"spillBytes"`
	Limits Part   `json:"limits"`
	Error  string `json:"error,omitempty"`
}

func (s *Server) memory(w http.ResponseWriter, r *http.Request, token string, _ *person) {
	n, ok := s.nodeOf(w, r)
	if !ok {
		return
	}
	out := MemoryAnswer{Tags: s.part(r.Context(), *n, token, stmtMemory), Limits: s.part(r.Context(), *n, token, stmtLimits)}
	if spill := s.part(r.Context(), *n, token, stmtSpill); spill.Error == "" {
		var total int64
		for _, row := range spill.Rows {
			if v, ok := row["size"].(int64); ok {
				total += v
			}
		}
		out.Spill = &total
	} else {
		out.Error = spill.Error
	}
	answer(w, out)
}

// InventoryAnswer is what the node came up with: its extensions, and against its profile its sources and settings
// (drift) and the items that apply to it - the profile's views are the cluster bundle's.
type InventoryAnswer struct {
	Extensions Part `json:"extensions"`
	Drift      Part `json:"drift"`
	Effective  Part `json:"effective"`
}

func (s *Server) inventory(w http.ResponseWriter, r *http.Request, token string, p *person) {
	n, ok := s.nodeOf(w, r)
	if !ok {
		return
	}
	out := InventoryAnswer{Extensions: s.part(r.Context(), *n, token, stmtExtensions)}
	if p.rights.Infrastructure {
		out.Drift = s.part(r.Context(), *n, token, stmtDrift)
		out.Effective = s.part(r.Context(), *n, token, stmtEffective)
	} else {
		none := Part{Rows: []map[string]any{}, Error: "the cluster right reads the node against its profile"}
		out.Drift, out.Effective = none, none
	}
	answer(w, out)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, token string, _ *person) {
	n, ok := s.nodeOf(w, r)
	if !ok {
		return
	}
	result, err := s.Nodes.Query(r.Context(), *n, token, stmtEvents)
	if err != nil {
		nodeProblem(w, err)
		return
	}
	answer(w, map[string]any{"events": result.Records()})
}

// TracingChange is `POST nodes/{label}/sessions/{id}/tracing`: either level, "" inherits.
type TracingChange struct {
	Audit   *string `json:"audit,omitempty"`
	Profile *string `json:"profile,omitempty"`
}

func (s *Server) tracing(w http.ResponseWriter, r *http.Request, token string, p *person) {
	var change TracingChange
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&change); err != nil || (change.Audit == nil && change.Profile == nil) {
		problem(w, http.StatusBadRequest, "bad_request", "send {\"audit\": level} and/or {\"profile\": level}")
		return
	}
	id := r.PathValue("id")
	var statements []string
	if change.Audit != nil {
		statement, err := auditLevel(id, *change.Audit)
		if err != nil {
			problem(w, http.StatusBadRequest, "bad_request", "audit is '', off, denied, decisions or all")
			return
		}
		statements = append(statements, statement)
	}
	if change.Profile != nil {
		statement, err := profileLevel(id, *change.Profile)
		if err != nil {
			problem(w, http.StatusBadRequest, "bad_request", "profile is '', sampled or all")
			return
		}
		statements = append(statements, statement)
	}
	if _, err := killSession(id); err != nil {
		problem(w, http.StatusBadRequest, "bad_request", "not a session id")
		return
	}
	n, ok := s.nodeOf(w, r)
	if !ok {
		return
	}
	for _, statement := range statements {
		result, err := s.Nodes.Query(r.Context(), *n, token, statement)
		if err != nil {
			nodeProblem(w, err)
			return
		}
		if len(result.Rows) != 1 || len(result.Rows[0]) != 1 || result.Rows[0][0] != true {
			problem(w, http.StatusNotFound, "not_found", "this node holds no such session; it may have ended")
			return
		}
	}
	s.Log.Info("session tracing", "node", n.Label, "by", p.subject, "audit", change.Audit, "profile", change.Profile)
	w.WriteHeader(http.StatusNoContent)
}

// ClusterAnswer is the Cluster page: the profile as written (the cluster bundle's), the lineage each node reports, the
// links out. The telemetry each node reports is hugr-node's platform.telemetry, read when it exists.
type ClusterAnswer struct {
	Profile Part                `json:"profile"`
	Lineage []NodeLineage       `json:"lineage"`
	Links   []map[string]string `json:"links"`
}

// NodeLineage is one node's lineage_status.
type NodeLineage struct {
	Node  string         `json:"node"`
	Row   map[string]any `json:"row,omitempty"`
	Error string         `json:"error,omitempty"`
}

// clusterFanOut bounds the nodes asked at once for the Cluster page.
const clusterFanOut = 8

func (s *Server) cluster(w http.ResponseWriter, r *http.Request, token string, p *person) {
	nodes := answering(s.view(), s.now())
	out := ClusterAnswer{Lineage: []NodeLineage{}, Links: []map[string]string{}}
	if platform, err := s.Platform(r.Context()); err == nil && platform.Spec.Console != nil {
		for _, l := range platform.Spec.Console.Links {
			out.Links = append(out.Links, map[string]string{"kind": l.Kind, "name": l.Name, "url": l.URL})
		}
	}
	if len(nodes) == 0 {
		problem(w, http.StatusServiceUnavailable, "no_node", "no node answers")
		return
	}
	if p.rights.Infrastructure {
		out.Profile = s.part(r.Context(), *nodes[0], token, stmtItems)
	} else {
		out.Profile = Part{Rows: []map[string]any{}, Error: "the cluster right reads the profile"}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, clusterFanOut)
	for _, n := range nodes {
		wg.Add(1)
		go func(n fleet.Node) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			part := s.part(r.Context(), n, token, stmtLineage)
			row := NodeLineage{Node: n.Label, Error: part.Error}
			if part.Error == "" && len(part.Rows) == 1 {
				row.Row = part.Rows[0]
			}
			mu.Lock()
			out.Lineage = append(out.Lineage, row)
			mu.Unlock()
		}(*n)
	}
	wg.Wait()
	sort.Slice(out.Lineage, func(i, j int) bool { return out.Lineage[i].Node < out.Lineage[j].Node })
	answer(w, out)
}
