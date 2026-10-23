package httptrafficsplit_test

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
	"github.com/JLCode-tech/bnkctl/pkg/scenarios/httptrafficsplit"
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
	s := scenarios.Find("http-traffic-split")
	if s == nil {
		t.Fatal("http-traffic-split not registered — init() not called?")
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

	s := scenarios.Find("http-traffic-split")
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
		// The weighted HTTPRoute must carry both weights.
		if strings.HasSuffix(p, "05-httproute.yaml") {
			if !strings.Contains(content, "weight: 70") {
				t.Errorf("05-httproute.yaml missing weight 70:\n%s", content)
			}
			if !strings.Contains(content, "weight: 30") {
				t.Errorf("05-httproute.yaml missing weight 30:\n%s", content)
			}
		}
	}
}

// TestVerifyCallOrder guards the load-bearing step order in Verify:
//
//	waitDeploymentAvailable(backend-a) → waitDeploymentAvailable(backend-b) →
//	waitCondition(Programmed) → waitHTTPRouteCondition(Accepted) →
//	waitHTTPRouteCondition(ResolvedRefs) → ResyncHTTPRoutes → RunBodyProbes
func TestVerifyCallOrder(t *testing.T) {
	var calls []string

	deps := httptrafficsplit.VerifyDeps{
		WaitDeploymentAvailableFn: func(_ context.Context, _ *scenarios.Context, _, name string, _ time.Duration) error {
			calls = append(calls, "waitDeploymentAvailable("+name+")")
			return nil
		},
		WaitConditionFn: func(_ context.Context, _ *scenarios.Context, _ schema.GroupVersionResource, _, _, condType string, _ time.Duration) error {
			calls = append(calls, "waitCondition("+condType+")")
			return nil
		},
		WaitHTTPRouteConditionFn: func(_ context.Context, _ *scenarios.Context, _, _, condType string, _ time.Duration) error {
			calls = append(calls, "waitHTTPRouteCondition("+condType+")")
			return nil
		},
		ResyncHTTPRoutesFn: func(_ context.Context, _ *scenarios.Context, _ string) error {
			calls = append(calls, "ResyncHTTPRoutes")
			return nil
		},
		RunBodyProbesFn: func(_ context.Context, _ *scenarios.Context, _, _ string, _ int, _ time.Duration) (bool, bool, string) {
			calls = append(calls, "RunBodyProbes")
			return true, true, "recorded"
		},
	}

	s := httptrafficsplit.NewScenarioForTest(deps)
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

	s.Verify(sctx)

	want := []string{
		"waitDeploymentAvailable(backend-a)",
		"waitDeploymentAvailable(backend-b)",
		"waitCondition(Programmed)",
		"waitHTTPRouteCondition(Accepted)",
		"waitHTTPRouteCondition(ResolvedRefs)",
		"ResyncHTTPRoutes",
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
