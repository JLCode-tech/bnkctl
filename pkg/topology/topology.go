// Package topology builds a declarative + live-state view of an azrbnkctl
// cluster and renders it as ASCII, Mermaid, Graphviz DOT, or JSON.
//
// The package is pure data + pure renderers except for EnrichFromK8s, which
// makes read-only k8s CoreV1 calls (ListNodes, ListPods) to overlay live node
// and pod counts when a kubeconfig is reachable. Every renderer is
// deterministic given the same Model.
package topology

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

// Subnet describes one subnet in the cluster VNet.
type Subnet struct {
	Role string `json:"role"`           // system | public | external | internal
	Name string `json:"name,omitempty"` // snet-... (when known)
	ID   string `json:"id,omitempty"`   // Azure Subnet resource ID (when provisioned)
	CIDR string `json:"cidr,omitempty"`
}

// NodePool describes either the system AKS agent pool or the TMM AKS agent pool.
type NodePool struct {
	Role         string   `json:"role"` // system | tmm
	Name         string   `json:"name,omitempty"`
	InstanceType string   `json:"instanceType,omitempty"`
	DesiredCount int      `json:"desiredCount"`
	LiveNodes    []string `json:"liveNodes,omitempty"`
}

// TMMDataPlane captures the BNK TMM attachment details on the TMM node.
type TMMDataPlane struct {
	ManifestVersion string `json:"manifestVersion,omitempty"`
	DeploymentSize  string `json:"deploymentSize,omitempty"`
	CPU             string `json:"cpu,omitempty"`
	Memory          string `json:"memory,omitempty"`
	MTU             int    `json:"mtu,omitempty"`
	NodeName        string `json:"nodeName,omitempty"`
	ExtSelfIP       string `json:"extSelfIP,omitempty"`
	ExtPrefix       int    `json:"extPrefix,omitempty"`
	ExtPoolStart    string `json:"extPoolStart,omitempty"`
	ExtPoolEnd      string `json:"extPoolEnd,omitempty"`
	ExtPCI          string `json:"extPCI,omitempty"`
	ExtIfName       string `json:"extIfName,omitempty"`
	IntSelfIP       string `json:"intSelfIP,omitempty"`
	IntPrefix       int    `json:"intPrefix,omitempty"`
	IntPoolStart    string `json:"intPoolStart,omitempty"`
	IntPoolEnd      string `json:"intPoolEnd,omitempty"`
	IntPCI          string `json:"intPCI,omitempty"`
	IntIfName       string `json:"intIfName,omitempty"`
}

// Jumphost describes the dual-homed test/diagnostic VM when deployed.
type Jumphost struct {
	InstanceID string `json:"instanceId,omitempty"`
	MgmtIP     string `json:"mgmtIP,omitempty"`
	BnkExtIP   string `json:"bnkExtIP,omitempty"`
}

// NamespacePods summarizes running / total pods in a namespace (live mode).
type NamespacePods struct {
	Namespace string `json:"namespace"`
	Running   int    `json:"running"`
	Total     int    `json:"total"`
}

// Model is the complete topology snapshot passed to a renderer.
type Model struct {
	ClusterName       string `json:"clusterName"`
	SubscriptionID    string `json:"subscriptionId,omitempty"`
	Location          string `json:"location"`
	Region            string `json:"region,omitempty"`
	ResourceGroup     string `json:"resourceGroup,omitempty"`
	Pattern           string `json:"pattern"`
	KubernetesVersion string `json:"kubernetesVersion"`

	VNetName          string `json:"vnetName,omitempty"`
	VNetID            string `json:"vnetId,omitempty"`
	VNetCIDR          string `json:"vnetCidr"`
	VPCID             string `json:"vpcId,omitempty"`
	VPCCIDR           string `json:"vpcCidr,omitempty"`
	NatGWName         string `json:"natGwName,omitempty"`
	NatPIPAddress     string `json:"natPipAddress,omitempty"`
	IngressPIPAddress string `json:"ingressPipAddress,omitempty"`

	SystemSubnet   Subnet  `json:"systemSubnet"`
	PublicSubnet   *Subnet `json:"publicSubnet,omitempty"`
	ExternalSubnet Subnet  `json:"externalSubnet"`
	InternalSubnet *Subnet `json:"internalSubnet,omitempty"`

	SystemPool NodePool     `json:"systemPool"`
	TMMPool    NodePool     `json:"tmmPool"`
	TMM        TMMDataPlane `json:"tmm"`
	Jumphost   *Jumphost    `json:"jumphost,omitempty"`

	GatewayClassName string          `json:"gatewayClassName,omitempty"`
	VIPRange         string          `json:"vipRange,omitempty"`
	NamespacePods    []NamespacePods `json:"namespacePods,omitempty"`
}

