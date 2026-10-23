package clusterwidewatch

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/JLCode-tech/bnkctl/pkg/jumphost"
	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
)

//go:embed manifests/*.yaml
var manifestFS embed.FS

const (
	scnName      = "cluster-wide-watch"
	scnTitle     = "Components needing cluster-wide access (how-to #2) — single controller reconciling a new namespace"
	scnNamespace = "awsbnkctl-scn-cwatch"
)

func init() { scenarios.Register(&scenario{}) }

type VerifyDeps struct {
	WaitDeploymentAvailableFn func(ctx context.Context, sctx *scenarios.Context, ns, name string, timeout time.Duration) error
	WaitConditionFn           func(ctx context.Context, sctx *scenarios.Context, gvr schema.GroupVersionResource, ns, name, condType string, timeout time.Duration) error
	WaitHTTPRouteConditionFn  func(ctx context.Context, sctx *scenarios.Context, ns, name, condType string, timeout time.Duration) error
	// RunCurlProbesFn drives HTTP through the VIP from the jumphost's data-path
	// ENI (same probe as http-routing-e2e). nil skips the data-path step —
	// unit tests that stub only the three waits keep their three assertions.
	RunCurlProbesFn func(ctx context.Context, sctx *scenarios.Context, vip string, iterations int, timeout time.Duration) (bool, string)
}

// scnHostname must match hostnames: in manifests/05-httproute.yaml.
const scnHostname = "cwatch.awsbnkctl.local"

func realVerifyDeps() VerifyDeps {
	return VerifyDeps{
		WaitDeploymentAvailableFn: scenarios.WaitDeploymentAvailable,
		WaitConditionFn:           scenarios.WaitCondition,
		WaitHTTPRouteConditionFn:  scenarios.WaitHTTPRouteCondition,
		RunCurlProbesFn: func(_ context.Context, sctx *scenarios.Context, vip string, iterations int, timeout time.Duration) (bool, string) {
			probes, probeRunErr := jumphost.RunCurlProbes(sctx.Ctx, jumphost.ProbeOptions{
				Region:     sctx.Cluster.Metadata.Region,
				InstanceID: sctx.State.Get("JUMPHOST_INSTANCE_ID"),
				SourceIP:   sctx.State.Get("JUMPHOST_BNK_EXT_ENI_IP"),
				VIP:        vip,
				Iterations: iterations,
				Timeout:    timeout,
				Hostname:   scnHostname,
			})
			successCount, lastErrStr := 0, ""
			for _, p := range probes {
				if p.HTTPCode == 200 && p.Err == "" {
					successCount++
				} else if p.Err != "" {
					lastErrStr = p.Err
				}
			}
			ok := probeRunErr == nil && successCount == iterations
			got := fmt.Sprintf("%d/%d curls returned HTTP 200", successCount, iterations)
			if probeRunErr != nil {
				got += " — probe error: " + probeRunErr.Error()
			} else if !ok && lastErrStr != "" {
				got += " — last error: " + lastErrStr
			}
			return ok, got
		},
	}
}

type scenario struct {
	vDeps *VerifyDeps
}

func (s *scenario) Name() string             { return scnName }
func (s *scenario) Title() string            { return scnTitle }
func (s *scenario) Rating() scenarios.Rating { return scenarios.Green }
func (s *scenario) Dependencies() []string   { return []string{} }
func (s *scenario) Description() string {
	return strings.TrimSpace(`
Demonstrates BNK's cluster-wide watch capabilities: a single F5 controller reconciles Gateways
and HTTPRoutes across multiple isolated tenant namespaces without requiring per-namespace controller installs.
`)
}

func (s *scenario) Namespace(ctx *scenarios.Context) string {
	if ns := ctx.Options["namespace"]; ns != "" {
		return ns
	}
	return scnNamespace
}

type templateData struct {
	Namespace        string
	GatewayClassName string
	VIP              string
	ExternalCIDR     string
}

func (s *scenario) renderData(ctx *scenarios.Context) (templateData, error) {
	ns := s.Namespace(ctx)
	gwClass := "f5-cne"
	vip := "10.0.10.105"
	cidr := "10.0.10.0/24"

	if ctx.Cluster != nil {
		if ctx.Cluster.Metadata.Name != "" {
			gwClass = ctx.Cluster.Metadata.Name + "-gatewayclass"
		}
		if ctx.Cluster.Network.DataPath.External.CIDR != "" {
			cidr = ctx.Cluster.Network.DataPath.External.CIDR
		}
		if derivedVIP, err := ctx.Cluster.DefaultVIP(); err == nil && derivedVIP != "" {
			vip = derivedVIP
		}
	}
	if v := ctx.Options["vip"]; v != "" {
		vip = v
	}
	// Pin this scenario's octet (see docs/SCENARIOS.md, "VIP plan") so it never
	// shares a pool address with http-routing-e2e (.100) or any other scenario.
	vip = scenarios.WithLastOctet(vip, "105")
	return templateData{
		Namespace:        ns,
		GatewayClassName: gwClass,
		VIP:              vip,
		ExternalCIDR:     cidr,
	}, nil
}

