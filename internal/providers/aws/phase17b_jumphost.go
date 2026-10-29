package aws

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JLCode-tech/bnkctl/internal/providers/aws/awsmw"
	"github.com/JLCode-tech/bnkctl/internal/providers/aws/tags"
	"github.com/JLCode-tech/bnkctl/pkg/forge"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

const al2023AMIParam = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64"

// EICEPollInterval is the polling interval when waiting for an EC2 Instance Connect Endpoint state transition.
var EICEPollInterval = 10 * time.Second

// EICEPollTimeout is the maximum time to wait for an EC2 Instance Connect Endpoint state transition.
var EICEPollTimeout = 10 * time.Minute

// JumphostPollInterval is the polling interval when waiting for the jumphost instance state transition.
var JumphostPollInterval = 5 * time.Second

// JumphostPollTimeout is the maximum time to wait for the jumphost instance state transition.
var JumphostPollTimeout = 5 * time.Minute

func deriveFourthOctetIP(cidr string, host int) (string, error) {
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", err
	}
	v4 := ip.To4()
	if v4 == nil {
		return "", fmt.Errorf("non-IPv4 CIDR %s", cidr)
	}
	return fmt.Sprintf("%d.%d.%d.%d", v4[0], v4[1], v4[2], host), nil
}

func resolveAL2023AMI(ctx context.Context, c *Clients) (string, error) {
	if c.SSM == nil {
		return "", fmt.Errorf("SSM client is nil")
	}
	out, err := c.SSM.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(al2023AMIParam),
	})
	if err != nil {
		return "", fmt.Errorf("SSM GetParameter %s: %w", al2023AMIParam, err)
	}
	if out.Parameter == nil || out.Parameter.Value == nil || *out.Parameter.Value == "" {
		return "", fmt.Errorf("SSM parameter %s returned empty value", al2023AMIParam)
	}
	return *out.Parameter.Value, nil
}

func ensureJumphostSG(ctx context.Context, c *Clients, st *state.State, cl *intent.Cluster, vpcID string) (string, error) {
	if existing := st.Get("JUMPHOST_SG_ID"); existing != "" {
		out, err := c.PhaseEC2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
			GroupIds: []string{existing},
		})
		if err == nil && len(out.SecurityGroups) > 0 {
			return existing, nil
		}
		if err != nil && !awsmw.IsNotFound(err) {
			return "", fmt.Errorf("DescribeSecurityGroups %s: %w", existing, err)
		}
		st.Delete("JUMPHOST_SG_ID")
	}

	sgName := cl.Metadata.Name + "-jumphost-sg"
	t := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, map[string]string{
		"Name": sgName,
	})
	createOut, err := c.PhaseEC2.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:         aws.String(sgName),
		Description:       aws.String("BNK test jumphost security group"),
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
						Description: aws.String("Allow intra-VPC traffic (EICE SSH + BNK return traffic)"),
					},
				},
			},
		},
	})
	if ierr != nil && !strings.Contains(ierr.Error(), "InvalidPermission.Duplicate") {
		return "", fmt.Errorf("AuthorizeSecurityGroupIngress on %s: %w", sgID, ierr)
	}

	st.Set("JUMPHOST_SG_ID", sgID)
	if err := st.Save(); err != nil {
		return "", err
	}
	return sgID, nil
}

