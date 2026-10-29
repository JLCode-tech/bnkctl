package aws

import (
	"context"
	"fmt"
	"time"

	"github.com/JLCode-tech/bnkctl/internal/providers/aws/awsmw"
	"github.com/JLCode-tech/bnkctl/internal/providers/aws/tags"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// VPCSweepRetryInterval is the delay between VPC blocker sweeps when DeleteVpc returns DependencyViolation.
var VPCSweepRetryInterval = 2 * time.Second

// VPCSweepMaxAttempts is the maximum number of sweep + DeleteVpc attempts per VPC.
var VPCSweepMaxAttempts = 3

// findExistingTaggedVPC looks up an existing VPC whose tags explicitly match clusterName.
func findExistingTaggedVPC(ctx context.Context, ec2c PhaseEC2API, clusterName string) (string, error) {
	if ec2c == nil || clusterName == "" {
		return "", nil
	}
	filters := [][]ec2types.Filter{
		{tags.ClusterFilter(clusterName)},
		{{Name: aws.String("tag:bnk.f5.com/cluster"), Values: []string{clusterName}}},
	}
	var firstErr error
	for _, f := range filters {
		out, err := ec2c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
			Filters: f,
		})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if out == nil {
			continue
		}
		for _, v := range out.Vpcs {
			if v.VpcId == nil || *v.VpcId == "" {
				continue
			}
			for _, t := range v.Tags {
				if (aws.ToString(t.Key) == tags.KeyCluster || aws.ToString(t.Key) == "bnk.f5.com/cluster") &&
					aws.ToString(t.Value) == clusterName {
					return *v.VpcId, nil
				}
			}
		}
	}
	return "", firstErr
}

// findVPCsByTag returns all VPC IDs matching the cluster's tags (awsbnkctl:cluster, bnk.f5.com/cluster, or Name=<cluster>-vpc).
func findVPCsByTag(ctx context.Context, ec2c PhaseEC2API, clusterName string) ([]string, error) {
	if ec2c == nil || clusterName == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	var ids []string
	addFrom := func(out *ec2.DescribeVpcsOutput) {
		if out == nil {
			return
		}
		for _, v := range out.Vpcs {
			if v.VpcId != nil && *v.VpcId != "" && !seen[*v.VpcId] {
				seen[*v.VpcId] = true
				ids = append(ids, *v.VpcId)
			}
		}
	}

	out, err := ec2c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []ec2types.Filter{tags.ClusterFilter(clusterName)},
	})
	if err != nil {
		return nil, err
	}
	addFrom(out)

	if out2, err2 := ec2c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:bnk.f5.com/cluster"), Values: []string{clusterName}},
		},
	}); err2 == nil {
		addFrom(out2)
	}
	if out3, err3 := ec2c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:Name"), Values: []string{clusterName + "-vpc"}},
		},
	}); err3 == nil {
		addFrom(out3)
	}
	return ids, nil
}

func enableVPCDNS(ctx context.Context, ec2c PhaseEC2API, vpcID string) error {
	if _, err := ec2c.ModifyVpcAttribute(ctx, &ec2.ModifyVpcAttributeInput{
		VpcId:            aws.String(vpcID),
		EnableDnsSupport: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)},
	}); err != nil {
		return fmt.Errorf("phase 02 vpc: EnableDnsSupport on %s: %w", vpcID, err)
	}
	if _, err := ec2c.ModifyVpcAttribute(ctx, &ec2.ModifyVpcAttributeInput{
		VpcId:              aws.String(vpcID),
		EnableDnsHostnames: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)},
	}); err != nil {
		return fmt.Errorf("phase 02 vpc: EnableDnsHostnames on %s: %w", vpcID, err)
	}
	return nil
}

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

	if existingID, err := findExistingTaggedVPC(ctx, c.PhaseEC2, cl.Metadata.Name); err == nil && existingID != "" {
		if err := enableVPCDNS(ctx, c.PhaseEC2, existingID); err != nil {
			return err
		}
		st.Set("VPC_ID", existingID)
		return st.Save()
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

	if err := enableVPCDNS(ctx, c.PhaseEC2, vpcID); err != nil {
		return err
	}

	st.Set("VPC_ID", vpcID)
	return st.Save()
}

