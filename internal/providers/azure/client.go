package azure

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"k8s.io/client-go/kubernetes"
)

// ResourceGroupsAPI abstracts resource group operations.
type ResourceGroupsAPI interface {
	Get(ctx context.Context, resourceGroupName string, options *armresources.ResourceGroupsClientGetOptions) (armresources.ResourceGroupsClientGetResponse, error)
	CreateOrUpdate(ctx context.Context, resourceGroupName string, parameters armresources.ResourceGroup, options *armresources.ResourceGroupsClientCreateOrUpdateOptions) (armresources.ResourceGroupsClientCreateOrUpdateResponse, error)
	BeginDelete(ctx context.Context, resourceGroupName string, options *armresources.ResourceGroupsClientBeginDeleteOptions) (*runtime.Poller[armresources.ResourceGroupsClientDeleteResponse], error)
}

// VirtualNetworksAPI abstracts VNet operations.
type VirtualNetworksAPI interface {
	Get(ctx context.Context, resourceGroupName string, virtualNetworkName string, options *armnetwork.VirtualNetworksClientGetOptions) (armnetwork.VirtualNetworksClientGetResponse, error)
	BeginCreateOrUpdate(ctx context.Context, resourceGroupName string, virtualNetworkName string, parameters armnetwork.VirtualNetwork, options *armnetwork.VirtualNetworksClientBeginCreateOrUpdateOptions) (*runtime.Poller[armnetwork.VirtualNetworksClientCreateOrUpdateResponse], error)
	BeginDelete(ctx context.Context, resourceGroupName string, virtualNetworkName string, options *armnetwork.VirtualNetworksClientBeginDeleteOptions) (*runtime.Poller[armnetwork.VirtualNetworksClientDeleteResponse], error)
}

// SubnetsAPI abstracts subnet operations.
type SubnetsAPI interface {
	Get(ctx context.Context, resourceGroupName string, virtualNetworkName string, subnetName string, options *armnetwork.SubnetsClientGetOptions) (armnetwork.SubnetsClientGetResponse, error)
	BeginCreateOrUpdate(ctx context.Context, resourceGroupName string, virtualNetworkName string, subnetName string, subnetParameters armnetwork.Subnet, options *armnetwork.SubnetsClientBeginCreateOrUpdateOptions) (*runtime.Poller[armnetwork.SubnetsClientCreateOrUpdateResponse], error)
	BeginDelete(ctx context.Context, resourceGroupName string, virtualNetworkName string, subnetName string, options *armnetwork.SubnetsClientBeginDeleteOptions) (*runtime.Poller[armnetwork.SubnetsClientDeleteResponse], error)
}

// PublicIPAddressesAPI abstracts Public IP operations.
type PublicIPAddressesAPI interface {
	Get(ctx context.Context, resourceGroupName string, publicIPAddressName string, options *armnetwork.PublicIPAddressesClientGetOptions) (armnetwork.PublicIPAddressesClientGetResponse, error)
	BeginCreateOrUpdate(ctx context.Context, resourceGroupName string, publicIPAddressName string, parameters armnetwork.PublicIPAddress, options *armnetwork.PublicIPAddressesClientBeginCreateOrUpdateOptions) (*runtime.Poller[armnetwork.PublicIPAddressesClientCreateOrUpdateResponse], error)
	BeginDelete(ctx context.Context, resourceGroupName string, publicIPAddressName string, options *armnetwork.PublicIPAddressesClientBeginDeleteOptions) (*runtime.Poller[armnetwork.PublicIPAddressesClientDeleteResponse], error)
}

// NatGatewaysAPI abstracts NAT Gateway operations.
type NatGatewaysAPI interface {
	Get(ctx context.Context, resourceGroupName string, natGatewayName string, options *armnetwork.NatGatewaysClientGetOptions) (armnetwork.NatGatewaysClientGetResponse, error)
	BeginCreateOrUpdate(ctx context.Context, resourceGroupName string, natGatewayName string, parameters armnetwork.NatGateway, options *armnetwork.NatGatewaysClientBeginCreateOrUpdateOptions) (*runtime.Poller[armnetwork.NatGatewaysClientCreateOrUpdateResponse], error)
	BeginDelete(ctx context.Context, resourceGroupName string, natGatewayName string, options *armnetwork.NatGatewaysClientBeginDeleteOptions) (*runtime.Poller[armnetwork.NatGatewaysClientDeleteResponse], error)
}

