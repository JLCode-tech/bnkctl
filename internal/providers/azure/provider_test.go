package azure

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/JLCode-tech/bnkctl/pkg/bnkconst"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/provider"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestAzureProviderUnderlayAndHooks(t *testing.T) {
	ctx := context.Background()
	p := NewProvider()
	if p.Name() != intent.ProviderAzure {
		t.Fatalf("expected name %s, got %s", intent.ProviderAzure, p.Name())
	}

	cl := &intent.Cluster{
		APIVersion: intent.APIVersion,
		Kind:       intent.Kind,
		Cloud:      &intent.CloudSpec{Provider: intent.ProviderAzure},
		Metadata:   intent.Metadata{Name: "aks-test", Region: "eastus2", ResourceGroup: "rg-test"},
		Pattern:    intent.PatternDualInterface,
		Network: intent.Network{
			VPCCidr: "10.60.0.0/16",
			DataPath: &intent.DataPathSpec{
				External: intent.SubnetSpec{CIDR: "10.60.100.0/24"},
				Internal: intent.SubnetSpec{CIDR: "10.60.101.0/24"},
			},
		},
	}

	st, err := state.Load(filepath.Join(t.TempDir(), "state.env"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Set("AZURE_CLIENT_ID", "00000000-1111-2222-3333-444444444444")

	u, err := p.ProvisionUnderlay(ctx, cl, st, false)
	if err != nil {
		t.Fatalf("ProvisionUnderlay: %v", err)
	}
	if u.DataplaneDriver != provider.DriverSocket {
		t.Fatalf("expected DriverSocket, got %s", u.DataplaneDriver)
	}
	if ext := u.ExternalNetwork(); ext == nil || ext.TMMInterface != "0.2" {
		t.Fatalf("unexpected external network: %+v", ext)
	}
	if intl := u.InternalNetwork(); intl == nil || intl.TMMInterface != "0.3" {
		t.Fatalf("unexpected internal network: %+v", intl)
	}

	cs := k8sfake.NewClientset(
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "aks-tmm-0",
				Labels: map[string]string{"topology.kubernetes.io/zone": "eastus2-1"},
			},
			Spec: corev1.NodeSpec{ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/aks-tmm/virtualMachines/0"},
		},
		&corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      bnkconst.CNEControllerServiceAccount,
				Namespace: bnkconst.InstanceNamespace,
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "f5-tmm-0",
				Namespace: bnkconst.InstanceNamespace,
				Labels:    map[string]string{"app": "f5-tmm"},
			},
		},
	)

	if err := u.DiscoverDataplaneResources(ctx, cs, nil); err != nil {
		t.Fatalf("DiscoverDataplaneResources: %v", err)
	}
	if len(u.DataplaneNodes) != 1 || len(u.DataplaneNodes[0].NICs) != 2 {
		t.Fatalf("unexpected DataplaneNodes: %+v", u.DataplaneNodes)
	}
	if u.DataplaneNodes[0].NICs[0].TMMInterface != "0.2" || u.DataplaneNodes[0].NICs[1].TMMInterface != "0.3" {
		t.Fatalf("unexpected NICs: %+v", u.DataplaneNodes[0].NICs)
	}

	if err := u.ReconcileControllerIdentity(ctx, cs, nil); err != nil {
		t.Fatalf("ReconcileControllerIdentity: %v", err)
	}
	sa, err := cs.CoreV1().ServiceAccounts(bnkconst.InstanceNamespace).Get(ctx, bnkconst.CNEControllerServiceAccount, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get ServiceAccount: %v", err)
	}
	if sa.Annotations["azure.workload.identity/client-id"] != "00000000-1111-2222-3333-444444444444" {
		t.Fatalf("unexpected SA annotations: %+v", sa.Annotations)
	}
	if sa.Annotations["azure.workload.identity/use"] != "true" || sa.Labels["azure.workload.identity/use"] != "true" {
		t.Fatalf("expected azure.workload.identity/use=true on SA annotations and labels, got annotations=%+v labels=%+v", sa.Annotations, sa.Labels)
	}

	if err := u.PostActivationHook(ctx, cs, nil); err != nil {
		t.Fatalf("PostActivationHook: %v", err)
	}
	pod, err := cs.CoreV1().Pods(bnkconst.InstanceNamespace).Get(ctx, "f5-tmm-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get Pod: %v", err)
	}
	if pod.Labels["f5-traffic"] != "true" {
		t.Fatalf("expected f5-traffic=true label on TMM pod, got %+v", pod.Labels)
	}
}