// sweepVPCBlockers removes residual EICEs, available ENIs, non-default security groups,
// subnets, non-main route tables, and attached internet gateways inside vpcID so DeleteVpc can succeed.
func sweepVPCBlockers(ctx context.Context, c *Clients, vpcID string) {
	if c == nil || c.PhaseEC2 == nil || vpcID == "" {
		return
	}
	vpcFilter := []ec2types.Filter{
		{Name: aws.String("vpc-id"), Values: []string{vpcID}},
	}

	// 1. Delete any lingering EC2 Instance Connect Endpoints in the VPC.
	if eiceOut, err := c.PhaseEC2.DescribeInstanceConnectEndpoints(ctx, &ec2.DescribeInstanceConnectEndpointsInput{
		Filters: vpcFilter,
	}); err == nil && eiceOut != nil {
		for _, ep := range eiceOut.InstanceConnectEndpoints {
			if ep.InstanceConnectEndpointId == nil || ep.State == ec2types.Ec2InstanceConnectEndpointStateDeleteComplete {
				continue
			}
			id := *ep.InstanceConnectEndpointId
			if ep.State != ec2types.Ec2InstanceConnectEndpointStateDeleteInProgress {
				_, _ = c.PhaseEC2.DeleteInstanceConnectEndpoint(ctx, &ec2.DeleteInstanceConnectEndpointInput{
					InstanceConnectEndpointId: aws.String(id),
				})
			}
			_ = waitForEICEState(ctx, c, id, ec2types.Ec2InstanceConnectEndpointStateDeleteComplete)
		}
	}

	// 1b. Terminate any lingering EC2 instances in the VPC (e.g., demo stranger instance).
	if instOut, err := c.PhaseEC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: vpcFilter,
	}); err == nil && instOut != nil {
		var toTerminate []string
		for _, r := range instOut.Reservations {
			for _, inst := range r.Instances {
				if inst.InstanceId == nil || inst.State == nil {
					continue
				}
				if inst.State.Name != ec2types.InstanceStateNameTerminated && inst.State.Name != ec2types.InstanceStateNameShuttingDown {
					toTerminate = append(toTerminate, *inst.InstanceId)
				}
			}
		}
		if len(toTerminate) > 0 {
			_, _ = c.PhaseEC2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
				InstanceIds: toTerminate,
			})
			for _, id := range toTerminate {
				_ = waitForInstanceState(ctx, c, id, ec2types.InstanceStateNameTerminated)
			}
		}
	}

	// 2. Detach any non-primary attached ENIs and delete all available ENIs in the VPC.
	if eniOut, err := c.PhaseEC2.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
		Filters: vpcFilter,
	}); err == nil && eniOut != nil {
		for _, eni := range eniOut.NetworkInterfaces {
			if eni.NetworkInterfaceId == nil {
				continue
			}
			if eni.Status != ec2types.NetworkInterfaceStatusAvailable && eni.Attachment != nil && eni.Attachment.AttachmentId != nil {
				if eni.Attachment.DeviceIndex == nil || *eni.Attachment.DeviceIndex != 0 {
					_, _ = c.PhaseEC2.DetachNetworkInterface(ctx, &ec2.DetachNetworkInterfaceInput{
						AttachmentId: eni.Attachment.AttachmentId,
						Force:        aws.Bool(true),
					})
					_ = waitForENIAvailable(ctx, c, *eni.NetworkInterfaceId)
				}
			}
			_, _ = c.PhaseEC2.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{
				NetworkInterfaceId: eni.NetworkInterfaceId,
			})
		}
	}

	// 3. Revoke all rules on non-default security groups first (breaking circular/cross-SG refs), then delete them.
	if sgOut, err := c.PhaseEC2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: vpcFilter,
	}); err == nil && sgOut != nil {
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

	// 4. Delete any remaining subnets in the VPC.
	if subOut, err := c.PhaseEC2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: vpcFilter,
	}); err == nil && subOut != nil {
		for _, s := range subOut.Subnets {
			if s.SubnetId == nil {
				continue
			}
			_, _ = c.PhaseEC2.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{
				SubnetId: s.SubnetId,
			})
		}
	}

	// 5. Disassociate and delete any remaining non-main route tables in the VPC.
	if rtOut, err := c.PhaseEC2.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		Filters: vpcFilter,
	}); err == nil && rtOut != nil {
		for _, rt := range rtOut.RouteTables {
			if rt.RouteTableId == nil {
				continue
			}
			isMain := false
			for _, assoc := range rt.Associations {
				if aws.ToBool(assoc.Main) {
					isMain = true
					continue
				}
				if assoc.RouteTableAssociationId != nil {
					_, _ = c.PhaseEC2.DisassociateRouteTable(ctx, &ec2.DisassociateRouteTableInput{
						AssociationId: assoc.RouteTableAssociationId,
					})
				}
			}
			if !isMain {
				_, _ = c.PhaseEC2.DeleteRouteTable(ctx, &ec2.DeleteRouteTableInput{
					RouteTableId: rt.RouteTableId,
				})
			}
		}
	}

	// 6. Detach and delete any remaining internet gateways attached to the VPC.
	if igwOut, err := c.PhaseEC2.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("attachment.vpc-id"), Values: []string{vpcID}},
		},
	}); err == nil && igwOut != nil {
		for _, igw := range igwOut.InternetGateways {
			if igw.InternetGatewayId == nil {
				continue
			}
			_, _ = c.PhaseEC2.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{
				InternetGatewayId: igw.InternetGatewayId,
				VpcId:             aws.String(vpcID),
			})
			_, _ = c.PhaseEC2.DeleteInternetGateway(ctx, &ec2.DeleteInternetGatewayInput{
				InternetGatewayId: igw.InternetGatewayId,
			})
		}
	}
}

