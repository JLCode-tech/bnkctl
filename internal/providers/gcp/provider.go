package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// Provider implements provider.Provider for GCP (GKE + Multi-VPC + Cloud NAT + Jumphost).
type Provider struct {
	Clients *Clients
}

// NewProvider returns a new GCP underlay provider.
func NewProvider() *Provider {
	return &Provider{}
}

// Name returns the canonical cloud provider identifier.
func (p *Provider) Name() string {
	return intent.ProviderGCP
}

// ensureClients lazily constructs live GCP SDK clients when p.Clients == nil,
// !dryRun, and !ShouldSkipAuth().
func (p *Provider) ensureClients(ctx context.Context, cl *intent.Cluster, st *state.State, dryRun bool) error {
	if p.Clients != nil || dryRun || ShouldSkipAuth() {
		return nil
	}
	if testing.Testing() && os.Getenv("BNKCTL_FORCE_LIVE_AUTH") != "1" {
		return nil
	}
	project := ""
	if cl != nil && !isPlaceholderProject(cl.Metadata.Project) {
		project = strings.TrimSpace(cl.Metadata.Project)
	}
	if project == "" && st != nil && !isPlaceholderProject(st.Get("GCP_PROJECT")) {
		project = strings.TrimSpace(st.Get("GCP_PROJECT"))
	}
	if project == "" {
		for _, envKey := range []string{"GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT", "CLOUDSDK_CORE_PROJECT"} {
			if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
				project = v
				break
			}
		}
	}
	if project == "" {
		return nil
	}
	if cl != nil && isPlaceholderProject(cl.Metadata.Project) {
		cl.Metadata.Project = project
	}
	if cl != nil && cl.Metadata.Zone == "" {
		if st != nil && st.Get("GCP_ZONE") != "" {
			cl.Metadata.Zone = st.Get("GCP_ZONE")
		} else if z := strings.TrimSpace(os.Getenv("CLOUDSDK_COMPUTE_ZONE")); z != "" {
			cl.Metadata.Zone = z
		}
		ensureGCPDefaults(cl)
	}
	clients, err := NewClients(ctx, project)
	if err != nil {
		return fmt.Errorf("gcp provider: initializing clients: %w", err)
	}
	p.Clients = clients
	return nil
}

func isPlaceholderProject(p string) bool {
	p = strings.TrimSpace(p)
	return p == "" || p == "YOUR_GCP_PROJECT_ID"
}

func ensureGCPDefaults(cl *intent.Cluster) {
	if cl == nil {
		return
	}
	if cl.Region == "" && cl.Metadata.Zone != "" {
		if idx := strings.LastIndex(cl.Metadata.Zone, "-"); idx > 0 {
			cl.Region = cl.Metadata.Zone[:idx]
		}
	}
	if cl.ClusterSpec == nil {
		cl.ClusterSpec = &intent.ClusterSpec{}
	}
	if cl.ClusterSpec.TmmPool.Name == "" {
		cl.ClusterSpec.TmmPool.Name = "tmm-pool"
	}
	if cl.ClusterSpec.AgentPool.Name == "" {
		cl.ClusterSpec.AgentPool.Name = "agent-pool"
	}
	if cl.Bnk == nil {
		cl.Bnk = &intent.BnkSpec{}
	}
}

