package azure

import (
	"context"
	"fmt"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v6"
	"github.com/JLCode-tech/bnkctl/internal/providers/azure/tags"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

// Phase21AKSCluster provisions the AKS control plane and system node pool.
func Phase21AKSCluster(ctx context.Context, cl *intent.Cluster, st *state.State, clients *Clients, dryRun bool) error {
	ensureAzureDefaults(cl)
	rgName := cl.Derived.ResourceGroupName
	clusterName := cl.Derived.AKSClusterName
	location := cl.Metadata.Location
	k8sVersion := cl.ClusterSpec.KubernetesVersion

	fmt.Fprintf(os.Stderr, "[phase 21] aks-cluster: %s (k8s %s in %s)\n", clusterName, k8sVersion, location)

	if dryRun {
		if st != nil {
			st.Set("AZURE_AKS_CLUSTER_NAME", clusterName)
		}
		fmt.Fprintf(os.Stderr, "[phase 21] [dry-run] would create AKS cluster %s\n", clusterName)
		return nil
	}

	if clients == nil || clients.ManagedClusters == nil {
		return nil
	}

	aksTags := tags.ForCluster(cl.Metadata.Name, "aks", cl.Tags)
	dnsPrefix := cl.Metadata.Name

	systemPoolName := cl.ClusterSpec.SystemNodePool.Name
	if systemPoolName == "" {
		systemPoolName = "system"
	}
	// #nosec G115 -- node count bounded by cluster schema
	systemCount := int32(cl.ClusterSpec.SystemNodePool.Count)
	if systemCount <= 0 {
		systemCount = 1
	}
	systemVMSize := cl.ClusterSpec.SystemNodePool.VMSize
	if systemVMSize == "" {
		systemVMSize = "Standard_D4s_v5"
	}
	systemMode := armcontainerservice.AgentPoolModeSystem
	osType := armcontainerservice.OSTypeLinux

	var systemSubnetID string
	if st != nil {
		systemSubnetID = st.Get("AZURE_SYSTEM_SUBNET_ID")
	}

	netPlugin := armcontainerservice.NetworkPluginAzure
	netPluginMode := armcontainerservice.NetworkPluginModeOverlay
	serviceCIDR := "10.240.0.0/16"
	dnsServiceIP := "10.240.0.10"
	podCIDR := "10.244.0.0/16"

	managedIdentity := armcontainerservice.ResourceIdentityTypeSystemAssigned

	managedCluster := armcontainerservice.ManagedCluster{
		Location: &location,
		Tags:     aksTags,
		Identity: &armcontainerservice.ManagedClusterIdentity{
			Type: &managedIdentity,
		},
		Properties: &armcontainerservice.ManagedClusterProperties{
			KubernetesVersion: &k8sVersion,
			DNSPrefix:         &dnsPrefix,
			AgentPoolProfiles: []*armcontainerservice.ManagedClusterAgentPoolProfile{
				{
					Name:         &systemPoolName,
					Count:        &systemCount,
					VMSize:       &systemVMSize,
					Mode:         &systemMode,
					OSType:       &osType,
					VnetSubnetID: &systemSubnetID,
				},
			},
			NetworkProfile: &armcontainerservice.NetworkProfile{
				NetworkPlugin:     &netPlugin,
				NetworkPluginMode: &netPluginMode,
				ServiceCidr:       &serviceCIDR,
				DNSServiceIP:      &dnsServiceIP,
				PodCidr:           &podCIDR,
			},
		},
	}

	poller, err := clients.ManagedClusters.BeginCreateOrUpdate(ctx, rgName, clusterName, managedCluster, nil)
	if err != nil {
		return fmt.Errorf("phase21: BeginCreateOrUpdate AKS cluster %s: %w", clusterName, err)
	}

	var resp armcontainerservice.ManagedClustersClientCreateOrUpdateResponse
	if poller != nil {
		resp, err = poller.PollUntilDone(ctx, nil)
		if err != nil {
			return fmt.Errorf("phase21: waiting for AKS cluster %s: %w", clusterName, err)
		}
	}

	if st != nil {
		if resp.ID != nil {
			st.Set("AZURE_AKS_CLUSTER_ID", *resp.ID)
			st.Set("AKS_CLUSTER_ID", *resp.ID)
		}
		st.Set("AZURE_AKS_CLUSTER_NAME", clusterName)
		st.Set("SERVICE_CIDR", serviceCIDR)
		st.Set("POD_CIDR", podCIDR)

		if resp.Properties != nil && resp.Properties.NodeResourceGroup != nil {
			st.Set("AZURE_AKS_NODE_RESOURCE_GROUP", *resp.Properties.NodeResourceGroup)
			fmt.Fprintf(os.Stderr, "[phase 21] AKS Node Resource Group: %s\n", *resp.Properties.NodeResourceGroup)
		}

		if err := st.Save(); err != nil {
			return fmt.Errorf("phase21: saving state: %w", err)
		}
	}

	fmt.Fprintf(os.Stderr, "[phase 21] AKS cluster %s OK\n", clusterName)
	return nil
}
