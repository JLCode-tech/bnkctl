package azure

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/bnkconst"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/jumphost"
	"github.com/JLCode-tech/bnkctl/pkg/provider"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Provider implements provider.Provider for Azure (AKS + VNet + NSG + NAT + TMM Nodepool).
type Provider struct {
	Clients *Clients
}

// NewProvider returns a new Azure underlay provider.
func NewProvider() *Provider {
	return &Provider{}
}

// Name returns the canonical cloud provider identifier.
func (p *Provider) Name() string {
	return intent.ProviderAzure
}

func resolveAzureSubscriptionID(cl *intent.Cluster, st *state.State) string {
	if st != nil {
		for _, k := range []string{"SUBSCRIPTION_ID", "AZURE_SUBSCRIPTION_ID"} {
			if v := strings.TrimSpace(st.Get(k)); v != "" && v != "00000000-0000-0000-0000-000000000000" {
				return v
			}
		}
		for _, k := range []string{"AZURE_RESOURCE_GROUP_ID", "RG_ID", "AZURE_AKS_CLUSTER_ID", "AKS_CLUSTER_ID", "AZURE_VNET_ID", "VNET_ID"} {
			val := strings.TrimSpace(st.Get(k))
			if strings.HasPrefix(strings.ToLower(val), "/subscriptions/") {
				parts := strings.Split(val, "/")
				if len(parts) >= 3 && parts[2] != "" && parts[2] != "00000000-0000-0000-0000-000000000000" {
					return parts[2]
				}
			}
		}
	}
	if cl != nil {
		if v := strings.TrimSpace(cl.Metadata.SubscriptionID); v != "" && v != "00000000-0000-0000-0000-000000000000" {
			return v
		}
	}
	return ""
}

func (p *Provider) ensureClients(cl *intent.Cluster, st *state.State, dryRun bool) error {
	if dryRun || ShouldSkipAuth() {
		return nil
	}
	intentSubID := resolveAzureSubscriptionID(cl, st)
	if p.Clients != nil {
		if intentSubID == "" || p.Clients.SubscriptionID == "" || strings.EqualFold(p.Clients.SubscriptionID, intentSubID) {
			return nil
		}
	}
	if testing.Testing() && os.Getenv("BNKCTL_FORCE_LIVE_AUTH") != "1" {
		return nil
	}
	subID, err := ResolveSubscriptionID(intentSubID)
	if err != nil {
		return fmt.Errorf("azure provider: %w", err)
	}
	clients, err := NewClientsWithDefault(subID)
	if err != nil {
		return fmt.Errorf("azure provider: initializing clients: %w", err)
	}
	p.Clients = clients
	return nil
}

