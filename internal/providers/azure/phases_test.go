package azure

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

type fakeRGClient struct {
	ResourceGroupsAPI
	getErr     error
	createdRG  string
	createdLoc string
}

func (f *fakeRGClient) Get(ctx context.Context, rg string, opt *armresources.ResourceGroupsClientGetOptions) (armresources.ResourceGroupsClientGetResponse, error) {
	if f.getErr != nil {
		return armresources.ResourceGroupsClientGetResponse{}, f.getErr
	}
	return armresources.ResourceGroupsClientGetResponse{
		ResourceGroup: armresources.ResourceGroup{
			ID:   to.Ptr("/subscriptions/sub-123/resourceGroups/" + rg),
			Name: to.Ptr(rg),
		},
	}, nil
}

func (f *fakeRGClient) CreateOrUpdate(ctx context.Context, rg string, p armresources.ResourceGroup, opt *armresources.ResourceGroupsClientCreateOrUpdateOptions) (armresources.ResourceGroupsClientCreateOrUpdateResponse, error) {
	f.createdRG = rg
	if p.Location != nil {
		f.createdLoc = *p.Location
	}
	return armresources.ResourceGroupsClientCreateOrUpdateResponse{
		ResourceGroup: armresources.ResourceGroup{
			ID:   to.Ptr("/subscriptions/sub-123/resourceGroups/" + rg),
			Name: to.Ptr(rg),
		},
	}, nil
}

type fakeNSGClient struct {
	SecurityGroupsAPI
	createdNSG string
	deletedNSG string
	ruleCount  int
}

func (f *fakeNSGClient) BeginCreateOrUpdate(ctx context.Context, rg, name string, p armnetwork.SecurityGroup, opt *armnetwork.SecurityGroupsClientBeginCreateOrUpdateOptions) (*runtime.Poller[armnetwork.SecurityGroupsClientCreateOrUpdateResponse], error) {
	f.createdNSG = name
	if p.Properties != nil {
		f.ruleCount = len(p.Properties.SecurityRules)
	}
	return nil, nil
}

func (f *fakeNSGClient) BeginDelete(ctx context.Context, rg, name string, opt *armnetwork.SecurityGroupsClientBeginDeleteOptions) (*runtime.Poller[armnetwork.SecurityGroupsClientDeleteResponse], error) {
	f.deletedNSG = name
	return nil, nil
}

type fakeVNetClient struct {
	VirtualNetworksAPI
	createdVNet string
	deletedVNet string
	subnetCount int
}

func (f *fakeVNetClient) BeginCreateOrUpdate(ctx context.Context, rg, name string, p armnetwork.VirtualNetwork, opt *armnetwork.VirtualNetworksClientBeginCreateOrUpdateOptions) (*runtime.Poller[armnetwork.VirtualNetworksClientCreateOrUpdateResponse], error) {
	f.createdVNet = name
	if p.Properties != nil {
		f.subnetCount = len(p.Properties.Subnets)
	}
	return nil, nil
}

func (f *fakeVNetClient) BeginDelete(ctx context.Context, rg, name string, opt *armnetwork.VirtualNetworksClientBeginDeleteOptions) (*runtime.Poller[armnetwork.VirtualNetworksClientDeleteResponse], error) {
	f.deletedVNet = name
	return nil, nil
}

type fakePIPClient struct {
	PublicIPAddressesAPI
	createdPIPs []string
	deletedPIPs []string
}

func (f *fakePIPClient) BeginCreateOrUpdate(ctx context.Context, rg, name string, p armnetwork.PublicIPAddress, opt *armnetwork.PublicIPAddressesClientBeginCreateOrUpdateOptions) (*runtime.Poller[armnetwork.PublicIPAddressesClientCreateOrUpdateResponse], error) {
	f.createdPIPs = append(f.createdPIPs, name)
	return nil, nil
}

func (f *fakePIPClient) BeginDelete(ctx context.Context, rg, name string, opt *armnetwork.PublicIPAddressesClientBeginDeleteOptions) (*runtime.Poller[armnetwork.PublicIPAddressesClientDeleteResponse], error) {
	f.deletedPIPs = append(f.deletedPIPs, name)
	return nil, nil
}

