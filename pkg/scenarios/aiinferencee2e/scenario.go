// Package aiinferencee2e implements scenario "ai-inference-e2e" — deploys a
// vLLM serving Llama-3-8B-Instruct on the GPU nodegroup behind the BNK VIP
// and asserts an HTTP 200 + streamed SSE OpenAI completion through the VIP.
//
// GPU nodegroup prerequisite: the cluster must have a node group with
// gpu: true and the nvidia.com/gpu=present:NoSchedule taint (PRD-11 S1).
// The scenario targets GPU nodes via nodeSelector + toleration.
//
// Llama-3-8B-Instruct is a gated HuggingFace model. Create a Secret
// named "hf-token" in the scenario namespace with key "token" set to a valid
// HF access credential before running:
//
//	kubectl -n awsbnkctl-scn-aiinference create secret generic hf-token \
//	  --from-literal=token=<YOUR_HF_TOKEN>
//
// The Secret is optional=true in the Deployment so the pod starts without it,
// but model pull will fail at runtime if the model requires auth.
//
// Verify order (stubbable via VerifyDeps for offline tests):
//
//  1. Wait vLLM Deployment Available (GPU model load takes several minutes;
//     up to 20 min to absorb a liveness restart during cold model load).
//  2. Wait Gateway scn-aiinference-gateway Programmed=True.
//  3. Wait HTTPRoute scn-aiinference-route Accepted=True (BNK 2.4: and F5EPP
//     vllm-pool-epp Available=True).
//  4. Live HTTP probe via jumphost: POST /v1/chat/completions with stream=true,
//     assert HTTP 200 + SSE framing (data: prefix + [DONE] terminator).
//     The probe is retried for up to 5 minutes (15 s between attempts) so that
//     a just-becoming-ready vLLM HTTP server does not cause a spurious failure.
//
// Rating Green: all four steps exercise real data-plane traffic when live.
// Steps 1-3 are stubbable for offline unit tests.
package aiinferencee2e

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/jumphost"
	k8sapply "github.com/JLCode-tech/bnkctl/pkg/k8s"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/manifests"
	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
)

//go:embed manifests/*.yaml
var manifestFS embed.FS

// simKVRelay is the KV-event relay sidecar script (see SimKVEventsEndpoint).
//
//go:embed sim_kv_relay.py
var simKVRelay string

const (
	scnName      = "ai-inference-e2e"
	scnTitle     = "vLLM Llama-3-8B-Instruct on GPU nodegroup, SSE completions via BNK VIP (Green)"
	scnNamespace = "awsbnkctl-scn-aiinference"
	scnHostname  = "awsbnkctl-aiinference.local"
)

func init() { scenarios.Register(&scenario{}) }

// VerifyDeps holds the function pointers used by Verify. The zero value routes
// every call to the real package-level implementations. Tests swap individual
// fields to a recording stub to assert call order without touching the cluster.
type VerifyDeps struct {
	WaitDeploymentAvailableFn func(ctx context.Context, sctx *scenarios.Context, ns, name string, timeout time.Duration) error
	WaitConditionFn           func(ctx context.Context, sctx *scenarios.Context, gvr schema.GroupVersionResource, ns, name, condType string, timeout time.Duration) error
	WaitHTTPRouteConditionFn  func(ctx context.Context, sctx *scenarios.Context, ns, name, condType string, timeout time.Duration) error
	// RunVLLMSSEProbeFn issues a single streaming POST /v1/chat/completions via
	// the jumphost and returns (HTTP 200 ok, SSE framing ok, detail). The live
	// implementation uses jumphost.RunStagingCommands; tests inject a stub.
	// pollVLLMSSEProbe calls this repeatedly until http200==true or the deadline.
	RunVLLMSSEProbeFn func(ctx context.Context, sctx *scenarios.Context, vip string) (http200 bool, sseOK bool, detail string)

	// ProbeTimeout and ProbeInterval control the outer retry loop in
	// pollVLLMSSEProbe. Zero values fall back to production defaults
	// (5 min / 15 s). Tests set small values to keep suite fast.
	ProbeTimeout  time.Duration
	ProbeInterval time.Duration
}