// ProvisionUnderlay provisions or reconciles the Azure IaaS underlay (Resource Group, NSG, VNet,
// Subnets, NAT Gateway, Public IPs, AKS Cluster, TMM NodePool, Kubeconfig, Node Labels)
// and returns the normalized provider.UnderlayContext for the BNK overlay engine.
func (p *Provider) ProvisionUnderlay(ctx context.Context, cl *intent.Cluster, st *state.State, dryRun bool) (*provider.UnderlayContext, error) {
	if cl == nil {
		return nil, fmt.Errorf("azure provider: cluster intent is nil")
	}
	ensureAzureDefaults(cl)

	if err := p.ensureClients(cl, st, dryRun); err != nil {
		return nil, err
	}

	hasFullIntent := cl.Metadata.SubscriptionID != "" || cl.Network.Subnets.System != ""
	if p.Clients != nil || (dryRun && hasFullIntent) {
		if err := Phase00Preflight(ctx, cl, st, p.Clients, dryRun); err != nil {
			return nil, err
		}
		underlayAdopted := !dryRun && st != nil && st.Get("AZURE_AKS_CLUSTER_ID") != "" && st.Get("AZURE_VNET_ID") != "" && st.Get("AZURE_TMM_POOL_ID") != ""
		if !underlayAdopted {
			if err := Phase10ResourceGroup(ctx, cl, st, p.Clients, dryRun); err != nil {
				return nil, err
			}
			if err := Phase11NSG(ctx, cl, st, p.Clients, dryRun); err != nil {
				return nil, err
			}
			if err := Phase12VNet(ctx, cl, st, p.Clients, dryRun); err != nil {
				return nil, err
			}
			if err := Phase13NatGateway(ctx, cl, st, p.Clients, dryRun); err != nil {
				return nil, err
			}
			if err := Phase14PublicIPs(ctx, cl, st, p.Clients, dryRun); err != nil {
				return nil, err
			}
			if err := Phase21AKSCluster(ctx, cl, st, p.Clients, dryRun); err != nil {
				return nil, err
			}
			if err := Phase22TMMNodePool(ctx, cl, st, p.Clients, dryRun); err != nil {
				return nil, err
			}
		} else {
			fmt.Fprintf(os.Stderr, "[phase 10-22] adopting existing Azure underlay from state (%s in %s)\n",
				st.Get("AZURE_AKS_CLUSTER_NAME"), st.Get("AZURE_RESOURCE_GROUP"))
		}
		if err := Phase23Kubeconfig(ctx, cl, st, p.Clients, dryRun); err != nil {
			return nil, err
		}
		if err := Phase24NodeLabels(ctx, cl, st, p.Clients, dryRun); err != nil {
			return nil, err
		}
		if err := Phase25Discovery(ctx, cl, st, p.Clients, dryRun); err != nil {
			return nil, err
		}
		if err := Phase29ForgeRegister(ctx, cl, st, p.Clients, dryRun); err != nil {
			return nil, err
		}
	}

	u, err := provider.BuildUnderlayFromIntent(cl)
	if err != nil {
		return nil, fmt.Errorf("azure provider: building underlay context: %w", err)
	}
	u.CloudName = intent.ProviderAzure
	u.DataplaneDriver = provider.DriverSocket

	p.attachHooks(u, cl, st, dryRun)
	p.hydrateFromState(u, st)

	if st != nil {
		st.Set("CLOUD_PROVIDER", intent.ProviderAzure)
		st.Set("CLUSTER_NAME", u.ClusterName)
		st.Set("AZURE_LOCATION", u.Region)
		if cl.Metadata.ResourceGroup != "" {
			st.Set("AZURE_RESOURCE_GROUP", cl.Metadata.ResourceGroup)
		}
		if u.NodeCIDR != "" {
			st.Set("VNET_CIDR", u.NodeCIDR)
		}
		if st.Get("NODE_PRIMARY_IFNAME") == "" {
			st.Set("NODE_PRIMARY_IFNAME", "eth0")
		}
		if ext := u.ExternalNetwork(); ext != nil && len(ext.SelfIPs) > 0 {
			st.Set("TMM_EXT_SELFIP", strings.Split(ext.SelfIPs[0], "/")[0])
		}
		if intl := u.InternalNetwork(); intl != nil && len(intl.SelfIPs) > 0 {
			st.Set("TMM_INT_SELFIP", strings.Split(intl.SelfIPs[0], "/")[0])
		}
		if vip, err := cl.DefaultVIP(); err == nil && vip != "" {
			st.Set("GATEWAY_VIP", vip)
		}
		if !dryRun {
			_ = st.Save()
		}
	}

	return u, nil
}

// DiscoverUnderlay inspects existing Azure / state.env metadata and constructs an UnderlayContext.
func (p *Provider) DiscoverUnderlay(ctx context.Context, cl *intent.Cluster, st *state.State) (*provider.UnderlayContext, error) {
	if cl == nil {
		cl = &intent.Cluster{Cloud: &intent.CloudSpec{Provider: intent.ProviderAzure}}
	}
	if st != nil {
		if v := st.Get("AZURE_RESOURCE_GROUP"); v != "" {
			cl.Metadata.ResourceGroup = v
			cl.Derived.ResourceGroupName = v
		} else if v := st.Get("RESOURCE_GROUP"); v != "" {
			cl.Metadata.ResourceGroup = v
			cl.Derived.ResourceGroupName = v
		}
		if v := st.Get("AZURE_AKS_CLUSTER_NAME"); v != "" {
			cl.Derived.AKSClusterName = v
		} else if v := st.Get("CLUSTER_NAME"); v != "" {
			cl.Derived.AKSClusterName = v
		}
		if cl.Metadata.Name == "" {
			cl.Metadata.Name = st.Get("CLUSTER_NAME")
		}
		if v := st.Get("AZURE_LOCATION"); v != "" {
			cl.Metadata.Location = v
			cl.Metadata.Region = v
		} else if v := st.Get("LOCATION"); v != "" {
			cl.Metadata.Location = v
			cl.Metadata.Region = v
		}
		if subID := resolveAzureSubscriptionID(cl, st); subID != "" {
			cl.Metadata.SubscriptionID = subID
		}
	}
	ensureAzureDefaults(cl)
	_ = p.ensureClients(cl, st, false)
	if st != nil && st.Get("KUBECONFIG") == "" && st.Get("KUBECONFIG_PATH") == "" && p.Clients != nil && p.Clients.ManagedClusters != nil && cl.Metadata.Name != "" && cl.Metadata.ResourceGroup != "" {
		_ = Phase23Kubeconfig(ctx, cl, st, p.Clients, false)
	}
	u, err := provider.BuildUnderlayFromIntent(cl)
	if err != nil {
		return nil, err
	}
	u.CloudName = intent.ProviderAzure
	u.DataplaneDriver = provider.DriverSocket
	p.attachHooks(u, cl, st, false)
	p.hydrateFromState(u, st)
	return u, nil
}

