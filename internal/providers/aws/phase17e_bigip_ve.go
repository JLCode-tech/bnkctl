package aws

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/JLCode-tech/bnkctl/internal/providers/aws/awsmw"
	"github.com/JLCode-tech/bnkctl/internal/providers/aws/tags"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

const (
	bigipPaygAllTwoBootLocationsNamePattern = "F5 BIGIP-21.0.0*PAYG-Good 25Mbps*260113*"
	bigipHostFourthOctet                    = 200
)

func resolveBIGIPAMI(ctx context.Context, c *Clients, explicitAMI string) (string, error) {
	if explicitAMI != "" {
		return explicitAMI, nil
	}
	out, err := c.PhaseEC2.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("name"), Values: []string{bigipPaygAllTwoBootLocationsNamePattern}},
			{Name: aws.String("state"), Values: []string{"available"}},
			{Name: aws.String("architecture"), Values: []string{"x86_64"}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("DescribeImages (%s): %w", bigipPaygAllTwoBootLocationsNamePattern, err)
	}
	if len(out.Images) == 0 {
		return "", fmt.Errorf("no BIG-IP VE PAYG AMI found matching %q in this region; set bigip.amiId explicitly or accept the F5 BIG-IP Marketplace subscription", bigipPaygAllTwoBootLocationsNamePattern)
	}
	images := out.Images
	sort.Slice(images, func(i, j int) bool {
		return aws.ToString(images[i].CreationDate) > aws.ToString(images[j].CreationDate)
	})
	if images[0].ImageId == nil {
		return "", fmt.Errorf("DescribeImages returned image with nil ImageId")
	}
	return *images[0].ImageId, nil
}

func ensureBIGIPSG(ctx context.Context, c *Clients, st *state.State, cl *intent.Cluster, vpcID string) (string, error) {
	if existing := st.Get("BIGIP_SG_ID"); existing != "" {
		out, err := c.PhaseEC2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
			GroupIds: []string{existing},
		})
		if err == nil && len(out.SecurityGroups) > 0 {
			return existing, nil
		}
		if err != nil && !awsmw.IsNotFound(err) {
			return "", fmt.Errorf("DescribeSecurityGroups %s: %w", existing, err)
		}
		st.Delete("BIGIP_SG_ID")
	}

	sgName := cl.Metadata.Name + "-bigip-ve-sg"
	t := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, map[string]string{
		"Name": sgName,
	})
	createOut, err := c.PhaseEC2.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:         aws.String(sgName),
		Description:       aws.String("BIG-IP VE Tier-1 ADC security group (mgmt + BNK_EXT data plane + BGP)"),
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
						Description: aws.String("Allow all intra-VPC traffic (SSH/TMUI from jumphost, BGP TCP 179 + BFD + HTTP from/to TMM)"),
					},
				},
			},
		},
	})
	if ierr != nil && !strings.Contains(ierr.Error(), "InvalidPermission.Duplicate") {
		return "", fmt.Errorf("AuthorizeSecurityGroupIngress on %s: %w", sgID, ierr)
	}

	st.Set("BIGIP_SG_ID", sgID)
	if err := st.Save(); err != nil {
		return "", err
	}
	return sgID, nil
}

