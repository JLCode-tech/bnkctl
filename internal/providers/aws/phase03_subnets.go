package aws

import (
	"context"
	"fmt"
	"strings"

	"github.com/JLCode-tech/bnkctl/internal/providers/aws/awsmw"
	"github.com/JLCode-tech/bnkctl/internal/providers/aws/tags"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func subnetStateKey(prefix string, index int) string {
	return fmt.Sprintf("%s_%d", prefix, index)
}

func ensureSubnet(
	ctx context.Context,
	c *Clients,
	st *state.State,
	cl *intent.Cluster,
	vpcID string,
	stateKey string,
	name string,
	spec intent.SubnetSpec,
	mapPublicIP bool,
	extraTags map[string]string,
) (string, error) {
	if existing := st.Get(stateKey); existing != "" {
		out, err := c.PhaseEC2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
			SubnetIds: []string{existing},
		})
		if err == nil && len(out.Subnets) > 0 {
			return existing, nil
		}
		if err != nil && !awsmw.IsNotFound(err) {
			return "", fmt.Errorf("describing existing subnet %s (%s): %w", existing, stateKey, err)
		}
		st.Delete(stateKey)
	}

	mergedExtra := map[string]string{"Name": name}
	for k, v := range extraTags {
		mergedExtra[k] = v
	}
	t := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, mergedExtra)

	out, err := c.PhaseEC2.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId:             aws.String(vpcID),
		CidrBlock:         aws.String(spec.CIDR),
		AvailabilityZone:  aws.String(spec.AZ),
		TagSpecifications: tags.ToTagSpec(ec2types.ResourceTypeSubnet, t),
	})
	if err != nil {
		return "", fmt.Errorf("CreateSubnet %s (%s): %w", name, spec.CIDR, err)
	}
	if out.Subnet == nil || out.Subnet.SubnetId == nil {
		return "", fmt.Errorf("CreateSubnet %s returned nil SubnetId", name)
	}
	subnetID := *out.Subnet.SubnetId

	if mapPublicIP {
		if _, err := c.PhaseEC2.ModifySubnetAttribute(ctx, &ec2.ModifySubnetAttributeInput{
			SubnetId:            aws.String(subnetID),
			MapPublicIpOnLaunch: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)},
		}); err != nil {
			return "", fmt.Errorf("ModifySubnetAttribute MapPublicIpOnLaunch on %s: %w", subnetID, err)
		}
	}

	st.Set(stateKey, subnetID)
	if err := st.Save(); err != nil {
		return "", err
	}
	return subnetID, nil
}

