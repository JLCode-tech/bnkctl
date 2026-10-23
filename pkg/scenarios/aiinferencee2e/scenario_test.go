package aiinferencee2e_test

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
	_ "github.com/JLCode-tech/bnkctl/pkg/scenarios/aiinferencee2e"

	"github.com/JLCode-tech/bnkctl/pkg/scenarios/aiinferencee2e"
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
	s := scenarios.Find("ai-inference-e2e")
	if s == nil {
		t.Fatal("ai-inference-e2e not registered — init() not called?")
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

	s := scenarios.Find("ai-inference-e2e")
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
		// vLLM manifest must request the GPU resource and carry the nodeSelector.
		if strings.HasSuffix(p, "03-vllm.yaml") {
			if !strings.Contains(content, "nvidia.com/gpu") {
				t.Errorf("03-vllm.yaml missing nvidia.com/gpu resource request:\n%s", content)
			}
			if !strings.Contains(content, "awsbnkctl.io/gpu") {
				t.Errorf("03-vllm.yaml missing awsbnkctl.io/gpu nodeSelector:\n%s", content)
			}
			if !strings.Contains(content, "meta-llama/Meta-Llama-3-8B-Instruct") {
				t.Errorf("03-vllm.yaml missing Llama-3-8B-Instruct model arg:\n%s", content)
			}
			if !strings.Contains(content, "NoSchedule") {
				t.Errorf("03-vllm.yaml missing nvidia.com/gpu NoSchedule toleration:\n%s", content)
			}
		}
		// HTTPRoute must attach to the scenario gateway and target vllm backend.
		if strings.HasSuffix(p, "05-httproute.yaml") {
			if !strings.Contains(content, "scn-aiinference-gateway") {
				t.Errorf("05-httproute.yaml missing parentRef scn-aiinference-gateway:\n%s", content)
			}
			if !strings.Contains(content, "name: vllm") {
				t.Errorf("05-httproute.yaml missing backendRef name: vllm:\n%s", content)
			}
		}
	}
}

// TestVerifyOrder_AllGreen asserts that when all fn-pointer stubs succeed
// the result is ok with 5 passing assertions (Available + Programmed +
// Accepted + HTTP200 + SSE).
func TestVerifyOrder_AllGreen(t *testing.T) {
	dir := t.TempDir()
	cl := makeCluster()
	callOrder := make([]string, 0, 5)

	deps := aiinferencee2e.VerifyDeps{
		WaitDeploymentAvailableFn: func(_ context.Context, _ *scenarios.Context, ns, name string, _ time.Duration) error {
			callOrder = append(callOrder, "WaitDeploymentAvailable:"+ns+"/"+name)
			return nil
		},
		WaitConditionFn: func(_ context.Context, _ *scenarios.Context, _ schema.GroupVersionResource, ns, name, condType string, _ time.Duration) error {
			callOrder = append(callOrder, "WaitCondition:"+name+"/"+condType)
			return nil
		},
		WaitHTTPRouteConditionFn: func(_ context.Context, _ *scenarios.Context, ns, name, condType string, _ time.Duration) error {
			callOrder = append(callOrder, "WaitHTTPRouteCondition:"+name+"/"+condType)
			return nil
		},
		RunVLLMSSEProbeFn: func(_ context.Context, _ *scenarios.Context, vip string) (bool, bool, string) {
			callOrder = append(callOrder, "RunVLLMSSEProbe:"+vip)
			return true, true, "HTTP 200 — SSE data: found=true, [DONE] found=true"
		},
	}

	s := aiinferencee2e.NewScenarioForTest(deps)

	sctx := &scenarios.Context{
		Ctx:          context.Background(),
		Cluster:      cl,
		Out:          io.Discard,
		WorkspaceDir: dir,
		Options:      map[string]string{},
	}

	result := s.Verify(sctx)

	if result.Status != "ok" {
		t.Errorf("status = %q, want ok; summary: %s", result.Status, result.Summary)
	}
	if len(result.Assertions) != 5 {
		t.Errorf("assertions count = %d, want 5", len(result.Assertions))
	}
	for _, a := range result.Assertions {
		if !a.OK {
			t.Errorf("assertion %q failed: %s", a.Description, a.Got)
		}
	}

	// Verify call order: WaitDeploymentAvailable must come before WaitCondition
	// (Gateway), which must come before WaitHTTPRouteCondition, which must come
	// before RunVLLMSSEProbe.
	if len(callOrder) != 4 {
		t.Fatalf("expected 4 calls, got %d: %v", len(callOrder), callOrder)
	}
	if !strings.HasPrefix(callOrder[0], "WaitDeploymentAvailable") {
		t.Errorf("call[0] = %q, want WaitDeploymentAvailable", callOrder[0])
	}
	if !strings.HasPrefix(callOrder[1], "WaitCondition") {
		t.Errorf("call[1] = %q, want WaitCondition (Gateway Programmed)", callOrder[1])
	}
	if !strings.HasPrefix(callOrder[2], "WaitHTTPRouteCondition") {
		t.Errorf("call[2] = %q, want WaitHTTPRouteCondition", callOrder[2])
	}
	if !strings.HasPrefix(callOrder[3], "RunVLLMSSEProbe") {
		t.Errorf("call[3] = %q, want RunVLLMSSEProbe", callOrder[3])
	}
}

