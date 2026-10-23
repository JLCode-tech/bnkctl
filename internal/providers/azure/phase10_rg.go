package azure

import (
	"context"
	"fmt"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/JLCode-tech/bnkctl/internal/providers/azure/tags"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

// Phase10ResourceGroup ensures the Azure Resource Group exists with proper tagging.
func Phase10ResourceGroup(ctx context.Context, cl *intent.Cluster, st *state.State, clients *Clients, dryRun bool) error {
	ensureAzureDefaults(cl)
	rgName := cl.Derived.ResourceGroupName
	location := cl.Metadata.Location

	fmt.Fprintf(os.Stderr, "[phase 10] resource-group: %s (location: %s)\n", rgName, location)

	if dryRun {
		if st != nil {
			st.Set("AZURE_RESOURCE_GROUP", rgName)
		}
		fmt.Fprintf(os.Stderr, "[phase 10] [dry-run] would create/adopt resource group %s\n", rgName)
		return nil
	}

	if clients == nil || clients.ResourceGroups == nil {
		return nil
	}

	// Try adopting existing resource group first to support scoped permissions
	if existing, getErr := clients.ResourceGroups.Get(ctx, rgName, nil); getErr == nil {
		if st != nil {
			if existing.ID != nil {
				st.Set("AZURE_RESOURCE_GROUP_ID", *existing.ID)
				st.Set("RG_ID", *existing.ID)
			}
			st.Set("AZURE_RESOURCE_GROUP", rgName)
			if err := st.Save(); err != nil {
				return fmt.Errorf("phase10: saving state: %w", err)
			}
		}
		fmt.Fprintf(os.Stderr, "[phase 10] adopting existing resource group %s OK\n", rgName)
		return nil
	}

	rgTags := tags.ForCluster(cl.Metadata.Name, "rg", cl.Tags)

	resp, err := clients.ResourceGroups.CreateOrUpdate(ctx, rgName, armresources.ResourceGroup{
		Location: &location,
		Tags:     rgTags,
	}, nil)
	if err != nil {
		return fmt.Errorf("phase10: CreateOrUpdate resource group %s: %w", rgName, err)
	}

	if st != nil {
		if resp.ID != nil {
			st.Set("AZURE_RESOURCE_GROUP_ID", *resp.ID)
			st.Set("RG_ID", *resp.ID)
		}
		st.Set("AZURE_RESOURCE_GROUP", rgName)
		if err := st.Save(); err != nil {
			return fmt.Errorf("phase10: saving state: %w", err)
		}
	}

	fmt.Fprintf(os.Stderr, "[phase 10] resource group %s OK\n", rgName)
	return nil
}
