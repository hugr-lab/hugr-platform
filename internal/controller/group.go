// Package controller reconciles node groups (spec 001): the plan checked again over every group, the group's objects
// kept, and its status aggregated from its Deployment and its nodes' Leases - with each pod's deletion cost from the
// sessions it reported.
package controller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/build"
	"github.com/hugr-lab/hugr-platform/internal/upgrade"
	"github.com/hugr-lab/hugr-platform/pkg/autoscale"
	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
	"github.com/hugr-lab/hugr-platform/pkg/plan"
)

// FieldOwner is the operator's name in server-side apply.
const FieldOwner = "hugr-platform"

// StatusEvery is how often a group's status is aggregated from its Leases (spec 001: at most every 5 s).
const StatusEvery = 5 * time.Second

// AnnotationDeletionCost is Kubernetes' own: a scale-down removes the pod with the lowest cost first.
const AnnotationDeletionCost = "controller.kubernetes.io/pod-deletion-cost"

// Condition types of a group.
const (
	ConditionReady    = "Ready"
	ConditionRefused  = "Refused"
	ConditionDraining = "Draining"
)

// GroupReconciler keeps a group's objects and its status.
type GroupReconciler struct {
	client.Client
	// Direct reads from the API server what the plan is judged on: the cache may not have seen another group's
	// Deployment yet, and two growths would then each count the other's old size
	Direct   client.Reader
	Scheme   *runtime.Scheme
	Settings build.Settings
	Now      func() time.Time
}

// +kubebuilder:rbac:groups=platform.hugr.io,resources=hugrplatforms,verbs=get;list;watch
// +kubebuilder:rbac:groups=platform.hugr.io,resources=hugrnodegroups,verbs=get;list;watch
// +kubebuilder:rbac:groups=platform.hugr.io,resources=hugrnodegroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.hugr.io,resources=hugrnodegroups/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=serviceaccounts;services,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile one group.
func (r *GroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	var group v1alpha1.HugrNodeGroup
	if err := r.Get(ctx, req.NamespacedName, &group); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if group.DeletionTimestamp != nil {
		return ctrl.Result{}, nil // going: the garbage collector takes its objects, nothing is applied again
	}
	status := group.Status.DeepCopy()
	status.ObservedGeneration = group.Generation

	var platform v1alpha1.HugrPlatform
	if err := r.Get(ctx, types.NamespacedName{Namespace: group.Namespace, Name: group.Spec.Platform}, &platform); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		setCondition(status, group.Generation, ConditionReady, metav1.ConditionFalse, "PlatformMissing",
			fmt.Sprintf("the platform %q is not in the namespace", group.Spec.Platform))
		return ctrl.Result{RequeueAfter: StatusEvery}, r.writeStatus(ctx, &group, status)
	}

	// the plan, again: two changes may each pass the webhook alone and not fit together
	refusal, running, err := r.checkPlan(ctx, &platform, &group)
	if err != nil {
		return ctrl.Result{}, err
	}

	if refusal == "" {
		// a platform edit is judged too: a size the node cannot size its engine from is refused, never rolled
		if err := build.CheckSize(&platform, group.Spec.Size); err != nil {
			refusal = err.Error() + " - the group keeps what it runs"
		}
	}
	// one object that cannot be applied stops none of the rest, and the status still says what happens
	var applyErr error
	for _, obj := range []client.Object{build.ServiceAccount(&group), build.Service(&group),
		build.PodDisruptionBudget(&group), build.NetworkPolicy(&group)} {
		if err := r.apply(ctx, &group, obj); err != nil && applyErr == nil {
			applyErr = err
		}
	}
	if refusal == "" {
		deployment, err := build.Deployment(r.withImage(ctx, &platform, &group), &group, r.Settings)
		if err == nil {
			err = r.apply(ctx, &group, deployment)
		}
		if err != nil && applyErr == nil {
			applyErr = err
		}
		setCondition(status, group.Generation, ConditionRefused, metav1.ConditionFalse, "WithinPlan", "the group fits the plan")
	} else {
		// what runs is kept: the Deployment is left as it is, never scaled away - but a scale-down is applied, the
		// one change that brings a group back within the plan
		logger.Info("group refused by the plan", "group", group.Name, "reason", refusal)
		setCondition(status, group.Generation, ConditionRefused, metav1.ConditionTrue, "PastPlan", refusal)
		if err := r.scaleDown(ctx, &group); err != nil && applyErr == nil {
			applyErr = err
		}
	}

	if err := r.aggregate(ctx, &platform, &group, running, status); err != nil {
		return ctrl.Result{}, err
	}
	if applyErr != nil {
		setCondition(status, group.Generation, ConditionReady, metav1.ConditionFalse, "ApplyFailed", applyErr.Error())
	}
	if err := r.writeStatus(ctx, &group, status); err != nil {
		return ctrl.Result{}, err
	}
	if applyErr != nil {
		return ctrl.Result{}, applyErr // retried with backoff
	}
	return ctrl.Result{RequeueAfter: StatusEvery}, nil
}

