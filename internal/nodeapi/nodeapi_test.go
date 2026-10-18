package nodeapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/build"
	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

// fakeReviewer answers a TokenReview per token, and counts the reviews it was asked for.
type fakeReviewer struct {
	answers map[string]authenticationv1.TokenReviewStatus
	calls   int
}

func (f *fakeReviewer) Review(_ context.Context, token string) (authenticationv1.TokenReviewStatus, error) {
	f.calls++
	return f.answers[token], nil
}

// jwt is a token-shaped bearer for a name (three base64url parts): what the API server would review
func jwt(name string) string {
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(name)) + ".c2lnbmF0dXJl"
}

func ref(owner metav1.Object, kind, apiVersion string) metav1.OwnerReference {
	yes := true
	return metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: owner.GetName(), UID: owner.GetUID(), Controller: &yes}
}

type world struct {
	client   client.Client
	server   *Server
	reviewer *fakeReviewer
	now      time.Time
}

// newWorld: the platform, the groups etl and bi, etl's ServiceAccount, Deployment, ReplicaSet and pod, bi's pod.
func newWorld(t *testing.T) *world {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	platform := &v1alpha1.HugrPlatform{ObjectMeta: metav1.ObjectMeta{Name: "hugr", Namespace: "hugr"},
		Spec: v1alpha1.HugrPlatformSpec{
			Node:       v1alpha1.NodeSpec{Image: "img"},
			IdP:        v1alpha1.IdPSpec{Issuer: "https://login.example/t/v2.0"},
			Tresor:     v1alpha1.TresorSpec{Host: "tresor.example:443"},
			Repository: v1alpha1.RepositorySpec{Prefix: "https://kista.example/t/prod"},
			Heartbeat:  v1alpha1.HeartbeatSpec{IntervalSeconds: 10},
		}}
	var objs []client.Object
	// an account of the namespace that is no group's
	objs = append(objs, platform, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "hugr"}})
	for _, name := range []string{"etl", "bi"} {
		group := &v1alpha1.HugrNodeGroup{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "hugr", UID: types.UID("group-" + name)},
			Spec: v1alpha1.HugrNodeGroupSpec{Platform: "hugr", Replicas: 1, Size: "large",
				Identity: v1alpha1.WorkloadIdentity{ClientID: name + "-app"}}}
		groupRef := ref(group, "HugrNodeGroup", v1alpha1.GroupVersion.String())
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: build.Name(group), Namespace: "hugr",
			Labels: map[string]string{build.LabelGroup: name}, OwnerReferences: []metav1.OwnerReference{groupRef}}}
		deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: build.Name(group), Namespace: "hugr",
			UID: types.UID("deploy-" + name), OwnerReferences: []metav1.OwnerReference{groupRef}}}
		rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: build.Name(group) + "-7f9c", Namespace: "hugr",
			UID: types.UID("rs-" + name), OwnerReferences: []metav1.OwnerReference{ref(deployment, "Deployment", "apps/v1")}}}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: build.Name(group) + "-7f9c-x2k8p", Namespace: "hugr",
			UID: types.UID("pod-" + name), Labels: map[string]string{build.LabelGroup: name},
			OwnerReferences: []metav1.OwnerReference{ref(rs, "ReplicaSet", "apps/v1")}},
			Spec: corev1.PodSpec{ServiceAccountName: build.Name(group)}}
		objs = append(objs, group, sa, deployment, rs, pod)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	w := &world{client: c, reviewer: &fakeReviewer{answers: map[string]authenticationv1.TokenReviewStatus{}},
		now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	w.server = &Server{Auth: &Authenticator{Reviewer: w.reviewer, Reader: c, Direct: c, Namespace: "hugr",
		Now: func() time.Time { return w.now }},
		Leases: c, Direct: c, Log: logr.Discard(), Now: func() time.Time { return w.now }}
	// the good token: etl's pod
	w.reviewer.answers[etlToken] = review("hugr", "hugr-node-etl", "hugr-node-etl-7f9c-x2k8p", "pod-etl")
	return w
}

// etlToken stands for etl's pod's projected token
var etlToken = jwt("etl-pod-token-7d1")

