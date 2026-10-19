// Package watch keeps the front's fleet current (spec 002): the EndpointSlices of the node groups' headless Services
// and the nodes' Leases of the platform's namespace, read by two informers, made into a fleet.View at every change.
// The front is ready once both have synced, and not while a watch has been lost for 3 heartbeats.
package watch

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/hugr-lab/hugr-platform/internal/build"
	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

// ServicePrefix of a node group's headless Service (spec 001: hugr-node-<group>).
const ServicePrefix = build.NodeName + "-"

// Watcher of one namespace.
type Watcher struct {
	fleet     *fleet.Fleet
	heartbeat time.Duration
	now       func() time.Time

	factories      []informers.SharedInformerFactory
	slices, leases cache.SharedIndexInformer
	building       sync.Mutex // one view built and set at a time: an older one never replaces a newer
	mu             sync.Mutex
	lost           map[string]lost // by informer
}

type lost struct {
	since   time.Time
	version string // the informer's resource version at the loss: a later one is a watch that came back
}

// New watcher of the namespace, keeping `f` current. A watch lost for three heartbeats makes the front not ready.
func New(client kubernetes.Interface, namespace string, f *fleet.Fleet, heartbeat time.Duration) *Watcher {
	w := &Watcher{fleet: f, heartbeat: heartbeat, now: time.Now, lost: map[string]lost{}}
	factory := informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithNamespace(namespace))
	w.slices = factory.Discovery().V1().EndpointSlices().Informer()
	// the Leases the operator keeps for its nodes (spec 001), not every Lease of the namespace
	managed := labels.SelectorFromSet(labels.Set{build.LabelManagedBy: build.ManagedBy}).String()
	leaseFactory := informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = managed }))
	w.leases = leaseFactory.Coordination().V1().Leases().Informer()
	w.factories = []informers.SharedInformerFactory{factory, leaseFactory}
	for name, informer := range map[string]cache.SharedIndexInformer{"endpointslices": w.slices, "leases": w.leases} {
		name, informer := name, informer
		_ = informer.SetWatchErrorHandler(func(_ *cache.Reflector, _ error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			if _, ok := w.lost[name]; !ok {
				w.lost[name] = lost{since: w.now(), version: informer.LastSyncResourceVersion()}
			}
		})
		_, _ = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    func(any) { w.changed(name) },
			UpdateFunc: func(any, any) { w.changed(name) },
			DeleteFunc: func(any) { w.changed(name) },
		})
	}
	return w
}

// Run the informers until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	for _, f := range w.factories {
		f.Start(ctx.Done())
	}
	<-ctx.Done()
	for _, f := range w.factories {
		f.Shutdown()
	}
}

func (w *Watcher) changed(name string) {
	w.mu.Lock()
	delete(w.lost, name)
	w.mu.Unlock()
	w.building.Lock()
	defer w.building.Unlock()
	w.fleet.Set(View(w.slices.GetStore().List(), w.leases.GetStore().List()))
}

// Ready once both informers have synced, and while neither has lost its watch for 3 heartbeats.
func (w *Watcher) Ready() error {
	if !w.slices.HasSynced() || !w.leases.HasSynced() {
		return errors.New("the fleet is not read yet")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for name, l := range w.lost {
		informer := w.slices
		if name == "leases" {
			informer = w.leases
		}
		if informer.LastSyncResourceVersion() != l.version {
			delete(w.lost, name) // listed or watched again since
			continue
		}
		if w.now().Sub(l.since) > 3*w.heartbeat {
			return errors.New("the watch of " + name + " is lost")
		}
	}
	return nil
}

// View of what the informers hold: each endpoint of a node group's Service a node, its Lease the one named like it.
func View(slices, leases []any) *fleet.View {
	byName := map[string]*coordinationv1.Lease{}
	for _, o := range leases {
		if l, ok := o.(*coordinationv1.Lease); ok {
			byName[l.Name] = l
		}
	}
	// one reading whatever order the store lists them in: IPv4 slices before IPv6 (a dual-stack pod is reached by its
	// IPv4 address), then by name - a pod in two slices keeps its first, and its connections are not flipped
	var ordered []*discoveryv1.EndpointSlice
	for _, o := range slices {
		if s, ok := o.(*discoveryv1.EndpointSlice); ok && s.AddressType != discoveryv1.AddressTypeFQDN {
			ordered = append(ordered, s)
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if (a.AddressType == discoveryv1.AddressTypeIPv4) != (b.AddressType == discoveryv1.AddressTypeIPv4) {
			return a.AddressType == discoveryv1.AddressTypeIPv4
		}
		return a.Name < b.Name
	})
	var nodes []fleet.Node
	for _, s := range ordered {
		service := s.Labels[discoveryv1.LabelServiceName]
		if !strings.HasPrefix(service, ServicePrefix) {
			continue
		}
		var flight, quack int32
		for _, p := range s.Ports {
			if p.Name == nil || p.Port == nil {
				continue
			}
			switch *p.Name {
			case "flight":
				flight = *p.Port
			case "quack":
				quack = *p.Port
			}
		}
		for _, e := range s.Endpoints {
			if e.TargetRef == nil || e.TargetRef.Kind != "Pod" || len(e.Addresses) == 0 {
				continue
			}
			// a condition not set reads as true (the API's own rule for an unknown state)
			ready := e.Conditions.Ready == nil || *e.Conditions.Ready
			serving := ready
			if e.Conditions.Serving != nil {
				serving = *e.Conditions.Serving
			}
			n := fleet.Node{Label: e.TargetRef.Name, Service: service, Address: e.Addresses[0], FlightPort: flight,
				QuackPort: quack, Ready: ready, Serving: serving}
			if l := byName[e.TargetRef.Name]; l != nil && l.Spec.RenewTime != nil && l.Spec.LeaseDurationSeconds != nil {
				n.Lease = &fleet.Lease{
					Until:   l.Spec.RenewTime.Add(time.Duration(*l.Spec.LeaseDurationSeconds) * time.Second),
					Summary: nodeproto.ParseSummary(l.Annotations),
				}
			}
			nodes = append(nodes, n)
		}
	}
	return fleet.NewView(nodes)
}
