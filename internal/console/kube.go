package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"

	"github.com/hugr-lab/hugr-platform/internal/build"
)

// The console reads a node's pod, its events, its log and its usage (spec 003 round 2), and the groups' pods and
// Deployments - how far a scale has got.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list

// Log bounds: a page reads at most this much of a pod's log (spec 003 round 2, Q6).
const (
	logLinesMax = 500
	logBytesMax = 1 << 20
)

// Kube is what the console reads of Kubernetes about a node's pod.
type Kube interface {
	Pod(ctx context.Context, name string) (*corev1.Pod, error)
	Events(ctx context.Context, pod string) ([]corev1.Event, error)
	Logs(ctx context.Context, pod string, previous bool, lines int64) (string, error)
	// Usage is the pod's CPU and memory now; nil without the metrics API (metrics-server).
	Usage(ctx context.Context, pod string) (*Usage, error)
	// UsageAll is every pod's of the namespace, by name: one read for a page of nodes; nil without the metrics API.
	UsageAll(ctx context.Context) (map[string]Usage, error)
	// GroupPods are the namespace's node pods (a group's label), those a scale made before they serve included.
	GroupPods(ctx context.Context) ([]corev1.Pod, error)
	// GroupDeployments are the groups' Deployments: a pod their ReplicaSet cannot make says so there.
	GroupDeployments(ctx context.Context) ([]appsv1.Deployment, error)
	// PodWarnings are the namespace's Warning events of pods: a pod being made that will not be says so there first.
	PodWarnings(ctx context.Context) ([]corev1.Event, error)
}

// Usage of a pod, by the metrics API.
type Usage struct {
	CPUMilli    int64 `json:"cpuMilli"`
	MemoryBytes int64 `json:"memoryBytes"`
}

// KubeClient reads the namespace's pods through the API server.
type KubeClient struct {
	Clientset kubernetes.Interface
	Namespace string
}

// Pod by name.
func (k KubeClient) Pod(ctx context.Context, name string) (*corev1.Pod, error) {
	return k.Clientset.CoreV1().Pods(k.Namespace).Get(ctx, name, metav1.GetOptions{})
}

// Events of the pod, all of them (the API server keeps a pod's for an hour, a repeat counted on one): the page keeps
// the newest.
func (k KubeClient) Events(ctx context.Context, pod string) ([]corev1.Event, error) {
	list, err := k.Clientset.CoreV1().Events(k.Namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fields.AndSelectors(fields.OneTermEqualSelector("involvedObject.kind", "Pod"),
			fields.OneTermEqualSelector("involvedObject.name", pod)).String()})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// GroupPods by the group label.