func review(namespace, account, pod, uid string) authenticationv1.TokenReviewStatus {
	extra := map[string]authenticationv1.ExtraValue{}
	if pod != "" {
		extra[ExtraPodName] = authenticationv1.ExtraValue{pod}
		extra[ExtraPodUID] = authenticationv1.ExtraValue{uid}
	}
	return authenticationv1.TokenReviewStatus{Authenticated: true, Audiences: []string{build.OrchestratorAud},
		User: authenticationv1.UserInfo{Username: "system:serviceaccount:" + namespace + ":" + account, Extra: extra}}
}

func (w *world) call(method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	w.server.Handler().ServeHTTP(rec, req)
	return rec
}

func TestBootstrapAnswersTheGroupsPod(t *testing.T) {
	w := newWorld(t)
	rec := w.call(http.MethodGet, "/v1/bootstrap", etlToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var doc nodeproto.Document
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.NodeGroup != "etl" || doc.NodeLabel != "hugr-node-etl-7f9c-x2k8p" || doc.Tresor.Identity.ClientID != "etl-app" ||
		doc.Heartbeat.IntervalSeconds != 10 {
		t.Errorf("the document is not the group's: %+v", doc)
	}
	if strings.Contains(rec.Body.String(), etlToken) {
		t.Error("the document carries the token")
	}
}

// TestTokenReviewRefusals: every answer but a bound token of a group's own pod is a 403.
func TestTokenReviewRefusals(t *testing.T) {
	w := newWorld(t)
	notAuthenticated := review("hugr", "hugr-node-etl", "hugr-node-etl-7f9c-x2k8p", "pod-etl")
	notAuthenticated.Authenticated = false
	otherAudience := review("hugr", "hugr-node-etl", "hugr-node-etl-7f9c-x2k8p", "pod-etl")
	otherAudience.Audiences = []string{"https://kubernetes.default.svc"}
	user := review("hugr", "x", "", "")
	user.User.Username = "alice"
	w.reviewer.answers[jwt("not-authenticated")] = notAuthenticated
	w.reviewer.answers[jwt("other-audience")] = otherAudience
	w.reviewer.answers[jwt("a-user")] = user
	w.reviewer.answers[jwt("foreign-account")] = review("hugr", "default", "hugr-node-etl-7f9c-x2k8p", "pod-etl")
	w.reviewer.answers[jwt("other-namespace")] = review("prod", "hugr-node-etl", "hugr-node-etl-7f9c-x2k8p", "pod-etl")
	w.reviewer.answers[jwt("unbound")] = review("hugr", "hugr-node-etl", "", "")
	w.reviewer.answers[jwt("pod-of-another-group")] = review("hugr", "hugr-node-etl", "hugr-node-bi-7f9c-x2k8p", "pod-bi")
	w.reviewer.answers[jwt("missing-pod")] = review("hugr", "hugr-node-etl", "hugr-node-etl-gone", "pod-gone")
	w.reviewer.answers[jwt("recreated-pod")] = review("hugr", "hugr-node-etl", "hugr-node-etl-7f9c-x2k8p", "an-older-uid")
	for _, name := range []string{"unknown", "not-authenticated", "other-audience", "a-user", "foreign-account",
		"other-namespace", "unbound", "pod-of-another-group", "missing-pod", "recreated-pod"} {
		if rec := w.call(http.MethodGet, "/v1/bootstrap", jwt(name), ""); rec.Code != http.StatusForbidden {
			t.Errorf("token %q: status %d, want 403", name, rec.Code)
		}
	}
	// a bearer that is no token is refused before it costs the API server a review
	calls := w.reviewer.calls
	for _, garbage := range []string{"", "not-a-token", "a.b", "a..c", "a.b.c=", jwt(strings.Repeat("x", MaxTokenBytes))} {
		if rec := w.call(http.MethodGet, "/v1/bootstrap", garbage, ""); rec.Code != http.StatusForbidden {
			t.Errorf("garbage %.20q: status %d, want 403", garbage, rec.Code)
		}
	}
	if w.reviewer.calls != calls {
		t.Errorf("garbage bearers cost %d reviews", w.reviewer.calls-calls)
	}
}

func TestPodNotOfTheGroupsDeployment(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	// a pod someone made by hand, running as the group's account with its label - not of its Deployment
	var etl v1alpha1.HugrNodeGroup
	_ = w.client.Get(ctx, types.NamespacedName{Namespace: "hugr", Name: "etl"}, &etl)
	hand := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "by-hand", Namespace: "hugr", UID: "pod-hand",
		Labels: map[string]string{build.LabelGroup: "etl"}}, Spec: corev1.PodSpec{ServiceAccountName: "hugr-node-etl"}}
	if err := w.client.Create(ctx, hand); err != nil {
		t.Fatal(err)
	}
	w.reviewer.answers[jwt("hand")] = review("hugr", "hugr-node-etl", "by-hand", "pod-hand")
	if rec := w.call(http.MethodGet, "/v1/bootstrap", jwt("hand"), ""); rec.Code != http.StatusForbidden {
		t.Errorf("a pod outside the group's Deployment: status %d, want 403", rec.Code)
	}
}

