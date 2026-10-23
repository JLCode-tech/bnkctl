//lint:file-ignore SA1019 k8sfake.NewSimpleClientset is still functional — NewClientset requires --with-applyconfig codegen
package k8s

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// cneGVR is the GVR for CNEInstance, used across wait tests.
var cneGVR = schema.GroupVersionResource{
	Group:    "k8s.f5.com",
	Version:  "v1",
	Resource: "cneinstances",
}

// buildCNEInstance constructs an unstructured CNEInstance with the given
// status.state value.
func buildCNEInstance(name, ns, state string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "k8s.f5.com/v1",
			"kind":       "CNEInstance",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
		},
	}
	if state != "" {
		obj.Object["status"] = map[string]interface{}{
			"state": state,
		}
	}
	return obj
}

// buildFakeDynamicClient builds a dynamicfake client seeded with the given objects.
func buildFakeDynamicClient(scheme *runtime.Scheme, gvr schema.GroupVersionResource, listKind string, objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		gvr: listKind,
	}, objs...)
}

// waitScheme returns a minimal runtime.Scheme for the dynamic fake client.
func waitScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(schema.GroupVersionKind{
		Group:   "k8s.f5.com",
		Version: "v1",
		Kind:    "CNEInstance",
	}, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{
		Group:   "k8s.f5.com",
		Version: "v1",
		Kind:    "CNEInstanceList",
	}, &unstructured.UnstructuredList{})
	return s
}

// ─── WaitForUnstructuredCondition tests ────────────────────────────────────

// TestWaitForUnstructuredCondition_SuccessOnMatch verifies that the function
// returns nil when the field value already matches the expected value.
func TestWaitForUnstructuredCondition_SuccessOnMatch(t *testing.T) {
	scheme := waitScheme()
	cne := buildCNEInstance("my-bnk", "f5-cne-system", "Ready")
	dyn := buildFakeDynamicClient(scheme, cneGVR, "CNEInstanceList", cne)

	err := WaitForUnstructuredCondition(
		context.Background(), dyn, cneGVR,
		"f5-cne-system", "my-bnk",
		"status.state", "Ready",
		10*time.Second,
	)
	if err != nil {
		t.Fatalf("WaitForUnstructuredCondition: expected nil, got: %v", err)
	}
}

// TestWaitForUnstructuredCondition_TimeoutOnNoMatch verifies that the function
// returns an error when the field never reaches the expected value before timeout.
func TestWaitForUnstructuredCondition_TimeoutOnNoMatch(t *testing.T) {
	scheme := waitScheme()
	cne := buildCNEInstance("my-bnk", "f5-cne-system", "Pending")
	dyn := buildFakeDynamicClient(scheme, cneGVR, "CNEInstanceList", cne)

	err := WaitForUnstructuredCondition(
		context.Background(), dyn, cneGVR,
		"f5-cne-system", "my-bnk",
		"status.state", "Ready",
		100*time.Millisecond, // very short timeout
	)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

// TestWaitForUnstructuredCondition_CtxCancellation verifies that cancelling the
// context causes the function to return promptly with an error.
func TestWaitForUnstructuredCondition_CtxCancellation(t *testing.T) {
	scheme := waitScheme()
	cne := buildCNEInstance("my-bnk", "f5-cne-system", "Pending")
	dyn := buildFakeDynamicClient(scheme, cneGVR, "CNEInstanceList", cne)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately.
	cancel()

	err := WaitForUnstructuredCondition(
		ctx, dyn, cneGVR,
		"f5-cne-system", "my-bnk",
		"status.state", "Ready",
		10*time.Second,
	)
	if err == nil {
		t.Fatal("expected error from ctx cancellation, got nil")
	}
}

// ─── splitDotPath tests ─────────────────────────────────────────────────────

func TestSplitDotPath(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"status.state", []string{"status", "state"}},
		{"status", []string{"status"}},
		{"a.b.c", []string{"a", "b", "c"}},
		{"", nil},
	}
	for _, tc := range cases {
		got := splitDotPath(tc.input)
		if len(got) != len(tc.want) {
			t.Errorf("splitDotPath(%q): got %v, want %v", tc.input, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitDotPath(%q)[%d]: got %q, want %q", tc.input, i, got[i], tc.want[i])
			}
		}
	}
}

