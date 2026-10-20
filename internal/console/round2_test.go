package console

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/hugr-lab/hugr-platform/internal/build"
	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
)

type fakeKube struct {
	logs        []string // "<pod> <previous> <lines>"
	noUsage     bool
	unsampled   bool // the metrics API answers, not yet for the pod
	down        bool // the API server fails every log read
	pods        []corev1.Pod
	deployments []appsv1.Deployment
}

func (k *fakeKube) Pod(_ context.Context, name string) (*corev1.Pod, error) {
	for i := range k.pods {
		if k.pods[i].Name == name {
			return k.pods[i].DeepCopy(), nil
		}
	}
	if name != "hugr-node-etl-a" {
		return nil, errors.New("not found")
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PodSpec{NodeName: "aks-pool1-vmss000002", Containers: []corev1.Container{{Name: build.NodeName,
			Image: "hugr-node:0.4.0", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("16Gi")}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.3.17",
			ContainerStatuses: []corev1.ContainerStatus{{Name: build.NodeName, RestartCount: 3, ImageID: "sha256:4f1c",
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Reason: "OOMKilled", ExitCode: 137}}}}}}, nil
}

func (k *fakeKube) Events(context.Context, string) ([]corev1.Event, error) {
	return []corev1.Event{
		{Reason: "Scheduled", Message: "assigned", LastTimestamp: metav1.NewTime(testNow.Add(-time.Hour))},
		{Reason: "Started", Message: "started", LastTimestamp: metav1.NewTime(testNow.Add(-time.Minute))},
	}, nil
}

func (k *fakeKube) Logs(_ context.Context, pod string, previous bool, lines int64) (string, error) {
	k.logs = append(k.logs, pod+" "+map[bool]string{true: "previous", false: "current"}[previous]+" "+
		strings.Repeat("x", int(lines%7)))
	if k.down {
		return "", errors.New("the server is currently unable to handle the request")
	}
	if previous {
		// what the API server answers a container that has not restarted
		return "", apierrors.NewBadRequest(`previous terminated container "hugr-node" in pod "` + pod + `" not found`)
	}
	return "bootstrap: environment read\ndoors: Flight on :32010\n", nil
}

func (k *fakeKube) GroupPods(context.Context) ([]corev1.Pod, error) { return k.pods, nil }

func (k *fakeKube) GroupDeployments(context.Context) ([]appsv1.Deployment, error) {
	return k.deployments, nil
}

func (k *fakeKube) PodWarnings(context.Context) ([]corev1.Event, error) { return nil, nil }

func (k *fakeKube) Usage(context.Context, string) (*Usage, error) {
	if k.noUsage || k.unsampled {
		return nil, nil
	}
	return &Usage{CPUMilli: 2100, MemoryBytes: 9 << 30}, nil
}

func (k *fakeKube) UsageAll(context.Context) (map[string]Usage, error) {
	if k.noUsage {
		return nil, nil
	}
	if k.unsampled {
		return map[string]Usage{}, nil
	}
	return map[string]Usage{"hugr-node-etl-a": {CPUMilli: 2100, MemoryBytes: 9 << 30}}, nil
}

func clusterRights() *Result {
	return infoRow([]string{"observe", "operate", "cluster"}, []string{"node_load", "sessions", "audit_events", "drift",
		"cluster_items", "cluster_effective"},
		[]string{"kill_session", "drain", "resume", "session_audit_level", "session_profile"})
}

