package aws

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/JLCode-tech/bnkctl/internal/providers/aws/awsmw"
	"github.com/JLCode-tech/bnkctl/internal/providers/aws/tags"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
)

const hostDeviceTagKey = "f5-cne-device"

// TMMDeviceIndices returns the EC2 ENI device indices for the external and internal TMM NICs.
func TMMDeviceIndices(_ string) (extIdx, intIdx int32) {
	return 3, 2
}

func ensureDataplaneSG(ctx context.Context, c *Clients, st *state.State, cl *intent.Cluster, vpcID string) (string, error) {
	sgName := cl.Metadata.Name + "-bnk-datapath-sg"
	if existing := st.Get("BNK_DATAPATH_SG_ID"); existing != "" {
		out, err := c.PhaseEC2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
			GroupIds: []string{existing},
		})
		if err == nil && len(out.SecurityGroups) > 0 {
			return existing, nil
		}
		if err != nil && !awsmw.IsNotFound(err) {
			return "", fmt.Errorf("DescribeSecurityGroups %s: %w", existing, err)
		}
		st.Delete("BNK_DATAPATH_SG_ID")
	}

	t := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, map[string]string{
		"Name": sgName,
	})
	createOut, err := c.PhaseEC2.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:         aws.String(sgName),
		Description:       aws.String("BNK TMM data-path ENI security group"),
		VpcId:             aws.String(vpcID),
		TagSpecifications: tags.ToTagSpec(ec2types.ResourceTypeSecurityGroup, t),
	})
	if err != nil {
		return "", fmt.Errorf("CreateSecurityGroup %s: %w", sgName, err)
	}
	if createOut.GroupId == nil {
		return "", fmt.Errorf("CreateSecurityGroup %s returned nil GroupId", sgName)
	}
	sgID := *createOut.GroupId

	_, ierr := c.PhaseEC2.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []ec2types.IpPermission{
			{
				IpProtocol: aws.String("-1"),
				IpRanges: []ec2types.IpRange{
					{
						CidrIp:      aws.String(cl.Network.VPCCidr),
						Description: aws.String("Allow all intra-VPC traffic to TMM data-path ENIs"),
					},
				},
			},
		},
	})
	if ierr != nil && !strings.Contains(ierr.Error(), "InvalidPermission.Duplicate") {
		return "", fmt.Errorf("AuthorizeSecurityGroupIngress on %s: %w", sgID, ierr)
	}

	st.Set("BNK_DATAPATH_SG_ID", sgID)
	if err := st.Save(); err != nil {
		return "", err
	}
	return sgID, nil
}

func authorizeEKSNodeSGDataPathIngress(ctx context.Context, c *Clients, cl *intent.Cluster, vpcID string) error {
	out, err := c.PhaseEC2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
			{Name: aws.String("tag:kubernetes.io/cluster/" + cl.Metadata.Name), Values: []string{"owned"}},
		},
	})
	if err != nil {
		return fmt.Errorf("finding EKS cluster security group in VPC %s: %w", vpcID, err)
	}
	for _, sg := range out.SecurityGroups {
		if sg.GroupId == nil {
			continue
		}
		_, ierr := c.PhaseEC2.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
			GroupId: sg.GroupId,
			IpPermissions: []ec2types.IpPermission{
				{
					IpProtocol: aws.String("-1"),
					IpRanges: []ec2types.IpRange{
						{
							CidrIp:      aws.String(cl.Network.VPCCidr),
							Description: aws.String("Allow intra-VPC traffic from BNK data-path subnets to worker primary ENIs"),
						},
					},
				},
			},
		})
		if ierr != nil && !strings.Contains(ierr.Error(), "InvalidPermission.Duplicate") {
			return fmt.Errorf("AuthorizeSecurityGroupIngress on EKS SG %s: %w", *sg.GroupId, ierr)
		}
	}
	return nil
}