// SecurityGroupsAPI abstracts NSG operations.
type SecurityGroupsAPI interface {
	Get(ctx context.Context, resourceGroupName string, networkSecurityGroupName string, options *armnetwork.SecurityGroupsClientGetOptions) (armnetwork.SecurityGroupsClientGetResponse, error)
	BeginCreateOrUpdate(ctx context.Context, resourceGroupName string, networkSecurityGroupName string, parameters armnetwork.SecurityGroup, options *armnetwork.SecurityGroupsClientBeginCreateOrUpdateOptions) (*runtime.Poller[armnetwork.SecurityGroupsClientCreateOrUpdateResponse], error)
	BeginDelete(ctx context.Context, resourceGroupName string, networkSecurityGroupName string, options *armnetwork.SecurityGroupsClientBeginDeleteOptions) (*runtime.Poller[armnetwork.SecurityGroupsClientDeleteResponse], error)
}

// ManagedClustersAPI abstracts AKS cluster operations.
type ManagedClustersAPI interface {
	Get(ctx context.Context, resourceGroupName string, resourceName string, options *armcontainerservice.ManagedClustersClientGetOptions) (armcontainerservice.ManagedClustersClientGetResponse, error)
	BeginCreateOrUpdate(ctx context.Context, resourceGroupName string, resourceName string, parameters armcontainerservice.ManagedCluster, options *armcontainerservice.ManagedClustersClientBeginCreateOrUpdateOptions) (*runtime.Poller[armcontainerservice.ManagedClustersClientCreateOrUpdateResponse], error)
	BeginDelete(ctx context.Context, resourceGroupName string, resourceName string, options *armcontainerservice.ManagedClustersClientBeginDeleteOptions) (*runtime.Poller[armcontainerservice.ManagedClustersClientDeleteResponse], error)
	ListClusterAdminCredentials(ctx context.Context, resourceGroupName string, resourceName string, options *armcontainerservice.ManagedClustersClientListClusterAdminCredentialsOptions) (armcontainerservice.ManagedClustersClientListClusterAdminCredentialsResponse, error)
}

// AgentPoolsAPI abstracts AKS node pool operations.
type AgentPoolsAPI interface {
	Get(ctx context.Context, resourceGroupName string, resourceName string, agentPoolName string, options *armcontainerservice.AgentPoolsClientGetOptions) (armcontainerservice.AgentPoolsClientGetResponse, error)
	BeginCreateOrUpdate(ctx context.Context, resourceGroupName string, resourceName string, agentPoolName string, parameters armcontainerservice.AgentPool, options *armcontainerservice.AgentPoolsClientBeginCreateOrUpdateOptions) (*runtime.Poller[armcontainerservice.AgentPoolsClientCreateOrUpdateResponse], error)
	BeginDelete(ctx context.Context, resourceGroupName string, resourceName string, agentPoolName string, options *armcontainerservice.AgentPoolsClientBeginDeleteOptions) (*runtime.Poller[armcontainerservice.AgentPoolsClientDeleteResponse], error)
}

// ResourcesAPI abstracts generic ARM resource deletion (e.g. Compute VM, Disk, NIC).
type ResourcesAPI interface {
	BeginDelete(ctx context.Context, resourceGroupName string, resourceProviderNamespace string, parentResourcePath string, resourceType string, resourceName string, apiVersion string, options *armresources.ClientBeginDeleteOptions) (*runtime.Poller[armresources.ClientDeleteResponse], error)
}

// Clients bundles all Azure service APIs.
type Clients struct {
	SubscriptionID    string
	ResourceGroups    ResourceGroupsAPI
	Resources         ResourcesAPI
	VirtualNetworks   VirtualNetworksAPI
	Subnets           SubnetsAPI
	PublicIPAddresses PublicIPAddressesAPI
	NatGateways       NatGatewaysAPI
	SecurityGroups    SecurityGroupsAPI
	ManagedClusters   ManagedClustersAPI
	AgentPools        AgentPoolsAPI
	K8s               kubernetes.Interface
}

// NewClientsWithDefault initializes live ARM clients using DefaultAzureCredential.
func NewClientsWithDefault(subscriptionID string) (*Clients, error) {
	if ShouldSkipAuth() {
		return &Clients{SubscriptionID: subscriptionID}, nil
	}
	cred, err := NewCredentialForSubscription(subscriptionID)
	if err != nil {
		return nil, err
	}
	return NewClients(subscriptionID, cred)
}