// BNKNamespaces is the ordered list of namespaces queried in live mode.
var BNKNamespaces = []string{"f5-cne-core", "f5-cne-system", "f5-bnk-instance", "f5-utils"}

// Build constructs a Model from the declarative cluster.yaml and an optional
// state.env overlay. Either input may be nil, though cl is expected in normal
// operation. Never touches the network or filesystem.
func Build(cl *intent.Cluster, st *state.State) Model {
	var m Model
	if cl != nil {
		m.ClusterName = cl.EffectiveName()
		m.SubscriptionID = cl.Metadata.SubscriptionID
		m.Location = cl.EffectiveRegion()
		m.Region = cl.EffectiveRegion()
		m.ResourceGroup = cl.Metadata.ResourceGroup
		if m.ResourceGroup == "" && cl.Derived.ResourceGroupName != "" {
			m.ResourceGroup = cl.Derived.ResourceGroupName
		}
		m.Pattern = cl.Pattern
		if cl.ClusterSpec != nil && cl.ClusterSpec.KubernetesVersion != "" {
			m.KubernetesVersion = cl.ClusterSpec.KubernetesVersion
		}
		m.VNetName = cl.Derived.VNetName
		if m.VNetName == "" && cl.Network.Main.VPC != "" {
			m.VNetName = cl.Network.Main.VPC
		}
		vnetCIDR := cl.Network.VNetCIDR
		if vnetCIDR == "" {
			vnetCIDR = cl.Network.VPCCidr
		}
		if vnetCIDR == "" {
			vnetCIDR = cl.Network.Main.CIDR
		}
		m.VNetCIDR = vnetCIDR
		m.VPCCIDR = vnetCIDR
		m.NatGWName = cl.Derived.NatGatewayName

		sysCIDR := cl.Network.Subnets.System
		if sysCIDR == "" && len(cl.Network.Subnets.PrivateList) > 0 {
			sysCIDR = cl.Network.Subnets.PrivateList[0].CIDR
		}
		if sysCIDR == "" {
			sysCIDR = cl.Network.Main.CIDR
		}
		m.SystemSubnet = Subnet{
			Role: "system",
			Name: cl.Derived.SystemSubnetName,
			CIDR: sysCIDR,
		}

		pubCIDR := cl.Network.Subnets.Public
		if pubCIDR == "" && len(cl.Network.Subnets.PublicList) > 0 {
			pubCIDR = cl.Network.Subnets.PublicList[0].CIDR
		}
		if pubCIDR != "" {
			m.PublicSubnet = &Subnet{
				Role: "public",
				Name: cl.Derived.PublicSubnetName,
				CIDR: pubCIDR,
			}
		}

		extCIDR := cl.Network.Subnets.External
		if extCIDR == "" && cl.Network.External.CIDR != "" {
			extCIDR = cl.Network.External.CIDR
		}
		if extCIDR == "" && cl.Network.DataPath != nil {
			extCIDR = cl.Network.DataPath.External.CIDR
		}
		m.ExternalSubnet = Subnet{
			Role: "external",
			Name: cl.Derived.ExternalSubnetName,
			CIDR: extCIDR,
		}

		intCIDR := cl.Network.Subnets.Internal
		if intCIDR == "" && cl.Network.Internal.CIDR != "" {
			intCIDR = cl.Network.Internal.CIDR
		}
		if intCIDR == "" && cl.Network.DataPath != nil {
			intCIDR = cl.Network.DataPath.Internal.CIDR
		}
		if cl.HasInternalInterface() && intCIDR != "" {
			m.InternalSubnet = &Subnet{
				Role: "internal",
				Name: cl.Derived.InternalSubnetName,
				CIDR: intCIDR,
			}
		}

		if cl.ClusterSpec != nil && cl.ClusterSpec.SystemNodePool.Name != "" {
			m.SystemPool = NodePool{
				Role:         "system",
				Name:         cl.ClusterSpec.SystemNodePool.Name,
				InstanceType: cl.ClusterSpec.SystemNodePool.VMSize,
				DesiredCount: cl.ClusterSpec.SystemNodePool.Count,
			}
			m.TMMPool = NodePool{
				Role:         "tmm",
				Name:         cl.ClusterSpec.TMMNodePool.Name,
				InstanceType: cl.ClusterSpec.TMMNodePool.VMSize,
				DesiredCount: cl.ClusterSpec.TMMNodePool.Count,
			}
		} else if cl.ClusterSpec != nil && cl.ClusterSpec.DefaultPool.Name != "" {
			m.SystemPool = NodePool{
				Role:         "system",
				Name:         cl.ClusterSpec.DefaultPool.Name,
				InstanceType: cl.ClusterSpec.DefaultPool.MachineType,
				DesiredCount: cl.ClusterSpec.DefaultPool.NodeCount,
			}
			m.TMMPool = NodePool{
				Role:         "tmm",
				Name:         cl.ClusterSpec.TmmPool.Name,
				InstanceType: cl.ClusterSpec.TmmPool.MachineType,
				DesiredCount: cl.ClusterSpec.TmmPool.NodeCount,
			}
		} else if cl.ClusterSpec != nil && len(cl.ClusterSpec.NodeGroups) > 0 {
			ng := cl.ClusterSpec.NodeGroups[0]
			m.SystemPool = NodePool{
				Role:         "system",
				Name:         ng.Name,
				InstanceType: ng.InstanceType,
				DesiredCount: ng.DesiredSize,
			}
			m.TMMPool = m.SystemPool
			m.TMMPool.Role = "tmm"
		}

		if cl.Bnk != nil {
			m.TMM = TMMDataPlane{
				ManifestVersion: cl.EffectiveBNKVersion(),
				DeploymentSize:  cl.Bnk.DeploymentSize,
				CPU:             cl.Bnk.TmmCpu,
				Memory:          cl.Bnk.TmmMemory,
				MTU:             cl.Bnk.TmmMtuInt(),
			}
		}
		if ip, pfx := intent.DeriveSelfIP(extCIDR, 50); ip != "" {
			m.TMM.ExtSelfIP = ip
			m.TMM.ExtPrefix = pfx
			if s, e, err := intent.SelfIPPoolRange(ip); err == nil {
				m.TMM.ExtPoolStart, m.TMM.ExtPoolEnd = s, e
			}
		}
		if cl.HasInternalInterface() && intCIDR != "" {
			if ip, pfx := intent.DeriveSelfIP(intCIDR, 50); ip != "" {
				m.TMM.IntSelfIP = ip
				m.TMM.IntPrefix = pfx
				if s, e, err := intent.SelfIPPoolRange(ip); err == nil {
					m.TMM.IntPoolStart, m.TMM.IntPoolEnd = s, e
				}
			}
		}
		m.VIPRange = deriveVIPRange(extCIDR)
	}

	if st != nil {
		if v := st.Get("AZURE_RESOURCE_GROUP"); v != "" {
			m.ResourceGroup = v
		}
		if v := st.Get("AZURE_VNET_NAME"); v != "" {
			m.VNetName = v
		}
		if v := firstNonEmpty(st, "AZURE_VNET_ID", "VPC_ID"); v != "" {
			m.VNetID = v
			m.VPCID = v
		}
		if v := st.Get("AZURE_NAT_GW_NAME"); v != "" {
			m.NatGWName = v
		}
		if v := st.Get("AZURE_NAT_PIP_ADDRESS"); v != "" {
			m.NatPIPAddress = v
		}
		if v := st.Get("AZURE_INGRESS_PIP_ADDRESS"); v != "" {
			m.IngressPIPAddress = v
		}
		if v := st.Get("AZURE_SYSTEM_SUBNET_ID"); v != "" {
			m.SystemSubnet.ID = v
		}
		if v := st.Get("AZURE_EXTERNAL_SUBNET_ID"); v != "" {
			m.ExternalSubnet.ID = v
		}
		if v := st.Get("AZURE_INTERNAL_SUBNET_ID"); v != "" {
			if m.InternalSubnet == nil {
				m.InternalSubnet = &Subnet{Role: "internal"}
			}
			m.InternalSubnet.ID = v
		}
		if v := st.Get("AZURE_TMM_POOL_NAME"); v != "" {
			m.TMMPool.Name = v
		}
		if v := firstNonEmpty(st, "AZURE_TMM_NODE_NAME", "TMM_NODE_NAME"); v != "" {
			m.TMM.NodeName = v
		}
		if v := firstNonEmpty(st, "AZURE_EXT_SELF_IP", "TMM_EXT_SELFIP"); v != "" {
			m.TMM.ExtSelfIP = v
			if s, e, err := intent.SelfIPPoolRange(v); err == nil {
				m.TMM.ExtPoolStart, m.TMM.ExtPoolEnd = s, e
			}
		}
		if v := firstNonEmpty(st, "AZURE_INT_SELF_IP", "TMM_INT_SELFIP"); v != "" {
			m.TMM.IntSelfIP = v
			if s, e, err := intent.SelfIPPoolRange(v); err == nil {
				m.TMM.IntPoolStart, m.TMM.IntPoolEnd = s, e
			}
		}
		m.TMM.ExtPCI = st.Get("TMM_EXT_PCI")
		m.TMM.IntPCI = st.Get("TMM_INT_PCI")
		m.TMM.ExtIfName = st.Get("EXTERNAL_IFNAME")
		m.TMM.IntIfName = st.Get("INTERNAL_IFNAME")
		m.GatewayClassName = st.Get("GATEWAYCLASS_NAME")

		jhID := firstNonEmpty(st, "AZURE_JUMPHOST_VM_NAME", "JUMPHOST_INSTANCE_NAME", "JUMPHOST_INSTANCE_ID")
		jhMgmt := st.Get("JUMPHOST_MGMT_ENI_IP")
		jhExt := st.Get("JUMPHOST_BNK_EXT_ENI_IP")
		if jhID != "" || jhMgmt != "" || jhExt != "" {
			m.Jumphost = &Jumphost{
				InstanceID: jhID,
				MgmtIP:     jhMgmt,
				BnkExtIP:   jhExt,
			}
		}
	}

	return m
}

