package aws

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type fakeVPC struct {
	vpcs    *ec2.DescribeVpcsOutput
	vpcsErr error

	subnets    *ec2.DescribeSubnetsOutput
	subnetsErr error
}

func (f *fakeVPC) DescribeVpcs(ctx context.Context, in *ec2.DescribeVpcsInput, opts ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	return f.vpcs, f.vpcsErr
}
func (f *fakeVPC) DescribeSubnets(ctx context.Context, in *ec2.DescribeSubnetsInput, opts ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	return f.subnets, f.subnetsErr
}

func TestDescribeVpc_Projects(t *testing.T) {
	id := "vpc-1234"
	cidr := "10.0.0.0/16"
	def := false
	c := &Clients{
		VPC: &fakeVPC{
			vpcs: &ec2.DescribeVpcsOutput{
				Vpcs: []ec2types.Vpc{{
					VpcId:     &id,
					CidrBlock: &cidr,
					IsDefault: &def,
					State:     ec2types.VpcStateAvailable,
				}},
			},
		},
	}
	v, err := c.DescribeVpc(context.Background(), id)
	if err != nil {
		t.Fatalf("DescribeVpc: %v", err)
	}
	if v.ID != id || v.CidrBlock != cidr || v.IsDefault {
		t.Fatalf("projection mismatch: %+v", v)
	}
}

func TestDescribeVpc_NotFound(t *testing.T) {
	c := &Clients{
		VPC: &fakeVPC{
			vpcs: &ec2.DescribeVpcsOutput{},
		},
	}
	_, err := c.DescribeVpc(context.Background(), "vpc-missing")
	if err == nil {
		t.Fatal("expected error for missing VPC")
	}
}

func TestDescribeVpc_EmptyID(t *testing.T) {
	c := &Clients{VPC: &fakeVPC{}}
	_, err := c.DescribeVpc(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty vpcID")
	}
}

func TestDescribeSubnets_Projects(t *testing.T) {
	id1, id2 := "subnet-a", "subnet-b"
	az1, az2 := "us-east-1a", "us-east-1b"
	vpcID := "vpc-1"
	cidr := "10.0.1.0/24"
	c := &Clients{
		VPC: &fakeVPC{
			subnets: &ec2.DescribeSubnetsOutput{
				Subnets: []ec2types.Subnet{
					{SubnetId: &id1, VpcId: &vpcID, AvailabilityZone: &az1, CidrBlock: &cidr, State: ec2types.SubnetStateAvailable},
					{SubnetId: &id2, VpcId: &vpcID, AvailabilityZone: &az2, CidrBlock: &cidr, State: ec2types.SubnetStateAvailable},
				},
			},
		},
	}
	subs, err := c.DescribeSubnets(context.Background(), []string{id1, id2})
	if err != nil {
		t.Fatalf("DescribeSubnets: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("expected 2 subnets, got %d", len(subs))
	}
	if subs[0].AvailabilityZone != az1 || subs[1].AvailabilityZone != az2 {
		t.Fatalf("AZ ordering mismatch: %+v", subs)
	}
}

func TestDescribeSubnets_PropagatesErr(t *testing.T) {
	sentinel := errors.New("denied")
	c := &Clients{VPC: &fakeVPC{subnetsErr: sentinel}}
	_, err := c.DescribeSubnets(context.Background(), []string{"subnet-x"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

func TestCountUniqueAZs(t *testing.T) {
	subs := []SubnetInfo{
		{AvailabilityZone: "us-east-1a"},
		{AvailabilityZone: "us-east-1a"},
		{AvailabilityZone: "us-east-1b"},
		{AvailabilityZone: "us-east-1c"},
		{AvailabilityZone: ""}, // ignored
	}
	if got := CountUniqueAZs(subs); got != 3 {
		t.Fatalf("expected 3, got %d", got)
	}
}
