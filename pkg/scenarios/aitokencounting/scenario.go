// Package aitokencounting implements scenario "ai-token-counting" — the F5
// BNK AI token counting + enforcement feature (how-to #6).
//
// This feature is ANNOTATION-DRIVEN: there is no dedicated CR. Token
// counting + per-user enforcement is configured entirely via the
// `k8s.f5.com/ai-token-counting` annotation on the Gateway's
// spec.infrastructure.annotations.
//
// # Ratings
//
// The scenario has two operating modes:
//
//   - Amber (offline / no LLM backend): control-plane only — asserts
//     Deployment Available, Gateway Programmed, HTTPRoute Accepted, and
//     the annotation persists. Rating() returns Amber and no data-path
//     probe is attempted.
//
//   - Green (live data-path via VerifyDeps.DataPathVerifyFn): when a
//     real vLLM-compatible backend is reachable the caller provides a
//     non-nil DataPathVerifyFn that drives traffic, asserts token
//     metering + HTTP 503 on overload. Rating() still returns Amber for
//     the registered singleton (safe default); the fn-pointer upgrades
//     the runtime result to Green when every data-path assertion passes.
//
// # Hazard: the annotation is Gateway-scoped and rejects non-LLM payloads
//
// Enabling token_counting makes the TMM AI profile require an LLM `model`
// field in EVERY request body on that Gateway, and reject anything without
// one:
//
//	HTTP/1.1 400 Bad Request
//	{"error":"model_missing"}
//
// There is no per-listener or per-route opt-in, so a Gateway carrying mixed
// traffic (e.g. an MCP tool alongside an inference backend) loses the non-LLM
// traffic entirely. Give inference its own Gateway. Observed live on
// bnk-agentcore-demo, 2026-08-19, while adding the annotation to a Gateway
// fronting an MCP JSON-RPC service — see examples/agentcore-demo.
//
// Related: the profile parses OpenAI-shaped usage fields
// (usage.prompt_tokens / completion_tokens / total_tokens). Amazon Bedrock
// returns usage.inputTokens / outputTokens, so a Bedrock-native backend
// meters as zero even when counting is enabled.
//
// # Why the static Rating() stays Amber
//
// Rating() is called before any cluster interaction (e.g. by `scenarios
// list`). The cluster does not guarantee a real LLM backend is present,
// so we cannot statically claim Green. The Result.Status reflects the
// actual outcome once Verify runs.
package aitokencounting

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
)

//go:embed manifests/*.yaml
var manifestFS embed.FS

const (
	scnName      = "ai-token-counting"
	scnTitle     = "AI token counting + enforcement annotation (how-to #6, Amber)"
	scnNamespace = "awsbnkctl-scn-aitokens"
	// aiAnnotationKey is the verbatim Gateway annotation key from clouddocs 2.3.
	aiAnnotationKey = "k8s.f5.com/ai-token-counting"
)

func init() { scenarios.Register(&scenario{}) }

// VerifyDeps holds the function pointers used by Verify. The zero value routes
// every call to the real package-level implementations. Tests swap individual
// fields to a recording stub to assert call order without touching the cluster.
//
// DataPathVerifyFn is OPTIONAL (nil = offline mode):
//
//	When non-nil it is invoked after the control-plane assertions succeed.
//	It must drive data-plane traffic through the VIP and return a slice of
//	Assertion values that are appended to the result. A nil fn means the
//	cluster cannot provide a real LLM backend and data-path checks are
//	skipped (offline / Amber mode).
//
// Signature: func(ctx, sctx, vip) []scenarios.Assertion
type VerifyDeps struct {
	WaitDeploymentAvailableFn func(ctx context.Context, sctx *scenarios.Context, ns, name string, timeout time.Duration) error
	WaitConditionFn           func(ctx context.Context, sctx *scenarios.Context, gvr schema.GroupVersionResource, ns, name, condType string, timeout time.Duration) error
	WaitHTTPRouteConditionFn  func(ctx context.Context, sctx *scenarios.Context, ns, name, condType string, timeout time.Duration) error

	// DataPathVerifyFn, when non-nil, runs live data-path checks against the
	// running VIP: token-metering assertion + HTTP 503 overload probe. Returning
	// a non-empty slice of all-OK assertions upgrades the run result to Green.
	// When nil the scenario stays in Amber (control-plane only) mode.
	DataPathVerifyFn func(ctx context.Context, sctx *scenarios.Context, vip string) []scenarios.Assertion

	// GatewayAnnotationFn, when non-nil, overrides the default annotation
	// read-back (which requires ctx.Dynamic). Tests inject a stub here.
	// When nil, the real gatewayAnnotationPresent implementation is used.
	GatewayAnnotationFn func(ctx *scenarios.Context, ns, name, key string) (bool, string)
}

