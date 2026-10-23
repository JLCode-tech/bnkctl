// Package proxyprotocoll4 implements scenario "proxy-protocol-l4" — passing the
// real client connection info to a backend over an L4 (TCP) route via a
// PROXY-protocol iRule (how-to #9). The novelty vs the HTTP scenarios is three
// F5-specific CRs working together on a TCP listener:
//
//   - F5BigCneIrule (group: k8s.f5net.com, kind: F5BigCneIrule) carrying a TCL
//     iRule that, on SERVER_CONNECTED, prepends a PROXY v1 header containing the
//     captured client addr/port + local addr/port.
//   - NetPolicy (group: gateway.k8s.f5.com/v1alpha1) whose extensionRefs
//     attach the iRule and whose targetRefs bind it to the Gateway's TCP listener.
//   - L4Route (group: gateway.k8s.f5.com/v1, protocol TCP) routing the
//     listener to the nginx backend Service.
//
// nginx is configured with `listen 80 proxy_protocol`, so it parses the PROXY
// header and exposes the real client IP as $proxy_protocol_addr. The end-to-end
// curl from the jumphost (source IP = JUMPHOST_BNK_EXT_ENI_IP) should see the
// backend echo back THAT source IP — proving the PROXY header was applied.
// Without the iRule, nginx would instead see TMM's SNAT address.
//
// AWS-specific shape mirrors httptrafficsplit / externalresourcepool:
//   - GatewayClass provisioned by Phase 23b (<cluster>-gatewayclass).
//   - GatewaySettings is owned by the scenario (02-gatewaysettings.yaml);
//     single address (VIP only, .103) so it does not collide with other
//     scenarios' pools (.100 e2e / .101 split / .102 ext-pool).
//   - Verification curls through SSH+EICE from the jumphost's BNK_EXT ENI.
//
// Verify order (load-bearing):
//  1. Wait proxy-backend Deployment Available.
//  2. Wait Gateway scn-proxyproto-gateway Programmed=True.
//  3. Wait L4Route scn-proxyproto-route Accepted=True.
//  4. Best-effort confirm the F5BigCneIrule + NetPolicy CRs exist.
//  5. Probe: curl the VIP (no Host header — this is L4/TCP) N times reading the
//     body; assert >=1 returns HTTP 200 AND the body contains the jumphost
//     source IP (proving proxy_protocol_addr == real client IP).
//
// NOTE: this is L4/TCP — there is NO HTTP hostname (curl sends no Host header)
// and NO ResyncHTTPRoutes (that is an HTTP-only pool-member workaround).
package proxyprotocoll4

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

	"github.com/JLCode-tech/bnkctl/pkg/jumphost"
	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
)

//go:embed manifests/*.yaml
var manifestFS embed.FS

const (
	scnName      = "proxy-protocol-l4"
	scnTitle     = "Proxy Protocol iRule support for L4 routes (how-to #9)"
	scnNamespace = "awsbnkctl-scn-proxyproto"
)

// l4RouteGVR is the BNK L4Route CR (Gateway API extension). Verified against a
// live BNK cluster: group gateway.k8s.f5.com, version v1, resource l4routes.
var l4RouteGVR = schema.GroupVersionResource{
	Group:    "gateway.k8s.f5.com",
	Version:  "v1",
	Resource: "l4routes",
}

// bnkNetPolicyGVR is the NetPolicy CR that attaches the iRule to the Gateway.
var bnkNetPolicyGVR = schema.GroupVersionResource{
	Group:    "gateway.k8s.f5.com",
	Version:  "v1alpha1",
	Resource: "netpolicies",
}

// f5BigCneIruleGVR is the F5BigCneIrule CR carrying the PROXY-protocol iRule.
var f5BigCneIruleGVR = schema.GroupVersionResource{
	Group:    "k8s.f5net.com",
	Version:  "v1",
	Resource: "f5-big-cne-irules",
}

func init() { scenarios.Register(&scenario{}) }

