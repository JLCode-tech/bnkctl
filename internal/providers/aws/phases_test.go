package aws

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JLCode-tech/bnkctl/internal/providers/aws/tags"
	"github.com/JLCode-tech/bnkctl/pkg/forge"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

type fakeNotFoundErr struct{}

func (fakeNotFoundErr) Error() string                 { return "ResourceNotFoundException: not found" }
func (fakeNotFoundErr) ErrorCode() string             { return "ResourceNotFoundException" }
func (fakeNotFoundErr) ErrorMessage() string          { return "not found" }
func (fakeNotFoundErr) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

type fakePhaseSTS struct{}

func (f *fakePhaseSTS) GetCallerIdentity(_ context.Context, _ *sts.GetCallerIdentityInput, _ ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return &sts.GetCallerIdentityOutput{
		Account: aws.String("123456789012"),
		Arn:     aws.String("arn:aws:iam::123456789012:user/tester"),
	}, nil
}

type fakePhaseEC2 struct {
	PhaseEC2API
	vpcDeleted         bool
	instanceTerminated bool
}

func (f *fakePhaseEC2) DescribeAvailabilityZones(_ context.Context, _ *ec2.DescribeAvailabilityZonesInput, _ ...func(*ec2.Options)) (*ec2.DescribeAvailabilityZonesOutput, error) {
	return &ec2.DescribeAvailabilityZonesOutput{
		AvailabilityZones: []ec2types.AvailabilityZone{
			{ZoneName: aws.String("ap-southeast-2a"), State: ec2types.AvailabilityZoneStateAvailable},
		},
	}, nil
}

func (f *fakePhaseEC2) DescribeVpcs(_ context.Context, _ *ec2.DescribeVpcsInput, _ ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	if f.vpcDeleted {
		return nil, fakeNotFoundErr{}
	}
	return &ec2.DescribeVpcsOutput{
		Vpcs: []ec2types.Vpc{{VpcId: aws.String("vpc-12345")}},
	}, nil
}

func (f *fakePhaseEC2) CreateVpc(_ context.Context, _ *ec2.CreateVpcInput, _ ...func(*ec2.Options)) (*ec2.CreateVpcOutput, error) {
	return &ec2.CreateVpcOutput{
		Vpc: &ec2types.Vpc{VpcId: aws.String("vpc-12345")},
	}, nil
}

func (f *fakePhaseEC2) ModifyVpcAttribute(_ context.Context, _ *ec2.ModifyVpcAttributeInput, _ ...func(*ec2.Options)) (*ec2.ModifyVpcAttributeOutput, error) {
	return &ec2.ModifyVpcAttributeOutput{}, nil
}

func (f *fakePhaseEC2) DeleteVpc(_ context.Context, _ *ec2.DeleteVpcInput, _ ...func(*ec2.Options)) (*ec2.DeleteVpcOutput, error) {
	f.vpcDeleted = true
	return &ec2.DeleteVpcOutput{}, nil
}

func (f *fakePhaseEC2) CreateInternetGateway(_ context.Context, _ *ec2.CreateInternetGatewayInput, _ ...func(*ec2.Options)) (*ec2.CreateInternetGatewayOutput, error) {
	return &ec2.CreateInternetGatewayOutput{
		InternetGateway: &ec2types.InternetGateway{InternetGatewayId: aws.String("igw-12345")},
	}, nil
}

func (f *fakePhaseEC2) AttachInternetGateway(_ context.Context, _ *ec2.AttachInternetGatewayInput, _ ...func(*ec2.Options)) (*ec2.AttachInternetGatewayOutput, error) {
	return &ec2.AttachInternetGatewayOutput{}, nil
}

func (f *fakePhaseEC2) DescribeInternetGateways(_ context.Context, _ *ec2.DescribeInternetGatewaysInput, _ ...func(*ec2.Options)) (*ec2.DescribeInternetGatewaysOutput, error) {
	return &ec2.DescribeInternetGatewaysOutput{}, nil
}

func (f *fakePhaseEC2) DetachInternetGateway(_ context.Context, _ *ec2.DetachInternetGatewayInput, _ ...func(*ec2.Options)) (*ec2.DetachInternetGatewayOutput, error) {
	return &ec2.DetachInternetGatewayOutput{}, nil
}

func (f *fakePhaseEC2) DeleteInternetGateway(_ context.Context, _ *ec2.DeleteInternetGatewayInput, _ ...func(*ec2.Options)) (*ec2.DeleteInternetGatewayOutput, error) {
	return &ec2.DeleteInternetGatewayOutput{}, nil
}

func (f *fakePhaseEC2) DescribeSubnets(_ context.Context, _ *ec2.DescribeSubnetsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	return &ec2.DescribeSubnetsOutput{}, nil
}

func (f *fakePhaseEC2) CreateSubnet(_ context.Context, in *ec2.CreateSubnetInput, _ ...func(*ec2.Options)) (*ec2.CreateSubnetOutput, error) {
	return &ec2.CreateSubnetOutput{
		Subnet: &ec2types.Subnet{SubnetId: aws.String("subnet-" + aws.ToString(in.CidrBlock))},
	}, nil
}

func (f *fakePhaseEC2) ModifySubnetAttribute(_ context.Context, _ *ec2.ModifySubnetAttributeInput, _ ...func(*ec2.Options)) (*ec2.ModifySubnetAttributeOutput, error) {
	return &ec2.ModifySubnetAttributeOutput{}, nil
}

func (f *fakePhaseEC2) DeleteSubnet(_ context.Context, _ *ec2.DeleteSubnetInput, _ ...func(*ec2.Options)) (*ec2.DeleteSubnetOutput, error) {
	return &ec2.DeleteSubnetOutput{}, nil
}

func (f *fakePhaseEC2) AllocateAddress(_ context.Context, _ *ec2.AllocateAddressInput, _ ...func(*ec2.Options)) (*ec2.AllocateAddressOutput, error) {
	return &ec2.AllocateAddressOutput{AllocationId: aws.String("eipalloc-12345")}, nil
}

func (f *fakePhaseEC2) ReleaseAddress(_ context.Context, _ *ec2.ReleaseAddressInput, _ ...func(*ec2.Options)) (*ec2.ReleaseAddressOutput, error) {
	return &ec2.ReleaseAddressOutput{}, nil
}

func (f *fakePhaseEC2) CreateNatGateway(_ context.Context, _ *ec2.CreateNatGatewayInput, _ ...func(*ec2.Options)) (*ec2.CreateNatGatewayOutput, error) {
	return &ec2.CreateNatGatewayOutput{
		NatGateway: &ec2types.NatGateway{NatGatewayId: aws.String("nat-12345"), State: ec2types.NatGatewayStateAvailable},
	}, nil
}

func (f *fakePhaseEC2) DescribeNatGateways(_ context.Context, _ *ec2.DescribeNatGatewaysInput, _ ...func(*ec2.Options)) (*ec2.DescribeNatGatewaysOutput, error) {
	return &ec2.DescribeNatGatewaysOutput{
		NatGateways: []ec2types.NatGateway{{NatGatewayId: aws.String("nat-12345"), State: ec2types.NatGatewayStateDeleted}},
	}, nil
}

func (f *fakePhaseEC2) DeleteNatGateway(_ context.Context, _ *ec2.DeleteNatGatewayInput, _ ...func(*ec2.Options)) (*ec2.DeleteNatGatewayOutput, error) {
	return &ec2.DeleteNatGatewayOutput{}, nil
}

func (f *fakePhaseEC2) DescribeRouteTables(_ context.Context, _ *ec2.DescribeRouteTablesInput, _ ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error) {
	return &ec2.DescribeRouteTablesOutput{}, nil
}

func (f *fakePhaseEC2) CreateRouteTable(_ context.Context, _ *ec2.CreateRouteTableInput, _ ...func(*ec2.Options)) (*ec2.CreateRouteTableOutput, error) {
	return &ec2.CreateRouteTableOutput{
		RouteTable: &ec2types.RouteTable{RouteTableId: aws.String("rtb-12345")},
	}, nil
}

func (f *fakePhaseEC2) CreateRoute(_ context.Context, _ *ec2.CreateRouteInput, _ ...func(*ec2.Options)) (*ec2.CreateRouteOutput, error) {
	return &ec2.CreateRouteOutput{}, nil
}

func (f *fakePhaseEC2) AssociateRouteTable(_ context.Context, _ *ec2.AssociateRouteTableInput, _ ...func(*ec2.Options)) (*ec2.AssociateRouteTableOutput, error) {
	return &ec2.AssociateRouteTableOutput{AssociationId: aws.String("rtbassoc-12345")}, nil
}

func (f *fakePhaseEC2) DisassociateRouteTable(_ context.Context, _ *ec2.DisassociateRouteTableInput, _ ...func(*ec2.Options)) (*ec2.DisassociateRouteTableOutput, error) {
	return &ec2.DisassociateRouteTableOutput{}, nil
}

func (f *fakePhaseEC2) DeleteRouteTable(_ context.Context, _ *ec2.DeleteRouteTableInput, _ ...func(*ec2.Options)) (*ec2.DeleteRouteTableOutput, error) {
	return &ec2.DeleteRouteTableOutput{}, nil
}

func (f *fakePhaseEC2) DescribeSecurityGroups(_ context.Context, _ *ec2.DescribeSecurityGroupsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	return &ec2.DescribeSecurityGroupsOutput{}, nil
}

func (f *fakePhaseEC2) CreateSecurityGroup(_ context.Context, in *ec2.CreateSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.CreateSecurityGroupOutput, error) {
	return &ec2.CreateSecurityGroupOutput{GroupId: aws.String("sg-" + aws.ToString(in.GroupName))}, nil
}

func (f *fakePhaseEC2) AuthorizeSecurityGroupIngress(_ context.Context, _ *ec2.AuthorizeSecurityGroupIngressInput, _ ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	return &ec2.AuthorizeSecurityGroupIngressOutput{}, nil
}

func (f *fakePhaseEC2) AuthorizeSecurityGroupEgress(_ context.Context, _ *ec2.AuthorizeSecurityGroupEgressInput, _ ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupEgressOutput, error) {
	return &ec2.AuthorizeSecurityGroupEgressOutput{}, nil
}

func (f *fakePhaseEC2) RevokeSecurityGroupIngress(_ context.Context, _ *ec2.RevokeSecurityGroupIngressInput, _ ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupIngressOutput, error) {
	return &ec2.RevokeSecurityGroupIngressOutput{}, nil
}