func firstNonEmpty(st *state.State, keys ...string) string {
	for _, k := range keys {
		if v := st.Get(k); v != "" {
			return v
		}
	}
	return ""
}

// deriveVIPRange returns "<a.b.c>.100-.119" for a /24 CIDR, or "" if the CIDR
// cannot be parsed as an IPv4 /24.
func deriveVIPRange(cidr string) string {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	b := ipnet.IP.To4()
	if b == nil {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.100-%d.%d.%d.119", b[0], b[1], b[2], b[0], b[1], b[2])
}

// EnrichFromK8s queries the live cluster via cs and populates LiveNodes on the
// two node pools (classified by the app=f5-tmm label) plus NamespacePods for
// each namespace in BNKNamespaces. Errors from individual calls are non-fatal:
// partial data is preserved and the first error encountered is returned so the
// caller can log a warning.
func EnrichFromK8s(ctx context.Context, cs kubernetes.Interface, m *Model) error {
	if cs == nil || m == nil {
		return nil
	}
	var firstErr error

	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		firstErr = fmt.Errorf("list nodes: %w", err)
	} else {
		var sys, tmm []string
		for _, n := range nodes.Items {
			if n.Labels["app"] == "f5-tmm" || n.Labels["node.kubernetes.io/Role"] == "tmm" {
				tmm = append(tmm, n.Name)
			} else {
				sys = append(sys, n.Name)
			}
		}
		sort.Strings(sys)
		sort.Strings(tmm)
		m.SystemPool.LiveNodes = sys
		m.TMMPool.LiveNodes = tmm
		if m.TMM.NodeName == "" && len(tmm) > 0 {
			m.TMM.NodeName = tmm[0]
		}
	}

	var nsPods []NamespacePods
	for _, ns := range BNKNamespaces {
		pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("list pods in %s: %w", ns, err)
			}
			continue
		}
		if len(pods.Items) == 0 {
			continue
		}
		running := 0
		for _, p := range pods.Items {
			if p.Status.Phase == "Running" {
				running++
			}
		}
		nsPods = append(nsPods, NamespacePods{
			Namespace: ns,
			Running:   running,
			Total:     len(pods.Items),
		})
	}
	m.NamespacePods = nsPods
	return firstErr
}