// NewClients initializes live ARM clients using the provided credential.
func NewClients(subscriptionID string, cred azcore.TokenCredential) (*Clients, error) {
	if ShouldSkipAuth() || cred == nil {
		return &Clients{SubscriptionID: subscriptionID}, nil
	}

	opt := &arm.ClientOptions{}

	rgClient, err := armresources.NewResourceGroupsClient(subscriptionID, cred, opt)
	if err != nil {
		return nil, fmt.Errorf("azure client: ResourceGroupsClient: %w", err)
	}

	resClient, err := armresources.NewClient(subscriptionID, cred, opt)
	if err != nil {
		return nil, fmt.Errorf("azure client: ResourcesClient: %w", err)
	}

	vnetClient, err := armnetwork.NewVirtualNetworksClient(subscriptionID, cred, opt)
	if err != nil {
		return nil, fmt.Errorf("azure client: VirtualNetworksClient: %w", err)
	}

	subnetsClient, err := armnetwork.NewSubnetsClient(subscriptionID, cred, opt)
	if err != nil {
		return nil, fmt.Errorf("azure client: SubnetsClient: %w", err)
	}

	pipClient, err := armnetwork.NewPublicIPAddressesClient(subscriptionID, cred, opt)
	if err != nil {
		return nil, fmt.Errorf("azure client: PublicIPAddressesClient: %w", err)
	}

	natClient, err := armnetwork.NewNatGatewaysClient(subscriptionID, cred, opt)
	if err != nil {
		return nil, fmt.Errorf("azure client: NatGatewaysClient: %w", err)
	}

	nsgClient, err := armnetwork.NewSecurityGroupsClient(subscriptionID, cred, opt)
	if err != nil {
		return nil, fmt.Errorf("azure client: SecurityGroupsClient: %w", err)
	}

	aksClient, err := armcontainerservice.NewManagedClustersClient(subscriptionID, cred, opt)
	if err != nil {
		return nil, fmt.Errorf("azure client: ManagedClustersClient: %w", err)
	}

	poolClient, err := armcontainerservice.NewAgentPoolsClient(subscriptionID, cred, opt)
	if err != nil {
		return nil, fmt.Errorf("azure client: AgentPoolsClient: %w", err)
	}

	return &Clients{
		SubscriptionID:    subscriptionID,
		ResourceGroups:    &resourceGroupsAdapter{rgClient},
		Resources:         &resourcesAdapter{resClient},
		VirtualNetworks:   &virtualNetworksAdapter{vnetClient},
		Subnets:           &subnetsAdapter{subnetsClient},
		PublicIPAddresses: &publicIPAddressesAdapter{pipClient},
		NatGateways:       &natGatewaysAdapter{natClient},
		SecurityGroups:    &securityGroupsAdapter{nsgClient},
		ManagedClusters:   &managedClustersAdapter{aksClient},
		AgentPools:        &agentPoolsAdapter{poolClient},
	}, nil
}

// Concrete adapters wrapping ARM SDK clients

type resourcesAdapter struct {
	c *armresources.Client
}

func (a *resourcesAdapter) BeginDelete(ctx context.Context, rg, providerNamespace, parentPath, resourceType, name, apiVersion string, opt *armresources.ClientBeginDeleteOptions) (*runtime.Poller[armresources.ClientDeleteResponse], error) {
	return a.c.BeginDelete(ctx, rg, providerNamespace, parentPath, resourceType, name, apiVersion, opt)
}

type resourceGroupsAdapter struct {
	c *armresources.ResourceGroupsClient
}

func (a *resourceGroupsAdapter) Get(ctx context.Context, rg string, opt *armresources.ResourceGroupsClientGetOptions) (armresources.ResourceGroupsClientGetResponse, error) {
	return a.c.Get(ctx, rg, opt)
}
func (a *resourceGroupsAdapter) CreateOrUpdate(ctx context.Context, rg string, p armresources.ResourceGroup, opt *armresources.ResourceGroupsClientCreateOrUpdateOptions) (armresources.ResourceGroupsClientCreateOrUpdateResponse, error) {
	return a.c.CreateOrUpdate(ctx, rg, p, opt)
}
func (a *resourceGroupsAdapter) BeginDelete(ctx context.Context, rg string, opt *armresources.ResourceGroupsClientBeginDeleteOptions) (*runtime.Poller[armresources.ResourceGroupsClientDeleteResponse], error) {
	return a.c.BeginDelete(ctx, rg, opt)
}

