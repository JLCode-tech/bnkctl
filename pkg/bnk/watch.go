package bnk

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// endpointSliceGVR is the discovery.k8s.io/v1 EndpointSlice resource.
var endpointSliceGVR = schema.GroupVersionResource{
	Group:    "discovery.k8s.io",
	Version:  "v1",
	Resource: "endpointslices",
}

// serviceNameLabel is the well-known EndpointSlice label naming the parent
// Service (discoveryv1.LabelServiceName).
const serviceNameLabel = "kubernetes.io/service-name"

// routeKey identifies one HTTPRoute.
type routeKey struct {
	ns   string
	name string
}

// WatchOptions configures WatchHTTPRoutes.
type WatchOptions struct {
	// ResyncOptions selects the HTTPRoutes to guard (same selectors as a
	// one-shot resync). DryRun propagates to the triggered resyncs.
	ResyncOptions
	// Debounce is the batch window after the first EndpointSlice event
	// before the queued resyncs fire. A pod replacement emits several
	// slice updates back-to-back; one toggle per route per burst is
	// enough. Defaults to 2s.
	Debounce time.Duration
	// IndexRefresh bounds how stale the service→route index may get when
	// HTTPRoutes are created/deleted while watching. Each refresh
	// re-resolves the target routes and re-establishes the watch.
	// Defaults to 5m.
	IndexRefresh time.Duration
}

// WatchHTTPRoutes runs the operator-side mitigation for the cne-controller
// EndpointSlice gap (docs/upstream-issues/) as a daemon: it watches
// EndpointSlices whose parent Service is referenced by a targeted
// HTTPRoute's backendRefs and, on any change, triggers the same
// weight-toggle resync as the one-shot command. This converts "VIP serves
// HTTP 500 until an operator notices" into "self-heals within
// Debounce + ~1s" until the upstream fix lands.
//
// Blocks until ctx is done (returns ctx.Err()) or the initial target
// resolution fails. Watch interruptions are re-established with
// exponential backoff, re-resolving the route index each time.
func WatchHTTPRoutes(ctx context.Context, dyn dynamic.Interface, opts WatchOptions) error {
	if opts.Debounce <= 0 {
		opts.Debounce = 2 * time.Second
	}
	if opts.IndexRefresh <= 0 {
		opts.IndexRefresh = 5 * time.Minute
	}

	backoff := time.Second
	first := true
	for {
		index, err := buildServiceIndex(ctx, dyn, opts.ResyncOptions)
		if err != nil {
			if first {
				// Can't even resolve targets once — surface immediately
				// rather than retrying a doomed selector.
				return fmt.Errorf("watch: resolving targets: %w", err)
			}
			fmt.Fprintf(Stderr, "[watch] index refresh failed: %v — retrying in %s\n", err, backoff)
		} else {
			if first {
				fmt.Fprintf(Stderr, "[watch] guarding %d backend service(s); debounce %s\n", len(index), opts.Debounce)
			}
			first = false
			err = watchOnce(ctx, dyn, opts, index)
			if err == nil {
				// Periodic index refresh — loop around immediately.
				backoff = time.Second
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintf(Stderr, "[watch] watch interrupted: %v — re-establishing in %s\n", err, backoff)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// watchNamespace returns the namespace scope for the EndpointSlice watch:
// all namespaces for --gateway-class targeting, the route namespace
// otherwise. Cross-namespace backendRefs outside this scope (ReferenceGrant
// setups) are indexed but their slice events are only seen in
// gateway-class mode; the periodic resync-on-refresh does not cover them —
// documented limitation until someone needs it.
func watchNamespace(opts ResyncOptions) string {
	if opts.GatewayClass != "" {
		return ""
	}
	return opts.Namespace
}

// watchOnce consumes one established watch until the index-refresh
// interval elapses (returns nil), the context ends (ctx.Err()), or the
// stream breaks (non-nil error).
func watchOnce(ctx context.Context, dyn dynamic.Interface, opts WatchOptions, index map[string][]routeKey) error {
	w, err := dyn.Resource(endpointSliceGVR).Namespace(watchNamespace(opts.ResyncOptions)).Watch(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("establishing EndpointSlice watch: %w", err)
	}
	defer w.Stop()

	refresh := time.NewTimer(opts.IndexRefresh)
	defer refresh.Stop()

	pending := map[routeKey]bool{}
	// debounceC is nil (blocks forever in select) until the first event
	// of a burst arms it. Fixed batch window: later events in the same
	// burst do NOT push the deadline out, so a steady event stream can't
	// starve the resync.
	var debounceC <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-refresh.C:
			return nil

		case ev, ok := <-w.ResultChan():
			if !ok {
				return fmt.Errorf("EndpointSlice watch channel closed")
			}
			obj, ok := ev.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			svc := obj.GetLabels()[serviceNameLabel]
			if svc == "" {
				continue
			}
			for _, rk := range index[obj.GetNamespace()+"/"+svc] {
				if !pending[rk] {
					pending[rk] = true
					fmt.Fprintf(Stderr, "[watch] EndpointSlice %s/%s (service %s) %s → queueing resync of HTTPRoute %s/%s\n",
						obj.GetNamespace(), obj.GetName(), svc, ev.Type, rk.ns, rk.name)
				}
			}
			if len(pending) > 0 && debounceC == nil {
				debounceC = time.After(opts.Debounce)
			}

		case <-debounceC:
			for rk := range pending {
				if _, rerr := ResyncHTTPRoutes(ctx, dyn, ResyncOptions{
					Namespace: rk.ns,
					Name:      rk.name,
					DryRun:    opts.DryRun,
				}); rerr != nil {
					fmt.Fprintf(Stderr, "[watch] resync %s/%s failed: %v\n", rk.ns, rk.name, rerr)
				}
			}
			pending = map[routeKey]bool{}
			debounceC = nil
		}
	}
}

// buildServiceIndex maps "namespace/serviceName" → HTTPRoutes whose
// backendRefs reference that Service, across the routes selected by opts.
// Kind defaults to Service when unset; non-Service backendRefs are
// ignored. A backendRef's namespace defaults to the route's own.
func buildServiceIndex(ctx context.Context, dyn dynamic.Interface, opts ResyncOptions) (map[string][]routeKey, error) {
	targets, err := resolveTargets(ctx, dyn, opts)
	if err != nil {
		return nil, err
	}
	index := map[string][]routeKey{}
	for _, route := range targets {
		rk := routeKey{ns: route.GetNamespace(), name: route.GetName()}
		rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
		for _, rRaw := range rules {
			r, ok := rRaw.(map[string]interface{})
			if !ok {
				continue
			}
			refs, _ := r["backendRefs"].([]interface{})
			for _, bRaw := range refs {
				b, ok2 := bRaw.(map[string]interface{})
				if !ok2 {
					continue
				}
				if kind, _ := b["kind"].(string); kind != "" && kind != "Service" {
					continue
				}
				name, _ := b["name"].(string)
				if name == "" {
					continue
				}
				ns, _ := b["namespace"].(string)
				if ns == "" {
					ns = route.GetNamespace()
				}
				key := ns + "/" + name
				if !containsRoute(index[key], rk) {
					index[key] = append(index[key], rk)
				}
			}
		}
	}
	return index, nil
}

func containsRoute(routes []routeKey, rk routeKey) bool {
	for _, r := range routes {
		if r == rk {
			return true
		}
	}
	return false
}
