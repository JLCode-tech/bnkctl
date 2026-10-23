// Package externalresourcepool implements scenario "external-resource-pool" —
// load-balancing HTTP traffic to an off-cluster backend via a Kubernetes
// EndpointSlice (how-to #10). BNK 2.3 has NO Pool CRD; the documented way to
// load-balance to an *external* resource is a selectorless Service plus a
// manually-managed EndpointSlice whose endpoint is the external IP. The
// novelty vs http-routing-e2e / http-traffic-split is exactly that: the
// HTTPRoute's backendRef points at a selectorless Service (no Pods, no
// selector), and the hand-written EndpointSlice carries an *external* IP —
// here, the jumphost's BNK_EXT ENI IP (JUMPHOST_BNK_EXT_ENI_IP),
// which sits on the same L2 as the VIP and is therefore directly TMM-reachable.
//
// AWS-specific shape mirrors httptrafficsplit:
//   - GatewayClass provisioned by Phase 23b (<cluster>-gatewayclass).
//   - GatewaySettings is owned by the scenario (02-gatewaysettings.yaml);
//     single address (VIP only, .102) so it does not collide with other
//     scenarios' pools (.100 e2e / .101 split).
//   - The "external" backend is a tiny python3 http.server started on the
//     jumphost (port 8080, marker "external-resource-pool-OK").
//   - Verification curls through SSH+EICE from the jumphost's BNK_EXT ENI,
//     reading the response body to confirm the marker is served via the VIP.
//
// Verify order (load-bearing):
//  1. StartHTTPResponder on the jumphost FIRST so the EndpointSlice endpoint is
//     live before TMM health-checks it.
//  2. Wait Gateway scn-extpool-gateway Programmed=True.
//  3. Wait HTTPRoute scn-extpool-route Accepted=True.
//  4. Wait HTTPRoute scn-extpool-route ResolvedRefs=True.
//  5. ResyncHTTPRoutes — idempotent pool-member workaround.
//  6. Probe: curl the VIP; assert >=1 returns HTTP 200 AND body has the marker.
package externalresourcepool

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
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/jumphost"
	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
)

//go:embed manifests/*.yaml
var manifestFS embed.FS

const (
	scnName      = "external-resource-pool"
	scnTitle     = "Load balance traffic to external resources via EndpointSlice (how-to #10)"
	scnNamespace = "awsbnkctl-scn-extpool"
	// scnHostname must match the hostnames: value in manifests/06-httproute.yaml.
	scnHostname = "awsbnkctl-extpool.local"
	// responderPort + responderMarker define the off-cluster HTTP backend.
	responderPort   = 8080
	responderMarker = "external-resource-pool-OK"
)

func init() { scenarios.Register(&scenario{}) }

// VerifyDeps holds the function pointers used by Verify. The zero value routes
// every call to the real package-level implementations. Tests swap individual
// fields to a recording stub to assert call order without touching the cluster.
type VerifyDeps struct {
	StartHTTPResponderFn     func(ctx context.Context, sctx *scenarios.Context, port int, marker string) error
	WaitConditionFn          func(ctx context.Context, sctx *scenarios.Context, gvr schema.GroupVersionResource, ns, name, condType string, timeout time.Duration) error
	WaitHTTPRouteConditionFn func(ctx context.Context, sctx *scenarios.Context, ns, name, condType string, timeout time.Duration) error
	ResyncHTTPRoutesFn       func(ctx context.Context, sctx *scenarios.Context, ns string) error
	// RunBodyProbesFn curls the VIP and reports whether the marker was seen on
	// at least one HTTP 200 response, plus a human-readable summary string.
	RunBodyProbesFn func(ctx context.Context, sctx *scenarios.Context, vip, host, marker string, iterations int, timeout time.Duration) (seen bool, got string)
}

