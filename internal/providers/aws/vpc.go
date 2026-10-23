package aws

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// VPCAPI is the subset of the EC2 client's VPC surface awsbnkctl uses
// for the "use existing VPC" path. Kept as a distinct interface from
// EC2API so tests can mock the two
// independently — the doctor pre-flight calls a small set of VPC
// methods that's deliberately separate from the instance-type probes.
type VPCAPI interface {
	DescribeVpcs(ctx context.Context, in *ec2.DescribeVpcsInput, opts ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error)
	DescribeSubnets(ctx context.Context, in *ec2.DescribeSubnetsInput, opts ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
}

// VPCInfo is the awsbnkctl-shaped projection of ec2:DescribeVpcs.
type VPCInfo struct {
	ID        string
	CidrBlock string
	State     string
	IsDefault bool
}

// DescribeVpc returns details for the given VPC ID. Used by the
// doctor's "VPC reachable" check and by `awsbnkctl init`'s validation
// when the user pre-creates the VPC.
func (c *Clients) DescribeVpc(ctx context.Context, vpcID string) (*VPCInfo, error) {
	if c == nil || c.VPC == nil {
		return nil, fmt.Errorf("aws.Clients is nil")
	}
	if vpcID == "" {
		return nil, errors.New("vpcID is empty")
	}
	out, err := c.VPC.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		VpcIds: []string{vpcID},
	})
	if err != nil {
		return nil, fmt.Errorf("ec2:DescribeVpcs %s: %w", vpcID, err)
	}
	if len(out.Vpcs) == 0 {
		return nil, fmt.Errorf("vpc %s not found", vpcID)
	}
	v := out.Vpcs[0]
	return &VPCInfo{
		ID:        aws_string_or_empty(v.VpcId),
		CidrBlock: aws_string_or_empty(v.CidrBlock),
		State:     string(v.State),
		IsDefault: v.IsDefault != nil && *v.IsDefault,
	}, nil
}

// SubnetInfo is the awsbnkctl-shaped projection of ec2:DescribeSubnets.
// The cluster requires >=3 AZs for HA; the doctor pre-flight validates
// this from the returned AZ set.
type SubnetInfo struct {
	ID               string
	VpcID            string
	AvailabilityZone string
	CidrBlock        string
	State            string
}

// DescribeSubnets returns details for the given subnet IDs.
func (c *Clients) DescribeSubnets(ctx context.Context, subnetIDs []string) ([]SubnetInfo, error) {
	if c == nil || c.VPC == nil {
		return nil, fmt.Errorf("aws.Clients is nil")
	}
	if len(subnetIDs) == 0 {
		return nil, nil
	}
	out, err := c.VPC.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		SubnetIds: subnetIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("ec2:DescribeSubnets: %w", err)
	}
	res := make([]SubnetInfo, 0, len(out.Subnets))
	for _, s := range out.Subnets {
		res = append(res, SubnetInfo{
			ID:               aws_string_or_empty(s.SubnetId),
			VpcID:            aws_string_or_empty(s.VpcId),
			AvailabilityZone: aws_string_or_empty(s.AvailabilityZone),
			CidrBlock:        aws_string_or_empty(s.CidrBlock),
			State:            string(s.State),
		})
	}
	return res, nil
}

// CountUniqueAZs returns the number of distinct AZs across the given
// subnets. The cluster requires >=3 AZs; the doctor surfaces a failure
// when this drops below 3.
func CountUniqueAZs(subnets []SubnetInfo) int {
	seen := map[string]struct{}{}
	for _, s := range subnets {
		if s.AvailabilityZone == "" {
			continue
		}
		seen[s.AvailabilityZone] = struct{}{}
	}
	return len(seen)
}

// silenceUnused keeps the ec2types import referenced — needed once
// we extend the filter surface to subnet tag matching in v0.x.
var _ = ec2types.Filter{}
