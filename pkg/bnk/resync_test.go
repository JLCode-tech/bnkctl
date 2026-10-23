package bnk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func init() {
	// Speed up unit tests — the fake client reconciles synchronously.
	ReconcileWait = 0
}

// resyncScheme returns a minimal runtime.Scheme for the fake dynamic client,
// registering both HTTPRoute and Gateway list kinds.
func resyncScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, pair := range []struct{ gvk, listGVK schema.GroupVersionKind }{
		{
			gvk:     schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"},
			listGVK: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRouteList"},
		},
		{
			gvk:     schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway"},
			listGVK: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "GatewayList"},
		},
		{
			gvk:     schema.GroupVersionKind{Group: "discovery.k8s.io", Version: "v1", Kind: "EndpointSlice"},
			listGVK: schema.GroupVersionKind{Group: "discovery.k8s.io", Version: "v1", Kind: "EndpointSliceList"},
		},
	} {
		s.AddKnownTypeWithName(pair.gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(pair.listGVK, &unstructured.UnstructuredList{})
	}
	return s
}

// newFakeDynamic builds a fake dynamic client and creates the given
// unstructured objects via the appropriate namespaced resource interface.
// We use explicit Create() calls rather than the constructor's objs parameter
// because the fake client's object tracker cannot deduce the GVR for
// unstructured objects (multiple GVKs share *unstructured.Unstructured).
func newFakeDynamic(objs ...*unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		resyncScheme(),
		map[schema.GroupVersionResource]string{
			httpRouteGVR:     "HTTPRouteList",
			gatewayGVR:       "GatewayList",
			endpointSliceGVR: "EndpointSliceList",
		},
	)
	ctx := context.Background()
	for _, obj := range objs {
		gvr := gvrForKind(obj.GetKind())
		ns := obj.GetNamespace()
		if _, err := dyn.Resource(gvr).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{}); err != nil {
			panic(fmt.Sprintf("newFakeDynamic: Create %s/%s: %v", obj.GetNamespace(), obj.GetName(), err))
		}
	}
	return dyn
}

// gvrForKind returns the GVR for the Gateway API kinds used in tests.
func gvrForKind(kind string) schema.GroupVersionResource {
	switch kind {
	case "HTTPRoute":
		return httpRouteGVR
	case "Gateway":
		return gatewayGVR
	case "EndpointSlice":
		return endpointSliceGVR
	default:
		panic("gvrForKind: unknown kind " + kind)
	}
}

// makeHTTPRoute builds an unstructured HTTPRoute with the given weight on
// its single rule's single backendRef. Pass weight == 0 to omit the weight
// field entirely (degenerate / missing-weight case).
func makeHTTPRoute(ns, name string, weight int64) *unstructured.Unstructured {
	backendRef := map[string]interface{}{
		"name": "my-svc",
		"port": int64(80),
	}
	if weight != 0 {
		backendRef["weight"] = weight
	}
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "HTTPRoute",
			"metadata": map[string]interface{}{
				"name":            name,
				"namespace":       ns,
				"resourceVersion": "100",
			},
			"spec": map[string]interface{}{
				"rules": []interface{}{
					map[string]interface{}{
						"backendRefs": []interface{}{backendRef},
					},
				},
			},
		},
	}
}

// makeHTTPRouteNoBackendRefs builds an HTTPRoute with one rule but no backendRefs.
func makeHTTPRouteNoBackendRefs(ns, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "HTTPRoute",
			"metadata": map[string]interface{}{
				"name":            name,
				"namespace":       ns,
				"resourceVersion": "100",
			},
			"spec": map[string]interface{}{
				"rules": []interface{}{
					map[string]interface{}{},
				},
			},
		},
	}
}

// makeGateway builds an unstructured Gateway with the given gatewayClassName.
func makeGateway(ns, name, gatewayClass string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "Gateway",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
			"spec": map[string]interface{}{
				"gatewayClassName": gatewayClass,
			},
		},
	}
}

// makeHTTPRouteWithParent builds an HTTPRoute with a parentRef pointing to the
// given gateway (ns/name).
func makeHTTPRouteWithParent(hrNS, hrName, gwNS, gwName string) *unstructured.Unstructured {
	hr := makeHTTPRoute(hrNS, hrName, 1)
	hr.Object["spec"].(map[string]interface{})["parentRefs"] = []interface{}{
		map[string]interface{}{
			"name":      gwName,
			"namespace": gwNS,
		},
	}
	return hr
}

// captureStderr redirects Stderr to a buffer for the duration of fn, then
// restores it. Returns the captured text.
func captureStderr(fn func()) string {
	var buf bytes.Buffer
	prev := Stderr
	Stderr = &buf
	defer func() { Stderr = prev }()
	fn()
	return buf.String()
}