// DestroyUnderlay tears down the Azure underlay resources in reverse dependency order.
func (p *Provider) DestroyUnderlay(ctx context.Context, cl *intent.Cluster, st *state.State, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("azure provider: cluster intent is nil")
	}
	ensureAzureDefaults(cl)
	if err := p.ensureClients(cl, st, dryRun); err != nil {
		return err
	}
	_ = Phase29ForgeRegisterDown(ctx, cl, st, dryRun)
	if !dryRun && p.Clients != nil {
		if err := PhaseDownCluster(ctx, p.Clients, cl, st); err != nil {
			return err
		}
		if err := PhaseDownBigIPVE(ctx, p.Clients, cl, st); err != nil {
			return err
		}
		if err := PhaseDownNetworks(ctx, p.Clients, cl, st); err != nil {
			return err
		}
		if err := PhaseDownNAT(ctx, p.Clients, cl, st); err != nil {
			return err
		}
		deleteRG := st != nil && st.Get("DELETE_RESOURCE_GROUP") == "true"
		if err := PhaseDownResourceGroup(ctx, p.Clients, cl, st, deleteRG); err != nil {
			return err
		}
	}
	if st != nil && !dryRun {
		if dir := st.Dir(); dir != "" {
			for _, f := range []string{"kubeconfig", "portable-kubeconfig.yaml", "forge_link.json", "jumphost.pem", "bigip_id_rsa", "bigip_id_rsa.pub"} {
				_ = os.Remove(filepath.Join(dir, f))
			}
			_ = os.RemoveAll(filepath.Join(dir, "artifacts"))
		}
		for k := range st.All() {
			st.Delete(k)
		}
		st.Set("UNDERLAY_DESTROYED", "true")
		_ = st.Save()
	}
	return nil
}

// OpenJumphostSession opens a runner session to the Azure jumphost or in-cluster probe pod.
func (p *Provider) OpenJumphostSession(ctx context.Context, cl *intent.Cluster, st *state.State) (jumphost.Runner, error) {
	if st == nil {
		return nil, fmt.Errorf("azure jumphost: state is nil")
	}
	vmID := st.Get("JUMPHOST_RESOURCE_ID")
	ip := st.Get("JUMPHOST_PUBLIC_IP")
	if vmID == "" && ip == "" {
		if st.Get("KUBECONFIG") != "" || st.Get("KUBECONFIG_PATH") != "" {
			if kc := st.KubeconfigPath(); kc != "" {
				return &jumphost.PodExecRunner{Kubeconfig: kc}, nil
			}
		}
		return nil, fmt.Errorf("azure jumphost: neither JUMPHOST_RESOURCE_ID nor JUMPHOST_PUBLIC_IP found in state")
	}
	loc := st.Get("AZURE_LOCATION")
	if loc == "" && cl != nil {
		loc = cl.EffectiveRegion()
	}
	return &jumphost.SSHRunner{
		Opts: jumphost.ProbeOptions{
			Location:   loc,
			ResourceID: vmID,
			JumphostIP: ip,
		},
	}, nil
}