func realVerifyDeps() VerifyDeps {
	return VerifyDeps{
		WaitDeploymentAvailableFn: scenarios.WaitDeploymentAvailable,
		WaitConditionFn:           scenarios.WaitCondition,
		WaitHTTPRouteConditionFn:  scenarios.WaitHTTPRouteCondition,
		RunVLLMSSEProbeFn:         runVLLMSSEProbe,
	}
}

type scenario struct {
	// vDeps is nil for the registered singleton; tests inject a non-nil value.
	vDeps *VerifyDeps
	// createHFTokenSecretFn is the function used to create the hf-token Secret
	// in Apply. Nil uses the real implementation. Tests may inject a no-op stub.
	createHFTokenSecretFn func(ctx *scenarios.Context, ns, token string) error
}

func (s *scenario) Name() string             { return scnName }
func (s *scenario) Title() string            { return scnTitle }
func (s *scenario) Rating() scenarios.Rating { return scenarios.Green }
func (s *scenario) Dependencies() []string   { return []string{} }
func (s *scenario) Description() string {
	return strings.TrimSpace(`
AI inference scenario: vLLM serving Llama-3-8B-Instruct on the GPU nodegroup
behind the BNK VIP (Green — full data-plane).

Applies 5 templated manifests into the scenario namespace:
  Namespace, GatewaySettings (listener context),
  vLLM Deployment (GPU nodeSelector + nvidia.com/gpu taint toleration,
  nvidia.com/gpu: "1", serves meta-llama/Meta-Llama-3-8B-Instruct) + Service,
  Gateway (spec.addresses=[VIP]), HTTPRoute (-> vllm:80).
  BNK 2.4: the route's backend is InferencePool vllm-pool, fronted by F5EPP
  vllm-pool-epp (the F5 Endpoint Picker); the Inference Extension CRDs are
  installed when absent.

HuggingFace auth: create a Secret named "hf-token" with key "token" before
running if the model is gated. The Secret is optional in the Deployment.

Verify order:
  1. Wait vLLM Deployment Available (GPU model load -- up to 20 min).
  2. Wait Gateway scn-aiinference-gateway Programmed=True.
  3. Wait HTTPRoute scn-aiinference-route Accepted=True (BNK 2.4: and F5EPP
     vllm-pool-epp Available=True).
  4. POST /v1/chat/completions (stream=true) via jumphost curl through the VIP;
     assert HTTP 200 + SSE framing (data: chunks + [DONE] terminator).
     Retried for up to 5 min (15 s between attempts) for cold-start resilience.

Rating Green: steps 1-3 are stubbable; step 4 exercises the real data path.

Cleanup: delete the scenario namespace (idempotent).
`)
}

// SimKVEventsEndpoint is where the simulator publishes KV-cache events. The simulator
// connects out while vLLM listens, so a relay sidecar takes the events here and serves
// them on :20080 like vLLM; the simulator serves replay on :20081 itself.
const SimKVEventsEndpoint = "tcp://127.0.0.1:5557"

// SimKVRelayImage runs the KV-event relay sidecar (stdlib Python, no packages).
const SimKVRelayImage = "python:3.12-slim"

// SimRenderImage runs vLLM's render server next to the simulator, so the simulator tokenizes
// like vLLM (CPU build; tokenizer and chat template only).
const SimRenderImage = "vllm/vllm-openai-cpu:v0.21.0"