// ─── unstructured.NestedString helper test ─────────────────────────────────

// TestUnstructuredNestedString_MissingFieldReturnsEmpty verifies that NestedString
// returns an empty string (not an error) for a missing field.
func TestUnstructuredNestedString_MissingFieldReturnsEmpty(t *testing.T) {
	obj := map[string]interface{}{
		"status": map[string]interface{}{},
	}
	val, found, err := unstructured.NestedString(obj, "status", "state")
	if err != nil {
		t.Fatalf("NestedString: unexpected error: %v", err)
	}
	if found {
		t.Errorf("NestedString: found=true for missing key, got %q", val)
	}
	if val != "" {
		t.Errorf("NestedString: val=%q, want empty string", val)
	}
}

// ─── WaitForCRDExists + WaitForCertificateReady smoke ─────────────────────

// TestWaitForCRDExists_ImmediatelyPresent verifies the happy path: CRD exists
// in the dynamic client and WaitForCRDExists returns nil without waiting.
func TestWaitForCRDExists_ImmediatelyPresent(t *testing.T) {
	crdGVR := schema.GroupVersionResource{
		Group:    "apiextensions.k8s.io",
		Version:  "v1",
		Resource: "customresourcedefinitions",
	}
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(schema.GroupVersionKind{
		Group:   "apiextensions.k8s.io",
		Version: "v1",
		Kind:    "CustomResourceDefinition",
	}, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{
		Group:   "apiextensions.k8s.io",
		Version: "v1",
		Kind:    "CustomResourceDefinitionList",
	}, &unstructured.UnstructuredList{})

	crd := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apiextensions.k8s.io/v1",
			"kind":       "CustomResourceDefinition",
			"metadata": map[string]interface{}{
				"name": "cneinstances.k8s.f5.com",
			},
		},
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(s, map[schema.GroupVersionResource]string{
		crdGVR: "CustomResourceDefinitionList",
	}, crd)

	err := WaitForCRDExists(context.Background(), dyn, "cneinstances.k8s.f5.com", 10*time.Second)
	if err != nil {
		t.Fatalf("WaitForCRDExists: %v", err)
	}
}