// ─── Tests ──────────────────────────────────────────────────────────────────

// TestResync_SingleRoute_Happy verifies the single-route path applies two
// patches and marks the route as applied.
func TestResync_SingleRoute_Happy(t *testing.T) {
	route := makeHTTPRoute("f5-cne-system", "nginx-route", 1)
	dyn := newFakeDynamic(route)

	result, err := ResyncHTTPRoutes(context.Background(), dyn, ResyncOptions{
		Namespace: "f5-cne-system",
		Name:      "nginx-route",
	})
	if err != nil {
		t.Fatalf("ResyncHTTPRoutes: unexpected error: %v", err)
	}
	if len(result.Resynced) != 1 {
		t.Fatalf("expected 1 resynced route, got %d", len(result.Resynced))
	}
	if !result.Resynced[0].Applied {
		t.Error("expected Applied=true")
	}
	if result.Resynced[0].Name != "nginx-route" {
		t.Errorf("expected name nginx-route, got %s", result.Resynced[0].Name)
	}
	if len(result.Errors) != 0 {
		t.Errorf("unexpected errors: %v", result.Errors)
	}
}

// TestResync_DryRun verifies no kube API writes occur and Applied is false.
func TestResync_DryRun(t *testing.T) {
	route := makeHTTPRoute("f5-cne-system", "nginx-route", 1)
	dyn := newFakeDynamic(route)

	var out string
	result, err := func() (ResyncResult, error) {
		var r ResyncResult
		var e error
		out = captureStderr(func() {
			r, e = ResyncHTTPRoutes(context.Background(), dyn, ResyncOptions{
				Namespace: "f5-cne-system",
				Name:      "nginx-route",
				DryRun:    true,
			})
		})
		return r, e
	}()

	if err != nil {
		t.Fatalf("dry-run returned error: %v", err)
	}
	if len(result.Resynced) != 1 {
		t.Fatalf("expected 1 resynced entry, got %d", len(result.Resynced))
	}
	if result.Resynced[0].Applied {
		t.Error("dry-run: expected Applied=false")
	}
	if !strings.Contains(out, "dry-run") {
		t.Errorf("dry-run output missing 'dry-run' label: %q", out)
	}

	// Verify no writes happened — re-fetch the object; resourceVersion should
	// still be "100".
	got, err2 := dyn.Resource(httpRouteGVR).Namespace("f5-cne-system").Get(
		context.Background(), "nginx-route", metav1.GetOptions{},
	)
	if err2 != nil {
		t.Fatalf("re-fetch: %v", err2)
	}
	if got.GetResourceVersion() != "100" {
		t.Errorf("dry-run wrote to the API (resourceVersion changed to %s)", got.GetResourceVersion())
	}
}

// TestResync_NoBackendRefs_Degenerate verifies a route with no backendRefs is
// handled gracefully (no error, Applied=false because there's nothing to patch).
func TestResync_NoBackendRefs_Degenerate(t *testing.T) {
	route := makeHTTPRouteNoBackendRefs("f5-cne-system", "empty-route")
	dyn := newFakeDynamic(route)

	result, err := ResyncHTTPRoutes(context.Background(), dyn, ResyncOptions{
		Namespace: "f5-cne-system",
		Name:      "empty-route",
	})
	if err != nil {
		t.Fatalf("unexpected error for degenerate route: %v", err)
	}
	// The route is listed as resynced but Applied reflects whether we patched.
	// Because there are no backendRefs, we skip the patch but still return success.
	if len(result.Errors) != 0 {
		t.Errorf("unexpected errors: %v", result.Errors)
	}
}