// ProvisionUnderlay provisions or reconciles the GCP IaaS underlay (Main/External/Internal VPCs,
// Subnetworks, Firewall Rules, Cloud Router & NAT, GKE Cluster & TMM Multi-NIC NodePool, Jumphost)
// and returns the normalized provider.UnderlayContext for the BNK overlay engine.
func (p *Provider) ProvisionUnderlay(ctx context.Context, cl *intent.Cluster, st *state.State, dryRun bool) (*provider.UnderlayContext, error) {
	if cl == nil {
		return nil, fmt.Errorf("gcp provider: cluster intent is nil")
	}
	ensureGCPDefaults(cl)

	if err := p.ensureClients(ctx, cl, st, dryRun); err != nil {
		return nil, err
	}

	hasFullIntent := cl.Network.Main.VPC != ""
	if p.Clients != nil || (dryRun && hasFullIntent) {
		if err := Phase00Preflight(ctx, cl, st, p.Clients, dryRun); err != nil {
			return nil, err
		}
		if hasFullIntent || (p.Clients != nil && p.Clients.Networks != nil) {
			if err := Phase10Networks(ctx, cl, st, p.Clients, dryRun); err != nil {
				return nil, err
			}
		}
		if hasFullIntent || (p.Clients != nil && p.Clients.Instances != nil) {
			if st != nil {
				if err := Phase15Jumphost(ctx, cl, st, p.Clients, dryRun); err != nil {
					return nil, err
				}
			}
		}
		if hasFullIntent || (p.Clients != nil && p.Clients.GKE != nil) {
			if st != nil {
				if err := Phase20Cluster(ctx, cl, st, p.Clients, dryRun); err != nil {
					return nil, err
				}
			}
		}
		if st != nil && ((dryRun && hasFullIntent) || (p.Clients != nil && p.Clients.Instances != nil && p.Clients.K8s != nil)) {
			if err := Phase25Discover(ctx, cl, st, p.Clients, dryRun); err != nil {
				return nil, err
			}
		}
		if st != nil {
			if err := Phase29ForgeRegister(ctx, cl, st, p.Clients, dryRun); err != nil {
				return nil, err
			}
		}
	}

	u, err := provider.BuildUnderlayFromIntent(cl)
	if err != nil {
		return nil, fmt.Errorf("gcp provider: building underlay context: %w", err)
	}
	u.CloudName = intent.ProviderGCP
	u.DataplaneDriver = provider.DriverVirtio

	if !dryRun && p.Clients != nil {
		if p.Clients.RestConfig != nil {
			u.RestConfig = p.Clients.RestConfig
		} else if p.Clients.GKE != nil && u.ClusterName != "" && cl.Metadata.Zone != "" {
			if err := p.Clients.AttachK8s(ctx, u.ClusterName, cl.Metadata.Zone); err == nil && p.Clients.RestConfig != nil {
				u.RestConfig = p.Clients.RestConfig
			}
		}
	}

	p.attachHooks(u, cl, st, dryRun)
	p.hydrateFromState(u, st)

	if st != nil {
		st.Set("CLOUD_PROVIDER", intent.ProviderGCP)
		st.Set("CLUSTER_NAME", u.ClusterName)
		st.Set("GCP_REGION", u.Region)
		if cl.Metadata.Project != "" {
			st.Set("GCP_PROJECT", cl.Metadata.Project)
		}
		if cl.Metadata.Zone != "" {
			st.Set("GCP_ZONE", cl.Metadata.Zone)
		}
		if cl.Network.Main.VPC != "" {
			st.Set("MAIN_VPC", cl.Network.Main.VPC)
		}
		if cl.Network.Main.Subnet != "" {
			st.Set("MAIN_SUBNET", cl.Network.Main.Subnet)
		}
		if u.NodeCIDR != "" {
			st.Set("MAIN_CIDR", u.NodeCIDR)
		}
		if st.Get("NODE_PRIMARY_IFNAME") == "" {
			st.Set("NODE_PRIMARY_IFNAME", "eth0")
		}
		if st.Get("TMM_EXT_SELFIP") == "" {
			if ext := u.ExternalNetwork(); ext != nil && len(ext.SelfIPs) > 0 {
				st.Set("TMM_EXT_SELFIP", strings.Split(ext.SelfIPs[0], "/")[0])
			}
		}
		if st.Get("TMM_INT_SELFIP") == "" {
			if intl := u.InternalNetwork(); intl != nil && len(intl.SelfIPs) > 0 {
				st.Set("TMM_INT_SELFIP", strings.Split(intl.SelfIPs[0], "/")[0])
			}
		}
		if vip := st.Get("EXT_VIP"); vip != "" {
			st.Set("GATEWAY_VIP", vip)
		} else if vip, err := cl.DefaultVIP(); err == nil && vip != "" {
			st.Set("GATEWAY_VIP", vip)
		}
		if !dryRun {
			_ = st.Save()
		}
	}

	return u, nil
}