// Phase02VPCDown sweeps residual blockers and deletes the cluster VPC (plus any duplicate tagged VPCs).
func Phase02VPCDown(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if st == nil {
		return fmt.Errorf("phase 02 vpc down: state is nil")
	}
	if dryRun {
		return nil
	}

	seen := map[string]bool{}
	var vpcIDs []string
	addVPC := func(id string) {
		if id != "" && id != "dry-run-vpc" && !seen[id] {
			seen[id] = true
			vpcIDs = append(vpcIDs, id)
		}
	}

	addVPC(st.Get("VPC_ID"))
	if cl != nil && cl.Metadata.Name != "" && c != nil && c.PhaseEC2 != nil {
		if tagged, err := findVPCsByTag(ctx, c.PhaseEC2, cl.Metadata.Name); err == nil {
			for _, id := range tagged {
				addVPC(id)
			}
		}
	}

	if len(vpcIDs) == 0 {
		st.Delete("VPC_ID")
		st.Delete("IGW_ID")
		st.Delete("SG_BNK_DATA")
		st.Delete("EKS_SECURITY_GROUP")
		return st.Save()
	}
	if c == nil || c.PhaseEC2 == nil {
		return fmt.Errorf("phase 02 vpc down: EC2 client is nil")
	}

	maxAttempts := VPCSweepMaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	for _, vpcID := range vpcIDs {
		var err error
		for attempt := 0; attempt < maxAttempts; attempt++ {
			sweepVPCBlockers(ctx, c, vpcID)
			_, err = c.PhaseEC2.DeleteVpc(ctx, &ec2.DeleteVpcInput{
				VpcId: aws.String(vpcID),
			})
			if err == nil || awsmw.IsNotFound(err) {
				err = nil
				break
			}
			if !awsmw.IsDependencyViolation(err) {
				return fmt.Errorf("phase 02 vpc down: DeleteVpc %s: %w", vpcID, err)
			}
			if attempt+1 < maxAttempts && VPCSweepRetryInterval > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(VPCSweepRetryInterval):
				}
			}
		}
		if err != nil && !awsmw.IsNotFound(err) {
			return fmt.Errorf("phase 02 vpc down: DeleteVpc %s: %w", vpcID, err)
		}
	}

	st.Delete("VPC_ID")
	st.Delete("IGW_ID")
	st.Delete("SG_BNK_DATA")
	st.Delete("EKS_SECURITY_GROUP")
	return st.Save()
}
