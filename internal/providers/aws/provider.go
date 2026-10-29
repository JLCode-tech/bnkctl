package aws

import (
	"context"
	"encoding/json"
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
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Provider implements provider.Provider for AWS (EKS + VPC + secondary ENIs + Pod Identity / IRSA).
type Provider struct {
	Clients *Clients
}

// NewProvider returns a new AWS underlay provider.
func NewProvider() *Provider {
	return &Provider{}
}

// Name returns the canonical cloud provider identifier.
func (p *Provider) Name() string {
	return intent.ProviderAWS
}

// ensureClients lazily initializes live AWS SDK clients when p.Clients is nil,
// not in dryRun, and authentication is not bypassed.
func (p *Provider) ensureClients(ctx context.Context, cl *intent.Cluster, st *state.State, dryRun bool) error {
	if p.Clients != nil || dryRun || ShouldSkipAuth() || (testing.Testing() && os.Getenv("BNKCTL_FORCE_LIVE_AUTH") != "1") {
		return nil
	}
	var region, profile, accountID string
	if cl != nil {
		region = cl.EffectiveRegion()
		profile = strings.TrimSpace(cl.Metadata.Profile)
	}
	if st != nil {
		if region == "" {
			region = strings.TrimSpace(st.Get("AWS_REGION"))
		}
		if profile == "" {
			profile = strings.TrimSpace(st.Get("AWS_PROFILE"))
		}
		accountID = extractAccountIDFromState(st)
	}
	if region == "" {
		for _, envKey := range []string{"AWS_REGION", "AWS_DEFAULT_REGION"} {
			if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
				region = v
				break
			}
		}
	}
	if region == "" {
		return nil
	}
	profile = ResolveAWSProfile(ctx, region, profile, accountID)
	if profile != "" && os.Getenv("AWS_PROFILE") == "" && !testing.Testing() {
		_ = os.Setenv("AWS_PROFILE", profile)
	}
	clients, err := NewClients(ctx, Options{
		Region:  region,
		Profile: profile,
	})
	if err != nil {
		return fmt.Errorf("aws provider: initializing SDK clients: %w", err)
	}
	p.Clients = clients
	return nil
}

// extractAccountIDFromState returns AWS_ACCOUNT_ID from state.env or extracts
// the 12-digit AWS account ID from any ARN stored in state.env.
func extractAccountIDFromState(st *state.State) string {
	if st == nil {
		return ""
	}
	if acct := strings.TrimSpace(st.Get("AWS_ACCOUNT_ID")); acct != "" {
		return acct
	}
	for _, v := range st.All() {
		if !strings.HasPrefix(v, "arn:aws") {
			continue
		}
		parts := strings.Split(v, ":")
		if len(parts) >= 5 && len(parts[4]) == 12 {
			allDigits := true
			for _, ch := range parts[4] {
				if ch < '0' || ch > '9' {
					allDigits = false
					break
				}
			}
			if allDigits {
				return parts[4]
			}
		}
	}
	return ""
}