func (f *fakePhaseEC2) RevokeSecurityGroupEgress(_ context.Context, _ *ec2.RevokeSecurityGroupEgressInput, _ ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupEgressOutput, error) {
	return &ec2.RevokeSecurityGroupEgressOutput{}, nil
}

func (f *fakePhaseEC2) DeleteSecurityGroup(_ context.Context, in *ec2.DeleteSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error) {
	if in != nil && aws.ToString(in.GroupId) == "sg-default" {
		return nil, fmt.Errorf("api error CannotDelete: the specified group: %q name: \"default\" cannot be deleted by a user", aws.ToString(in.GroupId))
	}
	return &ec2.DeleteSecurityGroupOutput{}, nil
}

func (f *fakePhaseEC2) DescribeLaunchTemplates(_ context.Context, _ *ec2.DescribeLaunchTemplatesInput, _ ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplatesOutput, error) {
	return &ec2.DescribeLaunchTemplatesOutput{}, nil
}

func (f *fakePhaseEC2) CreateLaunchTemplate(_ context.Context, _ *ec2.CreateLaunchTemplateInput, _ ...func(*ec2.Options)) (*ec2.CreateLaunchTemplateOutput, error) {
	return &ec2.CreateLaunchTemplateOutput{
		LaunchTemplate: &ec2types.LaunchTemplate{LaunchTemplateId: aws.String("lt-12345")},
	}, nil
}

func (f *fakePhaseEC2) DeleteLaunchTemplate(_ context.Context, _ *ec2.DeleteLaunchTemplateInput, _ ...func(*ec2.Options)) (*ec2.DeleteLaunchTemplateOutput, error) {
	return &ec2.DeleteLaunchTemplateOutput{}, nil
}

func (f *fakePhaseEC2) DescribeInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	st := ec2types.InstanceStateNameRunning
	if f.instanceTerminated {
		st = ec2types.InstanceStateNameTerminated
	}
	return &ec2.DescribeInstancesOutput{
		Reservations: []ec2types.Reservation{
			{
				Instances: []ec2types.Instance{
					{
						InstanceId:       aws.String("i-0123456789abcdef0"),
						PrivateDnsName:   aws.String("ip-10-80-1-10.ap-southeast-2.compute.internal"),
						PrivateIpAddress: aws.String("10.80.1.10"),
						Placement:        &ec2types.Placement{AvailabilityZone: aws.String("ap-southeast-2a")},
						State:            &ec2types.InstanceState{Name: st},
					},
				},
			},
		},
	}, nil
}

func (f *fakePhaseEC2) DescribeNetworkInterfaces(_ context.Context, _ *ec2.DescribeNetworkInterfacesInput, _ ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error) {
	return &ec2.DescribeNetworkInterfacesOutput{}, nil
}

func (f *fakePhaseEC2) CreateNetworkInterface(_ context.Context, in *ec2.CreateNetworkInterfaceInput, _ ...func(*ec2.Options)) (*ec2.CreateNetworkInterfaceOutput, error) {
	return &ec2.CreateNetworkInterfaceOutput{
		NetworkInterface: &ec2types.NetworkInterface{
			NetworkInterfaceId: aws.String("eni-" + aws.ToString(in.SubnetId)),
			MacAddress:         aws.String("06:aa:bb:cc:dd:02"),
			PrivateIpAddress:   aws.String("10.80.100.15"),
		},
	}, nil
}

func (f *fakePhaseEC2) AttachNetworkInterface(_ context.Context, _ *ec2.AttachNetworkInterfaceInput, _ ...func(*ec2.Options)) (*ec2.AttachNetworkInterfaceOutput, error) {
	return &ec2.AttachNetworkInterfaceOutput{AttachmentId: aws.String("eni-attach-12345")}, nil
}

func (f *fakePhaseEC2) ModifyNetworkInterfaceAttribute(_ context.Context, _ *ec2.ModifyNetworkInterfaceAttributeInput, _ ...func(*ec2.Options)) (*ec2.ModifyNetworkInterfaceAttributeOutput, error) {
	return &ec2.ModifyNetworkInterfaceAttributeOutput{}, nil
}

func (f *fakePhaseEC2) DetachNetworkInterface(_ context.Context, _ *ec2.DetachNetworkInterfaceInput, _ ...func(*ec2.Options)) (*ec2.DetachNetworkInterfaceOutput, error) {
	return &ec2.DetachNetworkInterfaceOutput{}, nil
}

func (f *fakePhaseEC2) DeleteNetworkInterface(_ context.Context, _ *ec2.DeleteNetworkInterfaceInput, _ ...func(*ec2.Options)) (*ec2.DeleteNetworkInterfaceOutput, error) {
	return &ec2.DeleteNetworkInterfaceOutput{}, nil
}

func (f *fakePhaseEC2) DeleteTags(_ context.Context, _ *ec2.DeleteTagsInput, _ ...func(*ec2.Options)) (*ec2.DeleteTagsOutput, error) {
	return &ec2.DeleteTagsOutput{}, nil
}

func (f *fakePhaseEC2) DescribeVolumes(_ context.Context, _ *ec2.DescribeVolumesInput, _ ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	return &ec2.DescribeVolumesOutput{}, nil
}

func (f *fakePhaseEC2) DeleteVolume(_ context.Context, _ *ec2.DeleteVolumeInput, _ ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error) {
	return &ec2.DeleteVolumeOutput{}, nil
}

func (f *fakePhaseEC2) DescribeInstanceConnectEndpoints(_ context.Context, _ *ec2.DescribeInstanceConnectEndpointsInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceConnectEndpointsOutput, error) {
	return &ec2.DescribeInstanceConnectEndpointsOutput{}, nil
}

func (f *fakePhaseEC2) DeleteInstanceConnectEndpoint(_ context.Context, _ *ec2.DeleteInstanceConnectEndpointInput, _ ...func(*ec2.Options)) (*ec2.DeleteInstanceConnectEndpointOutput, error) {
	return &ec2.DeleteInstanceConnectEndpointOutput{}, nil
}

func (f *fakePhaseEC2) TerminateInstances(_ context.Context, _ *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.instanceTerminated = true
	return &ec2.TerminateInstancesOutput{}, nil
}

func (f *fakePhaseEC2) ModifyInstanceMetadataOptions(_ context.Context, _ *ec2.ModifyInstanceMetadataOptionsInput, _ ...func(*ec2.Options)) (*ec2.ModifyInstanceMetadataOptionsOutput, error) {
	return &ec2.ModifyInstanceMetadataOptionsOutput{}, nil
}

type fakePhaseIAM struct {
	PhaseIAMAPI
	oidcProviders     []iamtypes.OpenIDConnectProviderListEntry
	deletedOIDCARNs   []string
	deletedPolicyARNs []string
}

func (f *fakePhaseIAM) GetRole(_ context.Context, _ *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	return nil, fakeNotFoundErr{}
}

func (f *fakePhaseIAM) CreateRole(_ context.Context, in *iam.CreateRoleInput, _ ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
	return &iam.CreateRoleOutput{
		Role: &iamtypes.Role{
			RoleName: in.RoleName,
			Arn:      aws.String("arn:aws:iam::123456789012:role/" + aws.ToString(in.RoleName)),
		},
	}, nil
}

func (f *fakePhaseIAM) AttachRolePolicy(_ context.Context, _ *iam.AttachRolePolicyInput, _ ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error) {
	return &iam.AttachRolePolicyOutput{}, nil
}

func (f *fakePhaseIAM) PutRolePolicy(_ context.Context, _ *iam.PutRolePolicyInput, _ ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error) {
	return &iam.PutRolePolicyOutput{}, nil
}

func (f *fakePhaseIAM) ListAttachedRolePolicies(_ context.Context, _ *iam.ListAttachedRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
	return &iam.ListAttachedRolePoliciesOutput{}, nil
}

func (f *fakePhaseIAM) ListRolePolicies(_ context.Context, _ *iam.ListRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error) {
	return &iam.ListRolePoliciesOutput{}, nil
}

func (f *fakePhaseIAM) RemoveRoleFromInstanceProfile(_ context.Context, _ *iam.RemoveRoleFromInstanceProfileInput, _ ...func(*iam.Options)) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	return &iam.RemoveRoleFromInstanceProfileOutput{}, nil
}

func (f *fakePhaseIAM) DeleteInstanceProfile(_ context.Context, _ *iam.DeleteInstanceProfileInput, _ ...func(*iam.Options)) (*iam.DeleteInstanceProfileOutput, error) {
	return &iam.DeleteInstanceProfileOutput{}, nil
}

func (f *fakePhaseIAM) ListInstanceProfilesForRole(_ context.Context, _ *iam.ListInstanceProfilesForRoleInput, _ ...func(*iam.Options)) (*iam.ListInstanceProfilesForRoleOutput, error) {
	return &iam.ListInstanceProfilesForRoleOutput{}, nil
}

func (f *fakePhaseIAM) DeleteRole(_ context.Context, _ *iam.DeleteRoleInput, _ ...func(*iam.Options)) (*iam.DeleteRoleOutput, error) {
	return &iam.DeleteRoleOutput{}, nil
}

func (f *fakePhaseIAM) ListOpenIDConnectProviders(_ context.Context, _ *iam.ListOpenIDConnectProvidersInput, _ ...func(*iam.Options)) (*iam.ListOpenIDConnectProvidersOutput, error) {
	return &iam.ListOpenIDConnectProvidersOutput{
		OpenIDConnectProviderList: f.oidcProviders,
	}, nil
}

func (f *fakePhaseIAM) CreateOpenIDConnectProvider(_ context.Context, _ *iam.CreateOpenIDConnectProviderInput, _ ...func(*iam.Options)) (*iam.CreateOpenIDConnectProviderOutput, error) {
	return &iam.CreateOpenIDConnectProviderOutput{
		OpenIDConnectProviderArn: aws.String("arn:aws:iam::123456789012:oidc-provider/oidc.eks.ap-southeast-2.amazonaws.com/id/EXAMPLED539D4633E53DE1B716D3041E"),
	}, nil
}

