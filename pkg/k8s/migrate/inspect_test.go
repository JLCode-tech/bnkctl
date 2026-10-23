package migrate

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

func TestInspect_Fixture(t *testing.T) {
	dyn := newFakeDynamic(t, parseFixture(t, fixture23)...)
	inv, err := Inspect(context.Background(), dyn, "")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if inv.Namespace != DefaultInstanceNamespace {
		t.Errorf("namespace = %q", inv.Namespace)
	}
	counts := map[string]int{
		"Vlans": len(inv.Vlans), "StaticRoutes": len(inv.StaticRoutes), "Egresses": len(inv.Egresses),
		"Snatpools": len(inv.Snatpools), "BnkGateways": len(inv.BnkGateways), "SecPolicies": len(inv.SecPolicies),
		"NetPolicies": len(inv.NetPolicies), "Gateways": len(inv.Gateways), "GatewayClasses": len(inv.GatewayClasses),
	}
	want := map[string]int{"Vlans": 2, "StaticRoutes": 2, "Egresses": 1, "Snatpools": 1, "BnkGateways": 1, "SecPolicies": 1, "NetPolicies": 1, "Gateways": 3, "GatewayClasses": 2}
	for k, w := range want {
		if counts[k] != w {
			t.Errorf("%s = %d, want %d", k, counts[k], w)
		}
	}
	if inv.CNEInstance == nil || inv.CNEInstance.GetName() != "lab-bnk" {
		t.Errorf("CNEInstance = %v", inv.CNEInstance)
	}
	if inv.ManifestVersion() != "2.3.3-3.2598.3-0.0.509" {
		t.Errorf("ManifestVersion = %q", inv.ManifestVersion())
	}
	if len(inv.Missing) != 0 {
		t.Errorf("Missing = %v", inv.Missing)
	}
	// Sorted by namespace then name.
	if inv.Gateways[0].GetNamespace() != "default" || inv.Gateways[0].GetName() != "bnk-agentcore-demo-gateway" || inv.Gateways[2].GetNamespace() != "http2-scenario" {
		t.Errorf("Gateways not sorted: %s, %s, %s", objKey(inv.Gateways[0]), objKey(inv.Gateways[1]), objKey(inv.Gateways[2]))
	}
	if inv.Vlans[0].GetName() != "ext-vlan" || inv.Vlans[1].GetName() != "int-vlan" {
		t.Errorf("Vlans not sorted")
	}
	summary := strings.Join(inv.Summary(), "\n")
	for _, want := range []string{"F5SPKVlan: 2", "F5SPKEgress: 1", "Gateway: 3"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
}

func TestInspect_MissingCRD(t *testing.T) {
	dyn := newFakeDynamic(t, parseFixture(t, fixture23)...)
	notServed := func(gvr schema.GroupVersionResource) {
		dyn.PrependReactor("list", gvr.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(gvr.GroupResource(), "")
		})
	}
	notServed(EgressGVR)
	notServed(BNKNetPolGVR)
	inv, err := Inspect(context.Background(), dyn, "f5-cne-system")
	if err != nil {
		t.Fatalf("Inspect must tolerate unserved CRDs: %v", err)
	}
	if strings.Join(inv.Missing, ",") != "F5SPKEgress,BNKNetPolicy" {
		t.Errorf("Missing = %v", inv.Missing)
	}
	if len(inv.Egresses) != 0 || len(inv.Vlans) != 2 {
		t.Errorf("Egresses = %d, Vlans = %d", len(inv.Egresses), len(inv.Vlans))
	}
	if !strings.Contains(strings.Join(inv.Summary(), "\n"), "CRDs not served: F5SPKEgress, BNKNetPolicy") {
		t.Errorf("summary = %v", inv.Summary())
	}
}

func TestInspect_OtherErrorsPropagate(t *testing.T) {
	dyn := newFakeDynamic(t, parseFixture(t, fixture23)...)
	dyn.PrependReactor("list", VlanGVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(VlanGVR.GroupResource(), "", nil)
	})
	if _, err := Inspect(context.Background(), dyn, ""); err == nil || !strings.Contains(err.Error(), "F5SPKVlan") {
		t.Errorf("err = %v, want a forbidden error naming F5SPKVlan", err)
	}
}

func TestInspect_NoCNEInstance(t *testing.T) {
	objs := parseFixture(t, fixture23)
	var rest []*unstructured.Unstructured
	for _, o := range objs {
		if o.GetKind() != "CNEInstance" {
			rest = append(rest, o)
		}
	}
	dyn := newFakeDynamic(t, rest...)
	inv, err := Inspect(context.Background(), dyn, "")
	if err != nil {
		t.Fatal(err)
	}
	if inv.CNEInstance != nil || inv.ManifestVersion() != "" {
		t.Errorf("CNEInstance = %v", inv.CNEInstance)
	}
}
