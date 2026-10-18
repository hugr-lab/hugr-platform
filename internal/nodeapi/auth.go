// Package nodeapi answers the nodes (spec 001): GET /v1/bootstrap (the bootstrap document) and POST /v1/heartbeat
// (the report, kept as the pod's Lease), each for a pod of a group, bound by its projected ServiceAccount token and
// checked by TokenReview. Any replica answers: the state is Kubernetes'.
package nodeapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/build"
)

// The extra fields a bound ServiceAccount token's review carries.
const (
	ExtraPodName = "authentication.kubernetes.io/pod-name"
	ExtraPodUID  = "authentication.kubernetes.io/pod-uid"
)

// Reviewer checks a token with the API server (TokenReview, for the audience hugr-platform).
type Reviewer interface {
	Review(ctx context.Context, token string) (authenticationv1.TokenReviewStatus, error)
}

// KubeReviewer is the API server's TokenReview.
type KubeReviewer struct {
	Client kubernetes.Interface
}

// Review a token for the orchestrator's audience.
func (k KubeReviewer) Review(ctx context.Context, token string) (authenticationv1.TokenReviewStatus, error) {
	review, err := k.Client.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: []string{build.OrchestratorAud}},
	}, metav1.CreateOptions{})
	if err != nil {
		return authenticationv1.TokenReviewStatus{}, err
	}
	return review.Status, nil
}

// Node is a pod of a group, as its token proved it.
type Node struct {
	Pod      *corev1.Pod
	Group    *v1alpha1.HugrNodeGroup
	Platform *v1alpha1.HugrPlatform
}

// Refusal is why a token is not a node's: logged with the account and the pod, never with the token.
type Refusal struct {
	Reason  string
	Account string
	Pod     string
}

func (r *Refusal) Error() string {
	return r.Reason
}

func refuse(reason, account, pod string) *Refusal {
	return &Refusal{Reason: reason, Account: account, Pod: pod}
}

// The limits of a review: a token is a JWT of at most MaxTokenBytes; at most ReviewsInFlight reviews run at once,
// each within ReviewTimeout; a positive review is kept for ReviewCacheFor (keyed by the token's hash, never the
// token), so a node's heartbeats do not cost a review each - the pod chain is still read every time.
const (
	MaxTokenBytes   = 16 * 1024
	ReviewsInFlight = 32
	ReviewTimeout   = 10 * time.Second
	ReviewCacheFor  = time.Minute
)

// Authenticator turns a token into a node.
type Authenticator struct {
	Reviewer Reviewer
	// Reader reads from the operator's cache; Direct from the API server, when the cache has not seen a new pod yet.
	Reader client.Reader
	Direct client.Reader
	// Namespace the operator serves ("" = any).
	Namespace string
	Now       func() time.Time

	once     sync.Once
	inFlight chan struct{}
	mu       sync.Mutex
	reviewed map[[sha256.Size]byte]cachedReview
}

type cachedReview struct {
	status  authenticationv1.TokenReviewStatus
	expires time.Time
}