func ensureSecondaryENI(
	ctx context.Context,
	c *Clients,
	st *state.State,
	cl *intent.Cluster,
	instanceID string,
	subnetID string,
	sgID string,
	deviceIndex int32,
	privateIP string,
	name string,
	eniStateKey string,
	attachStateKey string,
	macStateKey string,
	ipStateKey string,
	extraTags map[string]string,
) (string, error) {
	if existing := st.Get(eniStateKey); existing != "" {
		out, err := c.PhaseEC2.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
			NetworkInterfaceIds: []string{existing},
		})
		if err == nil && len(out.NetworkInterfaces) > 0 {
			eni := out.NetworkInterfaces[0]
			if eni.Attachment != nil && eni.Attachment.InstanceId != nil && *eni.Attachment.InstanceId == instanceID {
				if eni.MacAddress != nil {
					st.Set(macStateKey, *eni.MacAddress)
				}
				if eni.PrivateIpAddress != nil {
					st.Set(ipStateKey, *eni.PrivateIpAddress)
				}
				_ = st.Save()
				return existing, nil
			}
		}
		if err != nil && !awsmw.IsNotFound(err) {
			return "", fmt.Errorf("DescribeNetworkInterfaces %s: %w", existing, err)
		}
		st.Delete(eniStateKey)
		st.Delete(attachStateKey)
	}

	if instOut, ierr := c.PhaseEC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}); ierr == nil {
		for _, r := range instOut.Reservations {
			for _, inst := range r.Instances {
				for _, ni := range inst.NetworkInterfaces {
					if ni.Attachment != nil && ni.Attachment.DeviceIndex != nil && *ni.Attachment.DeviceIndex == deviceIndex && ni.NetworkInterfaceId != nil {
						eniID := *ni.NetworkInterfaceId
						if ni.SubnetId != nil && *ni.SubnetId == subnetID {
							_, _ = c.PhaseEC2.CreateTags(ctx, &ec2.CreateTagsInput{
								Resources: []string{eniID},
								Tags: []ec2types.Tag{
									{Key: aws.String("node.k8s.amazonaws.com/no_manage"), Value: aws.String("true")},
								},
							})
							st.Set(eniStateKey, eniID)
							if ni.Attachment.AttachmentId != nil {
								st.Set(attachStateKey, *ni.Attachment.AttachmentId)
							}
							if ni.MacAddress != nil {
								st.Set(macStateKey, *ni.MacAddress)
							}
							if ni.PrivateIpAddress != nil {
								st.Set(ipStateKey, *ni.PrivateIpAddress)
							}
							_ = st.Save()
							return eniID, nil
						}
						// Stray ENI in wrong subnet (e.g. attached by aws-node warm pool) — detach & delete it.
						_ = detachAndDeleteENIByID(ctx, c, eniID)
					}
				}
			}
		}
	}

	mergedExtra := map[string]string{"Name": name}
	for k, v := range extraTags {
		mergedExtra[k] = v
	}
	t := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, mergedExtra)

	createIn := &ec2.CreateNetworkInterfaceInput{
		SubnetId:          aws.String(subnetID),
		Groups:            []string{sgID},
		Description:       aws.String(name),
		TagSpecifications: tags.ToTagSpec(ec2types.ResourceTypeNetworkInterface, t),
	}
	if privateIP != "" {
		createIn.PrivateIpAddress = aws.String(privateIP)
	}

	createOut, err := c.PhaseEC2.CreateNetworkInterface(ctx, createIn)
	if err != nil {
		return "", fmt.Errorf("CreateNetworkInterface %s: %w", name, err)
	}
	if createOut.NetworkInterface == nil || createOut.NetworkInterface.NetworkInterfaceId == nil {
		return "", fmt.Errorf("CreateNetworkInterface %s returned nil NetworkInterfaceId", name)
	}
	eniID := *createOut.NetworkInterface.NetworkInterfaceId
	if createOut.NetworkInterface.MacAddress != nil {
		st.Set(macStateKey, *createOut.NetworkInterface.MacAddress)
	}
	if createOut.NetworkInterface.PrivateIpAddress != nil {
		st.Set(ipStateKey, *createOut.NetworkInterface.PrivateIpAddress)
	}
	st.Set(eniStateKey, eniID)
	if err := st.Save(); err != nil {
		return "", err
	}

	attachOut, err := c.PhaseEC2.AttachNetworkInterface(ctx, &ec2.AttachNetworkInterfaceInput{
		NetworkInterfaceId: aws.String(eniID),
		InstanceId:         aws.String(instanceID),
		DeviceIndex:        aws.Int32(deviceIndex),
	})
	if err != nil {
		return "", fmt.Errorf("AttachNetworkInterface %s to %s (deviceIndex=%d): %w", eniID, instanceID, deviceIndex, err)
	}
	if attachOut.AttachmentId != nil {
		st.Set(attachStateKey, *attachOut.AttachmentId)
		if err := st.Save(); err != nil {
			return "", err
		}
	}

	if _, err := c.PhaseEC2.ModifyNetworkInterfaceAttribute(ctx, &ec2.ModifyNetworkInterfaceAttributeInput{
		NetworkInterfaceId: aws.String(eniID),
		SourceDestCheck:    &ec2types.AttributeBooleanValue{Value: aws.Bool(false)},
	}); err != nil {
		return "", fmt.Errorf("disabling SourceDestCheck on %s: %w", eniID, err)
	}

	return eniID, nil
}

