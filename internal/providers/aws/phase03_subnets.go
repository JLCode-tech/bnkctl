package aws

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/JLCode-tech/bnkctl/internal/providers/aws/awsmw"
	"github.com/JLCode-tech/bnkctl/internal/providers/aws/tags"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func resolveClusterAZs(ctx context.Context, cl *intent.Cluster, ec2c PhaseEC2API) {
	if cl == nil || ec2c == nil {
		return
	}
	out, err := ec2c.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("state"), Values: []string{"available"}},
			{Name: aws.String("opt-in-status"), Values: []string{"opt-in-not-required", "opted-in"}},
		},
	})
	if err != nil || out == nil || len(out.AvailabilityZones) == 0 {
		return
	}
	var avail []string
	availSet := make(map[string]bool, len(out.AvailabilityZones))
	for _, z := range out.AvailabilityZones {
		if z.ZoneName == nil || *z.ZoneName == "" {
			continue
		}
		if z.ZoneType != nil && *z.ZoneType != "" && *z.ZoneType != "availability-zone" {
			continue
		}
		name := *z.ZoneName
		avail = append(avail, name)
		availSet[name] = true
	}
	if len(avail) == 0 {
		return
	}
	sort.Strings(avail)

	needRemap := len(cl.Network.AZs) == 0
	for _, az := range cl.Network.AZs {
		if az == "" || az == "auto" || !availSet[az] {
			needRemap = true
			break
		}
	}
	if needRemap {
		count := len(cl.Network.AZs)
		if count < 2 {
			count = 2
		}
		if count > len(avail) {
			count = len(avail)
		}
		cl.Network.AZs = append([]string(nil), avail[:count]...)
	}
	if len(cl.Network.AZs) == 0 {
		return
	}
	for i := range cl.Network.Subnets.PublicList {
		az := cl.Network.Subnets.PublicList[i].AZ
		if az == "" || az == "auto" || !availSet[az] {
			cl.Network.Subnets.PublicList[i].AZ = cl.Network.AZs[i%len(cl.Network.AZs)]
		}
	}
	for i := range cl.Network.Subnets.PrivateList {
		az := cl.Network.Subnets.PrivateList[i].AZ
		if az == "" || az == "auto" || !availSet[az] {
			cl.Network.Subnets.PrivateList[i].AZ = cl.Network.AZs[i%len(cl.Network.AZs)]
		}
	}
	if cl.Network.DataPath != nil {
		if cl.Network.DataPath.External.CIDR != "" {
			az := cl.Network.DataPath.External.AZ
			if az == "" || az == "auto" || !availSet[az] {
				cl.Network.DataPath.External.AZ = cl.Network.AZs[0]
			}
		}
		if cl.Network.DataPath.Internal.CIDR != "" {
			az := cl.Network.DataPath.Internal.AZ
			if az == "" || az == "auto" || !availSet[az] {
				cl.Network.DataPath.Internal.AZ = cl.Network.AZs[0]
			}
		}
	}
	fmt.Fprintf(os.Stderr, "[phase 03] dynamically discovered available AZs: %v (active: %v)\n", avail, cl.Network.AZs)
}

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

	resolveClusterAZs(ctx, cl, c.PhaseEC2)
	if len(cl.Network.AZs) > 0 {
		st.Set("ACTIVE_AZS", strings.Join(cl.Network.AZs, ","))
	}

	clusterTag := "kubernetes.io/cluster/" + cl.Metadata.Name

	for i, spec := range cl.Network.Subnets.PublicList {
		key := subnetStateKey("PUBLIC_SUBNET", i)
		name := fmt.Sprintf("%s-public-%d", cl.Metadata.Name, i)
		extra := map[string]string{
			clusterTag:               "shared",
			"kubernetes.io/role/elb": "1",
			"awsbnkctl:component":    "subnet-public",
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
			"awsbnkctl:component":             "subnet-private",
		}
		if _, err := ensureSubnet(ctx, c, st, cl, vpcID, key, name, spec, false, extra); err != nil {
			return fmt.Errorf("phase 03 subnets: %w", err)
		}
	}

	if cl.HasExternalDataPath() && cl.Network.DataPath != nil {
		name := cl.Metadata.Name + "-bnk-ext"
		extra := map[string]string{
			"bnk.f5.com/datapath": "external",
			"awsbnkctl:component": "subnet-bnk-ext",
		}
		if _, err := ensureSubnet(ctx, c, st, cl, vpcID, "BNK_EXT_SUBNET", name, cl.Network.DataPath.External, false, extra); err != nil {
			return fmt.Errorf("phase 03 subnets: %w", err)
		}
	}

	if cl.HasInternalInterface() && cl.Network.DataPath != nil {
		name := cl.Metadata.Name + "-bnk-int"
		extra := map[string]string{
			"bnk.f5.com/datapath": "internal",
			"awsbnkctl:component": "subnet-bnk-int",
		}
		if _, err := ensureSubnet(ctx, c, st, cl, vpcID, "BNK_INT_SUBNET", name, cl.Network.DataPath.Internal, false, extra); err != nil {
			return fmt.Errorf("phase 03 subnets: %w", err)
		}
	}

	return nil
}

