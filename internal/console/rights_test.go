package console

import (
	"testing"
)

func TestRightsOf(t *testing.T) {
	none := RightsOf(Info{})
	if none.Any() || none.Fleet || none.Scale || none.EndSession {
		t.Errorf("an answer naming nothing grants nothing: %+v", none)
	}
	observe := RightsOf(Info{Bundles: []string{"observe"}, MyViews: []string{"node_load", "sessions", "audit_events"}})
	if !observe.Fleet || !observe.Sessions || !observe.Events || observe.EndSession || observe.Drain || observe.Scale {
		t.Errorf("observe reads, acts on nothing: %+v", observe)
	}
	operate := RightsOf(Info{Bundles: []string{"observe", "operate"}, MyViews: []string{"node_load", "sessions"},
		MyFunctions: []string{"kill_session", "drain", "resume", "session_audit_level", "session_profile"}})
	if !operate.EndSession || !operate.Drain || !operate.Resume || !operate.Tracing || operate.Scale ||
		operate.Infrastructure {
		t.Errorf("operate acts on a node, never on Kubernetes: %+v", operate)
	}
	// a point grant on one operation stands in for it (duckdb-acl spec 118), the rest stays closed
	point := RightsOf(Info{MyFunctions: []string{"kill_session"}})
	if !point.EndSession || point.Drain || point.Tracing {
		t.Errorf("a point grant opens its own operation only: %+v", point)
	}
	cluster := RightsOf(Info{Bundles: []string{"observe", "operate", "cluster"}})
	if !cluster.Scale || !cluster.Infrastructure {
		t.Errorf("the cluster bundle scales and restarts: %+v", cluster)
	}
	passthrough := RightsOf(Info{Bundles: []string{"passthrough"}})
	if !passthrough.Scale {
		t.Errorf("passthrough implies the cluster bundle: %+v", passthrough)
	}
	policy := RightsOf(Info{Bundles: []string{"policy"}})
	if policy.Scale {
		t.Errorf("the policy bundle is not the infrastructure's: %+v", policy)
	}
}

func TestKillSession(t *testing.T) {
	statement, err := killSession("0a1B2c3D4e5F")
	if err != nil || statement != "KILL SESSION '0A1B2C3D4E5F'" {
		t.Fatalf("an ops id: %q, %v", statement, err)
	}
	for _, id := range []string{"", "0A1B2C3D4E5", "0A1B2C3D4E5F0", "0A1B2C3D4E5G", "x'; DROP NODE --",
		"0A1B2C3D4E5F'", "0A1B2C3D4E5F\n"} {
		if _, err := killSession(id); err == nil {
			t.Errorf("%q is no ops id, never SQL", id)
		}
	}
}

func TestSubjectOf(t *testing.T) {
	// header.{"sub":"alice@example.org"}.signature - the console reads, the node verified
	token := "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJhbGljZUBleGFtcGxlLm9yZyJ9.c2ln"
	if got := subjectOf(token); got != "alice@example.org" {
		t.Errorf("sub: %q", got)
	}
	for _, bad := range []string{"", "opaque-token", "a.b", "a.!!!.c", "a.e30.c"} {
		if got := subjectOf(bad); got != "" {
			t.Errorf("%q has no sub, got %q", bad, got)
		}
	}
}
