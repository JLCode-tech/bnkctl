package corefiles

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
)

const testNS = "f5-cne-system"

func fastPoll(t *testing.T) {
	t.Helper()
	oldT, oldI := reconcileTimeout, pollInterval
	reconcileTimeout, pollInterval = 50*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { reconcileTimeout, pollInterval = oldT, oldI })
}

func f5Scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, k := range []string{"CNEInstance", "CNEInstanceList", "CoreMond", "CoreMondList"} {
		obj := runtime.Object(&unstructured.Unstructured{})
		if strings.HasSuffix(k, "List") {
			obj = &unstructured.UnstructuredList{}
		}
		s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "k8s.f5.com", Version: "v1", Kind: k}, obj)
	}
	return s
}

func cneInstance(name string, coreMonCondition string, enabled bool) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8s.f5.com/v1",
		"kind":       "CNEInstance",
		"metadata":   map[string]interface{}{"name": name, "namespace": testNS},
		"spec":       map[string]interface{}{"coreCollection": map[string]interface{}{"enabled": enabled}},
		"status":     map[string]interface{}{"conditions": []interface{}{}},
	}}
	if coreMonCondition != "" {
		obj.Object["status"] = map[string]interface{}{"conditions": []interface{}{
			map[string]interface{}{"type": "CoreMonAvailable", "status": coreMonCondition},
		}}
	}
	return obj
}

func coreMond() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8s.f5.com/v1",
		"kind":       "CoreMond",
		"metadata":   map[string]interface{}{"name": "f5-coremond", "namespace": testNS},
	}}
}

func coreMondDS(ready, desired int32) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "f5-coremond", Namespace: testNS, Labels: map[string]string{"app": "f5-coremond"}},
		Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: desired, NumberReady: ready},
	}
}

func tmmDS(volumes ...string) *appsv1.DaemonSet {
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "f5-tmm", Namespace: testNS}}
	for _, v := range volumes {
		ds.Spec.Template.Spec.Volumes = append(ds.Spec.Template.Spec.Volumes, corev1.Volume{Name: v})
	}
	return ds
}

func testCtx(dyn *dynamicfake.FakeDynamicClient, cs *k8sfake.Clientset) *scenarios.Context {
	return &scenarios.Context{
		Ctx:       context.Background(),
		Cluster:   &intent.Cluster{Metadata: intent.Metadata{Name: "bnk-extonly"}},
		Dynamic:   dyn,
		Clientset: cs,
		Options:   map[string]string{},
	}
}

func fakeDyn(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(f5Scheme(), map[schema.GroupVersionResource]string{
		cneInstanceGVR: "CNEInstanceList",
		coreMondGVR:    "CoreMondList",
	}, objs...)
}

// TestCheckCoreMond_AllGreen: CR present, DaemonSet fully ready, CoreMon
// condition True on the CNEInstance Phase 22 applied.
func TestCheckCoreMond_AllGreen(t *testing.T) {
	fastPoll(t)
	sctx := testCtx(fakeDyn(cneInstance("bnk-extonly-bnk", "True", true), coreMond()), k8sfake.NewClientset(coreMondDS(3, 3)))
	crOK, dsOK, condOK, details := checkCoreMond(context.Background(), sctx)
	if !crOK || !dsOK || !condOK {
		t.Fatalf("want all true, got cr=%v ds=%v cond=%v (%s)", crOK, dsOK, condOK, details)
	}
}

// TestCheckCoreMond_ReportsWhatIsMissing pins the fix for the vacuous verifier:
// a half-rolled DaemonSet and a CoreMon condition that is not True must fail.
func TestCheckCoreMond_ReportsWhatIsMissing(t *testing.T) {
	fastPoll(t)
	sctx := testCtx(fakeDyn(cneInstance("bnk-extonly-bnk", "False", true), coreMond()), k8sfake.NewClientset(coreMondDS(1, 3)))
	crOK, dsOK, condOK, details := checkCoreMond(context.Background(), sctx)
	if !crOK {
		t.Errorf("crOK = false, want true (%s)", details)
	}
	if dsOK {
		t.Errorf("dsOK = true for a 1/3 DaemonSet (%s)", details)
	}
	if condOK {
		t.Errorf("condOK = true for CoreMonAvailable=False (%s)", details)
	}
	if !strings.Contains(details, "ready 1/3") {
		t.Errorf("details should name the rollout state, got %q", details)
	}
}

// TestCheckCoreMond_NoCRIsNotGreen: nothing reconciled yet → every fact false.
func TestCheckCoreMond_NoCRIsNotGreen(t *testing.T) {
	fastPoll(t)
	sctx := testCtx(fakeDyn(cneInstance("bnk-extonly-bnk", "", false)), k8sfake.NewClientset())
	crOK, dsOK, condOK, _ := checkCoreMond(context.Background(), sctx)
	if crOK || dsOK || condOK {
		t.Fatalf("want all false, got cr=%v ds=%v cond=%v", crOK, dsOK, condOK)
	}
}

// TestCheckCoreMond_SpecFallback: a build with no CoreMon condition is accepted
// only when the patch visibly landed (spec.coreCollection.enabled=true), and
// the detail says that is what was checked.
func TestCheckCoreMond_SpecFallback(t *testing.T) {
	fastPoll(t)
	sctx := testCtx(fakeDyn(cneInstance("bnk-extonly-bnk", "", true), coreMond()), k8sfake.NewClientset(coreMondDS(2, 2)))
	_, _, condOK, details := checkCoreMond(context.Background(), sctx)
	if !condOK || !strings.Contains(details, "spec.coreCollection.enabled=true") {
		t.Fatalf("condOK=%v details=%q", condOK, details)
	}
}

// TestCheckTMMVolumes: the crash mount must actually be on the f5-tmm
// DaemonSet in the CNEInstance namespace.
func TestCheckTMMVolumes(t *testing.T) {
	fastPoll(t)
	ok, detail := checkTMMVolumes(context.Background(), testCtx(nil, k8sfake.NewClientset(tmmDS("config", "core-dumps"))))
	if !ok || !strings.Contains(detail, "core-dumps") {
		t.Errorf("with core volume: ok=%v detail=%q", ok, detail)
	}
	ok, _ = checkTMMVolumes(context.Background(), testCtx(nil, k8sfake.NewClientset(tmmDS("config"))))
	if ok {
		t.Error("without core volume: ok=true, want false")
	}
	ok, _ = checkTMMVolumes(context.Background(), testCtx(nil, k8sfake.NewClientset()))
	if ok {
		t.Error("without f5-tmm DaemonSet: ok=true, want false")
	}
}