func recordPrimaryENIMAC(ctx context.Context, c *Clients, st *state.State, instanceID string) error {
	out, err := c.PhaseEC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return fmt.Errorf("DescribeInstances %s: %w", instanceID, err)
	}
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			for _, ni := range inst.NetworkInterfaces {
				if ni.Attachment != nil && ni.Attachment.DeviceIndex != nil && *ni.Attachment.DeviceIndex == 0 {
					if ni.MacAddress != nil {
						st.Set("PRIMARY_ENI_MAC", *ni.MacAddress)
					}
					if ni.NetworkInterfaceId != nil {
						st.Set("PRIMARY_ENI_ID", *ni.NetworkInterfaceId)
						_, _ = c.PhaseEC2.ModifyNetworkInterfaceAttribute(ctx, &ec2.ModifyNetworkInterfaceAttributeInput{
							NetworkInterfaceId: ni.NetworkInterfaceId,
							SourceDestCheck:    &ec2types.AttributeBooleanValue{Value: aws.Bool(false)},
						})
					}
					return st.Save()
				}
			}
		}
	}
	return nil
}

// Phase17SecondaryENIs creates, tags, and attaches the external (and optional internal) secondary ENIs to the TMM worker node.
func Phase17SecondaryENIs(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 17 secondary enis: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 17 secondary enis: state is nil")
	}
	if !cl.HasExternalDataPath() || dryRun {
		return nil
	}
	if c == nil || c.PhaseEC2 == nil {
		return fmt.Errorf("phase 17 secondary enis: EC2 client is nil")
	}

	vpcID := st.Get("VPC_ID")
	if vpcID == "" {
		return fmt.Errorf("phase 17 secondary enis: VPC_ID not found in state")
	}
	instanceID := st.Get("TMM_INSTANCE_ID")
	if instanceID == "" {
		return fmt.Errorf("phase 17 secondary enis: TMM_INSTANCE_ID not found in state (did phase 16 run?)")
	}
	extSubnet := st.Get("BNK_EXT_SUBNET")
	if extSubnet == "" {
		return fmt.Errorf("phase 17 secondary enis: BNK_EXT_SUBNET not found in state")
	}

	if err := recordPrimaryENIMAC(ctx, c, st, instanceID); err != nil {
		return fmt.Errorf("phase 17 secondary enis: %w", err)
	}

	sgID, err := ensureDataplaneSG(ctx, c, st, cl, vpcID)
	if err != nil {
		return fmt.Errorf("phase 17 secondary enis: %w", err)
	}
	if err := authorizeEKSNodeSGDataPathIngress(ctx, c, cl, vpcID); err != nil {
		return fmt.Errorf("phase 17 secondary enis: %w", err)
	}

	var extSelfIP string
	if cl.Network.DataPath != nil && cl.Network.DataPath.SelfIPs != nil {
		extSelfIP = strings.Split(cl.Network.DataPath.SelfIPs.External, "/")[0]
	}

	var instanceType string
	if cl.ClusterSpec != nil && len(cl.ClusterSpec.NodeGroups) > 0 {
		instanceType = cl.ClusterSpec.NodeGroups[0].InstanceType
	}
	extDevIdx, intDevIdx := TMMDeviceIndices(instanceType)

	extExtraTags := map[string]string{
		"bnk.f5.com/datapath":              "external",
		hostDeviceTagKey:                   "external",
		"node.k8s.amazonaws.com/no_manage": "true",
	}

	extENIID, err := ensureSecondaryENI(
		ctx, c, st, cl,
		instanceID,
		extSubnet,
		sgID,
		extDevIdx,
		extSelfIP,
		cl.Metadata.Name+"-tmm-ext-eni",
		"EXT_ENI_ID",
		"EXT_ENI_ATTACHMENT_ID",
		"EXT_ENI_MAC",
		"EXT_ENI_IP",
		extExtraTags,
	)
	if err != nil {
		return fmt.Errorf("phase 17 secondary enis: %w", err)
	}

	if cl.HasInternalInterface() {
		intSubnet := st.Get("BNK_INT_SUBNET")
		if intSubnet == "" {
			return fmt.Errorf("phase 17 secondary enis: BNK_INT_SUBNET not found in state")
		}
		var intSelfIP string
		if cl.Network.DataPath != nil && cl.Network.DataPath.SelfIPs != nil {
			intSelfIP = strings.Split(cl.Network.DataPath.SelfIPs.Internal, "/")[0]
		}
		intExtraTags := map[string]string{
			"bnk.f5.com/datapath":              "internal",
			hostDeviceTagKey:                   "internal",
			"node.k8s.amazonaws.com/no_manage": "true",
		}
		intENIID, err := ensureSecondaryENI(
			ctx, c, st, cl,
			instanceID,
			intSubnet,
			sgID,
			intDevIdx,
			intSelfIP,
			cl.Metadata.Name+"-tmm-int-eni",
			"INT_ENI_ID",
			"INT_ENI_ATTACHMENT_ID",
			"INT_ENI_MAC",
			"INT_ENI_IP",
			intExtraTags,
		)
		if err != nil {
			return fmt.Errorf("phase 17 secondary enis: %w", err)
		}

		if extRT := st.Get("BNK_EXT_RT_ID"); extRT != "" && intENIID != "" {
			if err := replaceOrCreateENIRoute(ctx, c, extRT, "192.168.0.0/16", intENIID); err != nil {
				return fmt.Errorf("phase 17 secondary enis: route 192.168.0.0/16 -> %s on %s: %w", intENIID, extRT, err)
			}
		}
	} else {
		if pubRT := st.Get("PUBLIC_RT_ID"); pubRT != "" && extENIID != "" {
			if err := replaceOrCreateENIRoute(ctx, c, pubRT, "192.168.0.0/16", extENIID); err != nil {
				return fmt.Errorf("phase 17 secondary enis: route 192.168.0.0/16 -> %s on %s: %w", extENIID, pubRT, err)
			}
		}
	}

	if primaryENIID := st.Get("PRIMARY_ENI_ID"); primaryENIID != "" {
		svcCIDR := discoverClusterServiceCIDR(ctx, c, cl, st)
		for _, rtKey := range []string{"BNK_EXT_RT_ID", "BNK_INT_RT_ID"} {
			if rtID := st.Get(rtKey); rtID != "" {
				if err := replaceOrCreateENIRoute(ctx, c, rtID, svcCIDR, primaryENIID); err != nil {
					return fmt.Errorf("phase 17 secondary enis: route %s -> %s on %s: %w", svcCIDR, primaryENIID, rtID, err)
				}
			}
		}
	}

	return nil
}