type virtualNetworksAdapter struct {
	c *armnetwork.VirtualNetworksClient
}

func (a *virtualNetworksAdapter) Get(ctx context.Context, rg, name string, opt *armnetwork.VirtualNetworksClientGetOptions) (armnetwork.VirtualNetworksClientGetResponse, error) {
	return a.c.Get(ctx, rg, name, opt)
}
func (a *virtualNetworksAdapter) BeginCreateOrUpdate(ctx context.Context, rg, name string, p armnetwork.VirtualNetwork, opt *armnetwork.VirtualNetworksClientBeginCreateOrUpdateOptions) (*runtime.Poller[armnetwork.VirtualNetworksClientCreateOrUpdateResponse], error) {
	return a.c.BeginCreateOrUpdate(ctx, rg, name, p, opt)
}
func (a *virtualNetworksAdapter) BeginDelete(ctx context.Context, rg, name string, opt *armnetwork.VirtualNetworksClientBeginDeleteOptions) (*runtime.Poller[armnetwork.VirtualNetworksClientDeleteResponse], error) {
	return a.c.BeginDelete(ctx, rg, name, opt)
}

type subnetsAdapter struct{ c *armnetwork.SubnetsClient }

func (a *subnetsAdapter) Get(ctx context.Context, rg, vnet, name string, opt *armnetwork.SubnetsClientGetOptions) (armnetwork.SubnetsClientGetResponse, error) {
	return a.c.Get(ctx, rg, vnet, name, opt)
}
func (a *subnetsAdapter) BeginCreateOrUpdate(ctx context.Context, rg, vnet, name string, p armnetwork.Subnet, opt *armnetwork.SubnetsClientBeginCreateOrUpdateOptions) (*runtime.Poller[armnetwork.SubnetsClientCreateOrUpdateResponse], error) {
	return a.c.BeginCreateOrUpdate(ctx, rg, vnet, name, p, opt)
}
func (a *subnetsAdapter) BeginDelete(ctx context.Context, rg, vnet, name string, opt *armnetwork.SubnetsClientBeginDeleteOptions) (*runtime.Poller[armnetwork.SubnetsClientDeleteResponse], error) {
	return a.c.BeginDelete(ctx, rg, vnet, name, opt)
}

type publicIPAddressesAdapter struct {
	c *armnetwork.PublicIPAddressesClient
}

func (a *publicIPAddressesAdapter) Get(ctx context.Context, rg, name string, opt *armnetwork.PublicIPAddressesClientGetOptions) (armnetwork.PublicIPAddressesClientGetResponse, error) {
	return a.c.Get(ctx, rg, name, opt)
}
func (a *publicIPAddressesAdapter) BeginCreateOrUpdate(ctx context.Context, rg, name string, p armnetwork.PublicIPAddress, opt *armnetwork.PublicIPAddressesClientBeginCreateOrUpdateOptions) (*runtime.Poller[armnetwork.PublicIPAddressesClientCreateOrUpdateResponse], error) {
	return a.c.BeginCreateOrUpdate(ctx, rg, name, p, opt)
}
func (a *publicIPAddressesAdapter) BeginDelete(ctx context.Context, rg, name string, opt *armnetwork.PublicIPAddressesClientBeginDeleteOptions) (*runtime.Poller[armnetwork.PublicIPAddressesClientDeleteResponse], error) {
	return a.c.BeginDelete(ctx, rg, name, opt)
}

type natGatewaysAdapter struct{ c *armnetwork.NatGatewaysClient }

func (a *natGatewaysAdapter) Get(ctx context.Context, rg, name string, opt *armnetwork.NatGatewaysClientGetOptions) (armnetwork.NatGatewaysClientGetResponse, error) {
	return a.c.Get(ctx, rg, name, opt)
}
func (a *natGatewaysAdapter) BeginCreateOrUpdate(ctx context.Context, rg, name string, p armnetwork.NatGateway, opt *armnetwork.NatGatewaysClientBeginCreateOrUpdateOptions) (*runtime.Poller[armnetwork.NatGatewaysClientCreateOrUpdateResponse], error) {
	return a.c.BeginCreateOrUpdate(ctx, rg, name, p, opt)
}
func (a *natGatewaysAdapter) BeginDelete(ctx context.Context, rg, name string, opt *armnetwork.NatGatewaysClientBeginDeleteOptions) (*runtime.Poller[armnetwork.NatGatewaysClientDeleteResponse], error) {
	return a.c.BeginDelete(ctx, rg, name, opt)
}