// manifestVars holds the template variables for the 5 manifests.
type manifestVars struct {
	Namespace        string
	ClusterName      string
	GatewayClassName string
	VIP              string
	ExternalCIDR     string
	Synthetic        bool
	// Sim is ai.synthetic with defaults applied (synthetic mode only).
	Sim intent.SyntheticSpec
	// KVEventsEndpoint is where the simulator publishes KV-cache events (the relay sidecar).
	KVEventsEndpoint string
	// KVRelayImage, KVRelayScript (indented for the ConfigMap) and KVRelaySHA (pod
	// annotation, so a script change rolls the pods) describe the relay sidecar.
	KVRelayImage  string
	KVRelayScript string
	KVRelaySHA    string
	// RenderImage is the vLLM render sidecar the simulator tokenizes with.
	RenderImage string
	// EPP routes through the F5 Endpoint Picker (BNK 2.4+): F5EPP + InferencePool, the
	// HTTPRoute backend is the pool.
	EPP bool
	// TokenizerModel is the HF repo the F5 EPP loads the tokenizer from.
	TokenizerModel string
}

func (s *scenario) Manifests(ctx *scenarios.Context) ([]string, error) {
	v, err := buildManifestVars(ctx)
	if err != nil {
		return nil, err
	}

	var paths []string
	err = fs.WalkDir(manifestFS, "manifests", func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return werr
		}
		tmplBytes, e := manifestFS.ReadFile(p)
		if e != nil {
			return e
		}
		rendered, e := scenarios.RenderTemplate(string(tmplBytes), v)
		if e != nil {
			return fmt.Errorf("rendering %s: %w", p, e)
		}
		base := p[len("manifests/"):]
		out, e := scenarios.WriteManifest(ctx.WorkspaceDir, scnName, base, rendered)
		if e != nil {
			return e
		}
		paths = append(paths, out)
		return nil
	})
	return paths, err
}

func (s *scenario) Apply(ctx *scenarios.Context) error {
	ns := namespace(ctx)
	// Ensure the namespace exists before creating any namespace-scoped resources
	// (e.g. the hf-token Secret). The namespace manifest (01-namespace.yaml) is
	// applied by ApplyManifests below, but that happens after the Secret creation
	// call — so we pre-create it here idempotently.
	if err := ensureNamespace(ctx, ns); err != nil {
		return fmt.Errorf("ensuring namespace %s: %w", ns, err)
	}

	if isSynthetic(ctx) {
		fmt.Fprintf(ctx.Out, "[ai-inference-e2e] running in synthetic GPU mode (llm-d-inference-sim) — skipping hf-token Secret creation\n")
	} else {
		// Create the hf-token Secret from HF_TOKEN before applying the vLLM Deployment.
		// This is skipped gracefully when HF_TOKEN is unset (offline / public-model path).
		createFn := s.createHFTokenSecretFn
		if createFn == nil {
			createFn = createHFTokenSecret
		}
		token := os.Getenv("HF_TOKEN")
		if token != "" {
			if err := createFn(ctx, ns, token); err != nil {
				return fmt.Errorf("creating hf-token Secret: %w", err)
			}
		} else {
			fmt.Fprintf(ctx.Out, "[ai-inference-e2e] HF_TOKEN not set — skipping hf-token Secret creation (gated models will fail to pull)\n")
		}
	}
	if eppEnabled(ctx) {
		if err := ensureGIECRDs(ctx); err != nil {
			return fmt.Errorf("installing Gateway API Inference Extension CRDs: %w", err)
		}
		if err := ensureFARSecret(ctx, ns); err != nil {
			return fmt.Errorf("copying the FAR pull secret for the F5 EPP image: %w", err)
		}
	}
	return scenarios.ApplyManifests(ctx, scnName)
}