func ensureJumphostKeyPair(ctx context.Context, c *Clients, st *state.State, cl *intent.Cluster) (string, error) {
	keyName := cl.Metadata.Name + "-jumphost-key"
	keyPath := filepath.Join(filepath.Dir(st.Path()), "jumphost.pem")

	if existing := st.Get("JUMPHOST_KEY_NAME"); existing != "" {
		out, err := c.PhaseEC2.DescribeKeyPairs(ctx, &ec2.DescribeKeyPairsInput{
			KeyNames: []string{existing},
		})
		if err == nil && len(out.KeyPairs) > 0 {
			return existing, nil
		}
		if err != nil && !awsmw.IsNotFound(err) {
			return "", fmt.Errorf("DescribeKeyPairs %s: %w", existing, err)
		}
		st.Delete("JUMPHOST_KEY_NAME")
	}

	t := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, map[string]string{
		"Name": keyName,
	})
	out, err := c.PhaseEC2.CreateKeyPair(ctx, &ec2.CreateKeyPairInput{
		KeyName:           aws.String(keyName),
		KeyType:           ec2types.KeyTypeEd25519,
		TagSpecifications: tags.ToTagSpec(ec2types.ResourceTypeKeyPair, t),
	})
	if err != nil {
		return "", fmt.Errorf("CreateKeyPair %s: %w", keyName, err)
	}
	if out.KeyMaterial != nil {
		if err := os.WriteFile(keyPath, []byte(*out.KeyMaterial), 0o600); err != nil {
			return "", fmt.Errorf("writing jumphost key %s: %w", keyPath, err)
		}
		st.Set("JUMPHOST_KEY_PATH", keyPath)
	}
	st.Set("JUMPHOST_KEY_NAME", keyName)
	if err := st.Save(); err != nil {
		return "", err
	}
	return keyName, nil
}

func waitForEICEState(ctx context.Context, c *Clients, eiceID string, target ec2types.Ec2InstanceConnectEndpointState) error {
	deadline := time.Now().Add(EICEPollTimeout)
	for {
		out, err := c.PhaseEC2.DescribeInstanceConnectEndpoints(ctx, &ec2.DescribeInstanceConnectEndpointsInput{
			InstanceConnectEndpointIds: []string{eiceID},
		})
		if err != nil {
			if target == ec2types.Ec2InstanceConnectEndpointStateDeleteComplete && awsmw.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("DescribeInstanceConnectEndpoints %s: %w", eiceID, err)
		}
		if len(out.InstanceConnectEndpoints) == 0 {
			if target == ec2types.Ec2InstanceConnectEndpointStateDeleteComplete {
				return nil
			}
			return fmt.Errorf("EICE %s disappeared while waiting for %s", eiceID, target)
		}
		ep := out.InstanceConnectEndpoints[0]
		if ep.State == target {
			return nil
		}
		if ep.State == ec2types.Ec2InstanceConnectEndpointStateCreateFailed || ep.State == ec2types.Ec2InstanceConnectEndpointStateDeleteFailed {
			msg := ""
			if ep.StateMessage != nil {
				msg = *ep.StateMessage
			}
			return fmt.Errorf("EICE %s entered %s: %s", eiceID, ep.State, msg)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for EICE %s to reach %s (current: %s)", eiceID, target, ep.State)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(EICEPollInterval):
		}
	}
}