func realVerifyDeps() VerifyDeps {
	return VerifyDeps{
		WaitDeploymentAvailableFn: scenarios.WaitDeploymentAvailable,
		WaitConditionFn:           scenarios.WaitCondition,
		WaitHTTPRouteConditionFn:  scenarios.WaitHTTPRouteCondition,
		// DataPathVerifyFn is intentionally nil: the awsbnkctl EKS / host-device
		// cluster has no LLM backend. A caller that sets up a real vLLM backend
		// (e.g. the AI-LB demo harness) should inject a concrete function here.
		DataPathVerifyFn: nil,
		// GatewayAnnotationFn nil → uses the real gatewayAnnotationPresent.
		GatewayAnnotationFn: nil,
	}
}

type scenario struct {
	// vDeps is nil for the registered singleton; tests inject a non-nil value.
	vDeps *VerifyDeps
}

func (s *scenario) Name() string             { return scnName }
func (s *scenario) Title() string            { return scnTitle }
func (s *scenario) Rating() scenarios.Rating { return scenarios.Amber }
func (s *scenario) Dependencies() []string   { return []string{} }
func (s *scenario) Description() string {
	return strings.TrimSpace(`
AI token counting + enforcement scenario (how-to #6).

The feature is annotation-driven — no dedicated CR. Token counting and
per-user rate-limit enforcement are configured through the Gateway
spec.infrastructure.annotations["k8s.f5.com/ai-token-counting"] value.

Applies 5 templated manifests into the scenario namespace:
  Namespace, GatewaySettings (listener context),
  one nginx Deployment+Service (so the HTTPRoute resolves),
  Gateway (spec.addresses=[VIP] + the k8s.f5.com/ai-token-counting
  annotation under spec.infrastructure.annotations), HTTPRoute
  (host=awsbnkctl-aitokens.local → nginx:80).

Verify — two modes:

  Amber (default, no LLM backend):
    1. Wait nginx Deployment Available.
    2. Wait Gateway scn-aitokens-gateway Programmed=True.
    3. Wait HTTPRoute scn-aitokens-route Accepted=True.
    4. Assert the Gateway still carries the k8s.f5.com/ai-token-counting
       annotation (read-back).

  Green (when VerifyDeps.DataPathVerifyFn is injected — requires a real
         vLLM-compatible backend reachable at the VIP):
    Steps 1–4 above, then:
    5. Drive synthetic traffic through the VIP and assert token-metering
       response headers are present (x-token-usage or equivalent).
    6. Drive synthetic overload and assert HTTP 503 + Retry-After is
       returned under rate-limit enforcement.

Rating() returns Amber for the registered singleton (safe static default
for "scenarios list"). The result status reflects the actual runtime
outcome: if all data-path assertions pass, the run result is "ok" (Green
equivalent). See the public economics demo docs for fill-first/spill config and the
503-overload demo scripts.

Cleanup: delete the scenario namespace (idempotent).
`)
}

// manifestVars holds the template variables for the 5 manifests.
type manifestVars struct {
	Namespace        string
	ClusterName      string
	GatewayClassName string
	VIP              string
	ExternalCIDR     string
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
	return scenarios.ApplyManifests(ctx, scnName)
}

