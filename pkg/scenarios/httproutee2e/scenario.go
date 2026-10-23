// Package httproutee2e implements scenario "http-routing-e2e" — the
// full data-plane version of how-to #8 (HTTP traffic steering with
// Gateway API HTTPRoute).
//
// AWS-specific shape:
//   - GatewayClass is provisioned by Phase 23b (<cluster>-gatewayclass).
//   - GatewaySettings is owned by the scenario (02-gatewaysettings.yaml)
//     because no lifecycle phase provisions it cluster-wide; the scenario
//     provisions a namespaced pool so it can be self-contained and cleaned up.
//   - Verification curls through SSH+EICE from the jumphost's
//     BNK_EXT ENI (JUMPHOST_BNK_EXT_ENI_IP in state.env), not from an
//     in-cluster pod, so traffic exercises the real data path.
//
// Verify order (load-bearing):
//  1. Wait for control-plane conditions (Deployment Available, Gateway
//     Programmed=True, HTTPRoute Accepted+ResolvedRefs=True).
//  2. Call pkg/bnk.ResyncHTTPRoutes — idempotent pool-member workaround.
//  3. Run SSH+EICE curl probes via internal/jumphost.
//  4. Assert 5/5 HTTP 200 + body contains marker.
package httproutee2e

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
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
	scnName      = "http-routing-e2e"
	scnTitle     = "HTTP traffic steering with Gateway API HTTPRoute (how-to #8)"
	scnNamespace = "awsbnkctl-scn-httproute-e2e"
	// scnHostname must match the hostnames: value in manifests/05-httproute.yaml.
	scnHostname = "awsbnkctl.local"
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
	RunCurlProbesFn           func(ctx context.Context, sctx *scenarios.Context, vip string, iterations int, timeout time.Duration) (bool, string)
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
		RunCurlProbesFn: func(_ context.Context, sctx *scenarios.Context, vip string, iterations int, timeout time.Duration) (bool, string) {
			instanceID := sctx.State.Get("JUMPHOST_INSTANCE_ID")
			sourceIP := sctx.State.Get("JUMPHOST_BNK_EXT_ENI_IP")
			probeOpts := jumphost.ProbeOptions{
				Region:     sctx.Cluster.Metadata.Region,
				InstanceID: instanceID,
				SourceIP:   sourceIP,
				VIP:        vip,
				Iterations: iterations,
				Timeout:    timeout,
				Hostname:   scnHostname,
			}
			probes, probeRunErr := jumphost.RunCurlProbes(sctx.Ctx, probeOpts)
			successCount := 0
			var lastErrStr string
			for _, p := range probes {
				if p.HTTPCode == 200 && p.Err == "" {
					successCount++
				} else if p.Err != "" {
					lastErrStr = p.Err
				}
			}
			curlOK := probeRunErr == nil && successCount == iterations
			got := fmt.Sprintf("%d/%d curls returned HTTP 200", successCount, iterations)
			if probeRunErr != nil {
				got += " — probe error: " + probeRunErr.Error()
			} else if !curlOK && lastErrStr != "" {
				got += " — last error: " + lastErrStr
			}
			return curlOK, got
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
End-to-end HTTPRoute scenario with real data-plane traffic via AWS EICE jumphost.

Applies 5 templated manifests into the scenario namespace:
  Namespace, GatewaySettings, nginx Deployment+Service,
  Gateway (spec.addresses=[VIP]), HTTPRoute (host=awsbnkctl.local → nginx).

Verify order (load-bearing):
  1. Wait for control-plane conditions: nginx Available, Gateway Programmed=True,
     HTTPRoute Accepted=True + ResolvedRefs=True.
  2. Call pkg/bnk.ResyncHTTPRoutes — idempotent pool-member workaround for
     cne-controller stale pool-member bug (project_pool_member_sync_root_cause).
  3. Open SSH via EICE to the jumphost and curl --interface <BNK_EXT_ENI_IP>
     http://<VIP>/ N times (default 5).
  4. Assert: N/N HTTP 200.

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

var gatewaySettingsGVR = schema.GroupVersionResource{
	Group:    "gateway.k8s.f5.com",
	Version:  "v1alpha1",
	Resource: "gatewaysettings",
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
	// Order is load-bearing: control-plane must be settled before ResyncHTTPRoutes.

	// nginx Deployment Available.
	err := d.WaitDeploymentAvailableFn(ctx.Ctx, ctx, ns, "nginx", 3*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "nginx Deployment Available",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// Gateway Programmed=True.
	err = d.WaitConditionFn(ctx.Ctx, ctx, scenarios.GatewayGVR, ns, "scn-gateway", "Programmed", 5*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "Gateway scn-gateway Programmed=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// HTTPRoute Accepted=True.
	err = d.WaitHTTPRouteConditionFn(ctx.Ctx, ctx, ns, "scn-route", "Accepted", 3*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "HTTPRoute scn-route Accepted=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// HTTPRoute ResolvedRefs=True.
	err = d.WaitHTTPRouteConditionFn(ctx.Ctx, ctx, ns, "scn-route", "ResolvedRefs", 3*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "HTTPRoute scn-route ResolvedRefs=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// Best-effort: GatewaySettings present (skipped when Dynamic client is nil).
	if ctx.Dynamic != nil {
		_, ferr := ctx.Dynamic.Resource(gatewaySettingsGVR).Namespace(ns).Get(ctx.Ctx, "awsbnkctl-default", metav1.GetOptions{})
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "GatewaySettings awsbnkctl-default present",
			OK:          ferr == nil,
			Got:         scenarios.ErrString(ferr),
		})
	}

	// --- Step 2: ResyncHTTPRoutes (after control-plane is ready) ---
	// Idempotent workaround for cne-controller pool-member stale bug.
	// ResyncHTTPRoutes MUST run after HTTPRoute conditions are settled; moving
	// it before the condition waits regresses the pool-member fix.
	resyncErr := d.ResyncHTTPRoutesFn(ctx.Ctx, ctx, ns)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "ResyncHTTPRoutes (pool-member refresh)",
		OK:          resyncErr == nil,
		Got:         scenarios.ErrString(resyncErr),
	})

	// --- Step 3: SSH+EICE curl probes ---
	vip, iterations, timeout, probeErr := scenarios.BuildProbeParams(ctx)
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

	curlOK, got := d.RunCurlProbesFn(ctx.Ctx, ctx, vip, iterations, timeout)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: fmt.Sprintf("%d/%d end-to-end curls via Gateway return HTTP 200", iterations, iterations),
		OK:          curlOK,
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
	v.VIP = vip
	return v, nil
}