func ensureBIGIPExtENI(ctx context.Context, c *Clients, st *state.State, cl *intent.Cluster, extSubnet, sgID, extIP, vipIP string) (string, error) {
	if existing := st.Get("BIGIP_EXT_ENI_ID"); existing != "" {
		out, err := c.PhaseEC2.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
			NetworkInterfaceIds: []string{existing},
		})
		if err == nil && len(out.NetworkInterfaces) > 0 {
			return existing, nil
		}
		if err != nil && !awsmw.IsNotFound(err) {
			return "", fmt.Errorf("DescribeNetworkInterfaces %s: %w", existing, err)
		}
		st.Delete("BIGIP_EXT_ENI_ID")
		st.Delete("BIGIP_EXT_ENI_ATTACHMENT_ID")
	}

	name := cl.Metadata.Name + "-bigip-ext-eni"
	t := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, map[string]string{
		"Name": name,
	})
	ipSpecs := []ec2types.PrivateIpAddressSpecification{
		{
			PrivateIpAddress: aws.String(extIP),
			Primary:          aws.Bool(true),
		},
	}
	if vipIP != "" && vipIP != extIP {
		ipSpecs = append(ipSpecs, ec2types.PrivateIpAddressSpecification{
			PrivateIpAddress: aws.String(vipIP),
			Primary:          aws.Bool(false),
		})
	}

	out, err := c.PhaseEC2.CreateNetworkInterface(ctx, &ec2.CreateNetworkInterfaceInput{
		SubnetId:           aws.String(extSubnet),
		Groups:             []string{sgID},
		Description:        aws.String("BIG-IP VE 1.1 external data-plane ENI"),
		PrivateIpAddresses: ipSpecs,
		TagSpecifications:  tags.ToTagSpec(ec2types.ResourceTypeNetworkInterface, t),
	})
	if err != nil {
		return "", fmt.Errorf("CreateNetworkInterface %s: %w", name, err)
	}
	if out.NetworkInterface == nil || out.NetworkInterface.NetworkInterfaceId == nil {
		return "", fmt.Errorf("CreateNetworkInterface %s returned nil NetworkInterfaceId", name)
	}
	eniID := *out.NetworkInterface.NetworkInterfaceId

	if _, err := c.PhaseEC2.ModifyNetworkInterfaceAttribute(ctx, &ec2.ModifyNetworkInterfaceAttributeInput{
		NetworkInterfaceId: aws.String(eniID),
		SourceDestCheck:    &ec2types.AttributeBooleanValue{Value: aws.Bool(false)},
	}); err != nil {
		return "", fmt.Errorf("disabling SourceDestCheck on %s: %w", eniID, err)
	}

	st.Set("BIGIP_EXT_ENI_ID", eniID)
	st.Set("BIGIP_EXT_IP", extIP)
	if vipIP != "" {
		st.Set("BIGIP_VIP", vipIP)
	}
	if err := st.Save(); err != nil {
		return "", err
	}
	return eniID, nil
}