const beat = `{"node_id": "hugr-node-etl-7f9c-x2k8p", "config": {"target": 4, "applied": 4, "needs_restart": false},
 "load": {"draining": false, "group_known": true, "sessions": {"live": 3}, "admit": {"new_session": true}}}`

func TestHeartbeatKeepsTheLease(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if rec := w.call(http.MethodPost, "/v1/heartbeat", etlToken, beat); rec.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var lease coordinationv1.Lease
	if err := w.client.Get(ctx, types.NamespacedName{Namespace: "hugr", Name: "hugr-node-etl-7f9c-x2k8p"}, &lease); err != nil {
		t.Fatal(err)
	}
	if *lease.Spec.HolderIdentity != "hugr-node-etl-7f9c-x2k8p" || *lease.Spec.LeaseDurationSeconds != 30 {
		t.Errorf("holder %v duration %v, want the pod and 3 intervals", *lease.Spec.HolderIdentity, *lease.Spec.LeaseDurationSeconds)
	}
	if len(lease.OwnerReferences) != 1 || lease.OwnerReferences[0].Kind != "Pod" || lease.OwnerReferences[0].UID != "pod-etl" {
		t.Errorf("the Lease is not the pod's: %+v", lease.OwnerReferences)
	}
	if lease.Labels[build.LabelGroup] != "etl" {
		t.Errorf("the Lease is not labelled with its group: %v", lease.Labels)
	}
	s := nodeproto.ParseSummary(lease.Annotations)
	if s.Target != 4 || s.Applied != 4 || s.Sessions != 3 || !s.AdmitSession {
		t.Errorf("the summary: %+v", s)
	}
	first := lease.Spec.RenewTime.Time
	w.now = w.now.Add(10 * time.Second)
	if rec := w.call(http.MethodPost, "/v1/heartbeat", etlToken, strings.Replace(beat, `"live": 3`, `"live": 9`, 1)); rec.Code != http.StatusNoContent {
		t.Fatalf("second beat: status %d", rec.Code)
	}
	_ = w.client.Get(ctx, types.NamespacedName{Namespace: "hugr", Name: "hugr-node-etl-7f9c-x2k8p"}, &lease)
	if !lease.Spec.RenewTime.Time.After(first) || nodeproto.ParseSummary(lease.Annotations).Sessions != 9 {
		t.Errorf("the second beat did not renew the Lease: %v %v", lease.Spec.RenewTime, lease.Annotations)
	}
}

func TestHeartbeatForAnotherNode(t *testing.T) {
	w := newWorld(t)
	body := strings.Replace(beat, "hugr-node-etl-7f9c-x2k8p", "hugr-node-bi-7f9c-x2k8p", 1)
	if rec := w.call(http.MethodPost, "/v1/heartbeat", etlToken, body); rec.Code != http.StatusConflict {
		t.Errorf("a node beating for another: status %d, want 409", rec.Code)
	}
	var lease coordinationv1.Lease
	if err := w.client.Get(context.Background(), types.NamespacedName{Namespace: "hugr", Name: "hugr-node-bi-7f9c-x2k8p"}, &lease); err == nil {
		t.Error("the other node's Lease was written")
	}
}

