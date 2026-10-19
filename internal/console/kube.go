package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"

	"github.com/hugr-lab/hugr-platform/internal/build"
)

// The console reads a node's pod, its events, its log and its usage (spec 003 round 2).
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

// Events of the pod, newest last.
func (k KubeClient) Events(ctx context.Context, pod string) ([]corev1.Event, error) {
	list, err := k.Clientset.CoreV1().Events(k.Namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fields.AndSelectors(fields.OneTermEqualSelector("involvedObject.kind", "Pod"),
			fields.OneTermEqualSelector("involvedObject.name", pod)).String(), Limit: 100})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
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