func TestRound2Rights(t *testing.T) {
	routes := []struct {
		method, path, body string
		allowed            func(Rights) bool
	}{
		{"GET", "nodes/hugr-node-etl-a/kubernetes", "", func(r Rights) bool { return r.Fleet }},
		{"GET", "nodes/hugr-node-etl-a/memory", "", func(r Rights) bool { return r.Fleet }},
		{"GET", "nodes/hugr-node-etl-a/inventory", "", func(r Rights) bool { return r.Fleet }},
		{"GET", "nodes/hugr-node-etl-a/logs", "", func(r Rights) bool { return r.Infrastructure }},
		{"GET", "nodes/hugr-node-etl-a/events", "", func(r Rights) bool { return r.Events }},
		{"POST", "nodes/hugr-node-etl-a/sessions/0A1B2C3D4E5F/tracing", `{"audit": "all"}`,
			func(r Rights) bool { return r.Tracing }},
		{"GET", "cluster", "", func(r Rights) bool { return r.Fleet }},
	}
	for _, info := range []*Result{infoRow(nil, nil, nil), observeInfo, operateInfo, clusterRights()} {
		for _, route := range routes {
			h := newHarness(t, info, node("hugr-node-etl-a", "etl"))
			h.server.Kube = &fakeKube{}
			code, _, _ := h.do(t, route.method, APIPrefix+route.path, opsToken, route.body)
			rights, _ := infoOf(info)
			if route.allowed(RightsOf(rights)) {
				if code >= 400 && code != http.StatusNotFound {
					t.Errorf("%s %s with %v: %d, want allowed", route.method, route.path, rights.Bundles, code)
				}
			} else if code != http.StatusForbidden {
				t.Errorf("%s %s with %v: %d, want 403", route.method, route.path, rights.Bundles, code)
			}
		}
	}
}

func TestKubernetesPart(t *testing.T) {
	h := newHarness(t, clusterRights(), node("hugr-node-etl-a", "etl"))
	h.server.Kube = &fakeKube{}
	code, out, _ := h.do(t, "GET", APIPrefix+"nodes/hugr-node-etl-a/kubernetes", opsToken, "")
	if code != 200 || out["restarts"] != float64(3) || out["lastTermination"].(map[string]any)["reason"] != "OOMKilled" ||
		out["limits"].(map[string]any)["memoryBytes"] != float64(16<<30) || out["usage"].(map[string]any)["cpuMilli"] != float64(2100) ||
		out["podIP"] != "10.244.3.17" || out["imageID"] != "sha256:4f1c" {
		t.Fatalf("the pod: %d %v", code, out)
	}
	events := out["events"].([]any)
	if len(events) != 2 || events[0].(map[string]any)["reason"] != "Started" {
		t.Errorf("the events, newest first: %v", events)
	}
	h.server.Kube = &fakeKube{noUsage: true}
	_, out, _ = h.do(t, "GET", APIPrefix+"nodes/hugr-node-etl-a/kubernetes", opsToken, "")
	if out["usage"] != nil || !strings.Contains(out["usageError"].(string), "no metrics API") || out["usagePending"] != nil {
		t.Errorf("no metrics API: the page says so: %v", out)
	}
	// a new pod: the API answers, not yet for it - the page asks again shortly
	h.server.Kube = &fakeKube{unsampled: true}
	_, out, _ = h.do(t, "GET", APIPrefix+"nodes/hugr-node-etl-a/kubernetes", opsToken, "")
	if out["usage"] != nil || out["usagePending"] != true || !strings.Contains(out["usageError"].(string), "no sample of this pod yet") {
		t.Errorf("a pod not sampled yet: %v", out)
	}
}

func TestLogsBounded(t *testing.T) {
	h := newHarness(t, clusterRights(), node("hugr-node-etl-a", "etl"))
	kube := &fakeKube{}
	h.server.Kube = kube
	code, out, _ := h.do(t, "GET", APIPrefix+"nodes/hugr-node-etl-a/logs?lines=500", opsToken, "")
	if code != 200 || !strings.Contains(out["text"].(string), "bootstrap") {
		t.Fatalf("the log: %d %v", code, out)
	}
	for _, q := range []string{"lines=0", "lines=501", "lines=x"} {
		if code, _, _ := h.do(t, "GET", APIPrefix+"nodes/hugr-node-etl-a/logs?"+q, opsToken, ""); code != 400 {
			t.Errorf("%s: %d", q, code)
		}
	}
	code, out, _ = h.do(t, "GET", APIPrefix+"nodes/hugr-node-etl-a/logs?previous=true", opsToken, "")
	if code != 404 || out["error"] != "no_previous" {
		t.Errorf("no previous container: %d %v", code, out)
	}
	kube.down = true
	code, out, _ = h.do(t, "GET", APIPrefix+"nodes/hugr-node-etl-a/logs?previous=true", opsToken, "")
	if code != http.StatusServiceUnavailable || out["error"] != "kubernetes" {
		t.Errorf("the API server's failure is no container's history: %d %v", code, out)
	}
}

