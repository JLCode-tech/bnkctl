package aws

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
	sqtypes "github.com/aws/aws-sdk-go-v2/service/servicequotas/types"
)

// fakeServiceQuotas implements ServiceQuotasAPI for unit tests. Captures
// the last GetServiceQuota input so assertions can pin the quota code +
// service code on the wire.
type fakeServiceQuotas struct {
	out  *servicequotas.GetServiceQuotaOutput
	err  error
	last *servicequotas.GetServiceQuotaInput
}

func (f *fakeServiceQuotas) GetServiceQuota(_ context.Context, in *servicequotas.GetServiceQuotaInput, _ ...func(*servicequotas.Options)) (*servicequotas.GetServiceQuotaOutput, error) {
	f.last = in
	return f.out, f.err
}

// TestRunningOnDemandVCPUQuota_HappyPath pins the wire shape: probes
// ec2 / L-1216C47A, returns the SDK's float value to the caller.
func TestRunningOnDemandVCPUQuota_HappyPath(t *testing.T) {
	val := 256.0
	fake := &fakeServiceQuotas{out: &servicequotas.GetServiceQuotaOutput{
		Quota: &sqtypes.ServiceQuota{Value: &val},
	}}
	c := &Clients{}
	c.SetServiceQuotasForTest(fake)

	got, err := c.RunningOnDemandVCPUQuota(context.Background())
	if err != nil {
		t.Fatalf("RunningOnDemandVCPUQuota: %v", err)
	}
	if got != 256 {
		t.Errorf("vCPU quota: got %v, want 256", got)
	}
	if fake.last == nil {
		t.Fatal("fake.last is nil — GetServiceQuota not called")
	}
	if fake.last.ServiceCode == nil || *fake.last.ServiceCode != QuotaServiceCodeEC2 {
		t.Errorf("ServiceCode: got %v, want %q", strOrNil(fake.last.ServiceCode), QuotaServiceCodeEC2)
	}
	if fake.last.QuotaCode == nil || *fake.last.QuotaCode != QuotaCodeRunningOnDemandStandardInstances {
		t.Errorf("QuotaCode: got %v, want %q", strOrNil(fake.last.QuotaCode), QuotaCodeRunningOnDemandStandardInstances)
	}
}

// TestRunningOnDemandVCPUQuota_AccessDeniedSurfaces pins the
// AccessDenied path — caller (doctor) maps it to a Warning row.
func TestRunningOnDemandVCPUQuota_AccessDeniedSurfaces(t *testing.T) {
	fake := &fakeServiceQuotas{err: errors.New("AccessDeniedException: User is not authorized to perform servicequotas:GetServiceQuota")}
	c := &Clients{}
	c.SetServiceQuotasForTest(fake)

	_, err := c.RunningOnDemandVCPUQuota(context.Background())
	if err == nil {
		t.Fatal("expected an error on AccessDenied, got nil")
	}
}

// TestRunningOnDemandVCPUQuota_NilQuotaSurfacesError guards against a
// (theoretical) SDK response with no Quota.Value. Without the guard the
// caller would deref nil; the helper returns an actionable error
// instead.
func TestRunningOnDemandVCPUQuota_NilQuotaSurfacesError(t *testing.T) {
	fake := &fakeServiceQuotas{out: &servicequotas.GetServiceQuotaOutput{Quota: nil}}
	c := &Clients{}
	c.SetServiceQuotasForTest(fake)

	if _, err := c.RunningOnDemandVCPUQuota(context.Background()); err == nil {
		t.Fatal("expected an error on nil Quota, got nil")
	}
}

// strOrNil is a tiny test helper so the assertion above prints "(nil)"
// instead of a panicky deref when the wire field is unset.
func strOrNil(p *string) string {
	if p == nil {
		return "(nil)"
	}
	return *p
}
