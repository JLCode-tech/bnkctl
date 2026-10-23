package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// isAzureNotFound returns true only when err is an Azure ARM HTTP 404 or one of
// the standard NotFound error codes. Non-404 errors (e.g. 409 ScopeLocked, 403
// AuthorizationFailed) must be propagated so state keys are not prematurely
// cleared while resources still exist in Azure.
func isAzureNotFound(err error) bool {
	if err == nil {
		return false
	}
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		if respErr.StatusCode == http.StatusNotFound {
			return true
		}
		switch respErr.ErrorCode {
		case "ResourceNotFound", "ResourceGroupNotFound", "ParentResourceNotFound", "NotFound":
			return true
		}
	}
	return false
}

func resolveAzureDownTarget(cl *intent.Cluster, st *state.State) (rg, aksClusterName, baseName string) {
	if cl != nil {
		ensureAzureDefaults(cl)
	}
	if st != nil {
		rg = strings.TrimSpace(st.Get("AZURE_RESOURCE_GROUP"))
		if rg == "" {
			rg = strings.TrimSpace(st.Get("RESOURCE_GROUP"))
		}
		aksClusterName = strings.TrimSpace(st.Get("AZURE_AKS_CLUSTER_NAME"))
		if aksClusterName == "" {
			aksClusterName = strings.TrimSpace(st.Get("CLUSTER_NAME"))
		}
	}
	if rg == "" && cl != nil {
		rg = strings.TrimSpace(cl.Metadata.ResourceGroup)
		if rg == "" {
			rg = strings.TrimSpace(cl.Derived.ResourceGroupName)
		}
	}
	if aksClusterName == "" && cl != nil {
		aksClusterName = strings.TrimSpace(cl.Derived.AKSClusterName)
		if aksClusterName == "" {
			aksClusterName = strings.TrimSpace(cl.Metadata.Name)
		}
	}
	if aksClusterName != "" {
		baseName = strings.TrimPrefix(aksClusterName, "aks-")
	} else if cl != nil {
		baseName = strings.TrimSpace(cl.Metadata.Name)
	}
	return rg, aksClusterName, baseName
}

func uniqueNonEmpty(items ...string) []string {
	var out []string
	for _, raw := range items {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		dup := false
		for _, existing := range out {
			if existing == s {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, s)
		}
	}
	return out
}

// PhaseDownCluster deletes the AKS TMM nodepool and AKS cluster in reverse order.
func PhaseDownCluster(ctx context.Context, c *Clients, cl *intent.Cluster, st *state.State) error {
	if c == nil || ShouldSkipAuth() {
		if st != nil {
			st.Set("AKS_CLUSTER_ID", "")
			st.Set("TMM_POOL_ID", "")
			return st.Save()
		}
		return nil
	}

	rg, clusterName, baseName := resolveAzureDownTarget(cl, st)
	tmmPoolName := ""
	stateClusterName := ""
	if st != nil {
		stateClusterName = strings.TrimSpace(st.Get("AZURE_AKS_CLUSTER_NAME"))
		if stateClusterName == "" {
			stateClusterName = strings.TrimSpace(st.Get("CLUSTER_NAME"))
		}
		tmmPoolName = strings.TrimSpace(st.Get("AZURE_TMM_POOL_NAME"))
		if tmmPoolName == "" {
			tmmPoolName = strings.TrimSpace(st.Get("TMM_NODEPOOL_NAME"))
		}
	}
	if tmmPoolName == "" && cl != nil && cl.ClusterSpec != nil && strings.TrimSpace(cl.ClusterSpec.TMMNodePool.Name) != "" {
		tmmPoolName = strings.TrimSpace(cl.ClusterSpec.TMMNodePool.Name)
	}
	if tmmPoolName == "" {
		tmmPoolName = "tmm"
	}

	var clusterCandidates []string
	if stateClusterName != "" {
		clusterCandidates = uniqueNonEmpty(stateClusterName)
	} else if baseName != "" {
		clusterCandidates = uniqueNonEmpty(clusterName, baseName, "aks-"+baseName)
	} else {
		clusterCandidates = uniqueNonEmpty(clusterName)
	}

	if c.AgentPools != nil && rg != "" {
		for _, cand := range clusterCandidates {
			poller, err := c.AgentPools.BeginDelete(ctx, rg, cand, tmmPoolName, nil)
			if err != nil {
				if !isAzureNotFound(err) {
					return fmt.Errorf("failed to delete TMM nodepool %s: %w", tmmPoolName, err)
				}
			} else if poller != nil {
				if _, err := poller.PollUntilDone(ctx, nil); err != nil && !isAzureNotFound(err) {
					return fmt.Errorf("polling TMM nodepool %s deletion: %w", tmmPoolName, err)
				}
			}
		}
	}

	if c.ManagedClusters != nil && rg != "" {
		for _, cand := range clusterCandidates {
			poller, err := c.ManagedClusters.BeginDelete(ctx, rg, cand, nil)
			if err != nil {
				if !isAzureNotFound(err) {
					return fmt.Errorf("failed to delete AKS cluster %s: %w", cand, err)
				}
			} else if poller != nil {
				if _, err := poller.PollUntilDone(ctx, nil); err != nil && !isAzureNotFound(err) {
					return fmt.Errorf("polling AKS cluster %s deletion: %w", cand, err)
				}
			}
		}
	}

	if st != nil {
		st.Set("AKS_CLUSTER_ID", "")
		st.Set("TMM_POOL_ID", "")
		return st.Save()
	}
	return nil
}

