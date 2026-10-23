package bnk

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// cneResyncScheme returns a minimal scheme with CNEInstance registered.
func cneResyncScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "k8s.f5.com", Version: "v1", Kind: "CNEInstance"},
		&unstructured.Unstructured{},
	)
	s.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "k8s.f5.com", Version: "v1", Kind: "CNEInstanceList"},
		&unstructured.UnstructuredList{},
	)
	return s
}

// newFakeDynamicCNE builds a fake dynamic client with CNEInstance support and
// pre-creates the given objects.
func newFakeDynamicCNE(objs ...*unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		cneResyncScheme(),
		map[schema.GroupVersionResource]string{
			cneInstanceGVR: "CNEInstanceList",
		},
	)
	ctx := context.Background()
	for _, obj := range objs {
		if _, err := dyn.Resource(cneInstanceGVR).Namespace(obj.GetNamespace()).Create(
			ctx, obj, metav1.CreateOptions{},
		); err != nil {
			panic("newFakeDynamicCNE: " + err.Error())
		}
	}
	return dyn
}

func makeCNEInstance(ns, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "k8s.f5.com/v1",
			"kind":       "CNEInstance",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
		},
	}
}

// TestResyncCNEInstance_Happy verifies that a present CNEInstance gets the
// annotation applied and the function returns nil.
func TestResyncCNEInstance_Happy(t *testing.T) {
	const ns, name = "f5-cne-system", "syd-tracer-bnk"
	obj := makeCNEInstance(ns, name)
	dyn := newFakeDynamicCNE(obj)

	var logOut string
	err := captureStderr(func() {
		logOut = captureStderr(func() {
			if e := ResyncCNEInstance(context.Background(), dyn, ns, name); e != nil {
				t.Fatalf("ResyncCNEInstance: unexpected error: %v", e)
			}
		})
	})
	_ = err
	_ = logOut

	// Re-fetch and verify the annotation was written.
	got, err2 := dyn.Resource(cneInstanceGVR).Namespace(ns).Get(
		context.Background(), name, metav1.GetOptions{},
	)
	if err2 != nil {
		t.Fatalf("re-fetch: %v", err2)
	}
	annots := got.GetAnnotations()
	if annots == nil {
		t.Fatal("expected annotations map, got nil")
	}
	ts, ok := annots["awsbnkctl.io/resync-trigger"]
	if !ok {
		t.Fatal("annotation awsbnkctl.io/resync-trigger not set")
	}
	if ts == "" {
		t.Error("annotation value is empty")
	}
}

// TestResyncCNEInstance_LogOutput verifies that a successful patch logs the
// expected message to Stderr.
func TestResyncCNEInstance_LogOutput(t *testing.T) {
	const ns, name = "f5-cne-system", "syd-tracer-bnk"
	obj := makeCNEInstance(ns, name)
	dyn := newFakeDynamicCNE(obj)

	out := captureStderr(func() {
		if err := ResyncCNEInstance(context.Background(), dyn, ns, name); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	if !strings.Contains(out, "resync-cne") {
		t.Errorf("expected [resync-cne] in log output, got: %q", out)
	}
}

// TestResyncCNEInstance_NotFound verifies that patching a nonexistent
// CNEInstance returns a non-nil error.
func TestResyncCNEInstance_NotFound(t *testing.T) {
	dyn := newFakeDynamicCNE() // empty — no objects pre-created

	err := ResyncCNEInstance(context.Background(), dyn, "f5-cne-system", "does-not-exist")
	if err == nil {
		t.Fatal("expected error for nonexistent CNEInstance, got nil")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error message should mention the resource name, got: %v", err)
	}
}
