// Package demand is the front's side of waking a node group (spec 011 stage 2): while it holds a client of resource
// groups with no placeable node, the front keeps a Lease per group - created, renewed every 10 s, left to expire - and
// the operator starts a node of an auto group it names, saying so on the Lease. The front holds the client while the
// operator says a node is starting, and answers "no node" as before when nobody does.
package demand

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/hugr-lab/hugr-platform/internal/build"
	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

// AnswerWait is how long the front holds a client before the operator's answer must be on a group's Lease: the
// operator looks at every group every 5 s.
const AnswerWait = 15 * time.Second

// DefaultMax is how long a client is held for a starting node when the front says nothing else.
const DefaultMax = 5 * time.Minute

// Demand keeps the front's demand Leases in one namespace.
type Demand struct {
	Client    kubernetes.Interface
	Namespace string
	// Holder is the front's pod: the Leases' holder.
	Holder string
	// Max is the longest a client is held for a starting node (its own timeout usually ends it first); 0 is
	// DefaultMax.
	Max time.Duration
	// Answer is how long the operator has to say a node starts (0: AnswerWait); Poll how often the front looks
	// (0: 2 s). Tests shorten them.
	Answer, Poll time.Duration
	Now          func() time.Time

	mu   sync.Mutex
	held map[string]*hold // by resource group
}

type hold struct {
	count int
	stop  context.CancelFunc
}

func (d *Demand) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Hold says the front holds a client of these groups: each group's Lease is written now and renewed while anybody
// holds one. release ends this holder's part.
func (d *Demand) Hold(groups []string) (release func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.held == nil {
		d.held = map[string]*hold{}
	}
	for _, g := range groups {
		h := d.held[g]
		if h == nil {
			ctx, stop := context.WithCancel(context.Background())
			h = &hold{stop: stop}
			d.held[g] = h
			go d.renew(ctx, g)
		}
		h.count++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			for _, g := range groups {
				if h := d.held[g]; h != nil {
					if h.count--; h.count <= 0 {
						h.stop()
						delete(d.held, g)
					}
				}
			}
		})
	}
}

// renew the group's Lease every DemandRenew until stopped; the Lease then expires by itself (another front replica may
// hold a client of the group still).
func (d *Demand) renew(ctx context.Context, group string) {
	ticker := time.NewTicker(nodeproto.DemandRenew)
	defer ticker.Stop()
	for {
		d.write(ctx, group)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// write the group's Lease: created, or its holder and renew time patched (the operator's answer stays).
func (d *Demand) write(ctx context.Context, group string) {
	leases := d.Client.CoordinationV1().Leases(d.Namespace)
	name := nodeproto.DemandLeaseName(group)
	now := metav1.NewMicroTime(d.now())
	seconds := int32(nodeproto.DemandSeconds)
	holder := d.Holder
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"holderIdentity": holder, "renewTime": now,
		"leaseDurationSeconds": seconds}})
	_, err := leases.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "hugr-front"})
	if !apierrors.IsNotFound(err) {
		return // written, or the API's error: the next renew tries again
	}
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: d.Namespace,
			Labels:      map[string]string{nodeproto.LabelDemand: "true", build.LabelManagedBy: "hugr-front"},
			Annotations: map[string]string{nodeproto.AnnotationDemandGroup: group}},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &seconds, RenewTime: &now,
			AcquireTime: &now},
	}
	_, _ = leases.Create(ctx, lease, metav1.CreateOptions{FieldManager: "hugr-front"})
}

// Starting says whether the operator answered that a node starts for one of the groups.
func (d *Demand) Starting(ctx context.Context, groups []string) bool {
	leases := d.Client.CoordinationV1().Leases(d.Namespace)
	for _, g := range groups {
		lease, err := leases.Get(ctx, nodeproto.DemandLeaseName(g), metav1.GetOptions{})
		if err == nil && lease.Annotations[nodeproto.AnnotationStarting] != "" {
			return true
		}
	}
	return false
}

// Wait for a node `pick` chooses: noNodeWait as always, and - when the groups are ones a node named (a verified
// principal's) and none has a placeable node - for as long as the operator says one starts, up to Max or the client's
// end. A nil Demand waits noNodeWait only.
func (d *Demand) Wait(ctx context.Context, f *fleet.Fleet, noNodeWait time.Duration, groups []string,
	pick func(*fleet.View) *fleet.Node) *fleet.Node {
	if d == nil || len(groups) == 0 {
		return f.Wait(ctx, noNodeWait, pick)
	}
	view, _ := f.View()
	if n := pick(view); n != nil {
		return n
	}
	release := d.Hold(groups)
	defer release()
	start := d.now()
	if n := f.Wait(ctx, noNodeWait, pick); n != nil || ctx.Err() != nil {
		return n
	}
	limit, answer, poll := or(d.Max, DefaultMax), or(d.Answer, AnswerWait), or(d.Poll, 2*time.Second)
	for {
		elapsed := d.now().Sub(start)
		starting := d.Starting(ctx, groups)
		if !starting && elapsed >= answer || elapsed >= limit {
			return nil // nobody starts a node of these groups: the client hears "no node", as before
		}
		if n := f.Wait(ctx, poll, pick); n != nil || ctx.Err() != nil {
			return n
		}
	}
}

func or(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}
