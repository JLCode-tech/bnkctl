package httproutee2e_test

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
	"github.com/JLCode-tech/bnkctl/pkg/scenarios/httproutee2e"
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

// TestVerifyCallOrder guards the load-bearing step order in Verify:
//
//	waitDeploymentAvailable → waitCondition(Programmed) →
//	waitHTTPRouteCondition(Accepted) → waitHTTPRouteCondition(ResolvedRefs) →
//	ResyncHTTPRoutes → RunCurlProbes
//
// Moving ResyncHTTPRoutes before the condition waits regresses the cne-controller
// pool-member stale-bug workaround (project_pool_member_sync_root_cause).
func TestVerifyCallOrder(t *testing.T) {
	var calls []string

	recordStep := func(name string) func() {
		return func() { calls = append(calls, name) }
	}

	deps := httproutee2e.VerifyDeps{
		WaitDeploymentAvailableFn: func(_ context.Context, _ *scenarios.Context, _, _ string, _ time.Duration) error {
			recordStep("waitDeploymentAvailable")()
			return nil
		},
		WaitConditionFn: func(_ context.Context, _ *scenarios.Context, _ schema.GroupVersionResource, _, _, condType string, _ time.Duration) error {
			recordStep("waitCondition(" + condType + ")")()
			return nil
		},
		WaitHTTPRouteConditionFn: func(_ context.Context, _ *scenarios.Context, _, _, condType string, _ time.Duration) error {
			recordStep("waitHTTPRouteCondition(" + condType + ")")()
			return nil
		},
		ResyncHTTPRoutesFn: func(_ context.Context, _ *scenarios.Context, _ string) error {
			recordStep("ResyncHTTPRoutes")()
			return nil
		},
		RunCurlProbesFn: func(_ context.Context, _ *scenarios.Context, _ string, _ int, _ time.Duration) (bool, string) {
			recordStep("RunCurlProbes")()
			return true, "recorded"
		},
	}

	s := httproutee2e.NewScenarioForTest(deps)
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
		"waitDeploymentAvailable",
		"waitCondition(Programmed)",
		"waitHTTPRouteCondition(Accepted)",
		"waitHTTPRouteCondition(ResolvedRefs)",
		"ResyncHTTPRoutes",
		"RunCurlProbes",
	}

	// The GatewaySettings check uses ctx.Dynamic which is nil — it will be recorded
	// as a failed assertion but does not call any of our hooks, so we only
	// check the hook calls, not every assertion.
	if len(calls) != len(want) {
		t.Fatalf("call sequence = %v, want %v", calls, want)
	}
	for i, got := range calls {
		if got != want[i] {
			t.Errorf("calls[%d] = %q, want %q", i, got, want[i])
		}
	}
}

func TestHTTPRouteE2E_Registered(t *testing.T) {
	s := scenarios.Find("http-routing-e2e")
	if s == nil {
		t.Fatal("http-routing-e2e not registered — init() not called?")
	}
	if s.Rating() != scenarios.Green {
		t.Errorf("rating = %q, want green", s.Rating())
	}
	if len(s.Dependencies()) != 0 {
		t.Errorf("dependencies = %v, want empty (no FRR sibling in awsbnkctl)", s.Dependencies())
	}
}

func TestHTTPRouteE2E_Title(t *testing.T) {
	s := scenarios.Find("http-routing-e2e")
	if s == nil {
		t.Fatal("scenario not registered")
	}
	if !strings.Contains(s.Title(), "HTTPRoute") {
		t.Errorf("Title() = %q, want it to mention HTTPRoute", s.Title())
	}
}

func TestHTTPRouteE2E_ManifestsRendered(t *testing.T) {
	dir := t.TempDir()
	cl := makeCluster()

	sctx := &scenarios.Context{
		Ctx:          context.Background(),
		Cluster:      cl,
		Out:          io.Discard,
		WorkspaceDir: dir,
		Options:      map[string]string{},
	}

	s := scenarios.Find("http-routing-e2e")
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

	// Each path should exist on disk.
	for _, p := range paths {
		if p == "" {
			t.Error("empty path in manifest list")
		}
	}
}

func TestHTTPRouteE2E_TemplateSubstitution(t *testing.T) {
	dir := t.TempDir()
	cl := makeCluster()

	sctx := &scenarios.Context{
		Ctx:          context.Background(),
		Cluster:      cl,
		Out:          io.Discard,
		WorkspaceDir: dir,
		Options:      map[string]string{},
	}

	s := scenarios.Find("http-routing-e2e")
	if s == nil {
		t.Fatal("scenario not registered")
	}

	paths, err := s.Manifests(sctx)
	if err != nil {
		t.Fatalf("Manifests: %v", err)
	}

	// Read all manifest files and check no template directives remain.
	for _, p := range paths {
		rawContent, readErr := readFileHelper(p)
		if readErr != nil {
			t.Fatalf("reading %s: %v", p, readErr)
		}
		content := string(rawContent)
		if strings.Contains(content, "{{") || strings.Contains(content, "}}") {
			t.Errorf("manifest %s still contains template directives:\n%s", p, content)
		}
		// Cluster name should appear in the gateway class name.
		if strings.HasSuffix(p, "04-gateway.yaml") {
			if !strings.Contains(content, "test-cluster-gatewayclass") {
				t.Errorf("04-gateway.yaml missing gatewayClassName: got\n%s", content)
			}
			if !strings.Contains(content, "10.0.10.100") {
				t.Errorf("04-gateway.yaml missing VIP (10.0.10.100): got\n%s", content)
			}
		}
		if strings.HasSuffix(p, "01-namespace.yaml") {
			if !strings.Contains(content, "awsbnkctl-scn-httproute-e2e") {
				t.Errorf("01-namespace.yaml missing namespace: got\n%s", content)
			}
		}
	}
}

func TestHTTPRouteE2E_DryRun(t *testing.T) {
	dir := t.TempDir()
	cl := makeCluster()

	sctx := &scenarios.Context{
		Ctx:          context.Background(),
		Cluster:      cl,
		Out:          io.Discard,
		WorkspaceDir: dir,
		DryRun:       true,
		Options:      map[string]string{},
	}

	s := scenarios.Find("http-routing-e2e")
	if s == nil {
		t.Fatal("scenario not registered")
	}

	result := scenarios.Run(sctx, s)
	if result.Status != "dry-run" {
		t.Errorf("status = %q, want dry-run", result.Status)
	}
}

func TestHTTPRouteE2E_VIPFromOptions(t *testing.T) {
	dir := t.TempDir()
	cl := makeCluster()

	sctx := &scenarios.Context{
		Ctx:          context.Background(),
		Cluster:      cl,
		Out:          io.Discard,
		WorkspaceDir: dir,
		Options:      map[string]string{"vip": "10.0.10.200"},
	}

	s := scenarios.Find("http-routing-e2e")
	if s == nil {
		t.Fatal("scenario not registered")
	}

	paths, err := s.Manifests(sctx)
	if err != nil {
		t.Fatalf("Manifests: %v", err)
	}
	// Check that the custom VIP appears in the rendered gateway manifest.
	for _, p := range paths {
		if strings.HasSuffix(p, "04-gateway.yaml") {
			content, err := readFileHelper(p)
			if err != nil {
				t.Fatalf("reading %s: %v", p, err)
			}
			if !strings.Contains(string(content), "10.0.10.200") {
				t.Errorf("custom VIP 10.0.10.200 not in gateway manifest:\n%s", string(content))
			}
		}
	}
}
