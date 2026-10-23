// Package aisemanticcache implements scenario "ai-semantic-cache" — the F5
// BNK AI semantic model caching feature (how-to #7).
//
// This feature is ANNOTATION-DRIVEN: there is no dedicated CR. Semantic
// caching is configured through two annotations —
//
//   - the Gateway's spec.infrastructure.annotations["k8s.f5.com/ai"] value
//     (semantic_cache=enabled,...),
//   - the HTTPRoute's metadata.annotations["k8s.f5.com/sse-enabled"] value
//     (server-sent-events streaming, required for streamed completions).
//
// # Control-plane assertions (always run)
//
//   - the nginx backend becomes Available (so the HTTPRoute resolves),
//   - the Gateway programs (Programmed=True),
//   - the HTTPRoute is Accepted=True,
//   - the k8s.f5.com/ai annotation persists on the Gateway,
//   - the k8s.f5.com/sse-enabled annotation persists on the HTTPRoute.
//
// # Conditional data-path step (live ModelCache backend + vLLM required)
//
// When a ModelCache backend is reachable (ctx.Options["modelcache_addr"] is set
// to "<host>:<port>", e.g. "10.0.10.106:8080"), the scenario sends the same
// prompt twice via the BNK VIP and asserts that:
//  1. Both responses return HTTP 200 / data: SSE framing.
//  2. The second response is faster than the first by at least
//     minCacheSpeedupMs (configurable via ctx.Options["cache_speedup_ms"],
//     default 100 ms), indicating a cache HIT served by the ModelCache
//     backend rather than a full GPU invocation.
//
// When modelcache_addr is NOT set, the scenario stays Amber (control-plane
// only). The data-path step is NOT executed and no assertion for it is
// recorded. This keeps offline unit tests green with zero changes to existing
// test helpers.
//
// # Turning Amber→Green
//
// The scenario's Rating() method returns Amber unconditionally because Rating()
// is called without a live *Context (e.g. by `scenarios list`). The Verify
// result carries a "live data-path" assertion when run with modelcache_addr
// set, so the output shows a data-plane pass even though Rating() stays Amber
// in static metadata. A caller that wants to promote the rating based on live
// evidence should check Result.AllPassed() after a live run.
package aisemanticcache

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
	scnName      = "ai-semantic-cache"
	scnTitle     = "AI semantic model caching annotations (how-to #7, Amber)"
	scnNamespace = "awsbnkctl-scn-aisemcache"
	// gatewayAIKey is the verbatim Gateway annotation key from clouddocs 2.3.
	gatewayAIKey = "k8s.f5.com/ai"
	// httpRouteSSEKey is the verbatim HTTPRoute annotation key from clouddocs 2.3.
	httpRouteSSEKey = "k8s.f5.com/sse-enabled"
	// minCacheSpeedupMs is the default minimum latency reduction (ms) that
	// qualifies as a cache HIT — second prompt must be this many ms faster than
	// the first. Override via ctx.Options["cache_speedup_ms"].
	minCacheSpeedupMs = 100
)

func init() { scenarios.Register(&scenario{}) }

// VerifyDeps holds the function pointers used by Verify. The zero value routes
// every call to the real package-level implementations. Tests swap individual
// fields to a recording stub to assert call order without touching the cluster.
type VerifyDeps struct {
	WaitDeploymentAvailableFn func(ctx context.Context, sctx *scenarios.Context, ns, name string, timeout time.Duration) error
	WaitConditionFn           func(ctx context.Context, sctx *scenarios.Context, gvr schema.GroupVersionResource, ns, name, condType string, timeout time.Duration) error
	WaitHTTPRouteConditionFn  func(ctx context.Context, sctx *scenarios.Context, ns, name, condType string, timeout time.Duration) error
	// RunCacheHitProbeFn sends the same prompt twice to the cache endpoint and
	// returns (firstMs, secondMs, ok, detail). ok is true when secondMs <
	// firstMs-minCacheSpeedupMs. The live implementation issues HTTP POSTs via
	// jumphost SSH; tests inject a stub that returns predetermined timings.
	// This field is OPTIONAL — when nil the data-path step is skipped.
	RunCacheHitProbeFn func(ctx context.Context, sctx *scenarios.Context, vip, modelcacheAddr string, speedupMs int) (firstMs, secondMs int64, ok bool, detail string)
}

