package controller

import (
	"sort"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

// NodeState is one node as its Lease says: lost when the Lease expired (nothing keeps a timer), and the summary of
// its last heartbeat.
type NodeState struct {
	Name    string
	Lost    bool
	Summary nodeproto.Summary
}

// NodeStateOf a pod's Lease at a time.
func NodeStateOf(name string, lease *coordinationv1.Lease, now time.Time) NodeState {
	state := NodeState{Name: name, Summary: nodeproto.ParseSummary(lease.Annotations)}
	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		state.Lost = true
		return state
	}
	expires := lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
	state.Lost = now.After(expires)
	return state
}

// Fleet is a group's state from its nodes.
type Fleet struct {
	Profile    v1alpha1.ProfileStatus
	Lost       []string
	GroupKnown *bool
	Builds     []v1alpha1.BuildCount
	Catalog    *v1alpha1.CatalogStamp
}

// Aggregate the live nodes: the target is the highest any reports, applied is how many have it, who needs a restart;
// the lost ones listed; the resource group known when every node that says knows it, unknown when one does not.
func Aggregate(nodes []NodeState) Fleet {
	var fleet Fleet
	var live []NodeState
	for _, n := range nodes {
		if n.Lost {
			fleet.Lost = append(fleet.Lost, n.Name)
			continue
		}
		live = append(live, n)
		if n.Summary.Target > fleet.Profile.Target {
			fleet.Profile.Target = n.Summary.Target
		}
	}
	for _, n := range live {
		if fleet.Profile.Target > 0 && n.Summary.Applied == fleet.Profile.Target {
			fleet.Profile.Applied++
		}
		if n.Summary.NeedsRestart {
			fleet.Profile.NeedsRestart = append(fleet.Profile.NeedsRestart, n.Name)
		}
		if n.Summary.GroupKnown != nil {
			known := *n.Summary.GroupKnown
			if fleet.GroupKnown == nil || !known {
				fleet.GroupKnown = &known
			}
		}
		if n.Summary.CatalogSchema > 0 && (fleet.Catalog == nil || n.Summary.CatalogSchema > fleet.Catalog.Schema) {
			fleet.Catalog = &v1alpha1.CatalogStamp{Schema: n.Summary.CatalogSchema, MinReader: n.Summary.CatalogMinReader}
		}
	}
	fleet.Builds = BuildsOf(live)
	sort.Strings(fleet.Lost)
	sort.Strings(fleet.Profile.NeedsRestart)
	return fleet
}

func setCondition(status *v1alpha1.HugrNodeGroupStatus, generation int64, kind string, value metav1.ConditionStatus,
	reason, message string) {
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: kind, Status: value, Reason: reason,
		Message: message, ObservedGeneration: generation})
}

// equalStatus ignores the conditions' transition times, which SetStatusCondition keeps when nothing changed.
func equalStatus(a, b *v1alpha1.HugrNodeGroupStatus) bool {
	return equality.Semantic.DeepEqual(a, b)
}

// BuildsOf the live nodes that report one (spec 008): a count per build and DuckDB release, in their order.
func BuildsOf(live []NodeState) []v1alpha1.BuildCount {
	counts := map[v1alpha1.BuildCount]int32{}
	for _, n := range live {
		if n.Summary.Build == "" {
			continue
		}
		counts[v1alpha1.BuildCount{Build: n.Summary.Build, DuckDB: n.Summary.DuckDBVersion}]++
	}
	var out []v1alpha1.BuildCount
	for key, nodes := range counts {
		key.Nodes = nodes
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Build != out[j].Build {
			return out[i].Build < out[j].Build
		}
		return out[i].DuckDB < out[j].DuckDB
	})
	return out
}
