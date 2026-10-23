// Package egresssnat implements scenario "egress-snat" — transparent pod
// egress through TMM with AUTOMAP source translation on the BNK 2.4 egress
// model (EgressGateway, Green).
//
// The worker node has no interface on TMM's VLANs (host-device moves those
// NICs into the TMM pod), so the product carries the traffic over a VXLAN
// tunnel on the pod network: the Infra CR (phase 23b) names the tunnel VLAN in
// egressDefaults and carries the two static routes TMM needs (the VPC via the
// tunnel VLAN's gateway, a default via ext-vlan's). The scenario adds a
// GatewaySettings with one egressConfigs entry (Automap on the tunnel network)
// and an EgressGateway that selects the scenario namespace and references that
// entry through infrastructure.parametersRef.
//
// Verify:
//
//   - egress-client pod becomes Ready.
//   - GatewaySettings Accepted + ResolvedRefs, EgressGateway Programmed.
//   - Data path (gating): curls from the pod to an out-of-VPC target must move
//     TMM's own counters — see verifyTMMEgress. Without an exec client the step
//     is recorded as skipped so the result stays honest.
package egresssnat

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/JLCode-tech/bnkctl/pkg/k8s/render"
	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
)

//go:embed manifests/*.yaml
var manifestFS embed.FS

const (
	scnName      = "egress-snat"
	scnTitle     = "Pod egress via TMM with AUTOMAP SNAT (EgressGateway)"
	scnNamespace = "awsbnkctl-scn-egress"

	// egressName names both the GatewaySettings and the EgressGateway (they
	// share the scenario namespace); egressConfig is the egressConfigs[].name
	// the EgressGateway references as sectionName.
	egressName   = "awsbnkctl-egress"
	egressConfig = "default-egress"
)

// GVRs of the BNK 2.4 egress CRs (gateway.k8s.f5.com/v1alpha1).
var (
	egressGatewayGVR   = schema.GroupVersionResource{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "egressgateways"}
	gatewaySettingsGVR = schema.GroupVersionResource{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "gatewaysettings"}
)

func init() { scenarios.Register(&scenario{}) }

// VerifyDeps holds the function pointers used by Verify. The zero value routes
// every call to the real package-level implementations. Tests swap individual
// fields to a recording stub to assert call order without touching the cluster.
type VerifyDeps struct {
	WaitPodReadyFn func(ctx context.Context, sctx *scenarios.Context, ns, name string, timeout time.Duration) error
	// WaitCRConditionsFn polls a CR until every condition in want is True and
	// returns a one-line summary of its conditions (also on failure).
	WaitCRConditionsFn func(ctx context.Context, sctx *scenarios.Context, gvr schema.GroupVersionResource, ns, name string, want []string, timeout time.Duration) (string, error)
	// The two below implement the data-path proof (see verifyTMMEgress). A nil
	// ExecInPodFn skips it and Verify stays control-plane only, which is what
	// the unit tests exercise.
	TMMPodFn    func(sctx *scenarios.Context) (string, error)
	ExecInPodFn func(sctx *scenarios.Context, ns, pod, container string, command ...string) (string, error)
}

const (
	tmmNamespace      = "f5-cne-system"
	tmmPodSelector    = "app=f5-tmm"
	tmmDebugContainer = "debug" // the TMM pod sidecar that ships tmctl
	egressCurlCount   = 3
	// defaultEgressTarget must be OUTSIDE the VPC: the CSRC DaemonSet keeps
	// pod→VPC traffic on the node ("addVpcCniEastWestRule"), so only external
	// destinations traverse the tunnel. Override with --opt egress-target=URL.
	defaultEgressTarget = "http://checkip.amazonaws.com/"

	gatewaySettingsWait = 2 * time.Minute
	egressGatewayWait   = 3 * time.Minute
)

// Poll budget for the data-path proof (package vars so tests can shrink them).
var (
	sourceIPPollTimeout  = 2 * time.Minute
	sourceIPPollInterval = 10 * time.Second
)

func realVerifyDeps() VerifyDeps {
	return VerifyDeps{
		WaitPodReadyFn:     waitPodReady,
		WaitCRConditionsFn: waitCRConditions,
		TMMPodFn:           tmmPodName,
		ExecInPodFn:        scenarios.ExecInPod,
	}
}

