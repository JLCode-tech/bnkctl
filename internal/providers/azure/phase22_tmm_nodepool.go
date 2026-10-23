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

// Phase22TMMNodePool provisions the dedicated TMM worker node pool with Accelerated Networking.
func Phase22TMMNodePool(ctx context.Context, cl *intent.Cluster, st *state.State, clients *Clients, dryRun bool) error {
	ensureAzureDefaults(cl)
	rgName := cl.Derived.ResourceGroupName
	clusterName := cl.Derived.AKSClusterName
	poolName := cl.ClusterSpec.TMMNodePool.Name
	if poolName == "" {
		poolName = "tmm"
	}
	vmSize := cl.ClusterSpec.TMMNodePool.VMSize
	if vmSize == "" {
		vmSize = "Standard_D8s_v5"
	}
	// #nosec G115 -- node pool count bounded by cluster schema
	count := int32(cl.ClusterSpec.TMMNodePool.Count)
	if count <= 0 {
		count = 1
	}

	fmt.Fprintf(os.Stderr, "[phase 22] tmm-nodepool: %s (size: %s, count: %d)\n", poolName, vmSize, count)

	if dryRun {
		if st != nil {
			st.Set("AZURE_TMM_POOL_NAME", poolName)
		}
		fmt.Fprintf(os.Stderr, "[phase 22] [dry-run] would create TMM node pool %s (%s)\n", poolName, vmSize)
		return nil
	}

	if clients == nil || clients.AgentPools == nil {
		return nil
	}

	poolTags := tags.ForCluster(cl.Metadata.Name, "tmm-pool", cl.Tags)
	var systemSubnetID string
	if st != nil {
		systemSubnetID = st.Get("AZURE_SYSTEM_SUBNET_ID")
	}
	userMode := armcontainerservice.AgentPoolModeUser
	osType := armcontainerservice.OSTypeLinux

	nodeLabels := map[string]*string{
		"app":             ptr("f5-tmm"),
		"f5.com/nodepool": ptr("tmm"),
	}

	agentPool := armcontainerservice.AgentPool{
		Properties: &armcontainerservice.ManagedClusterAgentPoolProfileProperties{
			Count:        &count,
			VMSize:       &vmSize,
			Mode:         &userMode,
			OSType:       &osType,
			VnetSubnetID: &systemSubnetID,
			NodeLabels:   nodeLabels,
			Tags:         poolTags,
		},
	}

	poller, err := clients.AgentPools.BeginCreateOrUpdate(ctx, rgName, clusterName, poolName, agentPool, nil)
	if err != nil {
		return fmt.Errorf("phase22: BeginCreateOrUpdate TMM pool %s: %w", poolName, err)
	}

	var resp armcontainerservice.AgentPoolsClientCreateOrUpdateResponse
	if poller != nil {
		resp, err = poller.PollUntilDone(ctx, nil)
		if err != nil {
			return fmt.Errorf("phase22: waiting for TMM pool %s: %w", poolName, err)
		}
	}

	if st != nil {
		if resp.ID != nil {
			st.Set("AZURE_TMM_POOL_ID", *resp.ID)
			st.Set("TMM_POOL_ID", *resp.ID)
		}
		st.Set("AZURE_TMM_POOL_NAME", poolName)
		if err := st.Save(); err != nil {
			return fmt.Errorf("phase22: saving state: %w", err)
		}
	}

	fmt.Fprintf(os.Stderr, "[phase 22] TMM node pool %s OK\n", poolName)
	return nil
}

func ptr[T any](v T) *T {
	return &v
}