// ensureNamespace creates the given namespace if it does not already exist.
// It is a no-op when ctx.Clientset is nil (offline / unit-test path).
func ensureNamespace(ctx *scenarios.Context, ns string) error {
	if ctx.Clientset == nil {
		return nil
	}
	obj := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: ns,
		},
	}
	_, err := ctx.Clientset.CoreV1().Namespaces().Create(ctx.Ctx, obj, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// createHFTokenSecret is the real implementation: creates (or updates) a
// Kubernetes Secret named "hf-token" in ns with key "token" = token.
// It is skipped when ctx.Clientset is nil (no cluster connection available).
func createHFTokenSecret(ctx *scenarios.Context, ns, token string) error {
	if ctx.Clientset == nil {
		fmt.Fprintf(ctx.Out, "[ai-inference-e2e] no Clientset available — skipping hf-token Secret creation\n")
		return nil
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "hf-token",
			Namespace: ns,
		},
		StringData: map[string]string{
			"token": token,
		},
	}
	_, err := ctx.Clientset.CoreV1().Secrets(ns).Create(ctx.Ctx, secret, metav1.CreateOptions{})
	if err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create Secret hf-token in %s: %w", ns, err)
		}
		// Secret already exists — update it so a token rotation takes effect.
		_, err = ctx.Clientset.CoreV1().Secrets(ns).Update(ctx.Ctx, secret, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("update Secret hf-token in %s: %w", ns, err)
		}
		fmt.Fprintf(ctx.Out, "[ai-inference-e2e] updated hf-token Secret in namespace %s\n", ns)
		return nil
	}
	fmt.Fprintf(ctx.Out, "[ai-inference-e2e] created hf-token Secret in namespace %s\n", ns)
	return nil
}

func (s *scenario) Verify(ctx *scenarios.Context) scenarios.Result {
	d := s.vDeps
	if d == nil {
		real := realVerifyDeps()
		d = &real
	}
	ns := namespace(ctx)
	synthetic := isSynthetic(ctx)
	details := "Green: full data-plane — vLLM must be running on a GPU node and reachable through the BNK VIP."
	if synthetic {
		details = "Green: full data-plane (synthetic GPU) — llm-d-inference-sim running on CPU node and reachable through the BNK VIP."
	}
	res := scenarios.Result{
		DataPath: true,
		Details:  details,
	}

	// 1. vLLM Deployment Available (GPU model load can take several minutes;
	//    synthetic starts in seconds).
	deployTimeout := 20 * time.Minute
	if synthetic {
		deployTimeout = 2 * time.Minute
	}
	err := d.WaitDeploymentAvailableFn(ctx.Ctx, ctx, ns, "vllm", deployTimeout)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "vLLM Deployment Available",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})
	if err != nil {
		// Short-circuit: the HTTP probe is meaningless if the backend is down.
		return scenarios.FinalizeResult(res)
	}

	// 2. Gateway Programmed=True.
	err = d.WaitConditionFn(ctx.Ctx, ctx, scenarios.GatewayGVR, ns, "scn-aiinference-gateway", "Programmed", 5*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "Gateway scn-aiinference-gateway Programmed=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// 3. HTTPRoute Accepted=True.
	err = d.WaitHTTPRouteConditionFn(ctx.Ctx, ctx, ns, "scn-aiinference-route", "Accepted", 3*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "HTTPRoute scn-aiinference-route Accepted=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// 3b. BNK 2.4: the F5 Endpoint Picker the InferencePool consults is up.
	if eppEnabled(ctx) {
		err = d.WaitConditionFn(ctx.Ctx, ctx, f5EPPGVR, ns, "vllm-pool-epp", "Available", 5*time.Minute)
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "F5EPP vllm-pool-epp Available=True",
			OK:          err == nil,
			Got:         scenarios.ErrString(err),
		})
	}

	// 4. Live SSE probe: POST /v1/chat/completions (stream=true) through VIP.
	// Retry until HTTP 200 or the deadline — vLLM's HTTP server may not be
	// serving immediately after the Deployment reports Available.
	vip := vipFromCtx(ctx)
	http200, sseOK, detail := pollVLLMSSEProbe(ctx.Ctx, ctx, d, vip)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "POST /v1/chat/completions returns HTTP 200",
		OK:          http200,
		Got:         detail,
	})
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "response carries SSE framing (data: chunk + [DONE])",
		OK:          sseOK,
		Got:         detail,
	})

	return scenarios.FinalizeResult(res)
}

