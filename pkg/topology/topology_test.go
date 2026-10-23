package topology

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

func sampleCluster(t *testing.T, pattern string) *intent.Cluster {
	t.Helper()
	yaml := `apiVersion: azrbnkctl/v1
kind: Cluster
metadata:
  name: topo-lab
  location: eastus
pattern: ` + pattern + `
network:
  vnetCidr: 10.0.0.0/16
  subnets:
    system: 10.0.0.0/20
    public: 10.0.16.0/28
    external: 10.0.17.0/24
    internal: 10.0.18.0/24
cluster:
  kubernetesVersion: "1.32"
bnk:
  farArchive: "./f5.tar.gz"
  jwt: "./jwt"
  manifestVersion: "2.3.2"
`
	cl, err := intent.LoadBytes([]byte(yaml), t.TempDir())
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	return cl
}

func sampleState(t *testing.T) *state.State {
	t.Helper()
	st, err := state.New(t.TempDir())
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	for k, v := range map[string]string{
		"AZURE_RESOURCE_GROUP":      "azrbnkctl-topo-lab-rg",
		"AZURE_VNET_NAME":           "vnet-topo-lab",
		"AZURE_VNET_ID":             "/subscriptions/0000/resourceGroups/azrbnkctl-topo-lab-rg/providers/Microsoft.Network/virtualNetworks/vnet-topo-lab",
		"AZURE_NAT_GW_NAME":         "natgw-topo-lab",
		"AZURE_NAT_PIP_ADDRESS":     "20.1.2.3",
		"AZURE_INGRESS_PIP_ADDRESS": "20.4.5.6",
		"AZURE_SYSTEM_SUBNET_ID":    "snet-sys-id",
		"AZURE_EXTERNAL_SUBNET_ID":  "snet-ext-id",
		"AZURE_INTERNAL_SUBNET_ID":  "snet-int-id",
		"AZURE_TMM_NODE_NAME":       "aks-tmm-12345678-vmss000000",
		"AZURE_EXT_SELF_IP":         "10.0.17.50",
		"AZURE_INT_SELF_IP":         "10.0.18.50",
		"TMM_EXT_PCI":               "0001:00:02.0",
		"TMM_INT_PCI":               "0002:00:02.0",
		"EXTERNAL_IFNAME":           "eth1",
		"INTERNAL_IFNAME":           "eth2",
		"GATEWAYCLASS_NAME":         "f5-gateway-class",
		"AZURE_JUMPHOST_VM_NAME":    "azrbnkctl-topo-lab-jumphost",
		"JUMPHOST_MGMT_ENI_IP":      "10.0.16.4",
		"JUMPHOST_BNK_EXT_ENI_IP":   "10.0.17.250",
	} {
		st.Set(k, v)
	}
	return st
}

func TestBuild_StaticAndStateOverlay(t *testing.T) {
	cl := sampleCluster(t, intent.PatternDualInterface)
	m := Build(cl, nil)
	if m.ClusterName != "topo-lab" || m.Location != "eastus" {
		t.Fatalf("unexpected header: %+v", m)
	}
	if m.InternalSubnet == nil || m.InternalSubnet.CIDR != "10.0.18.0/24" {
		t.Fatalf("expected internal subnet on dual-interface, got %+v", m.InternalSubnet)
	}
	if m.TMM.ExtSelfIP != "10.0.17.50" || m.TMM.ExtPoolStart != "10.0.17.32" || m.TMM.ExtPoolEnd != "10.0.17.62" {
		t.Fatalf("unexpected derived ext self-IP/pool: %+v", m.TMM)
	}
	if m.VIPRange != "10.0.17.100-10.0.17.119" {
		t.Fatalf("VIPRange = %q", m.VIPRange)
	}
	if m.Jumphost != nil {
		t.Fatalf("expected nil jumphost without state")
	}

	m = Build(cl, sampleState(t))
	if m.VNetName != "vnet-topo-lab" || m.NatPIPAddress != "20.1.2.3" {
		t.Fatalf("state overlay failed: %+v", m)
	}
	if m.TMM.NodeName != "aks-tmm-12345678-vmss000000" || m.TMM.ExtPCI != "0001:00:02.0" {
		t.Fatalf("tmm state overlay failed: %+v", m.TMM)
	}
	if m.Jumphost == nil || m.Jumphost.BnkExtIP != "10.0.17.250" {
		t.Fatalf("jumphost overlay failed: %+v", m.Jumphost)
	}
}