func (f *fakePhaseIAM) DeleteOpenIDConnectProvider(_ context.Context, in *iam.DeleteOpenIDConnectProviderInput, _ ...func(*iam.Options)) (*iam.DeleteOpenIDConnectProviderOutput, error) {
	if in != nil && in.OpenIDConnectProviderArn != nil {
		f.deletedOIDCARNs = append(f.deletedOIDCARNs, *in.OpenIDConnectProviderArn)
	}
	return &iam.DeleteOpenIDConnectProviderOutput{}, nil
}

func (f *fakePhaseIAM) DetachRolePolicy(_ context.Context, _ *iam.DetachRolePolicyInput, _ ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error) {
	return &iam.DetachRolePolicyOutput{}, nil
}

func (f *fakePhaseIAM) DeleteRolePolicy(_ context.Context, _ *iam.DeleteRolePolicyInput, _ ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error) {
	return &iam.DeleteRolePolicyOutput{}, nil
}

func (f *fakePhaseIAM) CreatePolicy(_ context.Context, in *iam.CreatePolicyInput, _ ...func(*iam.Options)) (*iam.CreatePolicyOutput, error) {
	return &iam.CreatePolicyOutput{
		Policy: &iamtypes.Policy{
			PolicyName: in.PolicyName,
			Arn:        aws.String("arn:aws:iam::123456789012:policy/" + aws.ToString(in.PolicyName)),
		},
	}, nil
}

func (f *fakePhaseIAM) GetPolicy(_ context.Context, in *iam.GetPolicyInput, _ ...func(*iam.Options)) (*iam.GetPolicyOutput, error) {
	return &iam.GetPolicyOutput{
		Policy: &iamtypes.Policy{
			Arn: in.PolicyArn,
		},
	}, nil
}

func (f *fakePhaseIAM) DeletePolicy(_ context.Context, in *iam.DeletePolicyInput, _ ...func(*iam.Options)) (*iam.DeletePolicyOutput, error) {
	if in != nil && in.PolicyArn != nil {
		f.deletedPolicyARNs = append(f.deletedPolicyARNs, *in.PolicyArn)
	}
	return &iam.DeletePolicyOutput{}, nil
}

func (f *fakePhaseIAM) GetOpenIDConnectProvider(_ context.Context, _ *iam.GetOpenIDConnectProviderInput, _ ...func(*iam.Options)) (*iam.GetOpenIDConnectProviderOutput, error) {
	return &iam.GetOpenIDConnectProviderOutput{
		Url:          aws.String("oidc.eks.ap-southeast-2.amazonaws.com/id/EXAMPLED539D4633E53DE1B716D3041E"),
		ClientIDList: []string{"sts.amazonaws.com"},
	}, nil
}

func (f *fakePhaseIAM) CreateServiceLinkedRole(_ context.Context, _ *iam.CreateServiceLinkedRoleInput, _ ...func(*iam.Options)) (*iam.CreateServiceLinkedRoleOutput, error) {
	return &iam.CreateServiceLinkedRoleOutput{}, nil
}

type fakePhaseEKS struct {
	PhaseEKSAPI
	clusterCreated   bool
	nodegroupCreated bool
}

func (f *fakePhaseEKS) DescribeCluster(_ context.Context, in *eks.DescribeClusterInput, _ ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
	if !f.clusterCreated {
		return nil, fakeNotFoundErr{}
	}
	return &eks.DescribeClusterOutput{
		Cluster: &ekstypes.Cluster{
			Name:     in.Name,
			Arn:      aws.String("arn:aws:eks:ap-southeast-2:123456789012:cluster/" + aws.ToString(in.Name)),
			Endpoint: aws.String("https://eks.example.com"),
			Status:   ekstypes.ClusterStatusActive,
			CertificateAuthority: &ekstypes.Certificate{
				Data: aws.String("LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCg=="),
			},
			Identity: &ekstypes.Identity{
				Oidc: &ekstypes.OIDC{
					Issuer: aws.String("https://oidc.eks.ap-southeast-2.amazonaws.com/id/EXAMPLED539D4633E53DE1B716D3041E"),
				},
			},
			ResourcesVpcConfig: &ekstypes.VpcConfigResponse{
				ClusterSecurityGroupId: aws.String("sg-cluster-12345"),
			},
		},
	}, nil
}

func (f *fakePhaseEKS) CreateCluster(_ context.Context, in *eks.CreateClusterInput, _ ...func(*eks.Options)) (*eks.CreateClusterOutput, error) {
	f.clusterCreated = true
	return &eks.CreateClusterOutput{
		Cluster: &ekstypes.Cluster{
			Name:   in.Name,
			Status: ekstypes.ClusterStatusActive,
		},
	}, nil
}

func (f *fakePhaseEKS) DeleteCluster(_ context.Context, _ *eks.DeleteClusterInput, _ ...func(*eks.Options)) (*eks.DeleteClusterOutput, error) {
	f.clusterCreated = false
	return &eks.DeleteClusterOutput{}, nil
}

func (f *fakePhaseEKS) ListNodegroups(_ context.Context, _ *eks.ListNodegroupsInput, _ ...func(*eks.Options)) (*eks.ListNodegroupsOutput, error) {
	return &eks.ListNodegroupsOutput{}, nil
}

func (f *fakePhaseEKS) DescribeNodegroup(_ context.Context, in *eks.DescribeNodegroupInput, _ ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error) {
	if !f.nodegroupCreated {
		return nil, fakeNotFoundErr{}
	}
	return &eks.DescribeNodegroupOutput{
		Nodegroup: &ekstypes.Nodegroup{
			NodegroupName: in.NodegroupName,
			Status:        ekstypes.NodegroupStatusActive,
		},
	}, nil
}

func (f *fakePhaseEKS) CreateNodegroup(_ context.Context, in *eks.CreateNodegroupInput, _ ...func(*eks.Options)) (*eks.CreateNodegroupOutput, error) {
	f.nodegroupCreated = true
	return &eks.CreateNodegroupOutput{
		Nodegroup: &ekstypes.Nodegroup{
			NodegroupName: in.NodegroupName,
			Status:        ekstypes.NodegroupStatusActive,
		},
	}, nil
}

func (f *fakePhaseEKS) DeleteNodegroup(_ context.Context, _ *eks.DeleteNodegroupInput, _ ...func(*eks.Options)) (*eks.DeleteNodegroupOutput, error) {
	f.nodegroupCreated = false
	return &eks.DeleteNodegroupOutput{}, nil
}

func (f *fakePhaseEKS) DescribeAddon(_ context.Context, in *eks.DescribeAddonInput, _ ...func(*eks.Options)) (*eks.DescribeAddonOutput, error) {
	return &eks.DescribeAddonOutput{
		Addon: &ekstypes.Addon{
			AddonName: in.AddonName,
			Status:    ekstypes.AddonStatusActive,
		},
	}, nil
}

func (f *fakePhaseEKS) CreateAddon(_ context.Context, in *eks.CreateAddonInput, _ ...func(*eks.Options)) (*eks.CreateAddonOutput, error) {
	return &eks.CreateAddonOutput{
		Addon: &ekstypes.Addon{
			AddonName: in.AddonName,
			Status:    ekstypes.AddonStatusActive,
		},
	}, nil
}

func (f *fakePhaseEKS) UpdateAddon(_ context.Context, _ *eks.UpdateAddonInput, _ ...func(*eks.Options)) (*eks.UpdateAddonOutput, error) {
	return &eks.UpdateAddonOutput{}, nil
}

func (f *fakePhaseEKS) DeleteAddon(_ context.Context, _ *eks.DeleteAddonInput, _ ...func(*eks.Options)) (*eks.DeleteAddonOutput, error) {
	return &eks.DeleteAddonOutput{}, nil
}

func TestGreenfieldAWSPhasesEndToEnd(t *testing.T) {
	origNATPoll := NATPollInterval
	origEKSPoll := EKSPollInterval
	origNGPoll := NodeGroupPollInterval
	origK8sPoll := NodeReadyPollInterval
	origThumb := ThumbprintDialer
	NATPollInterval = time.Millisecond
	EKSPollInterval = time.Millisecond
	NodeGroupPollInterval = time.Millisecond
	NodeReadyPollInterval = time.Millisecond
	ThumbprintDialer = func(_ string) (string, error) {
		return "9e99a48a9960b14926bb7f3b02e22da2b0ab7280", nil
	}
	defer func() {
		NATPollInterval = origNATPoll
		EKSPollInterval = origEKSPoll
		NodeGroupPollInterval = origNGPoll
		NodeReadyPollInterval = origK8sPoll
		ThumbprintDialer = origThumb
	}()

	ctx := context.Background()
	st, err := state.Load(filepath.Join(t.TempDir(), "state.env"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}

	cl := &intent.Cluster{
		APIVersion: intent.APIVersion,
		Kind:       intent.Kind,
		Cloud:      &intent.CloudSpec{Provider: intent.ProviderAWS},
		Metadata:   intent.Metadata{Name: "eks-greenfield", Region: "ap-southeast-2"},
		Pattern:    intent.PatternExternalOnly,
		Network: intent.Network{
			VPCCidr: "10.80.0.0/16",
			AZs:     []string{"ap-southeast-2a"},
			Subnets: intent.SubnetsSpec{
				PublicList:  []intent.SubnetSpec{{CIDR: "10.80.1.0/24", AZ: "ap-southeast-2a"}},
				PrivateList: []intent.SubnetSpec{{CIDR: "10.80.2.0/24", AZ: "ap-southeast-2a"}},
			},
			DataPath: &intent.DataPathSpec{
				External: intent.SubnetSpec{CIDR: "10.80.100.0/24", AZ: "ap-southeast-2a"},
			},
		},
		ClusterSpec: &intent.ClusterSpec{
			KubernetesVersion: "1.34",
			NodeGroups: []intent.NodeGroupSpec{
				{
					Name:         "bnk-workers",
					InstanceType: "m6i.2xlarge",
					DesiredSize:  1,
					MinSize:      1,
					MaxSize:      2,
					Labels:       map[string]string{"role": "bnk"},
				},
			},
		},
	}

	cs := k8sfake.NewClientset(
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "ip-10-80-1-10.ap-southeast-2.compute.internal",
				Labels: map[string]string{"role": "bnk", "topology.kubernetes.io/zone": "ap-southeast-2a"},
			},
			Spec: corev1.NodeSpec{
				ProviderID: "aws:///ap-southeast-2a/i-0123456789abcdef0",
			},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{
					{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
				},
			},
		},
	)

	clients := &Clients{
		Region:   "ap-southeast-2",
		STS:      &fakePhaseSTS{},
		PhaseEC2: &fakePhaseEC2{},
		PhaseIAM: &fakePhaseIAM{},
		PhaseEKS: &fakePhaseEKS{},
		K8s:      cs,
	}

	p := &Provider{Clients: clients}
	u, err := p.ProvisionUnderlay(ctx, cl, st, false)
	if err != nil {
		t.Fatalf("ProvisionUnderlay: %v", err)
	}
	if u == nil || u.DataplaneDriver == "" {
		t.Fatalf("unexpected underlay: %+v", u)
	}
	if st.Get("VPC_ID") != "vpc-12345" {
		t.Fatalf("expected VPC_ID=vpc-12345, got %q", st.Get("VPC_ID"))
	}
	if st.Get("IGW_ID") != "igw-12345" {
		t.Fatalf("expected IGW_ID=igw-12345, got %q", st.Get("IGW_ID"))
	}
	if st.Get("EKS_CLUSTER_NAME") != "eks-greenfield" {
		t.Fatalf("expected EKS_CLUSTER_NAME=eks-greenfield, got %q", st.Get("EKS_CLUSTER_NAME"))
	}
	if st.Get("NODEGROUP_NAME_0") != "bnk-workers" {
		t.Fatalf("expected NODEGROUP_NAME_0=bnk-workers, got %q", st.Get("NODEGROUP_NAME_0"))
	}
	if st.Get("EXT_ENI_ID") == "" {
		t.Fatalf("expected EXT_ENI_ID to be set, got %q", st.Get("EXT_ENI_ID"))
	}
	if st.Get("CLOUD_NETWORK_MAPPING_APPLIED") != "true" {
		t.Fatalf("expected CLOUD_NETWORK_MAPPING_APPLIED=true, got %q", st.Get("CLOUD_NETWORK_MAPPING_APPLIED"))
	}

	if err := p.DestroyUnderlay(ctx, cl, st, false); err != nil {
		t.Fatalf("DestroyUnderlay: %v", err)
	}
	if st.Get("VPC_ID") != "" || st.Get("UNDERLAY_DESTROYED") != "true" {
		t.Fatalf("expected VPC_ID cleared and UNDERLAY_DESTROYED=true after DestroyUnderlay")
	}
}