func (s *scenario) Verify(ctx *scenarios.Context) scenarios.Result {
	d := s.vDeps
	if d == nil {
		real := realVerifyDeps()
		d = &real
	}
	ns := namespace(ctx)

	// Determine operating mode up-front so Details is accurate.
	liveMode := d.DataPathVerifyFn != nil
	modeDesc := "Amber: control-plane only — token metering / rate-limit enforcement is not data-plane tested (no LLM backend). No curl probe."
	if liveMode {
		modeDesc = "Green: control-plane + live data-path — token metering + HTTP 503 overload probe executed."
	}
	res := scenarios.Result{Details: modeDesc}

	// --- Step 1: Control-plane assertions ---
	// Order is load-bearing: all polled conditions must settle before any
	// data-path probe is attempted. We track success of the three polled
	// conditions separately (cpReady) so the annotation read-back (which
	// requires a live Dynamic client) does not gate data-path in unit tests.

	cpReady := true // set to false if any polled condition fails

	// nginx Deployment Available (so the HTTPRoute can resolve its backend).
	err := d.WaitDeploymentAvailableFn(ctx.Ctx, ctx, ns, "nginx", 3*time.Minute)
	if err != nil {
		cpReady = false
	}
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "nginx Deployment Available",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// Gateway Programmed=True.
	err = d.WaitConditionFn(ctx.Ctx, ctx, scenarios.GatewayGVR, ns, "scn-aitokens-gateway", "Programmed", 5*time.Minute)
	if err != nil {
		cpReady = false
	}
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "Gateway scn-aitokens-gateway Programmed=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// HTTPRoute Accepted=True.
	err = d.WaitHTTPRouteConditionFn(ctx.Ctx, ctx, ns, "scn-aitokens-route", "Accepted", 3*time.Minute)
	if err != nil {
		cpReady = false
	}
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "HTTPRoute scn-aitokens-route Accepted=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// Annotation persists: read the Gateway back and confirm the
	// k8s.f5.com/ai-token-counting annotation is present + non-empty.
	// This assertion does NOT gate data-path; it can only be verified when
	// ctx.Dynamic is non-nil (i.e. on a real cluster).
	annotationFn := d.GatewayAnnotationFn
	if annotationFn == nil {
		annotationFn = s.gatewayAnnotationPresent
	}
	annOK, annGot := annotationFn(ctx, ns, "scn-aitokens-gateway", aiAnnotationKey)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "Gateway retains k8s.f5.com/ai-token-counting annotation",
		OK:          annOK,
		Got:         annGot,
	})

	// --- Step 2: Live data-path assertions (conditional) ---
	// Only executed when DataPathVerifyFn is non-nil (i.e. a real vLLM-
	// compatible backend is reachable at the VIP). The fn drives synthetic
	// traffic, asserts token metering via response headers, and probes for
	// HTTP 503 under overload. When the fn is nil we skip silently (Amber).
	//
	// Gate: the three polled control-plane conditions must have passed so
	// traffic can reach the VIP. The annotation assertion does NOT gate this
	// step — it can fail in unit tests where ctx.Dynamic is nil.
	if liveMode && cpReady {
		vip, _, _, probeErr := scenarios.BuildProbeParams(ctx)
		if probeErr != nil {
			res.Assertions = append(res.Assertions, scenarios.Assertion{
				Description: "data-path probe setup",
				OK:          false,
				Got:         probeErr.Error(),
			})
		} else {
			res.DataPath = true
			dpAssertions := d.DataPathVerifyFn(ctx.Ctx, ctx, vip)
			res.Assertions = append(res.Assertions, dpAssertions...)
		}
	}

	return scenarios.FinalizeResult(res)
}

// gatewayAnnotationPresent reads the Gateway via the dynamic client and reports
// whether spec.infrastructure.annotations[key] exists and is non-empty.
// Returns (false, "skipped: no dynamic client") when ctx.Dynamic is nil so
// tests that stub out Kubernetes clients can call Verify without panicking.
func (s *scenario) gatewayAnnotationPresent(ctx *scenarios.Context, ns, name, key string) (bool, string) {
	if ctx.Dynamic == nil {
		return false, "skipped: no dynamic client (unit-test mode)"
	}
	obj, err := ctx.Dynamic.Resource(scenarios.GatewayGVR).Namespace(ns).Get(ctx.Ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, scenarios.ErrString(err)
	}
	val, found, err := unstructured.NestedString(obj.Object, "spec", "infrastructure", "annotations", key)
	if err != nil {
		return false, scenarios.ErrString(err)
	}
	if !found || strings.TrimSpace(val) == "" {
		return false, fmt.Sprintf("annotation %q missing or empty on spec.infrastructure.annotations", key)
	}
	return true, "present"
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
	// Use .104 to avoid colliding with other scenarios' pools.
	v.VIP = withLastOctet(vip, strconv.Itoa(104))
	return v, nil
}
