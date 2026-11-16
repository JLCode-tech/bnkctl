// Package providers resolves cloud/platform-specific provider.Provider implementations
// for AWS, Azure, GCP, OpenShift, On-Prem, Bare-Metal, and local clusters.
package providers

import (
	"context"
	"fmt"
	"strings"

	"github.com/JLCode-tech/bnkctl/internal/providers/aws"
	"github.com/JLCode-tech/bnkctl/internal/providers/azure"
	"github.com/JLCode-tech/bnkctl/internal/providers/gcp"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/jumphost"
	"github.com/JLCode-tech/bnkctl/pkg/provider"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

type genericProvider struct {
	name string
}

func (g *genericProvider) Name() string { return g.name }

func (g *genericProvider) ProvisionUnderlay(_ context.Context, cl *intent.Cluster, st *state.State, _ bool) (*provider.UnderlayContext, error) {
	return g.DiscoverUnderlay(context.Background(), cl, st)
}

func (g *genericProvider) DiscoverUnderlay(_ context.Context, cl *intent.Cluster, st *state.State) (*provider.UnderlayContext, error) {
	u := &provider.UnderlayContext{
		CloudName:       g.name,
		DataplaneDriver: provider.DriverSocket,
		Traits:          provider.DefaultTraitsForUnderlay(g.name, provider.DriverSocket),
	}
	if cl != nil {
		u.ClusterName = cl.EffectiveName()
		u.Region = cl.EffectiveRegion()
	}
	provider.HydrateUnderlayFromState(u, st)
	return u, nil
}

func (g *genericProvider) DestroyUnderlay(_ context.Context, _ *intent.Cluster, _ *state.State, _ bool) error {
	return nil
}

func (g *genericProvider) OpenJumphostSession(_ context.Context, _ *intent.Cluster, _ *state.State) (jumphost.Runner, error) {
	return nil, fmt.Errorf("jumphost is not supported for %s provider", g.name)
}

// ForCloud returns the provider.Provider implementation for the given cloud or platform identifier.
func ForCloud(cloud string) (provider.Provider, error) {
	switch strings.ToLower(strings.TrimSpace(cloud)) {
	case intent.ProviderAWS, "eks", "cloud-aws":
		return aws.NewProvider(), nil
	case intent.ProviderAzure, "azr", "aks", "cloud-azure":
		return azure.NewProvider(), nil
	case intent.ProviderGCP, "gke", "cloud-gcp":
		return gcp.NewProvider(), nil
	case intent.ProviderOpenShift, "ocp", "crc", "redhat":
		return &genericProvider{name: intent.ProviderOpenShift}, nil
	case intent.ProviderOnPrem, "on-prem", "on_prem":
		return &genericProvider{name: intent.ProviderOnPrem}, nil
	case intent.ProviderBareMetal, "bare-metal", "bare_metal":
		return &genericProvider{name: intent.ProviderBareMetal}, nil
	case intent.ProviderKind, "k3s", "minikube", "docker-desktop":
		return &genericProvider{name: intent.ProviderKind}, nil
	default:
		return nil, fmt.Errorf("unsupported cloud provider %q (expected aws, azure, gcp, openshift, onprem, baremetal, or kind)", cloud)
	}
}

// Resolve determines the active cloud provider using explicitCloud (if non-empty)
// or the cluster intent's EffectiveProvider() (falling back to "aws").
func Resolve(explicitCloud string, cl *intent.Cluster) (provider.Provider, error) {
	cloud := strings.TrimSpace(explicitCloud)
	if cloud == "" && cl != nil {
		cloud = cl.EffectiveProvider()
	}
	if cloud == "" {
		cloud = intent.ProviderAWS
	}
	return ForCloud(cloud)
}