// Phase17eBIGIPVE provisions the 2-NIC BIG-IP VE EC2 instance when bigip.enabled == true.
func Phase17eBIGIPVE(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 17e bigip-ve: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 17e bigip-ve: state is nil")
	}
	if !cl.HasBIGIP() || dryRun {
		return nil
	}
	if c == nil || c.PhaseEC2 == nil {
		return fmt.Errorf("phase 17e bigip-ve: EC2 client is nil")
	}

	vpcID := st.Get("VPC_ID")
	if vpcID == "" {
		return fmt.Errorf("phase 17e bigip-ve: VPC_ID not found in state")
	}
	extSubnet := st.Get("BNK_EXT_SUBNET")
	if extSubnet == "" {
		return fmt.Errorf("phase 17e bigip-ve: BNK_EXT_SUBNET not found in state")
	}

	sgID, err := ensureBIGIPSG(ctx, c, st, cl, vpcID)
	if err != nil {
		return fmt.Errorf("phase 17e bigip-ve: %w", err)
	}

	keyName := st.Get("JUMPHOST_KEY_NAME")
	if keyName == "" {
		keyName, err = ensureJumphostKeyPair(ctx, c, st, cl)
		if err != nil {
			return fmt.Errorf("phase 17e bigip-ve: %w", err)
		}
	}

	var extIP, vipIP string
	if cl.Network.DataPath != nil {
		extIP, err = deriveFourthOctetIP(cl.Network.DataPath.External.CIDR, bigipHostFourthOctet)
		if err != nil {
			return fmt.Errorf("phase 17e bigip-ve: deriving BIG-IP external IP: %w", err)
		}
	}
	if cl.BigIPVE != nil {
		if cl.BigIPVE.Tier1VIP != "" {
			vipIP = cl.BigIPVE.Tier1VIP
		} else if cl.BigIPVE.VIP != "" {
			vipIP = cl.BigIPVE.VIP
		}
	}
	if vipIP == "" {
		vipIP = extIP
	}

	extENIID, err := ensureBIGIPExtENI(ctx, c, st, cl, extSubnet, sgID, extIP, vipIP)
	if err != nil {
		return fmt.Errorf("phase 17e bigip-ve: %w", err)
	}

	if existing := st.Get("BIGIP_INSTANCE_ID"); existing != "" {
		out, derr := c.PhaseEC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []string{existing},
		})
		if derr == nil {
			for _, r := range out.Reservations {
				for _, inst := range r.Instances {
					if inst.State != nil && inst.State.Name != ec2types.InstanceStateNameTerminated && inst.State.Name != ec2types.InstanceStateNameShuttingDown {
						if err := waitForInstanceState(ctx, c, existing, ec2types.InstanceStateNameRunning); err != nil {
							return fmt.Errorf("phase 17e bigip-ve: %w", err)
						}
						return nil
					}
				}
			}
		}
		st.Delete("BIGIP_INSTANCE_ID")
	}

	var explicitAMI string
	instanceType := "m5.xlarge"
	if cl.BigIPVE != nil {
		explicitAMI = cl.BigIPVE.AMIID
		if cl.BigIPVE.InstanceType != "" {
			instanceType = cl.BigIPVE.InstanceType
		}
	}
	amiID, err := resolveBIGIPAMI(ctx, c, explicitAMI)
	if err != nil {
		return fmt.Errorf("phase 17e bigip-ve: %w", err)
	}
	st.Set("BIGIP_AMI_ID", amiID)

	var mgmtSubnet string
	if cl.Network.DataPath != nil {
		targetAZ := cl.Network.DataPath.External.AZ
		for i, s := range cl.Network.Subnets.PublicList {
			if s.AZ == targetAZ {
				mgmtSubnet = st.Get(subnetStateKey("PUBLIC_SUBNET", i))
				break
			}
		}
	}
	if mgmtSubnet == "" {
		mgmtSubnet = st.Get("PUBLIC_SUBNET_0")
	}
	if mgmtSubnet == "" {
		return fmt.Errorf("phase 17e bigip-ve: no public management subnet found in state")
	}

	var mgmtIP string
	for _, s := range cl.Network.Subnets.PublicList {
		if cl.Network.DataPath != nil && s.AZ == cl.Network.DataPath.External.AZ {
			mgmtIP, _ = deriveFourthOctetIP(s.CIDR, bigipHostFourthOctet)
			break
		}
	}
	if mgmtIP == "" && len(cl.Network.Subnets.PublicList) > 0 {
		mgmtIP, _ = deriveFourthOctetIP(cl.Network.Subnets.PublicList[0].CIDR, bigipHostFourthOctet)
	}

	mgmtNIC := ec2types.InstanceNetworkInterfaceSpecification{
		DeviceIndex:              aws.Int32(0),
		SubnetId:                 aws.String(mgmtSubnet),
		Groups:                   []string{sgID},
		AssociatePublicIpAddress: aws.Bool(false),
		DeleteOnTermination:      aws.Bool(true),
	}
	if mgmtIP != "" {
		mgmtNIC.PrivateIpAddress = aws.String(mgmtIP)
	}

	t := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, map[string]string{
		"Name": cl.Metadata.Name + "-bigip-ve",
	})

	runOut, err := c.PhaseEC2.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:           aws.String(amiID),
		InstanceType:      ec2types.InstanceType(instanceType),
		MinCount:          aws.Int32(1),
		MaxCount:          aws.Int32(1),
		KeyName:           aws.String(keyName),
		NetworkInterfaces: []ec2types.InstanceNetworkInterfaceSpecification{mgmtNIC},
		TagSpecifications: tags.ToTagSpec(ec2types.ResourceTypeInstance, t),
	})
	if err != nil {
		return fmt.Errorf("phase 17e bigip-ve: RunInstances: %w", err)
	}
	if len(runOut.Instances) == 0 || runOut.Instances[0].InstanceId == nil {
		return fmt.Errorf("phase 17e bigip-ve: RunInstances returned no instance ID")
	}
	inst := runOut.Instances[0]
	instanceID := *inst.InstanceId
	if inst.PrivateIpAddress != nil && *inst.PrivateIpAddress != "" {
		mgmtIP = *inst.PrivateIpAddress
	}

	st.Set("BIGIP_INSTANCE_ID", instanceID)
	st.Set("BIGIP_MGMT_IP", mgmtIP)
	if err := st.Save(); err != nil {
		return err
	}

	if err := waitForInstanceState(ctx, c, instanceID, ec2types.InstanceStateNameRunning); err != nil {
		return fmt.Errorf("phase 17e bigip-ve: %w", err)
	}

	attachOut, err := c.PhaseEC2.AttachNetworkInterface(ctx, &ec2.AttachNetworkInterfaceInput{
		NetworkInterfaceId: aws.String(extENIID),
		InstanceId:         aws.String(instanceID),
		DeviceIndex:        aws.Int32(1),
	})
	if err != nil {
		return fmt.Errorf("phase 17e bigip-ve: AttachNetworkInterface %s -> %s: %w", extENIID, instanceID, err)
	}
	if attachOut.AttachmentId != nil {
		st.Set("BIGIP_EXT_ENI_ATTACHMENT_ID", *attachOut.AttachmentId)
	}

	return st.Save()
}