// DiscoverUnderlay inspects existing GCP / state.env metadata and constructs an UnderlayContext.
func (p *Provider) DiscoverUnderlay(ctx context.Context, cl *intent.Cluster, st *state.State) (*provider.UnderlayContext, error) {
	if cl == nil {
		cl = &intent.Cluster{Cloud: &intent.CloudSpec{Provider: intent.ProviderGCP}}
	}
	if st != nil {
		if cl.Metadata.Name == "" {
			cl.Metadata.Name = st.Get("CLUSTER_NAME")
		}
		if isPlaceholderProject(cl.Metadata.Project) {
			if v := st.Get("GCP_PROJECT"); !isPlaceholderProject(v) {
				cl.Metadata.Project = v
			}
		}
		if cl.Metadata.Zone == "" {
			cl.Metadata.Zone = st.Get("GCP_ZONE")
		}
	}
	ensureGCPDefaults(cl)
	_ = p.ensureClients(ctx, cl, st, false)

	if st != nil && st.Get("KUBECONFIG") == "" && st.Get("KUBECONFIG_PATH") == "" && p.Clients != nil && p.Clients.GKE != nil && cl.Metadata.Name != "" && cl.Metadata.Zone != "" {
		if kcBytes, err := FetchAndGenerateKubeconfig(ctx, p.Clients, cl); err == nil && len(kcBytes) > 0 {
			kcPath := st.KubeconfigPath()
			if kcPath != "" {
				if werr := WriteKubeconfigFile(kcPath, kcBytes); werr == nil {
					st.Set("KUBECONFIG", kcPath)
					st.Set("KUBECONFIG_PATH", kcPath)
					_ = st.Save()
				}
			}
		}
	}

	u, err := provider.BuildUnderlayFromIntent(cl)
	if err != nil {
		return nil, err
	}
	u.CloudName = intent.ProviderGCP
	u.DataplaneDriver = provider.DriverVirtio
	p.attachHooks(u, cl, st, false)
	p.hydrateFromState(u, st)
	return u, nil
}

// DestroyUnderlay tears down the GCP underlay resources in reverse dependency order.
func (p *Provider) DestroyUnderlay(ctx context.Context, cl *intent.Cluster, st *state.State, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("gcp provider: cluster intent is nil")
	}
	ensureGCPDefaults(cl)
	if err := p.ensureClients(ctx, cl, st, dryRun); err != nil {
		return err
	}

	if st != nil {
		_ = Phase29ForgeRegisterDown(ctx, cl, st, p.Clients, dryRun)
	}

	if p.Clients != nil {
		if err := PhaseDownJumphost(ctx, cl, st, p.Clients, dryRun); err != nil {
			return err
		}
		if !dryRun {
			_ = stripStuckFinalizersWithClient(ctx, p.Clients.Dynamic, bnkconst.InstanceNamespace)
			if err := PhaseDownCluster(ctx, p.Clients, cl, st); err != nil {
				return err
			}
			if err := PhaseDownNetworks(ctx, p.Clients, cl, st); err != nil {
				return err
			}
			if !testing.Testing() {
				_, _ = RestoreGlobalKubeconfig()
			}
		}
	}
	if st != nil && !dryRun {
		if dir := st.Dir(); dir != "" {
			for _, f := range []string{"kubeconfig", "forge_link.json", "jumphost.pem", "bigip_id_rsa", "bigip_id_rsa.pub", "artifacts"} {
				_ = os.RemoveAll(filepath.Join(dir, f))
			}
		}
		for k := range st.All() {
			st.Delete(k)
		}
		st.Set("UNDERLAY_DESTROYED", "true")
		_ = st.Save()
	}
	return nil
}