func TestTracing(t *testing.T) {
	h := newHarness(t, clusterRights(), node("hugr-node-etl-a", "etl"))
	h.nodes.answer = func(_ fleet.Node, statement string) (*Result, error) {
		switch {
		case statement == stmtInfo:
			return clusterRights(), nil
		case strings.Contains(statement, "'0A1B2C3D4E5F'"):
			return &Result{Columns: []string{"x"}, Rows: [][]any{{true}}}, nil
		}
		return &Result{Columns: []string{"x"}, Rows: [][]any{{false}}}, nil
	}
	path := APIPrefix + "nodes/hugr-node-etl-a/sessions/0a1b2c3d4e5f/tracing"
	if code, _, _ := h.do(t, "POST", path, opsToken, `{"audit": "all", "profile": "sampled"}`); code != 204 {
		t.Fatalf("both levels: %d", code)
	}
	if h.nodes.statements("SET SESSION '0A1B2C3D4E5F' AUDIT LEVEL ALL") != 1 ||
		h.nodes.statements("PROFILE SESSION '0A1B2C3D4E5F' SAMPLED") != 1 {
		t.Errorf("the statements: %v", h.nodes.calls)
	}
	if code, _, _ := h.do(t, "POST", path, opsToken, `{"audit": "", "profile": ""}`); code != 204 ||
		h.nodes.statements("SET SESSION '0A1B2C3D4E5F' AUDIT LEVEL DEFAULT") != 1 ||
		h.nodes.statements("PROFILE SESSION '0A1B2C3D4E5F' OFF") != 1 {
		t.Errorf("inherit clears both overrides: %v", h.nodes.calls)
	}
	for _, body := range []string{`{}`, `{"audit": "everything"}`, `{"profile": "off"}`, `{"audit": "ALL; DROP"}`,
		`{"level": "all"}`} {
		if code, _, _ := h.do(t, "POST", path, opsToken, body); code != 400 {
			t.Errorf("%s: %d", body, code)
		}
	}
	if code, _, _ := h.do(t, "POST", APIPrefix+"nodes/hugr-node-etl-a/sessions/FFFFFFFFFFFF/tracing", opsToken,
		`{"audit": "all"}`); code != 404 {
		t.Errorf("a session the node does not hold: %d", code)
	}
	for _, c := range h.nodes.calls {
		if strings.Contains(c.statement, "DROP") || strings.Contains(c.statement, "everything") {
			t.Fatalf("the browser's text reached a node: %q", c.statement)
		}
	}
}

