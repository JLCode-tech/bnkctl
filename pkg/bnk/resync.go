// Package bnk provides runtime helpers for BNK (F5 BIG-IP Next for Kubernetes)
// cluster management. Functions here are callable both from the awsbnkctl CLI
// and programmatically by scenario runners (e.g. the future test traffic
// subcommand) without spawning a subprocess.
package bnk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// Stderr is the writer used for human-readable progress output. Tests may
// replace this with a bytes.Buffer to capture output without racing on
// os.Stderr.
var Stderr io.Writer = os.Stderr

// GVRs for Gateway API resources.
var (
	httpRouteGVR = schema.GroupVersionResource{
		Group:    "gateway.networking.k8s.io",
		Version:  "v1",
		Resource: "httproutes",
	}
	gatewayGVR = schema.GroupVersionResource{
		Group:    "gateway.networking.k8s.io",
		Version:  "v1",
		Resource: "gateways",
	}
)

// ResyncOptions controls which HTTPRoute(s) ResyncHTTPRoutes targets.
// Exactly one of Name+Namespace, AllInNamespace, or GatewayClass must be set.
type ResyncOptions struct {
	// Namespace is the namespace to scope the operation to (used with Name
	// and AllInNamespace).
	Namespace string
	// Name is the specific HTTPRoute to resync (requires Namespace).
	Name string
	// AllInNamespace, when true, resyncs every HTTPRoute in Namespace.
	AllInNamespace bool
	// GatewayClass resyncs every HTTPRoute (across all namespaces) whose
	// parent Gateway has gatewayClassName == GatewayClass.
	GatewayClass string
	// DryRun, when true, resolves targets and logs what would be patched
	// without making any API writes. Returns 0 errors on success.
	DryRun bool
}

// ResyncedRoute records the outcome for a single HTTPRoute.
type ResyncedRoute struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Rules     int    `json:"rules"`
	Applied   bool   `json:"applied"`
}

// ResyncError records a patch failure for one HTTPRoute.
type ResyncError struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Err       string `json:"error"`
}

// ResyncResult aggregates the outcome of a ResyncHTTPRoutes call.
type ResyncResult struct {
	Schema    string          `json:"schema"`
	Timestamp time.Time       `json:"timestamp"`
	Resynced  []ResyncedRoute `json:"resynced"`
	Errors    []ResyncError   `json:"errors"`
}

const resyncSchema = "awsbnkctl.resync.v1"

// ReconcileWait is the delay between the forward and restore patches.
// The cne-controller typically reconciles within milliseconds after the
// first patch; 1s is generous headroom in production. Tests may lower this.
var ReconcileWait = 1 * time.Second

// ResyncAll forces the F5 cne-controller to re-resolve stale TMM pool
// members for all targeted HTTPRoute(s) according to opts.
func ResyncAll(ctx context.Context, dyn dynamic.Interface, opts ResyncOptions) (ResyncResult, error) {
	if opts.Name == "" && !opts.AllInNamespace && opts.GatewayClass == "" {
		opts.AllInNamespace = true
	}
	return ResyncHTTPRoutes(ctx, dyn, opts)
}

// ResyncHTTPRoutes forces the F5 cne-controller to re-resolve stale TMM pool
// members for the targeted HTTPRoute(s).
//
// Mechanism: the cne-controller only reconciles pool members when the
// HTTPRoute spec changes. For each targeted route, this function patches
// spec.rules[i].backendRefs[j].weight by +1, waits 1s for the controller to
// process, then patches back to the original value. This triggers
// "GatewayReconciler: handling http route update" in the controller log and
// causes fresh pool-member IPs to be pushed to TMM.
//
// Live-validated on syd-tracer 2026-05-23 (see project_pool_member_sync_root_cause.md).
func ResyncHTTPRoutes(ctx context.Context, dyn dynamic.Interface, opts ResyncOptions) (ResyncResult, error) {
	result := ResyncResult{
		Schema:    resyncSchema,
		Timestamp: time.Now().UTC(),
	}

	targets, err := resolveTargets(ctx, dyn, opts)
	if err != nil {
		return result, err
	}

	for _, route := range targets {
		ns := route.GetNamespace()
		name := route.GetName()
		rules := countBackendRefs(route)

		fmt.Fprintf(Stderr, "[resync] target: HTTPRoute %s/%s (%d backend ref(s))\n", ns, name, rules)

		if opts.DryRun {
			result.Resynced = append(result.Resynced, ResyncedRoute{
				Namespace: ns,
				Name:      name,
				Rules:     rules,
				Applied:   false,
			})
			continue
		}

		if err := toggleWeights(ctx, dyn, ns, name, route); err != nil {
			fmt.Fprintf(Stderr, "[resync] ✗ %s/%s: %v\n", ns, name, err)
			result.Errors = append(result.Errors, ResyncError{
				Namespace: ns,
				Name:      name,
				Err:       err.Error(),
			})
			continue
		}

		result.Resynced = append(result.Resynced, ResyncedRoute{
			Namespace: ns,
			Name:      name,
			Rules:     rules,
			Applied:   true,
		})
	}

	resynced := len(result.Resynced)
	if opts.DryRun {
		fmt.Fprintf(Stderr, "[resync] dry-run: %d HTTPRoute(s) would be resynced\n", resynced)
	} else {
		fmt.Fprintf(Stderr, "[resync] ✓ %d HTTPRoute(s) resynced\n", resynced)
	}

	if len(result.Errors) > 0 {
		return result, fmt.Errorf("%d HTTPRoute(s) failed to resync", len(result.Errors))
	}
	return result, nil
}