// sweepAvailableSubnetENIs deletes any unattached ("available") ENIs in subnetID.
func sweepAvailableSubnetENIs(ctx context.Context, ec2c PhaseEC2API, subnetID string) {
	if ec2c == nil || subnetID == "" {
		return
	}
	out, err := ec2c.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("subnet-id"), Values: []string{subnetID}},
		},
	})
	if err != nil || out == nil {
		return
	}
	for _, eni := range out.NetworkInterfaces {
		if eni.NetworkInterfaceId == nil || eni.Status != ec2types.NetworkInterfaceStatusAvailable {
			continue
		}
		_, _ = ec2c.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{
			NetworkInterfaceId: eni.NetworkInterfaceId,
		})
	}
}

// Phase03SubnetsDown deletes all subnets recorded in state and any remaining tagged/VPC subnets.
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
	keys = append(keys, "BNK_INT_SUBNET", "BNK_EXT_SUBNET", "SUBNET_INT_ID", "SUBNET_EXT_ID")
	for i := len(cl.Network.Subnets.PrivateList) - 1; i >= 0; i-- {
		keys = append(keys, subnetStateKey("PRIVATE_SUBNET", i))
	}
	for i := len(cl.Network.Subnets.PublicList) - 1; i >= 0; i-- {
		keys = append(keys, subnetStateKey("PUBLIC_SUBNET", i))
	}

	deleted := map[string]bool{}
	deleteSubnetByID := func(id, source string) error {
		id = strings.TrimSpace(id)
		if id == "" || strings.HasPrefix(id, "dry-run-") || deleted[id] {
			return nil
		}
		sweepAvailableSubnetENIs(ctx, c.PhaseEC2, id)
		_, err := c.PhaseEC2.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{
			SubnetId: aws.String(id),
		})
		if err != nil && awsmw.IsDependencyViolation(err) {
			if vpcID := st.Get("VPC_ID"); vpcID != "" {
				sweepVPCInstancesAndENIs(ctx, c, vpcID)
			}
			sweepAvailableSubnetENIs(ctx, c.PhaseEC2, id)
			_, err = c.PhaseEC2.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{
				SubnetId: aws.String(id),
			})
			if err != nil && awsmw.IsDependencyViolation(err) {
				fmt.Fprintf(os.Stderr, "phase 03 subnets down: warning: subnet %s (%s) still has dependencies (%v); deferring to VPC blocker sweep\n", id, source, err)
				deleted[id] = true
				return nil
			}
		}
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

	if cl.Metadata.Name != "" {
		if subOut, serr := c.PhaseEC2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
			Filters: []ec2types.Filter{tags.ClusterFilter(cl.Metadata.Name)},
		}); serr == nil && subOut != nil {
			for _, s := range subOut.Subnets {
				if s.SubnetId != nil {
					if err := deleteSubnetByID(*s.SubnetId, "tag-discovery"); err != nil {
						return err
					}
				}
			}
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
	st.Delete("ACTIVE_AZS")
	return st.Save()
}