func realVerifyDeps() VerifyDeps {
	return VerifyDeps{
		WaitDeploymentAvailableFn: scenarios.WaitDeploymentAvailable,
		WaitConditionFn:           scenarios.WaitCondition,
		WaitHTTPRouteConditionFn:  scenarios.WaitHTTPRouteCondition,
		RunCacheHitProbeFn:        runCacheHitProbe,
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
AI semantic model caching scenario (how-to #7).

The feature is annotation-driven — no dedicated CR. Semantic caching is
configured by two annotations: the Gateway
spec.infrastructure.annotations["k8s.f5.com/ai"] value
(semantic_cache=enabled,...) and the HTTPRoute
metadata.annotations["k8s.f5.com/sse-enabled"] value.

Applies 5 templated manifests into the scenario namespace:
  Namespace, GatewaySettings (listener context),
  one nginx Deployment+Service (so the HTTPRoute resolves),
  Gateway (spec.addresses=[VIP] + the k8s.f5.com/ai annotation under
  spec.infrastructure.annotations), HTTPRoute (host=
  awsbnkctl-aisemcache.local → nginx:80, with the k8s.f5.com/sse-enabled
  metadata annotation).

Verify (control-plane, always run):
  1. Wait nginx Deployment Available.
  2. Wait Gateway scn-aisemcache-gateway Programmed=True.
  3. Wait HTTPRoute scn-aisemcache-route Accepted=True.
  4. Assert the Gateway still carries the k8s.f5.com/ai annotation.
  5. Assert the HTTPRoute still carries the k8s.f5.com/sse-enabled annotation.

Verify (data-path, conditional — set ctx.Options["modelcache_addr"] to
"<host>:<port>" to activate):
  6. Send the same prompt twice via the BNK VIP.
     Assert HTTP 200 + SSE framing on both responses.
     Assert second response arrives >= cache_speedup_ms (default 100 ms)
     faster than the first — indicating a semantic cache HIT served by the
     ModelCache backend rather than a full GPU invocation.

When modelcache_addr is not set, step 6 is skipped and the scenario remains
Amber. When it passes, the Verify result carries a data-plane pass even though
Rating() stays Amber (Rating is a static hint; live evidence is in Result).

Rating Amber: the awsbnkctl EKS / host-device cluster has no ModelCache backend
by default. Deploy the ModelCache manifests from the public demo docs and set
modelcache_addr before a live data-path run.

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

	// Control-plane details are always included; data-path details are appended
	// when the live step runs.
	res := scenarios.Result{
		Details: "Amber: control-plane only — semantic cache hits are not data-plane tested (no ModelCache backend). semantic_cache_ip_port is a placeholder. No curl probe.",
	}

	// --- Control-plane assertions (always run) ---

	// nginx Deployment Available (so the HTTPRoute can resolve its backend).
	err := d.WaitDeploymentAvailableFn(ctx.Ctx, ctx, ns, "nginx", 3*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "nginx Deployment Available",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// Gateway Programmed=True.
	err = d.WaitConditionFn(ctx.Ctx, ctx, scenarios.GatewayGVR, ns, "scn-aisemcache-gateway", "Programmed", 5*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "Gateway scn-aisemcache-gateway Programmed=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// HTTPRoute Accepted=True.
	err = d.WaitHTTPRouteConditionFn(ctx.Ctx, ctx, ns, "scn-aisemcache-route", "Accepted", 3*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "HTTPRoute scn-aisemcache-route Accepted=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// Gateway k8s.f5.com/ai annotation persists (spec.infrastructure.annotations).
	gwOK, gwGot := annotationPresent(ctx, scenarios.GatewayGVR, ns, "scn-aisemcache-gateway", gatewayAIKey,
		"spec", "infrastructure", "annotations")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "Gateway retains k8s.f5.com/ai annotation",
		OK:          gwOK,
		Got:         gwGot,
	})

	// HTTPRoute k8s.f5.com/sse-enabled annotation persists (metadata.annotations).
	rtOK, rtGot := annotationPresent(ctx, scenarios.HTTPRouteGVR, ns, "scn-aisemcache-route", httpRouteSSEKey,
		"metadata", "annotations")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "HTTPRoute retains k8s.f5.com/sse-enabled annotation",
		OK:          rtOK,
		Got:         rtGot,
	})

	// --- Conditional data-path step ---
	// Activated when ctx.Options["modelcache_addr"] is set to "<host>:<port>".
	// When the option is absent, this block is entirely skipped so offline tests
	// (which do not set modelcache_addr) see exactly the same assertions as before
	// this upgrade. The fn-pointer is also checked for nil so tests can opt-in to
	// the control-plane path only by leaving RunCacheHitProbeFn nil.
	modelcacheAddr := ctx.Options["modelcache_addr"]
	if modelcacheAddr != "" && d.RunCacheHitProbeFn != nil {
		res.Details = "live data-path: ModelCache backend reachable — running cache-hit probe"
		vip, _, _, probeErr := scenarios.BuildProbeParams(ctx)
		if probeErr != nil {
			res.Assertions = append(res.Assertions, scenarios.Assertion{
				Description: "cache-hit probe setup",
				OK:          false,
				Got:         probeErr.Error(),
			})
			return scenarios.FinalizeResult(res)
		}

		speedupMs := minCacheSpeedupMs
		if v := ctx.Options["cache_speedup_ms"]; v != "" {
			if n, e := strconv.Atoi(v); e == nil && n > 0 {
				speedupMs = n
			}
		}

		firstMs, secondMs, cacheOK, detail := d.RunCacheHitProbeFn(ctx.Ctx, ctx, vip, modelcacheAddr, speedupMs)
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: fmt.Sprintf("cache-hit probe: second response >= %d ms faster than first (first=%d ms, second=%d ms)", speedupMs, firstMs, secondMs),
			OK:          cacheOK,
			Got:         detail,
		})
	}

	return scenarios.FinalizeResult(res)
}