func TestAWSIfaceDiscoveryStateKeysPropagateToUnderlay(t *testing.T) {
	ctx := context.Background()
	st, err := state.Load(filepath.Join(t.TempDir(), "state.env"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Set("EXTERNAL_IFNAME", "ens6")
	st.Set("EXTERNAL_PCI", "0000:00:06.0")
	st.Set("INTERNAL_IFNAME", "ens7")
	st.Set("INTERNAL_PCI", "0000:00:07.0")

	cl := &intent.Cluster{
		APIVersion: intent.APIVersion,
		Kind:       intent.Kind,
		Cloud:      &intent.CloudSpec{Provider: intent.ProviderAWS},
		Metadata:   intent.Metadata{Name: "eks-dual", Region: "ap-southeast-2"},
		Pattern:    intent.PatternDualInterface,
		Network: intent.Network{
			VPCCidr: "10.80.0.0/16",
			DataPath: &intent.DataPathSpec{
				External: intent.SubnetSpec{CIDR: "10.80.100.0/24", AZ: "ap-southeast-2a"},
				Internal: intent.SubnetSpec{CIDR: "10.80.101.0/24", AZ: "ap-southeast-2a"},
			},
		},
	}

	p := NewProvider()
	u, err := p.DiscoverUnderlay(ctx, cl, st)
	if err != nil {
		t.Fatalf("DiscoverUnderlay: %v", err)
	}

	cs := k8sfake.NewClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "ip-10-80-1-10.ap-southeast-2.compute.internal",
			Labels: map[string]string{"topology.kubernetes.io/zone": "ap-southeast-2a"},
		},
		Spec: corev1.NodeSpec{ProviderID: "aws:///ap-southeast-2a/i-0123456789abcdef0"},
	})
	if err := u.DiscoverDataplaneResources(ctx, cs, nil); err != nil {
		t.Fatalf("DiscoverDataplaneResources: %v", err)
	}
	if len(u.DataplaneNodes) != 1 || len(u.DataplaneNodes[0].NICs) != 2 {
		t.Fatalf("expected 1 dataplane node with 2 NICs, got %+v", u.DataplaneNodes)
	}
	if u.DataplaneNodes[0].NICs[0].LinuxIfName != "ens6" || u.DataplaneNodes[0].NICs[0].PCIAddress != "0000:00:06.0" {
		t.Fatalf("unexpected external NIC attachment: %+v", u.DataplaneNodes[0].NICs[0])
	}
	if u.DataplaneNodes[0].NICs[1].LinuxIfName != "ens7" || u.DataplaneNodes[0].NICs[1].PCIAddress != "0000:00:07.0" {
		t.Fatalf("unexpected internal NIC attachment: %+v", u.DataplaneNodes[0].NICs[1])
	}
}