func discoverClusterServiceCIDR(ctx context.Context, c *Clients, cl *intent.Cluster, st *state.State) string {
	if svcCIDR := st.Get("SERVICE_CIDR"); svcCIDR != "" {
		return svcCIDR
	}
	if svcCIDR := st.Get("EKS_SERVICE_CIDR"); svcCIDR != "" {
		return svcCIDR
	}
	if c != nil && c.PhaseEKS != nil {
		clusterName := st.Get("EKS_CLUSTER_NAME")
		if clusterName == "" && cl != nil {
			clusterName = cl.Metadata.Name
		}
		if clusterName != "" {
			if out, err := c.PhaseEKS.DescribeCluster(ctx, &eks.DescribeClusterInput{
				Name: aws.String(clusterName),
			}); err == nil && out != nil && out.Cluster != nil &&
				out.Cluster.KubernetesNetworkConfig != nil &&
				out.Cluster.KubernetesNetworkConfig.ServiceIpv4Cidr != nil &&
				*out.Cluster.KubernetesNetworkConfig.ServiceIpv4Cidr != "" {
				cidr := *out.Cluster.KubernetesNetworkConfig.ServiceIpv4Cidr
				st.Set("EKS_SERVICE_CIDR", cidr)
				st.Set("SERVICE_CIDR", cidr)
				_ = st.Save()
				return cidr
			}
		}
	}
	return "172.20.0.0/16"
}