func TestPartsShowWhatTheNodeAllows(t *testing.T) {
	h := newHarness(t, clusterRights(), node("hugr-node-etl-a", "etl"))
	h.nodes.answer = func(_ fleet.Node, statement string) (*Result, error) {
		switch statement {
		case stmtInfo:
			return clusterRights(), nil
		case stmtMemory:
			return &Result{Columns: []string{"tag", "memory_usage_bytes"}, Rows: [][]any{{"BASE_TABLE", int64(1024)}}}, nil
		case stmtSpill:
			return &Result{Columns: []string{"size"}, Rows: [][]any{{int64(10)}, {int64(32)}}}, nil
		case stmtDrift, stmtEffective:
			return nil, &NodeError{Code: codes.InvalidArgument, Message: `acl: no access to object "platform.drift"`}
		}
		return &Result{Columns: []string{"extension_name"}, Rows: [][]any{{"acl"}}}, nil
	}
	_, out, _ := h.do(t, "GET", APIPrefix+"nodes/hugr-node-etl-a/memory", opsToken, "")
	if out["spillBytes"] != float64(42) || len(out["tags"].(map[string]any)["rows"].([]any)) != 1 {
		t.Errorf("the engine's memory, the spill summed: %v", out)
	}
	_, out, _ = h.do(t, "GET", APIPrefix+"nodes/hugr-node-etl-a/inventory", opsToken, "")
	if out["drift"].(map[string]any)["error"] == "" || len(out["extensions"].(map[string]any)["rows"].([]any)) != 1 {
		t.Errorf("a part the node refuses says so, the others stand: %v", out)
	}
	observe := newHarness(t, observeInfo, node("hugr-node-etl-a", "etl"))
	_, out, _ = observe.do(t, "GET", APIPrefix+"nodes/hugr-node-etl-a/inventory", opsToken, "")
	if !strings.Contains(out["drift"].(map[string]any)["error"].(string), "cluster bundle") ||
		observe.nodes.statements(stmtDrift) != 0 {
		t.Errorf("without the cluster right the profile's views are not asked: %v", out)
	}
	// a standalone node keeps no profile: console_info names no profile view, whatever the bundles - nothing is asked,
	// and the page says why
	standalone := infoRow([]string{"observe", "operate", "passthrough"}, []string{"node_load", "sessions"}, nil)
	standalone.Rows[0][4] = "standalone"
	solo := newHarness(t, standalone, node("hugr-node-etl-a", "etl"))
	_, out, _ = solo.do(t, "GET", APIPrefix+"nodes/hugr-node-etl-a/inventory", opsToken, "")
	if !strings.Contains(out["drift"].(map[string]any)["error"].(string), "standalone node") ||
		solo.nodes.statements(stmtDrift) != 0 || solo.nodes.statements(stmtEffective) != 0 {
		t.Errorf("a standalone node's inventory: %v", out)
	}
	_, out, _ = solo.do(t, "GET", APIPrefix+"cluster", opsToken, "")
	if !strings.Contains(out["profile"].(map[string]any)["error"].(string), "standalone node") ||
		solo.nodes.statements(stmtItems) != 0 {
		t.Errorf("a standalone node's cluster page: %v", out)
	}
}

func TestClusterPage(t *testing.T) {
	h := newHarness(t, clusterRights(), node("hugr-node-etl-a", "etl"), node("hugr-node-etl-b", "etl"))
	code, out, _ := h.do(t, "GET", APIPrefix+"cluster", opsToken, "")
	if code != 200 {
		t.Fatalf("cluster: %d", code)
	}
	lineage := out["lineage"].([]any)
	if len(lineage) != 2 || lineage[0].(map[string]any)["node"] != "hugr-node-etl-a" {
		t.Errorf("one lineage row per answering node, in order: %v", lineage)
	}
	if h.nodes.statements(stmtItems) != 1 || h.nodes.statements(stmtLineage) != 2 {
		t.Errorf("the profile once, the lineage per node: %v", h.nodes.calls)
	}
}

func TestGroupIdentity(t *testing.T) {
	h := newHarness(t, observeInfo, node("hugr-node-etl-a", "etl"))
	_, out, _ := h.do(t, "GET", APIPrefix+"groups", opsToken, "")
	id := out["groups"].([]any)[0].(map[string]any)["identity"].(map[string]any)
	if id["serviceAccount"] != "hugr-node-etl" || id["subject"] != "system:serviceaccount:hugr:hugr-node-etl" ||
		id["tlsName"] != "hugr-node-etl.hugr.svc" {
		t.Errorf("the group's identity: %v", id)
	}
}

