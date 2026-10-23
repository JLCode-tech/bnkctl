package forge

import (
	"context"
	"fmt"
	"strings"
)

// ValidProxyTypes is the canonical set of proxy type strings accepted by forge.
var ValidProxyTypes = map[string]bool{
	"envoy":    true,
	"nginx":    true,
	"haproxy":  true,
	"f5-bnk":   true,
	"nodeport": true,
}

// IsValidProxyType reports whether s is a forge-recognised proxy type.
func IsValidProxyType(s string) bool {
	return ValidProxyTypes[s]
}

// BenchmarkProxyEndpoint is the forge REST path segment for target proxy deployments.
const BenchmarkProxyEndpoint = "/api/benchmarks/targets"

// ProxyDiscoverOptions carries parameters for the discover-proxies call.
type ProxyDiscoverOptions struct {
	RestURL  string
	Creds    RestCreds
	TargetID int
}

// ProxyDiscoveryResultItem is one entry in the discovery response results list.
type ProxyDiscoveryResultItem struct {
	ProxyType string `json:"proxy_type"`
	Found     bool   `json:"found"`
}

// ProxyDiscoveryResult is the response from POST discover-proxies.
type ProxyDiscoveryResult struct {
	TargetID        int                        `json:"target_id"`
	DiscoveredCount int                        `json:"discovered_count"`
	TotalScanned    int                        `json:"total_scanned"`
	Results         []ProxyDiscoveryResultItem `json:"results"`
}

// ProxyDeployment is a single record from GET /proxies.
type ProxyDeployment struct {
	ID          int    `json:"id"`
	TargetID    int    `json:"target_id"`
	ProxyType   string `json:"proxy_type"`
	Status      string `json:"status"`
	ProxyURL    string `json:"proxy_url"`
	ExternalURL string `json:"external_url"`
}

// FindProxyDeployment returns a pointer to the ProxyDeployment whose proxy_type matches proxyType.
func FindProxyDeployment(proxies []ProxyDeployment, proxyType string) *ProxyDeployment {
	for i := range proxies {
		if proxies[i].ProxyType == proxyType {
			return &proxies[i]
		}
	}
	return nil
}

// DiscoverProxies calls POST /api/benchmarks/targets/{target_id}/discover-proxies.
func DiscoverProxies(ctx context.Context, opts ProxyDiscoverOptions) (ProxyDiscoveryResult, error) {
	if opts.RestURL == "" {
		return ProxyDiscoveryResult{}, fmt.Errorf("forge.DiscoverProxies: RestURL is required")
	}
	if opts.TargetID == 0 {
		return ProxyDiscoveryResult{}, fmt.Errorf("forge.DiscoverProxies: TargetID is required")
	}

	base := strings.TrimRight(opts.RestURL, "/")
	token, err := restLogin(ctx, base, opts.Creds.restUsername(), opts.Creds.restPassword())
	if err != nil {
		return ProxyDiscoveryResult{}, fmt.Errorf("forge discover proxies: login: %w", err)
	}

	url := fmt.Sprintf("%s%s/%d/discover-proxies", base, BenchmarkProxyEndpoint, opts.TargetID)
	var result ProxyDiscoveryResult
	if err := restPost(ctx, url, token, map[string]any{}, &result); err != nil {
		return ProxyDiscoveryResult{}, fmt.Errorf("forge discover proxies: %w", err)
	}
	return result, nil
}

// ListProxyDeployments calls GET /api/benchmarks/targets/{target_id}/proxies.
func ListProxyDeployments(ctx context.Context, opts ProxyDiscoverOptions) ([]ProxyDeployment, error) {
	if opts.RestURL == "" {
		return nil, fmt.Errorf("forge.ListProxyDeployments: RestURL is required")
	}
	if opts.TargetID == 0 {
		return nil, fmt.Errorf("forge.ListProxyDeployments: TargetID is required")
	}

	base := strings.TrimRight(opts.RestURL, "/")
	token, err := restLogin(ctx, base, opts.Creds.restUsername(), opts.Creds.restPassword())
	if err != nil {
		return nil, fmt.Errorf("forge list proxy deployments: login: %w", err)
	}

	url := fmt.Sprintf("%s%s/%d/proxies", base, BenchmarkProxyEndpoint, opts.TargetID)
	var list []ProxyDeployment
	if err := restGet(ctx, url, token, &list); err != nil {
		return nil, fmt.Errorf("forge list proxy deployments: %w", err)
	}
	return list, nil
}

// ResolveProxyDeploymentID returns the ID of the ProxyDeployment matching proxyType.
func ResolveProxyDeploymentID(proxies []ProxyDeployment, proxyType string) int {
	for _, p := range proxies {
		if p.ProxyType == proxyType {
			return p.ID
		}
	}
	return 0
}