func (s *scenario) Manifests(ctx *scenarios.Context) ([]string, error) {
	td, err := s.renderData(ctx)
	if err != nil {
		return nil, err
	}
	var paths []string
	err = fs.WalkDir(manifestFS, "manifests", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		body, rerr := manifestFS.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		rendered, renErr := scenarios.RenderTemplate(string(body), td)
		if renErr != nil {
			return fmt.Errorf("rendering %s: %w", p, renErr)
		}
		base := p[len("manifests/"):]
		outPath, werr := scenarios.WriteManifest(ctx.WorkspaceDir, scnName, base, rendered)
		if werr != nil {
			return werr
		}
		paths = append(paths, outPath)
		return nil
	})
	return paths, err
}

func (s *scenario) Apply(ctx *scenarios.Context) error {
	return scenarios.ApplyManifests(ctx, scnName)
}

func (s *scenario) Verify(ctx *scenarios.Context) scenarios.Result {
	ns := s.Namespace(ctx)
	deps := realVerifyDeps()
	if s.vDeps != nil {
		deps = *s.vDeps
	}

	var assertions []scenarios.Assertion

	// 1. Deployment Available
	err := deps.WaitDeploymentAvailableFn(ctx.Ctx, ctx, ns, "nginx", 60*time.Second)
	assertions = append(assertions, scenarios.Assertion{
		Description: "nginx deployment Available=True in newly created namespace",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// 2. Gateway Programmed
	err = deps.WaitConditionFn(ctx.Ctx, ctx, scenarios.GatewayGVR, ns, "scn-cwatch-gateway", "Programmed", 60*time.Second)
	assertions = append(assertions, scenarios.Assertion{
		Description: "scn-cwatch-gateway Gateway Programmed=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// 3. HTTPRoute Accepted
	err = deps.WaitHTTPRouteConditionFn(ctx.Ctx, ctx, ns, "scn-cwatch-route", "Accepted", 60*time.Second)
	assertions = append(assertions, scenarios.Assertion{
		Description: "scn-cwatch-route HTTPRoute Accepted=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// 4. Data path: the point of cluster-wide watch is that a Gateway in a
	// namespace the controller was not installed for actually carries traffic.
	// Same jumphost curl the HTTP scenarios use; the Green rating rests on it.
	if deps.RunCurlProbesFn != nil {
		vip, iterations, timeout, probeErr := scenarios.BuildProbeParams(ctx)
		vip = scenarios.WithLastOctet(vip, "105")
		switch {
		case probeErr != nil:
			assertions = append(assertions, scenarios.Assertion{Description: "jumphost probe setup", OK: false, Got: probeErr.Error()})
		case ctx.State == nil || ctx.State.Get("JUMPHOST_INSTANCE_ID") == "" || ctx.State.Get("JUMPHOST_BNK_EXT_ENI_IP") == "":
			assertions = append(assertions, scenarios.Assertion{
				Description: "jumphost state keys present",
				OK:          false,
				Got:         "JUMPHOST_INSTANCE_ID / JUMPHOST_BNK_EXT_ENI_IP missing from state.env — run `awsbnkctl up` with testing.jumphost.enabled=true",
			})
		default:
			ok, got := deps.RunCurlProbesFn(ctx.Ctx, ctx, vip, iterations, timeout)
			assertions = append(assertions, scenarios.Assertion{
				Description: fmt.Sprintf("%d/%d end-to-end curls via the cluster-wide-watch Gateway return HTTP 200", iterations, iterations),
				OK:          ok,
				Got:         got,
			})
		}
	}

	res := scenarios.Result{
		DataPath:   true,
		Assertions: assertions,
	}
	return scenarios.FinalizeResult(res)
}

func (s *scenario) Cleanup(ctx *scenarios.Context) error {
	ns := s.Namespace(ctx)
	if ctx.Clientset == nil {
		return nil
	}
	err := ctx.Clientset.CoreV1().Namespaces().Delete(ctx.Ctx, ns, metav1.DeleteOptions{})
	if err != nil && scenarios.IsNotFound(err) {
		return nil
	}
	return err
}
