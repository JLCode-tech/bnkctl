package providers

import (
	"context"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

func TestResolveAndUnderlayLifecycle(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		cloud    string
		expected string
		cl       *intent.Cluster
	}{
		{
			cloud:    "aws",
			expected: intent.ProviderAWS,
			cl: &intent.Cluster{
				Cloud:    &intent.CloudSpec{Provider: intent.ProviderAWS},
				Metadata: intent.Metadata{Name: "aws-test", Region: "ap-southeast-2"},
				Network:  intent.Network{VPCCidr: "10.90.0.0/16"},
			},
		},
		{
			cloud:    "azure",
			expected: intent.ProviderAzure,
			cl: &intent.Cluster{
				Cloud:    &intent.CloudSpec{Provider: intent.ProviderAzure},
				Metadata: intent.Metadata{Name: "azr-test", Location: "eastus2", ResourceGroup: "rg-test"},
				Network:  intent.Network{VNetCIDR: "10.90.0.0/16"},
			},
		},
		{
			cloud:    "gcp",
			expected: intent.ProviderGCP,
			cl: &intent.Cluster{
				Cloud:    &intent.CloudSpec{Provider: intent.ProviderGCP},
				Metadata: intent.Metadata{Name: "gcp-test", Region: "us-central1", Project: "proj-test", Zone: "us-central1-a"},
				Network:  intent.Network{Main: intent.MainNetworkSpec{CIDR: "10.90.1.0/24"}},
			},
		},
	} {
		t.Run(tc.cloud, func(t *testing.T) {
			p, err := Resolve(tc.cloud, tc.cl)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tc.cloud, err)
			}
			if p.Name() != tc.expected {
				t.Fatalf("p.Name() = %q, want %q", p.Name(), tc.expected)
			}

			st, err := state.Load(t.TempDir())
			if err != nil {
				t.Fatalf("state.Load: %v", err)
			}
			u, err := p.ProvisionUnderlay(ctx, tc.cl, st, true)
			if err != nil {
				t.Fatalf("ProvisionUnderlay: %v", err)
			}
			if u.CloudName != tc.expected {
				t.Fatalf("u.CloudName = %q, want %q", u.CloudName, tc.expected)
			}
			if len(u.Networks) == 0 {
				t.Fatalf("expected non-empty u.Networks")
			}

			u2, err := p.DiscoverUnderlay(ctx, tc.cl, st)
			if err != nil {
				t.Fatalf("DiscoverUnderlay: %v", err)
			}
			if u2.CloudName != tc.expected {
				t.Fatalf("u2.CloudName = %q, want %q", u2.CloudName, tc.expected)
			}

			if err := p.DestroyUnderlay(ctx, tc.cl, st, true); err != nil {
				t.Fatalf("DestroyUnderlay: %v", err)
			}
		})
	}
}