func (s *scenario) Cleanup(ctx *scenarios.Context) error {
	ns := namespace(ctx)
	err := ctx.Clientset.CoreV1().Namespaces().Delete(ctx.Ctx, ns, metav1.DeleteOptions{})
	if err != nil && !scenarios.IsNotFound(err) {
		return fmt.Errorf("deleting namespace %s: %w", ns, err)
	}
	return nil
}

func (s *scenario) Namespace(ctx *scenarios.Context) string { return namespace(ctx) }

// --- internal helpers ---

func namespace(ctx *scenarios.Context) string {
	if v := ctx.Options["namespace"]; v != "" {
		return v
	}
	return scnNamespace
}

// vipFromCtx returns the VIP with the ai-inference octet (.112) from options
// or derived from the cluster's default VIP. Empty string means probe will degrade.
func vipFromCtx(ctx *scenarios.Context) string {
	if vip := ctx.Options["vip"]; vip != "" {
		return vip
	}
	if ctx.Cluster != nil {
		v, err := ctx.Cluster.DefaultVIP()
		if err == nil {
			return withLastOctet(v, "112")
		}
	}
	return ""
}

// withLastOctet returns ip with its last dotted-quad octet replaced by octet.
// If ip is not a valid dotted-quad, it is returned unchanged.
func withLastOctet(ip, octet string) string {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return ip
	}
	parts[3] = octet
	return strings.Join(parts, ".")
}

// isSynthetic reports whether the scenario should run in synthetic GPU mode
// using llm-d-inference-sim rather than real GPU-backed vLLM.
func isSynthetic(ctx *scenarios.Context) bool {
	if ctx == nil {
		return false
	}
	if v, ok := ctx.Options["synthetic"]; ok {
		return strings.EqualFold(v, "true") || v == "1"
	}
	return ctx.Cluster.UsesSyntheticAI()
}

func buildManifestVars(ctx *scenarios.Context) (manifestVars, error) {
	var v manifestVars
	v.Namespace = namespace(ctx)
	if ctx.Cluster != nil {
		v.ClusterName = ctx.Cluster.Metadata.Name
		v.GatewayClassName = ctx.Cluster.Metadata.Name + "-gatewayclass"
		if ctx.Cluster.Network.DataPath != nil {
			v.ExternalCIDR = ctx.Cluster.Network.DataPath.External.CIDR
		}
	}
	vip := ctx.Options["vip"]
	if vip == "" && ctx.Cluster != nil {
		var err error
		vip, err = ctx.Cluster.DefaultVIP()
		if err != nil {
			return v, fmt.Errorf("deriving VIP: %w", err)
		}
	}
	if vip == "" {
		return v, fmt.Errorf("VIP not derivable — set network.dataPath.external.cidr in cluster.yaml or pass --vip")
	}
	// Use .112 — grpc-loadbalance owns .108 (see docs/SCENARIOS.md, "VIP plan").
	v.VIP = withLastOctet(vip, strconv.Itoa(112))

	v.Synthetic = isSynthetic(ctx)
	// Ungated copy of the Llama-3-8B tokenizer the GPU branch serves.
	v.TokenizerModel = "NousResearch/Meta-Llama-3-8B-Instruct"
	if v.Synthetic {
		v.Sim = ctx.Cluster.ResolvedSynthetic()
		v.KVEventsEndpoint = SimKVEventsEndpoint
		v.KVRelayImage = SimKVRelayImage
		v.RenderImage = SimRenderImage
		v.KVRelayScript = indent(simKVRelay, "    ")
		sum := sha256.Sum256([]byte(simKVRelay))
		v.KVRelaySHA = hex.EncodeToString(sum[:])[:12]
		v.TokenizerModel = v.Sim.Model
	}
	v.EPP = eppEnabled(ctx)

	return v, nil
}