func realVerifyDeps() VerifyDeps {
	return VerifyDeps{
		StartHTTPResponderFn: func(ctx context.Context, sctx *scenarios.Context, port int, marker string) error {
			opts := probeOptionsFrom(sctx, "")
			return jumphost.StartHTTPResponder(ctx, opts, port, marker)
		},
		WaitConditionFn:          scenarios.WaitCondition,
		WaitHTTPRouteConditionFn: scenarios.WaitHTTPRouteCondition,
		ResyncHTTPRoutesFn: func(ctx context.Context, sctx *scenarios.Context, ns string) error {
			_, err := bnk.ResyncHTTPRoutes(ctx, sctx.Dynamic, bnk.ResyncOptions{
				Namespace:      ns,
				AllInNamespace: true,
			})
			return err
		},
		RunBodyProbesFn: func(_ context.Context, sctx *scenarios.Context, vip, host, marker string, iterations int, timeout time.Duration) (bool, string) {
			probeOpts := probeOptionsFrom(sctx, host)
			probeOpts.VIP = vip
			probeOpts.Iterations = iterations
			probeOpts.Timeout = timeout
			probes, probeRunErr := jumphost.RunCurlBodyProbes(sctx.Ctx, probeOpts)
			var seen bool
			var lastErrStr string
			for _, p := range probes {
				if p.HTTPCode == 200 && strings.Contains(p.Body, marker) {
					seen = true
				}
				if p.Err != "" {
					lastErrStr = p.Err
				}
			}
			got := fmt.Sprintf("%d probes: marker seen on 200=%v", len(probes), seen)
			if probeRunErr != nil {
				got += " — probe error: " + probeRunErr.Error()
			} else if !seen && lastErrStr != "" {
				got += " — last error: " + lastErrStr
			}
			return seen, got
		},
	}
}