func TestUsageOf(t *testing.T) {
	u, err := usageOf([]byte(`{"containers": [{"name": "sidecar", "usage": {"cpu": "1", "memory": "1Gi"}},
		{"name": "hugr-node", "usage": {"cpu": "2100m", "memory": "9663676416"}}]}`))
	if err != nil || u.CPUMilli != 2100 || u.MemoryBytes != 9663676416 {
		t.Errorf("the node container's usage: %+v %v", u, err)
	}
}

// The cluster's profile is read on a node that answers the person: one of another resource group refuses it.
func TestProfileFromANodeThatAnswers(t *testing.T) {
	h := newHarness(t, clusterRights(), node("hugr-node-etl-a", "etl"), node("hugr-node-general-a", "general"))
	h.nodes.answer = func(n fleet.Node, statement string) (*Result, error) {
		switch {
		case statement == stmtInfo:
			return clusterRights(), nil
		case n.Group() == "etl":
			return nil, &NodeError{Code: codes.Unavailable, Message: "acl: wrong resource group", Groups: []string{"general"}}
		case statement == stmtItems:
			return &Result{Columns: []string{"kind", "name"}, Rows: [][]any{{"setting", "memory_limit"}}}, nil
		}
		return &Result{Columns: []string{"status"}, Rows: [][]any{{"off"}}}, nil
	}
	code, out, _ := h.do(t, "GET", APIPrefix+"cluster", opsToken, "")
	profile := out["profile"].(map[string]any)
	if code != 200 || profile["error"] != nil || len(profile["rows"].([]any)) != 1 {
		t.Errorf("the profile as the person's own group's node answers it: %d %v", code, out)
	}
}

// A node that does not answer the first read of a page is not asked the others: each would wait its deadline.
func TestMemoryOfASilentNode(t *testing.T) {
	h := newHarness(t, clusterRights(), node("hugr-node-etl-a", "etl"))
	h.nodes.answer = func(_ fleet.Node, statement string) (*Result, error) {
		if statement == stmtInfo {
			return clusterRights(), nil
		}
		return nil, errUnreachable
	}
	h.do(t, "GET", APIPrefix+"me", opsToken, "") // the rights, read once
	for _, path := range []string{"memory", "inventory"} {
		before := len(h.nodes.calls)
		code, out, _ := h.do(t, "GET", APIPrefix+"nodes/hugr-node-etl-a/"+path, opsToken, "")
		if code != 200 || len(h.nodes.calls)-before != 1 {
			t.Errorf("%s of a silent node: %d, %d reads, %v", path, code, len(h.nodes.calls)-before, out)
		}
	}
}

// A pod of the platform's group with no endpoint yet has its page: Kubernetes' parts answer, the node's own say it does
// not answer. Another platform's pod has none.
func TestPageOfAPodNotServing(t *testing.T) {
	h := newHarness(t, clusterRights(), node("hugr-node-etl-a", "etl"))
	made := func(name, group string) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"platform.hugr.io/group": group},
			CreationTimestamp: metav1.NewTime(testNow.Add(-time.Minute))},
			Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{Name: "hugr-node",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}}}}
	}
	h.server.Kube = &fakeKube{pods: []corev1.Pod{made("hugr-node-etl-new", "etl"), made("hugr-node-theirs-new", "theirs")}}
	code, out, _ := h.do(t, "GET", APIPrefix+"nodes/hugr-node-etl-new", opsToken, "")
	k, _ := out["kubernetes"].(map[string]any)
	if code != 200 || out["loadError"] == nil || k == nil || k["pod"].(map[string]any)["state"] != "starting" {
		t.Errorf("the page of a pod being made: %d %v", code, out)
	}
	if code, _, _ := h.do(t, "GET", APIPrefix+"nodes/hugr-node-etl-new/kubernetes", opsToken, ""); code != 200 {
		t.Errorf("its pod and events: %d", code)
	}
	if code, _, _ := h.do(t, "GET", APIPrefix+"nodes/hugr-node-theirs-new", opsToken, ""); code != http.StatusNotFound {
		t.Errorf("another platform's pod: %d", code)
	}
}