// pollVLLMSSEProbe retries d.RunVLLMSSEProbeFn until http200 is true or the
// deadline elapses. It returns the last attempt's values so the caller always
// gets a meaningful detail string regardless of whether the poll succeeded.
//
// Production defaults: 5 min total, 15 s between attempts.
// Tests override via d.ProbeTimeout / d.ProbeInterval.
func pollVLLMSSEProbe(ctx context.Context, sctx *scenarios.Context, d *VerifyDeps, vip string) (http200 bool, sseOK bool, detail string) {
	timeout := d.ProbeTimeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	interval := d.ProbeInterval
	if interval == 0 {
		interval = 15 * time.Second
	}

	deadline := time.Now().Add(timeout)
	for {
		http200, sseOK, detail = d.RunVLLMSSEProbeFn(ctx, sctx, vip)
		if http200 {
			return http200, sseOK, detail
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			// Deadline elapsed — return last attempt's values.
			return http200, sseOK, detail
		}
		sleep := interval
		if sleep > remaining {
			sleep = remaining
		}
		select {
		case <-ctx.Done():
			return http200, sseOK, detail
		case <-time.After(sleep):
		}
	}
}

// runVLLMSSEProbe issues a single streaming POST /v1/chat/completions via the
// jumphost EICE tunnel using jumphost.RunStagingCommands. It is the live
// implementation of RunVLLMSSEProbeFn.
//
// The remote curl command:
//   - Uses --no-buffer / -N to prevent output buffering so SSE chunks arrive
//     without buffering.
//   - Sends Accept: text/event-stream so the server knows to stream SSE.
//   - Requests max_tokens=32 — enough for the server to emit at least one
//     data: chunk and the [DONE] terminator without waiting for a full response.
//   - Appends -w '\n%{http_code}' so the HTTP status code is on the final line.
func runVLLMSSEProbe(ctx context.Context, sctx *scenarios.Context, vip string) (http200 bool, sseOK bool, detail string) {
	if vip == "" {
		return false, false, "VIP not set — cannot probe"
	}
	if sctx.State == nil {
		return false, false, "scenario state not available"
	}
	instanceID := sctx.State.Get("JUMPHOST_INSTANCE_ID")
	sourceIP := sctx.State.Get("JUMPHOST_BNK_EXT_ENI_IP")
	region := ""
	if sctx.Cluster != nil {
		region = sctx.Cluster.Metadata.Region
	}
	if instanceID == "" || sourceIP == "" || region == "" {
		return false, false, "jumphost state not available (JUMPHOST_INSTANCE_ID / JUMPHOST_BNK_EXT_ENI_IP / region)"
	}

	probeOpts := jumphost.ProbeOptions{
		Region:     region,
		InstanceID: instanceID,
		SourceIP:   sourceIP,
		VIP:        vip,
		Timeout:    60 * time.Second,
		Hostname:   scnHostname,
	}

	// Streaming completion request body — minimal tokens so the probe completes quickly.
	reqBody := `{"model":"llama3","messages":[{"role":"user","content":"Say hello."}],"stream":true,"max_tokens":32}`
	// Remote curl: output SSE body on stdout, HTTP code as the final line.
	remoteCmd := fmt.Sprintf(
		`curl -s -N --no-buffer --interface %s -H 'Host: %s' -H 'Content-Type: application/json' -H 'Accept: text/event-stream' -d %s -w '\n%%{http_code}' http://%s/v1/chat/completions`,
		sourceIP, scnHostname,
		shellSingleQuote(reqBody),
		vip,
	)

	outs, err := jumphost.RunStagingCommands(ctx, probeOpts, []string{remoteCmd})
	if err != nil {
		detail = "SSH probe error: " + err.Error()
		if len(outs) > 0 {
			detail += " (stdout: " + outs[0] + ")"
		}
		return false, false, detail
	}
	if len(outs) == 0 {
		return false, false, "no output from curl probe"
	}

	out := outs[0]
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 {
		return false, false, "empty response from curl"
	}

	// Last line is the HTTP status code written by -w '%{http_code}'.
	codeStr := strings.TrimSpace(lines[len(lines)-1])
	http200 = codeStr == "200"

	// Remaining lines are the SSE body: look for "data: " and "[DONE]".
	sseBody := strings.Join(lines[:len(lines)-1], "\n")
	hasData := strings.Contains(sseBody, "data: ")
	hasDone := strings.Contains(sseBody, "[DONE]")
	sseOK = hasData && hasDone

	detail = fmt.Sprintf("HTTP %s — SSE data: found=%v, [DONE] found=%v", codeStr, hasData, hasDone)
	return http200, sseOK, detail
}