// withImage is the platform as this group's Deployment is built from it: the node image the rollout gives the group
// (spec 008) - the target once the rollout has reached it, what it runs until then.
func (r *GroupReconciler) withImage(ctx context.Context, platform *v1alpha1.HugrPlatform,
	group *v1alpha1.HugrNodeGroup) *v1alpha1.HugrPlatform {
	current := ""
	var deployment appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: group.Namespace, Name: build.Name(group)}, &deployment); err == nil {
		current = build.NodeImage(&deployment.Spec.Template.Spec)
	}
	image := upgrade.ImageFor(platform, group.Name, current)
	if image == platform.Spec.Node.Image {
		return platform
	}
	with := platform.DeepCopy()
	with.Spec.Node.Image = image
	return with
}

// checkPlan answers the group's refusal ("" when it fits) and what its Deployment runs, as the plan read it.
func (r *GroupReconciler) checkPlan(ctx context.Context, platform *v1alpha1.HugrPlatform,
	group *v1alpha1.HugrNodeGroup) (string, int32, error) {
	var groups v1alpha1.HugrNodeGroupList
	if err := r.List(ctx, &groups, client.InNamespace(group.Namespace)); err != nil {
		return "", 0, err
	}
	var deployments appsv1.DeploymentList
	if err := r.reader().List(ctx, &deployments, client.InNamespace(group.Namespace), client.HasLabels{build.LabelGroup}); err != nil {
		return "", 0, err
	}
	running := map[string]int32{}
	for _, d := range deployments.Items {
		if d.Spec.Replicas != nil {
			running[d.Labels[build.LabelGroup]] = *d.Spec.Replicas
		}
	}
	// the plan is the namespace's: every group in it, one platform per namespace
	var demands []plan.Group
	named := map[string]bool{}
	for _, g := range groups.Items {
		named[g.Name] = true
	}
	// a Deployment whose group is gone (orphaned, or not yet collected) still runs its nodes
	for name, replicas := range running {
		if !named[name] {
			demands = append(demands, plan.Group{Name: name, Size: "", Replicas: replicas, Running: replicas})
		}
	}
	for _, g := range groups.Items {
		if g.DeletionTimestamp != nil && g.Name != group.Name {
			// a group going still runs what it runs, and asks nothing more
			demands = append(demands, plan.Group{Name: g.Name, Size: g.Spec.Size, Replicas: running[g.Name],
				Running: running[g.Name], Created: g.CreationTimestamp.Time})
			continue
		}
		demands = append(demands, plan.Group{Name: g.Name, Size: g.Spec.Size, Replicas: g.Spec.Replicas,
			Running: running[g.Name], Created: g.CreationTimestamp.Time})
	}
	return PlanOf(platform).Admit(demands)[group.Name], running[group.Name], nil
}

func (r *GroupReconciler) reader() client.Reader {
	if r.Direct != nil {
		return r.Direct
	}
	return r.Client
}

// scaleDown a refused group's Deployment to what the group asks, when it asks fewer: its replicas only, nothing else
// of what runs changes.
func (r *GroupReconciler) scaleDown(ctx context.Context, group *v1alpha1.HugrNodeGroup) error {
	var deployment appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: group.Namespace, Name: build.Name(group)}, &deployment); err != nil {
		return client.IgnoreNotFound(err)
	}
	if deployment.Spec.Replicas == nil || group.Spec.Replicas >= *deployment.Spec.Replicas {
		return nil
	}
	patch := client.MergeFrom(deployment.DeepCopy())
	replicas := group.Spec.Replicas
	deployment.Spec.Replicas = &replicas
	return r.Patch(ctx, &deployment, patch)
}