// TestWaitForCRDExists_Timeout verifies that WaitForCRDExists times out when
// the CRD is absent.
func TestWaitForCRDExists_Timeout(t *testing.T) {
	crdGVR := schema.GroupVersionResource{
		Group:    "apiextensions.k8s.io",
		Version:  "v1",
		Resource: "customresourcedefinitions",
	}
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(schema.GroupVersionKind{
		Group:   "apiextensions.k8s.io",
		Version: "v1",
		Kind:    "CustomResourceDefinitionList",
	}, &unstructured.UnstructuredList{})

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(s, map[schema.GroupVersionResource]string{
		crdGVR: "CustomResourceDefinitionList",
	})

	err := WaitForCRDExists(context.Background(), dyn, "licenses.k8s.f5net.com", 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

// ─── WaitForPodSucceeded tests ────────────────────────────────────────────────

func buildPodWithPhase(ns, name string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

// TestWaitForPodSucceeded_ImmediateSuccess: pod already Succeeded.
func TestWaitForPodSucceeded_ImmediateSuccess(t *testing.T) {
	pod := buildPodWithPhase("kube-system", "iface-discovery", corev1.PodSucceeded)
	cs := k8sfake.NewSimpleClientset(pod)

	if err := WaitForPodSucceeded(context.Background(), cs, "kube-system", "iface-discovery", 2*time.Second); err != nil {
		t.Fatalf("expected nil for Succeeded pod, got: %v", err)
	}
}

// TestWaitForPodSucceeded_FailedPod: pod enters Failed — should error immediately.
func TestWaitForPodSucceeded_FailedPod(t *testing.T) {
	pod := buildPodWithPhase("kube-system", "iface-discovery", corev1.PodFailed)
	cs := k8sfake.NewSimpleClientset(pod)

	err := WaitForPodSucceeded(context.Background(), cs, "kube-system", "iface-discovery", 2*time.Second)
	if err == nil {
		t.Fatal("expected error for Failed pod, got nil")
	}
	if !strings.Contains(err.Error(), "Failed phase") {
		t.Errorf("error should mention 'Failed phase': %v", err)
	}
}

// TestWaitForPodSucceeded_Timeout: pod stays Pending — times out.
func TestWaitForPodSucceeded_Timeout(t *testing.T) {
	pod := buildPodWithPhase("kube-system", "iface-discovery", corev1.PodPending)
	cs := k8sfake.NewSimpleClientset(pod)

	err := WaitForPodSucceeded(context.Background(), cs, "kube-system", "iface-discovery", 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error for Pending pod, got nil")
	}
}

// Ensure metav1 import is used (for GetOptions in the production code path).
var _ = metav1.GetOptions{}

// ─── WaitForNodeHugepagesCapacity tests ───────────────────────────────────────

func nodeWithHugepages(name, hugepagesCap string) *corev1.Node {
	cap := corev1.ResourceList{}
	if hugepagesCap != "" {
		cap["hugepages-2Mi"] = resource.MustParse(hugepagesCap)
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Capacity: cap},
	}
}

// TestWaitForNodeHugepagesCapacity_AlreadyAdvertised: node already reports
// >= want — returns nil on first poll.
func TestWaitForNodeHugepagesCapacity_AlreadyAdvertised(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(nodeWithHugepages("n1", "4Gi"))

	if err := WaitForNodeHugepagesCapacity(context.Background(), cs, "n1", "4Gi", 2*time.Second); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestWaitForNodeHugepagesCapacity_ExceedsRequest: node has more than the
// requested capacity — predicate passes (>= not ==).
func TestWaitForNodeHugepagesCapacity_ExceedsRequest(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(nodeWithHugepages("n1", "8Gi"))

	if err := WaitForNodeHugepagesCapacity(context.Background(), cs, "n1", "4Gi", 2*time.Second); err != nil {
		t.Fatalf("expected nil with excess capacity, got %v", err)
	}
}

// TestWaitForNodeHugepagesCapacity_BelowRequest_Timeout: node reports less
// than want — poll retries and eventually times out.
func TestWaitForNodeHugepagesCapacity_BelowRequest_Timeout(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(nodeWithHugepages("n1", "2Gi"))

	if err := WaitForNodeHugepagesCapacity(context.Background(), cs, "n1", "4Gi", 200*time.Millisecond); err == nil {
		t.Fatal("expected timeout error when capacity is below request, got nil")
	}
}

// TestWaitForNodeHugepagesCapacity_MissingCapacityKey: node lacks the
// hugepages-2Mi capacity key entirely — should time out, not panic.
func TestWaitForNodeHugepagesCapacity_MissingCapacityKey(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(nodeWithHugepages("n1", ""))

	if err := WaitForNodeHugepagesCapacity(context.Background(), cs, "n1", "4Gi", 200*time.Millisecond); err == nil {
		t.Fatal("expected timeout when capacity key missing, got nil")
	}
}

// TestWaitForNodeHugepagesCapacity_NodeNotFound: target node doesn't exist —
// poll keeps retrying (kubelet may register late) and times out.
func TestWaitForNodeHugepagesCapacity_NodeNotFound(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()

	if err := WaitForNodeHugepagesCapacity(context.Background(), cs, "missing-node", "4Gi", 200*time.Millisecond); err == nil {
		t.Fatal("expected timeout when node absent, got nil")
	}
}

// TestWaitForNodeHugepagesCapacity_BadQuantity: malformed `want` returns a
// parse error immediately, not a timeout.
func TestWaitForNodeHugepagesCapacity_BadQuantity(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(nodeWithHugepages("n1", "4Gi"))

	err := WaitForNodeHugepagesCapacity(context.Background(), cs, "n1", "not-a-quantity", time.Second)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse hugepages quantity") {
		t.Errorf("error message: got %q, want substring 'parse hugepages quantity'", err.Error())
	}
}