// ProvisionUnderlay provisions or reconciles the AWS IaaS underlay (VPC, Subnets, IGW, NAT,
// Route Tables, IAM Roles, EKS Cluster, Nodegroup, Secondary ENIs, Jumphost, OIDC/Pod Identity)
// and returns the normalized provider.UnderlayContext for the BNK overlay engine.
func (p *Provider) ProvisionUnderlay(ctx context.Context, cl *intent.Cluster, st *state.State, dryRun bool) (*provider.UnderlayContext, error) {
	if cl == nil {
		return nil, fmt.Errorf("aws provider: cluster intent is nil")
	}
	if err := p.ensureClients(ctx, cl, st, dryRun); err != nil {
		return nil, err
	}

	hasFullIntent := len(cl.Network.Subnets.PublicList) > 0 && cl.ClusterSpec != nil && len(cl.ClusterSpec.NodeGroups) > 0
	if p.Clients != nil || (dryRun && hasFullIntent) {
		if err := p.runGreenfieldPhases(ctx, cl, st, dryRun, hasFullIntent); err != nil {
			return nil, err
		}
	}

	u, err := provider.BuildUnderlayFromIntent(cl)
	if err != nil {
		return nil, fmt.Errorf("aws provider: building underlay context: %w", err)
	}
	u.CloudName = intent.ProviderAWS
	u.DataplaneDriver = provider.DriverDPDKPCI

	if !dryRun && p.Clients != nil && p.Clients.EKS != nil && u.ClusterName != "" {
		if ci, err := p.Clients.DescribeCluster(ctx, u.ClusterName); err == nil && ci != nil {
			if kc, err := p.Clients.KubeconfigFromCluster(ci); err == nil && kc != "" {
				u.Kubeconfig = []byte(kc)
				if st != nil && st.Get("KUBECONFIG") == "" && st.Path() != "" {
					kcPath := st.KubeconfigPath()
					if err := os.MkdirAll(filepath.Dir(kcPath), 0o750); err == nil {
						if err := os.WriteFile(kcPath, []byte(kc), 0o600); err == nil {
							st.Set("KUBECONFIG", kcPath)
							st.Set("KUBECONFIG_PATH", kcPath)
							u.KubeconfigPath = kcPath
						}
					}
				}
			}
			if st != nil && ci.OIDCIssuer != "" {
				st.Set("EKS_OIDC_ISSUER", ci.OIDCIssuer)
			}
		}
	}

	if p.Clients != nil && p.Clients.RESTConfig != nil {
		u.RestConfig = p.Clients.RESTConfig
	}

	p.attachHooks(u, cl, st, dryRun)
	p.hydrateFromState(u, st)

	if st != nil {
		st.Set("CLOUD_PROVIDER", intent.ProviderAWS)
		st.Set("CLUSTER_NAME", u.ClusterName)
		st.Set("AWS_REGION", u.Region)
		if u.NodeCIDR != "" {
			st.Set("VPC_CIDR", u.NodeCIDR)
		}
		if st.Get("NODE_PRIMARY_IFNAME") == "" {
			st.Set("NODE_PRIMARY_IFNAME", "ens5")
		}
		if ext := u.ExternalNetwork(); ext != nil && len(ext.SelfIPs) > 0 {
			st.Set("TMM_EXT_SELFIP", strings.Split(ext.SelfIPs[0], "/")[0])
		}
		if intl := u.InternalNetwork(); intl != nil && len(intl.SelfIPs) > 0 {
			st.Set("TMM_INT_SELFIP", strings.Split(intl.SelfIPs[0], "/")[0])
		}
		if !dryRun {
			_ = st.Save()
		}
	}

	return u, nil
}