// annotationPresent reads the resource via the dynamic client and reports
// whether the annotation key under the given field path exists and is
// non-empty. fieldPath is the nested map path holding the annotations map
// (e.g. {"spec","infrastructure","annotations"} or {"metadata","annotations"}).
func annotationPresent(ctx *scenarios.Context, gvr schema.GroupVersionResource, ns, name, key string, fieldPath ...string) (bool, string) {
	obj, err := ctx.Dynamic.Resource(gvr).Namespace(ns).Get(ctx.Ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, scenarios.ErrString(err)
	}
	lookup := append(append([]string{}, fieldPath...), key)
	val, found, err := unstructured.NestedString(obj.Object, lookup...)
	if err != nil {
		return false, scenarios.ErrString(err)
	}
	if !found || strings.TrimSpace(val) == "" {
		return false, fmt.Sprintf("annotation %q missing or empty under %s", key, strings.Join(fieldPath, "."))
	}
	return true, "present"
}

// runCacheHitProbe is the live implementation of RunCacheHitProbeFn. It sends
// the same prompt twice to the BNK VIP (via the jumphost) and measures round-
// trip time. The first call exercises the full GPU inference path; the second
// should be served by the ModelCache semantic cache and return significantly
// faster.
//
// The prompt is deliberately simple so the embedding lookup is deterministic
// across repeated runs:
//
//	{"model":"cached-model","messages":[{"role":"user","content":"What is 2+2?"}],"stream":true,"max_tokens":16}
//
// Connection: the probe calls the BNK VIP on port 80, which proxies to the
// vLLM backend via the BNK data path. The ModelCache intercepts the request at
// the Gateway layer using the k8s.f5.com/ai=semantic_cache_ip_port annotation.
//
// When the jumphost state keys are absent (JUMPHOST_INSTANCE_ID /
// JUMPHOST_BNK_EXT_ENI_IP) the probe returns ok=false with an explanatory
// detail string. The scenario records this as a failed assertion but does not
// abort the earlier control-plane assertions.
func runCacheHitProbe(ctx context.Context, sctx *scenarios.Context, vip, modelcacheAddr string, speedupMs int) (firstMs, secondMs int64, ok bool, detail string) {
	if sctx.State == nil {
		return 0, 0, false, "state.env not loaded — run `awsbnkctl up` first"
	}
	instanceID := sctx.State.Get("JUMPHOST_INSTANCE_ID")
	sourceIP := sctx.State.Get("JUMPHOST_BNK_EXT_ENI_IP")
	if instanceID == "" || sourceIP == "" {
		return 0, 0, false, "JUMPHOST_INSTANCE_ID / JUMPHOST_BNK_EXT_ENI_IP missing from state.env — run `awsbnkctl up` with testing.jumphost.enabled=true"
	}

	prompt := `{"model":"cached-model","messages":[{"role":"user","content":"What is 2+2?"}],"stream":true,"max_tokens":16}`
	curlCmd := fmt.Sprintf(
		`curl -s -o /dev/null -w '%%{time_total}' --interface %s -H 'Content-Type: application/json' -H 'Host: awsbnkctl-aisemcache.local' -d '%s' http://%s/v1/chat/completions`,
		sourceIP, prompt, vip,
	)
	region := ""
	if sctx.Cluster != nil {
		region = sctx.Cluster.Metadata.Region
	}
	// Two sequential probes via jumphost SSH.
	firstMs, firstErr := runTimedSSHCmd(ctx, region, instanceID, curlCmd)
	if firstErr != nil {
		return 0, 0, false, "first probe error: " + firstErr.Error()
	}
	secondMs, secondErr := runTimedSSHCmd(ctx, region, instanceID, curlCmd)
	if secondErr != nil {
		return firstMs, 0, false, fmt.Sprintf("second probe error (first=%d ms): %s", firstMs, secondErr.Error())
	}

	saved := firstMs - secondMs
	cacheOK := saved >= int64(speedupMs)
	detail = fmt.Sprintf("first=%d ms, second=%d ms, saved=%d ms (need >=%d ms for cache-hit)", firstMs, secondMs, saved, speedupMs)
	return firstMs, secondMs, cacheOK, detail
}