// TestVerifyOrder_DeploymentFails_ShortCircuits asserts that when the vLLM
// Deployment fails to become Available, Verify short-circuits before the
// HTTP probe so the result has only 1 assertion (and is failed).
func TestVerifyOrder_DeploymentFails_ShortCircuits(t *testing.T) {
	dir := t.TempDir()
	cl := makeCluster()
	probeInvoked := false

	deps := aiinferencee2e.VerifyDeps{
		WaitDeploymentAvailableFn: func(_ context.Context, _ *scenarios.Context, _, _ string, _ time.Duration) error {
			return context.DeadlineExceeded
		},
		WaitConditionFn: func(_ context.Context, _ *scenarios.Context, _ schema.GroupVersionResource, _, _, _ string, _ time.Duration) error {
			return nil
		},
		WaitHTTPRouteConditionFn: func(_ context.Context, _ *scenarios.Context, _, _, _ string, _ time.Duration) error {
			return nil
		},
		RunVLLMSSEProbeFn: func(_ context.Context, _ *scenarios.Context, _ string) (bool, bool, string) {
			probeInvoked = true
			return true, true, "should not be called"
		},
	}

	s := aiinferencee2e.NewScenarioForTest(deps)

	sctx := &scenarios.Context{
		Ctx:          context.Background(),
		Cluster:      cl,
		Out:          io.Discard,
		WorkspaceDir: dir,
		Options:      map[string]string{},
	}

	result := s.Verify(sctx)

	if result.Status != "failed" {
		t.Errorf("status = %q, want failed", result.Status)
	}
	if len(result.Assertions) != 1 {
		t.Errorf("assertions count = %d, want 1 (short-circuit after Deployment failure)", len(result.Assertions))
	}
	if probeInvoked {
		t.Error("RunVLLMSSEProbeFn was invoked despite short-circuit — must not be called when Deployment is down")
	}
}

// TestVerifyPoll_EventuallySucceeds asserts that when the probe returns non-200
// on the first K calls and then 200, the verify result records the HTTP-200
// assertion as OK (the poll succeeded). ProbeTimeout/ProbeInterval are set to
// tiny values so the test completes in milliseconds.
func TestVerifyPoll_EventuallySucceeds(t *testing.T) {
	dir := t.TempDir()
	cl := makeCluster()

	callCount := 0
	deps := aiinferencee2e.VerifyDeps{
		WaitDeploymentAvailableFn: func(_ context.Context, _ *scenarios.Context, _, _ string, _ time.Duration) error {
			return nil
		},
		WaitConditionFn: func(_ context.Context, _ *scenarios.Context, _ schema.GroupVersionResource, _, _, _ string, _ time.Duration) error {
			return nil
		},
		WaitHTTPRouteConditionFn: func(_ context.Context, _ *scenarios.Context, _, _, _ string, _ time.Duration) error {
			return nil
		},
		RunVLLMSSEProbeFn: func(_ context.Context, _ *scenarios.Context, _ string) (bool, bool, string) {
			callCount++
			if callCount < 3 {
				// First two attempts fail.
				return false, false, "HTTP 503 — vLLM not ready yet"
			}
			// Third attempt succeeds.
			return true, true, "HTTP 200 — SSE data: found=true, [DONE] found=true"
		},
		// Tiny timeouts so the test runs in < 100 ms.
		ProbeTimeout:  200 * time.Millisecond,
		ProbeInterval: 10 * time.Millisecond,
	}

	s := aiinferencee2e.NewScenarioForTest(deps)

	sctx := &scenarios.Context{
		Ctx:          context.Background(),
		Cluster:      cl,
		Out:          io.Discard,
		WorkspaceDir: dir,
		Options:      map[string]string{},
	}

	result := s.Verify(sctx)

	if result.Status != "ok" {
		t.Errorf("status = %q, want ok; summary: %s", result.Status, result.Summary)
	}
	// Probe was called at least twice (transient failures) then once more for success.
	if callCount < 3 {
		t.Errorf("probe called %d times, expected at least 3 (2 failures + 1 success)", callCount)
	}

	var http200Assert *scenarios.Assertion
	for i := range result.Assertions {
		a := &result.Assertions[i]
		if strings.Contains(a.Description, "HTTP 200") {
			http200Assert = a
		}
	}
	if http200Assert == nil {
		t.Fatal("HTTP 200 assertion not found in result")
	}
	if !http200Assert.OK {
		t.Errorf("HTTP 200 assertion = failed, want ok (poll should have succeeded on 3rd attempt)")
	}
}

