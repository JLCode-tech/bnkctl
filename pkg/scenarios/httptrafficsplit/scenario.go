// Package httptrafficsplit implements scenario "http-traffic-split" — a
// weighted HTTPRoute (70/30) across two nginx backends, asserting both are
// served (how-to #8, weighted variant).
//
// AWS-specific shape mirrors httproutee2e:
//   - GatewayClass provisioned by Phase 23b (<cluster>-gatewayclass).
//   - GatewaySettings is owned by the scenario (02-gatewaysettings.yaml);
//     pool is a single address (VIP only) so it does not collide with other
//     scenarios' pools.
//   - Verification curls through SSH+EICE from the jumphost's
//     BNK_EXT ENI, reading the response body to detect which backend answered.
//
// Verify order (load-bearing):
//  1. Wait backend-a Deployment Available.
//  2. Wait backend-b Deployment Available.
//  3. Wait Gateway scn-split-gateway Programmed=True.
//  4. Wait HTTPRoute scn-split-route Accepted=True.
//  5. Wait HTTPRoute scn-split-route ResolvedRefs=True.
//  6. ResyncHTTPRoutes — idempotent pool-member workaround.
//  7. Probe: max(10, iterations) curls; assert BOTH "backend-a" and
//     "backend-b" appear in response bodies.
package httptrafficsplit

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/JLCode-tech/bnkctl/pkg/bnk"
	"github.com/JLCode-tech/bnkctl/pkg/jumphost"
	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
)

//go:embed manifests/*.yaml
var manifestFS embed.FS

const (
	scnName      = "http-traffic-split"
	scnTitle     = "HTTP weighted traffic split across two backends (how-to #8, weighted)"
	scnNamespace = "awsbnkctl-scn-traffic-split"
	// scnHostname must match the hostnames: value in manifests/05-httproute.yaml.
	scnHostname = "awsbnkctl-split.local"
)

func init() { scenarios.Register(&scenario{}) }

// VerifyDeps holds the function pointers used by Verify. The zero value routes
// every call to the real package-level implementations. Tests swap individual
// fields to a recording stub to assert call order without touching the cluster.
type VerifyDeps struct {
	WaitDeploymentAvailableFn func(ctx context.Context, sctx *scenarios.Context, ns, name string, timeout time.Duration) error
	WaitConditionFn           func(ctx context.Context, sctx *scenarios.Context, gvr schema.GroupVersionResource, ns, name, condType string, timeout time.Duration) error
	WaitHTTPRouteConditionFn  func(ctx context.Context, sctx *scenarios.Context, ns, name, condType string, timeout time.Duration) error
	ResyncHTTPRoutesFn        func(ctx context.Context, sctx *scenarios.Context, ns string) error
	// RunBodyProbesFn curls the VIP and reports whether backend-a and backend-b
	// were both seen, plus a human-readable summary string.
	RunBodyProbesFn func(ctx context.Context, sctx *scenarios.Context, vip, host string, iterations int, timeout time.Duration) (seenA bool, seenB bool, got string)
}

func realVerifyDeps() VerifyDeps {
	return VerifyDeps{
		WaitDeploymentAvailableFn: scenarios.WaitDeploymentAvailable,
		WaitConditionFn:           scenarios.WaitCondition,
		WaitHTTPRouteConditionFn:  scenarios.WaitHTTPRouteCondition,
		ResyncHTTPRoutesFn: func(ctx context.Context, sctx *scenarios.Context, ns string) error {
			_, err := bnk.ResyncHTTPRoutes(ctx, sctx.Dynamic, bnk.ResyncOptions{
				Namespace:      ns,
				AllInNamespace: true,
			})
			return err
		},
		RunBodyProbesFn: func(_ context.Context, sctx *scenarios.Context, vip, host string, iterations int, timeout time.Duration) (bool, bool, string) {
			instanceID := sctx.State.Get("JUMPHOST_INSTANCE_ID")
			sourceIP := sctx.State.Get("JUMPHOST_BNK_EXT_ENI_IP")
			probeOpts := jumphost.ProbeOptions{
				Region:     sctx.Cluster.Metadata.Region,
				InstanceID: instanceID,
				SourceIP:   sourceIP,
				VIP:        vip,
				Iterations: iterations,
				Timeout:    timeout,
				Hostname:   host,
			}
			probes, probeRunErr := jumphost.RunCurlBodyProbes(sctx.Ctx, probeOpts)
			var seenA, seenB bool
			var lastErrStr string
			for _, p := range probes {
				if strings.Contains(p.Body, "backend-a") {
					seenA = true
				}
				if strings.Contains(p.Body, "backend-b") {
					seenB = true
				}
				if p.Err != "" {
					lastErrStr = p.Err
				}
			}
			got := fmt.Sprintf("%d probes: seenA=%v seenB=%v", len(probes), seenA, seenB)
			if probeRunErr != nil {
				got += " — probe error: " + probeRunErr.Error()
			} else if (!seenA || !seenB) && lastErrStr != "" {
				got += " — last error: " + lastErrStr
			}
			return seenA, seenB, got
		},
	}
}

