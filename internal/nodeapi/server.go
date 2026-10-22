package nodeapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/build"
	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

// Server answers the nodes.
type Server struct {
	Auth *Authenticator
	// Leases reads (from the cache) and writes the pods' Leases; Direct reads them from the API server.
	Leases client.Client
	Direct client.Reader
	Log    logr.Logger
	Now    func() time.Time
}

// Handler of the node API, health included.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/bootstrap", s.bootstrap)
	mux.HandleFunc("/v1/heartbeat", s.heartbeat)
	// unauthenticated, and nothing about the fleet
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	mux.HandleFunc("/healthz", ok)
	mux.HandleFunc("/readyz", ok)
	return mux
}

func (s *Server) node(w http.ResponseWriter, r *http.Request) *Node {
	token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found {
		token = ""
	}
	node, err := s.Auth.Authenticate(r.Context(), strings.TrimSpace(token))
	if err == nil {
		return node
	}
	var refusal *Refusal
	if errors.As(err, &refusal) {
		logger := s.Log
		if refusal.Account == "" {
			logger = s.Log.V(1) // no account: a garbage bearer - not every one of a flood is worth a line
		}
		logger.Info("node refused", "path", r.URL.Path, "reason", refusal.Reason, "account", refusal.Account,
			"pod", refusal.Pod)
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil
	}
	s.Log.Error(err, "node not checked", "path", r.URL.Path)
	http.Error(w, "the orchestrator cannot check the token now", http.StatusServiceUnavailable)
	return nil
}

// bootstrap answers a pod its group's document: pointers and names only.
func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	node := s.node(w, r)
	if node == nil {
		return
	}
	document := build.Document(node.Platform, node.Group, node.Pod.Name,
		build.HeartbeatSeconds(node.Platform, node.Pod.Annotations))
	if node.Platform.Spec.FirstPolicy != nil {
		// spec 005: the first policy names the namespace's groups, so a node of any of them may write it
		var groups v1alpha1.HugrNodeGroupList
		if err := s.Auth.Reader.List(r.Context(), &groups, client.InNamespace(node.Platform.Namespace)); err != nil {
			s.Log.Error(err, "groups not listed for the first policy", "pod", node.Pod.Name)
			http.Error(w, "the orchestrator cannot read the platform now", http.StatusServiceUnavailable)
			return
		}
		policy, err := build.FirstPolicy(node.Platform, groups.Items)
		if err != nil {
			s.Log.Info("bootstrap document incomplete", "group", node.Group.Name, "pod", node.Pod.Name, "reason", err.Error())
			http.Error(w, "the platform's configuration is incomplete", http.StatusServiceUnavailable)
			return
		}
		document.FirstPolicy = policy
	}
	if err := document.Validate(); err != nil {
		// the installation is incomplete: the node would refuse the document, so it is not served
		s.Log.Info("bootstrap document incomplete", "group", node.Group.Name, "pod", node.Pod.Name, "reason", err.Error())
		http.Error(w, "the platform's configuration is incomplete", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(document)
}

// heartbeat keeps a pod's report as its Lease: renewed, with a summary in its annotations.
func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	node := s.node(w, r)
	if node == nil {
		return
	}
	report, load, err := nodeproto.ReadReport(r.Body, node.Pod.Name)
	switch {
	case errors.Is(err, nodeproto.ErrForeignNode):
		s.Log.Info("heartbeat for another node refused", "pod", node.Pod.Name)
		http.Error(w, "a node speaks only for itself", http.StatusConflict)
		return
	case errors.Is(err, nodeproto.ErrTooLarge):
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// the pod's own interval, the one its document gave it
	interval := time.Duration(build.HeartbeatSeconds(node.Platform, node.Pod.Annotations)) * time.Second
	summary := nodeproto.SummaryOf(report, load).Placed(node.Group.EffectiveResourceGroup())
	err = s.renew(r.Context(), node, summary, 3*interval)
	if errors.Is(err, errForeignLease) {
		s.Log.Info("a Lease named like the pod is not its own", "pod", node.Pod.Name)
		http.Error(w, "a Lease of this name is not the node's", http.StatusConflict)
		return
	}
	if err != nil {
		s.Log.Error(err, "lease not renewed", "pod", node.Pod.Name)
		http.Error(w, "the orchestrator cannot keep the report now", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// errForeignLease: a Lease of the pod's name that the operator did not write for this pod (the leader election's, one
// someone else keeps) - never taken over
var errForeignLease = errors.New("a Lease named like the pod is not its own")

// renew the pod's Lease: named after the pod, owned by it (it goes with the pod), held by it for 3 intervals.
func (s *Server) renew(ctx context.Context, node *Node, summary nodeproto.Summary, duration time.Duration) error {
	now := metav1.NewMicroTime(s.now())
	seconds := int32(duration / time.Second)
	key := types.NamespacedName{Namespace: node.Pod.Namespace, Name: node.Pod.Name}
	// a create racing another replica's is an AlreadyExists: read it and update instead
	retriable := func(err error) bool { return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) }
	attempt := 0
	return retry.OnError(retry.DefaultRetry, retriable, func() error {
		// the operator's cache first (it watches the Leases); after a conflict, the API server's
		reader := client.Reader(s.Leases)
		if attempt > 0 {
			reader = s.Direct
		}
		attempt++
		var lease coordinationv1.Lease
		err := reader.Get(ctx, key, &lease)
		if apierrors.IsNotFound(err) {
			lease = coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{
					Name: key.Name, Namespace: key.Namespace,
					Labels:      map[string]string{build.LabelGroup: node.Group.Name, build.LabelManagedBy: build.ManagedBy},
					Annotations: summary.Annotations(),
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: node.Pod.Name,
						UID: node.Pod.UID}},
				},
				Spec: coordinationv1.LeaseSpec{HolderIdentity: &key.Name, LeaseDurationSeconds: &seconds,
					AcquireTime: &now, RenewTime: &now},
			}
			return s.Leases.Create(ctx, &lease)
		}
		if err != nil {
			return err
		}
		if lease.Labels[build.LabelManagedBy] != build.ManagedBy || !ownedBy(&lease, node.Pod.UID) {
			return errForeignLease
		}
		if lease.Annotations == nil {
			lease.Annotations = map[string]string{}
		}
		written := summary.Annotations()
		for k, v := range written {
			lease.Annotations[k] = v
		}
		// what this report leaves out goes: a key never outlives the report that wrote it
		for _, k := range nodeproto.ReportedKeys {
			if _, ok := written[k]; !ok {
				delete(lease.Annotations, k)
			}
		}
		lease.Spec.HolderIdentity = &key.Name
		lease.Spec.LeaseDurationSeconds = &seconds
		lease.Spec.RenewTime = &now
		return s.Leases.Update(ctx, &lease)
	})
}

func ownedBy(lease *coordinationv1.Lease, uid types.UID) bool {
	for _, ref := range lease.OwnerReferences {
		if ref.Kind == "Pod" && ref.UID == uid {
			return true
		}
	}
	return false
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
