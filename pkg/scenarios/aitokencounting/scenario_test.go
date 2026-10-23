package aitokencounting_test

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
	"github.com/JLCode-tech/bnkctl/pkg/scenarios/aitokencounting"
	_ "github.com/JLCode-tech/bnkctl/pkg/scenarios/aitokencounting"
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
	s := scenarios.Find("ai-token-counting")
	if s == nil {
		t.Fatal("ai-token-counting not registered — init() not called?")
	}
	if s.Rating() != scenarios.Amber {
		t.Errorf("rating = %q, want amber", s.Rating())
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

	s := scenarios.Find("ai-token-counting")
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
		// The Gateway manifest must carry the exact AI annotation key.
		if strings.HasSuffix(p, "04-gateway.yaml") {
			if !strings.Contains(content, "k8s.f5.com/ai-token-counting:") {
				t.Errorf("04-gateway.yaml missing k8s.f5.com/ai-token-counting annotation key:\n%s", content)
			}
			if !strings.Contains(content, "infrastructure:") {
				t.Errorf("04-gateway.yaml missing spec.infrastructure block:\n%s", content)
			}
		}
	}
}

// makeVerifyContext returns a minimal Context usable with NewScenarioForTest.
// It has no Kubernetes clients (nil) so any test that reaches a real client
// call will fail — but with stubbed deps no real call is made.
func makeVerifyContext(t *testing.T) *scenarios.Context {
	t.Helper()
	cl := makeCluster()
	return &scenarios.Context{
		Ctx:          context.Background(),
		Cluster:      cl,
		Out:          io.Discard,
		WorkspaceDir: t.TempDir(),
		Options:      map[string]string{"vip": "10.0.10.104"},
	}
}

// noopControl returns a VerifyDeps whose control-plane stubs all succeed,
// whose annotation fn returns (true, "present") without a real client, and
// whose DataPathVerifyFn is nil (offline / Amber mode).
func noopControl() aitokencounting.VerifyDeps {
	return aitokencounting.VerifyDeps{
		WaitDeploymentAvailableFn: func(_ context.Context, _ *scenarios.Context, _, _ string, _ time.Duration) error {
			return nil
		},
		WaitConditionFn: func(_ context.Context, _ *scenarios.Context, _ schema.GroupVersionResource, _, _, _ string, _ time.Duration) error {
			return nil
		},
		WaitHTTPRouteConditionFn: func(_ context.Context, _ *scenarios.Context, _, _, _ string, _ time.Duration) error {
			return nil
		},
		GatewayAnnotationFn: func(_ *scenarios.Context, _, _, _ string) (bool, string) {
			return true, "present"
		},
		// DataPathVerifyFn intentionally nil — offline mode.
	}
}

