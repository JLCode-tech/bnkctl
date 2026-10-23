package multivip_test

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
	"github.com/JLCode-tech/bnkctl/pkg/scenarios/multivip"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

func readFileHelper(path string) ([]byte, error) {
	return os.ReadFile(path) // #nosec G304 — test helper, path from WriteManifest
}

func makeCluster() *intent.Cluster {
	return &intent.Cluster{
		Metadata: intent.Metadata{
			Name:   "test-cluster",
			Region: "ap-southeast-2",
		},
		Network: intent.Network{
			VPCCidr: "10.0.0.0/16",
			AZs:     []string{"ap-southeast-2a"},
			Subnets: intent.Subnets{
				PublicList:  []intent.SubnetSpec{{CIDR: "10.0.1.0/24", AZ: "ap-southeast-2a"}},
				PrivateList: []intent.SubnetSpec{{CIDR: "10.0.2.0/24", AZ: "ap-southeast-2a"}},
			},
			DataPath: &intent.DataPathSpec{
				External: intent.SubnetSpec{CIDR: "10.0.10.0/24", AZ: "ap-southeast-2a"},
				Internal: intent.SubnetSpec{CIDR: "10.0.20.0/24", AZ: "ap-southeast-2a"},
			},
		},
		Pattern: "host-device",
	}
}

func TestRegistered(t *testing.T) {
	s := scenarios.Find("multi-vip")
	if s == nil {
		t.Fatal("multi-vip not registered — init() not called?")
	}
	if s.Rating() != scenarios.Green {
		t.Errorf("rating = %q, want green", s.Rating())
	}
	if len(s.Dependencies()) != 0 {
		t.Errorf("dependencies = %v, want empty", s.Dependencies())
	}
}

func TestManifestsRendered(t *testing.T) {
	dir := t.TempDir()
	cl := makeCluster()

	sctx := &scenarios.Context{
		Ctx:          context.Background(),
		Cluster:      cl,
		Out:          io.Discard,
		WorkspaceDir: dir,
		Options:      map[string]string{},
	}

	s := scenarios.Find("multi-vip")
	if s == nil {
		t.Fatal("scenario not registered")
	}

	paths, err := s.Manifests(sctx)
	if err != nil {
		t.Fatalf("Manifests: %v", err)
	}
	if len(paths) != 5 {
		t.Errorf("expected 5 manifest paths, got %d: %v", len(paths), paths)
	}

	for _, p := range paths {
		if p == "" {
			t.Error("empty path in manifest list")
			continue
		}
		rawContent, readErr := readFileHelper(p)
		if readErr != nil {
			t.Fatalf("reading %s: %v", p, readErr)
		}
		content := string(rawContent)
		// No leftover template directives.
		if strings.Contains(content, "{{") || strings.Contains(content, "}}") {
			t.Errorf("manifest %s still contains template directives:\n%s", p, content)
		}

		// The two Gateways must each pin a distinct VIP from the shared pool.
		if strings.HasSuffix(p, "04-gateways.yaml") {
			for _, want := range []string{
				"scn-mv-gateway-a",
				"scn-mv-gateway-b",
				"10.0.10.115", // VIP A last octet .115
				"10.0.10.116", // VIP B last octet .116
			} {
				if !strings.Contains(content, want) {
					t.Errorf("04-gateways.yaml missing %q:\n%s", want, content)
				}
			}
		}

		// GatewaySettings binds both Gateways to the Infra listener pool.
		if strings.HasSuffix(p, "02-gatewaysettings.yaml") {
			for _, want := range []string{
				"kind: GatewaySettings",
				"name: listener-pool",
				"name: ext-vlan",
			} {
				if !strings.Contains(content, want) {
					t.Errorf("02-gatewaysettings.yaml missing %q:\n%s", want, content)
				}
			}
		}

		// Both routes + hostnames present.
		if strings.HasSuffix(p, "05-httproutes.yaml") {
			for _, want := range []string{
				"scn-mv-route-a",
				"scn-mv-route-b",
				"multivip-a.local",
				"multivip-b.local",
			} {
				if !strings.Contains(content, want) {
					t.Errorf("05-httproutes.yaml missing %q:\n%s", want, content)
				}
			}
		}
	}
}

// TestVerifyCallOrder guards the load-bearing step order in Verify:
//
//	waitDeploymentAvailable(mv-a) → waitDeploymentAvailable(mv-b) →
//	waitCondition(Programmed)x2 → waitHTTPRouteCondition(Accepted)x2 →
//	ResyncHTTPRoutes → RunBodyProbes(VIPA) → RunBodyProbes(VIPB)
func TestVerifyCallOrder(t *testing.T) {
	var calls []string

	deps := multivip.VerifyDeps{
		WaitDeploymentAvailableFn: func(_ context.Context, _ *scenarios.Context, _, name string, _ time.Duration) error {
			calls = append(calls, "waitDeploymentAvailable("+name+")")
			return nil
		},
		WaitConditionFn: func(_ context.Context, _ *scenarios.Context, _ schema.GroupVersionResource, _, name, condType string, _ time.Duration) error {
			calls = append(calls, "waitCondition("+name+","+condType+")")
			return nil
		},
		WaitHTTPRouteConditionFn: func(_ context.Context, _ *scenarios.Context, _, name, condType string, _ time.Duration) error {
			calls = append(calls, "waitHTTPRouteCondition("+name+","+condType+")")
			return nil
		},
		ResyncHTTPRoutesFn: func(_ context.Context, _ *scenarios.Context, _ string) error {
			calls = append(calls, "ResyncHTTPRoutes")
			return nil
		},
		RunBodyProbesFn: func(_ context.Context, _ *scenarios.Context, vip, host string, _ int, _ time.Duration) (string, string) {
			calls = append(calls, "RunBodyProbes("+vip+","+host+")")
			// Return a body containing the marker matching the host so both
			// per-VIP assertions pass.
			if host == "multivip-a.local" {
				return "multivip-backend-a", "stub"
			}
			return "multivip-backend-b", "stub"
		},
	}

	s := multivip.NewScenarioForTest(deps)
	cl := makeCluster()
	dir := t.TempDir()
	st, _ := state.Load(dir)
	st.Set("JUMPHOST_INSTANCE_ID", "i-test")
	st.Set("JUMPHOST_BNK_EXT_ENI_IP", "10.0.10.200")

	sctx := &scenarios.Context{
		Ctx:          context.Background(),
		Cluster:      cl,
		State:        st,
		Out:          io.Discard,
		WorkspaceDir: dir,
		Options:      map[string]string{"vip": "10.0.10.100"},
	}

	res := s.Verify(sctx)
	if !res.AllPassed() {
		t.Fatalf("Verify did not pass with all-true stubs: %s", res.Summary)
	}

	want := []string{
		"waitDeploymentAvailable(mv-a)",
		"waitDeploymentAvailable(mv-b)",
		"waitCondition(scn-mv-gateway-a,Programmed)",
		"waitCondition(scn-mv-gateway-b,Programmed)",
		"waitHTTPRouteCondition(scn-mv-route-a,Accepted)",
		"waitHTTPRouteCondition(scn-mv-route-b,Accepted)",
		"ResyncHTTPRoutes",
		"RunBodyProbes(10.0.10.115,multivip-a.local)",
		"RunBodyProbes(10.0.10.116,multivip-b.local)",
	}

	if len(calls) != len(want) {
		t.Fatalf("call sequence = %v, want %v", calls, want)
	}
	for i, got := range calls {
		if got != want[i] {
			t.Errorf("calls[%d] = %q, want %q", i, got, want[i])
		}
	}
}
