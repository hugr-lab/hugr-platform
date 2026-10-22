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
	refusal, err := r.checkPlan(ctx, &platform, &group)
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
		deployment, err := build.Deployment(&platform, &group, r.Settings)
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

	if err := r.aggregate(ctx, &group, status); err != nil {
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

// checkPlan answers the group's refusal ("" when it fits).
func (r *GroupReconciler) checkPlan(ctx context.Context, platform *v1alpha1.HugrPlatform, group *v1alpha1.HugrNodeGroup) (string, error) {
	var groups v1alpha1.HugrNodeGroupList
	if err := r.List(ctx, &groups, client.InNamespace(group.Namespace)); err != nil {
		return "", err
	}
	var deployments appsv1.DeploymentList
	if err := r.reader().List(ctx, &deployments, client.InNamespace(group.Namespace), client.HasLabels{build.LabelGroup}); err != nil {
		return "", err
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
	return PlanOf(platform).Admit(demands)[group.Name], nil
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
func (r *GroupReconciler) aggregate(ctx context.Context, group *v1alpha1.HugrNodeGroup, status *v1alpha1.HugrNodeGroupStatus) error {
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
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		lease, ok := byPod[pod.Name]
		if !ok {
			continue // not reported yet: neither lost nor counted
		}
		state := NodeStateOf(pod.Name, lease, r.now())
		nodes = append(nodes, state)
		if err := r.writeDeletionCost(ctx, pod, DeletionCost(state)); err != nil {
			return err
		}
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
// changes in kind, not at every session; a lost node costs -1, so a scale-down takes it before a live one.
func DeletionCost(state NodeState) int64 {
	if state.Lost {
		return -1
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