// RenderASCII returns a human-readable box diagram of the topology.
func RenderASCII(m Model) string {
	var b strings.Builder

	title := fmt.Sprintf("Cluster: %s  (location=%s  pattern=%s  k8s=%s)",
		orDash(m.ClusterName), orDash(m.Location), orDash(m.Pattern), orDash(m.KubernetesVersion))
	if m.ResourceGroup != "" {
		title += fmt.Sprintf("  [rg=%s]", m.ResourceGroup)
	}
	rule := strings.Repeat("=", len(title))
	fmt.Fprintln(&b, title)
	fmt.Fprintln(&b, rule)

	vnetHdr := fmt.Sprintf("VNet %s", orDash(m.VNetCIDR))
	if m.VNetName != "" {
		vnetHdr += fmt.Sprintf("  [%s]", m.VNetName)
	}
	if m.VNetID != "" {
		vnetHdr += fmt.Sprintf("  (%s)", m.VNetID)
	}
	fmt.Fprintf(&b, "\n+-- %s\n", vnetHdr)

	if m.NatGWName != "" || m.NatPIPAddress != "" || m.IngressPIPAddress != "" {
		fmt.Fprintf(&b, "|   Egress/Public IPs: natgw=%s (pip=%s)  ingress-pip=%s\n",
			orDash(m.NatGWName), orDash(m.NatPIPAddress), orDash(m.IngressPIPAddress))
	}

	fmt.Fprintln(&b, "|")
	fmt.Fprintln(&b, "|   Subnets:")
	writeSubnetASCII(&b, m.SystemSubnet)
	if m.PublicSubnet != nil {
		writeSubnetASCII(&b, *m.PublicSubnet)
	}
	writeSubnetASCII(&b, m.ExternalSubnet)
	if m.InternalSubnet != nil {
		writeSubnetASCII(&b, *m.InternalSubnet)
	}

	fmt.Fprintln(&b, "|")
	fmt.Fprintln(&b, "|   Compute:")
	writeNodePoolASCII(&b, "System Agent Pool", m.SystemPool, m.SystemSubnet.Role)
	writeNodePoolASCII(&b, "TMM Agent Pool   ", m.TMMPool, m.SystemSubnet.Role)

	fmt.Fprintln(&b, "|")
	fmt.Fprintf(&b, "|   BNK Data Plane (manifest=%s  size=%s  cpu=%s  mem=%s  mtu=%d):\n",
		orDash(m.TMM.ManifestVersion), orDash(m.TMM.DeploymentSize),
		orDash(m.TMM.CPU), orDash(m.TMM.Memory), m.TMM.MTU)
	if m.TMM.NodeName != "" {
		fmt.Fprintf(&b, "|     Host node  : %s\n", m.TMM.NodeName)
	}
	fmt.Fprintf(&b, "|     ext net    : self-IP %s/%d  pool [%s .. %s]",
		orDash(m.TMM.ExtSelfIP), m.TMM.ExtPrefix,
		orDash(m.TMM.ExtPoolStart), orDash(m.TMM.ExtPoolEnd))
	if m.TMM.ExtIfName != "" || m.TMM.ExtPCI != "" {
		fmt.Fprintf(&b, "  if=%s pci=%s", orDash(m.TMM.ExtIfName), orDash(m.TMM.ExtPCI))
	}
	fmt.Fprintln(&b)
	if m.VIPRange != "" {
		fmt.Fprintf(&b, "|     VIP range  : %s", m.VIPRange)
		if m.GatewayClassName != "" {
			fmt.Fprintf(&b, "  (GatewayClass=%s)", m.GatewayClassName)
		}
		fmt.Fprintln(&b)
	}
	if m.InternalSubnet != nil {
		fmt.Fprintf(&b, "|     int net    : self-IP %s/%d  pool [%s .. %s]",
			orDash(m.TMM.IntSelfIP), m.TMM.IntPrefix,
			orDash(m.TMM.IntPoolStart), orDash(m.TMM.IntPoolEnd))
		if m.TMM.IntIfName != "" || m.TMM.IntPCI != "" {
			fmt.Fprintf(&b, "  if=%s pci=%s", orDash(m.TMM.IntIfName), orDash(m.TMM.IntPCI))
		}
		fmt.Fprintln(&b)
	}

	if m.Jumphost != nil {
		fmt.Fprintln(&b, "|")
		fmt.Fprintf(&b, "|   Jumphost     : %s  mgmt=%s  bnk-ext=%s\n",
			orDash(m.Jumphost.InstanceID), orDash(m.Jumphost.MgmtIP), orDash(m.Jumphost.BnkExtIP))
	}

	if len(m.NamespacePods) > 0 {
		fmt.Fprintln(&b, "|")
		fmt.Fprintln(&b, "|   Live Pods:")
		for _, np := range m.NamespacePods {
			fmt.Fprintf(&b, "|     %-16s %d/%d Running\n", np.Namespace, np.Running, np.Total)
		}
	}

	fmt.Fprintln(&b, "+--")
	return b.String()
}

