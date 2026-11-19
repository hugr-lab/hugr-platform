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

// AnswerWait is how long the front holds a client, from its demand's first write, before the operator's answer must
// be on a group's Lease: the operator looks at every group every 5 s.
const AnswerWait = 15 * time.Second

// DefaultMax is how long a client is held for a starting node when the front says nothing else.
const DefaultMax = 5 * time.Minute

// Demand keeps the front's demand Leases in one namespace.
type Demand struct {
	Client    kubernetes.Interface
	Namespace string
	// Holder is the front's pod: the Leases' holder.
	Holder string
	// Max is the longest a client is held for a starting node, across every re-seat of its connection (its own
	// timeout usually ends it first); 0 is DefaultMax.
	Max time.Duration
	// Answer is how long the operator has to say a node starts (0: AnswerWait); Poll how often the front looks
	// (0: 2 s): one read of a group's Lease per Poll, whatever the clients waiting for it. Tests shorten them.
	Answer, Poll time.Duration
	Now          func() time.Time

	mu   sync.Mutex
	held map[string]*hold // by resource group
}

// hold is one group's demand while the front holds a client of it: who holds, and what its Lease says
type hold struct {
	count    int
	stop     context.CancelFunc
	written  time.Time // the first write of this demand that the API took (zero: none yet)
	starting bool      // the operator's answer, as the last read saw it
}

func (d *Demand) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Limit is the longest a connection is held for a starting node; a nil Demand holds none.
func (d *Demand) Limit() time.Duration {
	if d == nil {
		return 0
	}
	return or(d.Max, DefaultMax)
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
			go d.renew(ctx, g, h)
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

// renew the group's Lease every DemandRenew and read the operator's answer every Poll until stopped; the Lease then
// expires by itself (another front replica may hold a client of the group still).
func (d *Demand) renew(ctx context.Context, group string, h *hold) {
	poll := or(d.Poll, 2*time.Second)
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	var wrote time.Time
	for first := true; ; first = false {
		var lease *coordinationv1.Lease
		if first || d.now().Sub(wrote) >= nodeproto.DemandRenew {
			if lease = d.write(ctx, group, first); lease != nil {
				wrote = d.now()
			}
		} else {
			lease, _ = d.Client.CoordinationV1().Leases(d.Namespace).Get(ctx, nodeproto.DemandLeaseName(group),
				metav1.GetOptions{})
		}
		d.mu.Lock()
		if lease != nil {
			if h.written.IsZero() && !wrote.IsZero() {
				h.written = wrote
			}
			h.starting = lease.Annotations[nodeproto.AnnotationStarting] != ""
		}
		d.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// write the group's Lease: created, or its holder and renew time patched - the operator's answer stays while the
// demand lasts. A new demand (first) on a Lease that expired drops an answer left from before: it was for another
// demand, and the group may be manual by now, or gone. The Lease as the API answered, nil when it did not take it.
func (d *Demand) write(ctx context.Context, group string, first bool) *coordinationv1.Lease {
	leases := d.Client.CoordinationV1().Leases(d.Namespace)
	name := nodeproto.DemandLeaseName(group)
	now := metav1.NewMicroTime(d.now())
	seconds := int32(nodeproto.DemandSeconds)
	holder := d.Holder
	body := map[string]any{"spec": map[string]any{"holderIdentity": holder, "renewTime": now,
		"leaseDurationSeconds": seconds}}
	if first {
		if old, err := leases.Get(ctx, name, metav1.GetOptions{}); err == nil && !nodeproto.DemandLive(old, d.now()) {
			body["metadata"] = map[string]any{"annotations": map[string]any{nodeproto.AnnotationStarting: nil}}
		}
	}
	patch, _ := json.Marshal(body)
	lease, err := leases.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "hugr-front"})
	if err == nil {
		return lease
	}
	if !apierrors.IsNotFound(err) {
		return nil // the API's error: the next renew tries again
	}
	lease, err = leases.Create(ctx, &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: d.Namespace,
			Labels:      map[string]string{nodeproto.LabelDemand: "true", build.LabelManagedBy: "hugr-front"},
			Annotations: map[string]string{nodeproto.AnnotationDemandGroup: group}},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &seconds, RenewTime: &now,
			AcquireTime: &now},
	}, metav1.CreateOptions{FieldManager: "hugr-front"})
	if err != nil {
		return nil // another replica made it first, or the API refused: the next renew patches it
	}
	return lease
}

// state of the groups' demand: whether the operator says a node starts for one of them, and when the first of them
// was written (zero: none yet).
func (d *Demand) state(groups []string) (starting bool, written time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, g := range groups {
		h := d.held[g]
		if h == nil {
			continue
		}
		starting = starting || h.starting
		if !h.written.IsZero() && (written.IsZero() || h.written.Before(written)) {
			written = h.written
		}
	}
	return starting, written
}

// Wait for a node `pick` chooses: noNodeWait as always, and - when the groups are ones a node named (a verified
// principal's) and none has a placeable node - for as long as the operator says one starts, until `until` (one
// deadline for a connection's every re-seat: its first wait's start plus Limit) or ctx's end. A nil Demand waits
// noNodeWait only.
func (d *Demand) Wait(ctx context.Context, f *fleet.Fleet, noNodeWait time.Duration, groups []string, until time.Time,
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
	answer, poll := or(d.Answer, AnswerWait), or(d.Poll, 2*time.Second)
	for {
		starting, written := d.state(groups)
		// the operator's time to answer counts from the demand's first write: a write the API did not take yet
		// leaves it its renew's time more
		since := written
		if since.IsZero() {
			since = start.Add(nodeproto.DemandRenew)
		}
		if !starting && d.now().Sub(since) >= answer || !d.now().Before(until) {
			return nil // nobody starts a node of these groups (or not in time): the client hears "no node", as before
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
