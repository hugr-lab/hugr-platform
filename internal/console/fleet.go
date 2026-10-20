package console

import (
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/build"
	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
)

// FleetAnswer is `GET groups`: the plan and the groups with their nodes, from Kubernetes (spec 003, C1).
type FleetAnswer struct {
	Plan   PlanAnswer    `json:"plan"`
	Groups []GroupAnswer `json:"groups"`
}

// PlanAnswer is the installation's plan.
type PlanAnswer struct {
	MaxNodes int32    `json:"maxNodes"`
	Sizes    []string `json:"sizes"`
}

// GroupAnswer is one group: its spec's count and size, its status, its nodes.
type GroupAnswer struct {
	Name               string            `json:"name"`
	Size               string            `json:"size"`
	Replicas           int32             `json:"replicas"`
	ReadyReplicas      int32             `json:"readyReplicas"`
	ResourceGroup      string            `json:"resourceGroup"`
	ResourceGroupKnown *bool             `json:"resourceGroupKnown"`
	Profile            ProfileAnswer     `json:"profile"`
	Lost               []string          `json:"lost"`
	RestartedAt        string            `json:"restartedAt,omitempty"`
	Conditions         []ConditionAnswer `json:"conditions"`
	// Scaling is the shape the autoscaler will fill (spec 003 round 2, Q8); every group is manual until it exists.
	Scaling ScalingAnswer `json:"scaling"`
	// Identity is who the group's nodes are (round 2): every node of a group is the same workload.
	Identity IdentityAnswer `json:"identity"`
	// MemoryLimitBytes is a node's pod limit, its size's (0 when the size names none).
	MemoryLimitBytes int64 `json:"memoryLimitBytes"`
	// Problem is what the group's Deployment says stops its pods (none: empty).
	Problem string       `json:"problem,omitempty"`
	Nodes   []NodeAnswer `json:"nodes"`
}

// IdentityAnswer is a group's workload identity and the name its doors answer as - never a token.
type IdentityAnswer struct {
	ServiceAccount string `json:"serviceAccount"`
	Subject        string `json:"subject"` // what the IdP sees of the projected token
	ClientID       string `json:"clientID"`
	Tresor         string `json:"tresor"`
	TLSName        string `json:"tlsName"`
}

// ProfileAnswer is a group's convergence on the cluster profile.
type ProfileAnswer struct {
	Target       int64    `json:"target"`
	Applied      int32    `json:"applied"`
	NeedsRestart []string `json:"needsRestart"`
}

// ConditionAnswer is one of the group's conditions: Ready, Refused, Draining.
type ConditionAnswer struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// ScalingAnswer is how the group scales.
type ScalingAnswer struct {
	Mode string `json:"mode"` // manual | auto
	Min  *int32 `json:"min,omitempty"`
	Max  *int32 `json:"max,omitempty"`
}

// NodeAnswer is one node as Kubernetes and its Lease show it.
type NodeAnswer struct {
	Label   string `json:"label"`
	Ready   bool   `json:"ready"`
	Serving bool   `json:"serving"`
	// LeaseAge is the seconds since the node's last heartbeat; nil before its first.
	LeaseAge     *int64 `json:"leaseAge"`
	Current      bool   `json:"current"` // the Lease has not expired
	Sessions     int64  `json:"sessions"`
	SeatsLeft    int64  `json:"seatsLeft"` // -1: no quack door
	Admitting    bool   `json:"admitting"`
	Draining     bool   `json:"draining"`
	Target       int64  `json:"target"`
	Applied      int64  `json:"applied"`
	NeedsRestart bool   `json:"needsRestart"`
	// Usage is the pod's CPU and memory by the metrics API; nil without it.
	Usage *Usage `json:"usage"`
	// Pod is how far the node's pod has got; nil when Kubernetes' pods were not read.
	Pod *PodState `json:"pod"`
}

// groupOf names the node group a node belongs to: its Service is hugr-node-<group> (spec 001).
func groupOf(n *fleet.Node) string {
	prefix := build.NodeName + "-"
	if len(n.Service) > len(prefix) && n.Service[:len(prefix)] == prefix {
		return n.Service[len(prefix):]
	}
	return ""
}

// KubeState is what the fleet's page reads of Kubernetes beside the groups; a part it could not read is empty.
type KubeState struct {
	Usage       map[string]Usage
	Pods        []corev1.Pod
	Deployments []appsv1.Deployment
	Warnings    []corev1.Event
}

