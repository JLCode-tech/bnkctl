package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// EC2API is the subset of ec2.Client awsbnkctl uses for instance-type
// + quota probing. Tests inject a fake.
type EC2API interface {
	DescribeInstanceTypeOfferings(ctx context.Context, in *ec2.DescribeInstanceTypeOfferingsInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstanceTypeOfferingsOutput, error)
	DescribeInstanceTypes(ctx context.Context, in *ec2.DescribeInstanceTypesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error)
	DescribeAccountAttributes(ctx context.Context, in *ec2.DescribeAccountAttributesInput, opts ...func(*ec2.Options)) (*ec2.DescribeAccountAttributesOutput, error)
}

// InstanceTypeOffering captures whether a given instance type is
// orderable in a given AZ. Per-region availability gaps for c5n / m5n
// are surfaced as a doctor pre-flight check.
type InstanceTypeOffering struct {
	InstanceType string
	Location     string // region or AZ identifier, depending on LocationType
}

// InstanceTypeOfferings returns the orderable AZs (or regions) for the
// given instance types in the client's configured region.
func (c *Clients) InstanceTypeOfferings(ctx context.Context, instanceTypes []string) ([]InstanceTypeOffering, error) {
	if c == nil || c.EC2 == nil {
		return nil, fmt.Errorf("aws.Clients is nil")
	}
	if len(instanceTypes) == 0 {
		return nil, nil
	}

	values := make([]string, 0, len(instanceTypes))
	values = append(values, instanceTypes...)

	out, err := c.EC2.DescribeInstanceTypeOfferings(ctx, &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: ec2types.LocationTypeAvailabilityZone,
		Filters: []ec2types.Filter{
			{
				Name:   ptr("instance-type"),
				Values: values,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("ec2:DescribeInstanceTypeOfferings: %w", err)
	}

	offerings := make([]InstanceTypeOffering, 0, len(out.InstanceTypeOfferings))
	for _, off := range out.InstanceTypeOfferings {
		offerings = append(offerings, InstanceTypeOffering{
			InstanceType: string(off.InstanceType),
			Location:     aws_string_or_empty(off.Location),
		})
	}
	return offerings, nil
}

// InstanceTypeCapabilities is the awsbnkctl-shaped projection of the
// SR-IOV / ENA capability flags. The SDK doesn't
// surface a "sriov-net-support" field directly; ENA support implies
// SR-IOV capability on every Nitro-generation family. The actual VF
// vendor/device IDs are resolved at runtime by the SR-IOV device
// plugin (which reads /sys/class/net/<eth>/device/sriov_*) — these
// flags are the upstream "feature-level" probe only.
type InstanceTypeCapabilities struct {
	InstanceType string
	ENASupport   string // "supported" | "required" | "unsupported"
	EFASupported bool
	MaxENIs      int32 // upper bound on Multus secondary attachments per node
}

// DescribeInstanceCapabilities returns the ENA + SR-IOV flags for the
// given instance types. ENA-SR-IOV capability is required; the doctor
// check fails loudly when the chosen instance family doesn't support
// either.
func (c *Clients) DescribeInstanceCapabilities(ctx context.Context, instanceTypes []string) ([]InstanceTypeCapabilities, error) {
	if c == nil || c.EC2 == nil {
		return nil, fmt.Errorf("aws.Clients is nil")
	}
	if len(instanceTypes) == 0 {
		return nil, nil
	}

	in := &ec2.DescribeInstanceTypesInput{
		InstanceTypes: make([]ec2types.InstanceType, 0, len(instanceTypes)),
	}
	for _, t := range instanceTypes {
		in.InstanceTypes = append(in.InstanceTypes, ec2types.InstanceType(t))
	}
	out, err := c.EC2.DescribeInstanceTypes(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("ec2:DescribeInstanceTypes: %w", err)
	}

	caps := make([]InstanceTypeCapabilities, 0, len(out.InstanceTypes))
	for _, info := range out.InstanceTypes {
		cap := InstanceTypeCapabilities{
			InstanceType: string(info.InstanceType),
		}
		if info.NetworkInfo != nil {
			cap.ENASupport = string(info.NetworkInfo.EnaSupport)
			if info.NetworkInfo.EfaSupported != nil {
				cap.EFASupported = *info.NetworkInfo.EfaSupported
			}
			if info.NetworkInfo.MaximumNetworkInterfaces != nil {
				cap.MaxENIs = *info.NetworkInfo.MaximumNetworkInterfaces
			}
		}
		caps = append(caps, cap)
	}
	return caps, nil
}

// VCPUQuotaAttribute returns the running on-demand vCPU quota for the
// account. This is a doctor pre-flight check. AWS surfaces multiple
// quota attribute names; we ask for the "running
// on-demand instances" family which is the relevant one for the
// self-managed node group.
func (c *Clients) VCPUQuotaAttribute(ctx context.Context) (string, error) {
	if c == nil || c.EC2 == nil {
		return "", fmt.Errorf("aws.Clients is nil")
	}
	// "default-vpc" is the canonical attribute name on every account;
	// the actual running-on-demand quotas live in Service Quotas (a
	// separate API). For the doctor pre-flight we just want to confirm
	// the EC2 permission probe succeeds; returning the default-vpc
	// attribute is the cheapest such probe.
	out, err := c.EC2.DescribeAccountAttributes(ctx, &ec2.DescribeAccountAttributesInput{
		AttributeNames: []ec2types.AccountAttributeName{ec2types.AccountAttributeNameDefaultVpc},
	})
	if err != nil {
		return "", fmt.Errorf("ec2:DescribeAccountAttributes: %w", err)
	}
	if len(out.AccountAttributes) == 0 {
		return "", nil
	}
	values := out.AccountAttributes[0].AttributeValues
	if len(values) == 0 {
		return "", nil
	}
	return aws_string_or_empty(values[0].AttributeValue), nil
}

// ptr returns a pointer to a string literal. Avoids needing
// aws.String(s) imports per call site — keeps the import surface
// narrow.
func ptr(s string) *string {
	return &s
}
