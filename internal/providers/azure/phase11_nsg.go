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

// Phase11NSG creates the Network Security Group for the cluster subnets.
func Phase11NSG(ctx context.Context, cl *intent.Cluster, st *state.State, clients *Clients, dryRun bool) error {
	ensureAzureDefaults(cl)
	rgName := cl.Derived.ResourceGroupName
	nsgName := fmt.Sprintf("nsg-%s", cl.Metadata.Name)
	if cl.Derived.NSGName != "" {
		nsgName = cl.Derived.NSGName
	}
	location := cl.Metadata.Location

	fmt.Fprintf(os.Stderr, "[phase 11] nsg: %s\n", nsgName)

	if dryRun {
		if st != nil {
			st.Set("AZURE_NSG_NAME", nsgName)
		}
		fmt.Fprintf(os.Stderr, "[phase 11] [dry-run] would create NSG %s\n", nsgName)
		return nil
	}

	if clients == nil || clients.SecurityGroups == nil {
		return nil
	}

	nsgTags := tags.ForCluster(cl.Metadata.Name, "nsg", cl.Tags)

	allow := armnetwork.SecurityRuleAccessAllow
	inbound := armnetwork.SecurityRuleDirectionInbound
	tcp := armnetwork.SecurityRuleProtocolTCP
	udp := armnetwork.SecurityRuleProtocolUDP
	allProto := armnetwork.SecurityRuleProtocolAsterisk
	anySrc := "*"
	azureLB := "AzureLoadBalancer"

	p80 := "80"
	p443 := "443"
	p4789 := "4789"
	p179 := "179"
	pAny := "*"

	r80Name := "Allow-HTTP"
	r443Name := "Allow-HTTPS"
	rVxlanName := "Allow-VXLAN"
	rBgpName := "Allow-BGP"
	rLbName := "Allow-AzureLB-Probes"

	r80Priority := int32(100)
	r443Priority := int32(110)
	rVxlanPriority := int32(120)
	rBgpPriority := int32(130)
	rLbPriority := int32(140)

	rules := []*armnetwork.SecurityRule{
		{
			Name: &r80Name,
			Properties: &armnetwork.SecurityRulePropertiesFormat{
				Protocol:                 &tcp,
				SourceAddressPrefix:      &anySrc,
				SourcePortRange:          &anySrc,
				DestinationAddressPrefix: &anySrc,
				DestinationPortRange:     &p80,
				Access:                   &allow,
				Priority:                 &r80Priority,
				Direction:                &inbound,
			},
		},
		{
			Name: &r443Name,
			Properties: &armnetwork.SecurityRulePropertiesFormat{
				Protocol:                 &tcp,
				SourceAddressPrefix:      &anySrc,
				SourcePortRange:          &anySrc,
				DestinationAddressPrefix: &anySrc,
				DestinationPortRange:     &p443,
				Access:                   &allow,
				Priority:                 &r443Priority,
				Direction:                &inbound,
			},
		},
		{
			Name: &rVxlanName,
			Properties: &armnetwork.SecurityRulePropertiesFormat{
				Protocol:                 &udp,
				SourceAddressPrefix:      &anySrc,
				SourcePortRange:          &anySrc,
				DestinationAddressPrefix: &anySrc,
				DestinationPortRange:     &p4789,
				Access:                   &allow,
				Priority:                 &rVxlanPriority,
				Direction:                &inbound,
			},
		},
		{
			Name: &rBgpName,
			Properties: &armnetwork.SecurityRulePropertiesFormat{
				Protocol:                 &tcp,
				SourceAddressPrefix:      &anySrc,
				SourcePortRange:          &anySrc,
				DestinationAddressPrefix: &anySrc,
				DestinationPortRange:     &p179,
				Access:                   &allow,
				Priority:                 &rBgpPriority,
				Direction:                &inbound,
			},
		},
		{
			Name: &rLbName,
			Properties: &armnetwork.SecurityRulePropertiesFormat{
				Protocol:                 &allProto,
				SourceAddressPrefix:      &azureLB,
				SourcePortRange:          &anySrc,
				DestinationAddressPrefix: &anySrc,
				DestinationPortRange:     &pAny,
				Access:                   &allow,
				Priority:                 &rLbPriority,
				Direction:                &inbound,
			},
		},
	}

	poller, err := clients.SecurityGroups.BeginCreateOrUpdate(ctx, rgName, nsgName, armnetwork.SecurityGroup{
		Location: &location,
		Tags:     nsgTags,
		Properties: &armnetwork.SecurityGroupPropertiesFormat{
			SecurityRules: rules,
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("phase11: BeginCreateOrUpdate NSG %s: %w", nsgName, err)
	}

	var resp armnetwork.SecurityGroupsClientCreateOrUpdateResponse
	if poller != nil {
		resp, err = poller.PollUntilDone(ctx, nil)
		if err != nil {
			return fmt.Errorf("phase11: waiting for NSG %s: %w", nsgName, err)
		}
	}

	if st != nil {
		if resp.ID != nil {
			st.Set("AZURE_NSG_ID", *resp.ID)
			st.Set("NSG_ID", *resp.ID)
		}
		st.Set("AZURE_NSG_NAME", nsgName)
		if err := st.Save(); err != nil {
			return fmt.Errorf("phase11: saving state: %w", err)
		}
	}

	fmt.Fprintf(os.Stderr, "[phase 11] NSG %s OK\n", nsgName)
	return nil
}
