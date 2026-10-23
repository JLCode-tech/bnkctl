package bnk

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// makeEndpointSlice builds an unstructured EndpointSlice labelled for the
// given parent service.
func makeEndpointSlice(ns, name, serviceName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "discovery.k8s.io/v1",
			"kind":       "EndpointSlice",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
				"labels": map[string]interface{}{
					serviceNameLabel: serviceName,
				},
			},
			"addressType": "IPv4",
		},
	}
}

// routePatchCount counts recorded patch actions against the httproutes
// resource. The fake tracker does not bump resourceVersion on JSON patch,
// so the action log is the observable signal that a resync ran.
func routePatchCount(dyn *dynamicfake.FakeDynamicClient) int {
	n := 0
	for _, a := range dyn.Actions() {
		if a.GetVerb() == "patch" && a.GetResource() == httpRouteGVR {
			n++
		}
	}
	return n
}

// waitForPatches polls until at least want patch actions have been
// recorded against httproutes, or the timeout elapses.
func waitForPatches(dyn *dynamicfake.FakeDynamicClient, want int, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if n := routePatchCount(dyn); n >= want {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
	return routePatchCount(dyn)
}

// TestWatch_SliceChangeTriggersResync verifies the end-to-end daemon path:
// an EndpointSlice event for a service referenced by the watched HTTPRoute
// causes the weight-toggle resync (observable as a resourceVersion bump
// with the spec restored).
func TestWatch_SliceChangeTriggersResync(t *testing.T) {
	route := makeHTTPRoute("f5-cne-system", "nginx-route", 1)
	// makeHTTPRoute's backendRef name is "my-svc".
	dyn := newFakeDynamic(route)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- WatchHTTPRoutes(ctx, dyn, WatchOptions{
			ResyncOptions: ResyncOptions{Namespace: "f5-cne-system", Name: "nginx-route"},
			Debounce:      10 * time.Millisecond,
			IndexRefresh:  time.Hour,
		})
	}()

	// Give the goroutine time to establish the watch — events created
	// before the watch exists are not delivered by the fake tracker.
	time.Sleep(100 * time.Millisecond)

	if _, err := dyn.Resource(endpointSliceGVR).Namespace("f5-cne-system").Create(
		ctx, makeEndpointSlice("f5-cne-system", "my-svc-abc12", "my-svc"), metav1.CreateOptions{},
	); err != nil {
		t.Fatalf("creating EndpointSlice: %v", err)
	}

	// A resync is two patches (forward + restore).
	if n := waitForPatches(dyn, 2, 3*time.Second); n < 2 {
		t.Fatalf("EndpointSlice change did not trigger a resync (%d patch action(s) recorded)", n)
	}

	// The toggle must have restored the original weight.
	got, err := dyn.Resource(httpRouteGVR).Namespace("f5-cne-system").Get(context.Background(), "nginx-route", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	rules, _, _ := unstructured.NestedSlice(got.Object, "spec", "rules")
	ref := rules[0].(map[string]interface{})["backendRefs"].([]interface{})[0].(map[string]interface{})
	if w, _ := ref["weight"].(int64); w != 1 {
		t.Errorf("expected weight restored to 1 after auto-resync, got %v", ref["weight"])
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("WatchHTTPRoutes returned unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("WatchHTTPRoutes did not exit after context cancellation")
	}
}

// TestWatch_UnrelatedServiceIgnored verifies a slice for a service no
// targeted route references does not trigger a resync.
func TestWatch_UnrelatedServiceIgnored(t *testing.T) {
	route := makeHTTPRoute("f5-cne-system", "nginx-route", 1)
	dyn := newFakeDynamic(route)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- WatchHTTPRoutes(ctx, dyn, WatchOptions{
			ResyncOptions: ResyncOptions{Namespace: "f5-cne-system", Name: "nginx-route"},
			Debounce:      10 * time.Millisecond,
			IndexRefresh:  time.Hour,
		})
	}()
	time.Sleep(100 * time.Millisecond)

	if _, err := dyn.Resource(endpointSliceGVR).Namespace("f5-cne-system").Create(
		ctx, makeEndpointSlice("f5-cne-system", "other-svc-xyz", "other-svc"), metav1.CreateOptions{},
	); err != nil {
		t.Fatalf("creating EndpointSlice: %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	if n := routePatchCount(dyn); n != 0 {
		t.Errorf("unrelated service change triggered a resync (%d patch action(s) recorded)", n)
	}

	cancel()
	<-done
}

// TestBuildServiceIndex verifies kind filtering, namespace defaulting, and
// cross-namespace backendRefs.
func TestBuildServiceIndex(t *testing.T) {
	route := makeHTTPRoute("ns-a", "route-a", 1) // references my-svc in ns-a
	// Add a second rule: a cross-namespace Service ref and a non-Service ref.
	spec := route.Object["spec"].(map[string]interface{})
	spec["rules"] = append(spec["rules"].([]interface{}), map[string]interface{}{
		"backendRefs": []interface{}{
			map[string]interface{}{"name": "remote-svc", "namespace": "ns-b", "port": int64(80)},
			map[string]interface{}{"name": "not-a-svc", "kind": "ConfigMap", "port": int64(80)},
		},
	})
	dyn := newFakeDynamic(route)

	index, err := buildServiceIndex(context.Background(), dyn, ResyncOptions{Namespace: "ns-a", Name: "route-a"})
	if err != nil {
		t.Fatalf("buildServiceIndex: %v", err)
	}

	rk := routeKey{ns: "ns-a", name: "route-a"}
	if !containsRoute(index["ns-a/my-svc"], rk) {
		t.Errorf("expected ns-a/my-svc to map to route-a; index: %v", index)
	}
	if !containsRoute(index["ns-b/remote-svc"], rk) {
		t.Errorf("expected cross-namespace ns-b/remote-svc to map to route-a; index: %v", index)
	}
	if len(index["ns-a/not-a-svc"]) != 0 {
		t.Errorf("non-Service backendRef must not be indexed; index: %v", index)
	}
	if len(index) != 2 {
		t.Errorf("expected exactly 2 indexed services, got %d: %v", len(index), index)
	}
}

// TestWatch_BadSelectorFailsFast verifies an unresolvable selector returns
// an error instead of looping forever.
func TestWatch_BadSelectorFailsFast(t *testing.T) {
	dyn := newFakeDynamic()
	err := WatchHTTPRoutes(context.Background(), dyn, WatchOptions{
		ResyncOptions: ResyncOptions{}, // no selector at all
	})
	if err == nil {
		t.Fatal("expected error for empty selector, got nil")
	}
}
