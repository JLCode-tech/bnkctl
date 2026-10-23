package aws

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/JLCode-tech/bnkctl/pkg/bnkconst"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/k8s"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sagemaker"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// Canonical namespace and resource constants for the AWS BNK underlay phases.
const (
	CertManagerNamespace      = bnkconst.NamespaceCertManager
	CertManagerRelease        = "cert-manager"
	CertManagerChart          = "jetstack/cert-manager"
	CertManagerRepoName       = "jetstack"
	CertManagerRepoURL        = "https://charts.jetstack.io"
	CertManagerVersionDefault = "v1.17.1"

	OperatorNamespace    = bnkconst.CoreNamespace
	InstanceNamespace    = bnkconst.InstanceNamespace
	FarSecretName        = bnkconst.SecretRegistry
	OTELCollectorSvcName = "otel-collector"

	SampleWorkloadNamespace = "default"
	BackendAppName          = "my-app"
	TestClientPodName       = "test-client"

	FloReleaseName  = "flo"
	FloChartOCI     = "oci://repo.f5.com/charts/f5-lifecycle-operator"
	FloChartVersion = "v2.7.5-0.1.33"

	MaintenanceSettingsConfigMap = "f5-maintenance-settings"
	GatewayClassNameSuffix       = "-gatewayclass"
	GatewayName                  = "my-gateway"
	HTTPRouteName                = "backend-route"
	GRPCRouteName                = "grpc-echo-route"
	L4RouteName                  = "tcp-echo-route"
	TLSHostname                  = "tls.bnk.example"
	// #nosec G101 -- Kubernetes Secret name, not a credential.
	TLSCertSecretName = "bnk-gateway-tls-cert"

	// #nosec G101 -- Kubernetes ServiceAccount name, not a credential.
	CNEControllerServiceAccount = bnkconst.CNEControllerServiceAccount
	TMMDaemonSetName            = "f5-tmm"
	TMMContainerName            = "f5-tmm"
	TMMLabelKey                 = "app"
	TMMLabelValue               = "f5-tmm"
)

