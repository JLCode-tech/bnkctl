// Package multivip implements scenario "multi-vip" — MULTIPLE VIPs served from
// ONE GatewaySettings (the "chassis" model seen on the gold-reference
// cluster: one GatewaySettings with an address RANGE, multiple Gateways each
// pinning a distinct VIP from that range).
//
// Two Gateways → two VIPs → two HTTPRoutes → two distinct backends, asserting
// each VIP serves its OWN backend.
//
// AWS-specific shape mirrors httptrafficsplit:
//   - GatewayClass provisioned by Phase 23b (<cluster>-gatewayclass).
//   - GatewaySettings is owned by the scenario (02-gatewaysettings.yaml).
//     Unlike single-VIP scenarios the pool is a RANGE (.115–.117) so it can
//     hand out both pinned VIPs (.115 + .116).
//   - Verification curls through SSH+EICE from the jumphost's BNK_EXT
//     ENI, reading the response body to detect which backend answered.
//
// Verify order (load-bearing):
//  1. Wait mv-a Deployment Available.
//  2. Wait mv-b Deployment Available.
//  3. Wait Gateway scn-mv-gateway-a Programmed=True.
//  4. Wait Gateway scn-mv-gateway-b Programmed=True.
//  5. Wait HTTPRoute scn-mv-route-a Accepted=True.
//  6. Wait HTTPRoute scn-mv-route-b Accepted=True.
//  7. ResyncHTTPRoutes — idempotent pool-member workaround.
//  8. Probe VIP A (.115) with Host multivip-a.local; assert body contains
//     "multivip-backend-a".
//  9. Probe VIP B (.116) with Host multivip-b.local; assert body contains
//     "multivip-backend-b".
package multivip

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
	scnName      = "multi-vip"
	scnTitle     = "Multiple VIPs from one GatewaySettings pool (chassis model)"
	scnNamespace = "awsbnkctl-scn-multivip"
	// hostnames: values in manifests/05-httproutes.yaml.
	scnHostnameA = "multivip-a.local"
	scnHostnameB = "multivip-b.local"
	// Per-backend body markers in manifests/03-backends.yaml.
	markerA = "multivip-backend-a"
	markerB = "multivip-backend-b"
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
	// RunBodyProbesFn curls vip with the given Host header and returns the
	// concatenated response bodies (so Verify can contains-check a per-VIP
	// marker) plus a human-readable summary string.
	RunBodyProbesFn func(ctx context.Context, sctx *scenarios.Context, vip, host string, iterations int, timeout time.Duration) (bodies string, got string)
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
		RunBodyProbesFn: func(_ context.Context, sctx *scenarios.Context, vip, host string, iterations int, timeout time.Duration) (string, string) {
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
			var bodies []string
			var lastErrStr string
			for _, p := range probes {
				bodies = append(bodies, p.Body)
				if p.Err != "" {
					lastErrStr = p.Err
				}
			}
			joined := strings.Join(bodies, "\n")
			got := fmt.Sprintf("%d probes against %s (Host %s)", len(probes), vip, host)
			if probeRunErr != nil {
				got += " — probe error: " + probeRunErr.Error()
			} else if lastErrStr != "" {
				got += " — last error: " + lastErrStr
			}
			return joined, got
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
Multiple VIPs from ONE GatewaySettings pool — the "chassis" model.

Applies 5 templated manifests into the scenario namespace:
  Namespace, GatewaySettings (listener context),
  two nginx Deployments+Services (mv-a / mv-b),
  two Gateways (scn-mv-gateway-a pins VIP A .115, scn-mv-gateway-b pins
  VIP B .116 — both drawn from the same GatewaySettings pool),
  two HTTPRoutes (multivip-a.local → mv-a, multivip-b.local → mv-b).

Verify order (load-bearing):
  1. Wait mv-a and mv-b Deployments Available.
  2. Wait both Gateways Programmed=True, both HTTPRoutes Accepted=True.
  3. Call pkg/bnk.ResyncHTTPRoutes — idempotent pool-member workaround.
  4. Open SSH via EICE to the jumphost and curl --interface <BNK_EXT_ENI_IP>:
       VIP A with Host multivip-a.local → assert body has "multivip-backend-a".
       VIP B with Host multivip-b.local → assert body has "multivip-backend-b".

Cleanup: delete the scenario namespace (idempotent).
Requires: testing.jumphost.enabled=true in cluster.yaml + awsbnkctl up first.
`)
}

// manifestVars holds the template variables for the 5 manifests.
type manifestVars struct {
	Namespace        string
	ClusterName      string
	GatewayClassName string
	ExternalCIDR     string
	// VIPA / VIPB are the two pinned VIPs (.115 / .116), both inside the Infra
	// listener pool (.100-.199).
	VIPA string
	VIPB string
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

	// mv-a Deployment Available.
	err := d.WaitDeploymentAvailableFn(ctx.Ctx, ctx, ns, "mv-a", 3*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "mv-a Deployment Available",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// mv-b Deployment Available.
	err = d.WaitDeploymentAvailableFn(ctx.Ctx, ctx, ns, "mv-b", 3*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "mv-b Deployment Available",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// Gateway A Programmed=True.
	err = d.WaitConditionFn(ctx.Ctx, ctx, scenarios.GatewayGVR, ns, "scn-mv-gateway-a", "Programmed", 5*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "Gateway scn-mv-gateway-a Programmed=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// Gateway B Programmed=True.
	err = d.WaitConditionFn(ctx.Ctx, ctx, scenarios.GatewayGVR, ns, "scn-mv-gateway-b", "Programmed", 5*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "Gateway scn-mv-gateway-b Programmed=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// HTTPRoute A Accepted=True.
	err = d.WaitHTTPRouteConditionFn(ctx.Ctx, ctx, ns, "scn-mv-route-a", "Accepted", 3*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "HTTPRoute scn-mv-route-a Accepted=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// HTTPRoute B Accepted=True.
	err = d.WaitHTTPRouteConditionFn(ctx.Ctx, ctx, ns, "scn-mv-route-b", "Accepted", 3*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "HTTPRoute scn-mv-route-b Accepted=True",
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

	// --- Step 3: Body-capturing SSH+EICE curl probes, one per VIP ---
	v, varsErr := buildManifestVars(ctx)
	if varsErr != nil {
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "VIP derivation",
			OK:          false,
			Got:         varsErr.Error(),
		})
		return scenarios.FinalizeResult(res)
	}

	_, iterations, timeout, probeErr := scenarios.BuildProbeParams(ctx)
	if probeErr != nil {
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "jumphost probe setup",
			OK:          false,
			Got:         probeErr.Error(),
		})
		return scenarios.FinalizeResult(res)
	}

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

	// VIP A → backend mv-a.
	okA, gotA := scenarios.PollMarkers(ctx.Ctx, 120*time.Second, 10*time.Second, func() (bool, string) {
		bodies, g := d.RunBodyProbesFn(ctx.Ctx, ctx, v.VIPA, scnHostnameA, iterations, timeout)
		return strings.Contains(bodies, markerA), g
	})
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "VIP A (.115) serves backend mv-a",
		OK:          okA,
		Got:         gotA,
	})

	// VIP B → backend mv-b.
	okB, gotB := scenarios.PollMarkers(ctx.Ctx, 120*time.Second, 10*time.Second, func() (bool, string) {
		bodies, g := d.RunBodyProbesFn(ctx.Ctx, ctx, v.VIPB, scnHostnameB, iterations, timeout)
		return strings.Contains(bodies, markerB), g
	})
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "VIP B (.116) serves backend mv-b",
		OK:          okB,
		Got:         gotB,
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
	// Two distinct VIPs (.115 / .116) drawn from one GatewaySettings pool that
	// spans .115–.117 — the "chassis" address-range model. Octets chosen to
	// avoid colliding with httproutee2e (.100), httptrafficsplit (.101), etc.
	v.VIPA = withLastOctet(vip, strconv.Itoa(115))
	v.VIPB = withLastOctet(vip, strconv.Itoa(116))
	return v, nil
}