// resolveTargets returns the HTTPRoute objects matching opts.
func resolveTargets(ctx context.Context, dyn dynamic.Interface, opts ResyncOptions) ([]*unstructured.Unstructured, error) {
	switch {
	case opts.Name != "":
		// Single named route.
		obj, err := dyn.Resource(httpRouteGVR).Namespace(opts.Namespace).Get(ctx, opts.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("getting HTTPRoute %s/%s: %w", opts.Namespace, opts.Name, err)
		}
		return []*unstructured.Unstructured{obj}, nil

	case opts.AllInNamespace:
		list, err := dyn.Resource(httpRouteGVR).Namespace(opts.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("listing HTTPRoutes in %s: %w", opts.Namespace, err)
		}
		routes := make([]*unstructured.Unstructured, len(list.Items))
		for i := range list.Items {
			routes[i] = &list.Items[i]
		}
		return routes, nil

	case opts.GatewayClass != "":
		return routesForGatewayClass(ctx, dyn, opts.GatewayClass)

	default:
		return nil, fmt.Errorf("resync: one of Name, AllInNamespace, or GatewayClass must be set")
	}
}

// routesForGatewayClass returns all HTTPRoutes (across all namespaces) whose
// parent Gateway has spec.gatewayClassName == gatewayClass.
func routesForGatewayClass(ctx context.Context, dyn dynamic.Interface, gatewayClass string) ([]*unstructured.Unstructured, error) {
	// Collect gateways matching gatewayClass (cluster-scoped list via "" namespace).
	gwList, err := dyn.Resource(gatewayGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing Gateways: %w", err)
	}
	// Build a set of "namespace/name" gateway keys.
	gwKeys := make(map[string]bool)
	for i := range gwList.Items {
		gw := &gwList.Items[i]
		cls, _, _ := unstructured.NestedString(gw.Object, "spec", "gatewayClassName")
		if cls == gatewayClass {
			gwKeys[gw.GetNamespace()+"/"+gw.GetName()] = true
		}
	}
	if len(gwKeys) == 0 {
		return nil, nil
	}

	// List all HTTPRoutes and filter by parent gateway.
	hrList, err := dyn.Resource(httpRouteGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing HTTPRoutes: %w", err)
	}

	var routes []*unstructured.Unstructured
	for i := range hrList.Items {
		hr := &hrList.Items[i]
		parents, _, _ := unstructured.NestedSlice(hr.Object, "spec", "parentRefs")
		for _, pRaw := range parents {
			p, ok := pRaw.(map[string]interface{})
			if !ok {
				continue
			}
			// parentRef.namespace defaults to the HTTPRoute's own namespace.
			ns, _, _ := unstructured.NestedString(p, "namespace")
			if ns == "" {
				ns = hr.GetNamespace()
			}
			name, _, _ := unstructured.NestedString(p, "name")
			if gwKeys[ns+"/"+name] {
				routes = append(routes, hr)
				break
			}
		}
	}
	return routes, nil
}

// countBackendRefs returns the total number of backendRef entries across all
// rules in the HTTPRoute.
func countBackendRefs(route *unstructured.Unstructured) int {
	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	count := 0
	for _, rRaw := range rules {
		r, ok := rRaw.(map[string]interface{})
		if !ok {
			continue
		}
		if refs, ok2 := r["backendRefs"]; ok2 {
			if refSlice, ok3 := refs.([]interface{}); ok3 {
				count += len(refSlice)
			}
		}
	}
	return count
}

type patchOp struct {
	Op   string `json:"op"`
	Path string `json:"path"`
	// Value is a pointer so "remove" ops (which must not carry a value)
	// serialize without the field.
	Value *int64 `json:"value,omitempty"`
}

type weightEntry struct {
	ruleIdx     int
	refIdx      int
	orig        int64
	fieldExists bool // false → weight was absent; use "add" instead of "replace"
}

