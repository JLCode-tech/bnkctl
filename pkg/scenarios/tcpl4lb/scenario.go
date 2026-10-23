package tcpl4lb

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
	scnName      = "tcp-l4-loadbalance"
	scnTitle     = "TCP load balancer via L4Route — weighted backends (70/30)"
	scnNamespace = "awsbnkctl-scn-tcplb"
	gwPort       = "8080"
)

var l4RouteGVR = schema.GroupVersionResource{
	Group:    "gateway.k8s.f5.com",
	Version:  "v1",
	Resource: "l4routes",
}

func init() { scenarios.Register(&scenario{}) }

type VerifyDeps struct {
	WaitDeploymentAvailableFn func(ctx context.Context, sctx *scenarios.Context, ns, name string, timeout time.Duration) error
	WaitConditionFn           func(ctx context.Context, sctx *scenarios.Context, gvr schema.GroupVersionResource, ns, name, condType string, timeout time.Duration) error
	RunCurlProbesFn           func(ctx context.Context, sctx *scenarios.Context, vip, port string, iterations int, timeout time.Duration) (map[string]int, error)
}

func realVerifyDeps() VerifyDeps {
	return VerifyDeps{
		WaitDeploymentAvailableFn: scenarios.WaitDeploymentAvailable,
		WaitConditionFn:           scenarios.WaitCondition,
		RunCurlProbesFn: func(ctx context.Context, sctx *scenarios.Context, vip, port string, iterations int, timeout time.Duration) (map[string]int, error) {
			instanceID := sctx.State.Get("JUMPHOST_INSTANCE_ID")
			sourceIP := sctx.State.Get("JUMPHOST_BNK_EXT_ENI_IP")
			probeOpts := jumphost.ProbeOptions{
				Region:     sctx.Cluster.Metadata.Region,
				InstanceID: instanceID,
				SourceIP:   sourceIP,
				VIP:        vip + ":" + port,
				Iterations: iterations,
				Timeout:    timeout,
			}
			probes, err := jumphost.RunCurlBodyProbes(ctx, probeOpts)
			if err != nil {
				return nil, err
			}
			counts := make(map[string]int)
			for _, p := range probes {
				if p.HTTPCode == 200 {
					if strings.Contains(p.Body, "backend=A") {
						counts["A"]++
					}
					if strings.Contains(p.Body, "backend=B") {
						counts["B"]++
					}
				}
			}
			return counts, nil
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
Exercises L4Route's load-balancing behavior with two weighted backends (70/30).
Each backend is a separate nginx Deployment serving a distinct marker body
(backend=A / backend=B), asserting that L4Route weighted traffic splitting reaches both endpoints.
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
	vip := "10.0.10.106"
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
	vip = scenarios.WithLastOctet(vip, "106")
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

	// 1. Deployments Available
	errA := deps.WaitDeploymentAvailableFn(ctx.Ctx, ctx, ns, "tcp-lb-a", 60*time.Second)
	errB := deps.WaitDeploymentAvailableFn(ctx.Ctx, ctx, ns, "tcp-lb-b", 60*time.Second)
	assertions = append(assertions, scenarios.Assertion{
		Description: "tcp-lb-a and tcp-lb-b deployments Available=True",
		OK:          errA == nil && errB == nil,
		Got:         fmt.Sprintf("A=%v, B=%v", scenarios.ErrString(errA), scenarios.ErrString(errB)),
	})

	// 2. Gateway Programmed
	err := deps.WaitConditionFn(ctx.Ctx, ctx, scenarios.GatewayGVR, ns, "scn-tcp-lb-gateway", "Programmed", 60*time.Second)
	assertions = append(assertions, scenarios.Assertion{
		Description: "scn-tcp-lb-gateway Gateway Programmed=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// 3. L4Route Accepted
	err = deps.WaitConditionFn(ctx.Ctx, ctx, l4RouteGVR, ns, "scn-tcp-lb-route", "Accepted", 60*time.Second)
	assertions = append(assertions, scenarios.Assertion{
		Description: "scn-tcp-lb-route L4Route Accepted=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// 4. Data-plane probe: curls hitting both backends
	td, _ := s.renderData(ctx)
	counts, probeErr := deps.RunCurlProbesFn(ctx.Ctx, ctx, td.VIP, gwPort, 20, 10*time.Second)
	bothServed := probeErr == nil && counts["A"] > 0 && counts["B"] > 0
	gotStr := fmt.Sprintf("backend A=%d, backend B=%d", counts["A"], counts["B"])
	if probeErr != nil {
		gotStr += " — probe error: " + probeErr.Error()
	}
	assertions = append(assertions, scenarios.Assertion{
		Description: "TCP traffic split reached both backend A and backend B",
		OK:          bothServed,
		Got:         gotStr,
	})

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