// Phase17eBIGIPVEDown terminates the BIG-IP VE instance, detaches and deletes its external ENI, and deletes its security group.
func Phase17eBIGIPVEDown(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if st == nil || dryRun {
		return nil
	}
	if c == nil || c.PhaseEC2 == nil {
		return fmt.Errorf("phase 17e bigip-ve down: EC2 client is nil")
	}

	if attachID := st.Get("BIGIP_EXT_ENI_ATTACHMENT_ID"); attachID != "" {
		_, err := c.PhaseEC2.DetachNetworkInterface(ctx, &ec2.DetachNetworkInterfaceInput{
			AttachmentId: aws.String(attachID),
			Force:        aws.Bool(true),
		})
		if err != nil && !awsmw.IsNotFound(err) {
			return fmt.Errorf("phase 17e bigip-ve down: DetachNetworkInterface %s: %w", attachID, err)
		}
		st.Delete("BIGIP_EXT_ENI_ATTACHMENT_ID")
	}

	if instID := st.Get("BIGIP_INSTANCE_ID"); instID != "" {
		_, err := c.PhaseEC2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
			InstanceIds: []string{instID},
		})
		if err != nil && !awsmw.IsNotFound(err) {
			return fmt.Errorf("phase 17e bigip-ve down: TerminateInstances %s: %w", instID, err)
		}
		if err := waitForInstanceState(ctx, c, instID, ec2types.InstanceStateNameTerminated); err != nil {
			return fmt.Errorf("phase 17e bigip-ve down: %w", err)
		}
		st.Delete("BIGIP_INSTANCE_ID")
	}

	if eniID := st.Get("BIGIP_EXT_ENI_ID"); eniID != "" {
		deadline := time.Now().Add(2 * time.Minute)
		for {
			_, derr := c.PhaseEC2.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{
				NetworkInterfaceId: aws.String(eniID),
			})
			if derr == nil || awsmw.IsNotFound(derr) {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("phase 17e bigip-ve down: DeleteNetworkInterface %s: %w", eniID, derr)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}
		st.Delete("BIGIP_EXT_ENI_ID")
	}

	if sgID := st.Get("BIGIP_SG_ID"); sgID != "" {
		_, err := c.PhaseEC2.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{
			GroupId: aws.String(sgID),
		})
		if err != nil && !awsmw.IsNotFound(err) {
			return fmt.Errorf("phase 17e bigip-ve down: DeleteSecurityGroup %s: %w", sgID, err)
		}
		st.Delete("BIGIP_SG_ID")
	}

	st.Delete("BIGIP_AMI_ID")
	st.Delete("BIGIP_MGMT_IP")
	st.Delete("BIGIP_EXT_IP")
	st.Delete("BIGIP_VIP")
	st.Delete("BIGIP_ADMIN_PASSWORD")
	st.Delete("BIGIP_ONBOARDED")
	return st.Save()
}