func (k KubeClient) GroupPods(ctx context.Context) ([]corev1.Pod, error) {
	list, err := k.Clientset.CoreV1().Pods(k.Namespace).List(ctx, metav1.ListOptions{LabelSelector: build.LabelGroup})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// PodWarnings by their type.
func (k KubeClient) PodWarnings(ctx context.Context) ([]corev1.Event, error) {
	list, err := k.Clientset.CoreV1().Events(k.Namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fields.AndSelectors(fields.OneTermEqualSelector("involvedObject.kind", "Pod"),
			fields.OneTermEqualSelector("type", corev1.EventTypeWarning)).String()})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// GroupDeployments by the group label.
func (k KubeClient) GroupDeployments(ctx context.Context) ([]appsv1.Deployment, error) {
	list, err := k.Clientset.AppsV1().Deployments(k.Namespace).List(ctx, metav1.ListOptions{LabelSelector: build.LabelGroup})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// PodState is how far a node's pod has got, as Kubernetes says it: starting (made, not serving yet), running,
// stopping (going away: a drain, a scale down), failing - it will not start as it is (the reason and Kubernetes' own
// message say why) - or ended (a terminal phase: an eviction's leftover, replaced by its ReplicaSet; history, not a
// node).
type PodState struct {
	State    string     `json:"state"`
	Phase    string     `json:"phase"`
	Reason   string     `json:"reason,omitempty"`
	Message  string     `json:"message,omitempty"`
	Restarts int32      `json:"restarts"`
	Created  *time.Time `json:"created,omitempty"`
}

// waitingFails are the reasons a container waits that it will not leave by itself.
var waitingFails = map[string]bool{"CrashLoopBackOff": true, "ImagePullBackOff": true, "ErrImagePull": true,
	"CreateContainerConfigError": true, "CreateContainerError": true, "InvalidImageName": true,
	"RunContainerError": true, "PreCreateHookError": true, "PostStartHookError": true}

// madeFails are the warnings a pod being made does not get past by itself: a Secret or a ConfigMap it mounts is
// missing, the network has no address for it.
var madeFails = map[string]bool{"FailedMount": true, "FailedAttachVolume": true, "FailedCreatePodSandBox": true,
	"FailedCreatePodContainer": true}

// stuckAfter is how long a pod may wait to be placed or made before it is called failing: a cluster that adds a node
// for it (an autoscaler) takes minutes.
const stuckAfter = 10 * time.Minute

// podState of a node's pod at `now`; warning is the newest Warning event of the pod, if any.
func podState(p *corev1.Pod, now time.Time, warning *corev1.Event) PodState {
	out := PodState{Phase: string(p.Status.Phase)}
	var age time.Duration
	if !p.CreationTimestamp.IsZero() {
		t := p.CreationTimestamp.Time
		out.Created = &t
		age = now.Sub(t)
	}
	statuses := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
	for _, c := range statuses {
		out.Restarts += c.RestartCount
	}
	switch {
	case p.DeletionTimestamp != nil:
		out.State, out.Reason = "stopping", "Terminating"
		return out
	case p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded:
		out.State, out.Reason, out.Message = "ended", p.Status.Reason, p.Status.Message
		if out.Reason == "" {
			out.Reason = string(p.Status.Phase)
		}
		return out
	}
	for _, c := range statuses {
		if w := c.State.Waiting; w != nil && waitingFails[w.Reason] {
			out.State, out.Reason, out.Message = "failing", w.Reason, w.Message
			if t := c.LastTerminationState.Terminated; t != nil && w.Reason == "CrashLoopBackOff" {
				out.Message = fmt.Sprintf("the container exited with %d", t.ExitCode)
				if t.Reason != "" {
					out.Message += " (" + t.Reason + ")"
				}
			}
			return out
		}
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			// no node of the cluster takes it now: a full cluster, a size it cannot fit - or not yet, while a node joins
			out.State, out.Reason, out.Message = "starting", c.Reason, c.Message
			if age >= stuckAfter {
				out.State = "failing"
			}
			return out
		}
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			out.State = "running"
			return out
		}
	}
	out.State = "starting"
	for _, c := range statuses {
		if w := c.State.Waiting; w != nil {
			out.Reason = w.Reason // ContainerCreating, PodInitializing
			break
		}
	}
	if out.Reason == "" && p.Status.Phase == corev1.PodRunning {
		out.Reason = "NotReady" // its readiness probe: the node bootstraps, then its doors serve
	}
	switch {
	case warning != nil && madeFails[warning.Reason]:
		out.State, out.Reason, out.Message = "failing", warning.Reason, warning.Message
	case p.Status.Phase == corev1.PodPending && age >= stuckAfter:
		out.State, out.Message = "failing", "not made in "+stuckAfter.String()+": the pod's events say why"
	}
	return out
}

// newestWarnings are the pods' newest Warning events, by pod name.
func newestWarnings(events []corev1.Event) map[string]*corev1.Event {
	at := func(e *corev1.Event) time.Time {
		switch {
		case !e.LastTimestamp.IsZero():
			return e.LastTimestamp.Time
		case !e.EventTime.IsZero():
			return e.EventTime.Time
		}
		return e.CreationTimestamp.Time
	}
	out := map[string]*corev1.Event{}
	for i := range events {
		e := &events[i]
		if e.Type != corev1.EventTypeWarning || e.InvolvedObject.Kind != "Pod" {
			continue
		}
		if held := out[e.InvolvedObject.Name]; held == nil || at(e).After(at(held)) {
			out[e.InvolvedObject.Name] = e
		}
	}
	return out
}

// deploymentProblem is what a group's Deployment says stops its pods: the ReplicaSet cannot make one (a quota, an
// admission policy), or its pods are not available within its deadline (a roll's, a scale's).
func deploymentProblem(d *appsv1.Deployment) string {
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue {
			return "a pod cannot be made: " + c.Message
		}
	}
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse {
			return "the group's pods are not available within its deadline: " + c.Message
		}
	}
	return ""
}

// Logs of the pod's node container, the last `lines` (bounded) or the previous container's.
func (k KubeClient) Logs(ctx context.Context, pod string, previous bool, lines int64) (string, error) {
	limit := int64(logBytesMax)
	stream, err := k.Clientset.CoreV1().Pods(k.Namespace).GetLogs(pod, &corev1.PodLogOptions{Container: build.NodeName,
		Previous: previous, TailLines: &lines, LimitBytes: &limit}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	data, err := io.ReadAll(io.LimitReader(stream, logBytesMax))
	return string(data), err
}

// Usage by the metrics API (metrics.k8s.io/v1beta1), read raw: no dependency on its client for two numbers.
func (k KubeClient) Usage(ctx context.Context, pod string) (*Usage, error) {
	raw, err := k.Clientset.CoreV1().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces", k.Namespace, "pods", pod).DoRaw(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil // no metrics API, or no sample of the pod yet
		}
		return nil, err
	}
	return usageOf(raw)
}