// PlanOf a platform.
func PlanOf(platform *v1alpha1.HugrPlatform) plan.Plan {
	known := map[string]bool{}
	for name := range platform.Spec.Sizes {
		known[name] = true
	}
	return plan.Plan{MaxNodes: platform.Spec.Plan.MaxNodes, Sizes: platform.Spec.Plan.Sizes, Known: known}
}

// apply one object of the group, owned by it, by server-side apply: an object unchanged is not written.
func (r *GroupReconciler) apply(ctx context.Context, group *v1alpha1.HugrNodeGroup, obj client.Object) error {
	if err := controllerutil.SetControllerReference(group, obj, r.Scheme); err != nil {
		return err
	}
	gvk, err := r.Client.GroupVersionKindFor(obj)
	if err != nil {
		return err
	}
	obj.GetObjectKind().SetGroupVersionKind(gvk)
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return err
	}
	delete(content, "status")
	if metadata, ok := content["metadata"].(map[string]interface{}); ok {
		delete(metadata, "creationTimestamp")
	}
	applied := client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: content})
	return r.Client.Apply(ctx, applied, client.FieldOwner(FieldOwner), client.ForceOwnership)
}

// aggregate the group's status from its Deployment and its nodes' Leases, and write each pod's deletion cost.
// running is what the group's Deployment is asked to run, as the plan read it (directly - the cache may lag).
func (r *GroupReconciler) aggregate(ctx context.Context, platform *v1alpha1.HugrPlatform, group *v1alpha1.HugrNodeGroup,
	running int32, status *v1alpha1.HugrNodeGroupStatus) error {
	var deployment appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Namespace: group.Namespace, Name: build.Name(group)}, &deployment)
	switch {
	case apierrors.IsNotFound(err):
		status.Replicas, status.ReadyReplicas = 0, 0
	case err != nil:
		return err
	default:
		status.Replicas, status.ReadyReplicas = deployment.Status.Replicas, deployment.Status.ReadyReplicas
	}

	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(group.Namespace), client.MatchingLabels(build.SelectorLabels(group))); err != nil {
		return err
	}
	var leases coordinationv1.LeaseList
	if err := r.List(ctx, &leases, client.InNamespace(group.Namespace), client.MatchingLabels{build.LabelGroup: group.Name}); err != nil {
		return err
	}
	byPod := map[string]*coordinationv1.Lease{}
	for i := range leases.Items {
		byPod[leases.Items[i].Name] = &leases.Items[i]
	}
	var nodes []NodeState
	var scaling *autoscale.Scaling
	if group.Auto() {
		s := ScalingOf(group.Spec.Scaling)
		scaling = &s
	}
	alive, unreported := 0, 0
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			continue // going, or ended (an evicted pod stays Failed until pod GC): no node, as for the ReplicaSet
		}
		alive++
		lease, ok := byPod[pod.Name]
		if !ok {
			unreported++
			continue // not reported yet: neither lost nor counted
		}
		state := NodeStateOf(pod.Name, lease, r.now())
		nodes = append(nodes, state)
		var idle time.Duration
		if scaling != nil {
			idle = scaling.Idle
		}
		if err := r.writeDeletionCost(ctx, pod, DeletionCost(state, idle, r.now())); err != nil {
			return err
		}
	}
	switch {
	case scaling != nil:
		defaultGroup := ""
		if platform.Spec.FirstPolicy != nil {
			defaultGroup = platform.Spec.FirstPolicy.DefaultGroup
		}
		in := autoscale.Input{Replicas: group.Spec.Replicas, Running: running, Pods: alive, Unreported: unreported,
			Default: defaultGroup != "" && group.EffectiveResourceGroup() == defaultGroup,
			MayGrow: !meta.IsStatusConditionTrue(status.Conditions, ConditionRefused), Now: r.now()}
		for _, n := range nodes {
			in.Nodes = append(in.Nodes, nodeOf(n))
			in.Default = in.Default || n.Summary.GroupDefault
		}
		// the policy's default group is kept as such: one heartbeat during a catalog hiccup says not-default, and
		// the default group at 0 would leave no node to name any group to wake (the review)
		if status.Scaling == nil {
			status.Scaling = &v1alpha1.ScalingStatus{}
		}
		rg := group.EffectiveResourceGroup()
		switch {
		case in.Default:
			status.Scaling.DefaultGroup = rg
		case status.Scaling.DefaultGroup == rg:
			in.Default = true
		}
		// stage 2: the front holds a client of the group's resource group with no node to seat it on
		var wanted coordinationv1.Lease
		demandKey := types.NamespacedName{Namespace: group.Namespace,
			Name: nodeproto.DemandLeaseName(group.EffectiveResourceGroup())}
		hasDemand := r.Get(ctx, demandKey, &wanted) == nil
		in.Demand = hasDemand && nodeproto.DemandLive(&wanted, in.Now)
		r.autoscale(ctx, group, *scaling, in, status)
		if hasDemand {
			// "starting" while a node is on its way for it - asked for and not here yet, or not reported - never for
			// a group whose nodes run but are not placeable for the client (at capacity, past the critical level):
			// that client should hear "no node" soon, not be held (the review)
			onItsWay := group.Spec.Replicas > 0 && (alive < int(group.Spec.Replicas) || unreported > 0)
			r.answerDemand(ctx, group, &wanted, in.Demand && in.MayGrow && onItsWay)
		}
	case status.Scaling != nil:
		status.Scaling.PressureSince = nil // a manual group keeps its last change, never a pressure clock
	}
	fleet := Aggregate(nodes)
	status.Profile = fleet.Profile
	status.Lost = fleet.Lost
	status.ResourceGroupKnown = fleet.GroupKnown
	status.Builds = fleet.Builds
	status.Catalog = fleet.Catalog

	ready := status.ReadyReplicas == group.Spec.Replicas && status.Replicas == group.Spec.Replicas
	if meta.IsStatusConditionTrue(status.Conditions, ConditionRefused) {
		setCondition(status, group.Generation, ConditionReady, metav1.ConditionFalse, "PastPlan", "the group is past the plan")
	} else if ready {
		setCondition(status, group.Generation, ConditionReady, metav1.ConditionTrue, "Ready",
			fmt.Sprintf("%d of %d nodes ready", status.ReadyReplicas, group.Spec.Replicas))
	} else {
		setCondition(status, group.Generation, ConditionReady, metav1.ConditionFalse, "Progressing",
			fmt.Sprintf("%d of %d nodes ready", status.ReadyReplicas, group.Spec.Replicas))
	}
	switch {
	case fleet.GroupKnown == nil:
		setCondition(status, group.Generation, ConditionDraining, metav1.ConditionUnknown, "NotReported",
			"no node has reported yet")
	case !*fleet.GroupKnown:
		setCondition(status, group.Generation, ConditionDraining, metav1.ConditionTrue, "ResourceGroupMissing",
			fmt.Sprintf("the policy has no resource group %q: the nodes drain themselves", group.EffectiveResourceGroup()))
	default:
		setCondition(status, group.Generation, ConditionDraining, metav1.ConditionFalse, "ResourceGroupKnown", "")
	}
	return nil
}