// PhaseEC2API is the full EC2 surface required by the greenfield AWS provisioning phases.
type PhaseEC2API interface {
	CreateVpc(ctx context.Context, params *ec2.CreateVpcInput, optFns ...func(*ec2.Options)) (*ec2.CreateVpcOutput, error)
	ModifyVpcAttribute(ctx context.Context, params *ec2.ModifyVpcAttributeInput, optFns ...func(*ec2.Options)) (*ec2.ModifyVpcAttributeOutput, error)
	DescribeVpcs(ctx context.Context, params *ec2.DescribeVpcsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error)
	DeleteVpc(ctx context.Context, params *ec2.DeleteVpcInput, optFns ...func(*ec2.Options)) (*ec2.DeleteVpcOutput, error)

	CreateSubnet(ctx context.Context, params *ec2.CreateSubnetInput, optFns ...func(*ec2.Options)) (*ec2.CreateSubnetOutput, error)
	ModifySubnetAttribute(ctx context.Context, params *ec2.ModifySubnetAttributeInput, optFns ...func(*ec2.Options)) (*ec2.ModifySubnetAttributeOutput, error)
	DescribeSubnets(ctx context.Context, params *ec2.DescribeSubnetsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
	DeleteSubnet(ctx context.Context, params *ec2.DeleteSubnetInput, optFns ...func(*ec2.Options)) (*ec2.DeleteSubnetOutput, error)

	CreateInternetGateway(ctx context.Context, params *ec2.CreateInternetGatewayInput, optFns ...func(*ec2.Options)) (*ec2.CreateInternetGatewayOutput, error)
	AttachInternetGateway(ctx context.Context, params *ec2.AttachInternetGatewayInput, optFns ...func(*ec2.Options)) (*ec2.AttachInternetGatewayOutput, error)
	DetachInternetGateway(ctx context.Context, params *ec2.DetachInternetGatewayInput, optFns ...func(*ec2.Options)) (*ec2.DetachInternetGatewayOutput, error)
	DescribeInternetGateways(ctx context.Context, params *ec2.DescribeInternetGatewaysInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInternetGatewaysOutput, error)
	DeleteInternetGateway(ctx context.Context, params *ec2.DeleteInternetGatewayInput, optFns ...func(*ec2.Options)) (*ec2.DeleteInternetGatewayOutput, error)

	AllocateAddress(ctx context.Context, params *ec2.AllocateAddressInput, optFns ...func(*ec2.Options)) (*ec2.AllocateAddressOutput, error)
	ReleaseAddress(ctx context.Context, params *ec2.ReleaseAddressInput, optFns ...func(*ec2.Options)) (*ec2.ReleaseAddressOutput, error)
	DescribeAddresses(ctx context.Context, params *ec2.DescribeAddressesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error)

	CreateNatGateway(ctx context.Context, params *ec2.CreateNatGatewayInput, optFns ...func(*ec2.Options)) (*ec2.CreateNatGatewayOutput, error)
	DescribeNatGateways(ctx context.Context, params *ec2.DescribeNatGatewaysInput, optFns ...func(*ec2.Options)) (*ec2.DescribeNatGatewaysOutput, error)
	DeleteNatGateway(ctx context.Context, params *ec2.DeleteNatGatewayInput, optFns ...func(*ec2.Options)) (*ec2.DeleteNatGatewayOutput, error)

	CreateRouteTable(ctx context.Context, params *ec2.CreateRouteTableInput, optFns ...func(*ec2.Options)) (*ec2.CreateRouteTableOutput, error)
	CreateRoute(ctx context.Context, params *ec2.CreateRouteInput, optFns ...func(*ec2.Options)) (*ec2.CreateRouteOutput, error)
	ReplaceRoute(ctx context.Context, params *ec2.ReplaceRouteInput, optFns ...func(*ec2.Options)) (*ec2.ReplaceRouteOutput, error)
	DeleteRoute(ctx context.Context, params *ec2.DeleteRouteInput, optFns ...func(*ec2.Options)) (*ec2.DeleteRouteOutput, error)
	AssociateRouteTable(ctx context.Context, params *ec2.AssociateRouteTableInput, optFns ...func(*ec2.Options)) (*ec2.AssociateRouteTableOutput, error)
	DisassociateRouteTable(ctx context.Context, params *ec2.DisassociateRouteTableInput, optFns ...func(*ec2.Options)) (*ec2.DisassociateRouteTableOutput, error)
	DescribeRouteTables(ctx context.Context, params *ec2.DescribeRouteTablesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error)
	DeleteRouteTable(ctx context.Context, params *ec2.DeleteRouteTableInput, optFns ...func(*ec2.Options)) (*ec2.DeleteRouteTableOutput, error)

	CreateSecurityGroup(ctx context.Context, params *ec2.CreateSecurityGroupInput, optFns ...func(*ec2.Options)) (*ec2.CreateSecurityGroupOutput, error)
	AuthorizeSecurityGroupIngress(ctx context.Context, params *ec2.AuthorizeSecurityGroupIngressInput, optFns ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error)
	RevokeSecurityGroupIngress(ctx context.Context, params *ec2.RevokeSecurityGroupIngressInput, optFns ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupIngressOutput, error)
	RevokeSecurityGroupEgress(ctx context.Context, params *ec2.RevokeSecurityGroupEgressInput, optFns ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupEgressOutput, error)
	DescribeSecurityGroups(ctx context.Context, params *ec2.DescribeSecurityGroupsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
	DeleteSecurityGroup(ctx context.Context, params *ec2.DeleteSecurityGroupInput, optFns ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error)

	CreateNetworkInterface(ctx context.Context, params *ec2.CreateNetworkInterfaceInput, optFns ...func(*ec2.Options)) (*ec2.CreateNetworkInterfaceOutput, error)
	AttachNetworkInterface(ctx context.Context, params *ec2.AttachNetworkInterfaceInput, optFns ...func(*ec2.Options)) (*ec2.AttachNetworkInterfaceOutput, error)
	DetachNetworkInterface(ctx context.Context, params *ec2.DetachNetworkInterfaceInput, optFns ...func(*ec2.Options)) (*ec2.DetachNetworkInterfaceOutput, error)
	ModifyNetworkInterfaceAttribute(ctx context.Context, params *ec2.ModifyNetworkInterfaceAttributeInput, optFns ...func(*ec2.Options)) (*ec2.ModifyNetworkInterfaceAttributeOutput, error)
	DeleteNetworkInterface(ctx context.Context, params *ec2.DeleteNetworkInterfaceInput, optFns ...func(*ec2.Options)) (*ec2.DeleteNetworkInterfaceOutput, error)
	DescribeNetworkInterfaces(ctx context.Context, params *ec2.DescribeNetworkInterfacesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error)
	AssignPrivateIpAddresses(ctx context.Context, params *ec2.AssignPrivateIpAddressesInput, optFns ...func(*ec2.Options)) (*ec2.AssignPrivateIpAddressesOutput, error)

	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeImages(ctx context.Context, params *ec2.DescribeImagesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error)
	RunInstances(ctx context.Context, params *ec2.RunInstancesInput, optFns ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)

	CreateInstanceConnectEndpoint(ctx context.Context, params *ec2.CreateInstanceConnectEndpointInput, optFns ...func(*ec2.Options)) (*ec2.CreateInstanceConnectEndpointOutput, error)
	DescribeInstanceConnectEndpoints(ctx context.Context, params *ec2.DescribeInstanceConnectEndpointsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstanceConnectEndpointsOutput, error)
	DeleteInstanceConnectEndpoint(ctx context.Context, params *ec2.DeleteInstanceConnectEndpointInput, optFns ...func(*ec2.Options)) (*ec2.DeleteInstanceConnectEndpointOutput, error)

	DescribeKeyPairs(ctx context.Context, params *ec2.DescribeKeyPairsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeKeyPairsOutput, error)
	CreateKeyPair(ctx context.Context, params *ec2.CreateKeyPairInput, optFns ...func(*ec2.Options)) (*ec2.CreateKeyPairOutput, error)
	DeleteKeyPair(ctx context.Context, params *ec2.DeleteKeyPairInput, optFns ...func(*ec2.Options)) (*ec2.DeleteKeyPairOutput, error)

	CreateLaunchTemplate(ctx context.Context, params *ec2.CreateLaunchTemplateInput, optFns ...func(*ec2.Options)) (*ec2.CreateLaunchTemplateOutput, error)
	DescribeLaunchTemplates(ctx context.Context, params *ec2.DescribeLaunchTemplatesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplatesOutput, error)
	DeleteLaunchTemplate(ctx context.Context, params *ec2.DeleteLaunchTemplateInput, optFns ...func(*ec2.Options)) (*ec2.DeleteLaunchTemplateOutput, error)

	CreateTags(ctx context.Context, params *ec2.CreateTagsInput, optFns ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
	DeleteTags(ctx context.Context, params *ec2.DeleteTagsInput, optFns ...func(*ec2.Options)) (*ec2.DeleteTagsOutput, error)

	DescribeVolumes(ctx context.Context, params *ec2.DescribeVolumesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error)
	DeleteVolume(ctx context.Context, params *ec2.DeleteVolumeInput, optFns ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error)
}

// PhaseIAMAPI is the IAM surface required by the greenfield AWS provisioning phases.
type PhaseIAMAPI interface {
	CreateRole(ctx context.Context, params *iam.CreateRoleInput, optFns ...func(*iam.Options)) (*iam.CreateRoleOutput, error)
	GetRole(ctx context.Context, params *iam.GetRoleInput, optFns ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	DeleteRole(ctx context.Context, params *iam.DeleteRoleInput, optFns ...func(*iam.Options)) (*iam.DeleteRoleOutput, error)
	AttachRolePolicy(ctx context.Context, params *iam.AttachRolePolicyInput, optFns ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error)
	DetachRolePolicy(ctx context.Context, params *iam.DetachRolePolicyInput, optFns ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error)
	ListAttachedRolePolicies(ctx context.Context, params *iam.ListAttachedRolePoliciesInput, optFns ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error)
	ListRolePolicies(ctx context.Context, params *iam.ListRolePoliciesInput, optFns ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error)
	DeleteRolePolicy(ctx context.Context, params *iam.DeleteRolePolicyInput, optFns ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error)
	RemoveRoleFromInstanceProfile(ctx context.Context, params *iam.RemoveRoleFromInstanceProfileInput, optFns ...func(*iam.Options)) (*iam.RemoveRoleFromInstanceProfileOutput, error)
	DeleteInstanceProfile(ctx context.Context, params *iam.DeleteInstanceProfileInput, optFns ...func(*iam.Options)) (*iam.DeleteInstanceProfileOutput, error)
	ListInstanceProfilesForRole(ctx context.Context, params *iam.ListInstanceProfilesForRoleInput, optFns ...func(*iam.Options)) (*iam.ListInstanceProfilesForRoleOutput, error)
	CreatePolicy(ctx context.Context, params *iam.CreatePolicyInput, optFns ...func(*iam.Options)) (*iam.CreatePolicyOutput, error)
	GetPolicy(ctx context.Context, params *iam.GetPolicyInput, optFns ...func(*iam.Options)) (*iam.GetPolicyOutput, error)
	DeletePolicy(ctx context.Context, params *iam.DeletePolicyInput, optFns ...func(*iam.Options)) (*iam.DeletePolicyOutput, error)
	ListOpenIDConnectProviders(ctx context.Context, params *iam.ListOpenIDConnectProvidersInput, optFns ...func(*iam.Options)) (*iam.ListOpenIDConnectProvidersOutput, error)
	CreateOpenIDConnectProvider(ctx context.Context, params *iam.CreateOpenIDConnectProviderInput, optFns ...func(*iam.Options)) (*iam.CreateOpenIDConnectProviderOutput, error)
	DeleteOpenIDConnectProvider(ctx context.Context, params *iam.DeleteOpenIDConnectProviderInput, optFns ...func(*iam.Options)) (*iam.DeleteOpenIDConnectProviderOutput, error)
	GetOpenIDConnectProvider(ctx context.Context, params *iam.GetOpenIDConnectProviderInput, optFns ...func(*iam.Options)) (*iam.GetOpenIDConnectProviderOutput, error)
	CreateServiceLinkedRole(ctx context.Context, params *iam.CreateServiceLinkedRoleInput, optFns ...func(*iam.Options)) (*iam.CreateServiceLinkedRoleOutput, error)
}

// PhaseEKSAPI is the EKS surface required by the greenfield AWS provisioning phases.
type PhaseEKSAPI interface {
	CreateCluster(ctx context.Context, params *eks.CreateClusterInput, optFns ...func(*eks.Options)) (*eks.CreateClusterOutput, error)
	DescribeCluster(ctx context.Context, params *eks.DescribeClusterInput, optFns ...func(*eks.Options)) (*eks.DescribeClusterOutput, error)
	DeleteCluster(ctx context.Context, params *eks.DeleteClusterInput, optFns ...func(*eks.Options)) (*eks.DeleteClusterOutput, error)
	CreateNodegroup(ctx context.Context, params *eks.CreateNodegroupInput, optFns ...func(*eks.Options)) (*eks.CreateNodegroupOutput, error)
	DescribeNodegroup(ctx context.Context, params *eks.DescribeNodegroupInput, optFns ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error)
	DeleteNodegroup(ctx context.Context, params *eks.DeleteNodegroupInput, optFns ...func(*eks.Options)) (*eks.DeleteNodegroupOutput, error)
	ListNodegroups(ctx context.Context, params *eks.ListNodegroupsInput, optFns ...func(*eks.Options)) (*eks.ListNodegroupsOutput, error)
	CreateAddon(ctx context.Context, params *eks.CreateAddonInput, optFns ...func(*eks.Options)) (*eks.CreateAddonOutput, error)
	UpdateAddon(ctx context.Context, params *eks.UpdateAddonInput, optFns ...func(*eks.Options)) (*eks.UpdateAddonOutput, error)
	DescribeAddon(ctx context.Context, params *eks.DescribeAddonInput, optFns ...func(*eks.Options)) (*eks.DescribeAddonOutput, error)
	DeleteAddon(ctx context.Context, params *eks.DeleteAddonInput, optFns ...func(*eks.Options)) (*eks.DeleteAddonOutput, error)
}

// SSMAPI is the narrow SSM surface used by Phase 17b.
type SSMAPI interface {
	GetParameter(ctx context.Context, params *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

// SageMakerAPI is the narrow SageMaker surface used by the SageMaker HyperPod phase.
type SageMakerAPI interface {
	CreateCluster(ctx context.Context, params *sagemaker.CreateClusterInput, optFns ...func(*sagemaker.Options)) (*sagemaker.CreateClusterOutput, error)
	DescribeCluster(ctx context.Context, params *sagemaker.DescribeClusterInput, optFns ...func(*sagemaker.Options)) (*sagemaker.DescribeClusterOutput, error)
	DeleteCluster(ctx context.Context, params *sagemaker.DeleteClusterInput, optFns ...func(*sagemaker.Options)) (*sagemaker.DeleteClusterOutput, error)
	ListClusterNodes(ctx context.Context, params *sagemaker.ListClusterNodesInput, optFns ...func(*sagemaker.Options)) (*sagemaker.ListClusterNodesOutput, error)
}

// AutoScalingAPI is the narrow Auto Scaling surface used by Phase 11b.
type AutoScalingAPI interface {
	DescribeAutoScalingGroups(ctx context.Context, params *autoscaling.DescribeAutoScalingGroupsInput, optFns ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error)
	StartInstanceRefresh(ctx context.Context, params *autoscaling.StartInstanceRefreshInput, optFns ...func(*autoscaling.Options)) (*autoscaling.StartInstanceRefreshOutput, error)
	DescribeInstanceRefreshes(ctx context.Context, params *autoscaling.DescribeInstanceRefreshesInput, optFns ...func(*autoscaling.Options)) (*autoscaling.DescribeInstanceRefreshesOutput, error)
}

// PodExecer executes a command inside a pod container and returns its stdout/stderr.
type PodExecer interface {
	ExecInPod(ctx context.Context, namespace, pod, container string, command []string) (stdout, stderr string, err error)
	ExecInPodWithStdin(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader) (stdout, stderr string, err error)
}

// DefaultPodExecer executes commands in pods via SPDY remotecommand.
type DefaultPodExecer struct {
	K8s        kubernetes.Interface
	RESTConfig *rest.Config
}

func (e *DefaultPodExecer) ExecInPod(ctx context.Context, namespace, pod, container string, command []string) (string, string, error) {
	return e.ExecInPodWithStdin(ctx, namespace, pod, container, command, nil)
}

func (e *DefaultPodExecer) ExecInPodWithStdin(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader) (string, string, error) {
	if e.K8s == nil || e.RESTConfig == nil {
		return "", "", fmt.Errorf("pod exec: k8s client or rest.Config is nil")
	}
	req := e.K8s.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     stdin != nil,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(e.RESTConfig, http.MethodPost, req.URL())
	if err != nil {
		return "", "", fmt.Errorf("pod exec new spdy executor: %w", err)
	}
	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	return stdout.String(), stderr.String(), err
}

// KubeconfigTemplateData holds the values needed to render an EKS kubeconfig.
type KubeconfigTemplateData struct {
	ClusterName string
	Endpoint    string
	CAData      string
	Region      string
	RoleARN     string
}

var kubeconfigTmpl = template.Must(template.New("kubeconfig").Parse(`apiVersion: v1
kind: Config
clusters:
- name: {{ .ClusterName }}
  cluster:
    server: {{ .Endpoint }}
    certificate-authority-data: {{ .CAData }}
contexts:
- name: {{ .ClusterName }}
  context:
    cluster: {{ .ClusterName }}
    user: {{ .ClusterName }}
current-context: {{ .ClusterName }}
users:
- name: {{ .ClusterName }}
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: aws
      args:
        - --region
        - {{ .Region }}
        - eks
        - get-token
        - --cluster-name
        - {{ .ClusterName }}
{{- if .RoleARN }}
        - --role-arn
        - {{ .RoleARN }}
{{- end }}
`))

// RenderKubeconfig renders a standalone EKS kubeconfig YAML document.
func RenderKubeconfig(d KubeconfigTemplateData) ([]byte, error) {
	if d.ClusterName == "" {
		return nil, fmt.Errorf("kubeconfig: cluster name is required")
	}
	if d.Endpoint == "" {
		return nil, fmt.Errorf("kubeconfig: endpoint is required")
	}
	if d.CAData == "" {
		return nil, fmt.Errorf("kubeconfig: certificate-authority-data is required")
	}
	if d.Region == "" {
		return nil, fmt.Errorf("kubeconfig: region is required")
	}
	var buf bytes.Buffer
	if err := kubeconfigTmpl.Execute(&buf, d); err != nil {
		return nil, fmt.Errorf("rendering kubeconfig template: %w", err)
	}
	return buf.Bytes(), nil
}

// WriteKubeconfig writes the rendered kubeconfig to path with 0600 permissions.
func WriteKubeconfig(path string, data KubeconfigTemplateData) error {
	raw, err := RenderKubeconfig(data)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating kubeconfig parent dir: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("writing kubeconfig %s: %w", path, err)
	}
	return nil
}

const kubeBackupSuffix = ".awsbnkctl-backup"

func globalKubeconfigPath() (string, error) {
	if env := os.Getenv("KUBECONFIG"); env != "" {
		parts := filepath.SplitList(env)
		if len(parts) > 0 && parts[0] != "" {
			return parts[0], nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("cannot resolve user home directory for ~/.kube/config: %w", err)
	}
	return filepath.Join(home, ".kube", "config"), nil
}

// BackupAndInstallGlobalKubeconfig backs up ~/.kube/config and writes the workspace kubeconfig.
func BackupAndInstallGlobalKubeconfig(data KubeconfigTemplateData) (string, error) {
	globalPath, err := globalKubeconfigPath()
	if err != nil {
		return "", err
	}
	backupPath := globalPath + kubeBackupSuffix

	if info, statErr := os.Stat(globalPath); statErr == nil && !info.IsDir() {
		if _, bErr := os.Stat(backupPath); os.IsNotExist(bErr) {
			// #nosec G304 -- globalPath is resolved from KUBECONFIG or ~/.kube/config
			existing, rErr := os.ReadFile(globalPath)
			if rErr != nil {
				return "", fmt.Errorf("reading existing %s for backup: %w", globalPath, rErr)
			}
			// #nosec G304 G703 -- backupPath is derived from globalPath + kubeBackupSuffix
			if wErr := os.WriteFile(backupPath, existing, 0o600); wErr != nil {
				return "", fmt.Errorf("writing kubeconfig backup %s: %w", backupPath, wErr)
			}
		}
	}

	if err := WriteKubeconfig(globalPath, data); err != nil {
		return "", err
	}
	return globalPath, nil
}

// RestoreGlobalKubeconfig restores ~/.kube/config from its backup if present.
func RestoreGlobalKubeconfig() (bool, error) {
	globalPath, err := globalKubeconfigPath()
	if err != nil {
		return false, err
	}
	backupPath := globalPath + kubeBackupSuffix

	if _, statErr := os.Stat(backupPath); os.IsNotExist(statErr) {
		return false, nil
	}
	if err := os.Rename(backupPath, globalPath); err != nil {
		return false, fmt.Errorf("restoring kubeconfig backup %s -> %s: %w", backupPath, globalPath, err)
	}
	return true, nil
}

func resolveConfigPath(path string, st *state.State) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	if _, err := os.Stat(path); err == nil {
		return path
	}
	if st != nil && st.Path() != "" {
		wsDir := filepath.Dir(st.Path())
		candidate := filepath.Join(wsDir, path)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parentCandidate := filepath.Join(filepath.Dir(wsDir), path)
		if _, err := os.Stat(parentCandidate); err == nil {
			return parentCandidate
		}
	}
	return path
}

func applyRawYAML(ctx context.Context, c *Clients, raw []byte, defaultNamespace string) error {
	if c == nil || c.Dynamic == nil || c.RESTMapper == nil {
		return fmt.Errorf("apply yaml: dynamic client or RESTMapper is nil")
	}
	objs, err := k8s.ParseYAMLStream(raw)
	if err != nil {
		return err
	}
	applier := k8s.NewApplierFromInterfaces(c.Dynamic, c.RESTMapper)
	for _, obj := range objs {
		if obj.GetNamespace() == "" && defaultNamespace != "" {
			obj.SetNamespace(defaultNamespace)
		}
		if _, err := applier.Apply(ctx, obj, true); err != nil {
			return err
		}
	}
	return nil
}

// LookPathFunc abstracts exec.LookPath for unit testing.
type LookPathFunc func(file string) (string, error)

// RequiredBinaries is the list of external CLI tools awsbnkctl needs on PATH.
var RequiredBinaries = []string{"aws"}

func CheckBinaries(look LookPathFunc, bins []string) error {
	if look == nil {
		look = exec.LookPath
	}
	var missing []string
	for _, b := range bins {
		if _, err := look(b); err != nil {
			missing = append(missing, b)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("preflight: required binaries not found on PATH: %s", strings.Join(missing, ", "))
	}
	return nil
}

// PhaseS3API is the S3 surface required by the SageMaker HyperPod phase.
type PhaseS3API interface {
	HeadBucket(ctx context.Context, params *s3.HeadBucketInput, optFns ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	CreateBucket(ctx context.Context, params *s3.CreateBucketInput, optFns ...func(*s3.Options)) (*s3.CreateBucketOutput, error)
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	DeleteBucket(ctx context.Context, params *s3.DeleteBucketInput, optFns ...func(*s3.Options)) (*s3.DeleteBucketOutput, error)
}

// AuthValidator validates AWS caller identity via STS.
type AuthValidator struct {
	STS STSAPI
}

// NewAuthValidator constructs an AuthValidator backed by stsClient.
func NewAuthValidator(stsClient STSAPI) *AuthValidator {
	return &AuthValidator{STS: stsClient}
}

// Validate calls sts:GetCallerIdentity and returns the projected CallerIdentity.
func (v *AuthValidator) Validate(ctx context.Context) (*CallerIdentity, error) {
	if v == nil || v.STS == nil {
		return nil, fmt.Errorf("STS client is nil")
	}
	out, err := v.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, fmt.Errorf("sts:GetCallerIdentity: %w", err)
	}
	return &CallerIdentity{
		Account: aws_string_or_empty(out.Account),
		ARN:     aws_string_or_empty(out.Arn),
		UserID:  aws_string_or_empty(out.UserId),
	}, nil
}

// Phase00Preflight runs pre-flight checks before any AWS resources are created.
func Phase00Preflight(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	return Phase00PreflightWithLookPath(ctx, cl, st, c, dryRun, exec.LookPath)
}

// Phase00PreflightWithLookPath allows tests to inject a custom LookPathFunc.
func Phase00PreflightWithLookPath(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool, look LookPathFunc) error {
	if cl == nil {
		return fmt.Errorf("phase 00 preflight: cluster intent is nil")
	}

	if err := CheckBinaries(look, RequiredBinaries); err != nil {
		return err
	}

	if dryRun {
		return nil
	}

	if c == nil || (c.Auth == nil && c.STS == nil) {
		return fmt.Errorf("phase 00 preflight: AWS clients not initialised")
	}
	var (
		id  *CallerIdentity
		err error
	)
	if c.Auth != nil {
		id, err = c.Auth.Validate(ctx)
	} else {
		id, err = c.CallerIdentity(ctx)
	}
	if err != nil {
		return fmt.Errorf("phase 00 preflight: %w", err)
	}
	if st != nil && id != nil {
		st.Set("AWS_ACCOUNT_ID", id.Account)
		st.Set("AWS_CALLER_ARN", id.ARN)
	}

	if cl.Bnk != nil {
		if jwtFile := resolveConfigPath(cl.Bnk.EffectiveJWTPath(), st); jwtFile != "" {
			info, err := os.Stat(jwtFile)
			if err != nil {
				return fmt.Errorf("phase 00 preflight: bnk jwtFile %q not readable: %w", cl.Bnk.EffectiveJWTPath(), err)
			}
			if info.IsDir() {
				return fmt.Errorf("phase 00 preflight: bnk jwtFile %q is a directory", jwtFile)
			}
			if info.Size() == 0 {
				return fmt.Errorf("phase 00 preflight: bnk jwtFile %q is empty", jwtFile)
			}
		}
	}

	if cl.Forge != nil && cl.Forge.IsEnabled() {
		rawURL := cl.Forge.EffectiveURL()
		u, perr := url.Parse(rawURL)
		if perr != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("phase 00 preflight: forge.url %q is not a valid URL", rawURL)
		}
	}

	return nil
}