// collectWeights scans the route's rules and returns one weightEntry per
// backendRef. The Gateway API spec defaults an absent weight to 1.
func collectWeights(route *unstructured.Unstructured) []weightEntry {
	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	var entries []weightEntry
	for ri, rRaw := range rules {
		r, ok := rRaw.(map[string]interface{})
		if !ok {
			continue
		}
		refs, _ := r["backendRefs"].([]interface{})
		for bi, bRaw := range refs {
			b, ok2 := bRaw.(map[string]interface{})
			if !ok2 {
				continue
			}
			e := weightEntry{ruleIdx: ri, refIdx: bi, orig: 1}
			if raw, exists := b["weight"]; exists {
				e.fieldExists = true
				switch v := raw.(type) {
				case int64:
					e.orig = v
				case float64:
					e.orig = int64(v)
				case int:
					e.orig = int64(v)
				}
			}
			entries = append(entries, e)
		}
	}
	return entries
}

func weightPath(e weightEntry) string {
	return fmt.Sprintf("/spec/rules/%d/backendRefs/%d/weight", e.ruleIdx, e.refIdx)
}

func int64Ptr(v int64) *int64 { return &v }

// buildForwardPatches sets every backendRef weight to orig+1. Refs that
// already carry a weight are guarded by a "test" op asserting the value we
// read is still current, so a spec edit that raced our Get fails the patch
// loudly (RFC 6902 test failure) instead of being silently overwritten.
// Absent weights use "add" — there is no value to test.
func buildForwardPatches(entries []weightEntry) []patchOp {
	var ops []patchOp
	for _, e := range entries {
		p := weightPath(e)
		if e.fieldExists {
			ops = append(ops,
				patchOp{Op: "test", Path: p, Value: int64Ptr(e.orig)},
				patchOp{Op: "replace", Path: p, Value: int64Ptr(e.orig + 1)},
			)
		} else {
			ops = append(ops, patchOp{Op: "add", Path: p, Value: int64Ptr(e.orig + 1)})
		}
	}
	return ops
}

// buildRestorePatches returns the route to its original state: refs that had
// an explicit weight get it back via "replace"; refs that had none get the
// field removed again (leaving `weight: 1` behind would be semantically
// identical — the Gateway API default — but not spec-identical). Every entry
// is guarded by a "test" op on the toggled value so a concurrent edit during
// ReconcileWait aborts the restore instead of clobbering it.
func buildRestorePatches(entries []weightEntry) []patchOp {
	var ops []patchOp
	for _, e := range entries {
		p := weightPath(e)
		ops = append(ops, patchOp{Op: "test", Path: p, Value: int64Ptr(e.orig + 1)})
		if e.fieldExists {
			ops = append(ops, patchOp{Op: "replace", Path: p, Value: int64Ptr(e.orig)})
		} else {
			ops = append(ops, patchOp{Op: "remove", Path: p})
		}
	}
	return ops
}

// toggleWeights patches all backendRef weights in the route: first +1, waits
// 1s, then back to original. Uses JSON patch to avoid any YAML/struct
// unmarshalling of a CRD we don't own.
func toggleWeights(ctx context.Context, dyn dynamic.Interface, ns, name string, route *unstructured.Unstructured) error {
	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	if len(rules) == 0 {
		// Degenerate route with no rules — nothing to patch; treat as success.
		fmt.Fprintf(Stderr, "[resync]   no rules in %s/%s — skipping patch\n", ns, name)
		return nil
	}

	entries := collectWeights(route)
	if len(entries) == 0 {
		// Rules present but no backendRefs — degenerate; skip.
		fmt.Fprintf(Stderr, "[resync]   no backendRefs in %s/%s — skipping patch\n", ns, name)
		return nil
	}

	forward := buildForwardPatches(entries)
	restore := buildRestorePatches(entries)

	// Apply forward patch.
	rv, err := applyJSONPatch(ctx, dyn, ns, name, forward)
	if err != nil {
		return fmt.Errorf("forward patch: %w", err)
	}
	fmt.Fprintf(Stderr, "[resync]   patched weight %d → %d (resourceVersion %s → %s)\n",
		entries[0].orig, entries[0].orig+1,
		route.GetResourceVersion(), rv)

	// Wait for the controller to reconcile.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(ReconcileWait):
	}

	// Apply restore patch.
	rv2, err := applyJSONPatch(ctx, dyn, ns, name, restore)
	if err != nil {
		return fmt.Errorf("restore patch: %w", err)
	}
	fmt.Fprintf(Stderr, "[resync]   patched weight %d → %d (resourceVersion %s → %s)\n",
		entries[0].orig+1, entries[0].orig, rv, rv2)

	return nil
}

// applyJSONPatch sends a JSON Patch (RFC 6902) to the HTTPRoute and returns
// the new resourceVersion from the response.
func applyJSONPatch(ctx context.Context, dyn dynamic.Interface, ns, name string, ops interface{}) (string, error) {
	data, err := json.Marshal(ops)
	if err != nil {
		return "", err
	}
	result, err := dyn.Resource(httpRouteGVR).Namespace(ns).Patch(
		ctx, name, types.JSONPatchType, data, metav1.PatchOptions{},
	)
	if err != nil {
		return "", err
	}
	return result.GetResourceVersion(), nil
}