type scenario struct {
	// vDeps is nil for the registered singleton; tests inject a non-nil value.
	vDeps *VerifyDeps
}

func (s *scenario) Name() string             { return scnName }
func (s *scenario) Title() string            { return scnTitle }
func (s *scenario) Rating() scenarios.Rating { return scenarios.Green }
func (s *scenario) Dependencies() []string   { return []string{} }
func (s *scenario) Description() string {
	return strings.TrimSpace(`
Weighted HTTPRoute scenario exercising the 70/30 traffic-split feature.

Applies 5 templated manifests into the scenario namespace:
  Namespace, GatewaySettings (listener context),
  two nginx Deployments+Services (backend-a / backend-b),
  Gateway (spec.addresses=[VIP]), HTTPRoute (host=awsbnkctl-split.local →
  backend-a:weight=70 + backend-b:weight=30).

Verify order (load-bearing):
  1. Wait backend-a and backend-b Deployments Available.
  2. Wait Gateway Programmed=True, HTTPRoute Accepted=True + ResolvedRefs=True.
  3. Call pkg/bnk.ResyncHTTPRoutes — idempotent pool-member workaround.
  4. Open SSH via EICE to the jumphost and curl --interface <BNK_EXT_ENI_IP>
     http://<VIP>/ N times (default 10), reading the response body.
  5. Assert: BOTH "backend-a" and "backend-b" appear in response bodies.

Cleanup: delete the scenario namespace (idempotent).
Requires: testing.jumphost.enabled=true in cluster.yaml + awsbnkctl up first.
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
	res := scenarios.Result{DataPath: true}

	// --- Step 1: Control-plane assertions ---
	// Order is load-bearing: both backends must be Available before ResyncHTTPRoutes.

	// backend-a Deployment Available.
	err := d.WaitDeploymentAvailableFn(ctx.Ctx, ctx, ns, "backend-a", 3*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "backend-a Deployment Available",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// backend-b Deployment Available.
	err = d.WaitDeploymentAvailableFn(ctx.Ctx, ctx, ns, "backend-b", 3*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "backend-b Deployment Available",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// Gateway Programmed=True.
	err = d.WaitConditionFn(ctx.Ctx, ctx, scenarios.GatewayGVR, ns, "scn-split-gateway", "Programmed", 5*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "Gateway scn-split-gateway Programmed=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// HTTPRoute Accepted=True.
	err = d.WaitHTTPRouteConditionFn(ctx.Ctx, ctx, ns, "scn-split-route", "Accepted", 3*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "HTTPRoute scn-split-route Accepted=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// HTTPRoute ResolvedRefs=True.
	err = d.WaitHTTPRouteConditionFn(ctx.Ctx, ctx, ns, "scn-split-route", "ResolvedRefs", 3*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "HTTPRoute scn-split-route ResolvedRefs=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// --- Step 2: ResyncHTTPRoutes (after control-plane is ready) ---
	// Idempotent workaround for cne-controller pool-member stale bug.
	// ResyncHTTPRoutes MUST run after HTTPRoute conditions are settled.
	resyncErr := d.ResyncHTTPRoutesFn(ctx.Ctx, ctx, ns)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "ResyncHTTPRoutes (pool-member refresh)",
		OK:          resyncErr == nil,
		Got:         scenarios.ErrString(resyncErr),
	})

	// --- Step 3: Body-capturing SSH+EICE curl probes ---
	vip, iterations, timeout, probeErr := scenarios.BuildProbeParams(ctx)
	if probeErr != nil {
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "jumphost probe setup",
			OK:          false,
			Got:         probeErr.Error(),
		})
		return scenarios.FinalizeResult(res)
	}
	// Probe the scenario's pinned VIP (.101), matching the manifests — NOT the
	// default VIP that BuildProbeParams returns.
	vip = withLastOctet(vip, strconv.Itoa(101))

	instanceID := ctx.State.Get("JUMPHOST_INSTANCE_ID")
	sourceIP := ctx.State.Get("JUMPHOST_BNK_EXT_ENI_IP")
	if instanceID == "" || sourceIP == "" {
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "jumphost state keys present",
			OK:          false,
			Got:         "JUMPHOST_INSTANCE_ID / JUMPHOST_BNK_EXT_ENI_IP missing from state.env — run `awsbnkctl up` with testing.jumphost.enabled=true",
		})
		return scenarios.FinalizeResult(res)
	}

	// Use at least 10 probes to give both backends a chance to be selected.
	probeIter := iterations
	if probeIter < 10 {
		probeIter = 10
	}

	ok, got := scenarios.PollMarkers(ctx.Ctx, 120*time.Second, 10*time.Second, func() (bool, string) {
		seenA, seenB, g := d.RunBodyProbesFn(ctx.Ctx, ctx, vip, scnHostname, probeIter, timeout)
		return seenA && seenB, g
	})
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "weighted split serves both backend-a and backend-b",
		OK:          ok,
		Got:         got,
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
	// Use .101 to avoid colliding with httproutee2e (.100) and other scenarios.
	v.VIP = withLastOctet(vip, strconv.Itoa(101))
	return v, nil
}