type mockAgentPools struct {
	AgentPoolsAPI
	delErr error
}

func (m *mockAgentPools) BeginDelete(ctx context.Context, rg, clusterName, poolName string, opt *armcontainerservice.AgentPoolsClientBeginDeleteOptions) (*runtime.Poller[armcontainerservice.AgentPoolsClientDeleteResponse], error) {
	return nil, m.delErr
}

type mockResourceGroups struct {
	ResourceGroupsAPI
	delCalled bool
	delErr    error
}

func (m *mockResourceGroups) BeginDelete(ctx context.Context, rg string, opt *armresources.ResourceGroupsClientBeginDeleteOptions) (*runtime.Poller[armresources.ResourceGroupsClientDeleteResponse], error) {
	m.delCalled = true
	return nil, m.delErr
}

func TestIsAzureNotFoundAndDownPhases(t *testing.T) {
	ctx := context.Background()
	notFoundErr := &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: "ResourceNotFound"}
	scopeLockedErr := &azcore.ResponseError{StatusCode: http.StatusConflict, ErrorCode: "ScopeLocked"}

	if !isAzureNotFound(notFoundErr) {
		t.Fatalf("expected 404 ResourceNotFound to be classified as NotFound")
	}
	if isAzureNotFound(scopeLockedErr) {
		t.Fatalf("expected 409 ScopeLocked NOT to be classified as NotFound")
	}

	cl := &intent.Cluster{
		Metadata: intent.Metadata{Name: "aks-test", ResourceGroup: "rg-test"},
	}
	st, err := state.Load(filepath.Join(t.TempDir(), "state.env"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Set("AKS_CLUSTER_ID", "/subscriptions/sub/resourceGroups/rg-test/providers/Microsoft.ContainerService/managedClusters/aks-test")
	st.Set("TMM_POOL_ID", "/subscriptions/sub/resourceGroups/rg-test/providers/Microsoft.ContainerService/managedClusters/aks-test/agentPools/tmm")
	st.Set("RG_ID", "/subscriptions/sub/resourceGroups/rg-test")

	// 409 ScopeLocked must propagate error and preserve state keys
	cLocked := &Clients{
		AgentPools: &mockAgentPools{delErr: scopeLockedErr},
	}
	if err := PhaseDownCluster(ctx, cLocked, cl, st); err == nil {
		t.Fatalf("expected PhaseDownCluster to fail on 409 ScopeLocked")
	}
	if st.Get("AKS_CLUSTER_ID") == "" {
		t.Fatalf("expected AKS_CLUSTER_ID to be preserved when PhaseDownCluster fails")
	}

	// 404 ResourceNotFound must succeed and clear state keys
	cNotFound := &Clients{
		AgentPools: &mockAgentPools{delErr: notFoundErr},
	}
	if err := PhaseDownCluster(ctx, cNotFound, cl, st); err != nil {
		t.Fatalf("expected PhaseDownCluster to treat 404 as idempotent success, got: %v", err)
	}
	if st.Get("AKS_CLUSTER_ID") != "" || st.Get("TMM_POOL_ID") != "" {
		t.Fatalf("expected AKS_CLUSTER_ID and TMM_POOL_ID to be cleared after PhaseDownCluster")
	}

	// PhaseDownResourceGroup with deleteRG=false must be a no-op
	mockRG := &mockResourceGroups{delErr: notFoundErr}
	cRG := &Clients{ResourceGroups: mockRG}
	if err := PhaseDownResourceGroup(ctx, cRG, cl, st, false); err != nil {
		t.Fatalf("PhaseDownResourceGroup(deleteRG=false): %v", err)
	}
	if mockRG.delCalled || st.Get("RG_ID") == "" {
		t.Fatalf("expected Resource Group to be preserved when deleteRG=false")
	}

	// PhaseDownResourceGroup with deleteRG=true must delete RG and clear RG_ID
	if err := PhaseDownResourceGroup(ctx, cRG, cl, st, true); err != nil {
		t.Fatalf("PhaseDownResourceGroup(deleteRG=true): %v", err)
	}
	if !mockRG.delCalled || st.Get("RG_ID") != "" {
		t.Fatalf("expected Resource Group to be deleted and RG_ID cleared when deleteRG=true")
	}
}

type mockManagedClustersCapture struct {
	ManagedClustersAPI
	deletedRG       string
	deletedCluster  string
	deletedClusters []string
}

func (m *mockManagedClustersCapture) BeginDelete(ctx context.Context, rg, clusterName string, opt *armcontainerservice.ManagedClustersClientBeginDeleteOptions) (*runtime.Poller[armcontainerservice.ManagedClustersClientDeleteResponse], error) {
	m.deletedRG = rg
	m.deletedCluster = clusterName
	m.deletedClusters = append(m.deletedClusters, clusterName)
	return nil, &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: "ResourceNotFound"}
}

