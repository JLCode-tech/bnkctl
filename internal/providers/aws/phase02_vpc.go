package aws

import (
	"context"
	"fmt"

	"github.com/JLCode-tech/bnkctl/internal/providers/aws/awsmw"
	"github.com/JLCode-tech/bnkctl/internal/providers/aws/tags"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Phase02VPC creates (or re-uses) the cluster VPC and enables DNS support and hostnames.
func Phase02VPC(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 02 vpc: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 02 vpc: state is nil")
	}
	if dryRun {
		return nil
	}
	if c == nil || c.PhaseEC2 == nil {
		return fmt.Errorf("phase 02 vpc: EC2 client is nil")
	}

	if existing := st.Get("VPC_ID"); existing != "" {
		out, err := c.PhaseEC2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
			VpcIds: []string{existing},
		})
		if err == nil && len(out.Vpcs) > 0 {
			return nil
		}
		if err != nil && !awsmw.IsNotFound(err) {
			return fmt.Errorf("phase 02 vpc: describing existing VPC %s: %w", existing, err)
		}
		st.Delete("VPC_ID")
	}

	vpcName := cl.Metadata.Name + "-vpc"
	t := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, map[string]string{
		"Name": vpcName,
	})

	createOut, err := c.PhaseEC2.CreateVpc(ctx, &ec2.CreateVpcInput{
		CidrBlock:         aws.String(cl.Network.VPCCidr),
		TagSpecifications: tags.ToTagSpec(ec2types.ResourceTypeVpc, t),
	})
	if err != nil {
		return fmt.Errorf("phase 02 vpc: CreateVpc: %w", err)
	}
	if createOut.Vpc == nil || createOut.Vpc.VpcId == nil {
		return fmt.Errorf("phase 02 vpc: CreateVpc returned nil VpcId")
	}
	vpcID := *createOut.Vpc.VpcId

	if _, err := c.PhaseEC2.ModifyVpcAttribute(ctx, &ec2.ModifyVpcAttributeInput{
		VpcId:            aws.String(vpcID),
		EnableDnsSupport: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)},
	}); err != nil {
		return fmt.Errorf("phase 02 vpc: EnableDnsSupport on %s: %w", vpcID, err)
	}
	if _, err := c.PhaseEC2.ModifyVpcAttribute(ctx, &ec2.ModifyVpcAttributeInput{
		VpcId:              aws.String(vpcID),
		EnableDnsHostnames: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)},
	}); err != nil {
		return fmt.Errorf("phase 02 vpc: EnableDnsHostnames on %s: %w", vpcID, err)
	}

	st.Set("VPC_ID", vpcID)
	return st.Save()
}

// Phase02VPCDown deletes any remaining non-default security groups in the VPC and then deletes the VPC.
func Phase02VPCDown(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if st == nil {
		return fmt.Errorf("phase 02 vpc down: state is nil")
	}
	vpcID := st.Get("VPC_ID")
	if vpcID == "" || dryRun {
		return nil
	}
	if c == nil || c.PhaseEC2 == nil {
		return fmt.Errorf("phase 02 vpc down: EC2 client is nil")
	}

	if sgOut, serr := c.PhaseEC2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
		},
	}); serr == nil && sgOut != nil {
		var nonDefaultSGs []ec2types.SecurityGroup
		for _, sg := range sgOut.SecurityGroups {
			if sg.GroupId == nil || (sg.GroupName != nil && *sg.GroupName == "default") {
				continue
			}
			nonDefaultSGs = append(nonDefaultSGs, sg)
		}
		for _, sg := range nonDefaultSGs {
			if len(sg.IpPermissions) > 0 {
				_, _ = c.PhaseEC2.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
					GroupId:       sg.GroupId,
					IpPermissions: sg.IpPermissions,
				})
			}
			if len(sg.IpPermissionsEgress) > 0 {
				_, _ = c.PhaseEC2.RevokeSecurityGroupEgress(ctx, &ec2.RevokeSecurityGroupEgressInput{
					GroupId:       sg.GroupId,
					IpPermissions: sg.IpPermissionsEgress,
				})
			}
		}
		for _, sg := range nonDefaultSGs {
			_, _ = c.PhaseEC2.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{
				GroupId: sg.GroupId,
			})
		}
	}

	_, err := c.PhaseEC2.DeleteVpc(ctx, &ec2.DeleteVpcInput{
		VpcId: aws.String(vpcID),
	})
	if err != nil && !awsmw.IsNotFound(err) {
		return fmt.Errorf("phase 02 vpc down: DeleteVpc %s: %w", vpcID, err)
	}
	st.Delete("VPC_ID")
	st.Delete("SG_BNK_DATA")
	st.Delete("EKS_SECURITY_GROUP")
	return st.Save()
}