// TestVerifyPoll_NeverSucceeds asserts that when the probe always returns
// non-200, the HTTP-200 assertion is recorded as not-OK after the bounded
// poll, and the test completes quickly (ProbeTimeout is tiny).
func TestVerifyPoll_NeverSucceeds(t *testing.T) {
	dir := t.TempDir()
	cl := makeCluster()

	callCount := 0
	deps := aiinferencee2e.VerifyDeps{
		WaitDeploymentAvailableFn: func(_ context.Context, _ *scenarios.Context, _, _ string, _ time.Duration) error {
			return nil
		},
		WaitConditionFn: func(_ context.Context, _ *scenarios.Context, _ schema.GroupVersionResource, _, _, _ string, _ time.Duration) error {
			return nil
		},
		WaitHTTPRouteConditionFn: func(_ context.Context, _ *scenarios.Context, _, _, _ string, _ time.Duration) error {
			return nil
		},
		RunVLLMSSEProbeFn: func(_ context.Context, _ *scenarios.Context, _ string) (bool, bool, string) {
			callCount++
			return false, false, "HTTP 503 — vLLM never ready"
		},
		// Tiny timeouts so the test completes quickly.
		ProbeTimeout:  50 * time.Millisecond,
		ProbeInterval: 10 * time.Millisecond,
	}

	s := aiinferencee2e.NewScenarioForTest(deps)

	sctx := &scenarios.Context{
		Ctx:          context.Background(),
		Cluster:      cl,
		Out:          io.Discard,
		WorkspaceDir: dir,
		Options:      map[string]string{},
	}

	result := s.Verify(sctx)

	if result.Status != "failed" {
		t.Errorf("status = %q, want failed (probe always non-200)", result.Status)
	}
	if callCount == 0 {
		t.Error("probe was never called")
	}

	var http200Assert *scenarios.Assertion
	for i := range result.Assertions {
		a := &result.Assertions[i]
		if strings.Contains(a.Description, "HTTP 200") {
			http200Assert = a
		}
	}
	if http200Assert == nil {
		t.Fatal("HTTP 200 assertion not found in result")
	}
	if http200Assert.OK {
		t.Errorf("HTTP 200 assertion = ok, want failed (probe never returned 200)")
	}
}

// TestVerifyOrder_SSEFails_HTTP200OK asserts the individual SSE and HTTP200
// assertions are distinct: HTTP200 can pass while SSE framing fails.
func TestVerifyOrder_SSEFails_HTTP200OK(t *testing.T) {
	dir := t.TempDir()
	cl := makeCluster()

	deps := aiinferencee2e.VerifyDeps{
		WaitDeploymentAvailableFn: func(_ context.Context, _ *scenarios.Context, _, _ string, _ time.Duration) error {
			return nil
		},
		WaitConditionFn: func(_ context.Context, _ *scenarios.Context, _ schema.GroupVersionResource, _, _, _ string, _ time.Duration) error {
			return nil
		},
		WaitHTTPRouteConditionFn: func(_ context.Context, _ *scenarios.Context, _, _, _ string, _ time.Duration) error {
			return nil
		},
		RunVLLMSSEProbeFn: func(_ context.Context, _ *scenarios.Context, _ string) (bool, bool, string) {
			// HTTP 200 but no SSE framing (e.g. non-streaming JSON response).
			return true, false, "HTTP 200 — SSE data: found=false, [DONE] found=false"
		},
	}

	s := aiinferencee2e.NewScenarioForTest(deps)

	sctx := &scenarios.Context{
		Ctx:          context.Background(),
		Cluster:      cl,
		Out:          io.Discard,
		WorkspaceDir: dir,
		Options:      map[string]string{},
	}

	result := s.Verify(sctx)

	if result.Status != "failed" {
		t.Errorf("status = %q, want failed (SSE framing absent)", result.Status)
	}

	// Find the HTTP200 and SSE assertions.
	var http200Assert, sseAssert *scenarios.Assertion
	for i := range result.Assertions {
		a := &result.Assertions[i]
		if strings.Contains(a.Description, "HTTP 200") {
			http200Assert = a
		}
		if strings.Contains(a.Description, "SSE") {
			sseAssert = a
		}
	}
	if http200Assert == nil {
		t.Fatal("HTTP 200 assertion not found")
	}
	if !http200Assert.OK {
		t.Errorf("HTTP 200 assertion = failed, want ok (HTTP 200 was returned)")
	}
	if sseAssert == nil {
		t.Fatal("SSE assertion not found")
	}
	if sseAssert.OK {
		t.Errorf("SSE assertion = ok, want failed (no SSE framing)")
	}
}