// FleetOf the platform's groups and the fleet's view, at `now`. A group's nodes are the fleet's (an endpoint, a
// Lease) and its pods that have none yet - the node a scale made, starting or failing to - each with its pod's state.
func FleetOf(platform *v1alpha1.HugrPlatform, groups []v1alpha1.HugrNodeGroup, view *fleet.View,
	now time.Time, kube KubeState) FleetAnswer {
	answer := FleetAnswer{Plan: PlanAnswer{Sizes: []string{}}, Groups: []GroupAnswer{}}
	if platform != nil {
		answer.Plan = PlanAnswer{MaxNodes: platform.Spec.Plan.MaxNodes, Sizes: append([]string{}, platform.Spec.Plan.Sizes...)}
	}
	warnings := newestWarnings(kube.Warnings)
	pods := map[string]*corev1.Pod{}
	for i := range kube.Pods {
		pods[kube.Pods[i].Name] = &kube.Pods[i]
	}
	byGroup := map[string][]NodeAnswer{}
	seen := map[string]bool{}
	if view != nil {
		for _, n := range view.Nodes() {
			a := nodeAnswer(n, now)
			if u, ok := kube.Usage[n.Label]; ok {
				a.Usage = &u
			}
			if p := pods[n.Label]; p != nil {
				state := podState(p, now, warnings[p.Name])
				a.Pod = &state
			}
			seen[n.Label] = true
			byGroup[groupOf(n)] = append(byGroup[groupOf(n)], a)
		}
	}
	for _, p := range kube.Pods {
		if seen[p.Name] {
			continue
		}
		state := podState(&p, now, warnings[p.Name])
		if state.State == "ended" {
			continue // an eviction's leftover: its ReplicaSet made another; its events tell the history
		}
		byGroup[p.Labels[build.LabelGroup]] = append(byGroup[p.Labels[build.LabelGroup]],
			NodeAnswer{Label: p.Name, SeatsLeft: -1, Pod: &state})
	}
	problems := map[string]string{}
	for i := range kube.Deployments {
		d := &kube.Deployments[i]
		problems[d.Labels[build.LabelGroup]] = deploymentProblem(d)
	}
	for _, g := range groups {
		if platform != nil && g.Spec.Platform != platform.Name {
			continue
		}
		nodes := byGroup[g.Name]
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].Label < nodes[j].Label })
		if nodes == nil {
			nodes = []NodeAnswer{}
		}
		conditions := []ConditionAnswer{}
		for _, c := range g.Status.Conditions {
			conditions = append(conditions, ConditionAnswer{Type: c.Type, Status: string(c.Status), Reason: c.Reason,
				Message: c.Message})
		}
		var limit int64
		if platform != nil {
			if size, ok := platform.Spec.Sizes[g.Spec.Size]; ok {
				limit = size.Resources.Limits.Memory().Value()
			}
		}
		answer.Groups = append(answer.Groups, GroupAnswer{
			MemoryLimitBytes:   limit,
			Name:               g.Name,
			Size:               g.Spec.Size,
			Replicas:           g.Spec.Replicas,
			ReadyReplicas:      g.Status.ReadyReplicas,
			ResourceGroup:      g.EffectiveResourceGroup(),
			ResourceGroupKnown: g.Status.ResourceGroupKnown,
			Profile: ProfileAnswer{Target: g.Status.Profile.Target, Applied: g.Status.Profile.Applied,
				NeedsRestart: append([]string{}, g.Status.Profile.NeedsRestart...)},
			Lost:        append([]string{}, g.Status.Lost...),
			RestartedAt: g.Spec.RestartedAt,
			Conditions:  conditions,
			Scaling:     ScalingAnswer{Mode: "manual"},
			Identity:    identityOf(platform, &g),
			Problem:     problems[g.Name],
			Nodes:       nodes,
		})
	}
	sort.Slice(answer.Groups, func(i, j int) bool { return answer.Groups[i].Name < answer.Groups[j].Name })
	return answer
}

func identityOf(platform *v1alpha1.HugrPlatform, g *v1alpha1.HugrNodeGroup) IdentityAnswer {
	name := build.Name(g)
	out := IdentityAnswer{ServiceAccount: name, Subject: "system:serviceaccount:" + g.Namespace + ":" + name,
		ClientID: g.Spec.Identity.ClientID, TLSName: name + "." + g.Namespace + ".svc"}
	if platform != nil {
		out.Tresor = platform.Spec.Tresor.Host
	}
	return out
}

func nodeAnswer(n *fleet.Node, now time.Time) NodeAnswer {
	answer := NodeAnswer{Label: n.Label, Ready: n.Ready, Serving: n.Serving, SeatsLeft: -1}
	if n.Lease != nil {
		s := n.Lease.Summary
		answer.Current = n.Current(now)
		answer.Sessions = s.Sessions
		answer.SeatsLeft = s.SeatsLeft
		answer.Admitting = s.AdmitSession
		answer.Draining = s.Draining
		answer.Target = s.Target
		answer.Applied = s.Applied
		answer.NeedsRestart = s.NeedsRestart
	}
	if n.Lease != nil && !n.Lease.Renewed.IsZero() {
		age := int64(now.Sub(n.Lease.Renewed) / time.Second)
		answer.LeaseAge = &age
	}
	return answer
}
