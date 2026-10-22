package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/build"
	"github.com/hugr-lab/hugr-platform/internal/upgrade"
)

// UpgradeReconciler moves a platform's groups onto a new node image in order (spec 008): it reads what each group
// runs and writes the platform's status.image and status.upgrade. The group reconciler gives each group its image
// (upgrade.ImageFor), and the node API serves the canary its migration flag.
type UpgradeReconciler struct {
	client.Client
	// Repository answers what the repository lacks for the target (upgrade.Repository by default)
	Repository func(ctx context.Context, platform *v1alpha1.HugrPlatform, platforms, extensions []string) (string, error)
	Now        func() time.Time
}

// +kubebuilder:rbac:groups=platform.hugr.io,resources=hugrplatforms,verbs=get;list;watch
// +kubebuilder:rbac:groups=platform.hugr.io,resources=hugrplatforms/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

// Reconcile one platform's rollout.
func (r *UpgradeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var platform v1alpha1.HugrPlatform
	if err := r.Get(ctx, req.NamespacedName, &platform); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if platform.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}
	groups, platforms, extensions, err := r.fleet(ctx, &platform)
	if err != nil {
		return ctrl.Result{}, err
	}
	check := func() (string, error) {
		if platform.Spec.Node.DuckDBVersion == "" || len(platforms) == 0 || len(extensions) == 0 {
			return "", nil // nothing to judge by: a release that names no DuckDB, or a fleet that reports none
		}
		return r.repository(ctx, &platform, platforms, extensions)
	}
	image, up := upgrade.Decide(r.now(), upgrade.TargetOf(&platform), platform.Status.Image, platform.Status.Upgrade,
		groups, check)
	if image != platform.Status.Image || !equality.Semantic.DeepEqual(up, platform.Status.Upgrade) {
		// the rollout's fields are this reconciler's alone: a merge patch of them, whatever else the status holds
		original := platform.DeepCopy()
		platform.Status.Image, platform.Status.Upgrade = image, up
		if err := r.Status().Patch(ctx, &platform, client.MergeFrom(original)); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	return ctrl.Result{RequeueAfter: StatusEvery}, nil
}

// fleet is the platform's groups as the rollout sees them, with the platforms its nodes run and the catalog's
// extensions they installed from the repository (what a new release must find there).
func (r *UpgradeReconciler) fleet(ctx context.Context, platform *v1alpha1.HugrPlatform) ([]upgrade.Group, []string,
	[]string, error) {
	var groups v1alpha1.HugrNodeGroupList
	if err := r.List(ctx, &groups, client.InNamespace(platform.Namespace)); err != nil {
		return nil, nil, nil, err
	}
	var out []upgrade.Group
	var platforms, extensions []string
	now := r.now()
	defaultGroup := ""
	if platform.Spec.FirstPolicy != nil {
		defaultGroup = platform.Spec.FirstPolicy.DefaultGroup
	}
	for i := range groups.Items {
		group := &groups.Items[i]
		if group.Spec.Platform != platform.Name || group.DeletionTimestamp != nil {
			continue
		}
		g := upgrade.Group{Name: group.Name, Replicas: group.Spec.Replicas,
			Default: defaultGroup != "" && group.EffectiveResourceGroup() == defaultGroup}
		var deployment appsv1.Deployment
		err := r.Get(ctx, types.NamespacedName{Namespace: group.Namespace, Name: build.Name(group)}, &deployment)
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return nil, nil, nil, err
		default:
			g.Image = build.NodeImage(&deployment.Spec.Template.Spec)
			g.Rolled, g.Failed = DeploymentRolled(&deployment)
		}
		var pods corev1.PodList
		if err := r.List(ctx, &pods, client.InNamespace(group.Namespace),
			client.MatchingLabels(build.SelectorLabels(group))); err != nil {
			return nil, nil, nil, err
		}
		var leases coordinationv1.LeaseList
		if err := r.List(ctx, &leases, client.InNamespace(group.Namespace),
			client.MatchingLabels{build.LabelGroup: group.Name}); err != nil {
			return nil, nil, nil, err
		}
		byPod := map[string]*coordinationv1.Lease{}
		for j := range leases.Items {
			byPod[leases.Items[j].Name] = &leases.Items[j]
		}
		for j := range pods.Items {
			pod := &pods.Items[j]
			lease, ok := byPod[pod.Name]
			if pod.DeletionTimestamp != nil || !ok {
				continue
			}
			state := NodeStateOf(pod.Name, lease, now)
			if state.Lost {
				continue
			}
			s := state.Summary
			if s.GroupDefault && defaultGroup == "" {
				g.Default = true // no first policy names it: the nodes say which group the policy's default is
			}
			g.Nodes = append(g.Nodes, upgrade.Node{Name: pod.Name, Image: build.NodeImage(&pod.Spec),
				DuckDBVersion: s.DuckDBVersion, CatalogBuild: s.CatalogBuild, CatalogSchema: s.CatalogSchema,
				CatalogMinReader: s.CatalogMinReader})
			platforms = append(platforms, s.Platform)
			extensions = append(extensions, s.CatalogExtension)
		}
		out = append(out, g)
	}
	return out, platforms, extensions, nil
}

// DeploymentRolled says whether a Deployment runs only its template - its generation observed, every replica updated
// and available, none of an older ReplicaSet left - or that its rollout stopped: its progress deadline passed.
func DeploymentRolled(d *appsv1.Deployment) (bool, string) {
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse &&
			c.Reason == "ProgressDeadlineExceeded" {
			return false, c.Message
		}
	}
	if d.Status.ObservedGeneration < d.Generation {
		return false, ""
	}
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	return s.UpdatedReplicas == want && s.AvailableReplicas == want && s.Replicas == want, ""
}

// repository asks the platform's repository, trusting the platform's CA bundle (the one its nodes trust) or else the
// operator's own roots.
func (r *UpgradeReconciler) repository(ctx context.Context, platform *v1alpha1.HugrPlatform, platforms,
	extensions []string) (string, error) {
	if r.Repository != nil {
		return r.Repository(ctx, platform, platforms, extensions)
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if ref := platform.Spec.CABundle; ref != nil {
		var cm corev1.ConfigMap
		key := types.NamespacedName{Namespace: platform.Namespace, Name: ref.ConfigMapRef.Name}
		if err := r.Get(ctx, key, &cm); err != nil {
			return "", err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(cm.Data[ref.ConfigMapRef.Key])) {
			return "", errors.New("the platform's CA bundle holds no certificate")
		}
		tlsConfig.RootCAs = pool
	}
	httpClient := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsConfig,
		Proxy: http.ProxyFromEnvironment}}
	return upgrade.Repository(ctx, httpClient, platform.Spec.Repository.Prefix, platform.Spec.Node.DuckDBVersion,
		platforms, extensions)
}

func (r *UpgradeReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// SetupWithManager registers the reconciler: the platform, and every StatusEvery after (the groups' Deployments and
// Leases are read, not watched: the rollout moves at the status's pace).
func (r *UpgradeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.HugrPlatform{}).
		Named("upgrade").
		Complete(r)
}