// jwtShaped: three non-empty base64url parts - anything else is refused before it costs the API server a review
func jwtShaped(token string) bool {
	if len(token) > MaxTokenBytes {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, c := range part {
			if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

func (a *Authenticator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// review a token: from the cache when a positive review of it is fresh, else by the API server, bounded
func (a *Authenticator) review(ctx context.Context, token string) (authenticationv1.TokenReviewStatus, error) {
	a.once.Do(func() {
		a.inFlight = make(chan struct{}, ReviewsInFlight)
		a.reviewed = map[[sha256.Size]byte]cachedReview{}
	})
	key := sha256.Sum256([]byte(token))
	now := a.now()
	a.mu.Lock()
	cached, ok := a.reviewed[key]
	a.mu.Unlock()
	if ok && now.Before(cached.expires) {
		return cached.status, nil
	}
	select {
	case a.inFlight <- struct{}{}:
		defer func() { <-a.inFlight }()
	case <-ctx.Done():
		return authenticationv1.TokenReviewStatus{}, ctx.Err()
	}
	bounded, cancel := context.WithTimeout(ctx, ReviewTimeout)
	defer cancel()
	status, err := a.Reviewer.Review(bounded, token)
	if err != nil || !status.Authenticated {
		return status, err
	}
	a.mu.Lock()
	for k, v := range a.reviewed { // drop what expired: the map holds one entry per live node
		if !now.Before(v.expires) {
			delete(a.reviewed, k)
		}
	}
	a.reviewed[key] = cachedReview{status: status, expires: now.Add(ReviewCacheFor)}
	a.mu.Unlock()
	return status, nil
}

func (a *Authenticator) get(ctx context.Context, key types.NamespacedName, obj client.Object) error {
	err := a.Reader.Get(ctx, key, obj)
	if apierrors.IsNotFound(err) && a.Direct != nil {
		return a.Direct.Get(ctx, key, obj)
	}
	return err
}

// Authenticate a bearer token: an error is a Refusal (403) or the API server's (503).
func (a *Authenticator) Authenticate(ctx context.Context, token string) (*Node, error) {
	if token == "" {
		return nil, refuse("no bearer token", "", "")
	}
	if !jwtShaped(token) {
		return nil, refuse("the bearer is not a token", "", "")
	}
	status, err := a.review(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("the token review: %w", err)
	}
	if !status.Authenticated {
		return nil, refuse("the token does not authenticate", "", "")
	}
	if !contains(status.Audiences, build.OrchestratorAud) {
		return nil, refuse("the token is not for the audience "+build.OrchestratorAud, status.User.Username, "")
	}
	parts := strings.Split(status.User.Username, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" {
		return nil, refuse("the token is not a ServiceAccount's", status.User.Username, "")
	}
	namespace, account := parts[2], parts[3]
	if a.Namespace != "" && namespace != a.Namespace {
		return nil, refuse("the ServiceAccount is of another namespace", status.User.Username, "")
	}
	podName := first(status.User.Extra[ExtraPodName])
	podUID := first(status.User.Extra[ExtraPodUID])
	if podName == "" || podUID == "" {
		return nil, refuse("the token is not bound to a pod", status.User.Username, "")
	}

	var sa corev1.ServiceAccount
	if err := a.get(ctx, types.NamespacedName{Namespace: namespace, Name: account}, &sa); err != nil {
		return nil, notFound(err, "the ServiceAccount is gone", status.User.Username, podName)
	}
	groupName := sa.Labels[build.LabelGroup]
	owner := metav1.GetControllerOf(&sa)
	if groupName == "" || owner == nil || owner.Kind != "HugrNodeGroup" || owner.Name != groupName {
		return nil, refuse("the ServiceAccount is not a group's", status.User.Username, podName)
	}
	var group v1alpha1.HugrNodeGroup
	if err := a.get(ctx, types.NamespacedName{Namespace: namespace, Name: groupName}, &group); err != nil {
		return nil, notFound(err, "the group is gone", status.User.Username, podName)
	}
	if group.UID != owner.UID || account != build.Name(&group) {
		return nil, refuse("the ServiceAccount is not the group's", status.User.Username, podName)
	}

	var pod corev1.Pod
	if err := a.get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, &pod); err != nil {
		return nil, notFound(err, "the pod is gone", status.User.Username, podName)
	}
	if string(pod.UID) != podUID {
		return nil, refuse("the pod is not the one the token was bound to", status.User.Username, podName)
	}
	if pod.Spec.ServiceAccountName != account || pod.Labels[build.LabelGroup] != groupName {
		return nil, refuse("the pod runs as another account or group", status.User.Username, podName)
	}
	if err := a.ofDeployment(ctx, &pod, &group); err != nil {
		return nil, err
	}

	var platform v1alpha1.HugrPlatform
	if err := a.get(ctx, types.NamespacedName{Namespace: namespace, Name: group.Spec.Platform}, &platform); err != nil {
		return nil, notFound(err, "the platform is gone", status.User.Username, podName)
	}
	return &Node{Pod: &pod, Group: &group, Platform: &platform}, nil
}

// ofDeployment checks the pod belongs to the group's Deployment: its ReplicaSet's, controlled by the group's. The
// chain is the pod's own ownerReferences - whoever may create pods in the namespace may write them, as they may run a
// pod as the group's ServiceAccount at all: the namespace is the platform's alone (spec 001, Security; the optional
// admission policy in config/policy lets only the ReplicaSet controller create a group's pods).
func (a *Authenticator) ofDeployment(ctx context.Context, pod *corev1.Pod, group *v1alpha1.HugrNodeGroup) error {
	account := "system:serviceaccount:" + pod.Namespace + ":" + pod.Spec.ServiceAccountName
	rsRef := metav1.GetControllerOf(pod)
	if rsRef == nil || rsRef.Kind != "ReplicaSet" {
		return refuse("the pod is not of a ReplicaSet", account, pod.Name)
	}
	var rs appsv1.ReplicaSet
	if err := a.get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: rsRef.Name}, &rs); err != nil {
		return notFound(err, "the pod's ReplicaSet is gone", account, pod.Name)
	}
	if rs.UID != rsRef.UID {
		return refuse("the pod's ReplicaSet is another one of that name", account, pod.Name)
	}
	dRef := metav1.GetControllerOf(&rs)
	if dRef == nil || dRef.Kind != "Deployment" || dRef.Name != build.Name(group) {
		return refuse("the pod is not of the group's Deployment", account, pod.Name)
	}
	var deployment appsv1.Deployment
	if err := a.get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: dRef.Name}, &deployment); err != nil {
		return notFound(err, "the group's Deployment is gone", account, pod.Name)
	}
	gRef := metav1.GetControllerOf(&deployment)
	if deployment.UID != dRef.UID || gRef == nil || gRef.UID != group.UID {
		return refuse("the Deployment is not the group's", account, pod.Name)
	}
	return nil
}

func notFound(err error, reason, account, pod string) error {
	if apierrors.IsNotFound(err) {
		return refuse(reason, account, pod)
	}
	return err
}

func contains(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}

func first(values authenticationv1.ExtraValue) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
