package aws

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
)

// ServiceQuotasAPI is the subset of servicequotas.Client awsbnkctl uses
// for the optional doctor probe. The doctor's VCPUQuotaAttribute check
// probes ec2:DescribeAccountAttributes for the
// cheap "ec2 permissions work" signal; this richer probe surfaces the
// actual running-on-demand-vCPU quota when the operator's IAM permits
// the servicequotas:GetServiceQuota call.
//
// Tests inject a fake; production code constructs the real client via
// servicequotas.NewFromConfig in EnsureServiceQuotas.
type ServiceQuotasAPI interface {
	GetServiceQuota(ctx context.Context, in *servicequotas.GetServiceQuotaInput, opts ...func(*servicequotas.Options)) (*servicequotas.GetServiceQuotaOutput, error)
}

// QuotaCodeRunningOnDemandStandardInstances is the AWS Service Quotas
// quota code for "Running On-Demand Standard (A, C, D, H, I, M, R, T,
// Z) instances" — the per-account vCPU ceiling enforced against the
// self-managed EKS node group. Documented at
// https://docs.aws.amazon.com/general/latest/gr/ec2-service.html
// §"Service quotas".
const QuotaCodeRunningOnDemandStandardInstances = "L-1216C47A"

// QuotaServiceCodeEC2 is the Service Quotas `ServiceCode` for EC2.
const QuotaServiceCodeEC2 = "ec2"

// SetServiceQuotasForTest lets tests inject a fake ServiceQuotasAPI
// without exposing the (private) field name.
func (c *Clients) SetServiceQuotasForTest(api ServiceQuotasAPI) { c.serviceQuotas = api }

// EnsureServiceQuotas constructs a real servicequotas.Client off the
// resolved aws.Config and caches it on Clients. Lazy by design: only
// the doctor's optional Service Quotas probe touches this surface, so
// every other verb avoids the ~50 KB of import surface at no runtime
// cost.
//
// Idempotent — returns the cached client on subsequent calls.
func (c *Clients) EnsureServiceQuotas() (ServiceQuotasAPI, error) {
	if c == nil {
		return nil, fmt.Errorf("aws.Clients is nil")
	}
	if c.serviceQuotas != nil {
		return c.serviceQuotas, nil
	}
	if c.AWSConfig.Region == "" && c.Region == "" {
		return nil, errors.New("aws.Clients region is empty; cannot construct ServiceQuotas client")
	}
	c.serviceQuotas = servicequotas.NewFromConfig(c.AWSConfig)
	return c.serviceQuotas, nil
}

// RunningOnDemandVCPUQuota returns the live running-on-demand standard
// instance vCPU quota for the account in the client's region. Returns
// the numeric value (e.g. 80) on success, or an error the doctor maps
// to a Warning row pointing the operator at the IAM gap.
//
// AccessDenied is the common-case failure — the operator's IAM doesn't
// attach servicequotas:GetServiceQuota. The doctor falls back to the
// existing "default 5 instances / 80 vCPU" pointer in that case.
//
// Gated by an env-var feature flag (off by default) until v0.x
// validates the signal on a live AWS account.
func (c *Clients) RunningOnDemandVCPUQuota(ctx context.Context) (float64, error) {
	cli, err := c.EnsureServiceQuotas()
	if err != nil {
		return 0, err
	}
	serviceCode := QuotaServiceCodeEC2
	quotaCode := QuotaCodeRunningOnDemandStandardInstances
	out, err := cli.GetServiceQuota(ctx, &servicequotas.GetServiceQuotaInput{
		ServiceCode: &serviceCode,
		QuotaCode:   &quotaCode,
	})
	if err != nil {
		return 0, fmt.Errorf("servicequotas:GetServiceQuota %s/%s: %w", serviceCode, quotaCode, err)
	}
	if out.Quota == nil || out.Quota.Value == nil {
		return 0, fmt.Errorf("servicequotas:GetServiceQuota %s/%s returned nil quota value", serviceCode, quotaCode)
	}
	return *out.Quota.Value, nil
}