// VerifyDeps holds the function pointers used by Verify. The zero value routes
// every call to the real package-level implementations. Tests swap individual
// fields to a recording stub to assert call order without touching the cluster.
type VerifyDeps struct {
	WaitDeploymentAvailableFn func(ctx context.Context, sctx *scenarios.Context, ns, name string, timeout time.Duration) error
	WaitConditionFn           func(ctx context.Context, sctx *scenarios.Context, gvr schema.GroupVersionResource, ns, name, condType string, timeout time.Duration) error
	WaitL4RouteConditionFn    func(ctx context.Context, sctx *scenarios.Context, ns, name, condType string, timeout time.Duration) error
	IrulePresentFn            func(ctx context.Context, sctx *scenarios.Context, ns, name string) error
	NetPolicyPresentFn        func(ctx context.Context, sctx *scenarios.Context, ns, name string) error
	// RunBodyProbesFn curls the VIP (no Host header) and reports whether the
	// expected client IP was seen on at least one HTTP 200 response, plus a
	// human-readable summary string. The "wantIP" is the jumphost source IP.
	RunBodyProbesFn func(ctx context.Context, sctx *scenarios.Context, vip, wantIP string, iterations int, timeout time.Duration) (seen bool, got string)
}

func realVerifyDeps() VerifyDeps {
	return VerifyDeps{
		WaitDeploymentAvailableFn: scenarios.WaitDeploymentAvailable,
		WaitConditionFn:           scenarios.WaitCondition,
		WaitL4RouteConditionFn:    waitL4RouteCondition,
		IrulePresentFn: func(ctx context.Context, sctx *scenarios.Context, ns, name string) error {
			_, err := sctx.Dynamic.Resource(f5BigCneIruleGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
			return err
		},
		NetPolicyPresentFn: func(ctx context.Context, sctx *scenarios.Context, ns, name string) error {
			_, err := sctx.Dynamic.Resource(bnkNetPolicyGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
			return err
		},
		RunBodyProbesFn: func(_ context.Context, sctx *scenarios.Context, vip, wantIP string, iterations int, timeout time.Duration) (bool, string) {
			probeOpts := probeOptionsFrom(sctx)
			probeOpts.VIP = vip
			probeOpts.Iterations = iterations
			probeOpts.Timeout = timeout
			// L4/TCP: no Host header.
			probeOpts.Hostname = ""
			probes, probeRunErr := jumphost.RunCurlBodyProbes(sctx.Ctx, probeOpts)
			var seen bool
			var lastErrStr string
			for _, p := range probes {
				if p.HTTPCode == 200 && wantIP != "" && strings.Contains(p.Body, wantIP) {
					seen = true
				}
				if p.Err != "" {
					lastErrStr = p.Err
				}
			}
			got := fmt.Sprintf("%d probes: client IP %q seen on 200=%v", len(probes), wantIP, seen)
			if probeRunErr != nil {
				got += " — probe error: " + probeRunErr.Error()
			} else if !seen && lastErrStr != "" {
				got += " — last error: " + lastErrStr
			}
			return seen, got
		},
	}
}

// probeOptionsFrom builds jumphost.ProbeOptions from scenario state. This L4
// scenario never sets a Host header (the route is purely TCP).
func probeOptionsFrom(sctx *scenarios.Context) jumphost.ProbeOptions {
	region := ""
	if sctx.Cluster != nil {
		region = sctx.Cluster.Metadata.Region
	}
	return jumphost.ProbeOptions{
		Region:     region,
		InstanceID: sctx.State.Get("JUMPHOST_INSTANCE_ID"),
		SourceIP:   sctx.State.Get("JUMPHOST_BNK_EXT_ENI_IP"),
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
Proxy-Protocol L4 scenario exercising a PROXY-protocol iRule on an L4 (TCP) route
(how-to #9). Three F5 CRs cooperate: an F5BigCneIrule carrying the TCL that
prepends a PROXY v1 header on SERVER_CONNECTED, a NetPolicy that attaches that
iRule to the Gateway's TCP listener (extensionRefs → iRule, targetRefs → Gateway),
and an L4Route (protocol TCP) routing the listener to the nginx backend.

nginx listens with proxy_protocol, so it parses the PROXY header and exposes the
real client IP as $proxy_protocol_addr, echoing it in the response body. The
end-to-end curl from the jumphost's BNK_EXT ENI (JUMPHOST_BNK_EXT_ENI_IP) should
see the backend echo back THAT source IP — proving the PROXY header was applied
(without it, nginx would see TMM's SNAT address instead).

Applies 7 templated manifests into the scenario namespace (ordered so the
namespace, iRule, NetPolicy, and backend exist before the Gateway/L4Route that
reference them): Namespace, GatewaySettings (listener context),
nginx proxy_protocol backend, F5BigCneIrule, NetPolicy, Gateway (one TCP
listener), L4Route (protocol TCP → proxy-backend:80).

Verify order (load-bearing):
  1. proxy-backend Deployment Available.
  2. Gateway scn-proxyproto-gateway Programmed=True.
  3. L4Route scn-proxyproto-route Accepted=True.
  4. Best-effort confirm the F5BigCneIrule + NetPolicy CRs exist.
  5. SSH via EICE to the jumphost and curl --interface <BNK_EXT_ENI_IP>
     http://<VIP>/ N times (no Host header — L4), reading the body.
  6. Assert: >=1 curl returns HTTP 200 AND the body contains the jumphost
     source IP (proving proxy_protocol_addr == the real client IP).

Cleanup: delete the namespace (idempotent).
Requires: testing.jumphost.enabled=true in cluster.yaml + awsbnkctl up first.
`)
}

// manifestVars holds the template variables for the manifests.
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

	// --- Step 1: backend Deployment Available ---
	err := d.WaitDeploymentAvailableFn(ctx.Ctx, ctx, ns, "proxy-backend", 3*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "proxy-backend Deployment Available",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// --- Step 2: Gateway Programmed=True ---
	err = d.WaitConditionFn(ctx.Ctx, ctx, scenarios.GatewayGVR, ns, "scn-proxyproto-gateway", "Programmed", 5*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "Gateway scn-proxyproto-gateway Programmed=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// --- Step 3: L4Route Accepted=True ---
	err = d.WaitL4RouteConditionFn(ctx.Ctx, ctx, ns, "scn-proxyproto-route", "Accepted", 3*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "L4Route scn-proxyproto-route Accepted=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// --- Step 4: iRule + NetPolicy present (best-effort Get) ---
	err = d.IrulePresentFn(ctx.Ctx, ctx, ns, "pp-prepend")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "F5BigCneIrule pp-prepend present",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	err = d.NetPolicyPresentFn(ctx.Ctx, ctx, ns, "scn-proxyproto-attach")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "NetPolicy scn-proxyproto-attach present",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// --- Step 5: Body-capturing SSH+EICE curl probes ---
	vip, iterations, timeout, probeErr := scenarios.BuildProbeParams(ctx)
	if probeErr != nil {
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "jumphost probe setup",
			OK:          false,
			Got:         probeErr.Error(),
		})
		return scenarios.FinalizeResult(res)
	}
	// Use the scenario's distinct .103 VIP, not the default.
	vip = withLastOctet(vip, "103")

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

	seen, got := d.RunBodyProbesFn(ctx.Ctx, ctx, vip, sourceIP, iterations, timeout)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "L4 PROXY-protocol: backend sees real client IP via Gateway",
		OK:          seen,
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

// waitL4RouteCondition polls the L4Route for the named condition with
// status=True. It inspects BOTH the per-parent list (.status.parents[*].conditions)
// AND the flat top-level list (.status.conditions), so a True condition in either
// location satisfies the wait. This dual-path approach guards against uncertainty
// in the BNK L4Route status schema: some BNK versions surface conditions only at
// the top level, others use the Gateway-API per-parent shape.
func waitL4RouteCondition(ctx context.Context, sctx *scenarios.Context, ns, name, condType string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		obj, err := sctx.Dynamic.Resource(l4RouteGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			// Path 1: per-parent conditions (.status.parents[*].conditions).
			parents, _, _ := scenarios.NestedSlice(obj.Object, "status", "parents")
			for _, pRaw := range parents {
				p, ok := pRaw.(map[string]interface{})
				if !ok {
					continue
				}
				conditions, _, _ := scenarios.NestedSlice(p, "conditions")
				for _, cRaw := range conditions {
					c, ok2 := cRaw.(map[string]interface{})
					if !ok2 {
						continue
					}
					if c["type"] == condType && c["status"] == "True" {
						return nil
					}
				}
			}
			// Path 2: flat top-level conditions (.status.conditions[*]).
			conditions, _, _ := scenarios.NestedSlice(obj.Object, "status", "conditions")
			for _, cRaw := range conditions {
				c, ok := cRaw.(map[string]interface{})
				if !ok {
					continue
				}
				if c["type"] == condType && c["status"] == "True" {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("L4Route %s/%s condition %s not True after %s", ns, name, condType, timeout)
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
	// Use .103 to avoid colliding with httproutee2e (.100), http-traffic-split
	// (.101), and external-resource-pool (.102).
	v.VIP = withLastOctet(vip, strconv.Itoa(103))
	return v, nil
}