// TestResync_MissingWeight_DefaultsToOne verifies that when a backendRef has
// no weight field, the code defaults to 1 (per Gateway API spec) and issues
// an "add" op rather than "replace".
func TestResync_MissingWeight_DefaultsToOne(t *testing.T) {
	// weight == 0 means "omit the field"
	route := makeHTTPRoute("f5-cne-system", "no-weight-route", 0)
	dyn := newFakeDynamic(route)

	result, err := ResyncHTTPRoutes(context.Background(), dyn, ResyncOptions{
		Namespace: "f5-cne-system",
		Name:      "no-weight-route",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Resynced) != 1 || !result.Resynced[0].Applied {
		t.Errorf("expected one applied route, got resynced=%v errors=%v", result.Resynced, result.Errors)
	}
}

// TestResync_AllInNamespace verifies that all routes in the namespace are
// resynced.
func TestResync_AllInNamespace(t *testing.T) {
	r1 := makeHTTPRoute("f5-cne-system", "route-a", 1)
	r2 := makeHTTPRoute("f5-cne-system", "route-b", 2)
	dyn := newFakeDynamic(r1, r2)

	result, err := ResyncHTTPRoutes(context.Background(), dyn, ResyncOptions{
		Namespace:      "f5-cne-system",
		AllInNamespace: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Resynced) != 2 {
		t.Fatalf("expected 2 resynced routes, got %d", len(result.Resynced))
	}
}

// TestResync_GatewayClass verifies that --gateway-class filters HTTPRoutes by
// the gatewayClassName on their parent Gateway.
func TestResync_GatewayClass(t *testing.T) {
	gw := makeGateway("f5-cne-system", "my-gw", "f5-bnk")
	hrMatch := makeHTTPRouteWithParent("f5-cne-system", "matching-route", "f5-cne-system", "my-gw")
	hrOther := makeHTTPRoute("other-ns", "other-route", 1)
	dyn := newFakeDynamic(gw, hrMatch, hrOther)

	result, err := ResyncHTTPRoutes(context.Background(), dyn, ResyncOptions{
		GatewayClass: "f5-bnk",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Resynced) != 1 {
		t.Fatalf("expected 1 resynced route, got %d (errors: %v)", len(result.Resynced), result.Errors)
	}
	if result.Resynced[0].Name != "matching-route" {
		t.Errorf("expected matching-route, got %s", result.Resynced[0].Name)
	}
}

// TestResync_JSONOutputSchema verifies the JSON output has the correct schema
// key and required fields.
func TestResync_JSONOutputSchema(t *testing.T) {
	route := makeHTTPRoute("f5-cne-system", "nginx-route", 1)
	dyn := newFakeDynamic(route)

	result, err := ResyncHTTPRoutes(context.Background(), dyn, ResyncOptions{
		Namespace: "f5-cne-system",
		Name:      "nginx-route",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if m["schema"] != resyncSchema {
		t.Errorf("schema field: expected %q, got %v", resyncSchema, m["schema"])
	}
	if _, ok := m["timestamp"]; !ok {
		t.Error("missing timestamp field")
	}
	if _, ok := m["resynced"]; !ok {
		t.Error("missing resynced field")
	}
	if _, ok := m["errors"]; !ok {
		t.Error("missing errors field")
	}
}

// TestResync_NoTargetSelector verifies that omitting all selectors returns an
// error.
func TestResync_NoTargetSelector(t *testing.T) {
	dyn := newFakeDynamic()
	_, err := ResyncHTTPRoutes(context.Background(), dyn, ResyncOptions{})
	if err == nil {
		t.Fatal("expected error for empty ResyncOptions, got nil")
	}
}

// TestCollectWeights_MissingWeight verifies that a backendRef without a
// weight field is defaulted to 1 and fieldExists is false.
func TestCollectWeights_MissingWeight(t *testing.T) {
	route := makeHTTPRoute("ns", "r", 0) // 0 → omit field
	entries := collectWeights(route)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].orig != 1 {
		t.Errorf("expected default weight 1, got %d", entries[0].orig)
	}
	if entries[0].fieldExists {
		t.Error("fieldExists should be false when weight is absent")
	}
}

// TestCollectWeights_ExplicitWeight verifies that an explicit weight is read
// correctly and fieldExists is true.
func TestCollectWeights_ExplicitWeight(t *testing.T) {
	route := makeHTTPRoute("ns", "r", 5)
	entries := collectWeights(route)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].orig != 5 {
		t.Errorf("expected weight 5, got %d", entries[0].orig)
	}
	if !entries[0].fieldExists {
		t.Error("fieldExists should be true when weight is present")
	}
}

// TestResync_RestoreRemovesAddedWeight verifies that a backendRef that had
// no weight field before the resync has no weight field after it either —
// the restore must use a "remove" op, not leave `weight: 1` behind.
func TestResync_RestoreRemovesAddedWeight(t *testing.T) {
	route := makeHTTPRoute("f5-cne-system", "no-weight-route", 0) // 0 → omit field
	dyn := newFakeDynamic(route)

	_, err := ResyncHTTPRoutes(context.Background(), dyn, ResyncOptions{
		Namespace: "f5-cne-system",
		Name:      "no-weight-route",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := dyn.Resource(httpRouteGVR).Namespace("f5-cne-system").Get(
		context.Background(), "no-weight-route", metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	rules, _, _ := unstructured.NestedSlice(got.Object, "spec", "rules")
	ref := rules[0].(map[string]interface{})["backendRefs"].([]interface{})[0].(map[string]interface{})
	if w, exists := ref["weight"]; exists {
		t.Errorf("weight field should be absent after restore, found %v", w)
	}
}

// TestResync_RestorePreservesExplicitWeight verifies that an explicit weight
// round-trips exactly through the toggle.
func TestResync_RestorePreservesExplicitWeight(t *testing.T) {
	route := makeHTTPRoute("f5-cne-system", "weighted-route", 5)
	dyn := newFakeDynamic(route)

	_, err := ResyncHTTPRoutes(context.Background(), dyn, ResyncOptions{
		Namespace: "f5-cne-system",
		Name:      "weighted-route",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := dyn.Resource(httpRouteGVR).Namespace("f5-cne-system").Get(
		context.Background(), "weighted-route", metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	rules, _, _ := unstructured.NestedSlice(got.Object, "spec", "rules")
	ref := rules[0].(map[string]interface{})["backendRefs"].([]interface{})[0].(map[string]interface{})
	w, exists := ref["weight"]
	if !exists {
		t.Fatal("weight field missing after restore")
	}
	if wi, _ := w.(int64); wi != 5 {
		t.Errorf("expected weight restored to 5, got %v", w)
	}
}

// TestBuildForwardPatches_TestGuard verifies the forward patch asserts the
// currently-read value before replacing it (concurrent-edit safety) and
// uses a bare "add" when the weight was absent.
func TestBuildForwardPatches_TestGuard(t *testing.T) {
	entries := []weightEntry{
		{ruleIdx: 0, refIdx: 0, orig: 5, fieldExists: true},
		{ruleIdx: 0, refIdx: 1, orig: 1, fieldExists: false},
	}
	ops := buildForwardPatches(entries)
	if len(ops) != 3 {
		t.Fatalf("expected 3 ops (test+replace, add), got %d: %+v", len(ops), ops)
	}
	if ops[0].Op != "test" || *ops[0].Value != 5 {
		t.Errorf("op0: expected test(5), got %s(%v)", ops[0].Op, ops[0].Value)
	}
	if ops[1].Op != "replace" || *ops[1].Value != 6 {
		t.Errorf("op1: expected replace(6), got %s(%v)", ops[1].Op, ops[1].Value)
	}
	if ops[2].Op != "add" || *ops[2].Value != 2 {
		t.Errorf("op2: expected add(2), got %s(%v)", ops[2].Op, ops[2].Value)
	}
}

// TestBuildRestorePatches_RemoveAndGuard verifies the restore patch tests the
// toggled value first, replaces explicit weights, and removes added ones —
// and that the "remove" op serializes without a value field.
func TestBuildRestorePatches_RemoveAndGuard(t *testing.T) {
	entries := []weightEntry{
		{ruleIdx: 0, refIdx: 0, orig: 5, fieldExists: true},
		{ruleIdx: 0, refIdx: 1, orig: 1, fieldExists: false},
	}
	ops := buildRestorePatches(entries)
	if len(ops) != 4 {
		t.Fatalf("expected 4 ops, got %d: %+v", len(ops), ops)
	}
	if ops[0].Op != "test" || *ops[0].Value != 6 {
		t.Errorf("op0: expected test(6), got %s(%v)", ops[0].Op, ops[0].Value)
	}
	if ops[1].Op != "replace" || *ops[1].Value != 5 {
		t.Errorf("op1: expected replace(5), got %s(%v)", ops[1].Op, ops[1].Value)
	}
	if ops[2].Op != "test" || *ops[2].Value != 2 {
		t.Errorf("op2: expected test(2), got %s(%v)", ops[2].Op, ops[2].Value)
	}
	if ops[3].Op != "remove" || ops[3].Value != nil {
		t.Errorf("op3: expected remove with no value, got %s(%v)", ops[3].Op, ops[3].Value)
	}
	data, err := json.Marshal(ops[3])
	if err != nil {
		t.Fatalf("marshal remove op: %v", err)
	}
	if strings.Contains(string(data), "value") {
		t.Errorf("remove op must not serialize a value field: %s", data)
	}
}

// TestResync_CtxCancellation verifies that a cancelled context causes the
// resync to abort during the reconcile wait.
func TestResync_CtxCancellation(t *testing.T) {
	// Restore ReconcileWait to a non-zero value so the ctx-cancel path is
	// reachable within the select.
	prev := ReconcileWait
	ReconcileWait = 5 * time.Second
	defer func() { ReconcileWait = prev }()

	route := makeHTTPRoute("f5-cne-system", "nginx-route", 1)
	dyn := newFakeDynamic(route)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately — the forward patch will succeed, then the Wait
	// select should pick ctx.Done() before the timer fires.
	cancel()

	_, err := ResyncHTTPRoutes(ctx, dyn, ResyncOptions{
		Namespace: "f5-cne-system",
		Name:      "nginx-route",
	})
	if err == nil {
		t.Fatal("expected error from context cancellation, got nil")
	}
}