func replaceOrCreateENIRoute(ctx context.Context, c *Clients, rtID, destCIDR, eniID string) error {
	_, err := c.PhaseEC2.CreateRoute(ctx, &ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtID),
		DestinationCidrBlock: aws.String(destCIDR),
		NetworkInterfaceId:   aws.String(eniID),
	})
	if err == nil {
		return nil
	}
	if isRouteAlreadyExists(err) {
		_, rerr := c.PhaseEC2.ReplaceRoute(ctx, &ec2.ReplaceRouteInput{
			RouteTableId:         aws.String(rtID),
			DestinationCidrBlock: aws.String(destCIDR),
			NetworkInterfaceId:   aws.String(eniID),
		})
		return rerr
	}
	return err
}

// ENIDetachPollInterval is the polling interval when waiting for an ENI to reach status "available" after DetachNetworkInterface.
var ENIDetachPollInterval = 2 * time.Second

// ENIDetachPollTimeout is the maximum time to wait for an ENI to detach.
var ENIDetachPollTimeout = 2 * time.Minute

func waitForENIAvailable(ctx context.Context, c *Clients, eniID string) error {
	deadline := time.Now().Add(ENIDetachPollTimeout)
	for {
		out, err := c.PhaseEC2.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
			NetworkInterfaceIds: []string{eniID},
		})
		if err != nil {
			if awsmw.IsNotFound(err) {
				return nil
			}
			return err
		}
		if len(out.NetworkInterfaces) == 0 || out.NetworkInterfaces[0].Status == ec2types.NetworkInterfaceStatusAvailable {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for ENI %s to detach", eniID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(ENIDetachPollInterval):
		}
	}
}

func detachAndDeleteENIByID(ctx context.Context, c *Clients, eniID string) error {
	if eniID == "" {
		return nil
	}
	out, err := c.PhaseEC2.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []string{eniID},
	})
	if err != nil {
		if awsmw.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("DescribeNetworkInterfaces %s: %w", eniID, err)
	}
	if len(out.NetworkInterfaces) == 0 {
		return nil
	}
	eni := out.NetworkInterfaces[0]
	if eni.Attachment != nil && eni.Attachment.AttachmentId != nil && *eni.Attachment.AttachmentId != "" {
		attachID := *eni.Attachment.AttachmentId
		_, derr := c.PhaseEC2.DetachNetworkInterface(ctx, &ec2.DetachNetworkInterfaceInput{
			AttachmentId: aws.String(attachID),
			Force:        aws.Bool(true),
		})
		if derr != nil && !awsmw.IsNotFound(derr) {
			return fmt.Errorf("DetachNetworkInterface %s: %w", attachID, derr)
		}
	}
	if err := waitForENIAvailable(ctx, c, eniID); err != nil {
		return err
	}
	_, err = c.PhaseEC2.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{
		NetworkInterfaceId: aws.String(eniID),
	})
	if err != nil && !awsmw.IsNotFound(err) {
		return fmt.Errorf("DeleteNetworkInterface %s: %w", eniID, err)
	}
	return nil
}

