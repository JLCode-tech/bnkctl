package proxyprotocoll4_test

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
	"github.com/JLCode-tech/bnkctl/pkg/scenarios/proxyprotocoll4"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

const fakeSourceIP = "10.0.10.222"

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

// stateWithSourceIP returns a state.State seeded with the jumphost ENI IP so the
// probe has a concrete client IP to assert against.
func stateWithSourceIP(t *testing.T) *state.State {
	t.Helper()
	st, err := state.Load(t.TempDir())
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Set("JUMPHOST_INSTANCE_ID", "i-test")
	st.Set("JUMPHOST_BNK_EXT_ENI_IP", fakeSourceIP)
	return st
}

func TestRegistered(t *testing.T) {
	s := scenarios.Find("proxy-protocol-l4")
	if s == nil {
		t.Fatal("proxy-protocol-l4 not registered — init() not called?")
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
		State:        stateWithSourceIP(t),
		Out:          io.Discard,
		WorkspaceDir: dir,
		Options:      map[string]string{},
	}

	s := scenarios.Find("proxy-protocol-l4")
	if s == nil {
		t.Fatal("scenario not registered")
	}

	paths, err := s.Manifests(sctx)
	if err != nil {
		t.Fatalf("Manifests: %v", err)
	}
	if len(paths) != 7 {
		t.Errorf("expected 7 manifest paths, got %d: %v", len(paths), paths)
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
		// The L4Route must declare protocol TCP and kind L4Route.
		if strings.HasSuffix(p, "07-l4route.yaml") {
			if !strings.Contains(content, "kind: L4Route") {
				t.Errorf("07-l4route.yaml missing 'kind: L4Route':\n%s", content)
			}
			if !strings.Contains(content, "protocol: TCP") {
				t.Errorf("07-l4route.yaml missing 'protocol: TCP':\n%s", content)
			}
		}
		// The Gateway listener must be protocol TCP (this is L4).
		if strings.HasSuffix(p, "06-gateway.yaml") {
			if !strings.Contains(content, "protocol: TCP") {
				t.Errorf("06-gateway.yaml listener not 'protocol: TCP':\n%s", content)
			}
		}
		// The iRule manifest must carry the PROXY-protocol prepend.
		if strings.HasSuffix(p, "04-irule.yaml") {
			if !strings.Contains(content, "kind: F5BigCneIrule") {
				t.Errorf("04-irule.yaml missing 'kind: F5BigCneIrule':\n%s", content)
			}
			if !strings.Contains(content, "PROXY TCP4") {
				t.Errorf("04-irule.yaml missing 'PROXY TCP4' iRule body:\n%s", content)
			}
		}
		// The NetPolicy must reference the iRule + Gateway.
		if strings.HasSuffix(p, "05-netpolicy.yaml") {
			if !strings.Contains(content, "kind: NetPolicy") {
				t.Errorf("05-netpolicy.yaml missing 'kind: NetPolicy':\n%s", content)
			}
			if !strings.Contains(content, "F5BigCneIrule") {
				t.Errorf("05-netpolicy.yaml missing F5BigCneIrule extensionRef:\n%s", content)
			}
		}
		// The backend nginx conf must enable proxy_protocol.
		if strings.HasSuffix(p, "03-backend.yaml") {
			if !strings.Contains(content, "proxy_protocol") {
				t.Errorf("03-backend.yaml missing 'proxy_protocol' listener config:\n%s", content)
			}
		}
	}
}

// TestVerifyCallOrder guards the load-bearing step order in Verify:
//
//	WaitDeploymentAvailable → waitCondition(Programmed) →
//	waitL4RouteCondition(Accepted) → IrulePresent → NetPolicyPresent →
//	RunBodyProbes
func TestVerifyCallOrder(t *testing.T) {
	var calls []string

	deps := proxyprotocoll4.VerifyDeps{
		WaitDeploymentAvailableFn: func(_ context.Context, _ *scenarios.Context, _, _ string, _ time.Duration) error {
			calls = append(calls, "WaitDeploymentAvailable")
			return nil
		},
		WaitConditionFn: func(_ context.Context, _ *scenarios.Context, _ schema.GroupVersionResource, _, _, condType string, _ time.Duration) error {
			calls = append(calls, "waitCondition("+condType+")")
			return nil
		},
		WaitL4RouteConditionFn: func(_ context.Context, _ *scenarios.Context, _, _, condType string, _ time.Duration) error {
			calls = append(calls, "waitL4RouteCondition("+condType+")")
			return nil
		},
		IrulePresentFn: func(_ context.Context, _ *scenarios.Context, _, _ string) error {
			calls = append(calls, "IrulePresent")
			return nil
		},
		NetPolicyPresentFn: func(_ context.Context, _ *scenarios.Context, _, _ string) error {
			calls = append(calls, "NetPolicyPresent")
			return nil
		},
		RunBodyProbesFn: func(_ context.Context, _ *scenarios.Context, _, wantIP string, _ int, _ time.Duration) (bool, string) {
			calls = append(calls, "RunBodyProbes")
			if wantIP != fakeSourceIP {
				t.Errorf("RunBodyProbes wantIP = %q, want %q", wantIP, fakeSourceIP)
			}
			return true, "recorded"
		},
	}

	s := proxyprotocoll4.NewScenarioForTest(deps)
	cl := makeCluster()
	dir := t.TempDir()
	st := stateWithSourceIP(t)

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
		t.Errorf("expected all assertions to pass with stubbed deps, summary: %s", res.Summary)
	}

	want := []string{
		"WaitDeploymentAvailable",
		"waitCondition(Programmed)",
		"waitL4RouteCondition(Accepted)",
		"IrulePresent",
		"NetPolicyPresent",
		"RunBodyProbes",
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

// TestVerifyMissingJumphostState asserts the probe is skipped with a clear
// failure when the jumphost state keys are absent.
func TestVerifyMissingJumphostState(t *testing.T) {
	var probed bool
	deps := proxyprotocoll4.VerifyDeps{
		WaitDeploymentAvailableFn: func(_ context.Context, _ *scenarios.Context, _, _ string, _ time.Duration) error { return nil },
		WaitConditionFn: func(_ context.Context, _ *scenarios.Context, _ schema.GroupVersionResource, _, _, _ string, _ time.Duration) error {
			return nil
		},
		WaitL4RouteConditionFn: func(_ context.Context, _ *scenarios.Context, _, _, _ string, _ time.Duration) error { return nil },
		IrulePresentFn:         func(_ context.Context, _ *scenarios.Context, _, _ string) error { return nil },
		NetPolicyPresentFn:     func(_ context.Context, _ *scenarios.Context, _, _ string) error { return nil },
		RunBodyProbesFn: func(_ context.Context, _ *scenarios.Context, _, _ string, _ int, _ time.Duration) (bool, string) {
			probed = true
			return true, "should-not-run"
		},
	}

	st, err := state.Load(t.TempDir())
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}

	s := proxyprotocoll4.NewScenarioForTest(deps)
	sctx := &scenarios.Context{
		Ctx:          context.Background(),
		Cluster:      makeCluster(),
		State:        st,
		Out:          io.Discard,
		WorkspaceDir: t.TempDir(),
		Options:      map[string]string{"vip": "10.0.10.100"},
	}

	res := s.Verify(sctx)
	if res.AllPassed() {
		t.Error("expected failure when jumphost state keys are missing")
	}
	if probed {
		t.Error("RunBodyProbes should not run when jumphost state keys are missing")
	}
}