func (p *Provider) attachHooks(u *provider.UnderlayContext, cl *intent.Cluster, st *state.State, dryRun bool) {
	u.DiscoverDataplaneResources = func(ctx context.Context, k8sClient kubernetes.Interface, dynClient dynamic.Interface) error {
		if dryRun || k8sClient == nil {
			return nil
		}
		nodes, err := k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil
		}
		var names []string
		var dpNodes []provider.NodeAttachment
		for _, node := range nodes.Items {
			names = append(names, node.Name)
			nics := []provider.NICAttachment{
				{
					NetworkName:  "external-nad",
					NICIndex:     1,
					LinuxIfName:  "eth1",
					TMMInterface: "0.2",
					Driver:       provider.DriverSocket,
				},
			}
			if u.InternalNetwork() != nil {
				nics = append(nics, provider.NICAttachment{
					NetworkName:  "internal-nad",
					NICIndex:     2,
					LinuxIfName:  "eth2",
					TMMInterface: "0.3",
					Driver:       provider.DriverSocket,
				})
			}
			dpNodes = append(dpNodes, provider.NodeAttachment{
				NodeName:   node.Name,
				ProviderID: node.Spec.ProviderID,
				Zone:       node.Labels["topology.kubernetes.io/zone"],
				NICs:       nics,
			})
		}
		u.NodeNames = names
		u.DataplaneNodes = dpNodes
		return nil
	}
	u.ReconcileControllerIdentity = func(ctx context.Context, k8sClient kubernetes.Interface, dynClient dynamic.Interface) error {
		if dryRun || k8sClient == nil {
			return nil
		}
		clientID := ""
		if st != nil {
			clientID = st.Get("AZURE_CLIENT_ID")
			if clientID == "" {
				clientID = st.Get("AZURE_IDENTITY_CLIENT_ID")
			}
		}
		if clientID == "" {
			return nil
		}
		for _, ns := range []string{bnkconst.InstanceNamespace, bnkconst.CoreNamespace} {
			sa, err := k8sClient.CoreV1().ServiceAccounts(ns).Get(ctx, bnkconst.CNEControllerServiceAccount, metav1.GetOptions{})
			if err != nil {
				continue
			}
			if sa.Annotations == nil {
				sa.Annotations = make(map[string]string)
			}
			if sa.Labels == nil {
				sa.Labels = make(map[string]string)
			}
			sa.Annotations["azure.workload.identity/client-id"] = clientID
			sa.Annotations["azure.workload.identity/use"] = "true"
			sa.Labels["azure.workload.identity/use"] = "true"
			if _, err := k8sClient.CoreV1().ServiceAccounts(ns).Update(ctx, sa, metav1.UpdateOptions{}); err != nil {
				return err
			}
		}
		return nil
	}
	u.PostActivationHook = func(ctx context.Context, k8sClient kubernetes.Interface, dynClient dynamic.Interface) error {
		if dryRun || k8sClient == nil {
			return nil
		}
		pods, err := k8sClient.CoreV1().Pods(bnkconst.InstanceNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=f5-tmm",
		})
		if err != nil {
			return nil
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.Labels == nil {
				pod.Labels = make(map[string]string)
			}
			if pod.Labels["f5-traffic"] == "true" {
				continue
			}
			pod.Labels["f5-traffic"] = "true"
			_, _ = k8sClient.CoreV1().Pods(bnkconst.InstanceNamespace).Update(ctx, pod, metav1.UpdateOptions{})
		}
		return nil
	}
}

func (p *Provider) hydrateFromState(u *provider.UnderlayContext, st *state.State) {
	if st == nil {
		return
	}
	kcPath := st.Get("KUBECONFIG")
	if kcPath == "" {
		kcPath = st.Get("KUBECONFIG_PATH")
	}
	if kcPath != "" {
		if _, err := os.Stat(kcPath); err != nil {
			if candidate := st.KubeconfigPath(); candidate != "" {
				kcPath = candidate
			}
		}
	} else if candidate := st.KubeconfigPath(); candidate != "" {
		if _, err := os.Stat(candidate); err == nil {
			kcPath = candidate
		}
	}
	if kcPath != "" {
		u.KubeconfigPath = kcPath
		// #nosec G304 -- kubeconfig path comes from workspace state.env
		if raw, err := os.ReadFile(kcPath); err == nil {
			u.Kubeconfig = raw
		}
	}
	if svcCIDR := st.Get("SERVICE_CIDR"); svcCIDR != "" {
		u.ServiceCIDR = svcCIDR
	}
	if podCIDR := st.Get("POD_CIDR"); podCIDR != "" {
		u.PodCIDR = podCIDR
	}
}
