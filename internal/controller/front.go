package controller

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/build"
)

// ConditionFront is the platform's condition for its front (spec 002).
const ConditionFront = "FrontReady"

// FrontReconciler keeps a platform's front: its ServiceAccount, Role and RoleBinding, Deployment, Service and
// PodDisruptionBudget, built from spec.front. A platform without spec.front has no front: the objects of one it had
// are deleted (the change that asked for it is explicit).
type FrontReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Settings build.Settings
}

// +kubebuilder:rbac:groups=platform.hugr.io,resources=hugrplatforms/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=serviceaccounts;services,verbs=delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=delete

// Reconcile one platform's front.
func (r *FrontReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var platform v1alpha1.HugrPlatform
	if err := r.Get(ctx, req.NamespacedName, &platform); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if platform.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}
	if platform.Spec.Front == nil {
		return ctrl.Result{}, r.remove(ctx, &platform)
	}
	var applyErr error
	objects := []client.Object{build.FrontServiceAccount(&platform), build.FrontRole(&platform),
		build.FrontRoleBinding(&platform), build.FrontService(&platform), build.FrontPodDisruptionBudget(&platform)}
	deployment, err := build.FrontDeployment(&platform, r.Settings)
	if err != nil {
		applyErr = err
	} else {
		objects = append(objects, deployment)
	}
	for _, obj := range objects {
		if err := r.apply(ctx, &platform, obj); err != nil && applyErr == nil {
			applyErr = err
		}
	}
	condition := metav1.Condition{Type: ConditionFront, Status: metav1.ConditionTrue, Reason: "Applied",
		Message: "the front's objects are applied", ObservedGeneration: platform.Generation}
	if applyErr != nil {
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "ApplyFailed", applyErr.Error()
	}
	if meta.SetStatusCondition(&platform.Status.Conditions, condition) {
		if err := r.Status().Update(ctx, &platform); err != nil && !apierrors.IsConflict(err) {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, applyErr
}

func (r *FrontReconciler) apply(ctx context.Context, platform *v1alpha1.HugrPlatform, obj client.Object) error {
	if err := controllerutil.SetControllerReference(platform, obj, r.Scheme); err != nil {
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

// remove the front's objects the platform owns.
func (r *FrontReconciler) remove(ctx context.Context, platform *v1alpha1.HugrPlatform) error {
	key := types.NamespacedName{Namespace: platform.Namespace, Name: build.FrontName}
	for _, obj := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}, &policyv1.PodDisruptionBudget{},
		&rbacv1.RoleBinding{}, &rbacv1.Role{}, &corev1.ServiceAccount{}} {
		if err := r.Get(ctx, key, obj); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if !metav1.IsControlledBy(obj, platform) {
			continue // not ours: an object of that name the administrator made
		}
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	if !meta.RemoveStatusCondition(&platform.Status.Conditions, ConditionFront) {
		return nil
	}
	return client.IgnoreNotFound(r.Status().Update(ctx, platform))
}

// SetupWithManager registers the reconciler: the platform and the front's own objects.
func (r *FrontReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.HugrPlatform{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.Service{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Named("front").
		Complete(r)
}