func (r *GroupReconciler) writeDeletionCost(ctx context.Context, pod *corev1.Pod, value int64) error {
	cost := strconv.FormatInt(value, 10)
	if pod.Annotations[AnnotationDeletionCost] == cost {
		return nil
	}
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[AnnotationDeletionCost] = cost
	return client.IgnoreNotFound(r.Patch(ctx, pod, patch))
}

// DeletionCost of a node: its sessions in powers of two (0, 1, 2, 4, 8, ...), so a pod is patched when its load
// changes in kind, not at every session; a lost node costs -3 and, in an auto group (idle > 0), one with no session
// for its idle time -2 (spec 011), so a scale-down takes them before a live one.
func DeletionCost(state NodeState, idle time.Duration, now time.Time) int64 {
	if state.Lost {
		return -3
	}
	if idle > 0 && autoscale.IdlePast(nodeOf(state), idle, now) {
		return -2
	}
	sessions := state.Summary.Sessions
	if sessions <= 0 {
		return 0
	}
	bucket := int64(1)
	for bucket*2 <= sessions && bucket < 1<<30 {
		bucket *= 2
	}
	return bucket
}

// answerDemand says on the front's demand Lease whether a node of the group starts for it (spec 011 stage 2): the
// front holds its client while it does, and answers "no node" once it had time to hear and nobody does.
func (r *GroupReconciler) answerDemand(ctx context.Context, group *v1alpha1.HugrNodeGroup, lease *coordinationv1.Lease,
	starting bool) {
	current := lease.Annotations[nodeproto.AnnotationStarting]
	switch {
	case starting && current == group.Name, !starting && current != group.Name:
		return // as it should be - or another group's answer (two groups serving one resource group), not ours
	case starting && current != "":
		return // another group of the resource group answered first: one answer is enough
	}
	patch := client.MergeFrom(lease.DeepCopy())
	if lease.Annotations == nil {
		lease.Annotations = map[string]string{}
	}
	if starting {
		lease.Annotations[nodeproto.AnnotationStarting] = group.Name
	} else {
		delete(lease.Annotations, nodeproto.AnnotationStarting)
	}
	if err := r.Patch(ctx, lease, patch); err != nil {
		log.FromContext(ctx).Info("demand not answered, next look", "group", group.Name, "error", err.Error())
	}
}