func writeSubnetASCII(b *strings.Builder, s Subnet) {
	line := fmt.Sprintf("|     %-12s %-18s", s.Role, orDash(s.CIDR))
	if s.Name != "" {
		line += fmt.Sprintf("  [%s]", s.Name)
	}
	if s.ID != "" {
		line += fmt.Sprintf("  (%s)", s.ID)
	}
	fmt.Fprintln(b, line)
}

func writeNodePoolASCII(b *strings.Builder, label string, np NodePool, subnetRole string) {
	fmt.Fprintf(b, "|     %s : %s x%d (%s) in %s",
		label, orDash(np.InstanceType), np.DesiredCount, orDash(np.Name), subnetRole)
	if len(np.LiveNodes) > 0 {
		fmt.Fprintf(b, "  live=[%s]", strings.Join(np.LiveNodes, ", "))
	}
	fmt.Fprintln(b)
}

// RenderMermaid emits a Mermaid flowchart (`flowchart TB`) suitable for pasting
// into Markdown / GitHub / Notion.
func RenderMermaid(m Model) string {
	var b strings.Builder
	fmt.Fprintln(&b, "flowchart TB")
	vnetLabel := fmt.Sprintf("VNet %s (%s)", orDash(m.VNetName), orDash(m.VNetCIDR))
	fmt.Fprintf(&b, "  subgraph VNET[%q]\n", vnetLabel)

	if m.NatGWName != "" || m.NatPIPAddress != "" {
		fmt.Fprintf(&b, "    NATGW[%q]\n", fmt.Sprintf("NAT GW %s (%s)", orDash(m.NatGWName), orDash(m.NatPIPAddress)))
	}

	writeSubnetMermaid(&b, "SUB_SYS", m.SystemSubnet)
	if m.PublicSubnet != nil {
		writeSubnetMermaid(&b, "SUB_PUB", *m.PublicSubnet)
	}
	writeSubnetMermaid(&b, "SUB_EXT", m.ExternalSubnet)
	if m.InternalSubnet != nil {
		writeSubnetMermaid(&b, "SUB_INT", *m.InternalSubnet)
	}

	fmt.Fprintf(&b, "    POOL_SYS[%q]\n",
		fmt.Sprintf("System Agent Pool\\n%s x%d", orDash(m.SystemPool.InstanceType), m.SystemPool.DesiredCount))
	fmt.Fprintf(&b, "    POOL_TMM[%q]\n",
		fmt.Sprintf("TMM Agent Pool\\n%s x%d\\n%s", orDash(m.TMMPool.InstanceType), m.TMMPool.DesiredCount, m.TMM.NodeName))
	fmt.Fprintf(&b, "    TMM_POD[%q]\n",
		fmt.Sprintf("f5-tmm pod\\nBNK %s (%s)\\next %s/%d\\nVIPs %s",
			orDash(m.TMM.ManifestVersion), orDash(m.TMM.DeploymentSize),
			orDash(m.TMM.ExtSelfIP), m.TMM.ExtPrefix, orDash(m.VIPRange)))

	if m.Jumphost != nil {
		fmt.Fprintf(&b, "    JH[%q]\n",
			fmt.Sprintf("Jumphost %s\\nmgmt %s\\nbnk-ext %s",
				orDash(m.Jumphost.InstanceID), orDash(m.Jumphost.MgmtIP), orDash(m.Jumphost.BnkExtIP)))
	}

	fmt.Fprintln(&b, "  end")

	fmt.Fprintln(&b, "  SUB_SYS --> POOL_SYS")
	fmt.Fprintln(&b, "  SUB_SYS --> POOL_TMM")
	fmt.Fprintln(&b, "  POOL_TMM --> TMM_POD")
	fmt.Fprintln(&b, "  SUB_EXT -. secondary NIC / host-device .-> TMM_POD")
	if m.InternalSubnet != nil {
		fmt.Fprintln(&b, "  SUB_INT -. secondary NIC / host-device .-> TMM_POD")
	}
	if m.NatGWName != "" || m.NatPIPAddress != "" {
		fmt.Fprintln(&b, "  SUB_SYS --> NATGW")
	}
	if m.Jumphost != nil {
		if m.PublicSubnet != nil {
			fmt.Fprintln(&b, "  SUB_PUB --> JH")
		} else {
			fmt.Fprintln(&b, "  SUB_SYS --> JH")
		}
		fmt.Fprintln(&b, "  JH --> SUB_EXT")
	}
	return b.String()
}