type fakeNATClient struct {
	NatGatewaysAPI
	createdNAT string
	deletedNAT string
}

func (f *fakeNATClient) BeginCreateOrUpdate(ctx context.Context, rg, name string, p armnetwork.NatGateway, opt *armnetwork.NatGatewaysClientBeginCreateOrUpdateOptions) (*runtime.Poller[armnetwork.NatGatewaysClientCreateOrUpdateResponse], error) {
	f.createdNAT = name
	return nil, nil
}

func (f *fakeNATClient) BeginDelete(ctx context.Context, rg, name string, opt *armnetwork.NatGatewaysClientBeginDeleteOptions) (*runtime.Poller[armnetwork.NatGatewaysClientDeleteResponse], error) {
	f.deletedNAT = name
	return nil, nil
}

type fakeAKSClient struct {
	ManagedClustersAPI
	createdCluster string
	deletedCluster string
	kubeconfig     []byte
}

func (f *fakeAKSClient) BeginCreateOrUpdate(ctx context.Context, rg, name string, p armcontainerservice.ManagedCluster, opt *armcontainerservice.ManagedClustersClientBeginCreateOrUpdateOptions) (*runtime.Poller[armcontainerservice.ManagedClustersClientCreateOrUpdateResponse], error) {
	f.createdCluster = name
	return nil, nil
}

func (f *fakeAKSClient) BeginDelete(ctx context.Context, rg, name string, opt *armcontainerservice.ManagedClustersClientBeginDeleteOptions) (*runtime.Poller[armcontainerservice.ManagedClustersClientDeleteResponse], error) {
	f.deletedCluster = name
	return nil, nil
}

func (f *fakeAKSClient) ListClusterAdminCredentials(ctx context.Context, rg, name string, opt *armcontainerservice.ManagedClustersClientListClusterAdminCredentialsOptions) (armcontainerservice.ManagedClustersClientListClusterAdminCredentialsResponse, error) {
	return armcontainerservice.ManagedClustersClientListClusterAdminCredentialsResponse{
		CredentialResults: armcontainerservice.CredentialResults{
			Kubeconfigs: []*armcontainerservice.CredentialResult{
				{Value: f.kubeconfig},
			},
		},
	}, nil
}

type fakeAgentPoolClient struct {
	AgentPoolsAPI
	createdPool string
	deletedPool string
}

func (f *fakeAgentPoolClient) BeginCreateOrUpdate(ctx context.Context, rg, clusterName, poolName string, p armcontainerservice.AgentPool, opt *armcontainerservice.AgentPoolsClientBeginCreateOrUpdateOptions) (*runtime.Poller[armcontainerservice.AgentPoolsClientCreateOrUpdateResponse], error) {
	f.createdPool = poolName
	return nil, nil
}

func (f *fakeAgentPoolClient) BeginDelete(ctx context.Context, rg, clusterName, poolName string, opt *armcontainerservice.AgentPoolsClientBeginDeleteOptions) (*runtime.Poller[armcontainerservice.AgentPoolsClientDeleteResponse], error) {
	f.deletedPool = poolName
	return nil, nil
}

func testAzureClusterIntent() *intent.Cluster {
	return &intent.Cluster{
		APIVersion: intent.APIVersionAZR,
		Kind:       intent.Kind,
		Cloud:      &intent.CloudSpec{Provider: intent.ProviderAzure},
		Metadata: intent.Metadata{
			Name:           "azr-bnk",
			SubscriptionID: "11111111-2222-3333-4444-555555555555",
			ResourceGroup:  "azr-bnk-rg",
			Location:       "eastus2",
		},
		Pattern: intent.PatternDualInterface,
		Network: intent.Network{
			VNetCIDR: "10.60.0.0/16",
			Subnets: intent.SubnetsSpec{
				System:   "10.60.1.0/24",
				Public:   "10.60.2.0/24",
				External: "10.60.100.0/24",
				Internal: "10.60.101.0/24",
			},
		},
		ClusterSpec: &intent.ClusterSpec{
			KubernetesVersion: "1.30",
			SystemNodePool: intent.NodePoolSpec{
				Name:   "system",
				VMSize: "Standard_D4s_v5",
				Count:  1,
			},
			TMMNodePool: intent.NodePoolSpec{
				Name:   "tmm",
				VMSize: "Standard_D8s_v5",
				Count:  1,
			},
		},
	}
}

