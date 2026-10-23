// Package providers resolves cloud-specific provider.Provider implementations
// for AWS, Azure, and GCP.
package providers

import (
	"fmt"
	"strings"

	"github.com/JLCode-tech/bnkctl/internal/providers/aws"
	"github.com/JLCode-tech/bnkctl/internal/providers/azure"
	"github.com/JLCode-tech/bnkctl/internal/providers/gcp"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/provider"
)

// ForCloud returns the provider.Provider implementation for the given cloud identifier.
func ForCloud(cloud string) (provider.Provider, error) {
	switch strings.ToLower(strings.TrimSpace(cloud)) {
	case intent.ProviderAWS, "eks":
		return aws.NewProvider(), nil
	case intent.ProviderAzure, "azr", "aks":
		return azure.NewProvider(), nil
	case intent.ProviderGCP, "gke":
		return gcp.NewProvider(), nil
	default:
		return nil, fmt.Errorf("unsupported cloud provider %q (expected aws, azure, or gcp)", cloud)
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
