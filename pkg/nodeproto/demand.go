package nodeproto

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
)

// The front's demand for a resource group (spec 011 stage 2): a Lease the front keeps while it holds a client of a
// group that has no placeable node, and the operator reads to start one. Its name is the group's hash (a resource
// group is a policy's name, not a Kubernetes one); its holder the front's pod.
const (
	DemandPrefix = "hugr-demand-"
	// LabelDemand marks a demand Lease: never one of the nodes' (the front's fleet reads those by another label).
	LabelDemand = "platform.hugr.io/demand"
	// AnnotationDemandGroup names the resource group the demand is for.
	AnnotationDemandGroup = AnnotationGroup
	// AnnotationStarting is the operator's answer: the HugrNodeGroup that starts a node for the demand. The front
	// holds its client while it is there, and answers "no node" without it once the operator had time to answer.
	AnnotationStarting = "platform.hugr.io/starting"
	// DemandSeconds is a demand Lease's duration; the front renews it every DemandRenew while it holds a client.
	DemandSeconds = 30
	DemandRenew   = 10 * time.Second
)

// DemandLeaseName of a resource group.
func DemandLeaseName(group string) string {
	sum := sha256.Sum256([]byte(group))
	return DemandPrefix + hex.EncodeToString(sum[:8])
}

// DemandLive says whether a demand Lease is renewed: somebody holds a client of its group now.
func DemandLive(lease *coordinationv1.Lease, now time.Time) bool {
	if lease == nil || lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return false
	}
	return now.Before(lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second))
}

// AnnotationScaleDown marks the pod of the node the operator drains for a scale-down (spec 011): the heartbeat's
// answer then says {"drain": "scale-down"}, and the node drains itself (hugr-node spec 045).
const AnnotationScaleDown = "platform.hugr.io/scale-down"

// ScaleDownDrain is its one value; HeartbeatAnswer is the heartbeat's answer that carries it.
const ScaleDownDrain = "drain"

// HeartbeatAnswer is the orchestrator's answer to a heartbeat when it has something to say (else 204).
type HeartbeatAnswer struct {
	Drain string `json:"drain,omitempty"` // "scale-down"
}