// PhaseDownBigIPVE deletes any BIG-IP VE VM, NICs, OS disk, and Public IPs recorded in state
// or derived from the cluster base name when state was cleared.
func PhaseDownBigIPVE(ctx context.Context, c *Clients, cl *intent.Cluster, st *state.State) error {
	if c == nil || ShouldSkipAuth() {
		return nil
	}
	rg, _, baseName := resolveAzureDownTarget(cl, st)
	if rg == "" {
		return nil
	}

	deleteGeneric := func(providerNS, resType, name, apiVersion string) error {
		if c.Resources == nil || strings.TrimSpace(name) == "" {
			return nil
		}
		poller, err := c.Resources.BeginDelete(ctx, rg, providerNS, "", resType, strings.TrimSpace(name), apiVersion, nil)
		if err != nil {
			if isAzureNotFound(err) {
				return nil
			}
			return fmt.Errorf("failed to delete %s/%s %s: %w", providerNS, resType, name, err)
		}
		if poller != nil {
			if _, err := poller.PollUntilDone(ctx, nil); err != nil && !isAzureNotFound(err) {
				return fmt.Errorf("polling %s/%s %s deletion: %w", providerNS, resType, name, err)
			}
		}
		return nil
	}

	vmName := ""
	if st != nil {
		vmName = strings.TrimSpace(st.Get("BIGIP_VM_NAME"))
	}
	if vmName == "" && baseName != "" {
		vmName = baseName + "-bigip-ve"
	}
	if vmName != "" {
		if err := deleteGeneric("Microsoft.Compute", "virtualMachines", vmName, "2024-03-01"); err != nil {
			return err
		}
	}

	nicDefaults := []struct {
		key      string
		fallback string
	}{
		{"BIGIP_MGMT_NIC_NAME", "-bigip-mgmt-nic"},
		{"BIGIP_EXT_NIC_NAME", "-bigip-ext-nic"},
		{"BIGIP_INT_NIC_NAME", "-bigip-int-nic"},
	}
	for _, nd := range nicDefaults {
		nicName := ""
		if st != nil {
			nicName = strings.TrimSpace(st.Get(nd.key))
		}
		if nicName == "" && baseName != "" {
			nicName = baseName + nd.fallback
		}
		if nicName != "" {
			if err := deleteGeneric("Microsoft.Network", "networkInterfaces", nicName, "2023-11-01"); err != nil {
				return err
			}
		}
	}

	diskName := ""
	if st != nil {
		diskName = strings.TrimSpace(st.Get("BIGIP_OS_DISK_NAME"))
	}
	if diskName == "" && baseName != "" {
		diskName = baseName + "-bigip-ve-osdisk"
	}
	if diskName != "" {
		if err := deleteGeneric("Microsoft.Compute", "disks", diskName, "2023-10-02"); err != nil {
			return err
		}
	}

	if c.PublicIPAddresses != nil {
		pipDefaults := []struct {
			key      string
			fallback string
		}{
			{"BIGIP_MGMT_PIP_NAME", "-bigip-mgmt-pip"},
			{"BIGIP_EXT_PIP_NAME", "-bigip-ext-pip"},
		}
		for _, pd := range pipDefaults {
			pipName := ""
			if st != nil {
				pipName = strings.TrimSpace(st.Get(pd.key))
			}
			if pipName == "" && baseName != "" {
				pipName = baseName + pd.fallback
			}
			if pipName == "" {
				continue
			}
			poller, err := c.PublicIPAddresses.BeginDelete(ctx, rg, pipName, nil)
			if err != nil {
				if !isAzureNotFound(err) {
					return fmt.Errorf("failed to delete BIG-IP Public IP %s: %w", pipName, err)
				}
			} else if poller != nil {
				if _, err := poller.PollUntilDone(ctx, nil); err != nil && !isAzureNotFound(err) {
					return fmt.Errorf("polling BIG-IP Public IP %s deletion: %w", pipName, err)
				}
			}
		}
	}
	return nil
}