func TestManifestsRendered_Synthetic(t *testing.T) {
	dir := t.TempDir()
	cl := makeCluster()

	sctx := &scenarios.Context{
		Ctx:          context.Background(),
		Cluster:      cl,
		Out:          io.Discard,
		WorkspaceDir: dir,
		Options: map[string]string{
			"synthetic": "true",
		},
	}

	s := scenarios.Find("ai-inference-e2e")
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
		// vLLM manifest must use synthetic simulation and omit nvidia.com/gpu.
		if strings.HasSuffix(p, "03-vllm.yaml") {
			if strings.Contains(content, "nvidia.com/gpu") {
				t.Errorf("03-vllm.yaml should not contain nvidia.com/gpu in synthetic mode:\n%s", content)
			}
			if strings.Contains(content, "awsbnkctl.io/gpu") {
				t.Errorf("03-vllm.yaml should not contain awsbnkctl.io/gpu in synthetic mode:\n%s", content)
			}
			for _, want := range []string{intent.DefaultSyntheticImage, "--model=NousResearch/Meta-Llama-3.1-8B-Instruct", "--inter-token-latency=4.6ms", "--zmq-endpoint=" + aiinferencee2e.SimKVEventsEndpoint,
				"--kv-events-replay-endpoint=tcp://*:20081", "name: kv-relay", "image: " + aiinferencee2e.SimKVRelayImage, "name: kv-events\n          containerPort: 20080", "name: vllm-kv-relay", "def serve_publisher",
				"--render-url=http://localhost:8082", "image: " + aiinferencee2e.SimRenderImage, "restartPolicy: Always",
				"--use-vllm-map-event-format"} {
				if !strings.Contains(content, want) {
					t.Errorf("03-vllm.yaml missing %q:\n%s", want, content)
				}
			}
			// The relay script is embedded in a block scalar: every document must still parse.
			for _, doc := range strings.Split(content, "\n---\n") {
				var obj map[string]any
				if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
					t.Errorf("03-vllm.yaml document does not parse: %v\n%s", err, doc)
				}
			}
		}
	}
}

func TestVerify_Synthetic(t *testing.T) {
	dir := t.TempDir()
	cl := makeCluster()

	deps := aiinferencee2e.VerifyDeps{
		WaitDeploymentAvailableFn: func(_ context.Context, _ *scenarios.Context, _, _ string, timeout time.Duration) error {
			if timeout > 5*time.Minute {
				t.Errorf("synthetic mode should have shorter deployment timeout, got %v", timeout)
			}
			return nil
		},
		WaitConditionFn: func(_ context.Context, _ *scenarios.Context, _ schema.GroupVersionResource, _, _, _ string, _ time.Duration) error {
			return nil
		},
		WaitHTTPRouteConditionFn: func(_ context.Context, _ *scenarios.Context, _, _, _ string, _ time.Duration) error {
			return nil
		},
		RunVLLMSSEProbeFn: func(_ context.Context, _ *scenarios.Context, _ string) (bool, bool, string) {
			return true, true, "HTTP 200 — SSE ok"
		},
	}

	s := aiinferencee2e.NewScenarioForTest(deps)
	sctx := &scenarios.Context{
		Ctx:          context.Background(),
		Cluster:      cl,
		Out:          io.Discard,
		WorkspaceDir: dir,
		Options: map[string]string{
			"synthetic": "true",
		},
	}

	result := s.Verify(sctx)
	if result.Status != "ok" {
		t.Errorf("status = %q, want ok", result.Status)
	}
	if !strings.Contains(result.Details, "synthetic GPU") {
		t.Errorf("details should mention synthetic GPU, got %q", result.Details)
	}
}