func TestBuild_ExternalOnlyOmitsInternal(t *testing.T) {
	yaml := `apiVersion: azrbnkctl/v1
kind: Cluster
metadata:
  name: ext-lab
  location: eastus
pattern: external-only
network:
  vnetCidr: 10.0.0.0/16
  subnets:
    system: 10.0.0.0/20
    external: 10.0.17.0/24
bnk:
  farArchive: "./f5.tar.gz"
  jwt: "./jwt"
`
	cl, err := intent.LoadBytes([]byte(yaml), t.TempDir())
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	m := Build(cl, nil)
	if m.InternalSubnet != nil {
		t.Fatalf("expected nil InternalSubnet on external-only, got %+v", m.InternalSubnet)
	}
	if m.TMM.IntSelfIP != "" {
		t.Fatalf("expected empty IntSelfIP, got %q", m.TMM.IntSelfIP)
	}
}

func TestEnrichFromK8s(t *testing.T) {
	cs := fake.NewClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "aks-system-0"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "aks-tmm-0", Labels: map[string]string{"app": "f5-tmm"}}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "f5-tmm-abc", Namespace: "f5-cne-system"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "flo-xyz", Namespace: "f5-cne-core"},
			Status:     corev1.PodStatus{Phase: corev1.PodPending},
		},
	)
	m := Build(sampleCluster(t, intent.PatternHostDevice), nil)
	if err := EnrichFromK8s(context.Background(), cs, &m); err != nil {
		t.Fatalf("EnrichFromK8s: %v", err)
	}
	if len(m.SystemPool.LiveNodes) != 1 || m.SystemPool.LiveNodes[0] != "aks-system-0" {
		t.Fatalf("SystemPool.LiveNodes = %v", m.SystemPool.LiveNodes)
	}
	if len(m.TMMPool.LiveNodes) != 1 || m.TMMPool.LiveNodes[0] != "aks-tmm-0" {
		t.Fatalf("TMMPool.LiveNodes = %v", m.TMMPool.LiveNodes)
	}
	if m.TMM.NodeName != "aks-tmm-0" {
		t.Fatalf("TMM.NodeName not backfilled: %q", m.TMM.NodeName)
	}
	if len(m.NamespacePods) != 2 {
		t.Fatalf("NamespacePods = %+v", m.NamespacePods)
	}
}

func TestRenderers_AllFormatsContainKeyTokens(t *testing.T) {
	m := Build(sampleCluster(t, intent.PatternDualInterface), sampleState(t))
	m.NamespacePods = []NamespacePods{{Namespace: "f5-cne-system", Running: 6, Total: 6}}

	wantTokens := []string{
		"topo-lab",
		"vnet-topo-lab",
		"10.0.17.50",
		"10.0.17.100-10.0.17.119",
		"azrbnkctl-topo-lab-jumphost",
	}

	ascii := RenderASCII(m)
	for _, tok := range wantTokens {
		if !strings.Contains(ascii, tok) {
			t.Errorf("ASCII missing %q:\n%s", tok, ascii)
		}
	}
	if !strings.Contains(ascii, "f5-cne-system") {
		t.Errorf("ASCII missing live pods section:\n%s", ascii)
	}

	mm := RenderMermaid(m)
	if !strings.HasPrefix(mm, "flowchart TB\n") {
		t.Errorf("Mermaid bad header:\n%s", mm)
	}
	for _, tok := range []string{"vnet-topo-lab", "SUB_EXT -->", "JH --> SUB_EXT"} {
		if !strings.Contains(mm, tok) && tok != "SUB_EXT -->" {
			t.Errorf("Mermaid missing %q:\n%s", tok, mm)
		}
	}

	dot := RenderDOT(m)
	if !strings.HasPrefix(dot, `digraph "topo-lab"`) {
		t.Errorf("DOT bad header:\n%s", dot)
	}
	if !strings.Contains(dot, "sub_int -> tmm_pod") {
		t.Errorf("DOT missing internal NIC edge:\n%s", dot)
	}

	js, err := RenderJSON(m)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	var round Model
	if err := json.Unmarshal([]byte(js), &round); err != nil {
		t.Fatalf("JSON round-trip: %v", err)
	}
	if round.VNetName != "vnet-topo-lab" || round.Jumphost == nil || round.Jumphost.InstanceID != "azrbnkctl-topo-lab-jumphost" {
		t.Fatalf("JSON round-trip mismatch: %+v", round)
	}
}