// AnnotationScaledAt is when the autoscaler last changed the group's replicas, written with them (spec 011).
const AnnotationScaledAt = "platform.hugr.io/scaled-at"

// WakeOnDemand: the front's demand wakes a group at 0 (spec 011 stage 2), so an auto group may go to its min of 0.
const WakeOnDemand = true

// ScalingOf a group's scaling, its defaults filled in (spec 011).
func ScalingOf(spec *v1alpha1.ScalingSpec) autoscale.Scaling {
	s := autoscale.Scaling{Min: spec.Min, Max: spec.Max, Idle: autoscale.DefaultIdle, For: autoscale.DefaultFor,
		Cooldown: autoscale.DefaultCooldown, MemoryUsed: autoscale.DefaultMemoryUsed, WakeOnDemand: WakeOnDemand,
		TempUsed: autoscale.DefaultTempUsed, SessionsUsed: autoscale.DefaultSessionsUsed,
		SeatsUsed: autoscale.DefaultSeatsUsed, StreamsWaiting: autoscale.DefaultStreamsWaiting}
	duration := func(d *metav1.Duration, into *time.Duration) {
		if d != nil && d.Duration > 0 {
			*into = d.Duration
		}
	}
	duration(spec.Idle, &s.Idle)
	duration(spec.Cooldown, &s.Cooldown)
	duration(spec.ScaleUp.For, &s.For)
	percent := func(p int32, into *float64) {
		if p > 0 {
			*into = float64(p) / 100
		}
	}
	percent(spec.ScaleUp.MemoryUsed, &s.MemoryUsed)
	percent(spec.ScaleUp.TempUsed, &s.TempUsed)
	percent(spec.ScaleUp.SessionsUsed, &s.SessionsUsed)
	percent(spec.ScaleUp.SeatsUsed, &s.SeatsUsed)
	if spec.ScaleUp.StreamsWaiting > 0 {
		s.StreamsWaiting = int64(spec.ScaleUp.StreamsWaiting)
	}
	return s
}

// nodeOf a node's state as the autoscaler reads it.
func nodeOf(state NodeState) autoscale.Node {
	n := autoscale.Node{Name: state.Name, Lost: state.Lost, Draining: state.Summary.Draining,
		Sessions: state.Summary.Sessions, SessionsMax: state.Summary.SessionsMax, MemoryUsed: state.Summary.MemoryUsed,
		TempUsed: state.Summary.TempUsed, SeatsUsed: state.Summary.SeatsUsed,
		StreamsWaiting: state.Summary.StreamsWaiting, Default: state.Summary.GroupDefault}
	if state.Summary.IdleSince > 0 {
		n.IdleSince = time.Unix(state.Summary.IdleSince, 0)
	}
	return n
}