func TestMethodsAndHealth(t *testing.T) {
	w := newWorld(t)
	if rec := w.call(http.MethodPost, "/v1/bootstrap", etlToken, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /v1/bootstrap: %d", rec.Code)
	}
	if rec := w.call(http.MethodGet, "/v1/heartbeat", etlToken, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/heartbeat: %d", rec.Code)
	}
	if rec := w.call(http.MethodGet, "/healthz", "", ""); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("/healthz: %d %q - unauthenticated, and nothing about the fleet", rec.Code, rec.Body)
	}
}

func TestIncompletePlatformIsNotServed(t *testing.T) {
	w := newWorld(t)
	var etl v1alpha1.HugrNodeGroup
	ctx := context.Background()
	_ = w.client.Get(ctx, types.NamespacedName{Namespace: "hugr", Name: "etl"}, &etl)
	etl.Spec.Identity.ClientID = ""
	if err := w.client.Update(ctx, &etl); err != nil {
		t.Fatal(err)
	}
	if rec := w.call(http.MethodGet, "/v1/bootstrap", etlToken, ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a document the node would refuse: status %d, want 503", rec.Code)
	}
}

// TestReviewCache: a node's heartbeats cost one review a minute, not one each - the pod chain is still read each time
func TestReviewCache(t *testing.T) {
	w := newWorld(t)
	for i := 0; i < 3; i++ {
		if rec := w.call(http.MethodGet, "/v1/bootstrap", etlToken, ""); rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
	}
	if w.reviewer.calls != 1 {
		t.Errorf("%d reviews for three calls within a minute, want 1", w.reviewer.calls)
	}
	w.now = w.now.Add(ReviewCacheFor + time.Second)
	_ = w.call(http.MethodGet, "/v1/bootstrap", etlToken, "")
	if w.reviewer.calls != 2 {
		t.Errorf("%d reviews after the cache expired, want 2", w.reviewer.calls)
	}
	// a cached review does not keep a deleted pod in: the pod is read every time
	var pod corev1.Pod
	ctx := context.Background()
	_ = w.client.Get(ctx, types.NamespacedName{Namespace: "hugr", Name: "hugr-node-etl-7f9c-x2k8p"}, &pod)
	_ = w.client.Delete(ctx, &pod)
	if rec := w.call(http.MethodGet, "/v1/bootstrap", etlToken, ""); rec.Code != http.StatusForbidden {
		t.Errorf("a deleted pod's cached token: status %d, want 403", rec.Code)
	}
}

// TestReplicaSetOfAnotherUID: the pod's ownerReference names the ReplicaSet by name and uid - a ReplicaSet recreated
// under that name is not the one the pod was made by
func TestReplicaSetOfAnotherUID(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	var pod corev1.Pod
	_ = w.client.Get(ctx, types.NamespacedName{Namespace: "hugr", Name: "hugr-node-etl-7f9c-x2k8p"}, &pod)
	pod.OwnerReferences[0].UID = "an-old-replicaset"
	if err := w.client.Update(ctx, &pod); err != nil {
		t.Fatal(err)
	}
	if rec := w.call(http.MethodGet, "/v1/bootstrap", etlToken, ""); rec.Code != http.StatusForbidden {
		t.Errorf("a ReplicaSet of another uid: status %d, want 403", rec.Code)
	}
}

// TestForeignLeaseIsNotTaken: a Lease named like the pod that the operator did not write for it (the leader
// election's, anyone's) is never renewed by a heartbeat
func TestForeignLeaseIsNotTaken(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	holder := "someone"
	foreign := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "hugr-node-etl-7f9c-x2k8p", Namespace: "hugr"},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder}}
	if err := w.client.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if rec := w.call(http.MethodPost, "/v1/heartbeat", etlToken, beat); rec.Code != http.StatusConflict {
		t.Errorf("a heartbeat over a foreign Lease: status %d, want 409", rec.Code)
	}
	var lease coordinationv1.Lease
	_ = w.client.Get(ctx, types.NamespacedName{Namespace: "hugr", Name: "hugr-node-etl-7f9c-x2k8p"}, &lease)
	if *lease.Spec.HolderIdentity != "someone" {
		t.Errorf("the foreign Lease was taken: holder %q", *lease.Spec.HolderIdentity)
	}
}
