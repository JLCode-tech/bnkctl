package proxyprotocoll4_test

// Unit tests for waitL4RouteCondition covering both status paths:
//   (a) flat .status.conditions — introduced in CHANGE 2 (hardening)
//   (b) per-parent .status.parents[*].conditions — regression guard for
//       the original code path

import (
	"context"
	"io"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
	"github.com/JLCode-tech/bnkctl/pkg/scenarios/proxyprotocoll4"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

// l4RouteScheme builds a minimal runtime.Scheme that registers L4Route and its
// list kind, as required by dynamicfake.NewSimpleDynamicClientWithCustomListKinds.
func l4RouteScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	gvr := proxyprotocoll4.L4RouteGVRForTest
	gvk := proxyprotocoll4.L4RouteGVKForTest()
	listGVK := schema.GroupVersionKind{
		Group:   gvk.Group,
		Version: gvk.Version,
		Kind:    gvk.Kind + "List",
	}
	s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	_ = gvr // referenced indirectly via L4RouteGVRForTest in newFakeL4Client
	return s
}

// newFakeL4Client creates a fake dynamic client with the given L4Route object
// pre-seeded via Create (explicit Create avoids fake-client GVR deduction issues
// with unstructured objects — same pattern as pkg/bnk/resync_test.go).
func newFakeL4Client(obj *unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	gvr := proxyprotocoll4.L4RouteGVRForTest
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		l4RouteScheme(),
		map[schema.GroupVersionResource]string{
			gvr: "L4RouteList",
		},
	)
	ctx := context.Background()
	ns := obj.GetNamespace()
	if _, err := dyn.Resource(gvr).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		panic("newFakeL4Client: Create: " + err.Error())
	}
	return dyn
}

// makeL4Route builds an unstructured L4Route with the supplied status shape.
func makeL4RouteFlat(ns, name, condType, condStatus string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "gateway.k8s.f5.com/v1",
			"kind":       "L4Route",
			"metadata": map[string]interface{}{
				"name":            name,
				"namespace":       ns,
				"resourceVersion": "1",
			},
			"status": map[string]interface{}{
				"conditions": []interface{}{
					map[string]interface{}{
						"type":   condType,
						"status": condStatus,
					},
				},
			},
		},
	}
}

func makeL4RouteParents(ns, name, condType, condStatus string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "gateway.k8s.f5.com/v1",
			"kind":       "L4Route",
			"metadata": map[string]interface{}{
				"name":            name,
				"namespace":       ns,
				"resourceVersion": "1",
			},
			"status": map[string]interface{}{
				"parents": []interface{}{
					map[string]interface{}{
						"conditions": []interface{}{
							map[string]interface{}{
								"type":   condType,
								"status": condStatus,
							},
						},
					},
				},
			},
		},
	}
}

func makeSctxT(t *testing.T, dyn *dynamicfake.FakeDynamicClient) *scenarios.Context {
	t.Helper()
	st, err := state.Load(t.TempDir())
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	return &scenarios.Context{
		Ctx:     context.Background(),
		Dynamic: dyn,
		State:   st,
		Out:     io.Discard,
		Options: map[string]string{},
	}
}

// TestWaitL4RouteCondition_FlatConditions verifies that a True condition present
// ONLY in the flat .status.conditions list (path 2, added in CHANGE 2) satisfies
// the wait and returns nil.
func TestWaitL4RouteCondition_FlatConditions(t *testing.T) {
	const (
		ns       = "test-ns"
		name     = "my-l4route"
		condType = "Accepted"
	)
	obj := makeL4RouteFlat(ns, name, condType, "True")
	dyn := newFakeL4Client(obj)
	sctx := makeSctxT(t, dyn)

	err := proxyprotocoll4.WaitL4RouteConditionForTest(
		context.Background(), sctx, ns, name, condType, 2*time.Second,
	)
	if err != nil {
		t.Errorf("expected nil when condition True in flat .status.conditions, got: %v", err)
	}
}

// TestWaitL4RouteCondition_ParentConditions verifies the original per-parent
// path (.status.parents[*].conditions) still returns nil when the condition is
// True — regression guard for the code that existed before CHANGE 2.
func TestWaitL4RouteCondition_ParentConditions(t *testing.T) {
	const (
		ns       = "test-ns"
		name     = "my-l4route"
		condType = "Accepted"
	)
	obj := makeL4RouteParents(ns, name, condType, "True")
	dyn := newFakeL4Client(obj)
	sctx := makeSctxT(t, dyn)

	err := proxyprotocoll4.WaitL4RouteConditionForTest(
		context.Background(), sctx, ns, name, condType, 2*time.Second,
	)
	if err != nil {
		t.Errorf("expected nil when condition True in .status.parents[].conditions, got: %v", err)
	}
}
