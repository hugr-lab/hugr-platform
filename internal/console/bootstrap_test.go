package console

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
	"github.com/hugr-lab/hugr-platform/internal/nodeapi"
	"github.com/hugr-lab/hugr-platform/internal/upgrade"
)

// TestGroupBootstrap: a group's document is the node API's for a pod of the group, its label the placeholder; a
// document the node API refuses says why; the canary's new nodes migrate; another platform's group is no group.
func TestGroupBootstrap(t *testing.T) {
	h := newHarness(t, observeInfo, node("hugr-node-etl-a", "etl"))
	get := func(path string) (int, BootstrapAnswer) {
		t.Helper()
		code, _, raw := h.do(t, "GET", APIPrefix+path, opsToken, "")
		var out BootstrapAnswer
		_ = json.Unmarshal([]byte(raw), &out)
		return code, out
	}
	// the harness's platform names no tresor and its groups no identity: the node API refuses such a document
	code, out := get("groups/etl/bootstrap")
	if code != 200 || out.Served || !strings.Contains(out.Refused, "tresor needs a host, an issuer and a client_id") ||
		out.Document.NodeLabel != PodPlaceholder {
		t.Fatalf("an incomplete platform: %d %+v", code, out)
	}

	ctx := context.Background()
	var platform v1alpha1.HugrPlatform
	if err := h.kube.Get(ctx, client.ObjectKey{Namespace: "hugr", Name: "hugr"}, &platform); err != nil {
		t.Fatal(err)
	}
	platform.Spec.Tresor.Host = "tresor.hugr-tresor.svc:443"
	platform.Spec.Node.Image = "ghcr.io/hugr-lab/hugr-node:v0.2.0"
	platform.Spec.FirstPolicy = &v1alpha1.FirstPolicySpec{Administrators: "platform_admin",
		Issuer: v1alpha1.FirstPolicyIssuer{Name: "idp", RolesClaim: "roles"}}
	if err := h.kube.Update(ctx, &platform); err != nil {
		t.Fatal(err)
	}
	var groups v1alpha1.HugrNodeGroupList
	if err := h.kube.List(ctx, &groups, client.InNamespace("hugr")); err != nil {
		t.Fatal(err)
	}
	for i := range groups.Items {
		groups.Items[i].Spec.Identity.ClientID = "hugr-node-" + groups.Items[i].Name
		if err := h.kube.Update(ctx, &groups.Items[i]); err != nil {
			t.Fatal(err)
		}
	}
	code, out = get("groups/etl/bootstrap")
	d := out.Document
	if code != 200 || !out.Served || out.Refused != "" || d.NodeGroup != "etl" || d.NodeLabel != PodPlaceholder ||
		d.Tresor.Host != "tresor.hugr-tresor.svc:443" || d.Tresor.Identity.ClientID != "hugr-node-etl" ||
		d.Tresor.Identity.Issuer != "https://login.example.org" || d.Catalog.Secret != v1alpha1.DefaultCatalogSecret ||
		d.Catalog.Migrate || d.Repository.Prefix != "https://kista.example.org/ext/corp/prod" ||
		d.Heartbeat.IntervalSeconds < 1 || d.FirstPolicy == nil || d.FirstPolicy.Administrators != "platform_admin" {
		t.Fatalf("a complete platform's document: %d %+v", code, out)
	}
	// the node API's own, for a pod of the group made from the platform's image
	var etl v1alpha1.HugrNodeGroup
	_ = h.kube.Get(ctx, client.ObjectKey{Namespace: "hugr", Name: "etl"}, &etl)
	_ = h.kube.Get(ctx, client.ObjectKey{Namespace: "hugr", Name: "hugr"}, &platform)
	_ = h.kube.List(ctx, &groups, client.InNamespace("hugr"))
	served, err := nodeapi.NodeDocument(&platform, &etl, groups.Items, "hugr-node-etl-a", platform.Spec.Node.Image,
		int32(d.Heartbeat.IntervalSeconds))
	served.NodeLabel = PodPlaceholder
	a, _ := json.Marshal(served)
	b, _ := json.Marshal(d)
	if err != nil || string(a) != string(b) {
		t.Errorf("the console's document is not the node API's:\n%s\n%s (%v)", a, b, err)
	}

	// the rollout's canary: the group's new nodes, made from the target image, migrate the catalog; another group not
	platform.Status.Upgrade = &v1alpha1.UpgradeStatus{To: platform.Spec.Node.Image, Phase: upgrade.PhaseCanary,
		Group: "etl", Migrate: true}
	if err := h.kube.Update(ctx, &platform); err != nil { // the fake keeps the status with the object
		t.Fatal(err)
	}
	if _, out := get("groups/etl/bootstrap"); !out.Document.Catalog.Migrate {
		t.Error("the canary's document does not migrate")
	}
	if _, out := get("groups/general/bootstrap"); out.Document.Catalog.Migrate {
		t.Error("another group's document migrates")
	}
	for _, path := range []string{"groups/missing/bootstrap", "groups/theirs/bootstrap"} {
		if code, _ := get(path); code != http.StatusNotFound {
			t.Errorf("%s: %d", path, code)
		}
	}
}