// UsageAll by the metrics API's list of the namespace's pods.
func (k KubeClient) UsageAll(ctx context.Context) (map[string]Usage, error) {
	raw, err := k.Clientset.CoreV1().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces", k.Namespace, "pods").DoRaw(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	out := map[string]Usage{}
	for _, item := range list.Items {
		var named struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if json.Unmarshal(item, &named) != nil {
			continue
		}
		if u, err := usageOf(item); err == nil {
			out[named.Metadata.Name] = *u
		}
	}
	return out, nil
}

func usageOf(raw []byte) (*Usage, error) {
	var metrics struct {
		Containers []struct {
			Name  string            `json:"name"`
			Usage map[string]string `json:"usage"`
		} `json:"containers"`
	}
	if err := json.Unmarshal(raw, &metrics); err != nil {
		return nil, err
	}
	u := &Usage{}
	for _, c := range metrics.Containers {
		if c.Name != build.NodeName {
			continue
		}
		if q, err := resource.ParseQuantity(c.Usage["cpu"]); err == nil {
			u.CPUMilli = q.MilliValue()
		}
		if q, err := resource.ParseQuantity(c.Usage["memory"]); err == nil {
			u.MemoryBytes = q.Value()
		}
	}
	return u, nil
}

// PodAnswer is `GET nodes/{label}/kubernetes`: what Kubernetes says of the node's pod.
type PodAnswer struct {
	Phase      string        `json:"phase"`
	StartedAt  *time.Time    `json:"startedAt"`
	NodeName   string        `json:"nodeName"`
	PodIP      string        `json:"podIP"`
	Image      string        `json:"image"`
	ImageID    string        `json:"imageID"`
	Restarts   int32         `json:"restarts"`
	LastEnd    *EndAnswer    `json:"lastTermination"`
	Requests   ResourcesPair `json:"requests"`
	Limits     ResourcesPair `json:"limits"`
	Usage      *Usage        `json:"usage"`
	UsageError string        `json:"usageError,omitempty"`
	// UsagePending: the metrics API has not sampled the pod yet - a page asks again shortly.
	UsagePending bool `json:"usagePending,omitempty"`
	// CertificateExpiry of the node's doors, as the console's last TLS handshake with it showed it.
	CertificateExpiry *time.Time    `json:"certificateExpiry"`
	Events            []EventAnswer `json:"events"`
}

// ResourcesPair is a container's CPU (millicores) and memory (bytes).
type ResourcesPair struct {
	CPUMilli    int64 `json:"cpuMilli"`
	MemoryBytes int64 `json:"memoryBytes"`
}

// EndAnswer is how the container last stopped.
type EndAnswer struct {
	Reason     string    `json:"reason"`
	ExitCode   int32     `json:"exitCode"`
	FinishedAt time.Time `json:"finishedAt"`
}

// EventAnswer is one of the pod's events.
type EventAnswer struct {
	Type    string    `json:"type"`
	Reason  string    `json:"reason"`
	Message string    `json:"message"`
	Count   int32     `json:"count"`
	Last    time.Time `json:"last"`
}

func podAnswer(pod *corev1.Pod, events []corev1.Event) PodAnswer {
	a := PodAnswer{Phase: string(pod.Status.Phase), NodeName: pod.Spec.NodeName, PodIP: pod.Status.PodIP,
		Events: []EventAnswer{}}
	if pod.Status.StartTime != nil {
		t := pod.Status.StartTime.Time
		a.StartedAt = &t
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == build.NodeName {
			a.Image = c.Image
			a.Requests = ResourcesPair{CPUMilli: c.Resources.Requests.Cpu().MilliValue(),
				MemoryBytes: c.Resources.Requests.Memory().Value()}
			a.Limits = ResourcesPair{CPUMilli: c.Resources.Limits.Cpu().MilliValue(),
				MemoryBytes: c.Resources.Limits.Memory().Value()}
		}
	}
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name != build.NodeName {
			continue
		}
		a.ImageID = s.ImageID
		a.Restarts = s.RestartCount
		if t := s.LastTerminationState.Terminated; t != nil {
			a.LastEnd = &EndAnswer{Reason: t.Reason, ExitCode: t.ExitCode, FinishedAt: t.FinishedAt.Time}
		}
	}
	for _, e := range events {
		last := e.LastTimestamp.Time
		if last.IsZero() {
			last = e.EventTime.Time
		}
		a.Events = append(a.Events, EventAnswer{Type: e.Type, Reason: e.Reason, Message: e.Message, Count: e.Count,
			Last: last})
	}
	sort.Slice(a.Events, func(i, j int) bool { return a.Events[i].Last.After(a.Events[j].Last) })
	if len(a.Events) > 20 {
		a.Events = a.Events[:20]
	}
	return a
}

// errNoPrevious: the container has not restarted, so there is no previous log.
var errNoPrevious = errors.New("this container has not restarted: there is no previous log")