func TestAWSTeardownLegacyStateKeysAndResources(t *testing.T) {
	origNATPoll := NATPollInterval
	origEKSPoll := EKSPollInterval
	NATPollInterval = time.Millisecond
	EKSPollInterval = time.Millisecond
	defer func() {
		NATPollInterval = origNATPoll
		EKSPollInterval = origEKSPoll
	}()

	ctx := context.Background()
	st, err := state.Load(filepath.Join(t.TempDir(), "state.env"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}

	legacyKeys := map[string]string{
		"VPC_ID":                           "vpc-12345",
		"IGW_ID":                           "igw-12345",
		"PUBLIC_SUBNETS":                   "subnet-pub1,subnet-pub2",
		"PRIVATE_SUBNETS":                  "subnet-priv1,subnet-priv2",
		"BNK_EXT_SUBNET":                   "subnet-ext",
		"NAT_EIP_ALLOC":                    "eipalloc-12345",
		"NAT_GW_ID":                        "nat-12345",
		"PUBLIC_RTB":                       "rtb-pub",
		"PRIVATE_RTB":                      "rtb-priv",
		"SG_BNK_DATA":                      "sg-bnk-data",
		"EKS_CLUSTER_ROLE_ARN":             "arn:aws:iam::123456789012:role/test-eks-cluster-role",
		"EKS_NODE_ROLE_ARN":                "arn:aws:iam::123456789012:role/test-eks-node-role",
		"NODE_INSTANCE_PROFILE_NAME":       "test-eks-node-profile",
		"EKS_CLUSTER_NAME":                 "test-cluster",
		"EKS_SECURITY_GROUP":               "sg-eks",
		"LT_ID":                            "lt-12345",
		"NODEGROUP_DEFAULT_NAME":           "test-cluster-ng-default",
		"EXTERNAL_ENI":                     "eni-ext",
		"JUMPHOST_ID":                      "i-jumphost",
		"JUMPHOST_SG_ID":                   "sg-default",
		"JUMPHOST_EICE_SG_ID":              "sg-eice",
		"JUMPHOST_EICE_ID":                 "eice-12345",
		"JUMPHOST_BNK_EXT_ENI_ID":          "eni-jumphost-ext",
		"JUMPHOST_MGMT_ENI_ID":             "eni-jumphost-mgmt",
		"JUMPHOST_MGMT_ENI_IP":             "10.0.1.194",
		"JUMPHOST_BNK_EXT_ENI_IP":          "10.0.10.200",
		"JUMPHOST_INSTANCE_TYPE":           "c6i.4xlarge",
		"JUMPHOST_ROLE_NAME":               "test-jumphost-role",
		"JUMPHOST_INSTANCE_PROFILE_NAME":   "test-jumphost-profile",
		"CNE_IRSA_ROLE_NAME":               "test-cne-irsa",
		"IRSA_SA_APPLIED_AT":               "2026-04-16T07:57:40Z",
		"LB_CONTROLLER_IAM_ROLE_ARN":       "arn:aws:iam::123456789012:role/test-aws-lb-controller",
		"LB_CONTROLLER_POLICY_ARN":         "arn:aws:iam::123456789012:policy/test-AWSLoadBalancerControllerIAMPolicy",
		"EKS_OIDC_URL":                     "https://oidc.eks.ap-southeast-1.amazonaws.com/id/1234",
		"EKS_CA":                           "LS0tLS1CRUdJTi...",
		"EKS_ENDPOINT":                     "https://example.gr7.ap-southeast-1.eks.amazonaws.com",
		"EKS_VERSION":                      "1.33",
		"NODEGROUP_DEFAULT_ARN":            "arn:aws:eks:ap-southeast-1:123456789012:nodegroup/test-cluster/ng/123",
		"CLOUD_NETWORK_MAPPING_CM":         "cloud-network-mapping",
		"CLUSTER_NAME":                     "test-cluster",
		"AWS_REGION":                       "ap-southeast-1",
		"METRICS_SERVER_ADDON":             "metrics-server",
		"IFACE_DISCOVERY_AT":               "2026-04-16T07:57:40Z",
		"CLOUD_HOST_DEVICE_TAG":            "ens6",
		"EXTERNAL_NAD":                     "bnk-ext-nad",
		"INTERNAL_NAD":                     "bnk-int-nad",
		"TMM_EXT_SELFIP":                   "10.0.10.10",
		"TMM_INT_SELFIP":                   "10.0.20.10",
		"TMM_SELFIP_PREFIXLEN":             "24",
		"CERT_MANAGER_VERSION":             "v1.14.4",
		"MULTUS_VERSION":                   "v4.0.2",
		"OPERATOR_NS":                      "f5-utils",
		"INSTANCE_NS":                      "f5-cne-core",
		"BNK_NAMESPACES_CREATED":           "true",
		"BNK_FAR_SECRET_NAME":              "f5-far-auth-key",
		"BNK_LICENSE_JWT_SECRET":           "f5-license-jwt",
		"CNE_SA_NAME":                      "cne-controller",
		"BNK_SELFSIGNED_ISSUER":            "selfsigned-issuer",
		"BNK_CA_CERT_NAME":                 "cwc-ca-certificate",
		"BNK_CA_SECRET_NAME":               "cwc-ca-key-pair",
		"BNK_CA_ISSUER":                    "cwc-ca-issuer",
		"OTEL_SVR_CERT_NAME":               "otel-svr-cert",
		"OTEL_F5ING_CERT_NAME":             "f5ing-otel-cert",
		"FLO_VERSION":                      "v0.6.110-0.0.18",
		"FLO_RELEASE_NAME":                 "f5-lifecycle-operator",
		"FLO_NAMESPACE":                    "f5-utils",
		"FLO_INSTALLED_AT":                 "2026-04-16T07:58:00Z",
		"LICENSE_CRD_READY_AT":             "2026-04-16T07:58:05Z",
		"LICENSE_NAME":                     "f5-spk-license",
		"LICENSE_APPLIED_AT":               "2026-04-16T07:58:10Z",
		"BNK_API_GENERATION":               "2.4",
		"NADS_APPLIED_AT":                  "2026-04-16T07:58:15Z",
		"INFRA_APPLIED_AT":                 "2026-04-16T07:58:20Z",
		"CNEINSTANCE_NAME":                 "bnk-instance",
		"CNEINSTANCE_APPLIED_AT":           "2026-04-16T07:58:25Z",
		"CNEINSTANCE_RECONCILE_STARTED_AT": "2026-04-16T07:58:30Z",
		"CNEINSTANCE_READY_AT":             "2026-04-16T08:00:00Z",
		"DSSM_INSECURE_OVERLAY_APPLIED_AT": "2026-04-16T08:00:05Z",
		"TMM_LOG_STREAM_ENABLED_AT":        "2026-04-16T08:00:10Z",
		"GATEWAYCLASS_NAME":                "f5-gateway-class",
	}
	for k, v := range legacyKeys {
		st.Set(k, v)
	}

	cl := &intent.Cluster{
		APIVersion: intent.APIVersion,
		Kind:       intent.Kind,
		Cloud:      &intent.CloudSpec{Provider: intent.ProviderAWS},
		Metadata:   intent.Metadata{Name: "test-cluster", Region: "ap-southeast-1"},
		Pattern:    intent.PatternDualInterface,
		Network: intent.Network{

			VPCCidr: "10.0.0.0/16",
			DataPath: &intent.DataPathSpec{
				External: intent.SubnetSpec{CIDR: "10.0.100.0/24", AZ: "ap-southeast-1a"},
			},
		},
		ClusterSpec: &intent.ClusterSpec{
			NodeGroups: []intent.NodeGroupSpec{{Name: "default", InstanceType: "m5.2xlarge"}},
		},
	}

	fakeIAM := &fakePhaseIAM{
		oidcProviders: []iamtypes.OpenIDConnectProviderListEntry{
			{Arn: aws.String("arn:aws:iam::123456789012:oidc-provider/oidc.eks.ap-southeast-1.amazonaws.com/id/1234")},
		},
	}
	p := &Provider{
		Clients: &Clients{
			Region:   "ap-southeast-1",
			STS:      &fakePhaseSTS{},
			PhaseEC2: &fakePhaseEC2{},
			PhaseIAM: fakeIAM,
			PhaseEKS: &fakePhaseEKS{},
		},
	}

	if err := p.DestroyUnderlay(ctx, cl, st, false); err != nil {
		t.Fatalf("DestroyUnderlay with legacy state keys failed: %v", err)
	}
	if len(fakeIAM.deletedOIDCARNs) != 1 || fakeIAM.deletedOIDCARNs[0] != "arn:aws:iam::123456789012:oidc-provider/oidc.eks.ap-southeast-1.amazonaws.com/id/1234" {
		t.Fatalf("expected OIDC provider to be discovered from EKS_OIDC_URL and deleted, got %v", fakeIAM.deletedOIDCARNs)
	}

	for cleared := range legacyKeys {
		if got := st.Get(cleared); got != "" {
			t.Errorf("expected state key %s to be cleared, got %q", cleared, got)
		}
	}
}

func TestPhase09ForgeRegisterDown_UnreachableForgeRemovesLocalLink(t *testing.T) {
	ctx := context.Background()
	wsDir := t.TempDir()
	st, err := state.Load(filepath.Join(wsDir, "state.env"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Set("FORGE_PROJECT_ID", "16")
	st.Set("FORGE_CLUSTER_ID", "21")
	st.Set("FORGE_REGISTERED", "true")
	st.Set("FORGE_MCP_URL", "http://127.0.0.1:1/mcp/")

	link := &forge.Link{
		ForgeURL:     "http://127.0.0.1:1",
		ForgeMCPURL:  "http://127.0.0.1:1/mcp/",
		ProjectID:    16,
		ProjectName:  "awsbnkctl-agent-core-singapore",
		ClusterID:    21,
		ClusterName:  "agent-core-singapore",
		RegisteredAt: time.Now().UTC(),
		Workspace:    "agent-core-singapore",
		Status:       "registered",
	}
	if err := forge.WriteLink(wsDir, link); err != nil {
		t.Fatalf("forge.WriteLink: %v", err)
	}

	cl := &intent.Cluster{
		Metadata: intent.Metadata{Name: "agent-core-singapore", Region: "ap-southeast-1"},
		Forge: &intent.ForgeSpec{
			Enabled: true,
			URL:     "http://127.0.0.1:1",
			MCPURL:  "http://127.0.0.1:1/mcp/",
		},
	}

	if err := Phase09ForgeRegisterDown(ctx, cl, st, nil, false); err != nil {
		t.Fatalf("Phase09ForgeRegisterDown failed when Forge unreachable: %v", err)
	}

	if _, err := os.Stat(filepath.Join(wsDir, "forge_link.json")); !os.IsNotExist(err) {
		t.Fatalf("expected forge_link.json to be removed after Phase09ForgeRegisterDown, stat err=%v", err)
	}
	for _, k := range []string{"FORGE_PROJECT_ID", "FORGE_CLUSTER_ID", "FORGE_REGISTERED", "FORGE_MCP_URL"} {
		if got := st.Get(k); got != "" {
			t.Errorf("expected %s to be cleared, got %q", k, got)
		}
	}
}

func TestPhase11KubeconfigDown_RemovesWorkspaceKubeconfigWithRelativePath(t *testing.T) {
	ctx := context.Background()
	wsDir := t.TempDir()
	kcPath := filepath.Join(wsDir, "kubeconfig")
	if err := os.WriteFile(kcPath, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatalf("WriteFile kubeconfig: %v", err)
	}

	st, err := state.Load(filepath.Join(wsDir, "state.env"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	// Simulate a relative KUBECONFIG written from a different working directory.
	st.Set("KUBECONFIG", ".awsbnkctl/nonexistent-from-cwd/kubeconfig")

	if got := st.KubeconfigPath(); got != kcPath {
		t.Fatalf("expected st.KubeconfigPath() to fall back to %q, got %q", kcPath, got)
	}

	cl := &intent.Cluster{
		APIVersion: intent.APIVersion,
		Kind:       intent.Kind,
		Cloud:      &intent.CloudSpec{Provider: intent.ProviderAWS},
		Metadata:   intent.Metadata{Name: "agent-core-singapore", Region: "ap-southeast-1"},
	}
	p := NewProvider()
	u, err := p.DiscoverUnderlay(ctx, cl, st)
	if err != nil {
		t.Fatalf("DiscoverUnderlay: %v", err)
	}
	if u.KubeconfigPath != kcPath {
		t.Fatalf("expected u.KubeconfigPath=%q, got %q", kcPath, u.KubeconfigPath)
	}

	if err := Phase11KubeconfigDown(ctx, cl, st, nil, false); err != nil {
		t.Fatalf("Phase11KubeconfigDown: %v", err)
	}
	if _, err := os.Stat(kcPath); !os.IsNotExist(err) {
		t.Fatalf("expected workspace kubeconfig %s to be removed, stat err=%v", kcPath, err)
	}
	if st.Get("KUBECONFIG") != "" || st.Get("KUBECONFIG_PATH") != "" {
		t.Fatalf("expected KUBECONFIG and KUBECONFIG_PATH cleared from state")
	}
}

func TestAWSTeardownStatelessFallback(t *testing.T) {
	origNATPoll := NATPollInterval
	origEKSPoll := EKSPollInterval
	NATPollInterval = time.Millisecond
	EKSPollInterval = time.Millisecond
	defer func() {
		NATPollInterval = origNATPoll
		EKSPollInterval = origEKSPoll
	}()

	ctx := context.Background()
	st, err := state.Load(filepath.Join(t.TempDir(), "state.env"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	// Simulate wiped state.env where only UNDERLAY_DESTROYED=true remains
	st.Set("UNDERLAY_DESTROYED", "true")

	cl := &intent.Cluster{
		APIVersion: intent.APIVersion,
		Kind:       intent.Kind,
		Cloud:      &intent.CloudSpec{Provider: intent.ProviderAWS},
		Metadata:   intent.Metadata{Name: "eks-stateless", Region: "ap-southeast-2"},
		Pattern:    intent.PatternExternalOnly,
		Network: intent.Network{
			VPCCidr: "10.80.0.0/16",
			DataPath: &intent.DataPathSpec{
				External: intent.SubnetSpec{CIDR: "10.80.100.0/24", AZ: "ap-southeast-2a"},
			},
		},
	}

	fakeEC2 := &fakePhaseEC2{}
	fakeIAM := &fakePhaseIAM{
		oidcProviders: []iamtypes.OpenIDConnectProviderListEntry{
			{Arn: aws.String("arn:aws:iam::123456789012:oidc-provider/oidc.eks.ap-southeast-2.amazonaws.com/id/EXAMPLED539D4633E53DE1B716D3041E")},
		},
	}
	fakeEKS := &fakePhaseEKS{clusterCreated: true}

	p := &Provider{
		Clients: &Clients{
			Region:   "ap-southeast-2",
			STS:      &fakePhaseSTS{},
			PhaseEC2: fakeEC2,
			PhaseIAM: fakeIAM,
			PhaseEKS: fakeEKS,
		},
	}

	if err := p.DestroyUnderlay(ctx, cl, st, false); err != nil {
		t.Fatalf("DestroyUnderlay stateless failed: %v", err)
	}
	if !fakeEC2.vpcDeleted {
		t.Fatalf("expected VPC to be discovered by tag and deleted when VPC_ID was missing from state")
	}
	if len(fakeIAM.deletedOIDCARNs) != 1 || fakeIAM.deletedOIDCARNs[0] != "arn:aws:iam::123456789012:oidc-provider/oidc.eks.ap-southeast-2.amazonaws.com/id/EXAMPLED539D4633E53DE1B716D3041E" {
		t.Fatalf("expected OIDC provider to be discovered via DescribeCluster and deleted, got %v", fakeIAM.deletedOIDCARNs)
	}
	if len(fakeIAM.deletedPolicyARNs) == 0 || fakeIAM.deletedPolicyARNs[0] != "arn:aws:iam::123456789012:policy/eks-stateless-cne-controller-policy" {
		t.Fatalf("expected IRSA policy ARN to be reconstructed and deleted, got %v", fakeIAM.deletedPolicyARNs)
	}
}

type fakeDepViolationErr struct{}

func (fakeDepViolationErr) Error() string {
	return "DependencyViolation: resource has mapped dependencies"
}
func (fakeDepViolationErr) ErrorCode() string             { return "DependencyViolation" }
func (fakeDepViolationErr) ErrorMessage() string          { return "resource has mapped dependencies" }
func (fakeDepViolationErr) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

type hardeningFakeEC2 struct {
	fakePhaseEC2
	respectFilters          bool
	vpcs                    []ec2types.Vpc
	deletedVPCs             []string
	deleteVPCFailuresLeft   int
	eices                   []ec2types.Ec2InstanceConnectEndpoint
	deletedEICEs            []string
	enis                    []ec2types.NetworkInterface
	deletedENIs             []string
	sgs                     []ec2types.SecurityGroup
	revokedIngressGroupIDs  []string
	revokedEgressGroupIDs   []string
	deletedSGs              []string
	subnets                 []ec2types.Subnet
	deletedSubnets          []string
	deleteSubnetFailCounts  map[string]int
	routeTables             []ec2types.RouteTable
	disassociatedRTAssocIDs []string
	deletedRouteTables      []string
	igws                    []ec2types.InternetGateway
	detachedIGWs            []string
	deletedIGWs             []string
	natGateways             []ec2types.NatGateway
}

func (f *hardeningFakeEC2) DescribeVpcs(_ context.Context, in *ec2.DescribeVpcsInput, _ ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	if in != nil && len(in.VpcIds) > 0 {
		want := make(map[string]bool, len(in.VpcIds))
		for _, id := range in.VpcIds {
			want[id] = true
		}
		var matched []ec2types.Vpc
		for _, v := range f.vpcs {
			if want[aws.ToString(v.VpcId)] {
				matched = append(matched, v)
			}
		}
		return &ec2.DescribeVpcsOutput{Vpcs: matched}, nil
	}
	if f.respectFilters && in != nil && len(in.Filters) > 0 {
		var matched []ec2types.Vpc
		for _, v := range f.vpcs {
			ok := true
			for _, flt := range in.Filters {
				name := aws.ToString(flt.Name)
				if strings.HasPrefix(name, "tag:") {
					tagKey := strings.TrimPrefix(name, "tag:")
					foundTag := false
					for _, t := range v.Tags {
						if aws.ToString(t.Key) == tagKey {
							for _, val := range flt.Values {
								if aws.ToString(t.Value) == val {
									foundTag = true
									break
								}
							}
						}
					}
					if !foundTag {
						ok = false
						break
					}
				}
			}
			if ok {
				matched = append(matched, v)
			}
		}
		return &ec2.DescribeVpcsOutput{Vpcs: matched}, nil
	}
	return &ec2.DescribeVpcsOutput{Vpcs: f.vpcs}, nil
}

func (f *hardeningFakeEC2) DeleteVpc(_ context.Context, in *ec2.DeleteVpcInput, _ ...func(*ec2.Options)) (*ec2.DeleteVpcOutput, error) {
	if f.deleteVPCFailuresLeft > 0 {
		f.deleteVPCFailuresLeft--
		return nil, fakeDepViolationErr{}
	}
	f.deletedVPCs = append(f.deletedVPCs, aws.ToString(in.VpcId))
	return &ec2.DeleteVpcOutput{}, nil
}

func (f *hardeningFakeEC2) DescribeInstanceConnectEndpoints(_ context.Context, _ *ec2.DescribeInstanceConnectEndpointsInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceConnectEndpointsOutput, error) {
	return &ec2.DescribeInstanceConnectEndpointsOutput{InstanceConnectEndpoints: f.eices}, nil
}

func (f *hardeningFakeEC2) DeleteInstanceConnectEndpoint(_ context.Context, in *ec2.DeleteInstanceConnectEndpointInput, _ ...func(*ec2.Options)) (*ec2.DeleteInstanceConnectEndpointOutput, error) {
	id := aws.ToString(in.InstanceConnectEndpointId)
	f.deletedEICEs = append(f.deletedEICEs, id)
	for i := range f.eices {
		if aws.ToString(f.eices[i].InstanceConnectEndpointId) == id {
			f.eices[i].State = ec2types.Ec2InstanceConnectEndpointStateDeleteComplete
		}
	}
	return &ec2.DeleteInstanceConnectEndpointOutput{}, nil
}

func (f *hardeningFakeEC2) DescribeNetworkInterfaces(_ context.Context, _ *ec2.DescribeNetworkInterfacesInput, _ ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error) {
	return &ec2.DescribeNetworkInterfacesOutput{NetworkInterfaces: f.enis}, nil
}

func (f *hardeningFakeEC2) DeleteNetworkInterface(_ context.Context, in *ec2.DeleteNetworkInterfaceInput, _ ...func(*ec2.Options)) (*ec2.DeleteNetworkInterfaceOutput, error) {
	f.deletedENIs = append(f.deletedENIs, aws.ToString(in.NetworkInterfaceId))
	return &ec2.DeleteNetworkInterfaceOutput{}, nil
}

func (f *hardeningFakeEC2) DescribeSecurityGroups(_ context.Context, in *ec2.DescribeSecurityGroupsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	if in != nil && len(in.GroupIds) > 0 {
		want := make(map[string]bool, len(in.GroupIds))
		for _, id := range in.GroupIds {
			want[id] = true
		}
		var matched []ec2types.SecurityGroup
		for _, sg := range f.sgs {
			if want[aws.ToString(sg.GroupId)] {
				matched = append(matched, sg)
			}
		}
		return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: matched}, nil
	}
	return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: f.sgs}, nil
}

func (f *hardeningFakeEC2) RevokeSecurityGroupIngress(_ context.Context, in *ec2.RevokeSecurityGroupIngressInput, _ ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupIngressOutput, error) {
	f.revokedIngressGroupIDs = append(f.revokedIngressGroupIDs, aws.ToString(in.GroupId))
	return &ec2.RevokeSecurityGroupIngressOutput{}, nil
}

func (f *hardeningFakeEC2) RevokeSecurityGroupEgress(_ context.Context, in *ec2.RevokeSecurityGroupEgressInput, _ ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupEgressOutput, error) {
	f.revokedEgressGroupIDs = append(f.revokedEgressGroupIDs, aws.ToString(in.GroupId))
	return &ec2.RevokeSecurityGroupEgressOutput{}, nil
}

func (f *hardeningFakeEC2) DeleteSecurityGroup(_ context.Context, in *ec2.DeleteSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error) {
	id := aws.ToString(in.GroupId)
	f.deletedSGs = append(f.deletedSGs, id)
	var kept []ec2types.SecurityGroup
	for _, sg := range f.sgs {
		if aws.ToString(sg.GroupId) != id {
			kept = append(kept, sg)
		}
	}
	f.sgs = kept
	return &ec2.DeleteSecurityGroupOutput{}, nil
}

func (f *hardeningFakeEC2) DescribeSubnets(_ context.Context, _ *ec2.DescribeSubnetsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	return &ec2.DescribeSubnetsOutput{Subnets: f.subnets}, nil
}

func (f *hardeningFakeEC2) DeleteSubnet(_ context.Context, in *ec2.DeleteSubnetInput, _ ...func(*ec2.Options)) (*ec2.DeleteSubnetOutput, error) {
	id := aws.ToString(in.SubnetId)
	if f.deleteSubnetFailCounts != nil && f.deleteSubnetFailCounts[id] > 0 {
		f.deleteSubnetFailCounts[id]--
		return nil, fakeDepViolationErr{}
	}
	f.deletedSubnets = append(f.deletedSubnets, id)
	return &ec2.DeleteSubnetOutput{}, nil
}

func (f *hardeningFakeEC2) DescribeRouteTables(_ context.Context, _ *ec2.DescribeRouteTablesInput, _ ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error) {
	return &ec2.DescribeRouteTablesOutput{RouteTables: f.routeTables}, nil
}

func (f *hardeningFakeEC2) DisassociateRouteTable(_ context.Context, in *ec2.DisassociateRouteTableInput, _ ...func(*ec2.Options)) (*ec2.DisassociateRouteTableOutput, error) {
	f.disassociatedRTAssocIDs = append(f.disassociatedRTAssocIDs, aws.ToString(in.AssociationId))
	return &ec2.DisassociateRouteTableOutput{}, nil
}

func (f *hardeningFakeEC2) DeleteRouteTable(_ context.Context, in *ec2.DeleteRouteTableInput, _ ...func(*ec2.Options)) (*ec2.DeleteRouteTableOutput, error) {
	f.deletedRouteTables = append(f.deletedRouteTables, aws.ToString(in.RouteTableId))
	return &ec2.DeleteRouteTableOutput{}, nil
}

func (f *hardeningFakeEC2) DescribeInternetGateways(_ context.Context, _ *ec2.DescribeInternetGatewaysInput, _ ...func(*ec2.Options)) (*ec2.DescribeInternetGatewaysOutput, error) {
	return &ec2.DescribeInternetGatewaysOutput{InternetGateways: f.igws}, nil
}

func (f *hardeningFakeEC2) DetachInternetGateway(_ context.Context, in *ec2.DetachInternetGatewayInput, _ ...func(*ec2.Options)) (*ec2.DetachInternetGatewayOutput, error) {
	f.detachedIGWs = append(f.detachedIGWs, aws.ToString(in.InternetGatewayId))
	return &ec2.DetachInternetGatewayOutput{}, nil
}

func (f *hardeningFakeEC2) DeleteInternetGateway(_ context.Context, in *ec2.DeleteInternetGatewayInput, _ ...func(*ec2.Options)) (*ec2.DeleteInternetGatewayOutput, error) {
	f.deletedIGWs = append(f.deletedIGWs, aws.ToString(in.InternetGatewayId))
	return &ec2.DeleteInternetGatewayOutput{}, nil
}

func (f *hardeningFakeEC2) DescribeNatGateways(_ context.Context, _ *ec2.DescribeNatGatewaysInput, _ ...func(*ec2.Options)) (*ec2.DescribeNatGatewaysOutput, error) {
	return &ec2.DescribeNatGatewaysOutput{NatGateways: f.natGateways}, nil
}

func TestCheckVPCQuota_ExceedsLimitAndAllowsReuseOrOverride(t *testing.T) {
	ctx := context.Background()
	st, err := state.Load(filepath.Join(t.TempDir(), "state.env"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	cl := &intent.Cluster{
		Metadata: intent.Metadata{Name: "target-cluster", Region: "ap-southeast-2"},
	}

	fiveVPCs := []ec2types.Vpc{
		{VpcId: aws.String("vpc-1"), Tags: []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String("one-vpc")}, {Key: aws.String(tags.KeyCluster), Value: aws.String("other-1")}}},
		{VpcId: aws.String("vpc-2"), Tags: []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String("two-vpc")}}},
		{VpcId: aws.String("vpc-3")},
		{VpcId: aws.String("vpc-4")},
		{VpcId: aws.String("vpc-5")},
	}
	ec2Fake := &hardeningFakeEC2{vpcs: fiveVPCs}
	clients := &Clients{Region: "ap-southeast-2", PhaseEC2: ec2Fake}

	// 1. 5 unrelated VPCs should fail with quota error.
	err = checkVPCQuota(ctx, cl, st, clients)
	if err == nil {
		t.Fatalf("expected checkVPCQuota to fail when 5 unrelated VPCs exist")
	}
	if !strings.Contains(err.Error(), "has 5 VPCs (quota limit 5") || !strings.Contains(err.Error(), "vpc-1 (one-vpc) [cluster=other-1]") {
		t.Fatalf("unexpected quota error message: %v", err)
	}

	// 2. BNKCTL_VPC_QUOTA=10 override should allow proceeding.
	t.Setenv("BNKCTL_VPC_QUOTA", "10")
	if err := checkVPCQuota(ctx, cl, st, clients); err != nil {
		t.Fatalf("expected BNKCTL_VPC_QUOTA=10 override to pass, got: %v", err)
	}
	t.Setenv("BNKCTL_VPC_QUOTA", "")

	// 3. Matching cluster tag on an existing VPC should allow reuse even at quota.
	ec2Fake.vpcs[4].Tags = []ec2types.Tag{{Key: aws.String(tags.KeyCluster), Value: aws.String("target-cluster")}}
	if err := checkVPCQuota(ctx, cl, st, clients); err != nil {
		t.Fatalf("expected reusable cluster VPC to bypass quota failure, got: %v", err)
	}
}

func TestPhase02VPCDown_SweepsBlockersAndDeletesMultipleVPCs(t *testing.T) {
	origPoll := VPCSweepRetryInterval
	VPCSweepRetryInterval = time.Millisecond
	defer func() { VPCSweepRetryInterval = origPoll }()

	ctx := context.Background()
	st, err := state.Load(filepath.Join(t.TempDir(), "state.env"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Set("VPC_ID", "vpc-state")
	st.Set("IGW_ID", "igw-state")

	cl := &intent.Cluster{
		Metadata: intent.Metadata{Name: "sweep-cluster", Region: "ap-southeast-2"},
	}

	ec2Fake := &hardeningFakeEC2{
		vpcs: []ec2types.Vpc{
			{VpcId: aws.String("vpc-state")},
			{VpcId: aws.String("vpc-orphan")},
		},
		deleteVPCFailuresLeft: 1, // First DeleteVpc fails with DependencyViolation -> triggers sweepVPCBlockers
		eices: []ec2types.Ec2InstanceConnectEndpoint{
			{InstanceConnectEndpointId: aws.String("eice-residual"), State: ec2types.Ec2InstanceConnectEndpointStateCreateComplete},
		},
		enis: []ec2types.NetworkInterface{
			{NetworkInterfaceId: aws.String("eni-avail-1"), Status: ec2types.NetworkInterfaceStatusAvailable},
		},
		sgs: []ec2types.SecurityGroup{
			{GroupId: aws.String("sg-default"), GroupName: aws.String("default")},
			{
				GroupId:   aws.String("sg-worker"),
				GroupName: aws.String("worker"),
				IpPermissions: []ec2types.IpPermission{
					{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(22), ToPort: aws.Int32(22), UserIdGroupPairs: []ec2types.UserIdGroupPair{{GroupId: aws.String("sg-jumphost")}}},
				},
				IpPermissionsEgress: []ec2types.IpPermission{
					{IpProtocol: aws.String("-1"), IpRanges: []ec2types.IpRange{{CidrIp: aws.String("0.0.0.0/0")}}},
				},
			},
			{GroupId: aws.String("sg-jumphost"), GroupName: aws.String("jumphost")},
		},
		subnets: []ec2types.Subnet{
			{SubnetId: aws.String("subnet-residual-1")},
		},
		routeTables: []ec2types.RouteTable{
			{
				RouteTableId: aws.String("rtb-main"),
				Associations: []ec2types.RouteTableAssociation{{Main: aws.Bool(true), RouteTableAssociationId: aws.String("rtbassoc-main")}},
			},
			{
				RouteTableId: aws.String("rtb-custom"),
				Associations: []ec2types.RouteTableAssociation{{Main: aws.Bool(false), RouteTableAssociationId: aws.String("rtbassoc-custom")}},
			},
		},
		igws: []ec2types.InternetGateway{
			{InternetGatewayId: aws.String("igw-orphan")},
		},
	}

	clients := &Clients{Region: "ap-southeast-2", PhaseEC2: ec2Fake}
	if err := Phase02VPCDown(ctx, cl, st, clients, false); err != nil {
		t.Fatalf("Phase02VPCDown failed: %v", err)
	}

	if len(ec2Fake.deletedEICEs) == 0 || ec2Fake.deletedEICEs[0] != "eice-residual" {
		t.Errorf("expected EICE eice-residual to be swept, got %v", ec2Fake.deletedEICEs)
	}
	if len(ec2Fake.deletedENIs) == 0 || ec2Fake.deletedENIs[0] != "eni-avail-1" {
		t.Errorf("expected available ENI eni-avail-1 to be swept, got %v", ec2Fake.deletedENIs)
	}
	if len(ec2Fake.revokedIngressGroupIDs) == 0 || ec2Fake.revokedIngressGroupIDs[0] != "sg-worker" {
		t.Errorf("expected ingress rules on sg-worker to be revoked, got %v", ec2Fake.revokedIngressGroupIDs)
	}
	if len(ec2Fake.deletedSGs) == 0 {
		t.Errorf("expected non-default SGs deleted, got %v", ec2Fake.deletedSGs)
	}
	for _, sg := range ec2Fake.deletedSGs {
		if sg == "sg-default" {
			t.Errorf("expected sg-default to be skipped, got %v", ec2Fake.deletedSGs)
		}
	}
	if len(ec2Fake.deletedSubnets) == 0 || ec2Fake.deletedSubnets[0] != "subnet-residual-1" {
		t.Errorf("expected residual subnet to be swept, got %v", ec2Fake.deletedSubnets)
	}
	if len(ec2Fake.disassociatedRTAssocIDs) == 0 || ec2Fake.disassociatedRTAssocIDs[0] != "rtbassoc-custom" {
		t.Errorf("expected non-main route table association to be disassociated, got %v", ec2Fake.disassociatedRTAssocIDs)
	}
	for _, rtb := range ec2Fake.deletedRouteTables {
		if rtb == "rtb-main" {
			t.Errorf("expected main route table rtb-main to be skipped, got %v", ec2Fake.deletedRouteTables)
		}
	}
	if len(ec2Fake.deletedVPCs) != 2 || ec2Fake.deletedVPCs[0] != "vpc-state" || ec2Fake.deletedVPCs[1] != "vpc-orphan" {
		t.Errorf("expected both vpc-state and vpc-orphan to be deleted, got %v", ec2Fake.deletedVPCs)
	}
	if st.Get("VPC_ID") != "" || st.Get("IGW_ID") != "" {
		t.Errorf("expected VPC_ID and IGW_ID cleared from state")
	}
}

func TestPhase03SubnetsDown_SweepsAvailableENIsAndDefersDependencyViolation(t *testing.T) {
	ctx := context.Background()
	st, err := state.Load(filepath.Join(t.TempDir(), "state.env"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Set("VPC_ID", "vpc-123")
	st.Set("BNK_EXT_SUBNET", "subnet-retry-ok")
	st.Set("BNK_INT_SUBNET", "subnet-stuck")

	cl := &intent.Cluster{
		Metadata: intent.Metadata{Name: "subnet-cluster", Region: "ap-southeast-2"},
	}

	ec2Fake := &hardeningFakeEC2{
		enis: []ec2types.NetworkInterface{
			{NetworkInterfaceId: aws.String("eni-subnet-avail"), Status: ec2types.NetworkInterfaceStatusAvailable},
		},
		subnets: []ec2types.Subnet{
			{SubnetId: aws.String("subnet-from-vpc")},
		},
		deleteSubnetFailCounts: map[string]int{
			"subnet-retry-ok": 1,   // succeeds on 2nd attempt
			"subnet-stuck":    100, // persistent DependencyViolation -> deferred to Phase02VPCDown without failing Phase03
		},
	}

	clients := &Clients{Region: "ap-southeast-2", PhaseEC2: ec2Fake}
	if err := Phase03SubnetsDown(ctx, cl, st, clients, false); err != nil {
		t.Fatalf("Phase03SubnetsDown should defer persistent DependencyViolation instead of failing, got: %v", err)
	}

	if len(ec2Fake.deletedENIs) == 0 || ec2Fake.deletedENIs[0] != "eni-subnet-avail" {
		t.Errorf("expected available subnet ENI to be deleted, got %v", ec2Fake.deletedENIs)
	}
	deletedSet := make(map[string]bool)
	for _, s := range ec2Fake.deletedSubnets {
		deletedSet[s] = true
	}
	if !deletedSet["subnet-retry-ok"] || !deletedSet["subnet-from-vpc"] {
		t.Errorf("expected subnet-retry-ok and subnet-from-vpc to be deleted, got %v", ec2Fake.deletedSubnets)
	}
	if st.Get("BNK_EXT_SUBNET") != "" || st.Get("BNK_INT_SUBNET") != "" {
		t.Errorf("expected subnet state keys to be cleared")
	}
}

func TestPhase17bJumphostDown_RevokesCrossSGReferences(t *testing.T) {
	ctx := context.Background()
	st, err := state.Load(filepath.Join(t.TempDir(), "state.env"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Set("VPC_ID", "vpc-123")
	st.Set("JUMPHOST_SG_ID", "sg-jumphost")

	cl := &intent.Cluster{
		Metadata: intent.Metadata{Name: "jh-cluster", Region: "ap-southeast-2"},
	}

	ec2Fake := &hardeningFakeEC2{
		sgs: []ec2types.SecurityGroup{
			{
				GroupId:   aws.String("sg-eks-node"),
				GroupName: aws.String("eks-node"),
				IpPermissions: []ec2types.IpPermission{
					{
						IpProtocol:       aws.String("tcp"),
						FromPort:         aws.Int32(22),
						ToPort:           aws.Int32(22),
						UserIdGroupPairs: []ec2types.UserIdGroupPair{{GroupId: aws.String("sg-jumphost")}},
					},
				},
			},
			{
				GroupId:   aws.String("sg-jumphost"),
				GroupName: aws.String("jh-cluster-jumphost-sg"),
			},
		},
	}

	clients := &Clients{Region: "ap-southeast-2", PhaseEC2: ec2Fake}
	if err := Phase17bJumphostDown(ctx, cl, st, clients, false); err != nil {
		t.Fatalf("Phase17bJumphostDown failed: %v", err)
	}

	if len(ec2Fake.revokedIngressGroupIDs) != 1 || ec2Fake.revokedIngressGroupIDs[0] != "sg-eks-node" {
		t.Errorf("expected cross-SG ingress reference on sg-eks-node to be revoked, got %v", ec2Fake.revokedIngressGroupIDs)
	}
	if len(ec2Fake.deletedSGs) != 1 || ec2Fake.deletedSGs[0] != "sg-jumphost" {
		t.Errorf("expected sg-jumphost to be deleted, got %v", ec2Fake.deletedSGs)
	}
	if st.Get("JUMPHOST_SG_ID") != "" {
		t.Errorf("expected JUMPHOST_SG_ID to be cleared")
	}
}

func TestHydrateMissingStateFromAWS_PartialStateWithVPCID(t *testing.T) {
	ctx := context.Background()
	st, err := state.Load(filepath.Join(t.TempDir(), "state.env"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	// Simulate partial state where VPC_ID was saved before a crash, but subnets/IGW/NAT were not.
	st.Set("VPC_ID", "vpc-partial")

	cl := &intent.Cluster{
		Metadata: intent.Metadata{Name: "partial-cluster", Region: "ap-southeast-2"},
	}

	ec2Fake := &hardeningFakeEC2{
		igws: []ec2types.InternetGateway{
			{InternetGatewayId: aws.String("igw-hydrated")},
		},
		subnets: []ec2types.Subnet{
			{
				SubnetId: aws.String("subnet-pub-0"),
				Tags:     []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String("partial-cluster-public-0")}},
			},
			{
				SubnetId: aws.String("subnet-priv-0"),
				Tags:     []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String("partial-cluster-private-0")}},
			},
			{
				SubnetId: aws.String("subnet-ext"),
				Tags:     []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String("partial-cluster-bnk-ext")}},
			},
			{
				SubnetId: aws.String("subnet-int"),
				Tags:     []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String("partial-cluster-bnk-int")}},
			},
		},
		natGateways: []ec2types.NatGateway{
			{
				NatGatewayId: aws.String("nat-hydrated"),
				State:        ec2types.NatGatewayStateAvailable,
				NatGatewayAddresses: []ec2types.NatGatewayAddress{
					{AllocationId: aws.String("eipalloc-hydrated")},
				},
			},
		},
	}

	clients := &Clients{
		Region:   "ap-southeast-2",
		STS:      &fakePhaseSTS{},
		PhaseEC2: ec2Fake,
		PhaseIAM: &fakePhaseIAM{},
		PhaseEKS: &fakePhaseEKS{},
	}

	hydrateMissingStateFromAWS(ctx, cl, st, clients)

	expected := map[string]string{
		"VPC_ID":           "vpc-partial",
		"IGW_ID":           "igw-hydrated",
		"PUBLIC_SUBNET_0":  "subnet-pub-0",
		"PUBLIC_SUBNETS":   "subnet-pub-0",
		"MGMT_SUBNET":      "subnet-pub-0",
		"PRIVATE_SUBNET_0": "subnet-priv-0",
		"PRIVATE_SUBNETS":  "subnet-priv-0",
		"BNK_EXT_SUBNET":   "subnet-ext",
		"SUBNET_EXT_ID":    "subnet-ext",
		"BNK_INT_SUBNET":   "subnet-int",
		"SUBNET_INT_ID":    "subnet-int",
		"NAT_GW_ID":        "nat-hydrated",
		"NAT_EIP_ALLOC_ID": "eipalloc-hydrated",
	}
	for k, want := range expected {
		if got := st.Get(k); got != want {
			t.Errorf("state[%s] = %q, want %q", k, got, want)
		}
	}
}

func TestFindVPCsByTag_CollectsAllMatchingTagConventions(t *testing.T) {
	ctx := context.Background()
	ec2Fake := &hardeningFakeEC2{
		respectFilters: true,
		vpcs: []ec2types.Vpc{
			{
				VpcId: aws.String("vpc-primary"),
				Tags:  []ec2types.Tag{{Key: aws.String(tags.KeyCluster), Value: aws.String("multi-cluster")}},
			},
			{
				VpcId: aws.String("vpc-legacy-tag"),
				Tags:  []ec2types.Tag{{Key: aws.String("bnk.f5.com/cluster"), Value: aws.String("multi-cluster")}},
			},
			{
				VpcId: aws.String("vpc-name-tag"),
				Tags:  []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String("multi-cluster-vpc")}},
			},
			{
				VpcId: aws.String("vpc-unrelated"),
				Tags:  []ec2types.Tag{{Key: aws.String(tags.KeyCluster), Value: aws.String("other-cluster")}},
			},
		},
	}

	ids, err := findVPCsByTag(ctx, ec2Fake, "multi-cluster")
	if err != nil {
		t.Fatalf("findVPCsByTag: %v", err)
	}
	want := []string{"vpc-primary", "vpc-legacy-tag", "vpc-name-tag"}
	if len(ids) != len(want) {
		t.Fatalf("findVPCsByTag = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("ids[%d] = %q, want %q", i, ids[i], want[i])
		}
	}
}

func TestPhase02VPC_ReusesLegacyClusterTaggedVPC(t *testing.T) {
	ctx := context.Background()
	st, err := state.Load(filepath.Join(t.TempDir(), "state.env"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	cl := &intent.Cluster{
		Metadata: intent.Metadata{Name: "legacy-cluster", Region: "ap-southeast-2"},
		Network:  intent.Network{VPCCidr: "10.0.0.0/16"},
	}
	ec2Fake := &hardeningFakeEC2{
		respectFilters: true,
		vpcs: []ec2types.Vpc{
			{
				VpcId: aws.String("vpc-legacy-reuse"),
				Tags:  []ec2types.Tag{{Key: aws.String("bnk.f5.com/cluster"), Value: aws.String("legacy-cluster")}},
			},
		},
	}
	clients := &Clients{Region: "ap-southeast-2", PhaseEC2: ec2Fake}
	if err := Phase02VPC(ctx, cl, st, clients, false); err != nil {
		t.Fatalf("Phase02VPC: %v", err)
	}
	if got := st.Get("VPC_ID"); got != "vpc-legacy-reuse" {
		t.Errorf("VPC_ID = %q, want vpc-legacy-reuse", got)
	}
}

func TestPhase17bJumphostDown_TagDiscoveryAndMissingVPCID(t *testing.T) {
	ctx := context.Background()
	st, err := state.Load(filepath.Join(t.TempDir(), "state.env"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	// Neither VPC_ID nor JUMPHOST_SG_ID is in state.env.
	cl := &intent.Cluster{
		Metadata: intent.Metadata{Name: "jh-discover", Region: "ap-southeast-2"},
	}

	ec2Fake := &hardeningFakeEC2{
		sgs: []ec2types.SecurityGroup{
			{
				GroupId:   aws.String("sg-eks-node"),
				GroupName: aws.String("eks-node"),
				VpcId:     aws.String("vpc-discovered"),
				IpPermissions: []ec2types.IpPermission{
					{
						IpProtocol:       aws.String("tcp"),
						FromPort:         aws.Int32(22),
						ToPort:           aws.Int32(22),
						UserIdGroupPairs: []ec2types.UserIdGroupPair{{GroupId: aws.String("sg-jumphost")}},
					},
				},
			},
			{
				GroupId:   aws.String("sg-jumphost"),
				GroupName: aws.String("jh-discover-jumphost-sg"),
				VpcId:     aws.String("vpc-discovered"),
				IpPermissions: []ec2types.IpPermission{
					{
						IpProtocol: aws.String("-1"),
						IpRanges:   []ec2types.IpRange{{CidrIp: aws.String("10.0.0.0/16")}},
					},
				},
			},
		},
	}

	clients := &Clients{Region: "ap-southeast-2", PhaseEC2: ec2Fake}
	if err := Phase17bJumphostDown(ctx, cl, st, clients, false); err != nil {
		t.Fatalf("Phase17bJumphostDown failed: %v", err)
	}

	// Only sg-jumphost should be deleted (not sg-eks-node).
	if len(ec2Fake.deletedSGs) != 1 || ec2Fake.deletedSGs[0] != "sg-jumphost" {
		t.Errorf("expected only sg-jumphost to be deleted, got %v", ec2Fake.deletedSGs)
	}
	// Both sg-jumphost's own ingress and sg-eks-node's cross-reference should be revoked once each.
	if len(ec2Fake.revokedIngressGroupIDs) != 2 ||
		ec2Fake.revokedIngressGroupIDs[0] != "sg-jumphost" ||
		ec2Fake.revokedIngressGroupIDs[1] != "sg-eks-node" {
		t.Errorf("expected ingress revocations [sg-jumphost, sg-eks-node], got %v", ec2Fake.revokedIngressGroupIDs)
	}
}
