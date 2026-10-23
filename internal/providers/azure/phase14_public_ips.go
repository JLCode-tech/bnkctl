package azure

import (
	"context"
	"fmt"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/JLCode-tech/bnkctl/internal/providers/azure/tags"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

// Phase14PublicIPs allocates additional static public IPs for BNK external listeners.
func Phase14PublicIPs(ctx context.Context, cl *intent.Cluster, st *state.State, clients *Clients, dryRun bool) error {
	ensureAzureDefaults(cl)
	rgName := cl.Derived.ResourceGroupName
	location := cl.Metadata.Location
	pipName := fmt.Sprintf("pip-ingress-%s", cl.Metadata.Name)

	fmt.Fprintf(os.Stderr, "[phase 14] public-ips: ingress pip %s\n", pipName)

	if dryRun {
		if st != nil {
			st.Set("AZURE_INGRESS_PIP_NAME", pipName)
			st.Set("AZURE_INGRESS_PIP_ADDRESS", "198.51.100.20")
		}
		fmt.Fprintf(os.Stderr, "[phase 14] [dry-run] would allocate ingress Public IP %s\n", pipName)
		return nil
	}

	if clients == nil || clients.PublicIPAddresses == nil {
		return nil
	}

	pipTags := tags.ForCluster(cl.Metadata.Name, "ingress-pip", cl.Tags)
	skuStandard := armnetwork.PublicIPAddressSKUNameStandard
	allocStatic := armnetwork.IPAllocationMethodStatic

	poller, err := clients.PublicIPAddresses.BeginCreateOrUpdate(ctx, rgName, pipName, armnetwork.PublicIPAddress{
		Location: &location,
		Tags:     pipTags,
		SKU: &armnetwork.PublicIPAddressSKU{
			Name: &skuStandard,
		},
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: &allocStatic,
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("phase14: BeginCreateOrUpdate ingress PublicIP %s: %w", pipName, err)
	}

	var resp armnetwork.PublicIPAddressesClientCreateOrUpdateResponse
	if poller != nil {
		resp, err = poller.PollUntilDone(ctx, nil)
		if err != nil {
			return fmt.Errorf("phase14: waiting for ingress PublicIP %s: %w", pipName, err)
		}
	}

	if st != nil {
		if resp.ID != nil {
			st.Set("AZURE_INGRESS_PIP_ID", *resp.ID)
			st.Set("VIP_PIP_ID", *resp.ID)
		}
		st.Set("AZURE_INGRESS_PIP_NAME", pipName)
		if resp.Properties != nil && resp.Properties.IPAddress != nil {
			st.Set("AZURE_INGRESS_PIP_ADDRESS", *resp.Properties.IPAddress)
			st.Set("VIP_PUBLIC_IP", *resp.Properties.IPAddress)
			fmt.Fprintf(os.Stderr, "[phase 14] Ingress Public IP: %s\n", *resp.Properties.IPAddress)
		}

		if err := st.Save(); err != nil {
			return fmt.Errorf("phase14: saving state: %w", err)
		}
	}

	fmt.Fprintf(os.Stderr, "[phase 14] Ingress Public IP %s OK\n", pipName)
	return nil
}