// PhaseDownNAT deletes the Azure NAT Gateway and Public IPs.
func PhaseDownNAT(ctx context.Context, c *Clients, cl *intent.Cluster, st *state.State) error {
	if c == nil || ShouldSkipAuth() {
		if st != nil {
			st.Set("NAT_GW_ID", "")
			st.Set("NAT_PIP_ID", "")
			st.Set("VIP_PIP_ID", "")
			st.Set("VIP_PUBLIC_IP", "")
			st.Set("AZURE_NAT_GW_NAME", "")
			st.Set("AZURE_NAT_PIP_NAME", "")
			st.Set("AZURE_INGRESS_PIP_NAME", "")
			st.Set("AZURE_INGRESS_PUBLIC_IP", "")
			return st.Save()
		}
		return nil
	}

	rg, _, baseName := resolveAzureDownTarget(cl, st)

	stateNatName := ""
	if st != nil {
		stateNatName = strings.TrimSpace(st.Get("AZURE_NAT_GW_NAME"))
	}
	var natCandidates []string
	if stateNatName != "" {
		natCandidates = uniqueNonEmpty(stateNatName)
	} else {
		derivedNat := ""
		if cl != nil {
			derivedNat = cl.Derived.NatGatewayName
		}
		if baseName != "" {
			natCandidates = uniqueNonEmpty(derivedNat, fmt.Sprintf("%s-nat", baseName), fmt.Sprintf("natgw-%s", baseName))
		} else {
			natCandidates = uniqueNonEmpty(derivedNat)
		}
	}

	stateNatPip := ""
	stateIngressPip := ""
	if st != nil {
		stateNatPip = strings.TrimSpace(st.Get("AZURE_NAT_PIP_NAME"))
		stateIngressPip = strings.TrimSpace(st.Get("AZURE_INGRESS_PIP_NAME"))
	}
	derivedNatPip := ""
	if cl != nil {
		derivedNatPip = cl.Derived.NatGatewayPublicIPName
	}

	var pips []string
	if baseName != "" {
		pips = uniqueNonEmpty(
			stateNatPip,
			derivedNatPip,
			fmt.Sprintf("%s-nat-pip", baseName),
			fmt.Sprintf("pip-nat-%s", baseName),
			stateIngressPip,
			fmt.Sprintf("pip-ingress-%s", baseName),
			fmt.Sprintf("%s-vip-pip", baseName),
		)
	} else {
		pips = uniqueNonEmpty(stateNatPip, derivedNatPip, stateIngressPip)
	}

	if c.NatGateways != nil && rg != "" {
		for _, natName := range natCandidates {
			poller, err := c.NatGateways.BeginDelete(ctx, rg, natName, nil)
			if err != nil {
				if !isAzureNotFound(err) {
					return fmt.Errorf("failed to delete NAT gateway %s: %w", natName, err)
				}
			} else if poller != nil {
				if _, err := poller.PollUntilDone(ctx, nil); err != nil && !isAzureNotFound(err) {
					return fmt.Errorf("polling NAT gateway %s deletion: %w", natName, err)
				}
			}
		}
	}

	if c.PublicIPAddresses != nil && rg != "" {
		for _, pip := range pips {
			poller, err := c.PublicIPAddresses.BeginDelete(ctx, rg, pip, nil)
			if err != nil {
				if !isAzureNotFound(err) {
					return fmt.Errorf("failed to delete Public IP %s: %w", pip, err)
				}
			} else if poller != nil {
				if _, err := poller.PollUntilDone(ctx, nil); err != nil && !isAzureNotFound(err) {
					return fmt.Errorf("polling Public IP %s deletion: %w", pip, err)
				}
			}
		}
	}

	if st != nil {
		st.Set("NAT_GW_ID", "")
		st.Set("NAT_PIP_ID", "")
		st.Set("VIP_PIP_ID", "")
		st.Set("VIP_PUBLIC_IP", "")
		st.Set("AZURE_NAT_GW_NAME", "")
		st.Set("AZURE_NAT_PIP_NAME", "")
		st.Set("AZURE_INGRESS_PIP_NAME", "")
		st.Set("AZURE_INGRESS_PUBLIC_IP", "")
		return st.Save()
	}
	return nil
}