// TestVerify_OfflineMode confirms that without DataPathVerifyFn the scenario
// records exactly the 4 control-plane assertions and skips data-path entirely.
// The annotation assertion will fail (no real cluster) but that is expected —
// we only check the shape, not all pass.
func TestVerify_OfflineMode(t *testing.T) {
	var cpCalls []string

	deps := noopControl()
	deps.WaitDeploymentAvailableFn = func(_ context.Context, _ *scenarios.Context, _, name string, _ time.Duration) error {
		cpCalls = append(cpCalls, "waitDeployment:"+name)
		return nil
	}
	deps.WaitConditionFn = func(_ context.Context, _ *scenarios.Context, _ schema.GroupVersionResource, _, name, condType string, _ time.Duration) error {
		cpCalls = append(cpCalls, "waitCondition:"+name+"/"+condType)
		return nil
	}
	deps.WaitHTTPRouteConditionFn = func(_ context.Context, _ *scenarios.Context, _, name, condType string, _ time.Duration) error {
		cpCalls = append(cpCalls, "waitHTTPRoute:"+name+"/"+condType)
		return nil
	}

	s := aitokencounting.NewScenarioForTest(deps)
	res := s.Verify(makeVerifyContext(t))

	// Exactly 4 control-plane assertions: deployment, gateway, httproute, annotation.
	if len(res.Assertions) != 4 {
		t.Errorf("offline mode: expected 4 assertions, got %d: %v", len(res.Assertions), res.Assertions)
	}

	// DataPathVerifyFn was nil — no data-path call should be present.
	for _, a := range res.Assertions {
		if strings.Contains(a.Description, "token-metering") || strings.Contains(a.Description, "503") {
			t.Errorf("unexpected data-path assertion in offline mode: %q", a.Description)
		}
	}

	// Details must mention Amber.
	if !strings.Contains(res.Details, "Amber") {
		t.Errorf("Details should mention Amber in offline mode, got: %q", res.Details)
	}

	// 3 control-plane hook calls (annotation is a direct read, not a hook).
	wantCPCalls := []string{
		"waitDeployment:nginx",
		"waitCondition:scn-aitokens-gateway/Programmed",
		"waitHTTPRoute:scn-aitokens-route/Accepted",
	}
	if len(cpCalls) != len(wantCPCalls) {
		t.Fatalf("cp calls = %v, want %v", cpCalls, wantCPCalls)
	}
	for i, got := range cpCalls {
		if got != wantCPCalls[i] {
			t.Errorf("cpCalls[%d] = %q, want %q", i, got, wantCPCalls[i])
		}
	}
}

// TestVerify_LiveMode_DataPathCalled confirms that when DataPathVerifyFn is
// non-nil it is called after the control-plane assertions, and its returned
// assertions are appended to the result.
func TestVerify_LiveMode_DataPathCalled(t *testing.T) {
	deps := noopControl()

	var dataPathVIP string
	deps.DataPathVerifyFn = func(_ context.Context, _ *scenarios.Context, vip string) []scenarios.Assertion {
		dataPathVIP = vip
		return []scenarios.Assertion{
			{Description: "token-metering header present", OK: true, Got: "x-token-usage: 42"},
			{Description: "HTTP 503 under overload", OK: true, Got: "503 + Retry-After: 1"},
		}
	}

	s := aitokencounting.NewScenarioForTest(deps)
	sctx := makeVerifyContext(t)
	res := s.Verify(sctx)

	// 4 control-plane + 2 data-path = 6 total.
	if len(res.Assertions) != 6 {
		t.Errorf("live mode: expected 6 assertions, got %d: %v", len(res.Assertions), res.Assertions)
	}

	if dataPathVIP != "10.0.10.104" {
		t.Errorf("DataPathVerifyFn received VIP %q, want 10.0.10.104", dataPathVIP)
	}

	// Details must mention Green.
	if !strings.Contains(res.Details, "Green") {
		t.Errorf("Details should mention Green in live mode, got: %q", res.Details)
	}

	// All assertions passed → result should be ok.
	if res.Status != "ok" {
		t.Errorf("status = %q, want ok when all assertions pass", res.Status)
	}
}

// TestVerify_LiveMode_SkippedWhenControlPlaneFails confirms that
// DataPathVerifyFn is NOT called when a control-plane assertion fails (to
// avoid misleading errors from a broken control plane masking data-path).
func TestVerify_LiveMode_SkippedWhenControlPlaneFails(t *testing.T) {
	deps := noopControl()
	deps.WaitDeploymentAvailableFn = func(_ context.Context, _ *scenarios.Context, _, _ string, _ time.Duration) error {
		return context.DeadlineExceeded // simulate control-plane failure
	}

	dataPathCalled := false
	deps.DataPathVerifyFn = func(_ context.Context, _ *scenarios.Context, _ string) []scenarios.Assertion {
		dataPathCalled = true
		return nil
	}

	s := aitokencounting.NewScenarioForTest(deps)
	s.Verify(makeVerifyContext(t))

	if dataPathCalled {
		t.Error("DataPathVerifyFn must not be called when control-plane assertions fail")
	}
}
