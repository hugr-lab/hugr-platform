// Package webhook is the validating webhook of node groups (spec 001): a change of a group is checked against the
// installation's plan where it is written, and a refusal reaches kubectl, the console or the Managed Application with
// its reason. The reconcile checks every group again (internal/controller).
package webhook

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/build"
	"github.com/hugr-lab/hugr-platform/internal/controller"
	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
	"github.com/hugr-lab/hugr-platform/pkg/plan"
)

// GroupValidator checks a group's change.
type GroupValidator struct {
	Reader client.Reader
}

// +kubebuilder:webhook:path=/validate-platform-hugr-io-v1alpha1-hugrnodegroup,mutating=false,failurePolicy=fail,sideEffects=None,groups=platform.hugr.io,resources=hugrnodegroups,verbs=create;update,versions=v1alpha1,name=vhugrnodegroup.platform.hugr.io,admissionReviewVersions=v1

// ValidateCreate checks a new group.
func (v *GroupValidator) ValidateCreate(ctx context.Context, group *v1alpha1.HugrNodeGroup) (admission.Warnings, error) {
	return nil, v.check(ctx, group)
}

// ValidateUpdate checks a changed group. A change that does not grow it - a scale-down, a restart (restartedAt), its
// identity - is let through even while the namespace is past the plan: it is how a group gets back within it.
func (v *GroupValidator) ValidateUpdate(ctx context.Context, old, group *v1alpha1.HugrNodeGroup) (admission.Warnings, error) {
	if group.DeletionTimestamp != nil {
		return nil, nil // a group going (the garbage collector's finalizer removal is an update)
	}
	if group.Spec.ResourceGroup != old.Spec.ResourceGroup {
		if err := CheckResourceGroup(group.Spec.ResourceGroup); err != nil {
			return nil, err
		}
	}
	if old.Spec.Size == group.Spec.Size && old.Spec.Platform == group.Spec.Platform && group.Spec.Replicas <= old.Spec.Replicas {
		return nil, CheckTLS(group.Spec.TLS)
	}
	return nil, v.check(ctx, group)
}

// ValidateDelete lets a group go: deleting never exceeds a plan.
func (v *GroupValidator) ValidateDelete(context.Context, *v1alpha1.HugrNodeGroup) (admission.Warnings, error) {
	return nil, nil
}

// errNotRead is what a write learns when the webhook could not read what it judges by: try again.
var errNotRead = errors.New("the platform's groups could not be read to judge this; try again")

func (v *GroupValidator) check(ctx context.Context, group *v1alpha1.HugrNodeGroup) error {
	if err := plan.CheckName(group.Name); err != nil {
		return err
	}
	if err := CheckTLS(group.Spec.TLS); err != nil {
		return err
	}
	if err := CheckResourceGroup(group.Spec.ResourceGroup); err != nil {
		return err
	}
	var platform v1alpha1.HugrPlatform
	if err := v.Reader.Get(ctx, types.NamespacedName{Namespace: group.Namespace, Name: group.Spec.Platform}, &platform); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("the platform %q is not in the namespace %q", group.Spec.Platform, group.Namespace)
		}
		// the API server's own error names the operator's ServiceAccount and its URLs: logged, never the answer (a
		// person reads the answer in the console, spec 006)
		log.FromContext(ctx).Error(err, "the platform not read")
		return errNotRead
	}
	if err := build.CheckSize(&platform, group.Spec.Size); err != nil {
		return err
	}
	var groups v1alpha1.HugrNodeGroupList
	if err := v.Reader.List(ctx, &groups, client.InNamespace(group.Namespace)); err != nil {
		log.FromContext(ctx).Error(err, "the groups not read")
		return errNotRead
	}
	var others []plan.Group
	for _, g := range groups.Items {
		if g.Name != group.Name && g.DeletionTimestamp == nil {
			others = append(others, plan.Group{Name: g.Name, Size: g.Spec.Size, Replicas: g.Spec.Replicas})
		}
	}
	return controller.PlanOf(&platform).CheckChange(
		plan.Group{Name: group.Name, Size: group.Spec.Size, Replicas: group.Spec.Replicas}, others)
}

// CheckResourceGroup says whether a group may name this resource group (empty: its own name). Every node's bootstrap
// document lists every group's in the first policy, which takes plain names only: one it refuses stops every node of
// the platform from starting (spec 006). A group that names one already keeps it until the name changes.
func CheckResourceGroup(name string) error {
	if name != "" && !nodeproto.ValidPlainName(name) {
		return fmt.Errorf("the resource group %q is not a plain name: letters, digits, '_' and '-', at most 63", name)
	}
	return nil
}

// TLSSecretPrefix: a group mounts into its pods only a Secret named for the nodes - never another of the namespace
// (the operator's own serving key, a catalog's), which a group's editor without secrets/get could otherwise hand to
// every node pod
const TLSSecretPrefix = "hugr-node-"

// CheckTLSSecret says whether a group may mount the Secret it names as the doors' certificate.
func CheckTLSSecret(name string) error {
	if !strings.HasPrefix(name, TLSSecretPrefix) {
		return fmt.Errorf("the doors' certificate %q is not a Secret named %s… - a group mounts no other Secret of the "+
			"namespace", name, TLSSecretPrefix)
	}
	return nil
}

// CheckTLS says whether a group may name the doors' certificate it names: exactly one of a Secret named for the
// nodes and cert-manager's csi-driver, whose certificate lasts at least an hour. The csi-driver's names are the
// operator's (the group's Service), so a group's editor chooses only which issuer signs them.
func CheckTLS(tls v1alpha1.TLSSpec) error {
	if (tls.SecretRef == nil) == (tls.CSI == nil) {
		return fmt.Errorf("the doors' certificate names exactly one of tls.secretRef and tls.csi")
	}
	if tls.SecretRef != nil {
		return CheckTLSSecret(tls.SecretRef.Name)
	}
	if tls.CSI.Duration != "" {
		if d, err := time.ParseDuration(tls.CSI.Duration); err != nil || d < time.Hour {
			return fmt.Errorf("the doors' certificate's duration %q is not a duration of at least 1h", tls.CSI.Duration)
		}
	}
	return nil
}

// SetupWithManager registers the webhook.
func (v *GroupValidator) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &v1alpha1.HugrNodeGroup{}).WithValidator(v).Complete()
}