// OpenJumphostSession opens a runner session to the GCP jumphost or in-cluster probe pod.
func (p *Provider) OpenJumphostSession(ctx context.Context, cl *intent.Cluster, st *state.State) (jumphost.Runner, error) {
	if st == nil {
		return nil, fmt.Errorf("gcp jumphost: state is nil")
	}
	vmName := st.Get("JUMPHOST_VM_NAME")
	if vmName == "" {
		vmName = st.Get("JUMPHOST_NAME")
	}
	ip := st.Get("JUMPHOST_PUBLIC_IP")
	if vmName == "" && ip == "" {
		if st.Get("KUBECONFIG") != "" || st.Get("KUBECONFIG_PATH") != "" {
			if kc := st.KubeconfigPath(); kc != "" {
				return &jumphost.PodExecRunner{Kubeconfig: kc}, nil
			}
		}
		return nil, fmt.Errorf("gcp jumphost: neither JUMPHOST_VM_NAME nor JUMPHOST_PUBLIC_IP found in state")
	}
	zone := st.Get("GCP_ZONE")
	if zone == "" {
		zone = st.Get("JUMPHOST_ZONE")
	}
	if zone == "" && cl != nil {
		zone = cl.Metadata.Zone
	}
	return &jumphost.SSHRunner{
		Opts: jumphost.ProbeOptions{
			Location:   zone,
			VMName:     vmName,
			JumphostIP: ip,
		},
	}, nil
}

