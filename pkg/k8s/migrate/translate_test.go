package migrate

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var fixtureZones = map[string]string{"ext-vlan": "ap-southeast-2a", "int-vlan": "ap-southeast-2a"}

func translateFixture(t *testing.T, opts Options) *Plan {
	t.Helper()
	plan, err := Translate(inventoryFromFixture(t, fixture23), opts)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	return plan
}

// slice returns a nested slice or fails.
func slice(t *testing.T, obj map[string]any, fields ...string) []any {
	t.Helper()
	s, found, err := unstructured.NestedSlice(obj, fields...)
	if err != nil || !found {
		t.Fatalf("%v: not found (%v)", fields, err)
	}
	return s
}

// entry returns the element of a named list with name.
func entry(t *testing.T, list []any, name string) map[string]any {
	t.Helper()
	for _, e := range list {
		m, _ := e.(map[string]any)
		if m["name"] == name {
			return m
		}
	}
	t.Fatalf("no entry named %q in %v", name, list)
	return nil
}

func str(t *testing.T, obj map[string]any, fields ...string) string {
	t.Helper()
	s, _, err := unstructured.NestedString(obj, fields...)
	if err != nil {
		t.Fatalf("%v: %v", fields, err)
	}
	return s
}

func TestTranslate_Infra(t *testing.T) {
	plan := translateFixture(t, Options{ZoneByNetwork: fixtureZones})
	if plan.Infra == nil {
		t.Fatal("no Infra CR")
	}
	infra := plan.Infra.Object
	if got := plan.Infra.GetName(); got != "infra" {
		t.Errorf("Infra name = %q", got)
	}
	if got := plan.Infra.GetNamespace(); got != "f5-cne-system" {
		t.Errorf("Infra namespace = %q", got)
	}
	if got := str(t, infra, "apiVersion"); got != "gateway.k8s.f5.com/v1alpha1" {
		t.Errorf("apiVersion = %q", got)
	}

	ipams := slice(t, infra, "spec", "ipams")
	ext := entry(t, ipams, "ext-vlan-selfip")
	pool := ext["ipPools"].([]any)[0].(map[string]any)
	if pool["rangeStart"] != "10.0.10.224" || pool["rangeEnd"] != "10.0.10.254" || pool["availabilityZone"] != "ap-southeast-2a" {
		t.Errorf("ext-vlan-selfip pool = %v", pool)
	}
	pool = entry(t, ipams, "int-vlan-selfip")["ipPools"].([]any)[0].(map[string]any)
	if pool["rangeStart"] != "10.0.20.224" || pool["rangeEnd"] != "10.0.20.254" {
		t.Errorf("int-vlan-selfip pool = %v", pool)
	}
	pool = entry(t, ipams, "http2-scenario-http2-vip-pool")["ipPools"].([]any)[0].(map[string]any)
	if pool["rangeStart"] != "10.0.10.100" || pool["rangeEnd"] != "10.0.10.110" || pool["availabilityZone"] != "ap-southeast-2a" {
		t.Errorf("listener pool = %v", pool)
	}
	pool = entry(t, ipams, "default-bnk-agentcore-demo-gateway-static")["ipPools"].([]any)[0].(map[string]any)
	if pool["rangeStart"] != "10.0.10.121" || pool["rangeEnd"] != "10.0.10.121" {
		t.Errorf("static pool = %v", pool)
	}
	if snat := entry(t, ipams, "egress-snat-snat")["ipPools"].([]any); len(snat) != 2 {
		t.Errorf("snat pool has %d entries, want 2", len(snat))
	}

	attach := slice(t, infra, "spec", "networkAttachments")
	nads := entry(t, attach, "ext-vlan-attach")["networkAttachmentDefinitions"].([]any)
	if len(nads) != 1 || nads[0].(map[string]any)["name"] != "external-netdevice" {
		t.Errorf("ext-vlan-attach NADs = %v", nads)
	}
	nads = entry(t, attach, "int-vlan-attach")["networkAttachmentDefinitions"].([]any)
	if len(nads) != 1 || nads[0].(map[string]any)["name"] != "internal-netdevice" {
		t.Errorf("int-vlan-attach NADs = %v", nads)
	}

	nets := slice(t, infra, "spec", "networks")
	if len(nets) != 2 {
		t.Fatalf("networks = %d, want 2", len(nets))
	}
	extNet := entry(t, nets, "ext-vlan")
	vlan := extNet["vlan"].(map[string]any)
	if extNet["type"] != "vlan" || vlan["tag"] != int64(0) {
		t.Errorf("ext-vlan network = %v", extNet)
	}
	if ref := vlan["networkAttachmentRef"].(map[string]any); ref["name"] != "ext-vlan-attach" {
		t.Errorf("ext-vlan attachment ref = %v", ref)
	}
	if refs := vlan["ipamRefs"].([]any); refs[0].(map[string]any)["name"] != "ext-vlan-selfip" {
		t.Errorf("ext-vlan ipamRefs = %v", refs)
	}
	if _, has := vlan["mtu"]; has {
		t.Errorf("ext-vlan carries an mtu the 2.3 CR did not have")
	}
	if mtu := entry(t, nets, "int-vlan")["vlan"].(map[string]any)["mtu"]; mtu != int64(9000) {
		t.Errorf("int-vlan mtu = %v", mtu)
	}

	routes := slice(t, infra, "spec", "staticRoutes")
	if len(routes) != 2 {
		t.Fatalf("staticRoutes = %d", len(routes))
	}
	vpc := entry(t, routes, "bnk-egress-demo-vpc")
	if vpc["destinations"].([]any)[0] != "10.0.0.0/16" || vpc["nextHop"] != "10.0.20.1" {
		t.Errorf("vpc route = %v", vpc)
	}
	def := entry(t, routes, "bnk-egress-demo-default")
	if def["destinations"].([]any)[0] != "0.0.0.0/0" || def["nextHop"] != "10.0.10.1" {
		t.Errorf("default route = %v", def)
	}

	if got := str(t, infra, "spec", "egressDefaults", "subnet"); got != "192.168.0.0/16" {
		t.Errorf("egressDefaults.subnet = %q", got)
	}
	if got := str(t, infra, "spec", "egressDefaults", "networkRef", "name"); got != "int-vlan" {
		t.Errorf("egressDefaults.networkRef = %q", got)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(infra, "spec", "egressDefaults", "port"); found {
		t.Errorf("egressDefaults.port set although the 2.3 CR had none")
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(infra, "spec", "vrfs"); found {
		t.Errorf("vrfs present without Vrf CRs")
	}
}

func TestTranslate_GatewaySettingsAndPatches(t *testing.T) {
	plan := translateFixture(t, Options{ZoneByNetwork: fixtureZones})
	if len(plan.GatewaySettings) != 3 {
		t.Fatalf("GatewaySettings = %d, want 3", len(plan.GatewaySettings))
	}
	// Sorted by namespace/name.
	wantOrder := []string{"bnk-egress-demo/bnk-egress-demo", "default/bnk-agentcore-demo-gateway", "http2-scenario/http2-gateway"}
	for i, gs := range plan.GatewaySettings {
		if objKey(gs) != wantOrder[i] {
			t.Errorf("GatewaySettings[%d] = %s, want %s", i, objKey(gs), wantOrder[i])
		}
	}

	http2 := find(plan.GatewaySettings, "GatewaySettings", "http2-scenario", "http2-gateway").Object
	refs := slice(t, http2, "spec", "ingressConfig", "defaultListenerNetwork", "ipamRefs")
	if len(refs) != 1 || refs[0].(map[string]any)["name"] != "http2-scenario-http2-vip-pool" {
		t.Errorf("http2 ipamRefs = %v", refs)
	}
	nets := slice(t, http2, "spec", "ingressConfig", "defaultListenerNetwork", "networkRefs")
	if len(nets) != 1 || nets[0].(map[string]any)["name"] != "ext-vlan" {
		t.Errorf("http2 networkRefs = %v (internal VLAN must not be a listener network)", nets)
	}
	if got := str(t, http2, "spec", "ingressConfig", "defaultListenerNetwork", "sourceNATConfig", "type"); got != "Automap" {
		t.Errorf("http2 SNAT = %q", got)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(http2, "spec", "egressConfigs"); found {
		t.Errorf("http2 GatewaySettings carries egressConfigs")
	}

	egress := find(plan.GatewaySettings, "GatewaySettings", "bnk-egress-demo", "bnk-egress-demo").Object
	cfgs := slice(t, egress, "spec", "egressConfigs")
	cfg := entry(t, cfgs, "bnk-egress-demo")
	if cfg["networkRef"].(map[string]any)["name"] != "int-vlan" {
		t.Errorf("egressConfig networkRef = %v", cfg["networkRef"])
	}
	snat := cfg["sourceNATConfig"].(map[string]any)
	if snat["type"] != "Pool" || snat["sourceNATPoolRef"].(map[string]any)["name"] != "egress-snat" {
		t.Errorf("egress SNAT = %v", snat)
	}
	pools := slice(t, egress, "spec", "sourceNATPools")
	if p := entry(t, pools, "egress-snat"); p["ipamRefs"].([]any)[0].(map[string]any)["name"] != "egress-snat-snat" {
		t.Errorf("sourceNATPools = %v", pools)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(egress, "spec", "ingressConfig"); found {
		t.Errorf("egress-only GatewaySettings carries ingressConfig")
	}

	if len(plan.GatewayPatches) != 2 {
		t.Fatalf("GatewayPatches = %d, want 2 (the nginx Gateway is not BNK)", len(plan.GatewayPatches))
	}
	patch := find(plan.GatewayPatches, "Gateway", "http2-scenario", "http2-gateway")
	if patch == nil {
		t.Fatal("no patch for http2-gateway")
	}
	ref, _, _ := unstructured.NestedMap(patch.Object, "spec", "infrastructure", "parametersRef")
	if ref["group"] != "gateway.k8s.f5.com" || ref["kind"] != "GatewaySettings" || ref["name"] != "http2-gateway" {
		t.Errorf("parametersRef = %v", ref)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(patch.Object, "spec", "listeners"); found {
		t.Errorf("Gateway patch must be partial (SSA), got listeners")
	}
	if find(plan.GatewayPatches, "Gateway", "default", "other") != nil {
		t.Error("nginx Gateway patched")
	}
}

func TestTranslate_EgressGatewayAndPolicies(t *testing.T) {
	plan := translateFixture(t, Options{ZoneByNetwork: fixtureZones})
	if len(plan.EgressGateways) != 1 {
		t.Fatalf("EgressGateways = %d", len(plan.EgressGateways))
	}
	eg := plan.EgressGateways[0].Object
	if plan.EgressGateways[0].GetNamespace() != "bnk-egress-demo" || plan.EgressGateways[0].GetName() != "bnk-egress-demo" {
		t.Errorf("EgressGateway key = %s", objKey(plan.EgressGateways[0]))
	}
	if got := str(t, eg, "spec", "gatewayClassName"); got != "lab-gatewayclass" {
		t.Errorf("gatewayClassName = %q", got)
	}
	if str(t, eg, "spec", "infrastructure", "parametersRef", "name") != "bnk-egress-demo" || str(t, eg, "spec", "infrastructure", "parametersRef", "sectionName") != "bnk-egress-demo" {
		t.Errorf("parametersRef = %v", eg["spec"])
	}
	if got := str(t, eg, "spec", "sourceSelector", "selectionMode"); got != "NamespaceSelector" {
		t.Errorf("selectionMode = %q", got)
	}
	names := slice(t, eg, "spec", "sourceSelector", "namespaces", "matchNames")
	if len(names) != 1 || names[0] != "bnk-egress-demo" {
		t.Errorf("matchNames = %v", names)
	}

	if len(plan.Policies) != 3 {
		t.Fatalf("Policies = %d, want 3", len(plan.Policies))
	}
	fw := find(plan.Policies, "SecPolicy", "bnk-egress-demo", "egress-demo-fw")
	if fw == nil {
		t.Fatal("no firewall SecPolicy")
	}
	tr := slice(t, fw.Object, "spec", "targetRefs")[0].(map[string]any)
	if tr["group"] != "gateway.k8s.f5.com" || tr["kind"] != "EgressGateway" || tr["name"] != "bnk-egress-demo" {
		t.Errorf("firewall targetRefs = %v", tr)
	}
	er := slice(t, fw.Object, "spec", "extensionRefs")[0].(map[string]any)
	if er["group"] != "k8s.f5net.com" || er["kind"] != "F5BigFwPolicy" || er["name"] != "egress-demo-fw" {
		t.Errorf("firewall extensionRefs = %v", er)
	}

	np := find(plan.Policies, "NetPolicy", "default", "mcp-net-policy-http")
	if np == nil {
		t.Fatal("no NetPolicy")
	}
	if got := str(t, np.Object, "apiVersion"); got != "gateway.k8s.f5.com/v1alpha1" {
		t.Errorf("NetPolicy apiVersion = %q", got)
	}
	if np.GetLabels()["app"] != "agentcore" {
		t.Errorf("labels dropped: %v", np.GetLabels())
	}
	if ann := np.GetAnnotations(); ann["note"] != "keep" || ann["kubectl.kubernetes.io/last-applied-configuration"] != "" {
		t.Errorf("annotations = %v", ann)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(np.Object, "status"); found {
		t.Error("status copied")
	}
	tr = slice(t, np.Object, "spec", "targetRefs")[0].(map[string]any)
	if tr["sectionName"] != "http" || tr["name"] != "bnk-agentcore-demo-gateway" {
		t.Errorf("NetPolicy targetRefs = %v", tr)
	}
	if sp := find(plan.Policies, "SecPolicy", "default", "mcp-sec-policy"); sp == nil {
		t.Error("BNKSecPolicy not converted")
	}
}

func TestTranslate_Warnings(t *testing.T) {
	plan := translateFixture(t, Options{ZoneByNetwork: fixtureZones})
	for _, want := range []string{
		"allowed_services dropped",
		"nodeInterfaceName",
		"IPv6 tunnel subnet",
		"firewallEnforcedPolicy egress-demo-fw becomes SecPolicy bnk-egress-demo/egress-demo-fw",
		"listener pool default-bnk-agentcore-demo-gateway-static is only its static address",
		"ingress SNAT is Automap",
	} {
		if !hasWarning(plan, want) {
			t.Errorf("missing warning %q in\n%s", want, strings.Join(plan.Warnings, "\n"))
		}
	}
	if hasWarning(plan, "no availability zone") {
		t.Errorf("zone warning raised although zones were given")
	}
	plan = translateFixture(t, Options{})
	if !hasWarning(plan, "no availability zone known") {
		t.Error("no zone warning without ZoneByNetwork")
	}
}

func TestTranslate_Golden(t *testing.T) {
	plan := translateFixture(t, Options{ZoneByNetwork: fixtureZones})
	var buf bytes.Buffer
	if err := WriteYAML(&buf, plan); err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "plan-2.3-dual.golden.yaml")
	if *update {
		if err := os.WriteFile(golden, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if !bytes.Equal(want, buf.Bytes()) {
		t.Errorf("plan differs from %s (re-run with -update after checking):\n%s", golden, buf.String())
	}
}

func TestTranslate_NoVlans(t *testing.T) {
	plan, err := Translate(&Inventory{Namespace: "f5-cne-system"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Infra != nil {
		t.Error("Infra generated without VLANs")
	}
	if !plan.Empty() {
		t.Error("plan not empty")
	}
	if !hasWarning(plan, "no F5SPKVlan found") {
		t.Errorf("warnings = %v", plan.Warnings)
	}
	if _, err := Translate(nil, Options{}); err == nil {
		t.Error("nil inventory accepted")
	}
}

func TestTranslate_Options(t *testing.T) {
	plan := translateFixture(t, Options{SingleSelfIP: true, InfraName: "lab-infra", GatewayClassName: "forced"})
	if plan.Infra.GetName() != "lab-infra" {
		t.Errorf("Infra name = %q", plan.Infra.GetName())
	}
	pool := entry(t, slice(t, plan.Infra.Object, "spec", "ipams"), "ext-vlan-selfip")["ipPools"].([]any)[0].(map[string]any)
	if pool["rangeStart"] != "10.0.10.240" || pool["rangeEnd"] != "10.0.10.240" {
		t.Errorf("single self IP pool = %v", pool)
	}
	if got := str(t, plan.EgressGateways[0].Object, "spec", "gatewayClassName"); got != "forced" {
		t.Errorf("gatewayClassName = %q", got)
	}
}

func TestTranslate_SnatTypes(t *testing.T) {
	cases := map[string]string{"SRC_TRANS_AUTOMAP": "Automap", "": "Automap", "SRC_TRANS_NONE": "None", "BOGUS": "Automap"}
	for in, want := range cases {
		inv := inventoryFromFixture(t, fixture23)
		_ = unstructured.SetNestedField(inv.Egresses[0].Object, in, "spec", "snatType")
		plan, err := Translate(inv, Options{})
		if err != nil {
			t.Fatal(err)
		}
		gs := find(plan.GatewaySettings, "GatewaySettings", "bnk-egress-demo", "bnk-egress-demo")
		cfg := slice(t, gs.Object, "spec", "egressConfigs")[0].(map[string]any)
		if got := cfg["sourceNATConfig"].(map[string]any)["type"]; got != want {
			t.Errorf("snatType %q -> %v, want %s", in, got, want)
		}
		if in == "BOGUS" && !hasWarning(plan, "unknown snatType") {
			t.Error("no warning for unknown snatType")
		}
	}
}

const fixtureVxlan = `
apiVersion: k8s.f5net.com/v1
kind: F5SPKVlan
metadata: {name: ext-vlan, namespace: f5-cne-system}
spec: {name: ext-vlan, interfaces: ["1.3"], selfip_v4s: ["10.0.10.240"], tag: 0}
---
apiVersion: k8s.f5net.com/v1
kind: Vrf
metadata: {name: tenant-a, namespace: f5-cne-system}
spec: {strictIsolation: true, interconnectVrf: [tenant-b]}
---
apiVersion: k8s.f5net.com/v1
kind: Vxlan
metadata: {name: overlay, namespace: f5-cne-system}
spec: {vlan: ext-vlan, vni: 4242, ipv4Subnet: "10.99.0.0", ipv4PrefixLen: 24, port: 4790, vrf: tenant-a, evpn: true}
---
apiVersion: k8s.f5net.com/v1
kind: F5SPKStaticRoute
metadata: {name: drop, namespace: f5-cne-system}
spec: {destination: 10.9.0.0, prefixLen: 16, type: blackhole}
`

func TestTranslate_VrfVxlanAndUnknownInterface(t *testing.T) {
	inv := inventoryFromFixture(t, fixtureVxlan)
	plan, err := Translate(inv, Options{})
	if err != nil {
		t.Fatal(err)
	}
	infra := plan.Infra.Object
	vrfs := slice(t, infra, "spec", "vrfs")
	if v := entry(t, vrfs, "tenant-a"); v["strictIsolation"] != true {
		t.Errorf("vrf = %v", v)
	}
	if !hasWarning(plan, "interconnectVrf") {
		t.Error("no interconnectVrf warning")
	}
	nets := slice(t, infra, "spec", "networks")
	ov := entry(t, nets, "overlay")
	vx := ov["vxlan"].(map[string]any)
	if ov["type"] != "vxlan" || vx["vni"] != int64(4242) || vx["port"] != int64(4790) {
		t.Errorf("vxlan network = %v", ov)
	}
	if vx["networkRef"].(map[string]any)["name"] != "ext-vlan" || vx["vrfRef"].(map[string]any)["name"] != "tenant-a" {
		t.Errorf("vxlan refs = %v", vx)
	}
	if vx["ipamRefs"].([]any)[0].(map[string]any)["name"] != "overlay-vtep" {
		t.Errorf("vxlan ipamRefs = %v", vx["ipamRefs"])
	}
	if _, has := vx["vtepIpamRefs"]; has {
		t.Error("both networkRef and vtepIpamRefs set (CRD forbids it)")
	}
	vtep := entry(t, slice(t, infra, "spec", "ipams"), "overlay-vtep")["ipPools"].([]any)[0].(map[string]any)
	if vtep["cidr"] != "10.99.0.0/24" {
		t.Errorf("vtep pool = %v", vtep)
	}
	if !hasWarning(plan, "evpn") {
		t.Error("no evpn warning")
	}
	// Interface 1.3 has no NAD (no CNEInstance in this fixture).
	attach := entry(t, slice(t, infra, "spec", "networkAttachments"), "ext-vlan-attach")
	if nads := attach["networkAttachmentDefinitions"].([]any); len(nads) != 0 {
		t.Errorf("NADs = %v, want none", nads)
	}
	if !hasWarning(plan, `interface "1.3" has no matching CNEInstance networkAttachments entry`) {
		t.Errorf("no interface warning: %v", plan.Warnings)
	}
	// Blackhole route is not translated.
	if _, found, _ := unstructured.NestedFieldNoCopy(infra, "spec", "staticRoutes"); found {
		t.Error("blackhole route translated")
	}
	if !hasWarning(plan, `type "blackhole" not translated`) {
		t.Error("no blackhole warning")
	}
}

func TestTranslate_GatewayWithoutPool(t *testing.T) {
	inv := inventoryFromFixture(t, fixture23)
	// Drop the F5BnkGateway and the static address: the VIP source is unknown.
	inv.BnkGateways = nil
	for _, gw := range inv.Gateways {
		if gw.GetName() == "http2-gateway" {
			unstructured.RemoveNestedField(gw.Object, "spec", "addresses")
		}
	}
	plan, err := Translate(inv, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if find(plan.GatewaySettings, "GatewaySettings", "http2-scenario", "http2-gateway") != nil {
		t.Error("GatewaySettings generated without a VIP source")
	}
	if find(plan.GatewayPatches, "Gateway", "http2-scenario", "http2-gateway") != nil {
		t.Error("Gateway patched without a GatewaySettings")
	}
	if !hasWarning(plan, "Gateway http2-scenario/http2-gateway: no F5BnkGateway") {
		t.Errorf("warnings = %v", plan.Warnings)
	}
}

func TestTranslate_NoGatewayClass(t *testing.T) {
	inv := inventoryFromFixture(t, fixture23)
	inv.GatewayClasses = nil
	plan, err := Translate(inv, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// The nginx Gateway is now considered too; it has no VIP source, so it
	// ends up as a warning rather than a patch.
	if len(plan.GatewayPatches) != 2 {
		t.Errorf("patches = %d", len(plan.GatewayPatches))
	}
	if !hasWarning(plan, "treating every Gateway as a BNK Gateway") || !hasWarning(plan, "Gateway default/other: no F5BnkGateway") {
		t.Errorf("warnings = %v", plan.Warnings)
	}
	if got := str(t, plan.EgressGateways[0].Object, "spec", "gatewayClassName"); got != "lab-gatewayclass" {
		t.Errorf("EgressGateway class = %q (should come from the first BNK Gateway)", got)
	}
}

func TestWriteJSON(t *testing.T) {
	inv := inventoryFromFixture(t, fixture23)
	plan, _ := Translate(inv, Options{ZoneByNetwork: fixtureZones})
	var buf bytes.Buffer
	if err := WriteJSON(&buf, inv, plan); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"schema": "awsbnkctl.migrate-2.4/v1"`, `"manifestVersion": "2.3.3-3.2598.3-0.0.509"`, `"kind": "Infra"`, `"warnings"`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("JSON missing %s", want)
		}
	}
}

// TestTranslate_NoEgressStillSetsEgressDefaults: a 2.3 cluster without any
// F5SPKEgress still gets Infra egressDefaults, or the controller defaults the
// tunnel subnet to 10.0.0.0/16 and rejects the Infra (seen live 2026-09-14).
func TestTranslate_NoEgressStillSetsEgressDefaults(t *testing.T) {
	inv := inventoryFromFixture(t, fixture23)
	inv.Egresses = nil
	plan, err := Translate(inv, Options{})
	if err != nil {
		t.Fatal(err)
	}
	infra := plan.Infra.Object
	if got := str(t, infra, "spec", "egressDefaults", "subnet"); got != "192.168.0.0/16" {
		t.Errorf("egressDefaults.subnet = %q", got)
	}
	if got := str(t, infra, "spec", "egressDefaults", "networkRef", "name"); got != "int-vlan" {
		t.Errorf("egressDefaults.networkRef = %q", got)
	}
	if port, _, _ := unstructured.NestedFieldNoCopy(infra, "spec", "egressDefaults", "port"); port != int64(4789) {
		t.Errorf("egressDefaults.port = %v", port)
	}
	if !strings.Contains(strings.Join(plan.Warnings, "\n"), "no F5SPKEgress: Infra egressDefaults set to the bnkctl defaults") {
		t.Errorf("warnings = %v", plan.Warnings)
	}
}

// TestTranslate_NoStaticRoutesGetsDefault: a 2.3 cluster without any
// F5SPKStaticRoute gets the fresh-path default route via the external VLAN
// gateway, so TMM can answer clients beyond the external subnet.
func TestTranslate_NoStaticRoutesGetsDefault(t *testing.T) {
	inv := inventoryFromFixture(t, fixture23)
	inv.StaticRoutes = nil
	plan, err := Translate(inv, Options{})
	if err != nil {
		t.Fatal(err)
	}
	routes := slice(t, plan.Infra.Object, "spec", "staticRoutes")
	if len(routes) != 1 {
		t.Fatalf("staticRoutes = %v", routes)
	}
	def := entry(t, routes, "default")
	if def["destinations"].([]any)[0] != "0.0.0.0/0" || def["nextHop"] != "10.0.10.1" {
		t.Errorf("default route = %v", def)
	}
	if !strings.Contains(strings.Join(plan.Warnings, "\n"), "no F5SPKStaticRoute") {
		t.Errorf("warnings = %v", plan.Warnings)
	}
	if got := firstHost("10.0.20.240", 24); got != "10.0.20.1" {
		t.Errorf("firstHost = %q", got)
	}
	if got := firstHost("bad", 24); got != "" {
		t.Errorf("firstHost(bad) = %q", got)
	}
}

func TestTranslate_SocketInterfaces(t *testing.T) {
	inv := inventoryFromFixture(t, fixture23)
	for _, v := range inv.Vlans {
		switch v.GetName() {
		case "ext-vlan":
			_ = unstructured.SetNestedStringSlice(v.Object, []string{"0.2"}, "spec", "interfaces")
		case "int-vlan":
			_ = unstructured.SetNestedStringSlice(v.Object, []string{"0.3"}, "spec", "interfaces")
		}
	}
	plan, err := Translate(inv, Options{ZoneByNetwork: fixtureZones, TargetVersion: "2.4.0"})
	if err != nil {
		t.Fatal(err)
	}
	atts := slice(t, plan.Infra.Object, "spec", "networkAttachments")
	extAtt := entry(t, atts, "ext-vlan-attach")
	extNADs := extAtt["networkAttachmentDefinitions"].([]any)
	if len(extNADs) != 1 || extNADs[0].(map[string]any)["name"] != "external-netdevice" {
		t.Errorf("ext-vlan socket 0.2 NAD = %v", extNADs)
	}
	intAtt := entry(t, atts, "int-vlan-attach")
	intNADs := intAtt["networkAttachmentDefinitions"].([]any)
	if len(intNADs) != 1 || intNADs[0].(map[string]any)["name"] != "internal-netdevice" {
		t.Errorf("int-vlan socket 0.3 NAD = %v", intNADs)
	}
}
