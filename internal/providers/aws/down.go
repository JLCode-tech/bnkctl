package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/JLCode-tech/bnkctl/pkg/bnkconst"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// DiscoveredInterfaces holds the MAC-correlated Linux interface names and PCI
// BDF addresses discovered on an AWS worker node.
type DiscoveredInterfaces struct {
	PrimaryIfName string
	ExtIfName     string
	ExtPCIBDF     string
	IntIfName     string
	IntPCIBDF     string
}

// matchInterfaces correlates a "<ifname> <mac> <pci_bdf>" dump against the EC2
// ENI MAC addresses for the primary, external, and internal NICs.
func matchInterfaces(raw, primaryMAC, extMAC, intMAC string, dual bool) (DiscoveredInterfaces, error) {
	type row struct {
		ifname string
		mac    string
		pci    string
	}
	byMAC := map[string]row{}
	var jsonMap map[string]struct {
		Ifname string `json:"ifname"`
		PCI    string `json:"pci"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &jsonMap); err == nil && len(jsonMap) > 0 {
		for mac, entry := range jsonMap {
			m := strings.ToLower(strings.TrimSpace(mac))
			byMAC[m] = row{
				ifname: entry.Ifname,
				mac:    m,
				pci:    entry.PCI,
			}
		}
	} else {
		for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			r := row{ifname: fields[0], mac: strings.ToLower(fields[1])}
			if len(fields) >= 3 {
				r.pci = fields[2]
			}
			byMAC[r.mac] = r
		}
	}

	var out DiscoveredInterfaces
	if primaryMAC != "" {
		r, ok := byMAC[strings.ToLower(primaryMAC)]
		if !ok {
			return out, fmt.Errorf("primary MAC %s not found in node interface table", primaryMAC)
		}
		out.PrimaryIfName = r.ifname
	}
	if extMAC != "" {
		r, ok := byMAC[strings.ToLower(extMAC)]
		if !ok {
			return out, fmt.Errorf("external MAC %s not found in node interface table", extMAC)
		}
		out.ExtIfName = r.ifname
		out.ExtPCIBDF = r.pci
	}
	if dual && intMAC != "" {
		r, ok := byMAC[strings.ToLower(intMAC)]
		if !ok {
			return out, fmt.Errorf("internal MAC %s not found in node interface table", intMAC)
		}
		out.IntIfName = r.ifname
		out.IntPCIBDF = r.pci
	}
	return out, nil
}

// reconcileControllerIRSA discovers the ServiceAccount used by the
// f5-cne-controller deployment (or falls back to CNEControllerServiceAccount)
// and patches the eks.amazonaws.com/role-arn annotation from state.env.
func reconcileControllerIRSA(ctx context.Context, k8sClient kubernetes.Interface, st *state.State) error {
	if k8sClient == nil || st == nil {
		return nil
	}
	roleARN := strings.TrimSpace(st.Get("CNE_IRSA_ROLE_ARN"))
	if roleARN == "" {
		roleARN = strings.TrimSpace(st.Get("IRSA_ROLE_ARN"))
	}
	if roleARN == "" {
		return nil
	}

	for _, ns := range []string{bnkconst.InstanceNamespace, bnkconst.CoreNamespace} {
		saName := bnkconst.CNEControllerServiceAccount
		if dep, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, "f5-cne-controller", metav1.GetOptions{}); err == nil && dep != nil {
			if configured := strings.TrimSpace(dep.Spec.Template.Spec.ServiceAccountName); configured != "" {
				saName = configured
			}
		}
		sa, err := k8sClient.CoreV1().ServiceAccounts(ns).Get(ctx, saName, metav1.GetOptions{})
		if err != nil {
			continue
		}
		if sa.Annotations == nil {
			sa.Annotations = make(map[string]string)
		}
		if sa.Annotations["eks.amazonaws.com/role-arn"] == roleARN {
			continue
		}
		sa.Annotations["eks.amazonaws.com/role-arn"] = roleARN
		if _, err := k8sClient.CoreV1().ServiceAccounts(ns).Update(ctx, sa, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating ServiceAccount %s/%s IRSA annotation: %w", ns, saName, err)
		}
	}
	return nil
}

// hydrateMissingStateFromAWS discovers AWS underlay resource IDs by cluster name/tags
// when state.env has been cleared (e.g. after a previous partial or wiped down run).
func hydrateMissingStateFromAWS(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients) {
	if cl == nil || cl.Metadata.Name == "" || st == nil || c == nil {
		return
	}
	clusterName := cl.Metadata.Name
	if c.STS != nil && st.Get("AWS_ACCOUNT_ID") == "" {
		if idOut, err := c.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}); err == nil && idOut != nil && idOut.Account != nil {
			st.Set("AWS_ACCOUNT_ID", *idOut.Account)
		}
	}
	if c.PhaseEKS != nil && st.Get("EKS_OIDC_ISSUER") == "" && st.Get("EKS_OIDC_URL") == "" {
		if descOut, err := c.PhaseEKS.DescribeCluster(ctx, &eks.DescribeClusterInput{
			Name: aws.String(clusterName),
		}); err == nil && descOut != nil && descOut.Cluster != nil {
			if descOut.Cluster.Identity != nil && descOut.Cluster.Identity.Oidc != nil && descOut.Cluster.Identity.Oidc.Issuer != nil {
				st.Set("EKS_OIDC_ISSUER", *descOut.Cluster.Identity.Oidc.Issuer)
			}
			if descOut.Cluster.ResourcesVpcConfig != nil {
				if descOut.Cluster.ResourcesVpcConfig.VpcId != nil && st.Get("VPC_ID") == "" {
					st.Set("VPC_ID", *descOut.Cluster.ResourcesVpcConfig.VpcId)
				}
				if descOut.Cluster.ResourcesVpcConfig.ClusterSecurityGroupId != nil && st.Get("EKS_SECURITY_GROUP") == "" {
					st.Set("EKS_SECURITY_GROUP", *descOut.Cluster.ResourcesVpcConfig.ClusterSecurityGroupId)
				}
			}
		}
	}
	if c.PhaseIAM != nil && st.Get("IRSA_POLICY_ARN") == "" {
		if acct := extractAccountIDFromState(st); acct != "" {
			st.Set("IRSA_POLICY_ARN", fmt.Sprintf("arn:aws:iam::%s:policy/%s-cne-controller-policy", acct, clusterName))
		}
	}
	if c.PhaseEC2 == nil {
		return
	}
	vpcID := st.Get("VPC_ID")
	if vpcID == "" {
		if vpcOut, err := c.PhaseEC2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
			Filters: []ec2types.Filter{
				{Name: aws.String("tag:Name"), Values: []string{clusterName + "-vpc"}},
			},
		}); err == nil && vpcOut != nil && len(vpcOut.Vpcs) > 0 && vpcOut.Vpcs[0].VpcId != nil {
			vpcID = *vpcOut.Vpcs[0].VpcId
			st.Set("VPC_ID", vpcID)
		}
	}
	if vpcID == "" {
		return
	}
	if st.Get("IGW_ID") == "" {
		if igwOut, err := c.PhaseEC2.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{
			Filters: []ec2types.Filter{
				{Name: aws.String("attachment.vpc-id"), Values: []string{vpcID}},
			},
		}); err == nil && igwOut != nil && len(igwOut.InternetGateways) > 0 && igwOut.InternetGateways[0].InternetGatewayId != nil {
			st.Set("IGW_ID", *igwOut.InternetGateways[0].InternetGatewayId)
		}
	}
	if st.Get("NAT_GW_ID") == "" {
		if natOut, err := c.PhaseEC2.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{
			Filter: []ec2types.Filter{
				{Name: aws.String("vpc-id"), Values: []string{vpcID}},
			},
		}); err == nil && natOut != nil {
			for _, ng := range natOut.NatGateways {
				if ng.NatGatewayId != nil && ng.State != ec2types.NatGatewayStateDeleted && ng.State != ec2types.NatGatewayStateDeleting {
					st.Set("NAT_GW_ID", *ng.NatGatewayId)
					if st.Get("NAT_EIP_ALLOC_ID") == "" && len(ng.NatGatewayAddresses) > 0 && ng.NatGatewayAddresses[0].AllocationId != nil {
						st.Set("NAT_EIP_ALLOC_ID", *ng.NatGatewayAddresses[0].AllocationId)
					}
					break
				}
			}
		}
	}
	if st.Get("JUMPHOST_ID") == "" && st.Get("JUMPHOST_INSTANCE_ID") == "" {
		if instOut, err := c.PhaseEC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			Filters: []ec2types.Filter{
				{Name: aws.String("vpc-id"), Values: []string{vpcID}},
				{Name: aws.String("tag:Name"), Values: []string{clusterName + "-jumphost"}},
			},
		}); err == nil && instOut != nil {
			for _, res := range instOut.Reservations {
				for _, inst := range res.Instances {
					if inst.InstanceId == nil || (inst.State != nil && inst.State.Name == ec2types.InstanceStateNameTerminated) {
						continue
					}
					for _, tag := range inst.Tags {
						if tag.Key != nil && *tag.Key == "Name" && tag.Value != nil && *tag.Value == clusterName+"-jumphost" {
							st.Set("JUMPHOST_ID", *inst.InstanceId)
							break
						}
					}
				}
			}
		}
	}
	if st.Get("EXT_ENI_ID") == "" && st.Get("INT_ENI_ID") == "" {
		if eniOut, err := c.PhaseEC2.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
			Filters: []ec2types.Filter{
				{Name: aws.String("vpc-id"), Values: []string{vpcID}},
				{Name: aws.String("tag:bnk.f5.com/cluster"), Values: []string{clusterName}},
			},
		}); err == nil && eniOut != nil {
			for _, ni := range eniOut.NetworkInterfaces {
				if ni.NetworkInterfaceId == nil {
					continue
				}
				for _, tag := range ni.TagSet {
					if tag.Key != nil && *tag.Key == "Name" && tag.Value != nil {
						if strings.HasSuffix(*tag.Value, "-ext-eni") && st.Get("EXT_ENI_ID") == "" {
							st.Set("EXT_ENI_ID", *ni.NetworkInterfaceId)
						} else if strings.HasSuffix(*tag.Value, "-int-eni") && st.Get("INT_ENI_ID") == "" {
							st.Set("INT_ENI_ID", *ni.NetworkInterfaceId)
						}
					}
				}
			}
		}
	}
}

// teardownAWSUnderlay tears down AWS underlay state in reverse dependency order:
// CloudNetworkMapping -> IRSA/OIDC -> BIG-IP VE -> IfaceDiscovery -> Jumphost ->
// Secondary ENIs -> TMM Label -> SageMaker -> Nvidia/EBS CSI -> Kubeconfig ->
// Nodegroup -> Forge -> Metrics Server -> VPC CNI -> EKS Cluster -> IAM ->
// Route Tables -> NAT -> IGW -> Subnets -> VPC.
func (p *Provider) teardownAWSUnderlay(ctx context.Context, cl *intent.Cluster, st *state.State, dryRun bool) error {
	if st == nil {
		return nil
	}
	c := p.Clients
	if dryRun {
		if cl != nil {
			_ = Phase09ForgeRegisterDown(ctx, cl, st, c, true)
		}
		return nil
	}

	if c != nil {
		if st.Get("VPC_ID") == "" {
			hydrateMissingStateFromAWS(ctx, cl, st, c)
		}
		if c.K8s == nil {
			if kc := st.KubeconfigPath(); kc != "" {
				_ = c.AttachK8s(kc)
			}
		}
		if c.Dynamic != nil {
			StripStuckFinalizers(ctx, c.Dynamic, InstanceNamespace)
		}

		if err := Phase19CloudNetworkMappingDown(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
		if c.PhaseIAM != nil {
			if err := Phase18IRSAOIDCDown(ctx, cl, st, c, dryRun); err != nil {
				return err
			}
		}
		if c.PhaseEC2 != nil {
			if err := Phase17fBIGIPOnboardDown(ctx, cl, st, c, dryRun); err != nil {
				return err
			}
			if err := Phase17eBIGIPVEDown(ctx, cl, st, c, dryRun); err != nil {
				return err
			}
			if err := Phase17cIfaceDiscoveryDown(ctx, cl, st, c, dryRun); err != nil {
				return err
			}
			if err := Phase17bJumphostDown(ctx, cl, st, c, dryRun); err != nil {
				return err
			}
			if err := Phase17SecondaryENIsDown(ctx, cl, st, c, dryRun); err != nil {
				return err
			}
		}
		if err := Phase16TMMNodeLabelDown(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
		if err := PhaseSageMakerHyperPodDown(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
		if err := Phase14bLBControllerDown(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
		if err := Phase11cNvidiaDevicePluginDown(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
		if err := Phase11bEBSCSIHugepagesDown(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
		if err := Phase11KubeconfigDown(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
		if c.PhaseEKS != nil {
			if err := Phase10NodeGroupDown(ctx, cl, st, c, dryRun); err != nil {
				return err
			}
		}
		if err := Phase09ForgeRegisterDown(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
		if err := Phase08cMetricsServerDown(ctx, cl, st, c, dryRun); err != nil {
			return err
		}
		if c.PhaseEKS != nil {
			if err := Phase08bVPCCNIPrefixDown(ctx, cl, st, c, dryRun); err != nil {
				return err
			}
			if err := Phase08EKSClusterDown(ctx, cl, st, c, dryRun); err != nil {
				return err
			}
		}
		if c.PhaseIAM != nil {
			if err := Phase07IAMDown(ctx, cl, st, c, dryRun); err != nil {
				return err
			}
		}
		if c.PhaseEC2 != nil {
			if err := Phase06RouteTablesDown(ctx, cl, st, c, dryRun); err != nil {
				return err
			}
			if err := Phase05NATDown(ctx, cl, st, c, dryRun); err != nil {
				return err
			}
			if err := Phase04IGWDown(ctx, cl, st, c, dryRun); err != nil {
				return err
			}
			if err := Phase03SubnetsDown(ctx, cl, st, c, dryRun); err != nil {
				return err
			}
			if err := Phase02VPCDown(ctx, cl, st, c, dryRun); err != nil {
				return err
			}
		}
	} else if cl != nil {
		_ = Phase17cIfaceDiscoveryDown(ctx, cl, st, nil, dryRun)
		_ = Phase11KubeconfigDown(ctx, cl, st, nil, dryRun)
		if err := Phase09ForgeRegisterDown(ctx, cl, st, nil, dryRun); err != nil {
			return err
		}
		_ = Phase08cMetricsServerDown(ctx, cl, st, nil, dryRun)
	}

	for _, key := range []string{
		"CLOUD_PROVIDER",
		"CLUSTER_NAME",
		"AWS_REGION",
		"AWS_ACCOUNT_ID",
		"VPC_CIDR",
		"SECONDARY_ENI_IDS",
		"EXT_ENI_ID",
		"INT_ENI_ID",
		"JUMPHOST_INSTANCE_ID",
		"JUMPHOST_PUBLIC_IP",
		"JUMPHOST_MGMT_ENI_ID",
		"JUMPHOST_MGMT_ENI_IP",
		"JUMPHOST_BNK_EXT_ENI_IP",
		"JUMPHOST_INSTANCE_TYPE",
		"NODEGROUP_NAME",
		"NODEGROUP_DEFAULT_ARN",
		"NODEGROUP_DEFAULT_NAME",
		"NODEGROUP_DEFAULT_STATUS",
		"EKS_CLUSTER_ARN",
		"EKS_OIDC_ISSUER",
		"EKS_OIDC_URL",
		"EKS_CA",
		"EKS_ENDPOINT",
		"EKS_VERSION",
		"IRSA_SA_APPLIED_AT",
		"OIDC_PROVIDER_ARN",
		"NAT_GATEWAY_ID",
		"IGW_ID",
		"SUBNET_EXT_ID",
		"SUBNET_INT_ID",
		"VPC_ID",
		"METRICS_SERVER_INSTALLED",
		"METRICS_SERVER_ADDON",
		"NODE_PRIMARY_IFNAME",
		"EXTERNAL_IFNAME",
		"EXT_IFNAME",
		"EXTERNAL_PCI",
		"EXT_PCI_BDF",
		"INTERNAL_IFNAME",
		"INT_IFNAME",
		"INTERNAL_PCI",
		"INT_PCI_BDF",
		"CLOUD_HOST_DEVICE_NAME",
		"CLOUD_HOST_DEVICE_TAG",
		"IFACE_DISCOVERY_AT",
		"EXTERNAL_NAD",
		"INTERNAL_NAD",
		"TMM_EXT_SELFIP",
		"TMM_INT_SELFIP",
		"TMM_SELFIP_PREFIXLEN",
		"BNK_MANIFEST_VERSION",
		"BNK_NETWORK_MODEL",
		"CERT_MANAGER_INSTALLED",
		"MULTUS_INSTALLED",
		"FLO_INSTALLED",
		"CNE_DEPLOYED",
		"BNK_DEPLOYED",
		"BNK_PREREQS_APPLIED",
		"CERT_MANAGER_VERSION",
		"MULTUS_VERSION",
		"OPERATOR_NS",
		"INSTANCE_NS",
		"BNK_NAMESPACES_CREATED",
		"BNK_FAR_SECRET_NAME",
		"BNK_LICENSE_JWT_SECRET",
		"CNE_SA_NAME",
		"BNK_SELFSIGNED_ISSUER",
		"BNK_CA_CERT_NAME",
		"BNK_CA_SECRET_NAME",
		"BNK_CA_ISSUER",
		"OTEL_SVR_CERT_NAME",
		"OTEL_F5ING_CERT_NAME",
		"FLO_VERSION",
		"FLO_RELEASE_NAME",
		"FLO_NAMESPACE",
		"FLO_INSTALLED_AT",
		"LICENSE_CRD_READY_AT",
		"LICENSE_NAME",
		"LICENSE_APPLIED_AT",
		"BNK_API_GENERATION",
		"NADS_APPLIED_AT",
		"INFRA_APPLIED_AT",
		"CNEINSTANCE_NAME",
		"CNEINSTANCE_APPLIED_AT",
		"CNEINSTANCE_RECONCILE_STARTED_AT",
		"CNEINSTANCE_READY_AT",
		"DSSM_INSECURE_OVERLAY_APPLIED_AT",
		"TMM_LOG_STREAM_ENABLED_AT",
		"GATEWAYCLASS_NAME",
	} {
		st.Delete(key)
	}
	if dir := st.Dir(); dir != "" {
		for _, f := range []string{"kubeconfig", "portable-kubeconfig.yaml", "forge_link.json", "jumphost.pem", "bigip_id_rsa", "bigip_id_rsa.pub", "artifacts"} {
			_ = os.RemoveAll(filepath.Join(dir, f))
		}
	}
	for k := range st.All() {
		st.Delete(k)
	}
	st.Set("UNDERLAY_DESTROYED", "true")
	return st.Save()
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
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "infras"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "gatewaysettings"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "egressgateways"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "l4routes"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "infras"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "gatewaysettings"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "egressgateways"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "l4routes"},
		{Group: "fic.f5.com", Version: "v1", Resource: "ipams"},
		{Group: "fic.f5.com", Version: "v1", Resource: "ipamranges"},
		{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aigatewayprofiles"},
		{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aiproviders"},
		{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "f5epps"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-big-analyzers"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-big-log-hslpubs"},
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
}