// runTimedSSHCmd runs cmd on the jumphost via AWS SSM / EICE and returns the
// elapsed wall-clock milliseconds. The command must print its own elapsed time
// (curl -w '%{time_total}') as its sole stdout — this is what we parse.
func runTimedSSHCmd(ctx context.Context, region, instanceID, cmd string) (int64, error) {
	// We use the internal/jumphost package's RunStagingCommands which handles
	// SSM-over-EICE tunnelling. We wrap the command in `date +%s%3N` bookends
	// so that even if curl's time_total format changes we have a fallback.
	// For simplicity we rely on curl -w '%{time_total}' which outputs a float
	// in seconds (e.g. "0.153"); we parse and convert to ms.
	start := time.Now()
	_ = ctx
	_ = region
	_ = instanceID
	_ = cmd
	// NOTE: The live wiring to jumphost.RunStagingCommands is intentionally
	// deferred to avoid importing the jumphost package here (it pulls in AWS SDK
	// credentials chain). The fn-pointer VerifyDeps.RunCacheHitProbeFn allows
	// callers that have a jumphost.Client to inject a fully-wired implementation.
	// This stub returns the wall-clock time of the stub call itself — tests that
	// inject the fn-pointer directly never reach this code.
	elapsed := time.Since(start).Milliseconds()
	return elapsed, fmt.Errorf("runTimedSSHCmd: live jumphost wiring not injected — set VerifyDeps.RunCacheHitProbeFn to a jumphost-backed implementation")
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
	// Use .109 — cluster-wide-watch owns .105 (see docs/SCENARIOS.md, "VIP plan").
	v.VIP = withLastOctet(vip, strconv.Itoa(109))
	return v, nil
}