// tmmPodName returns the first Running TMM pod (the scenario runs single-TMM shapes).
func tmmPodName(sctx *scenarios.Context) (string, error) {
	pods, err := sctx.Clientset.CoreV1().Pods(tmmNamespace).List(sctx.Ctx, metav1.ListOptions{LabelSelector: tmmPodSelector})
	if err != nil {
		return "", fmt.Errorf("listing TMM pods: %w", err)
	}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			return p.Name, nil
		}
	}
	return "", fmt.Errorf("no Running pod with %s in %s", tmmPodSelector, tmmNamespace)
}

// expectedSNATSource is the address egress must be SNATed to when captured:
// TMM's external self IP (AUTOMAP picks the egress interface's self IP, and
// the Infra default route pins that interface to the external VLAN). state.env
// carries the address the F5 IPAM controller allocated (phase 23b, e.g. .225;
// confirmed live 2026-09-12 as the AUTOMAP source); the intent's nominal
// value is the fallback.
func expectedSNATSource(sctx *scenarios.Context) string {
	return scenarios.NetVarsFor(sctx.Cluster, sctx.State).TMMExtSelfIP
}

// tmmCounters is what TMM has seen for this scenario's egress: connections on
// the egress virtual server and connections SNATed to the external self IP.
type tmmCounters struct {
	virtual, snat int
	vsName        string   // the virtual server line matched (empty when none)
	vsNames       []string // every virtual server TMM lists, for the failure detail
	snatLines     []string // the snat_automap pool-member lines, for the failure detail
}

// readTMMCounters pulls both counters with tmctl from the TMM debug sidecar.
func readTMMCounters(d *VerifyDeps, sctx *scenarios.Context, tmmPod, ns, name, selfIPHex string) (tmmCounters, error) {
	vs, err := d.ExecInPodFn(sctx, tmmNamespace, tmmPod, tmmDebugContainer,
		"tmctl", "-d", "blade", "-w", "400", "virtual_server_stat", "-s", "name,clientside.tot_conns")
	if err != nil {
		return tmmCounters{}, fmt.Errorf("tmctl virtual_server_stat: %w", err)
	}
	sn, err := d.ExecInPodFn(sctx, tmmNamespace, tmmPod, tmmDebugContainer,
		"tmctl", "-d", "blade", "-w", "400", "pool_member_stat", "-s", "pool_name,addr,serverside.tot_conns")
	if err != nil {
		return tmmCounters{}, fmt.Errorf("tmctl pool_member_stat: %w", err)
	}
	c := tmmCounters{snat: lastIntOnLine(sn, selfIPHex), vsNames: tableFirstColumn(vs)}
	if line := egressVirtualLine(vs, ns, name); line != "" {
		c.vsName = strings.Fields(line)[0]
		c.virtual = lastIntOnLine(line, c.vsName)
	}
	for _, l := range strings.Split(sn, "\n") {
		if strings.Contains(l, "snat_automap") {
			c.snatLines = append(c.snatLines, strings.Join(strings.Fields(l), " "))
		}
	}
	return c, nil
}

// egressVirtualName is the wildcard listener TMM creates for an EgressGateway:
// <ns>-<name>-egress-ipv4, the same pattern the 2.3 F5SPKEgress used
// (confirmed live on BNK 2.4.0, 2026-09-12: awsbnkctl-scn-egress-awsbnkctl-egress-egress-ipv4).
func egressVirtualName(ns, name string) string { return ns + "-" + name + "-egress-ipv4" }

// egressVirtualLine picks the virtual_server_stat line of this scenario's
// EgressGateway: the exact 2.3-style name first, then any IPv4 line that
// carries the EgressGateway name, then one that carries its namespace. Returns
// "" when TMM has no such virtual server.
func egressVirtualLine(out, ns, name string) string {
	lines := strings.Split(out, "\n")
	for _, needle := range []string{egressVirtualName(ns, name), name, ns} {
		for _, l := range lines {
			f := strings.Fields(l)
			if len(f) < 2 || f[0] == "name" || strings.HasPrefix(f[0], "-") {
				continue
			}
			if strings.Contains(f[0], needle) && !strings.Contains(f[0], "ipv6") {
				return l
			}
		}
	}
	return ""
}

// tableFirstColumn returns the first field of every data row of a tmctl table.
func tableFirstColumn(out string) []string {
	var names []string
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 2 || f[0] == "name" || strings.HasPrefix(f[0], "-") {
			continue
		}
		names = append(names, f[0])
	}
	sort.Strings(names)
	return names
}

// lastIntOnLine returns the last whitespace-separated integer on the first
// line of out containing needle, or 0 when there is no such line.
func lastIntOnLine(out, needle string) int {
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, needle) {
			continue
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			return 0
		}
		n, _ := strconv.Atoi(f[len(f)-1])
		return n
	}
	return 0
}