func ensureEICE(ctx context.Context, c *Clients, st *state.State, cl *intent.Cluster, subnetID, sgID string) error {
	if existing := st.Get("EICE_ID"); existing != "" {
		out, err := c.PhaseEC2.DescribeInstanceConnectEndpoints(ctx, &ec2.DescribeInstanceConnectEndpointsInput{
			InstanceConnectEndpointIds: []string{existing},
		})
		if err == nil && len(out.InstanceConnectEndpoints) > 0 {
			return waitForEICEState(ctx, c, existing, ec2types.Ec2InstanceConnectEndpointStateCreateComplete)
		}
		if err != nil && !awsmw.IsNotFound(err) {
			return fmt.Errorf("DescribeInstanceConnectEndpoints %s: %w", existing, err)
		}
		st.Delete("EICE_ID")
	}

	if vpcID := st.Get("VPC_ID"); vpcID != "" {
		listOut, lerr := c.PhaseEC2.DescribeInstanceConnectEndpoints(ctx, &ec2.DescribeInstanceConnectEndpointsInput{
			Filters: []ec2types.Filter{
				{Name: aws.String("vpc-id"), Values: []string{vpcID}},
			},
		})
		if lerr == nil {
			for _, ep := range listOut.InstanceConnectEndpoints {
				if ep.InstanceConnectEndpointId == nil {
					continue
				}
				if ep.State == ec2types.Ec2InstanceConnectEndpointStateCreateComplete ||
					ep.State == ec2types.Ec2InstanceConnectEndpointStateCreateInProgress {
					id := *ep.InstanceConnectEndpointId
					st.Set("EICE_ID", id)
					if err := st.Save(); err != nil {
						return err
					}
					return waitForEICEState(ctx, c, id, ec2types.Ec2InstanceConnectEndpointStateCreateComplete)
				}
			}
		}
	}

	t := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, map[string]string{
		"Name": cl.Metadata.Name + "-eice",
	})
	out, err := c.PhaseEC2.CreateInstanceConnectEndpoint(ctx, &ec2.CreateInstanceConnectEndpointInput{
		SubnetId:          aws.String(subnetID),
		SecurityGroupIds:  []string{sgID},
		PreserveClientIp:  aws.Bool(false),
		TagSpecifications: tags.ToTagSpec(ec2types.ResourceTypeInstanceConnectEndpoint, t),
	})
	if err != nil {
		if strings.Contains(err.Error(), "ResourceCountExceeded") || strings.Contains(err.Error(), "QuotaExceeded") {
			fmt.Printf("warning: skipping EICE creation (%v); jumphost remains reachable in-VPC\n", err)
			return nil
		}
		return fmt.Errorf("CreateInstanceConnectEndpoint: %w", err)
	}
	if out.InstanceConnectEndpoint == nil || out.InstanceConnectEndpoint.InstanceConnectEndpointId == nil {
		return fmt.Errorf("CreateInstanceConnectEndpoint returned nil InstanceConnectEndpointId")
	}
	eiceID := *out.InstanceConnectEndpoint.InstanceConnectEndpointId
	st.Set("EICE_ID", eiceID)
	if err := st.Save(); err != nil {
		return err
	}
	return waitForEICEState(ctx, c, eiceID, ec2types.Ec2InstanceConnectEndpointStateCreateComplete)
}

func waitForInstanceState(ctx context.Context, c *Clients, instanceID string, target ec2types.InstanceStateName) error {
	deadline := time.Now().Add(JumphostPollTimeout)
	for {
		out, err := c.PhaseEC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []string{instanceID},
		})
		if err != nil {
			if target == ec2types.InstanceStateNameTerminated && awsmw.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("DescribeInstances %s: %w", instanceID, err)
		}
		var current ec2types.InstanceStateName
		found := false
		for _, r := range out.Reservations {
			for _, inst := range r.Instances {
				if inst.State != nil {
					current = inst.State.Name
					found = true
				}
			}
		}
		if !found && target == ec2types.InstanceStateNameTerminated {
			return nil
		}
		if current == target {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for instance %s to reach %s (current: %s)", instanceID, target, current)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(JumphostPollInterval):
		}
	}
}