func (p *Provider) runGreenfieldPhases(ctx context.Context, cl *intent.Cluster, st *state.State, dryRun, hasFullIntent bool) error {
	c := p.Clients
	if dryRun || (c != nil && (c.Auth != nil || c.STS != nil)) {
		if err := Phase00Preflight(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
	}
	if dryRun && hasFullIntent {
		fmt.Fprintf(os.Stderr, "[phase 02] [dry-run] would ensure VPC %q (%s) in %s\n", cl.Metadata.Name+"-vpc", cl.Network.VPCCidr, cl.EffectiveRegion())
		fmt.Fprintf(os.Stderr, "[phase 03-06] [dry-run] would ensure subnets (public=%d, private=%d, datapath=ext+int), IGW, NAT Gateway, and Route Tables\n",
			len(cl.Network.Subnets.PublicList), len(cl.Network.Subnets.PrivateList))
		fmt.Fprintf(os.Stderr, "[phase 07] [dry-run] would ensure IAM roles (EKS cluster role, nodegroup role, BNK controller IRSA/Pod Identity role)\n")
		if cl.ClusterSpec != nil && len(cl.ClusterSpec.NodeGroups) > 0 {
			ng := cl.ClusterSpec.NodeGroups[0]
			fmt.Fprintf(os.Stderr, "[phase 08-11] [dry-run] would ensure EKS cluster %q (k8s=%s) and nodegroup %q (%s, desired=%d)\n",
				cl.Metadata.Name, cl.ClusterSpec.KubernetesVersion, ng.Name, ng.InstanceType, ng.DesiredSize)
		}
		if cl.IsBNKPattern() && cl.Network.DataPath != nil {
			fmt.Fprintf(os.Stderr, "[phase 17] [dry-run] would attach secondary ENIs (external=%s, internal=%s), provision SSM jumphost, and discover PCIe/MACs\n",
				cl.Network.DataPath.External.CIDR, cl.Network.DataPath.Internal.CIDR)
		}
		fmt.Fprintf(os.Stderr, "[phase 18-19] [dry-run] would configure EKS OIDC / Pod Identity and CloudNetwork mappings\n")
	}
	if dryRun || (c != nil && c.PhaseEC2 != nil) {
		if err := Phase02VPC(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
		if len(cl.Network.Subnets.PublicList) > 0 || len(cl.Network.Subnets.PrivateList) > 0 {
			if err := Phase03Subnets(ctx, cl, st, c, dryRun); err != nil {
				return err
			}
			if err := Phase04IGW(ctx, cl, st, c, dryRun); err != nil {
				return err
			}
			if err := Phase05NAT(ctx, cl, st, c, dryRun); err != nil {
				return err
			}
			if err := Phase06RouteTables(ctx, cl, st, c, dryRun); err != nil {
				return err
			}
		}
	}
	if dryRun || (c != nil && c.PhaseIAM != nil) {
		if err := Phase07IAM(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
	}
	if (dryRun && hasFullIntent) || (c != nil && c.PhaseEKS != nil && len(cl.Network.Subnets.PublicList)+len(cl.Network.Subnets.PrivateList) >= 2) {
		if err := Phase08EKSCluster(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
		if err := Phase08bVPCCNIPrefix(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
	}
	if cl.Forge != nil && cl.Forge.IsEnabled() {
		if err := Phase09ForgeRegister(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
	}
	if (dryRun && hasFullIntent) || (c != nil && c.PhaseEKS != nil && cl.ClusterSpec != nil && len(cl.ClusterSpec.NodeGroups) > 0) {
		if err := Phase10NodeGroup(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
	}
	if st != nil && ((dryRun && hasFullIntent) || (st.Get("EKS_CLUSTER_ENDPOINT") != "" && st.Get("EKS_CLUSTER_CA") != "")) {
		if err := Phase11Kubeconfig(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
	}
	if c != nil && c.Dynamic != nil && c.RESTMapper != nil {
		if err := Phase08cMetricsServer(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
	}
	if c != nil && c.PhaseEKS != nil && c.Dynamic != nil && c.RESTMapper != nil {
		if err := Phase11bEBSCSIHugepages(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
	}
	if cl.HasGPUNodeGroups() && (dryRun || (c != nil && c.Dynamic != nil && c.RESTMapper != nil)) {
		if err := Phase11cNvidiaDevicePlugin(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
	}
	if cl.AI != nil && cl.AI.IsSageMakerHyperPod() && (dryRun || (c != nil && c.SageMaker != nil)) {
		if err := PhaseSageMakerHyperPod(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
	}
	if c != nil && c.K8s != nil {
		if err := Phase16TMMNodeLabel(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
	}
	if cl.IsBNKPattern() && cl.Network.DataPath != nil && ((dryRun && hasFullIntent) || (c != nil && c.PhaseEC2 != nil && st != nil && st.Get("BNK_EXT_SUBNET") != "")) {
		if err := Phase17SecondaryENIs(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
		if (dryRun && hasFullIntent) || (c != nil && c.SSM != nil) {
			if err := Phase17bJumphost(ctx, cl, st, c, dryRun); err != nil {
				return err
			}
		}
		if (dryRun && hasFullIntent) || (c != nil && c.K8s != nil && c.Dynamic != nil && c.RESTMapper != nil) {
			if err := Phase17cIfaceDiscovery(ctx, cl, st, c, dryRun); err != nil {
				return err
			}
		}
	}
	if cl.IsBIGIPVEPattern() && (dryRun || (c != nil && c.PhaseEC2 != nil)) {
		if err := Phase17eBIGIPVE(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
		if err := Phase17fBIGIPOnboard(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
	}
	if (dryRun && hasFullIntent) || (c != nil && c.PhaseIAM != nil && st != nil && st.Get("EKS_OIDC_ISSUER") != "") {
		if err := Phase18IRSAOIDC(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
	}
	if st != nil && ((dryRun && hasFullIntent) || st.Get("BNK_EXT_SUBNET") != "") {
		if err := Phase19CloudNetworkMapping(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
	}
	return nil
}

// DiscoverUnderlay inspects existing AWS / state.env metadata and constructs an UnderlayContext.
func (p *Provider) DiscoverUnderlay(ctx context.Context, cl *intent.Cluster, st *state.State) (*provider.UnderlayContext, error) {
	if cl == nil {
		cl = &intent.Cluster{Cloud: &intent.CloudSpec{Provider: intent.ProviderAWS}}
	}
	if st != nil {
		if cl.Metadata.Name == "" {
			cl.Metadata.Name = st.Get("CLUSTER_NAME")
		}
		if cl.Metadata.Region == "" && cl.Metadata.Location == "" {
			cl.Metadata.Region = st.Get("AWS_REGION")
		}
	}
	_ = p.ensureClients(ctx, cl, st, false)

	u, err := provider.BuildUnderlayFromIntent(cl)
	if err != nil {
		return nil, err
	}
	u.CloudName = intent.ProviderAWS
	u.DataplaneDriver = provider.DriverDPDKPCI

	if st != nil && st.Get("KUBECONFIG") == "" && p.Clients != nil && p.Clients.EKS != nil && u.ClusterName != "" {
		if ci, err := p.Clients.DescribeCluster(ctx, u.ClusterName); err == nil && ci != nil {
			if kc, err := p.Clients.KubeconfigFromCluster(ci); err == nil && kc != "" {
				u.Kubeconfig = []byte(kc)
				if st.Path() != "" {
					kcPath := st.KubeconfigPath()
					if err := os.MkdirAll(filepath.Dir(kcPath), 0o750); err == nil {
						if err := os.WriteFile(kcPath, []byte(kc), 0o600); err == nil {
							st.Set("KUBECONFIG", kcPath)
							st.Set("KUBECONFIG_PATH", kcPath)
							_ = st.Save()
						}
					}
				}
			}
		}
	}

	p.attachHooks(u, cl, st, false)
	p.hydrateFromState(u, st)
	return u, nil
}

// DestroyUnderlay tears down the AWS underlay resources in reverse dependency order.
func (p *Provider) DestroyUnderlay(ctx context.Context, cl *intent.Cluster, st *state.State, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("aws provider: cluster intent is nil")
	}
	if err := p.ensureClients(ctx, cl, st, dryRun); err != nil {
		return err
	}
	return p.teardownAWSUnderlay(ctx, cl, st, dryRun)
}

// OpenJumphostSession opens an interactive or command runner session to the AWS jumphost.
func (p *Provider) OpenJumphostSession(ctx context.Context, cl *intent.Cluster, st *state.State) (jumphost.Runner, error) {
	if st == nil {
		return nil, fmt.Errorf("aws jumphost: state is nil")
	}
	vmID := st.Get("JUMPHOST_INSTANCE_ID")
	if vmID == "" {
		vmID = st.Get("JUMPHOST_ID")
	}
	ip := st.Get("JUMPHOST_PUBLIC_IP")
	if ip == "" {
		ip = st.Get("JUMPHOST_EXT_IP")
	}
	if vmID == "" && ip == "" {
		if st.Get("KUBECONFIG") != "" || st.Get("KUBECONFIG_PATH") != "" {
			if kc := st.KubeconfigPath(); kc != "" {
				return &jumphost.PodExecRunner{Kubeconfig: kc}, nil
			}
		}
		return nil, fmt.Errorf("aws jumphost: neither JUMPHOST_INSTANCE_ID nor JUMPHOST_PUBLIC_IP found in state")
	}
	region := st.Get("AWS_REGION")
	if region == "" && cl != nil {
		region = cl.EffectiveRegion()
	}
	return &jumphost.SSHRunner{
		Opts: jumphost.ProbeOptions{
			Location:   region,
			VMName:     vmID,
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
		extIfName := "ens8"
		extPCI := ""
		intIfName := "ens7"
		intPCI := ""
		if st != nil {
			if v := st.Get("EXTERNAL_IFNAME"); v != "" {
				extIfName = v
			} else if v := st.Get("EXT_IFNAME"); v != "" {
				extIfName = v
			}
			extPCI = st.Get("EXTERNAL_PCI")
			if extPCI == "" {
				extPCI = st.Get("EXT_PCI_BDF")
			}
			if v := st.Get("INTERNAL_IFNAME"); v != "" {
				intIfName = v
			} else if v := st.Get("INT_IFNAME"); v != "" {
				intIfName = v
			}
			intPCI = st.Get("INTERNAL_PCI")
			if intPCI == "" {
				intPCI = st.Get("INT_PCI_BDF")
			}
		}
		if (extPCI == "" || (u.InternalNetwork() != nil && intPCI == "")) && p.Clients != nil && p.Clients.PhaseEC2 != nil && u.ClusterName != "" {
			if out, derr := p.Clients.PhaseEC2.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
				Filters: []ec2types.Filter{
					{Name: aws.String("tag:awsbnkctl:cluster"), Values: []string{u.ClusterName}},
				},
			}); derr == nil && out != nil {
				for _, ni := range out.NetworkInterfaces {
					dpRole := ""
					ifName := ""
					pciAddr := ""
					for _, t := range ni.TagSet {
						if t.Key == nil || t.Value == nil {
							continue
						}
						switch *t.Key {
						case "bnk.f5.com/datapath":
							dpRole = *t.Value
						case "bnk.f5.com/ifname":
							ifName = *t.Value
						case hostDeviceTagKey:
							if ifName == "" && *t.Value != "external" && *t.Value != "internal" {
								ifName = *t.Value
							}
						case "bnk.f5.com/pci":
							pciAddr = *t.Value
						}
					}
					switch dpRole {
					case "external":
						if ifName != "" {
							extIfName = ifName
						}
						if pciAddr != "" && extPCI == "" {
							extPCI = pciAddr
						}
					case "internal":
						if ifName != "" {
							intIfName = ifName
						}
						if pciAddr != "" && intPCI == "" {
							intPCI = pciAddr
						}
					}
				}
			}
		}
		var names []string
		var dpNodes []provider.NodeAttachment
		for _, node := range nodes.Items {
			names = append(names, node.Name)
			nics := []provider.NICAttachment{
				{
					NetworkName:  "external-nad",
					NICIndex:     1,
					LinuxIfName:  extIfName,
					PCIAddress:   extPCI,
					TMMInterface: "1.1",
					Driver:       provider.DriverDPDKPCI,
				},
			}
			if u.InternalNetwork() != nil {
				nics = append(nics, provider.NICAttachment{
					NetworkName:  "internal-nad",
					NICIndex:     2,
					LinuxIfName:  intIfName,
					PCIAddress:   intPCI,
					TMMInterface: "1.2",
					Driver:       provider.DriverDPDKPCI,
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
		return reconcileControllerIRSA(ctx, k8sClient, st)
	}
	u.PostActivationHook = func(ctx context.Context, k8sClient kubernetes.Interface, dynClient dynamic.Interface) error {
		if dryRun || k8sClient == nil {
			return nil
		}
		if cm, err := k8sClient.CoreV1().ConfigMaps(bnkconst.InstanceNamespace).Get(ctx, "tmm-init", metav1.GetOptions{}); err == nil && cm != nil {
			if cm.Data == nil {
				cm.Data = make(map[string]string)
			}
			cm.Data["user_conf.tcl"] = "bigdb tm.tcpsegmentationoffload disable\nbigdb tm.tcplargereceiveoffload disable\npool pool1 {\n}\n"
			_, _ = k8sClient.CoreV1().ConfigMaps(bnkconst.InstanceNamespace).Update(ctx, cm, metav1.UpdateOptions{})
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

	extPCI := st.Get("EXTERNAL_PCI")
	extIf := st.Get("EXTERNAL_IFNAME")
	if extIf == "" {
		extIf = "ens8"
	}
	intPCI := st.Get("INTERNAL_PCI")
	intIf := st.Get("INTERNAL_IFNAME")
	if intIf == "" {
		intIf = "ens7"
	}
	if extPCI != "" && len(u.Networks) > 0 {
		extCNI, _ := json.Marshal(map[string]any{
			"cniVersion": "0.3.1",
			"name":       "external-net",
			"type":       "host-device",
			"pciBusID":   extPCI,
		})
		u.Networks[0].CNIConfigJSON = string(extCNI)
	}
	if intPCI != "" && len(u.Networks) > 1 {
		intCNI, _ := json.Marshal(map[string]any{
			"cniVersion": "0.3.1",
			"name":       "internal-net",
			"type":       "host-device",
			"pciBusID":   intPCI,
		})
		u.Networks[1].CNIConfigJSON = string(intCNI)
	}
	if extPCI != "" || intPCI != "" {
		nics := []provider.NICAttachment{
			{
				NetworkName:  "external-net",
				NICIndex:     1,
				PCIAddress:   extPCI,
				LinuxIfName:  extIf,
				TMMInterface: "1.1",
				Driver:       provider.DriverDPDKPCI,
			},
		}
		if intPCI != "" || len(u.Networks) > 1 {
			nics = append(nics, provider.NICAttachment{
				NetworkName:  "internal-net",
				NICIndex:     2,
				PCIAddress:   intPCI,
				LinuxIfName:  intIf,
				TMMInterface: "1.2",
				Driver:       provider.DriverDPDKPCI,
			})
		}
		nodeName := st.Get("TMM_NODE_NAME")
		if nodeName == "" {
			nodeName = "tmm-node-0"
		}
		u.DataplaneNodes = []provider.NodeAttachment{
			{
				NodeName: nodeName,
				NICs:     nics,
			},
		}
	}
}
