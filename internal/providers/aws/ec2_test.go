package aws

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type fakeEC2 struct {
	offeringsOut *ec2.DescribeInstanceTypeOfferingsOutput
	offeringsErr error

	instanceTypesOut *ec2.DescribeInstanceTypesOutput
	instanceTypesErr error

	accountOut *ec2.DescribeAccountAttributesOutput
	accountErr error
}

func (f *fakeEC2) DescribeInstanceTypeOfferings(ctx context.Context, in *ec2.DescribeInstanceTypeOfferingsInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
	return f.offeringsOut, f.offeringsErr
}

func (f *fakeEC2) DescribeInstanceTypes(ctx context.Context, in *ec2.DescribeInstanceTypesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	return f.instanceTypesOut, f.instanceTypesErr
}

func (f *fakeEC2) DescribeAccountAttributes(ctx context.Context, in *ec2.DescribeAccountAttributesInput, opts ...func(*ec2.Options)) (*ec2.DescribeAccountAttributesOutput, error) {
	return f.accountOut, f.accountErr
}

func TestInstanceTypeOfferings_NilSafe(t *testing.T) {
	var c *Clients
	_, err := c.InstanceTypeOfferings(context.Background(), []string{"c5n.4xlarge"})
	if err == nil {
		t.Fatal("expected error on nil Clients")
	}
}

func TestInstanceTypeOfferings_EmptyInput(t *testing.T) {
	c := &Clients{EC2: &fakeEC2{}}
	res, err := c.InstanceTypeOfferings(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != nil {
		t.Errorf("expected nil result, got %v", res)
	}
}

func TestInstanceTypeOfferings_Projects(t *testing.T) {
	az := "us-east-1a"
	c := &Clients{
		EC2: &fakeEC2{
			offeringsOut: &ec2.DescribeInstanceTypeOfferingsOutput{
				InstanceTypeOfferings: []ec2types.InstanceTypeOffering{
					{
						InstanceType: ec2types.InstanceTypeC5n4xlarge,
						Location:     &az,
					},
				},
			},
		},
	}
	res, err := c.InstanceTypeOfferings(context.Background(), []string{"c5n.4xlarge"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("expected 1 offering, got %d", len(res))
	}
	if res[0].InstanceType != "c5n.4xlarge" {
		t.Errorf("InstanceType: got %q", res[0].InstanceType)
	}
	if res[0].Location != az {
		t.Errorf("Location: got %q", res[0].Location)
	}
}

func TestDescribeInstanceCapabilities_Projects(t *testing.T) {
	efa := true
	maxENI := int32(8)
	c := &Clients{
		EC2: &fakeEC2{
			instanceTypesOut: &ec2.DescribeInstanceTypesOutput{
				InstanceTypes: []ec2types.InstanceTypeInfo{
					{
						InstanceType: ec2types.InstanceTypeC5n4xlarge,
						NetworkInfo: &ec2types.NetworkInfo{
							EnaSupport:               ec2types.EnaSupportRequired,
							EfaSupported:             &efa,
							MaximumNetworkInterfaces: &maxENI,
						},
					},
				},
			},
		},
	}
	res, err := c.DescribeInstanceCapabilities(context.Background(), []string{"c5n.4xlarge"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("expected 1 result, got %d", len(res))
	}
	if res[0].ENASupport != string(ec2types.EnaSupportRequired) {
		t.Errorf("ENASupport: got %q", res[0].ENASupport)
	}
	if !res[0].EFASupported {
		t.Error("EFASupported: expected true")
	}
	if res[0].MaxENIs != maxENI {
		t.Errorf("MaxENIs: got %d", res[0].MaxENIs)
	}
}

func TestVCPUQuotaAttribute_PropagatesErr(t *testing.T) {
	sentinel := errors.New("denied")
	c := &Clients{EC2: &fakeEC2{accountErr: sentinel}}
	_, err := c.VCPUQuotaAttribute(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}
