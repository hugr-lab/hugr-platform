package console

import (
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/build"
	"github.com/hugr-lab/hugr-platform/internal/nodeapi"
	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

// PodPlaceholder stands for the pod's own name in a group's document: each node gets its own (spec 010).
const PodPlaceholder = "<pod>"

// BootstrapAnswer is what a node of the group is handed at its entry now (spec 010): the node API's own document, made
// from the platform's image, or why the node API refuses it - a refused node learns no more than "incomplete".
type BootstrapAnswer struct {
	Document nodeproto.Document `json:"document"`
	Served   bool               `json:"served"`
	Refused  string             `json:"refused,omitempty"`
}

// groupBootstrap answers the group's document: names and addresses only, never a credential (its own contract).
func (s *Server) groupBootstrap(w http.ResponseWriter, r *http.Request, _ string, _ *person) {
	platform, group, ok := s.platformGroup(w, r)
	if !ok {
		return
	}
	var groups []v1alpha1.HugrNodeGroup
	if platform.Spec.FirstPolicy != nil {
		var list v1alpha1.HugrNodeGroupList
		if err := s.Reader.List(r.Context(), &list, client.InNamespace(platform.Namespace)); err != nil {
			problem(w, http.StatusServiceUnavailable, "no_platform", "the platform's groups are not read now")
			return
		}
		groups = list.Items
	}
	// a valid label for the node's check, shown as the placeholder: every pod gets its own
	document, err := nodeapi.NodeDocument(platform, group, groups, "pod", platform.Spec.Node.Image,
		build.HeartbeatSeconds(platform, nil))
	document.NodeLabel = PodPlaceholder
	out := BootstrapAnswer{Document: document, Served: err == nil}
	if err != nil {
		out.Refused = err.Error()
	}
	answer(w, out)
}