func (p *Provider) attachHooks(u *provider.UnderlayContext, cl *intent.Cluster, st *state.State, dryRun bool) {
	u.DiscoverDataplaneResources = func(ctx context.Context, k8sClient kubernetes.Interface, dynClient dynamic.Interface) error {
		if dryRun || k8sClient == nil || len(u.DataplaneNodes) > 0 {
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
					TMMInterface: "1.1",
					Driver:       provider.DriverVirtio,
				},
			}
			if u.InternalNetwork() != nil {
				nics = append(nics, provider.NICAttachment{
					NetworkName:  "internal-nad",
					NICIndex:     2,
					LinuxIfName:  "eth2",
					TMMInterface: "1.2",
					Driver:       provider.DriverVirtio,
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
		gsaEmail := ""
		if st != nil {
			gsaEmail = st.Get("GCP_GSA_EMAIL")
		}
		if gsaEmail == "" {
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
			sa.Annotations["iam.gke.io/gcp-service-account"] = gsaEmail
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
		if dynClient != nil {
			_ = reconcileStaticRouteDrift(ctx, dynClient, cl, st)
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
		if kcPath != "" {
			st.Set("KUBECONFIG", kcPath)
		}
	}
	if kcPath != "" {
		if _, err := os.Stat(kcPath); err != nil {
			if candidate := st.KubeconfigPath(); candidate != "" {
				kcPath = candidate
			}
		}
	} else if st.KubeconfigPath() != "" {
		if _, err := os.Stat(st.KubeconfigPath()); err == nil {
			kcPath = st.KubeconfigPath()
			st.Set("KUBECONFIG", kcPath)
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

	extSelfIP := strings.TrimSpace(st.Get("TMM_EXT_SELFIP"))
	extIf := strings.TrimSpace(st.Get("TMM_EXT_IFACE"))
	if extIf == "" {
		extIf = strings.TrimSpace(st.Get("EXT_IFACE"))
	}
	if extIf == "" {
		extIf = "eth1"
	}
	if extSelfIP != "" && len(u.Networks) > 0 {
		addr := extSelfIP
		if !strings.Contains(addr, "/") {
			addr += "/28"
		}
		u.Networks[0].SelfIPs = []string{addr}
		extCNI, _ := json.Marshal(map[string]any{
			"cniVersion": "0.3.1",
			"name":       "external-net",
			"type":       "host-device",
			"device":     extIf,
			"ipam": map[string]any{
				"type": "static",
				"addresses": []map[string]any{
					{"address": addr},
				},
			},
		})
		u.Networks[0].CNIConfigJSON = string(extCNI)
	}

	intSelfIP := strings.TrimSpace(st.Get("TMM_INT_SELFIP"))
	intIf := strings.TrimSpace(st.Get("TMM_INT_IFACE"))
	if intIf == "" {
		intIf = strings.TrimSpace(st.Get("INT_IFACE"))
	}
	if intIf == "" {
		intIf = "eth2"
	}
	if intSelfIP != "" && len(u.Networks) > 1 {
		addr := intSelfIP
		if !strings.Contains(addr, "/") {
			addr += "/28"
		}
		u.Networks[1].SelfIPs = []string{addr}
		intCNI, _ := json.Marshal(map[string]any{
			"cniVersion": "0.3.1",
			"name":       "internal-net",
			"type":       "host-device",
			"device":     intIf,
			"ipam": map[string]any{
				"type": "static",
				"addresses": []map[string]any{
					{"address": addr},
				},
			},
		})
		u.Networks[1].CNIConfigJSON = string(intCNI)
	}

	if count, err := strconv.Atoi(st.Get("TMM_NODE_COUNT")); err == nil && count > 0 {
		var names []string
		var dpNodes []provider.NodeAttachment
		for i := 0; i < count; i++ {
			idx := strconv.Itoa(i)
			nodeName := st.Get("NODE_" + idx)
			if nodeName == "" {
				continue
			}
			names = append(names, nodeName)
			extIf := st.Get("EXT_IFACE_" + idx)
			if extIf == "" {
				extIf = "eth1"
			}
			nics := []provider.NICAttachment{
				{
					NetworkName:  "external-nad",
					NICIndex:     1,
					MACAddress:   st.Get("EXT_MAC_" + idx),
					PCIAddress:   st.Get("EXT_PCI_" + idx),
					LinuxIfName:  extIf,
					TMMInterface: "1.1",
					Driver:       provider.DriverVirtio,
				},
			}
			if intIP := st.Get("INT_IP_" + idx); intIP != "" {
				intIf := st.Get("INT_IFACE_" + idx)
				if intIf == "" {
					intIf = "eth2"
				}
				nics = append(nics, provider.NICAttachment{
					NetworkName:  "internal-nad",
					NICIndex:     2,
					MACAddress:   st.Get("INT_MAC_" + idx),
					PCIAddress:   st.Get("INT_PCI_" + idx),
					LinuxIfName:  intIf,
					TMMInterface: "1.2",
					Driver:       provider.DriverVirtio,
				})
			}
			dpNodes = append(dpNodes, provider.NodeAttachment{
				NodeName: nodeName,
				NICs:     nics,
			})
		}
		if len(dpNodes) > 0 {
			u.NodeNames = names
			u.DataplaneNodes = dpNodes
		}
	} else {
		tmmNode := st.Get("TMM_NODE")
		if tmmNode == "" {
			tmmNode = st.Get("TMM_NODE_NAME")
		}
		if tmmNode != "" && (st.Get("EXT_IP") != "" || st.Get("TMM_EXT_IFACE") != "") {
			extIf := st.Get("EXT_IFACE")
			if extIf == "" {
				extIf = st.Get("TMM_EXT_IFACE")
			}
			if extIf == "" {
				extIf = "eth1"
			}
			extPCI := st.Get("EXT_PCI")
			if extPCI == "" {
				extPCI = st.Get("TMM_EXT_PCI")
			}
			nics := []provider.NICAttachment{
				{
					NetworkName:  "external-nad",
					NICIndex:     1,
					MACAddress:   st.Get("EXT_MAC"),
					PCIAddress:   extPCI,
					LinuxIfName:  extIf,
					TMMInterface: "1.1",
					Driver:       provider.DriverVirtio,
				},
			}
			if st.Get("INT_IP") != "" || st.Get("TMM_INT_IFACE") != "" {
				intIf := st.Get("INT_IFACE")
				if intIf == "" {
					intIf = st.Get("TMM_INT_IFACE")
				}
				if intIf == "" {
					intIf = "eth2"
				}
				intPCI := st.Get("INT_PCI")
				if intPCI == "" {
					intPCI = st.Get("TMM_INT_PCI")
				}
				nics = append(nics, provider.NICAttachment{
					NetworkName:  "internal-nad",
					NICIndex:     2,
					MACAddress:   st.Get("INT_MAC"),
					PCIAddress:   intPCI,
					LinuxIfName:  intIf,
					TMMInterface: "1.2",
					Driver:       provider.DriverVirtio,
				})
			}
			u.NodeNames = []string{tmmNode}
			u.DataplaneNodes = []provider.NodeAttachment{
				{
					NodeName: tmmNode,
					NICs:     nics,
				},
			}
		}
	}
}
