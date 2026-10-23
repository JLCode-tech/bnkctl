package udpl4lb

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
)

//go:embed manifests/*.yaml
var manifestFS embed.FS

const (
	scnName      = "udp-l4-loadbalance"
	scnTitle     = "UDP load balancer via L4Route (UDP echo backend)"
	scnNamespace = "awsbnkctl-scn-udplb"
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
}

func realVerifyDeps() VerifyDeps {
	return VerifyDeps{
		WaitDeploymentAvailableFn: scenarios.WaitDeploymentAvailable,
		WaitConditionFn:           scenarios.WaitCondition,
	}
}

type scenario struct {
	vDeps *VerifyDeps
}

func (s *scenario) Name() string             { return scnName }
func (s *scenario) Title() string            { return scnTitle }
func (s *scenario) Rating() scenarios.Rating { return scenarios.Amber }
func (s *scenario) Dependencies() []string   { return []string{} }
func (s *scenario) Description() string {
	return strings.TrimSpace(`
Exercises L4Route's UDP load-balancing configuration with a 2-replica socat UDP echo backend.

Amber: control plane only. Verify asserts the backend is Available, the Gateway is
Programmed and the L4Route Accepted; no datagram is sent through the VIP. Promoting
to Green needs a UDP client on the jumphost (socat/nc) driving VIP:5353 and checking
the echo — see docs/SCENARIOS.md.
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
	vip := "10.0.10.107"
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
	vip = scenarios.WithLastOctet(vip, "107")
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
	err := deps.WaitDeploymentAvailableFn(ctx.Ctx, ctx, ns, "udp-echo", 60*time.Second)
	assertions = append(assertions, scenarios.Assertion{
		Description: "udp-echo deployment Available=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// 2. Gateway Programmed
	err = deps.WaitConditionFn(ctx.Ctx, ctx, scenarios.GatewayGVR, ns, "scn-udp-lb-gateway", "Programmed", 60*time.Second)
	assertions = append(assertions, scenarios.Assertion{
		Description: "scn-udp-lb-gateway Gateway Programmed=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// 3. L4Route Accepted
	err = deps.WaitConditionFn(ctx.Ctx, ctx, l4RouteGVR, ns, "scn-udp-lb-route", "Accepted", 60*time.Second)
	assertions = append(assertions, scenarios.Assertion{
		Description: "scn-udp-lb-route L4Route Accepted=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	res := scenarios.Result{
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