// Phase03Subnets creates all public, private, and BNK data-path subnets.
func Phase03Subnets(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 03 subnets: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 03 subnets: state is nil")
	}
	if dryRun {
		return nil
	}
	if c == nil || c.PhaseEC2 == nil {
		return fmt.Errorf("phase 03 subnets: EC2 client is nil")
	}
	vpcID := st.Get("VPC_ID")
	if vpcID == "" {
		return fmt.Errorf("phase 03 subnets: VPC_ID not found in state (did phase 02 run?)")
	}

	clusterTag := "kubernetes.io/cluster/" + cl.Metadata.Name

	for i, spec := range cl.Network.Subnets.PublicList {
		key := subnetStateKey("PUBLIC_SUBNET", i)
		name := fmt.Sprintf("%s-public-%d", cl.Metadata.Name, i)
		extra := map[string]string{
			clusterTag:               "shared",
			"kubernetes.io/role/elb": "1",
		}
		id, err := ensureSubnet(ctx, c, st, cl, vpcID, key, name, spec, true, extra)
		if err != nil {
			return fmt.Errorf("phase 03 subnets: %w", err)
		}
		if i == 0 {
			st.Set("MGMT_SUBNET", id)
			if err := st.Save(); err != nil {
				return err
			}
		}
	}

	for i, spec := range cl.Network.Subnets.PrivateList {
		key := subnetStateKey("PRIVATE_SUBNET", i)
		name := fmt.Sprintf("%s-private-%d", cl.Metadata.Name, i)
		extra := map[string]string{
			clusterTag:                        "shared",
			"kubernetes.io/role/internal-elb": "1",
		}
		if _, err := ensureSubnet(ctx, c, st, cl, vpcID, key, name, spec, false, extra); err != nil {
			return fmt.Errorf("phase 03 subnets: %w", err)
		}
	}

	if cl.HasExternalDataPath() && cl.Network.DataPath != nil {
		name := cl.Metadata.Name + "-bnk-ext"
		extra := map[string]string{
			"bnk.f5.com/datapath": "external",
		}
		if _, err := ensureSubnet(ctx, c, st, cl, vpcID, "BNK_EXT_SUBNET", name, cl.Network.DataPath.External, false, extra); err != nil {
			return fmt.Errorf("phase 03 subnets: %w", err)
		}
	}

	if cl.HasInternalInterface() && cl.Network.DataPath != nil {
		name := cl.Metadata.Name + "-bnk-int"
		extra := map[string]string{
			"bnk.f5.com/datapath": "internal",
		}
		if _, err := ensureSubnet(ctx, c, st, cl, vpcID, "BNK_INT_SUBNET", name, cl.Network.DataPath.Internal, false, extra); err != nil {
			return fmt.Errorf("phase 03 subnets: %w", err)
		}
	}

	return nil
}

// Phase03SubnetsDown deletes all subnets recorded in state in reverse order.
func Phase03SubnetsDown(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 03 subnets down: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 03 subnets down: state is nil")
	}
	if dryRun {
		return nil
	}
	if c == nil || c.PhaseEC2 == nil {
		return fmt.Errorf("phase 03 subnets down: EC2 client is nil")
	}

	var keys []string
	keys = append(keys, "BNK_INT_SUBNET", "BNK_EXT_SUBNET")
	for i := len(cl.Network.Subnets.PrivateList) - 1; i >= 0; i-- {
		keys = append(keys, subnetStateKey("PRIVATE_SUBNET", i))
	}
	for i := len(cl.Network.Subnets.PublicList) - 1; i >= 0; i-- {
		keys = append(keys, subnetStateKey("PUBLIC_SUBNET", i))
	}

	deleted := map[string]bool{}
	deleteSubnetByID := func(id, source string) error {
		id = strings.TrimSpace(id)
		if id == "" || deleted[id] {
			return nil
		}
		_, err := c.PhaseEC2.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{
			SubnetId: aws.String(id),
		})
		if err != nil && !awsmw.IsNotFound(err) {
			return fmt.Errorf("phase 03 subnets down: DeleteSubnet %s (%s): %w", id, source, err)
		}
		deleted[id] = true
		return nil
	}

	for _, k := range keys {
		id := st.Get(k)
		if id == "" {
			continue
		}
		if err := deleteSubnetByID(id, k); err != nil {
			return err
		}
		st.Delete(k)
	}

	for _, listKey := range []string{"PRIVATE_SUBNETS", "PUBLIC_SUBNETS"} {
		if raw := st.Get(listKey); raw != "" {
			for _, part := range strings.Split(raw, ",") {
				if err := deleteSubnetByID(part, listKey); err != nil {
					return err
				}
			}
			st.Delete(listKey)
		}
	}

	if vpcID := st.Get("VPC_ID"); vpcID != "" {
		if subOut, serr := c.PhaseEC2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
			Filters: []ec2types.Filter{
				{Name: aws.String("vpc-id"), Values: []string{vpcID}},
			},
		}); serr == nil && subOut != nil {
			for _, s := range subOut.Subnets {
				if s.SubnetId != nil {
					if err := deleteSubnetByID(*s.SubnetId, "vpc-discovery"); err != nil {
						return err
					}
				}
			}
		}
	}

	st.Delete("MGMT_SUBNET")
	return st.Save()
}