func writeSubnetMermaid(b *strings.Builder, nodeID string, s Subnet) {
	label := fmt.Sprintf("%s\\n%s", s.Role, orDash(s.CIDR))
	if s.Name != "" {
		label += "\\n" + s.Name
	}
	fmt.Fprintf(b, "    %s[%q]\n", nodeID, label)
}

// RenderDOT emits a Graphviz DOT digraph of the topology.
func RenderDOT(m Model) string {
	var b strings.Builder
	fmt.Fprintf(&b, "digraph %q {\n", orDash(m.ClusterName))
	fmt.Fprintln(&b, "  rankdir=TB;")
	fmt.Fprintln(&b, "  node [shape=box, fontname=\"Helvetica\"];")
	fmt.Fprintf(&b, "  subgraph cluster_vnet {\n    label=%q;\n",
		fmt.Sprintf("VNet %s (%s)", orDash(m.VNetName), orDash(m.VNetCIDR)))

	writeSubnetDOT(&b, "sub_sys", m.SystemSubnet)
	if m.PublicSubnet != nil {
		writeSubnetDOT(&b, "sub_pub", *m.PublicSubnet)
	}
	writeSubnetDOT(&b, "sub_ext", m.ExternalSubnet)
	if m.InternalSubnet != nil {
		writeSubnetDOT(&b, "sub_int", *m.InternalSubnet)
	}

	fmt.Fprintf(&b, "    pool_sys [label=%q];\n",
		fmt.Sprintf("System Agent Pool\n%s x%d", orDash(m.SystemPool.InstanceType), m.SystemPool.DesiredCount))
	fmt.Fprintf(&b, "    pool_tmm [label=%q];\n",
		fmt.Sprintf("TMM Agent Pool\n%s x%d", orDash(m.TMMPool.InstanceType), m.TMMPool.DesiredCount))
	fmt.Fprintf(&b, "    tmm_pod [label=%q];\n",
		fmt.Sprintf("f5-tmm (%s)\next %s/%d\nVIPs %s",
			orDash(m.TMM.ManifestVersion), orDash(m.TMM.ExtSelfIP), m.TMM.ExtPrefix, orDash(m.VIPRange)))
	if m.Jumphost != nil {
		fmt.Fprintf(&b, "    jumphost [label=%q];\n",
			fmt.Sprintf("Jumphost %s\nmgmt %s\nbnk-ext %s",
				orDash(m.Jumphost.InstanceID), orDash(m.Jumphost.MgmtIP), orDash(m.Jumphost.BnkExtIP)))
	}
	fmt.Fprintln(&b, "  }")

	fmt.Fprintln(&b, "  sub_sys -> pool_sys;")
	fmt.Fprintln(&b, "  sub_sys -> pool_tmm;")
	fmt.Fprintln(&b, "  pool_tmm -> tmm_pod;")
	fmt.Fprintln(&b, "  sub_ext -> tmm_pod [style=dashed, label=\"ext NIC\"];")
	if m.InternalSubnet != nil {
		fmt.Fprintln(&b, "  sub_int -> tmm_pod [style=dashed, label=\"int NIC\"];")
	}
	if m.Jumphost != nil {
		if m.PublicSubnet != nil {
			fmt.Fprintln(&b, "  sub_pub -> jumphost;")
		} else {
			fmt.Fprintln(&b, "  sub_sys -> jumphost;")
		}
		fmt.Fprintln(&b, "  jumphost -> sub_ext;")
	}
	fmt.Fprintln(&b, "}")
	return b.String()
}

func writeSubnetDOT(b *strings.Builder, id string, s Subnet) {
	label := fmt.Sprintf("%s\n%s", s.Role, orDash(s.CIDR))
	if s.Name != "" {
		label += "\n" + s.Name
	}
	fmt.Fprintf(b, "    %s [label=%q];\n", id, label)
}

// RenderJSON returns the pretty-printed JSON serialization of Model.
func RenderJSON(m Model) (string, error) {
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out) + "\n", nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