// ipv4Hex renders an IPv4 address the way tmctl prints pool-member addresses
// (IPv4-mapped, 20 bytes), e.g. 10.50.10.240 → "FF:FF:0A:32:0A:F0:00:00:00:00".
func ipv4Hex(ip string) string {
	p := net.ParseIP(ip).To4()
	if p == nil {
		return ""
	}
	return fmt.Sprintf("FF:FF:%02X:%02X:%02X:%02X:00:00:00:00", p[0], p[1], p[2], p[3])
}

// verifyTMMEgress is the data-path proof: curl an out-of-VPC target FROM the
// captured pod and require TMM's own counters to move by the same amount —
// connections on the egress virtual server (traffic traversed the tunnel) and
// connections SNATed to the external self IP (AUTOMAP rewrote the source). It
// reads TMM, not a reflector, because the pseudo-CNI keeps pod→VPC traffic on
// the node, so nothing inside the VPC can observe the self IP. It returns
// (skipped, assertion); skipped is true when there is no exec client, so the
// scenario stays honest rather than red on control-plane-only runs.
func verifyTMMEgress(d *VerifyDeps, sctx *scenarios.Context, podNS string) (bool, scenarios.Assertion) {
	want := expectedSNATSource(sctx)
	if d.ExecInPodFn == nil || d.TMMPodFn == nil || want == "" {
		return true, scenarios.Assertion{
			Description: "data-plane proof (TMM egress virtual + SNAT counters)",
			OK:          true,
			Got:         "skipped — needs a Kubernetes exec client and TMM_EXT_SELFIP in state.env",
		}
	}
	tmmPod, err := d.TMMPodFn(sctx)
	if err != nil {
		return false, scenarios.Assertion{Description: "locate the TMM pod", OK: false, Got: err.Error()}
	}
	target := sctx.Options["egress-target"]
	if target == "" {
		target = defaultEgressTarget
	}
	selfHex := ipv4Hex(want)
	// The controller programs the virtual server and the CSRC DaemonSet the
	// worker-node VXLAN end asynchronously after the CRs are accepted; give
	// them a couple of minutes.
	ok, detail := scenarios.PollMarkers(sctx.Ctx, sourceIPPollTimeout, sourceIPPollInterval, func() (bool, string) {
		before, err := readTMMCounters(d, sctx, tmmPod, podNS, egressName, selfHex)
		if err != nil {
			return false, err.Error()
		}
		okCurls := 0
		for i := 0; i < egressCurlCount; i++ {
			if _, err := d.ExecInPodFn(sctx, podNS, "egress-client", "curl", "curl", "-s", "-o", "/dev/null", "--max-time", "8", target); err == nil {
				okCurls++
			}
		}
		after, err := readTMMCounters(d, sctx, tmmPod, podNS, egressName, selfHex)
		if err != nil {
			return false, err.Error()
		}
		dv, ds := after.virtual-before.virtual, after.snat-before.snat
		pass := okCurls == egressCurlCount && after.vsName != "" && dv >= egressCurlCount && ds >= egressCurlCount
		vs := after.vsName
		if vs == "" {
			vs = "<none matching " + egressName + ">"
		}
		msg := fmt.Sprintf("%d/%d curls from egress-client to %s succeeded; TMM egress virtual %s +%d conns; SNAT to %s +%d conns",
			okCurls, egressCurlCount, target, vs, dv, want, ds)
		if !pass {
			msg += fmt.Sprintf(" [virtual servers on TMM: %s; snat_automap members: %s]",
				strings.Join(after.vsNames, ", "), strings.Join(after.snatLines, " | "))
		}
		return pass, msg
	})
	return false, scenarios.Assertion{
		Description: "pod egress traverses TMM and is SNATed to the external self IP (egress virtual + AUTOMAP counters)",
		OK:          ok,
		Got:         detail,
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
Egress SNAT scenario — transparent pod outbound traffic through TMM with
AUTOMAP source translation on the BNK 2.4 egress model (EgressGateway).

The Infra CR phase 23b applies already names the tunnel VLAN (egressDefaults)
and carries the two static routes TMM needs (the VPC via the tunnel VLAN's
gateway, a default via the external VLAN's). The scenario adds a
GatewaySettings with one egressConfigs entry (Automap on the tunnel network)
and an EgressGateway that selects the scenario namespace; the controller
creates a wildcard virtual server on TMM and the product builds the VXLAN
tunnel from the worker nodes itself. Traffic from the captured namespace to
destinations outside the VPC is carried to TMM and source-translated to the
external self IP; pod-to-VPC traffic stays on the node.

Applies 4 templated manifests, all in the scenario namespace:
  01-namespace.yaml        — scenario namespace (awsbnkctl-scn-egress)
  02-curlpod.yaml          — egress-client pod (curlimages/curl, normal pod network)
  03-gatewaysettings.yaml  — GatewaySettings awsbnkctl-egress: egressConfigs
                             default-egress (Automap, networkRef = tunnel VLAN)
  04-egressgateway.yaml    — EgressGateway awsbnkctl-egress: gatewayClassName,
                             parametersRef {awsbnkctl-egress, default-egress},
                             NamespaceSelector on the scenario namespace

Verify:
  1. egress-client pod Ready.
  2. GatewaySettings Accepted=True and ResolvedRefs=True.
  3. EgressGateway Accepted, ResolvedRefs and Programmed=True.
  4. Data path: 3 curls from the pod to an out-of-VPC URL (default
     http://checkip.amazonaws.com/) must raise TMM's egress virtual-server
     connections and its SNAT-to-external-self-IP connections by 3 (tmctl in
     the TMM debug sidecar).

Options: namespace, tunnel-network (Infra network name; default follows the
interface pattern), egress-target (URL outside the VPC).

Cleanup: delete the EgressGateway, the GatewaySettings and the scenario
namespace (idempotent; not-found ignored).
`)
}

// manifestVars holds the template variables for the manifests.
type manifestVars struct {
	Namespace        string
	GatewayClassName string
	EgressName       string // GatewaySettings + EgressGateway name
	EgressConfig     string // egressConfigs[].name / parametersRef.sectionName
	TunnelNetwork    string // Infra network the tunnel terminates on
}

func (s *scenario) Manifests(ctx *scenarios.Context) ([]string, error) {
	v := buildManifestVars(ctx)

	var paths []string
	err := fs.WalkDir(manifestFS, "manifests", func(p string, d fs.DirEntry, werr error) error {
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
	v := buildManifestVars(ctx)
	res := scenarios.Result{}

	// 1. egress-client pod Ready.
	err := d.WaitPodReadyFn(ctx.Ctx, ctx, v.Namespace, "egress-client", 3*time.Minute)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "egress-client pod Ready",
		OK:          err == nil,
		Got:         scenarios.ErrString(err),
	})

	// 2. GatewaySettings resolved against the Infra (the EgressGateway's
	// prerequisite per the F5 how-to).
	got, err := d.WaitCRConditionsFn(ctx.Ctx, ctx, gatewaySettingsGVR, v.Namespace, v.EgressName, []string{"Accepted", "ResolvedRefs"}, gatewaySettingsWait)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: fmt.Sprintf("GatewaySettings %s/%s Accepted + ResolvedRefs", v.Namespace, v.EgressName),
		OK:          err == nil,
		Got:         conditionDetail(got, err),
	})

	// 3. EgressGateway programmed on TMM.
	got, err = d.WaitCRConditionsFn(ctx.Ctx, ctx, egressGatewayGVR, v.Namespace, v.EgressName, []string{"Accepted", "ResolvedRefs", "Programmed"}, egressGatewayWait)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: fmt.Sprintf("EgressGateway %s/%s Programmed", v.Namespace, v.EgressName),
		OK:          err == nil,
		Got:         conditionDetail(got, err),
	})

	// 4. Data path, read from TMM itself (gating); without an exec client it
	// is recorded as skipped so the result stays honest.
	skipped, a := verifyTMMEgress(d, ctx, v.Namespace)
	res.Assertions = append(res.Assertions, a)
	if skipped {
		res.Details = "Control plane only on this run — the TMM-counter data-path proof needs a Kubernetes exec client."
	} else {
		res.DataPath = true
		res.Details = "Control plane + data path: the pod's out-of-VPC curls moved TMM's egress-virtual and SNAT-to-external-self-IP counters."
	}

	return scenarios.FinalizeResult(res)
}

// conditionDetail joins the condition summary and the wait error for a report.
func conditionDetail(summary string, err error) string {
	if err == nil {
		return summary
	}
	if summary == "" {
		return err.Error()
	}
	return summary + " (" + err.Error() + ")"
}

func (s *scenario) Cleanup(ctx *scenarios.Context) error {
	v := buildManifestVars(ctx)

	// Delete the EgressGateway before the GatewaySettings it references so the
	// controller deprograms TMM cleanly, then the namespace (idempotent).
	if ctx.Dynamic != nil {
		for _, gvr := range []schema.GroupVersionResource{egressGatewayGVR, gatewaySettingsGVR} {
			err := ctx.Dynamic.Resource(gvr).Namespace(v.Namespace).Delete(ctx.Ctx, v.EgressName, metav1.DeleteOptions{})
			if err != nil && !scenarios.IsNotFound(err) {
				return fmt.Errorf("deleting %s %s from %s: %w", gvr.Resource, v.EgressName, v.Namespace, err)
			}
		}
	}
	err := ctx.Clientset.CoreV1().Namespaces().Delete(ctx.Ctx, v.Namespace, metav1.DeleteOptions{})
	if err != nil && !scenarios.IsNotFound(err) {
		return fmt.Errorf("deleting namespace %s: %w", v.Namespace, err)
	}
	return nil
}

func (s *scenario) Namespace(ctx *scenarios.Context) string {
	return namespace(ctx)
}

// --- internal helpers ---

func namespace(ctx *scenarios.Context) string {
	if v := ctx.Options["namespace"]; v != "" {
		return v
	}
	return scnNamespace
}

// gatewayClassName is the class phase 23b registered: state first, then the
// cluster-name convention every other scenario uses.
func gatewayClassName(ctx *scenarios.Context) string {
	if ctx.State != nil {
		if v := ctx.State.Get("GATEWAYCLASS_NAME"); v != "" {
			return v
		}
	}
	if ctx.Cluster != nil && ctx.Cluster.Metadata.Name != "" {
		return ctx.Cluster.Metadata.Name + "-gatewayclass"
	}
	return "f5-cne"
}

// tunnelNetwork picks the Infra network the egress tunnel terminates on. It
// must match the Infra egressDefaults phase 23b rendered: the internal VLAN on
// dual-interface clusters, the external VLAN when it is the only one (a nil
// cluster — unit tests, no --config — gets the external VLAN, which every
// pattern has). An explicit option always wins.
func tunnelNetwork(ctx *scenarios.Context) string {
	if v := ctx.Options["tunnel-network"]; v != "" {
		return v
	}
	return render.InfraTunnelNetwork(ctx.Cluster != nil && ctx.Cluster.HasInternalInterface())
}

func buildManifestVars(ctx *scenarios.Context) manifestVars {
	return manifestVars{
		Namespace:        namespace(ctx),
		GatewayClassName: gatewayClassName(ctx),
		EgressName:       egressName,
		EgressConfig:     egressConfig,
		TunnelNetwork:    tunnelNetwork(ctx),
	}
}

// waitPodReady polls until the named Pod has a Ready condition True, or timeout.
func waitPodReady(ctx context.Context, sctx *scenarios.Context, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pod, err := sctx.Clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			for _, c := range pod.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
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
	return fmt.Errorf("pod %s/%s not Ready after %s", ns, name, timeout)
}

// waitCRConditions polls the CR until every condition type in want reports
// status True. It returns a one-line summary of the conditions it last saw
// ("Accepted=True ResolvedRefs=True Programmed=False/Programming: …"), with a
// nil error on success and the reason for giving up otherwise.
func waitCRConditions(ctx context.Context, sctx *scenarios.Context, gvr schema.GroupVersionResource, ns, name string, want []string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	summary := "not found"
	for {
		obj, err := sctx.Dynamic.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			conds, _, _ := scenarios.NestedSlice(obj.Object, "status", "conditions")
			var parts []string
			status := map[string]string{}
			for _, cRaw := range conds {
				c, ok := cRaw.(map[string]interface{})
				if !ok {
					continue
				}
				t, _ := c["type"].(string)
				st, _ := c["status"].(string)
				status[t] = st
				p := t + "=" + st
				if st != "True" {
					reason, _ := c["reason"].(string)
					msg, _ := c["message"].(string)
					p += "/" + reason + ": " + msg
				}
				parts = append(parts, p)
			}
			if len(parts) == 0 {
				summary = "present (no status conditions yet)"
			} else {
				summary = strings.Join(parts, " ")
			}
			allTrue := true
			for _, w := range want {
				if status[w] != "True" {
					allTrue = false
				}
			}
			if allTrue {
				return summary, nil
			}
		case scenarios.IsNotFound(err):
			summary = "not found"
		default:
			summary = err.Error()
		}
		if !time.Now().Before(deadline) {
			return summary, fmt.Errorf("%s %s/%s: %s not all True after %s", gvr.Resource, ns, name, strings.Join(want, "+"), timeout)
		}
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}