func TestAzurePhasesDryRunAndLive(t *testing.T) {
	ctx := context.Background()
	cl := testAzureClusterIntent()
	st, err := state.Load(t.TempDir())
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}

	// 1. Dry-run through Provider.ProvisionUnderlay
	pDry := NewProvider()
	uDry, err := pDry.ProvisionUnderlay(ctx, cl, st, true)
	if err != nil {
		t.Fatalf("ProvisionUnderlay(dryRun=true): %v", err)
	}
	if uDry.ClusterName != "azr-bnk" {
		t.Fatalf("expected cluster name azr-bnk, got %s", uDry.ClusterName)
	}
	if st.Get("AZURE_VNET_NAME") != "azr-bnk-vnet" {
		t.Fatalf("expected AZURE_VNET_NAME=azr-bnk-vnet, got %q", st.Get("AZURE_VNET_NAME"))
	}
	if st.Get("AZURE_EXT_GW_IP") != "10.60.100.1" || st.Get("AZURE_INT_GW_IP") != "10.60.101.1" {
		t.Fatalf("unexpected gateway IPs in dry-run: ext=%q int=%q", st.Get("AZURE_EXT_GW_IP"), st.Get("AZURE_INT_GW_IP"))
	}

	// 2. Live execution with fake ARM + K8s clients
	stLive, err := state.Load(t.TempDir())
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	rgMock := &fakeRGClient{getErr: fmt.Errorf("not found")}
	nsgMock := &fakeNSGClient{}
	vnetMock := &fakeVNetClient{}
	pipMock := &fakePIPClient{}
	natMock := &fakeNATClient{}
	aksMock := &fakeAKSClient{kubeconfig: []byte("apiVersion: v1\nkind: Config\n")}
	poolMock := &fakeAgentPoolClient{}
	k8sMock := k8sfake.NewClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "aks-tmm-vmss000000",
			Labels: map[string]string{"app": "f5-tmm"},
		},
	})

	pLive := &Provider{
		Clients: &Clients{
			SubscriptionID:    cl.Metadata.SubscriptionID,
			ResourceGroups:    rgMock,
			SecurityGroups:    nsgMock,
			VirtualNetworks:   vnetMock,
			PublicIPAddresses: pipMock,
			NatGateways:       natMock,
			ManagedClusters:   aksMock,
			AgentPools:        poolMock,
			K8s:               k8sMock,
		},
	}

	uLive, err := pLive.ProvisionUnderlay(ctx, cl, stLive, false)
	if err != nil {
		t.Fatalf("ProvisionUnderlay(dryRun=false): %v", err)
	}

	if rgMock.createdRG != "azr-bnk-rg" || rgMock.createdLoc != "eastus2" {
		t.Fatalf("expected RG azr-bnk-rg in eastus2, got rg=%q loc=%q", rgMock.createdRG, rgMock.createdLoc)
	}
	if nsgMock.createdNSG != "azr-bnk-nsg" || nsgMock.ruleCount != 5 {
		t.Fatalf("expected NSG azr-bnk-nsg with 5 rules, got nsg=%q rules=%d", nsgMock.createdNSG, nsgMock.ruleCount)
	}
	if vnetMock.createdVNet != "azr-bnk-vnet" || vnetMock.subnetCount != 4 {
		t.Fatalf("expected VNet azr-bnk-vnet with 4 subnets, got vnet=%q subnets=%d", vnetMock.createdVNet, vnetMock.subnetCount)
	}
	if len(pipMock.createdPIPs) != 2 {
		t.Fatalf("expected 2 public IPs (NAT + Ingress), got %v", pipMock.createdPIPs)
	}
	if natMock.createdNAT != "azr-bnk-nat" {
		t.Fatalf("expected NAT gateway azr-bnk-nat, got %q", natMock.createdNAT)
	}
	if aksMock.createdCluster != "azr-bnk" {
		t.Fatalf("expected AKS cluster azr-bnk, got %q", aksMock.createdCluster)
	}
	if poolMock.createdPool != "tmm" {
		t.Fatalf("expected TMM pool tmm, got %q", poolMock.createdPool)
	}
	if stLive.Get("AZURE_TMM_NODE_NAME") != "aks-tmm-vmss000000" {
		t.Fatalf("expected AZURE_TMM_NODE_NAME=aks-tmm-vmss000000, got %q", stLive.Get("AZURE_TMM_NODE_NAME"))
	}
	if stLive.Get("TMM_EXT_PCI") != "4567:00:02.0" || stLive.Get("TMM_INT_PCI") != "4567:00:03.0" {
		t.Fatalf("unexpected PCI addresses: ext=%q int=%q", stLive.Get("TMM_EXT_PCI"), stLive.Get("TMM_INT_PCI"))
	}
	if string(uLive.Kubeconfig) != "apiVersion: v1\nkind: Config\n" {
		t.Fatalf("expected UnderlayContext.Kubeconfig to be populated from written kubeconfig, got %q", string(uLive.Kubeconfig))
	}
	if _, err := os.Stat(filepath.Join(stLive.Dir(), "kubeconfig")); err != nil {
		t.Fatalf("expected kubeconfig file to exist on disk: %v", err)
	}

	// 3. DestroyUnderlay deletes exact created resource names (including pip-ingress-azr-bnk)
	if err := pLive.DestroyUnderlay(ctx, cl, stLive, false); err != nil {
		t.Fatalf("DestroyUnderlay: %v", err)
	}
	if poolMock.deletedPool != "tmm" || aksMock.deletedCluster != "azr-bnk" {
		t.Fatalf("expected pool tmm and cluster azr-bnk deleted, got pool=%q cluster=%q", poolMock.deletedPool, aksMock.deletedCluster)
	}
	if natMock.deletedNAT != "azr-bnk-nat" {
		t.Fatalf("expected NAT azr-bnk-nat deleted, got %q", natMock.deletedNAT)
	}
	foundIngressPIP := false
	foundNatPIP := false
	for _, pip := range pipMock.deletedPIPs {
		if pip == "pip-ingress-azr-bnk" {
			foundIngressPIP = true
		}
		if pip == "azr-bnk-nat-pip" {
			foundNatPIP = true
		}
	}
	if !foundIngressPIP || !foundNatPIP {
		t.Fatalf("expected both pip-ingress-azr-bnk and azr-bnk-nat-pip to be deleted, got %v", pipMock.deletedPIPs)
	}
	if vnetMock.deletedVNet != "azr-bnk-vnet" || nsgMock.deletedNSG != "azr-bnk-nsg" {
		t.Fatalf("expected VNet azr-bnk-vnet and NSG azr-bnk-nsg deleted, got vnet=%q nsg=%q", vnetMock.deletedVNet, nsgMock.deletedNSG)
	}

	// 4. DiscoverUnderlay fetches and writes kubeconfig when missing in state
	stDisc, err := state.Load(t.TempDir())
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	uDisc, err := pLive.DiscoverUnderlay(ctx, cl, stDisc)
	if err != nil {
		t.Fatalf("DiscoverUnderlay: %v", err)
	}
	if stDisc.Get("KUBECONFIG") == "" {
		t.Fatalf("expected KUBECONFIG to be set in state after DiscoverUnderlay")
	}
	if uDisc.KubeconfigPath == "" || string(uDisc.Kubeconfig) != "apiVersion: v1\nkind: Config\n" {
		t.Fatalf("expected UnderlayContext.Kubeconfig populated in DiscoverUnderlay, got path=%q kc=%q", uDisc.KubeconfigPath, string(uDisc.Kubeconfig))
	}
}