func TestAzureDownResolvesStateClusterNameAndCleansStateDir(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	st, err := state.Load(filepath.Join(tmpDir, "state.env"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Set("AZURE_RESOURCE_GROUP", "f5-bigip-ve-demo-rg")
	st.Set("AZURE_AKS_CLUSTER_NAME", "aks-poc-dual-interface")
	st.Set("SUBSCRIPTION_ID", "11111111-2222-3333-4444-555555555555")

	cl := &intent.Cluster{
		Metadata: intent.Metadata{
			Name:          "bnk-agentfoundry-demo",
			ResourceGroup: "azrbnkctl-bnk-agentfoundry-demo-rg",
		},
	}

	mc := &mockManagedClustersCapture{}
	p := &Provider{
		Clients: &Clients{
			SubscriptionID:  "11111111-2222-3333-4444-555555555555",
			ManagedClusters: mc,
		},
	}
	if err := p.DestroyUnderlay(ctx, cl, st, false); err != nil {
		t.Fatalf("DestroyUnderlay: %v", err)
	}
	if mc.deletedRG != "f5-bigip-ve-demo-rg" || mc.deletedCluster != "aks-poc-dual-interface" {
		t.Fatalf("expected DestroyUnderlay to target rg=f5-bigip-ve-demo-rg cluster=aks-poc-dual-interface, got rg=%q cluster=%q", mc.deletedRG, mc.deletedCluster)
	}
	if len(st.All()) != 1 || st.Get("UNDERLAY_DESTROYED") != "true" {
		t.Fatalf("expected state.env to contain only UNDERLAY_DESTROYED=true, got %+v", st.All())
	}
}

func TestAzureDownStatelessFallback(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	st, err := state.Load(filepath.Join(tmpDir, "state.env"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	// Simulate state.env after a previous destructive wipe (only UNDERLAY_DESTROYED=true)
	st.Set("UNDERLAY_DESTROYED", "true")

	cl := &intent.Cluster{
		Metadata: intent.Metadata{
			Name:           "poc-dual-interface",
			ResourceGroup:  "f5-bigip-ve-demo-rg",
			SubscriptionID: "11111111-2222-3333-4444-555555555555",
		},
	}

	mc := &mockManagedClustersCapture{}
	vnetMock := &fakeVNetClient{}
	nsgMock := &fakeNSGClient{}
	natMock := &fakeNATClient{}
	pipMock := &fakePIPClient{}

	p := &Provider{
		Clients: &Clients{
			SubscriptionID:    "11111111-2222-3333-4444-555555555555",
			ManagedClusters:   mc,
			VirtualNetworks:   vnetMock,
			SecurityGroups:    nsgMock,
			NatGateways:       natMock,
			PublicIPAddresses: pipMock,
		},
	}
	if err := p.DestroyUnderlay(ctx, cl, st, false); err != nil {
		t.Fatalf("DestroyUnderlay stateless: %v", err)
	}
	if len(mc.deletedClusters) != 2 || mc.deletedClusters[0] != "poc-dual-interface" || mc.deletedClusters[1] != "aks-poc-dual-interface" {
		t.Fatalf("expected stateless DestroyUnderlay to try both poc-dual-interface and aks-poc-dual-interface, got %v", mc.deletedClusters)
	}
	if vnetMock.deletedVNet != "vnet-poc-dual-interface" {
		t.Fatalf("expected last VNet delete attempt to be vnet-poc-dual-interface, got %q", vnetMock.deletedVNet)
	}
	if nsgMock.deletedNSG != "nsg-poc-dual-interface" {
		t.Fatalf("expected last NSG delete attempt to be nsg-poc-dual-interface, got %q", nsgMock.deletedNSG)
	}
	if natMock.deletedNAT != "natgw-poc-dual-interface" {
		t.Fatalf("expected last NAT delete attempt to be natgw-poc-dual-interface, got %q", natMock.deletedNAT)
	}
}