// Phase17bJumphost provisions the multi-NIC test jumphost and EICE endpoint for external BNK data-path testing.
func Phase17bJumphost(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 17b jumphost: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 17b jumphost: state is nil")
	}
	if !cl.HasExternalDataPath() || dryRun {
		return nil
	}
	if c == nil || c.PhaseEC2 == nil {
		return fmt.Errorf("phase 17b jumphost: EC2 client is nil")
	}

	vpcID := st.Get("VPC_ID")
	if vpcID == "" {
		return fmt.Errorf("phase 17b jumphost: VPC_ID not found in state")
	}
	extSubnet := st.Get("BNK_EXT_SUBNET")
	if extSubnet == "" {
		return fmt.Errorf("phase 17b jumphost: BNK_EXT_SUBNET not found in state")
	}

	sgID, err := ensureJumphostSG(ctx, c, st, cl, vpcID)
	if err != nil {
		return fmt.Errorf("phase 17b jumphost: %w", err)
	}

	keyName, err := ensureJumphostKeyPair(ctx, c, st, cl)
	if err != nil {
		return fmt.Errorf("phase 17b jumphost: %w", err)
	}

	if existing := st.Get("JUMPHOST_ID"); existing != "" {
		out, derr := c.PhaseEC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []string{existing},
		})
		if derr == nil {
			for _, r := range out.Reservations {
				for _, inst := range r.Instances {
					if inst.State != nil && inst.State.Name != ec2types.InstanceStateNameTerminated && inst.State.Name != ec2types.InstanceStateNameShuttingDown {
						if err := waitForInstanceState(ctx, c, existing, ec2types.InstanceStateNameRunning); err != nil {
							return fmt.Errorf("phase 17b jumphost: %w", err)
						}
						st.Set("JUMPHOST_INSTANCE_ID", existing)
						_ = st.Save()
						return ensureEICE(ctx, c, st, cl, extSubnet, sgID)
					}
				}
			}
		}
		st.Delete("JUMPHOST_ID")
		st.Delete("JUMPHOST_INSTANCE_ID")
	}

	amiID, err := resolveAL2023AMI(ctx, c)
	if err != nil {
		return fmt.Errorf("phase 17b jumphost: %w", err)
	}

	var mgmtSubnet string
	if cl.Network.DataPath != nil {
		targetAZ := cl.Network.DataPath.External.AZ
		for i, s := range cl.Network.Subnets.PrivateList {
			if s.AZ == targetAZ {
				mgmtSubnet = st.Get(subnetStateKey("PRIVATE_SUBNET", i))
				break
			}
		}
		if mgmtSubnet == "" {
			for i, s := range cl.Network.Subnets.PublicList {
				if s.AZ == targetAZ {
					mgmtSubnet = st.Get(subnetStateKey("PUBLIC_SUBNET", i))
					break
				}
			}
		}
	}
	if mgmtSubnet == "" {
		mgmtSubnet = st.Get("PRIVATE_SUBNET_0")
	}
	if mgmtSubnet == "" {
		mgmtSubnet = st.Get("PUBLIC_SUBNET_0")
	}
	if mgmtSubnet == "" {
		return fmt.Errorf("phase 17b jumphost: no management subnet found in state")
	}

	var extIP string
	if cl.Network.DataPath != nil {
		extIP, err = deriveFourthOctetIP(cl.Network.DataPath.External.CIDR, 50)
		if err != nil {
			return fmt.Errorf("phase 17b jumphost: deriving external IP: %w", err)
		}
	}

	nics := []ec2types.InstanceNetworkInterfaceSpecification{
		{
			DeviceIndex:         aws.Int32(0),
			SubnetId:            aws.String(mgmtSubnet),
			Groups:              []string{sgID},
			DeleteOnTermination: aws.Bool(true),
		},
		{
			DeviceIndex:         aws.Int32(1),
			SubnetId:            aws.String(extSubnet),
			Groups:              []string{sgID},
			PrivateIpAddress:    aws.String(extIP),
			DeleteOnTermination: aws.Bool(true),
		},
	}

	var intIP string
	if cl.HasInternalInterface() && cl.Network.DataPath != nil {
		intSubnet := st.Get("BNK_INT_SUBNET")
		if intSubnet == "" {
			return fmt.Errorf("phase 17b jumphost: BNK_INT_SUBNET not found in state")
		}
		intIP, err = deriveFourthOctetIP(cl.Network.DataPath.Internal.CIDR, 50)
		if err != nil {
			return fmt.Errorf("phase 17b jumphost: deriving internal IP: %w", err)
		}
		nics = append(nics, ec2types.InstanceNetworkInterfaceSpecification{
			DeviceIndex:         aws.Int32(2),
			SubnetId:            aws.String(intSubnet),
			Groups:              []string{sgID},
			PrivateIpAddress:    aws.String(intIP),
			DeleteOnTermination: aws.Bool(true),
		})
	}

	t := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, map[string]string{
		"Name": cl.Metadata.Name + "-jumphost",
	})

	var iamProfile *ec2types.IamInstanceProfileSpecification
	if descOut, derr := c.PhaseEC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
			{Name: aws.String("instance-state-name"), Values: []string{"running"}},
		},
	}); derr == nil && descOut != nil {
		for _, r := range descOut.Reservations {
			for _, inst := range r.Instances {
				if inst.IamInstanceProfile != nil && inst.IamInstanceProfile.Arn != nil && *inst.IamInstanceProfile.Arn != "" {
					iamProfile = &ec2types.IamInstanceProfileSpecification{
						Arn: inst.IamInstanceProfile.Arn,
					}
					break
				}
			}
			if iamProfile != nil {
				break
			}
		}
	}

	runOut, err := c.PhaseEC2.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:            aws.String(amiID),
		InstanceType:       ec2types.InstanceTypeT3Small,
		MinCount:           aws.Int32(1),
		MaxCount:           aws.Int32(1),
		KeyName:            aws.String(keyName),
		IamInstanceProfile: iamProfile,
		NetworkInterfaces:  nics,
		TagSpecifications:  tags.ToTagSpec(ec2types.ResourceTypeInstance, t),
	})
	if err != nil {
		return fmt.Errorf("phase 17b jumphost: RunInstances: %w", err)
	}
	if len(runOut.Instances) == 0 || runOut.Instances[0].InstanceId == nil {
		return fmt.Errorf("phase 17b jumphost: RunInstances returned no instance ID")
	}
	instanceID := *runOut.Instances[0].InstanceId

	st.Set("JUMPHOST_ID", instanceID)
	st.Set("JUMPHOST_INSTANCE_ID", instanceID)
	st.Set("JUMPHOST_EXT_IP", extIP)
	st.Set("JUMPHOST_PRIVATE_IP", extIP)
	if intIP != "" {
		st.Set("JUMPHOST_INT_IP", intIP)
	}
	if err := st.Save(); err != nil {
		return err
	}

	if err := waitForInstanceState(ctx, c, instanceID, ec2types.InstanceStateNameRunning); err != nil {
		return fmt.Errorf("phase 17b jumphost: %w", err)
	}

	return ensureEICE(ctx, c, st, cl, extSubnet, sgID)
}