// probeOptionsFrom builds jumphost.ProbeOptions from scenario state. host sets
// the Host header (empty = none).
func probeOptionsFrom(sctx *scenarios.Context, host string) jumphost.ProbeOptions {
	region := ""
	if sctx.Cluster != nil {
		region = sctx.Cluster.Metadata.Region
	}
	return jumphost.ProbeOptions{
		Region:     region,
		InstanceID: sctx.State.Get("JUMPHOST_INSTANCE_ID"),
		SourceIP:   sctx.State.Get("JUMPHOST_BNK_EXT_ENI_IP"),
		Hostname:   host,
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
External-resource scenario exercising a Kubernetes EndpointSlice as an HTTPRoute
backend (how-to #10). BNK 2.3 has NO Pool CRD; the documented way to load-balance
to an *external* resource is a selectorless Service plus a manually-managed
EndpointSlice whose single endpoint is an *off-cluster* IP — the
jumphost's BNK_EXT ENI (JUMPHOST_BNK_EXT_ENI_IP), which is on the VIP's L2 and
directly TMM-reachable.

Applies 5 templated manifests into the scenario namespace:
  Namespace, GatewaySettings (listener context),
  Gateway (spec.addresses=[VIP]), selectorless Service ext-backend +
  EndpointSlice ext-backend-1 (endpoint = jumphost BNK_EXT IP:8080),
  HTTPRoute (host=awsbnkctl-extpool.local → backendRef Service ext-backend).

Verify order (load-bearing):
  1. Start a python3 http.server on the jumphost (port 8080) serving a marker —
     done FIRST so the EndpointSlice endpoint is live before TMM health-checks it.
  2. Wait Gateway Programmed=True, HTTPRoute Accepted=True + ResolvedRefs=True.
  3. Call pkg/bnk.ResyncHTTPRoutes — idempotent pool-member workaround.
  4. Open SSH via EICE to the jumphost and curl --interface <BNK_EXT_ENI_IP>
     http://<VIP>/ N times (default 10), reading the response body.
  5. Assert: >=1 curl returns HTTP 200 AND the body contains the marker.

Cleanup: stop the responder (best-effort) then delete the namespace (idempotent).
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
	// BackendIP is the off-cluster EndpointSlice endpoint address
	// (jumphost BNK_EXT ENI).
	BackendIP string
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

	// --- Step 0: Start the off-cluster HTTP responder FIRST ---
	// The EndpointSlice endpoint must be live before TMM health-checks it,
	// otherwise the pool member is marked down and curls return 5xx.
	respErr := d.StartHTTPResponderFn(ctx.Ctx, ctx, responderPort, responderMarker)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "jumphost HTTP responder started",
		OK:          respErr == nil,
		Got:         scenarios.ErrString(respErr),
	})
	if respErr != nil {
		return scenarios.FinalizeResult(res)
	}

	// --- Step 1: Control-plane assertions ---

	// Gateway Programmed=True.
	err := d.WaitConditionFn(ctx.Ctx, ctx, scenarios.GatewayGVR, ns, "scn-extpool-gateway", "Programmed", 5*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "Gateway scn-extpool-gateway Programmed=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// HTTPRoute Accepted=True.
	err = d.WaitHTTPRouteConditionFn(ctx.Ctx, ctx, ns, "scn-extpool-route", "Accepted", 3*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "HTTPRoute scn-extpool-route Accepted=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// HTTPRoute ResolvedRefs=True.
	err = d.WaitHTTPRouteConditionFn(ctx.Ctx, ctx, ns, "scn-extpool-route", "ResolvedRefs", 3*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "HTTPRoute scn-extpool-route ResolvedRefs=True",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// --- Step 2: ResyncHTTPRoutes (after control-plane is ready) ---
	// Idempotent workaround for cne-controller pool-member stale bug.
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
	// Use the scenario's distinct .102 VIP, not the default.
	vip = withLastOctet(vip, "102")

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

	probeIter := iterations
	if probeIter < 10 {
		probeIter = 10
	}

	ok, got := scenarios.PollMarkers(ctx.Ctx, 120*time.Second, 10*time.Second, func() (bool, string) {
		s, g := d.RunBodyProbesFn(ctx.Ctx, ctx, vip, scnHostname, responderMarker, probeIter, timeout)
		return s, g
	})
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "end-to-end curl via Gateway reaches external EndpointSlice backend (HTTP 200 + marker)",
		OK:          ok,
		Got:         got,
	})

	return scenarios.FinalizeResult(res)
}

func (s *scenario) Cleanup(ctx *scenarios.Context) error {
	// Best-effort: stop the jumphost responder. Requires the jumphost state
	// keys; if absent we simply skip (nothing was started).
	if ctx.State != nil && ctx.State.Get("JUMPHOST_INSTANCE_ID") != "" {
		opts := probeOptionsFrom(ctx, "")
		_ = jumphost.StopHTTPResponder(ctx.Ctx, opts, responderPort)
	}

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
	// Use .102 to avoid colliding with httproutee2e (.100) and http-traffic-split (.101).
	v.VIP = withLastOctet(vip, strconv.Itoa(102))

	// BackendIP is the off-cluster Pool member — the jumphost's BNK_EXT ENI IP.
	// Reading state here is allowed (it is NOT k8s I/O).
	if ctx.State != nil {
		v.BackendIP = ctx.State.Get("JUMPHOST_BNK_EXT_ENI_IP")
	}
	if v.BackendIP == "" {
		return v, fmt.Errorf("JUMPHOST_BNK_EXT_ENI_IP empty in state.env — run `awsbnkctl up` with testing.jumphost.enabled=true (the jumphost is this scenario's external Pool backend)")
	}
	if v.BackendIP == v.VIP {
		return v, fmt.Errorf("backend IP (%s) cannot equal Gateway VIP (%s)", v.BackendIP, v.VIP)
	}
	parts := strings.Split(v.BackendIP, ".")
	if len(parts) == 4 {
		if octet, err := strconv.Atoi(parts[3]); err == nil && intent.IsReservedGatewayVIPOffset(octet) {
			return v, fmt.Errorf("backend IP (%s) collides with Gateway VIP range (.100-.119)", v.BackendIP)
		}
	}
	return v, nil
}
