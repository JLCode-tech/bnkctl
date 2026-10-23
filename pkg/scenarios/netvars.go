package scenarios

import (
	"net"
	"strings"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/netcalc"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

// NetVars are the cluster-derived network values templated manifests can
// reference — scenario manifests and `k apply --config` examples alike — so
// no example needs a hardcoded interface name, gateway, or SelfIP.
type NetVars struct {
	// NodeIfname is the worker's primary NIC (state NODE_PRIMARY_IFNAME, fallback TMM_EXT_IFACE).
	NodeIfname string
	// VpcNet / VpcPrefixLen split the cluster VPC/VNet CIDR.
	VpcNet       string
	VpcPrefixLen int
	// ExtGateway / IntGateway are the subnet routers (first host address)
	// of the external / internal dataplane subnets.
	ExtGateway string
	IntGateway string
	// TMMExtSelfIP / TMMIntSelfIP come from state with the intent's
	// derived values as fallback.
	TMMExtSelfIP string
	TMMIntSelfIP string
	// VIP and AgentCoreVIP are derived VIP addresses on the external subnet.
	VIP          string
	AgentCoreVIP string
}

// NetVarsFor derives NetVars from cluster.yaml and state.env across AWS, Azure, and GCP;
// either argument may be nil.
func NetVarsFor(cl *intent.Cluster, st *state.State) NetVars {
	var v NetVars
	if st != nil {
		v.NodeIfname = st.Get("NODE_PRIMARY_IFNAME")
		if v.NodeIfname == "" {
			v.NodeIfname = st.Get("TMM_EXT_IFACE")
		}
		v.TMMExtSelfIP = st.Get("TMM_EXT_SELFIP")
		v.TMMIntSelfIP = st.Get("TMM_INT_SELFIP")
		v.VIP = st.Get("GATEWAY_VIP")
	}
	if cl == nil {
		return v
	}

	switch {
	case cl.Network.VPCCidr != "":
		v.VpcNet, v.VpcPrefixLen = SplitCIDR(cl.Network.VPCCidr)
	case cl.Network.VNetCIDR != "":
		v.VpcNet, v.VpcPrefixLen = SplitCIDR(cl.Network.VNetCIDR)
	case cl.Network.Main.CIDR != "":
		v.VpcNet, v.VpcPrefixLen = SplitCIDR(cl.Network.Main.CIDR)
	}

	if dp := cl.Network.DataPath; dp != nil {
		v.ExtGateway = SubnetGateway(dp.External.CIDR)
		if cl.HasInternalInterface() {
			v.IntGateway = SubnetGateway(dp.Internal.CIDR)
		}
		if dp.SelfIPs != nil {
			if v.TMMExtSelfIP == "" {
				v.TMMExtSelfIP = dp.SelfIPs.External
			}
			if v.TMMIntSelfIP == "" {
				v.TMMIntSelfIP = dp.SelfIPs.Internal
			}
		}
	} else if cl.Network.Subnets.External != "" {
		v.ExtGateway = SubnetGateway(cl.Network.Subnets.External)
		if cl.HasInternalInterface() && cl.Network.Subnets.Internal != "" {
			v.IntGateway = SubnetGateway(cl.Network.Subnets.Internal)
		}
		if ip, err := netcalc.Nth(cl.Network.Subnets.External, 107); err == nil {
			v.AgentCoreVIP = ip
		}
		if v.VIP == "" {
			if vip, err := cl.DefaultVIP(); err == nil {
				v.VIP = vip
			}
		}
	} else {
		if cl.ExtGwIP != "" {
			v.ExtGateway = cl.ExtGwIP
		} else if cl.Network.External.CIDR != "" {
			v.ExtGateway = SubnetGateway(cl.Network.External.CIDR)
		}
		if cl.IntGwIP != "" {
			v.IntGateway = cl.IntGwIP
		} else if cl.Network.Internal.CIDR != "" {
			v.IntGateway = SubnetGateway(cl.Network.Internal.CIDR)
		}
		if v.VIP == "" {
			if vip, err := cl.DefaultVIP(); err == nil {
				v.VIP = vip
			}
		}
		if v.AgentCoreVIP == "" && v.VIP != "" {
			v.AgentCoreVIP = withLastOctetNetvars(v.VIP, "150")
		}
	}
	return v
}

// TemplateVars is the data `k apply --config` renders example manifests with:
// every state.env key by name plus the NetVars fields.
func TemplateVars(cl *intent.Cluster, st *state.State) map[string]any {
	m := map[string]any{}
	if st != nil {
		for k, val := range st.All() {
			m[k] = val
		}
	}
	nv := NetVarsFor(cl, st)
	m["NodeIfname"] = nv.NodeIfname
	m["VpcNet"], m["VpcPrefixLen"] = nv.VpcNet, nv.VpcPrefixLen
	m["ExtGateway"], m["IntGateway"] = nv.ExtGateway, nv.IntGateway
	m["TMMExtSelfIP"], m["TMMIntSelfIP"] = nv.TMMExtSelfIP, nv.TMMIntSelfIP
	if nv.VIP != "" {
		m["VIP"] = nv.VIP
	}
	if nv.AgentCoreVIP != "" {
		m["AgentCoreVIP"] = nv.AgentCoreVIP
	}
	return m
}

func withLastOctetNetvars(ip, octet string) string {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return ip
	}
	parts[3] = octet
	return strings.Join(parts, ".")
}

// SplitCIDR returns the network address and prefix length of cidr ("" / 0 if invalid).
func SplitCIDR(cidr string) (string, int) {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", 0
	}
	ones, _ := n.Mask.Size()
	return n.IP.String(), ones
}

// SubnetGateway is the subnet router: the first usable address of the CIDR.
func SubnetGateway(cidr string) string {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	gw := make(net.IP, len(n.IP))
	copy(gw, n.IP)
	gw[len(gw)-1]++
	return gw.String()
}