// PhaseDownNetworks deletes the VNet (and its subnets) and the NSG.
func PhaseDownNetworks(ctx context.Context, c *Clients, cl *intent.Cluster, st *state.State) error {
	if c == nil || ShouldSkipAuth() {
		if st != nil {
			st.Set("VNET_ID", "")
			st.Set("NSG_ID", "")
			st.Set("SUBNET_SYSTEM_ID", "")
			st.Set("SUBNET_TMM_ID", "")
			st.Set("SUBNET_EXT_ID", "")
			st.Set("SUBNET_INT_ID", "")
			st.Set("AZURE_VNET_NAME", "")
			st.Set("AZURE_NSG_NAME", "")
			return st.Save()
		}
		return nil
	}

	rg, _, baseName := resolveAzureDownTarget(cl, st)

	stateVNet := ""
	stateNSG := ""
	if st != nil {
		stateVNet = strings.TrimSpace(st.Get("AZURE_VNET_NAME"))
		stateNSG = strings.TrimSpace(st.Get("AZURE_NSG_NAME"))
	}

	var vnetCandidates []string
	if stateVNet != "" {
		vnetCandidates = uniqueNonEmpty(stateVNet)
	} else {
		derivedVNet := ""
		if cl != nil {
			derivedVNet = cl.Derived.VNetName
		}
		if baseName != "" {
			vnetCandidates = uniqueNonEmpty(derivedVNet, fmt.Sprintf("%s-vnet", baseName), fmt.Sprintf("vnet-%s", baseName))
		} else {
			vnetCandidates = uniqueNonEmpty(derivedVNet)
		}
	}

	var nsgCandidates []string
	if stateNSG != "" {
		nsgCandidates = uniqueNonEmpty(stateNSG)
	} else {
		derivedNSG := ""
		if cl != nil {
			derivedNSG = cl.Derived.NSGName
		}
		if baseName != "" {
			nsgCandidates = uniqueNonEmpty(derivedNSG, fmt.Sprintf("%s-nsg", baseName), fmt.Sprintf("nsg-%s", baseName))
		} else {
			nsgCandidates = uniqueNonEmpty(derivedNSG)
		}
	}

	if c.VirtualNetworks != nil && rg != "" {
		for _, vnetName := range vnetCandidates {
			poller, err := c.VirtualNetworks.BeginDelete(ctx, rg, vnetName, nil)
			if err != nil {
				if !isAzureNotFound(err) {
					return fmt.Errorf("failed to delete VNet %s: %w", vnetName, err)
				}
			} else if poller != nil {
				if _, err := poller.PollUntilDone(ctx, nil); err != nil && !isAzureNotFound(err) {
					return fmt.Errorf("polling VNet %s deletion: %w", vnetName, err)
				}
			}
		}
	}

	if c.SecurityGroups != nil && rg != "" {
		for _, nsgName := range nsgCandidates {
			poller, err := c.SecurityGroups.BeginDelete(ctx, rg, nsgName, nil)
			if err != nil {
				if !isAzureNotFound(err) {
					return fmt.Errorf("failed to delete NSG %s: %w", nsgName, err)
				}
			} else if poller != nil {
				if _, err := poller.PollUntilDone(ctx, nil); err != nil && !isAzureNotFound(err) {
					return fmt.Errorf("polling NSG %s deletion: %w", nsgName, err)
				}
			}
		}
	}

	if st != nil {
		st.Set("VNET_ID", "")
		st.Set("NSG_ID", "")
		st.Set("SUBNET_SYSTEM_ID", "")
		st.Set("SUBNET_TMM_ID", "")
		st.Set("SUBNET_EXT_ID", "")
		st.Set("SUBNET_INT_ID", "")
		st.Set("AZURE_VNET_NAME", "")
		st.Set("AZURE_NSG_NAME", "")
		return st.Save()
	}
	return nil
}