// Phase17bJumphostDown deletes the EICE endpoint, terminates the jumphost instance, and deletes its keypair, ENI, SGs, and IAM role/profile.
func Phase17bJumphostDown(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if st == nil || dryRun {
		return nil
	}
	if c == nil || c.PhaseEC2 == nil {
		return fmt.Errorf("phase 17b jumphost down: EC2 client is nil")
	}

	jhID := st.Get("JUMPHOST_ID")
	if jhID == "" {
		jhID = st.Get("JUMPHOST_INSTANCE_ID")
	}
	if jhID != "" && cl != nil && cl.Forge != nil && cl.Forge.Enabled {
		restURL := cl.Forge.ResolveURL()
		if restURL != "" {
			password, _ := cl.Forge.ResolvePassword()
			_ = forge.DeleteBenchmarkAgentByName(ctx, restURL, forge.RestCreds{
				Username: cl.Forge.ResolveUsername(),
				Password: password,
			}, "awsbnkctl-jumphost-"+jhID)
		}
	}

	eiceID := st.Get("EICE_ID")
	if eiceID == "" {
		eiceID = st.Get("JUMPHOST_EICE_ID")
	}
	if eiceID == "" {
		if vpcID := st.Get("VPC_ID"); vpcID != "" {
			if listOut, lerr := c.PhaseEC2.DescribeInstanceConnectEndpoints(ctx, &ec2.DescribeInstanceConnectEndpointsInput{
				Filters: []ec2types.Filter{
					{Name: aws.String("vpc-id"), Values: []string{vpcID}},
				},
			}); lerr == nil {
				for _, ep := range listOut.InstanceConnectEndpoints {
					if ep.InstanceConnectEndpointId != nil && ep.State != ec2types.Ec2InstanceConnectEndpointStateDeleteComplete {
						eiceID = *ep.InstanceConnectEndpointId
						break
					}
				}
			}
		}
	}
	if eiceID == "" && cl != nil && cl.Metadata.Name != "" {
		if listOut, lerr := c.PhaseEC2.DescribeInstanceConnectEndpoints(ctx, &ec2.DescribeInstanceConnectEndpointsInput{
			Filters: []ec2types.Filter{tags.ClusterFilter(cl.Metadata.Name)},
		}); lerr == nil && listOut != nil {
			for _, ep := range listOut.InstanceConnectEndpoints {
				if ep.InstanceConnectEndpointId != nil && ep.State != ec2types.Ec2InstanceConnectEndpointStateDeleteComplete {
					eiceID = *ep.InstanceConnectEndpointId
					break
				}
			}
		}
	}
	if eiceID != "" {
		_, err := c.PhaseEC2.DeleteInstanceConnectEndpoint(ctx, &ec2.DeleteInstanceConnectEndpointInput{
			InstanceConnectEndpointId: aws.String(eiceID),
		})
		if err != nil && !awsmw.IsNotFound(err) && !strings.Contains(err.Error(), "InvalidState") {
			return fmt.Errorf("phase 17b jumphost down: DeleteInstanceConnectEndpoint %s: %w", eiceID, err)
		}
		if err := waitForEICEState(ctx, c, eiceID, ec2types.Ec2InstanceConnectEndpointStateDeleteComplete); err != nil {
			return fmt.Errorf("phase 17b jumphost down: %w", err)
		}
		st.Delete("EICE_ID")
		st.Delete("JUMPHOST_EICE_ID")
		_ = st.Save()
	}

	if jhID != "" {
		_, err := c.PhaseEC2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
			InstanceIds: []string{jhID},
		})
		if err != nil && !awsmw.IsNotFound(err) {
			return fmt.Errorf("phase 17b jumphost down: TerminateInstances %s: %w", jhID, err)
		}
		if err := waitForInstanceState(ctx, c, jhID, ec2types.InstanceStateNameTerminated); err != nil {
			return fmt.Errorf("phase 17b jumphost down: %w", err)
		}
		st.Delete("JUMPHOST_ID")
		st.Delete("JUMPHOST_INSTANCE_ID")
		st.Delete("JUMPHOST_EXT_IP")
		st.Delete("JUMPHOST_INT_IP")
		st.Delete("JUMPHOST_PRIVATE_IP")
		st.Delete("JUMPHOST_MGMT_IP")
		st.Delete("JUMPHOST_PUBLIC_IP")
		st.Delete("JUMPHOST_BNK_EXT_IP")
		st.Delete("JUMPHOST_BNK_EXT_MAC")
		st.Delete("JUMPHOST_AZ")
		st.Delete("JUMPHOST_AMI_ID")
		st.Delete("JUMPHOST_SSM_READY")
		st.Delete("JUMPHOST_NETPLAN_READY")
		_ = st.Save()
	}

	for _, eniKey := range []string{"JUMPHOST_BNK_EXT_ENI_ID", "JUMPHOST_MGMT_ENI_ID"} {
		if jhENI := st.Get(eniKey); jhENI != "" {
			if err := detachAndDeleteENIByID(ctx, c, jhENI); err != nil {
				return fmt.Errorf("phase 17b jumphost down: %w", err)
			}
			st.Delete(eniKey)
			_ = st.Save()
		}
	}
	st.Delete("JUMPHOST_MGMT_ENI_IP")
	st.Delete("JUMPHOST_BNK_EXT_ENI_IP")
	st.Delete("JUMPHOST_INSTANCE_TYPE")

	if keyName := st.Get("JUMPHOST_KEY_NAME"); keyName != "" {
		_, err := c.PhaseEC2.DeleteKeyPair(ctx, &ec2.DeleteKeyPairInput{
			KeyName: aws.String(keyName),
		})
		if err != nil && !awsmw.IsNotFound(err) {
			return fmt.Errorf("phase 17b jumphost down: DeleteKeyPair %s: %w", keyName, err)
		}
		st.Delete("JUMPHOST_KEY_NAME")
	}
	if keyPath := st.Get("JUMPHOST_KEY_PATH"); keyPath != "" {
		_ = os.Remove(keyPath)
		st.Delete("JUMPHOST_KEY_PATH")
	}
	if st.Dir() != "" {
		_ = os.Remove(filepath.Join(st.Dir(), "jumphost.pem"))
	}

	jhSG := st.Get("JUMPHOST_SG_ID")
	eiceSG := st.Get("JUMPHOST_EICE_SG_ID")
	eksSG := st.Get("EKS_SECURITY_GROUP")
	if eksSG == "" {
		eksSG = st.Get("EKS_CLUSTER_SG_ID")
	}
	if eksSG != "" {
		revokeAllSecurityGroupRules(ctx, c, eksSG)
	}

	targetSGs := []struct {
		key string
		id  string
	}{
		{key: "JUMPHOST_EICE_SG_ID", id: eiceSG},
		{key: "JUMPHOST_SG_ID", id: jhSG},
	}
	if jhSG == "" && eiceSG == "" && cl != nil && cl.Metadata.Name != "" {
		sgName := cl.Metadata.Name + "-jumphost-sg"
		altName := cl.Metadata.Name + "-jumphost"
		if sgOut, serr := c.PhaseEC2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
			Filters: []ec2types.Filter{
				tags.ClusterFilter(cl.Metadata.Name),
			},
		}); serr == nil && sgOut != nil {
			for _, sg := range sgOut.SecurityGroups {
				if sg.GroupId == nil || *sg.GroupId == "" {
					continue
				}
				gname := aws.ToString(sg.GroupName)
				isJumphostSG := gname == sgName || gname == altName
				if !isJumphostSG {
					for _, t := range sg.Tags {
						k := aws.ToString(t.Key)
						v := aws.ToString(t.Value)
						if (k == tags.KeyComponent && v == tags.CompJumphostSG) ||
							(k == "Name" && (v == sgName || v == altName)) {
							isJumphostSG = true
							break
						}
					}
				}
				if isJumphostSG {
					targetSGs = append(targetSGs, struct {
						key string
						id  string
					}{key: "JUMPHOST_SG_ID", id: *sg.GroupId})
				}
			}
		}
	}

	vpcID := st.Get("VPC_ID")
	deletedSGs := map[string]bool{}
	for _, item := range targetSGs {
		if item.id == "" || deletedSGs[item.id] {
			continue
		}
		revokeReferencesToSG(ctx, c, vpcID, item.id)
		_, err := c.PhaseEC2.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{
			GroupId: aws.String(item.id),
		})
		if err != nil && !awsmw.IsNotFound(err) {
			if awsmw.IsDependencyViolation(err) || strings.Contains(err.Error(), "CannotDelete") {
				fmt.Fprintf(os.Stderr, "phase 17b jumphost down: warning: security group %s still has dependencies (%v); deferring to VPC blocker sweep\n", item.id, err)
			} else {
				return fmt.Errorf("phase 17b jumphost down: DeleteSecurityGroup %s: %w", item.id, err)
			}
		}
		deletedSGs[item.id] = true
		if item.key != "" {
			st.Delete(item.key)
		}
	}

	if c.PhaseIAM != nil {
		roleName := st.Get("JUMPHOST_ROLE_NAME")
		if roleName == "" && cl != nil && cl.Metadata.Name != "" {
			roleName = cl.Metadata.Name + "-jumphost-role"
		}
		profName := st.Get("JUMPHOST_INSTANCE_PROFILE_NAME")
		if profName == "" && cl != nil && cl.Metadata.Name != "" {
			profName = cl.Metadata.Name + "-jumphost-profile"
		}
		if roleName != "" && profName != "" {
			_, _ = c.PhaseIAM.RemoveRoleFromInstanceProfile(ctx, &iam.RemoveRoleFromInstanceProfileInput{
				InstanceProfileName: aws.String(profName),
				RoleName:            aws.String(roleName),
			})
		}
		if profName != "" {
			_, _ = c.PhaseIAM.DeleteInstanceProfile(ctx, &iam.DeleteInstanceProfileInput{
				InstanceProfileName: aws.String(profName),
			})
			st.Delete("JUMPHOST_INSTANCE_PROFILE_NAME")
		}
		if roleName != "" {
			if err := deleteRoleWithPolicies(ctx, c, roleName); err != nil {
				return fmt.Errorf("phase 17b jumphost down: %w", err)
			}
			st.Delete("JUMPHOST_ROLE_NAME")
		}
	}

	return st.Save()
}

