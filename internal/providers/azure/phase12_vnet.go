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

// Phase12VNet provisions the Virtual Network and required subnets.
func Phase12VNet(ctx context.Context, cl *intent.Cluster, st *state.State, clients *Clients, dryRun bool) error {
	ensureAzureDefaults(cl)
	rgName := cl.Derived.ResourceGroupName
	vnetName := cl.Derived.VNetName
	location := cl.Metadata.Location

	fmt.Fprintf(os.Stderr, "[phase 12] vnet: %s (CIDR: %s)\n", vnetName, cl.Network.VNetCIDR)

	if dryRun {
		if st != nil {
			st.Set("AZURE_VNET_NAME", vnetName)
			st.Set("AZURE_SYSTEM_SUBNET_NAME", cl.Derived.SystemSubnetName)
			st.Set("AZURE_EXTERNAL_SUBNET_NAME", cl.Derived.ExternalSubnetName)
			if cl.Derived.InternalSubnetName != "" {
				st.Set("AZURE_INTERNAL_SUBNET_NAME", cl.Derived.InternalSubnetName)
			}
		}
		fmt.Fprintf(os.Stderr, "[phase 12] [dry-run] would create VNet %s with subnets\n", vnetName)
		return nil
	}

	if clients == nil || clients.VirtualNetworks == nil {
		return nil
	}

	vnetTags := tags.ForCluster(cl.Metadata.Name, "vnet", cl.Tags)
	vnetCIDR := cl.Network.VNetCIDR

	var nsgID string
	if st != nil {
		nsgID = st.Get("AZURE_NSG_ID")
		if nsgID == "" {
			nsgID = st.Get("NSG_ID")
		}
	}
	var nsgRef *armnetwork.SecurityGroup
	if nsgID != "" {
		nsgRef = &armnetwork.SecurityGroup{ID: &nsgID}
	}

	subnets := []*armnetwork.Subnet{
		{
			Name: &cl.Derived.SystemSubnetName,
			Properties: &armnetwork.SubnetPropertiesFormat{
				AddressPrefix:        &cl.Network.Subnets.System,
				NetworkSecurityGroup: nsgRef,
			},
		},
		{
			Name: &cl.Derived.ExternalSubnetName,
			Properties: &armnetwork.SubnetPropertiesFormat{
				AddressPrefix:        &cl.Network.Subnets.External,
				NetworkSecurityGroup: nsgRef,
			},
		},
	}

	if cl.Network.Subnets.Public != "" {
		subnets = append(subnets, &armnetwork.Subnet{
			Name: &cl.Derived.PublicSubnetName,
			Properties: &armnetwork.SubnetPropertiesFormat{
				AddressPrefix:        &cl.Network.Subnets.Public,
				NetworkSecurityGroup: nsgRef,
			},
		})
	}

	if cl.Network.Subnets.Internal != "" {
		subnets = append(subnets, &armnetwork.Subnet{
			Name: &cl.Derived.InternalSubnetName,
			Properties: &armnetwork.SubnetPropertiesFormat{
				AddressPrefix:        &cl.Network.Subnets.Internal,
				NetworkSecurityGroup: nsgRef,
			},
		})
	}

	poller, err := clients.VirtualNetworks.BeginCreateOrUpdate(ctx, rgName, vnetName, armnetwork.VirtualNetwork{
		Location: &location,
		Tags:     vnetTags,
		Properties: &armnetwork.VirtualNetworkPropertiesFormat{
			AddressSpace: &armnetwork.AddressSpace{
				AddressPrefixes: []*string{&vnetCIDR},
			},
			Subnets: subnets,
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("phase12: BeginCreateOrUpdate VNet %s: %w", vnetName, err)
	}

	var resp armnetwork.VirtualNetworksClientCreateOrUpdateResponse
	if poller != nil {
		resp, err = poller.PollUntilDone(ctx, nil)
		if err != nil {
			return fmt.Errorf("phase12: waiting for VNet %s: %w", vnetName, err)
		}
	}

	if st != nil {
		if resp.ID != nil {
			st.Set("AZURE_VNET_ID", *resp.ID)
			st.Set("VNET_ID", *resp.ID)
		}
		st.Set("AZURE_VNET_NAME", vnetName)
		st.Set("AZURE_SYSTEM_SUBNET_NAME", cl.Derived.SystemSubnetName)
		st.Set("AZURE_EXTERNAL_SUBNET_NAME", cl.Derived.ExternalSubnetName)
		if cl.Derived.InternalSubnetName != "" {
			st.Set("AZURE_INTERNAL_SUBNET_NAME", cl.Derived.InternalSubnetName)
		}

		if resp.Properties != nil {
			for _, sn := range resp.Properties.Subnets {
				if sn.Name != nil && sn.ID != nil {
					if *sn.Name == cl.Derived.SystemSubnetName {
						st.Set("AZURE_SYSTEM_SUBNET_ID", *sn.ID)
						st.Set("SUBNET_SYSTEM_ID", *sn.ID)
					}
					if *sn.Name == cl.Derived.ExternalSubnetName {
						st.Set("AZURE_EXTERNAL_SUBNET_ID", *sn.ID)
						st.Set("SUBNET_EXT_ID", *sn.ID)
					}
					if *sn.Name == cl.Derived.InternalSubnetName {
						st.Set("AZURE_INTERNAL_SUBNET_ID", *sn.ID)
						st.Set("SUBNET_INT_ID", *sn.ID)
					}
				}
			}
		}

		if err := st.Save(); err != nil {
			return fmt.Errorf("phase12: saving state: %w", err)
		}
	}

	fmt.Fprintf(os.Stderr, "[phase 12] VNet %s OK\n", vnetName)
	return nil
}
