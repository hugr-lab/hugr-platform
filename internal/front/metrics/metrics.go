// Package metrics is what the front reports on its internal port (spec 002): calls and their outcome, re-seats,
// streams open, bytes through, and the fleet as the front sees it. No label holds a token, a cookie, a connection id
// or a group a client named: outcomes and reasons are fixed sets, groups are the fleet's.
package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	// Calls by protocol and outcome: routed, seated, re-seated, no_node, upstream_error.
	Calls = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hugr_front_calls",
		Help: "Calls by protocol and outcome."}, []string{"protocol", "outcome"})
	// Reseats by the node's reason: wrong_resource_group, draining (or at capacity), unreachable.
	Reseats = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hugr_front_reseats",
		Help: "Connections seated again, by the node's reason."}, []string{"reason"})
	// StreamsOpen by protocol.
	StreamsOpen = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "hugr_front_streams_open",
		Help: "Calls in flight."}, []string{"protocol"})
	// Bytes by protocol and direction (in: from clients, out: to clients).
	Bytes = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hugr_front_bytes",
		Help: "Bytes through the front."}, []string{"protocol", "direction"})
	// Nodes by group and state: placeable, serving (keeps its connections, takes no new one), unknown (no Lease).
	Nodes = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "hugr_front_nodes",
		Help: "The fleet as the front sees it."}, []string{"group", "state"})
)

// Registry of the front's metrics.
func Registry() *prometheus.Registry {
	r := prometheus.NewRegistry()
	r.MustRegister(Calls, Reseats, StreamsOpen, Bytes, Nodes)
	return r
}
