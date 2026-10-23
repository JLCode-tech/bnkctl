package scenarios

import (
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

func dualCluster() *intent.Cluster {
	return &intent.Cluster{
		Pattern: intent.PatternDualInterface,
		Network: intent.Network{
			VPCCidr: "10.50.0.0/16",
			DataPath: &intent.DataPathSpec{
				External: intent.SubnetSpec{CIDR: "10.50.10.0/24"},
				Internal: intent.SubnetSpec{CIDR: "10.50.20.0/24"},
				SelfIPs:  &intent.SelfIPsSpec{External: "10.50.10.240", Internal: "10.50.20.240"},
			},
		},
	}
}

func TestNetVarsFor(t *testing.T) {
	st, _ := state.Load(t.TempDir())
	st.Set("NODE_PRIMARY_IFNAME", "ens5")
	st.Set("TMM_EXT_SELFIP", "10.50.10.241") // state wins over the intent's derived value

	v := NetVarsFor(dualCluster(), st)
	want := NetVars{NodeIfname: "ens5", VpcNet: "10.50.0.0", VpcPrefixLen: 16,
		ExtGateway: "10.50.10.1", IntGateway: "10.50.20.1", TMMExtSelfIP: "10.50.10.241", TMMIntSelfIP: "10.50.20.240"}
	if v != want {
		t.Errorf("NetVarsFor = %+v, want %+v", v, want)
	}

	// Single-interface: no IntGateway; nil state and nil cluster are fine.
	cl := dualCluster()
	cl.Pattern = intent.PatternExternalOnly
	cl.Network.DataPath.Internal = intent.SubnetSpec{}
	if got := NetVarsFor(cl, nil); got.IntGateway != "" || got.ExtGateway != "10.50.10.1" || got.TMMExtSelfIP != "10.50.10.240" {
		t.Errorf("single-interface NetVars = %+v", got)
	}
	if got := NetVarsFor(nil, nil); got != (NetVars{}) {
		t.Errorf("nil inputs should give zero NetVars, got %+v", got)
	}
}

func TestTemplateVars(t *testing.T) {
	st, _ := state.Load(t.TempDir())
	st.Set("NODE_PRIMARY_IFNAME", "ens5")
	st.Set("VPC_ID", "vpc-123")
	m := TemplateVars(dualCluster(), st)
	for k, want := range map[string]any{"VPC_ID": "vpc-123", "NodeIfname": "ens5", "ExtGateway": "10.50.10.1", "VpcPrefixLen": 16} {
		if m[k] != want {
			t.Errorf("TemplateVars[%s] = %v, want %v", k, m[k], want)
		}
	}
	out, err := RenderTemplate("gw={{.ExtGateway}} nic={{.NodeIfname}} vpc={{.VPC_ID}}", m)
	if err != nil || out != "gw=10.50.10.1 nic=ens5 vpc=vpc-123" {
		t.Errorf("render = %q, %v", out, err)
	}
}

func TestSubnetHelpers(t *testing.T) {
	if gw := SubnetGateway("10.50.20.0/24"); gw != "10.50.20.1" {
		t.Errorf("SubnetGateway = %q, want 10.50.20.1", gw)
	}
	if gw := SubnetGateway("bad"); gw != "" {
		t.Errorf("SubnetGateway(bad) = %q, want empty", gw)
	}
	if n, l := SplitCIDR("10.50.0.0/16"); n != "10.50.0.0" || l != 16 {
		t.Errorf("SplitCIDR = %q/%d", n, l)
	}
}
