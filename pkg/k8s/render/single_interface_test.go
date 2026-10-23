package render

import (
	"strings"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/manifests"
)

// These tests pin the single-interface (external-only) rendering contract: the
// embedded templates must emit the external interface but OMIT every internal
// counterpart so an external-only cluster doesn't reference a nonexistent ENI.

func TestRenderNADs_SingleInterface_OmitsInternal(t *testing.T) {
	tmpl, err := manifests.FS.ReadFile("host-device/network-attachment-defs.yaml.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	out, err := RenderNADs(tmpl, "f5-cne-system", false, func(string) string { return "" })
	if err != nil {
		t.Fatalf("RenderNADs: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "external-netdevice") {
		t.Errorf("single-interface NADs must keep the external NAD:\n%s", s)
	}
	if strings.Contains(s, "internal-netdevice") {
		t.Errorf("single-interface NADs must omit the internal NAD:\n%s", s)
	}
}

func TestRenderCNEInstance_SingleInterface_OmitsInternal(t *testing.T) {
	tmpl, err := manifests.FS.ReadFile("shared/cneinstance.yaml.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	cl := cneInstanceCluster()
	cl.Pattern = intent.PatternExternalOnly // override the dual default
	out, err := RenderCNEInstance(tmpl, cl, func(k string) string {
		if k == "VPC_ID" {
			return "vpc-123"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("RenderCNEInstance: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "- external-netdevice") {
		t.Errorf("CNEInstance must list the external NAD:\n%s", s)
	}
	if strings.Contains(s, "- internal-netdevice") {
		t.Errorf("single-interface CNEInstance must not list the internal NAD:\n%s", s)
	}
	if strings.Contains(s, "ROBIN_VFIO_RESOURCE_2") {
		t.Errorf("single-interface CNEInstance must not set ROBIN_VFIO_RESOURCE_2:\n%s", s)
	}
	// The external resource must still be present.
	if !strings.Contains(s, "ROBIN_VFIO_RESOURCE_1") {
		t.Errorf("CNEInstance must keep ROBIN_VFIO_RESOURCE_1:\n%s", s)
	}
}

func TestRenderCloudNetworkMapping_SingleInterface_OmitsInternalSubnet(t *testing.T) {
	tmpl, err := manifests.FS.ReadFile("shared/cloud-network-mapping.yaml.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	cl := &intent.Cluster{
		Metadata: intent.Metadata{Name: "ext-only", Region: "ap-southeast-2"},
		Pattern:  intent.PatternExternalOnly,
		Network: intent.Network{
			AZs:     []string{"ap-southeast-2a"},
			Subnets: intent.Subnets{PublicList: []intent.SubnetSpec{{CIDR: "10.0.1.0/24", AZ: "ap-southeast-2a"}}},
			DataPath: &intent.DataPathSpec{
				External: intent.SubnetSpec{CIDR: "10.0.10.0/24", AZ: "ap-southeast-2a"},
			},
		},
	}
	// Note: BNK_INT_SUBNET deliberately absent — single-interface must not need it.
	getter := func(k string) string {
		switch k {
		case "MGMT_SUBNET":
			return "subnet-mgmt"
		case "BNK_EXT_SUBNET":
			return "subnet-ext"
		}
		return ""
	}
	out, err := RenderCloudNetworkMapping(tmpl, cl, getter)
	if err != nil {
		t.Fatalf("RenderCloudNetworkMapping (single-interface): %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "subnet-ext") {
		t.Errorf("cloud-network-mapping must include the external subnet:\n%s", s)
	}
	if strings.Contains(s, "10.0.20.0/24") {
		t.Errorf("single-interface cloud-network-mapping must omit the internal subnet entry:\n%s", s)
	}
}

func infraTestCluster(external, internal string) *intent.Cluster {
	cl := hostDeviceClusterForRender("bnk-test")
	cl.Network.DataPath.External.CIDR = "10.0.10.0/24"
	cl.Network.DataPath.Internal.CIDR = "10.0.20.0/24"
	cl.Network.DataPath.SelfIPs = &intent.SelfIPsSpec{External: external, Internal: internal, PrefixLen: 24}
	cl.Bnk = &intent.BnkSpec{TmmMtu: 9000}
	return cl
}

// TestRenderInfra_SingleInterface_OmitsInternal pins the external-only contract
// for the BNK 2.4 Infra CR: the external network, its self-IP pool and the
// listener pool are present; nothing internal is rendered.
func TestRenderInfra_SingleInterface_OmitsInternal(t *testing.T) {
	tmpl, err := manifests.FS.ReadFile("host-device/infra.yaml.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	out, err := RenderInfra(tmpl, infraTestCluster("10.0.10.240", ""), false)
	if err != nil {
		t.Fatalf("RenderInfra: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"kind: Infra", "name: infra", "namespace: f5-cne-system",
		"name: ext-vlan-infra", "type: vlan", "mtu: 9000",
		"name: external-netdevice",
		`rangeStart: "10.0.10.224"`, `rangeEnd: "10.0.10.254"`,
		"name: listener-pool", `rangeStart: "10.0.10.100"`, `rangeEnd: "10.0.10.199"`,
		// Egress: the tunnel ends on the only VLAN; both routes point at the external gateway.
		"egressDefaults:", "subnet: 192.168.0.0/16", "port: 4789", "name: ext-vlan-infra\n",
		"staticRoutes:", "name: vpc", "- 10.0.0.0/16", "nextHop: 10.0.10.1", "name: default", "- 0.0.0.0/0",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("single-interface Infra missing %q:\n%s", want, s)
		}
	}
	if strings.Count(s, "nextHop: 10.0.10.1") != 2 {
		t.Errorf("single-interface Infra must route both the VPC and the default via the external gateway:\n%s", s)
	}
	for _, unwanted := range []string{"name: int-vlan-infra", "int-selfip", "internal-netdevice", "10.0.20."} {
		if strings.Contains(s, unwanted) {
			t.Errorf("single-interface Infra must omit %q:\n%s", unwanted, s)
		}
	}
}

// TestRenderInfra_DualInterface renders both VLAN networks with pinned self IPs.
func TestRenderInfra_DualInterface(t *testing.T) {
	tmpl, err := manifests.FS.ReadFile("host-device/infra.yaml.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	out, err := RenderInfra(tmpl, infraTestCluster("10.0.10.240", "10.0.20.240"), true)
	if err != nil {
		t.Fatalf("RenderInfra: %v", err)
	}
	s := string(out)
	for _, want := range []string{`availabilityZone: "ap-southeast-2a"`, "name: int-vlan-infra", "name: int-selfip", "name: internal-netdevice", `rangeStart: "10.0.20.224"`, `rangeEnd: "10.0.20.254"`, "name: int-attach",
		// Egress: the tunnel ends on the internal VLAN, the VPC route goes via its gateway, the default via the external one.
		"egressDefaults:", "networkRef:\n      name: int-vlan-infra", "- 10.0.0.0/16\n      nextHop: 10.0.20.1", "- 0.0.0.0/0\n      nextHop: 10.0.10.1",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("dual-interface Infra missing %q:\n%s", want, s)
		}
	}
	if _, err := RenderInfra(tmpl, infraTestCluster("10.0.10.240", ""), true); err == nil {
		t.Error("dual-interface without an internal self IP must fail")
	}
	if InfraTunnelNetwork(true) != InfraIntNetwork || InfraTunnelNetwork(false) != InfraExtNetwork {
		t.Error("InfraTunnelNetwork must follow the interface pattern")
	}
}

// TestRenderInfra_NoVPCCidr_OmitsStaticRoutes: without a VPC CIDR there is
// nothing to route, so the Infra carries no staticRoutes block at all (an
// empty list would fail the CRD's minItems).
func TestRenderInfra_NoVPCCidr_OmitsStaticRoutes(t *testing.T) {
	tmpl, err := manifests.FS.ReadFile("host-device/infra.yaml.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	cl := infraTestCluster("10.0.10.240", "")
	cl.Network.VPCCidr = ""
	out, err := RenderInfra(tmpl, cl, false)
	if err != nil {
		t.Fatalf("RenderInfra: %v", err)
	}
	if strings.Contains(string(out), "staticRoutes") {
		t.Errorf("Infra without a VPC CIDR must omit staticRoutes:\n%s", out)
	}
	if !strings.Contains(string(out), "egressDefaults:") {
		t.Errorf("Infra must always carry egressDefaults:\n%s", out)
	}
}

// TestRenderInfra_SelfIPPoolRejectsReservedBlock pins the BNK 2.4 self-IP pool
// contract: a self IP in the first /27 of its subnet (AWS-reserved addresses)
// cannot be rendered.
func TestRenderInfra_SelfIPPoolRejectsReservedBlock(t *testing.T) {
	tmpl, err := manifests.FS.ReadFile("host-device/infra.yaml.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	if _, err := RenderInfra(tmpl, infraTestCluster("10.0.10.5", ""), false); err == nil {
		t.Error("external self IP in the reserved first /27 must fail to render")
	}
	if _, err := RenderInfra(tmpl, infraTestCluster("10.0.10.240", "10.0.20.9"), true); err == nil {
		t.Error("internal self IP in the reserved first /27 must fail to render")
	}
}

// TestRenderCNEControllerRBAC pins the FLO 2.30 workaround: the controller
// ServiceAccount gets get/list/watch on EndpointSlices through an
// awsbnkctl-managed ClusterRole and binding named after the cluster.
func TestRenderCNEControllerRBAC(t *testing.T) {
	tmpl, err := manifests.FS.ReadFile("shared/cne-controller-rbac.yaml.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	out, err := RenderCNEControllerRBAC(tmpl, infraTestCluster("10.0.10.240", ""))
	if err != nil {
		t.Fatalf("RenderCNEControllerRBAC: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"kind: ClusterRole\n", "kind: ClusterRoleBinding", "name: bnk-test-cne-controller-endpointslices",
		`resources: ["endpointslices"]`, `verbs: ["get", "list", "watch"]`,
		"name: f5-cne-controller", "namespace: f5-cne-system",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("cne-controller RBAC missing %q:\n%s", want, s)
		}
	}
	if _, err := RenderCNEControllerRBAC(tmpl, nil); err == nil {
		t.Error("nil cluster must fail")
	}
}

// TestRenderInfra_BGPFlagLeavesInfraUnchanged pins the BNK 2.4 BGP contract:
// bnk.bgp changes nothing in the Infra CR. The 2.4 Infra CRD has no per-VLAN
// allowed-services (the 2.3 F5SPKVlan's allowed_services for tcp:179/udp:3784
// have no counterpart), so the same manifest serves BGP and non-BGP clusters
// and phase 07's security-group rule is the only cluster-side change.
func TestRenderInfra_BGPFlagLeavesInfraUnchanged(t *testing.T) {
	tmpl, err := manifests.FS.ReadFile("host-device/infra.yaml.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	plain, err := RenderInfra(tmpl, infraTestCluster("10.0.10.240", "10.0.20.240"), true)
	if err != nil {
		t.Fatalf("RenderInfra: %v", err)
	}
	bgp := infraTestCluster("10.0.10.240", "10.0.20.240")
	bgp.Bnk.BGP = true
	if !bgp.IsBGPEnabled() {
		t.Fatal("test cluster must report BGP enabled")
	}
	withBGP, err := RenderInfra(tmpl, bgp, true)
	if err != nil {
		t.Fatalf("RenderInfra (bgp): %v", err)
	}
	if string(plain) != string(withBGP) {
		t.Errorf("bnk.bgp must not change the Infra CR (no per-VLAN allowed-services on 2.4):\n--- without\n%s\n--- with\n%s", plain, withBGP)
	}
	for _, unwanted := range []string{"allowed_services", "allowedServices", "179", "3784"} {
		if strings.Contains(string(withBGP), unwanted) {
			t.Errorf("Infra must not carry %q (the 2.4 CRD has no such field):\n%s", unwanted, withBGP)
		}
	}
}