// PhaseDownResourceGroup optionally deletes the Azure Resource Group when deleteRG is true.
func PhaseDownResourceGroup(ctx context.Context, c *Clients, cl *intent.Cluster, st *state.State, deleteRG bool) error {
	if !deleteRG {
		return nil
	}
	if c == nil || ShouldSkipAuth() {
		if st != nil {
			st.Set("RG_ID", "")
			return st.Save()
		}
		return nil
	}

	rg, _, _ := resolveAzureDownTarget(cl, st)

	if c.ResourceGroups != nil && rg != "" {
		poller, err := c.ResourceGroups.BeginDelete(ctx, rg, nil)
		if err != nil {
			if !isAzureNotFound(err) {
				return fmt.Errorf("failed to delete Resource Group %s: %w", rg, err)
			}
		} else if poller != nil {
			if _, err := poller.PollUntilDone(ctx, nil); err != nil && !isAzureNotFound(err) {
				return fmt.Errorf("polling Resource Group %s deletion: %w", rg, err)
			}
		}
	}

	if st != nil {
		st.Set("RG_ID", "")
		return st.Save()
	}
	return nil
}

// StripStuckFinalizers clears metadata.finalizers on any F5 / BNK custom resources
// in ns that remain after CNEInstance and Helm releases have been removed.
func StripStuckFinalizers(ctx context.Context, dyn dynamic.Interface, ns string) {
	if dyn == nil {
		return
	}
	gvrs := []schema.GroupVersionResource{
		{Group: "k8s.f5.com", Version: "v1", Resource: "cneinstances"},
		{Group: "k8s.f5.com", Version: "v1", Resource: "f5spkvlan"},
		{Group: "k8s.f5.com", Version: "v1", Resource: "f5spkvlans"},
		{Group: "k8s.f5.com", Version: "v1", Resource: "f5spkstaticroutes"},
		{Group: "k8s.f5.com", Version: "v1", Resource: "f5spksnatpools"},
		{Group: "k8s.f5.com", Version: "v1", Resource: "f5spkegresses"},
		{Group: "fic.f5.com", Version: "v1alpha1", Resource: "infras"},
		{Group: "fic.f5.com", Version: "v1alpha1", Resource: "gatewaysettings"},
		{Group: "fic.f5.com", Version: "v1alpha1", Resource: "egressgateways"},
		{Group: "fic.f5.com", Version: "v1alpha1", Resource: "l4routes"},
		{Group: "fic.f5.com", Version: "v1", Resource: "infras"},
		{Group: "fic.f5.com", Version: "v1", Resource: "gatewaysettings"},
		{Group: "fic.f5.com", Version: "v1", Resource: "egressgateways"},
		{Group: "fic.f5.com", Version: "v1", Resource: "l4routes"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "infras"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "gatewaysettings"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "egressgateways"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "l4routes"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "l4globalpolicies"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "apppolicies"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "infras"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "gatewaysettings"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "egressgateways"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "l4routes"},
		{Group: "gateway.k8s.f5net.com", Version: "v1", Resource: "l4routes"},
		{Group: "gateway.k8s.f5net.com", Version: "v1", Resource: "gatewaysettings"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "infras"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "egressgateways"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-big-dns-caches"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-big-persistence-profiles"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-spk-egress"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-spk-egresses"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-spk-snatpools"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-spk-staticroutes"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-spk-vlans"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "licenses"},
		{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "airoutepolicies"},
		{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "airoutes"},
		{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aigateways"},
		{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aigatewayprofiles"},
		{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aimodels"},
		{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aiproviders"},
		{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aicredentials"},
		{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aitenants"},
		{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"},
		{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"},
		{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "grpcroutes"},
	}

	patchBytes, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"finalizers": nil,
		},
	})

	for _, gvr := range gvrs {
		list, err := dyn.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
		if err != nil || list == nil {
			continue
		}
		for _, item := range list.Items {
			if len(item.GetFinalizers()) == 0 {
				continue
			}
			_, _ = dyn.Resource(gvr).Namespace(ns).Patch(
				ctx, item.GetName(), types.MergePatchType, patchBytes, metav1.PatchOptions{},
			)
		}
	}

	tenantGVR := schema.GroupVersionResource{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aitenants"}
	if list, err := dyn.Resource(tenantGVR).List(ctx, metav1.ListOptions{}); err == nil && list != nil {
		for _, item := range list.Items {
			if len(item.GetFinalizers()) == 0 {
				continue
			}
			_, _ = dyn.Resource(tenantGVR).Patch(
				ctx, item.GetName(), types.MergePatchType, patchBytes, metav1.PatchOptions{},
			)
		}
	}
}