// autoscale an auto group (spec 011): the decision from its nodes, the replicas written to its spec - the Deployment
// follows at the reconcile that write triggers - and what the autoscaler keeps in the status. A write that fails (a
// group changed meanwhile: the optimistic lock) is left to the next look; the rest of the status is written anyway.
func (r *GroupReconciler) autoscale(ctx context.Context, group *v1alpha1.HugrNodeGroup, scaling autoscale.Scaling,
	in autoscale.Input, status *v1alpha1.HugrNodeGroupStatus) {
	now := in.Now
	var kept autoscale.State
	if status.Scaling != nil {
		if status.Scaling.PressureSince != nil {
			kept.PressureSince = status.Scaling.PressureSince.Time
		}
		if status.Scaling.Last != nil {
			kept.LastChange = status.Scaling.Last.At.Time
		}
	}
	// the change's time rides on the same write as the replicas: a look that reads the new replicas reads it too,
	// whatever the status says yet (a status patch lost to a leader's end, a cache behind)
	if at, err := time.Parse(time.RFC3339, group.Annotations[AnnotationScaledAt]); err == nil && at.After(kept.LastChange) {
		kept.LastChange = at
	}
	in.State = kept
	decision := autoscale.Decide(scaling, in)
	if status.Scaling == nil {
		status.Scaling = &v1alpha1.ScalingStatus{}
	}
	status.Scaling.PressureSince = nil
	if !decision.State.PressureSince.IsZero() {
		status.Scaling.PressureSince = &metav1.Time{Time: decision.State.PressureSince}
	}
	if decision.Replicas == group.Spec.Replicas {
		return
	}
	logger := log.FromContext(ctx)
	// the group as read, or nothing: a person's change since (mode back to manual, a new max) is not written over
	patch := client.MergeFromWithOptions(group.DeepCopy(), client.MergeFromWithOptimisticLock{})
	from := group.Spec.Replicas
	annotations := group.Annotations
	group.Spec.Replicas = decision.Replicas
	group.Annotations = map[string]string{}
	for k, v := range annotations {
		group.Annotations[k] = v
	}
	group.Annotations[AnnotationScaledAt] = now.UTC().Format(time.RFC3339)
	if err := r.Patch(ctx, group, patch); err != nil {
		group.Spec.Replicas, group.Annotations = from, annotations
		logger.Info("autoscale not written, next look", "group", group.Name, "error", err.Error())
		return
	}
	logger.Info("autoscale", "group", group.Name, "from", from, "to", decision.Replicas, "reason", decision.Reason)
	status.ObservedGeneration = group.Generation
	status.Scaling.Last = &v1alpha1.ScalingDecision{At: metav1.Time{Time: now}, From: from, To: decision.Replicas,
		Reason: decision.Reason}
}

// writeStatus patches the status (a merge patch, no resourceVersion precondition): the group read from the cache may
// be a version behind - its spec changed by kubectl, say - and an update would then conflict and back off, leaving
// the status stale for as long as the backoff grows. The status is the operator's alone, so the patch is safe. A field
// the read and the new status agree on is left out of the patch: if the server holds a newer value of it (a write the
// cache has not seen yet), that value stays until the next status, StatusEvery later.
func (r *GroupReconciler) writeStatus(ctx context.Context, group *v1alpha1.HugrNodeGroup, status *v1alpha1.HugrNodeGroupStatus) error {
	if equalStatus(&group.Status, status) {
		return nil
	}
	original := group.DeepCopy()
	group.Status = *status
	return client.IgnoreNotFound(r.Status().Patch(ctx, group, client.MergeFrom(original)))
}

func (r *GroupReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// SetupWithManager registers the reconciler: a group's own objects, its platform, and nothing per Lease (the status
// is aggregated every StatusEvery, whatever the heartbeats' rate).
func (r *GroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.HugrNodeGroup{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.Service{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Watches(&v1alpha1.HugrPlatform{}, handler.EnqueueRequestsFromMapFunc(r.groupsOfPlatform)).
		Named("hugrnodegroup").
		Complete(r)
}

// groupsOfPlatform: a platform's change reaches every group of the namespace (its sizes, its plan, its image).
func (r *GroupReconciler) groupsOfPlatform(ctx context.Context, obj client.Object) []ctrl.Request {
	var groups v1alpha1.HugrNodeGroupList
	if err := r.List(ctx, &groups, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []ctrl.Request
	for _, g := range groups.Items {
		if g.Spec.Platform == obj.GetName() {
			out = append(out, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: g.Namespace, Name: g.Name}})
		}
	}
	return out
}
