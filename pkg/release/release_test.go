package release

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func TestParseTrain(t *testing.T) {
	cases := []struct {
		ver  string
		want Train
	}{
		{"2.3.2-3.2598.3-0.0.464", Train{2, 3}},
		{"2.4.0", Train{2, 4}},
		{"v2.5.0", Train{2, 5}},
		{"2.6.1-rc1", Train{2, 6}},
	}
	for _, tc := range cases {
		got := ParseTrain(tc.ver)
		if got != tc.want {
			t.Errorf("ParseTrain(%q) = %v, want %v", tc.ver, got, tc.want)
		}
	}
}

func TestResolveCatalogAndForwardCompatibility(t *testing.T) {
	ctx := context.Background()

	p23, err := Resolve(ctx, "2.3.2-3.2598.3-0.0.464", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p23.NetworkModel != NetworkModelLegacySPK || !p23.Train.Is23() {
		t.Fatalf("2.3 profile mismatch: %+v", p23)
	}
	if p23.ServedGatewayAPIVersion != "gateway.k8s.f5.com/v1alpha1" {
		t.Fatalf("2.3 gateway API = %q", p23.ServedGatewayAPIVersion)
	}

	p24, err := Resolve(ctx, "2.4.0", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p24.NetworkModel != NetworkModelGatewayInfra || !p24.Train.Is24OrLater() {
		t.Fatalf("2.4 profile mismatch: %+v", p24)
	}
	if p24.ServedGatewayAPIVersion != "gateway.k8s.f5.com/v1alpha1" {
		t.Fatalf("2.4 gateway API = %q, want gateway.k8s.f5.com/v1alpha1", p24.ServedGatewayAPIVersion)
	}
	if p24.FLOChartVersion != "v2.30.0-0.5.2" {
		t.Fatalf("2.4 FLOChartVersion = %q", p24.FLOChartVersion)
	}
	if !p24.Quirks.EndpointSliceGetRBAC {
		t.Fatalf("expected 2.4.0 EndpointSliceGetRBAC quirk to be enabled")
	}

	for _, ver241 := range []string{"2.4.1", "2.4.1-3.2850.1-0.0.9"} {
		p241, err := Resolve(ctx, ver241, "", nil)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", ver241, err)
		}
		if p241.NetworkModel != NetworkModelGatewayInfra || !p241.Train.Is24OrLater() {
			t.Fatalf("%s profile mismatch: %+v", ver241, p241)
		}
		if p241.FLOChartVersion != "v2.30.1-0.0.8" {
			t.Fatalf("%s FLOChartVersion = %q, want v2.30.1-0.0.8", ver241, p241.FLOChartVersion)
		}
		if p241.ServedF5InferenceAPIVersion != "inference.k8s.f5.com/v1alpha1" {
			t.Fatalf("%s ServedF5InferenceAPIVersion = %q, want inference.k8s.f5.com/v1alpha1", ver241, p241.ServedF5InferenceAPIVersion)
		}
		if p241.Quirks.PodManagerHeal {
			t.Fatalf("expected %s PodManagerHeal quirk to be false", ver241)
		}
	}

	p25, err := Resolve(ctx, "2.5.0", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !p25.Train.AtLeast(2, 5) || p25.NetworkModel != NetworkModelGatewayInfra {
		t.Fatalf("2.5 profile mismatch: %+v", p25)
	}
	if p25.ServedGatewayAPIVersion != "gateway.k8s.f5.com/v1" {
		t.Fatalf("2.5 gateway API = %q, want gateway.k8s.f5.com/v1", p25.ServedGatewayAPIVersion)
	}
	if p25.Quirks.EndpointSliceGetRBAC {
		t.Fatalf("expected 2.5.0 EndpointSliceGetRBAC quirk to default false")
	}

	// Simulated future 2.6.0-rc1 should synthesize cleanly without panic or error
	p26, err := Resolve(ctx, "2.6.0-rc1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !p26.Train.AtLeast(2, 6) || p26.NetworkModel != NetworkModelGatewayInfra {
		t.Fatalf("2.6 profile mismatch: %+v", p26)
	}
}

func TestDiscoverServedAPIVersions(t *testing.T) {
	cs := fake.NewClientset()
	fd, ok := cs.Discovery().(*fakediscovery.FakeDiscovery)
	if !ok {
		t.Fatal("expected FakeDiscovery")
	}
	fd.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "gateway.k8s.f5.com/v2",
			APIResources: []metav1.APIResource{{Name: "infras", Kind: "Infra"}},
		},
		{
			GroupVersion: "inference.networking.k8s.io/v1beta1",
			APIResources: []metav1.APIResource{{Name: "inferencepools", Kind: "InferencePool"}},
		},
		{
			GroupVersion: "inference.k8s.f5.com/v1beta1",
			APIResources: []metav1.APIResource{{Name: "aigatewayprofiles", Kind: "AIGatewayProfile"}},
		},
	}

	prof, err := Resolve(context.Background(), "2.5.0", "", fd)
	if err != nil {
		t.Fatal(err)
	}
	if prof.ServedGatewayAPIVersion != "gateway.k8s.f5.com/v2" {
		t.Fatalf("ServedGatewayAPIVersion = %q, want gateway.k8s.f5.com/v2", prof.ServedGatewayAPIVersion)
	}
	if prof.ServedInferenceAPIVersion != "inference.networking.k8s.io/v1beta1" {
		t.Fatalf("ServedInferenceAPIVersion = %q, want inference.networking.k8s.io/v1beta1", prof.ServedInferenceAPIVersion)
	}
	if prof.ServedF5InferenceAPIVersion != "inference.k8s.f5.com/v1beta1" {
		t.Fatalf("ServedF5InferenceAPIVersion = %q, want inference.k8s.f5.com/v1beta1", prof.ServedF5InferenceAPIVersion)
	}
}