func detachAndDeleteENI(ctx context.Context, c *Clients, st *state.State, eniKey, attachKey, macKey, ipKey string) error {
	eniID := st.Get(eniKey)
	attachID := st.Get(attachKey)
	if attachID != "" {
		_, err := c.PhaseEC2.DetachNetworkInterface(ctx, &ec2.DetachNetworkInterfaceInput{
			AttachmentId: aws.String(attachID),
			Force:        aws.Bool(true),
		})
		if err != nil && !awsmw.IsNotFound(err) {
			return fmt.Errorf("DetachNetworkInterface %s: %w", attachID, err)
		}
		st.Delete(attachKey)
	}
	if eniID != "" {
		if attachID == "" {
			if err := detachAndDeleteENIByID(ctx, c, eniID); err != nil {
				return err
			}
		} else {
			if err := waitForENIAvailable(ctx, c, eniID); err != nil {
				return err
			}
			_, err := c.PhaseEC2.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{
				NetworkInterfaceId: aws.String(eniID),
			})
			if err != nil && !awsmw.IsNotFound(err) {
				return fmt.Errorf("DeleteNetworkInterface %s: %w", eniID, err)
			}
		}
		st.Delete(eniKey)
	}
	st.Delete(macKey)
	st.Delete(ipKey)
	return nil
}

// Phase17SecondaryENIsDown detaches and deletes the secondary ENIs and deletes the dataplane security group.
func Phase17SecondaryENIsDown(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if st == nil || dryRun {
		return nil
	}
	if c == nil || c.PhaseEC2 == nil {
		return fmt.Errorf("phase 17 secondary enis down: EC2 client is nil")
	}

	if err := detachAndDeleteENI(ctx, c, st, "INT_ENI_ID", "INT_ENI_ATTACHMENT_ID", "INT_ENI_MAC", "INT_ENI_IP"); err != nil {
		return fmt.Errorf("phase 17 secondary enis down: %w", err)
	}
	if err := detachAndDeleteENI(ctx, c, st, "INTERNAL_ENI", "INT_ENI_ATTACHMENT_ID", "INTERNAL_ENI_MAC", "INT_ENI_PRIVATE_IP"); err != nil {
		return fmt.Errorf("phase 17 secondary enis down: %w", err)
	}
	if err := detachAndDeleteENI(ctx, c, st, "EXT_ENI_ID", "EXT_ENI_ATTACHMENT_ID", "EXT_ENI_MAC", "EXT_ENI_IP"); err != nil {
		return fmt.Errorf("phase 17 secondary enis down: %w", err)
	}
	if err := detachAndDeleteENI(ctx, c, st, "EXTERNAL_ENI", "EXT_ENI_ATTACHMENT_ID", "EXTERNAL_ENI_MAC", "EXT_ENI_PRIVATE_IP"); err != nil {
		return fmt.Errorf("phase 17 secondary enis down: %w", err)
	}

	sgID := st.Get("BNK_DATAPATH_SG_ID")
	if sgID == "" {
		sgID = st.Get("SG_BNK_DATA")
	}
	if sgID != "" {
		eksSG := st.Get("EKS_SECURITY_GROUP")
		if eksSG == "" {
			eksSG = st.Get("EKS_CLUSTER_SG_ID")
		}
		if eksSG != "" {
			revokeAllSecurityGroupRules(ctx, c, eksSG)
		}
		revokeAllSecurityGroupRules(ctx, c, sgID)
		_, err := c.PhaseEC2.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{
			GroupId: aws.String(sgID),
		})
		if err != nil && !awsmw.IsNotFound(err) {
			// Best-effort here; Phase07IAMDown retries after EKS cluster deletion if nodes still hold the SG
			_ = err
		} else {
			st.Delete("BNK_DATAPATH_SG_ID")
			st.Delete("SG_BNK_DATA")
		}
	}

	st.Delete("PRIMARY_ENI_MAC")
	st.Delete("PRIMARY_ENI_ID")
	return st.Save()
}