type securityGroupsAdapter struct {
	c *armnetwork.SecurityGroupsClient
}

func (a *securityGroupsAdapter) Get(ctx context.Context, rg, name string, opt *armnetwork.SecurityGroupsClientGetOptions) (armnetwork.SecurityGroupsClientGetResponse, error) {
	return a.c.Get(ctx, rg, name, opt)
}
func (a *securityGroupsAdapter) BeginCreateOrUpdate(ctx context.Context, rg, name string, p armnetwork.SecurityGroup, opt *armnetwork.SecurityGroupsClientBeginCreateOrUpdateOptions) (*runtime.Poller[armnetwork.SecurityGroupsClientCreateOrUpdateResponse], error) {
	return a.c.BeginCreateOrUpdate(ctx, rg, name, p, opt)
}
func (a *securityGroupsAdapter) BeginDelete(ctx context.Context, rg, name string, opt *armnetwork.SecurityGroupsClientBeginDeleteOptions) (*runtime.Poller[armnetwork.SecurityGroupsClientDeleteResponse], error) {
	return a.c.BeginDelete(ctx, rg, name, opt)
}

type managedClustersAdapter struct {
	c *armcontainerservice.ManagedClustersClient
}

func (a *managedClustersAdapter) Get(ctx context.Context, rg, name string, opt *armcontainerservice.ManagedClustersClientGetOptions) (armcontainerservice.ManagedClustersClientGetResponse, error) {
	return a.c.Get(ctx, rg, name, opt)
}
func (a *managedClustersAdapter) BeginCreateOrUpdate(ctx context.Context, rg, name string, p armcontainerservice.ManagedCluster, opt *armcontainerservice.ManagedClustersClientBeginCreateOrUpdateOptions) (*runtime.Poller[armcontainerservice.ManagedClustersClientCreateOrUpdateResponse], error) {
	return a.c.BeginCreateOrUpdate(ctx, rg, name, p, opt)
}
func (a *managedClustersAdapter) BeginDelete(ctx context.Context, rg, name string, opt *armcontainerservice.ManagedClustersClientBeginDeleteOptions) (*runtime.Poller[armcontainerservice.ManagedClustersClientDeleteResponse], error) {
	return a.c.BeginDelete(ctx, rg, name, opt)
}
func (a *managedClustersAdapter) ListClusterAdminCredentials(ctx context.Context, rg, name string, opt *armcontainerservice.ManagedClustersClientListClusterAdminCredentialsOptions) (armcontainerservice.ManagedClustersClientListClusterAdminCredentialsResponse, error) {
	return a.c.ListClusterAdminCredentials(ctx, rg, name, opt)
}

type agentPoolsAdapter struct {
	c *armcontainerservice.AgentPoolsClient
}

func (a *agentPoolsAdapter) Get(ctx context.Context, rg, clusterName, poolName string, opt *armcontainerservice.AgentPoolsClientGetOptions) (armcontainerservice.AgentPoolsClientGetResponse, error) {
	return a.c.Get(ctx, rg, clusterName, poolName, opt)
}
func (a *agentPoolsAdapter) BeginCreateOrUpdate(ctx context.Context, rg, clusterName, poolName string, p armcontainerservice.AgentPool, opt *armcontainerservice.AgentPoolsClientBeginCreateOrUpdateOptions) (*runtime.Poller[armcontainerservice.AgentPoolsClientCreateOrUpdateResponse], error) {
	return a.c.BeginCreateOrUpdate(ctx, rg, clusterName, poolName, p, opt)
}
func (a *agentPoolsAdapter) BeginDelete(ctx context.Context, rg, clusterName, poolName string, opt *armcontainerservice.AgentPoolsClientBeginDeleteOptions) (*runtime.Poller[armcontainerservice.AgentPoolsClientDeleteResponse], error) {
	return a.c.BeginDelete(ctx, rg, clusterName, poolName, opt)
}