// shellSingleQuote wraps s in single quotes safe for /bin/sh.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Hostname is the HTTPRoute hostname the scenario's Gateway matches; clients
// (the benchmark daemon) must send it as the Host header.
const Hostname = scnHostname

// GatewayVIP returns the scenario Gateway's VIP for the cluster's default VIP
// (same rule buildManifestVars applies), so benchmarks target the address the
// scenario actually listens on.
func GatewayVIP(defaultVIP string) string {
	return withLastOctet(defaultVIP, strconv.Itoa(112))
}

// eppEnabled reports whether the scenario routes through the F5 Endpoint Picker:
// BNK 2.4+ (2.3 has no F5EPP), unless --option epp=false.
func eppEnabled(ctx *scenarios.Context) bool {
	if v, ok := ctx.Options["epp"]; ok {
		return strings.EqualFold(v, "true") || v == "1"
	}
	return ctx.Cluster != nil && ctx.Cluster.Bnk != nil && !strings.HasPrefix(ctx.Cluster.Bnk.ManifestVersion, "2.3")
}

// giecrdManifest holds the InferencePool CRD the F5 EPP needs; BNK does not ship it.
const giecrdManifest = "gie/gateway-api-inference-extension-v1.5.0.yaml"

var f5EPPGVR = schema.GroupVersionResource{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "f5-epps"}

var crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

// ensureGIECRDs installs the Gateway API Inference Extension CRDs only when the
// InferencePool CRD is absent: it is cluster-wide and shared with other inference
// gateways, so an installed version is never replaced.
func ensureGIECRDs(ctx *scenarios.Context) error {
	if ctx.Dynamic == nil {
		return nil
	}
	_, err := ctx.Dynamic.Resource(crdGVR).Get(ctx.Ctx, "inferencepools.inference.networking.k8s.io", metav1.GetOptions{})
	if err == nil {
		fmt.Fprintf(ctx.Out, "[ai-inference-e2e] InferencePool CRD present — reusing\n")
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	data, err := manifests.FS.ReadFile(giecrdManifest)
	if err != nil {
		return err
	}
	dir := filepath.Join(ctx.WorkspaceDir, "artifacts", "gie")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	path := filepath.Join(dir, filepath.Base(giecrdManifest))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(ctx.Out, "[ai-inference-e2e] installing %s\n", filepath.Base(giecrdManifest))
	ao := &k8sapply.ApplyOptions{Filename: path, KubeconfigPath: ctx.KubeconfigPath}
	return ao.Run(ctx.Ctx)
}

// farSecretName is the FAR pull secret the CNEInstance registry names; the
// controller puts it on the F5 EPP pod, so it must exist in the pool namespace.
const farSecretName = "far-secret"

// ensureFARSecret copies the FAR pull secret from the BNK controller namespace
// into ns, where the F5 EPP pulls its image from repo.f5.com.
func ensureFARSecret(ctx *scenarios.Context, ns string) error {
	if ctx.Clientset == nil {
		return nil
	}
	secrets := ctx.Clientset.CoreV1()
	src, err := secrets.Secrets("f5-cne-system").Get(ctx.Ctx, farSecretName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	dst := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: farSecretName, Namespace: ns},
		Type:       src.Type,
		Data:       src.Data,
	}
	_, err = secrets.Secrets(ns).Create(ctx.Ctx, dst, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = secrets.Secrets(ns).Update(ctx.Ctx, dst, metav1.UpdateOptions{})
	}
	return err
}

// indent prefixes every non-empty line of s with prefix (YAML block scalars).
func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n")
}
