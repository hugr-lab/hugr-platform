package plan

import (
	"strings"
	"testing"
	"time"
)

var thePlan = Plan{MaxNodes: 6, Sizes: []string{"large"}, Known: map[string]bool{"large": true, "small": true}}

func TestCheckName(t *testing.T) {
	for _, ok := range []string{"etl", "a", "bi-2", strings.Repeat("g", MaxGroupName)} {
		if err := CheckName(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "ETL", "etl.prod", "-etl", "etl-", strings.Repeat("g", MaxGroupName+1)} {
		if err := CheckName(bad); err == nil {
			t.Errorf("%q accepted: its pods' names would be no labels", bad)
		}
	}
}

func TestCheckChange(t *testing.T) {
	others := []Group{{Name: "bi", Size: "large", Replicas: 3}}
	if err := thePlan.CheckChange(Group{Name: "etl", Size: "large", Replicas: 3}, others); err != nil {
		t.Errorf("a fitting change is refused: %v", err)
	}
	if err := thePlan.CheckChange(Group{Name: "etl", Size: "large", Replicas: 4}, others); err == nil ||
		!strings.Contains(err.Error(), "at most 6") {
		t.Errorf("a total past maxNodes over every group: %v", err)
	}
	if err := thePlan.CheckChange(Group{Name: "etl", Size: "small", Replicas: 1}, others); err == nil ||
		!strings.Contains(err.Error(), "not in the plan") {
		t.Errorf("a size the platform lists and the plan does not: %v", err)
	}
	if err := thePlan.CheckChange(Group{Name: "etl", Size: "huge", Replicas: 1}, others); err == nil ||
		!strings.Contains(err.Error(), "not one the platform lists") {
		t.Errorf("a size nobody lists: %v", err)
	}
	// a group's own old state is not counted twice
	if err := thePlan.CheckChange(Group{Name: "bi", Size: "large", Replicas: 6}, others); err != nil {
		t.Errorf("a group growing into the whole plan alone: %v", err)
	}
}

func TestAdmit(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	// two changes that each fit alone (6 nodes) and not together: the later group is refused and keeps what it runs
	groups := []Group{
		{Name: "late", Size: "large", Replicas: 4, Running: 1, Created: t0.Add(time.Minute)},
		{Name: "early", Size: "large", Replicas: 4, Running: 2, Created: t0},
	}
	refused := thePlan.Admit(groups)
	if len(refused) != 1 || refused["late"] == "" || !strings.Contains(refused["late"], "keeps the 1 it runs") {
		t.Fatalf("refused = %v, want only the later group, keeping what runs", refused)
	}
	// what a refused group runs counts for the groups after it
	groups = append(groups, Group{Name: "last", Size: "large", Replicas: 2, Created: t0.Add(2 * time.Minute)})
	if refused := thePlan.Admit(groups); refused["last"] == "" {
		t.Errorf("a group past the plan once a refused group's running nodes count: %v", refused)
	}
	// a group whose size the plan drops is refused, keeps its nodes, and the others still fit
	groups = []Group{
		{Name: "a", Size: "small", Replicas: 2, Running: 2, Created: t0},
		{Name: "b", Size: "large", Replicas: 4, Created: t0.Add(time.Minute)},
	}
	if refused := thePlan.Admit(groups); refused["a"] == "" || refused["b"] != "" {
		t.Errorf("refused = %v, want a only", refused)
	}
}

// TestAdmitNeverPassesThePlan: an older group whose size comes back into the plan does not grow into the nodes a
// newer group already runs - the reconcile never takes the namespace past MaxNodes by itself
func TestAdmitNeverPassesThePlan(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	p := Plan{MaxNodes: 10, Sizes: []string{"large"}, Known: map[string]bool{"large": true}}
	groups := []Group{
		{Name: "a", Size: "large", Replicas: 8, Running: 2, Created: t0},
		{Name: "b", Size: "large", Replicas: 8, Running: 8, Created: t0.Add(time.Minute)},
	}
	refused := p.Admit(groups)
	if refused["a"] == "" || refused["b"] != "" {
		t.Fatalf("refused = %v: the older group would grow to 8 beside the newer one's 8", refused)
	}
	var total int32
	for _, g := range groups {
		if refused[g.Name] != "" {
			total += g.Running
		} else {
			total += g.Replicas
		}
	}
	if total > p.MaxNodes {
		t.Errorf("the admitted state runs %d nodes past %d", total, p.MaxNodes)
	}
	// a scale-down frees its nodes once its Deployment runs fewer, never before: the growth after it waits
	groups = []Group{
		{Name: "a", Size: "large", Replicas: 7, Running: 2, Created: t0},
		{Name: "b", Size: "large", Replicas: 3, Running: 8, Created: t0.Add(time.Minute)},
	}
	if refused := p.Admit(groups); refused["a"] == "" || refused["b"] != "" {
		t.Errorf("refused = %v: a grows only once b runs 3", refused)
	}
	groups[1].Running = 3
	if refused := p.Admit(groups); len(refused) != 0 {
		t.Errorf("refused = %v: b runs 3, a has room for 7", refused)
	}
}