func permReferencesSG(perm ec2types.IpPermission, targetSGID string) bool {
	for _, pair := range perm.UserIdGroupPairs {
		if pair.GroupId != nil && *pair.GroupId == targetSGID {
			return true
		}
	}
	return false
}

// revokeReferencesToSG revokes all ingress/egress rules on targetSGID as well as any
// ingress/egress rules on other security groups in vpcID that reference targetSGID.
func revokeReferencesToSG(ctx context.Context, c *Clients, vpcID, targetSGID string) {
	if c == nil || c.PhaseEC2 == nil || targetSGID == "" {
		return
	}
	if desc, err := c.PhaseEC2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		GroupIds: []string{targetSGID},
	}); err == nil && desc != nil && len(desc.SecurityGroups) > 0 {
		sg := desc.SecurityGroups[0]
		if vpcID == "" && sg.VpcId != nil {
			vpcID = *sg.VpcId
		}
		if len(sg.IpPermissions) > 0 {
			_, _ = c.PhaseEC2.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
				GroupId:       aws.String(targetSGID),
				IpPermissions: sg.IpPermissions,
			})
		}
		if len(sg.IpPermissionsEgress) > 0 {
			_, _ = c.PhaseEC2.RevokeSecurityGroupEgress(ctx, &ec2.RevokeSecurityGroupEgressInput{
				GroupId:       aws.String(targetSGID),
				IpPermissions: sg.IpPermissionsEgress,
			})
		}
	}
	if vpcID == "" {
		return
	}
	out, err := c.PhaseEC2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
		},
	})
	if err != nil || out == nil {
		return
	}
	for _, sg := range out.SecurityGroups {
		if sg.GroupId == nil || *sg.GroupId == targetSGID {
			continue
		}
		var matchingIngress []ec2types.IpPermission
		for _, perm := range sg.IpPermissions {
			if permReferencesSG(perm, targetSGID) {
				matchingIngress = append(matchingIngress, perm)
			}
		}
		if len(matchingIngress) > 0 {
			_, _ = c.PhaseEC2.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
				GroupId:       sg.GroupId,
				IpPermissions: matchingIngress,
			})
		}
		var matchingEgress []ec2types.IpPermission
		for _, perm := range sg.IpPermissionsEgress {
			if permReferencesSG(perm, targetSGID) {
				matchingEgress = append(matchingEgress, perm)
			}
		}
		if len(matchingEgress) > 0 {
			_, _ = c.PhaseEC2.RevokeSecurityGroupEgress(ctx, &ec2.RevokeSecurityGroupEgressInput{
				GroupId:       sg.GroupId,
				IpPermissions: matchingEgress,
			})
		}
	}
}

func revokeAllSecurityGroupRules(ctx context.Context, c *Clients, sgID string) {
	if c == nil || c.PhaseEC2 == nil || sgID == "" {
		return
	}
	out, err := c.PhaseEC2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		GroupIds: []string{sgID},
	})
	if err != nil || len(out.SecurityGroups) == 0 {
		return
	}
	sg := out.SecurityGroups[0]
	if len(sg.IpPermissions) > 0 {
		_, _ = c.PhaseEC2.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
			GroupId:       aws.String(sgID),
			IpPermissions: sg.IpPermissions,
		})
	}
	if len(sg.IpPermissionsEgress) > 0 {
		_, _ = c.PhaseEC2.RevokeSecurityGroupEgress(ctx, &ec2.RevokeSecurityGroupEgressInput{
			GroupId:       aws.String(sgID),
			IpPermissions: sg.IpPermissionsEgress,
		})
	}
}
