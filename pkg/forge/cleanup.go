package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const benchmarkArtifactPrefix = "azrbnkctl-"

// ProxyDeploymentResponse is the subset of a forge ProxyDeployment record needed.
type ProxyDeploymentResponse struct {
	ID int `json:"id"`
}

// ListTargetsByClusterID returns all BenchmarkTarget records whose cluster_id matches clusterID.
func ListTargetsByClusterID(ctx context.Context, restURL string, creds RestCreds, clusterID int) ([]BenchmarkTargetResponse, error) {
	base := strings.TrimRight(restURL, "/")
	token, err := restLogin(ctx, base, creds.restUsername(), creds.restPassword())
	if err != nil {
		return nil, fmt.Errorf("forge.ListTargetsByClusterID: login: %w", err)
	}
	url := base + BenchmarkTargetEndpoint

	var list []BenchmarkTargetResponse
	if err := restGet(ctx, url, token, &list); err != nil {
		return nil, fmt.Errorf("forge.ListTargetsByClusterID: %w", err)
	}

	if len(list) == 0 {
		var wrapped struct {
			Targets []BenchmarkTargetResponse `json:"targets"`
		}
		_ = restGet(ctx, url, token, &wrapped)
		if len(wrapped.Targets) > 0 {
			list = wrapped.Targets
		}
	}

	var out []BenchmarkTargetResponse
	for _, t := range list {
		if t.ClusterID == clusterID {
			out = append(out, t)
		}
	}
	return out, nil
}

// ListProxiesForTarget returns the ProxyDeployment records for a target.
func ListProxiesForTarget(ctx context.Context, restURL string, creds RestCreds, targetID int) ([]ProxyDeploymentResponse, error) {
	base := strings.TrimRight(restURL, "/")
	token, err := restLogin(ctx, base, creds.restUsername(), creds.restPassword())
	if err != nil {
		return nil, fmt.Errorf("forge.ListProxiesForTarget: login: %w", err)
	}
	url := fmt.Sprintf("%s%s/%d/proxies", base, BenchmarkTargetEndpoint, targetID)

	var list []ProxyDeploymentResponse
	if err := restGet(ctx, url, token, &list); err != nil {
		return nil, fmt.Errorf("forge.ListProxiesForTarget: %w", err)
	}
	return list, nil
}

// DeleteProxyDeployment deletes a single ProxyDeployment record by ID.
func DeleteProxyDeployment(ctx context.Context, restURL string, creds RestCreds, proxyID int) error {
	base := strings.TrimRight(restURL, "/")
	token, err := restLogin(ctx, base, creds.restUsername(), creds.restPassword())
	if err != nil {
		return fmt.Errorf("forge.DeleteProxyDeployment: login: %w", err)
	}
	url := fmt.Sprintf("%s/api/benchmarks/proxies/%d", base, proxyID)
	if err := restDelete(ctx, url, token); err != nil {
		var herr *restHTTPErr
		if errors.As(err, &herr) && herr.StatusCode == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("delete proxy %d: %w", proxyID, err)
	}
	return nil
}

// DeleteBenchmarkTarget deletes a single BenchmarkTarget record by ID.
func DeleteBenchmarkTarget(ctx context.Context, restURL string, creds RestCreds, targetID int) error {
	base := strings.TrimRight(restURL, "/")
	token, err := restLogin(ctx, base, creds.restUsername(), creds.restPassword())
	if err != nil {
		return fmt.Errorf("forge.DeleteBenchmarkTarget: login: %w", err)
	}
	url := fmt.Sprintf("%s%s/%d", base, BenchmarkTargetEndpoint, targetID)
	if err := restDelete(ctx, url, token); err != nil {
		var herr *restHTTPErr
		if errors.As(err, &herr) && herr.StatusCode == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("delete target %d: %w", targetID, err)
	}
	return nil
}

// DeleteBenchmarkAgentByName deletes a BenchmarkAgent record matching name.
func DeleteBenchmarkAgentByName(ctx context.Context, restURL string, creds RestCreds, name string) error {
	base := strings.TrimRight(restURL, "/")
	token, err := restLogin(ctx, base, creds.restUsername(), creds.restPassword())
	if err != nil {
		return fmt.Errorf("forge.DeleteBenchmarkAgentByName: login: %w", err)
	}

	var list []BenchmarkAgentResponse
	if err := restGet(ctx, base+BenchmarkAgentEndpoint, token, &list); err != nil {
		return fmt.Errorf("list agents: %w", err)
	}
	for _, a := range list {
		if a.Name == name {
			url := fmt.Sprintf("%s%s/%d", base, BenchmarkAgentEndpoint, a.ID)
			if err := restDelete(ctx, url, token); err != nil {
				var herr *restHTTPErr
				if errors.As(err, &herr) && herr.StatusCode == http.StatusNotFound {
					return nil
				}
				return fmt.Errorf("delete agent %d (%s): %w", a.ID, name, err)
			}
			return nil
		}
	}
	return nil
}

// DeleteClusterBenchmarkArtifacts deletes cluster-scoped benchmark artifacts from forge.
func DeleteClusterBenchmarkArtifacts(ctx context.Context, restURL string, creds RestCreds, clusterID int) error {
	targets, err := ListTargetsByClusterID(ctx, restURL, creds, clusterID)
	if err != nil {
		return fmt.Errorf("list targets: %w", err)
	}

	var errs []string
	for _, t := range targets {
		proxies, perr := ListProxiesForTarget(ctx, restURL, creds, t.ID)
		if perr != nil {
			errs = append(errs, fmt.Sprintf("list proxies for target %d: %v", t.ID, perr))
		} else {
			for _, p := range proxies {
				if derr := DeleteProxyDeployment(ctx, restURL, creds, p.ID); derr != nil {
					errs = append(errs, fmt.Sprintf("delete proxy %d: %v", p.ID, derr))
				}
			}
		}

		if derr := DeleteBenchmarkTarget(ctx, restURL, creds, t.ID); derr != nil {
			errs = append(errs, fmt.Sprintf("delete target %d: %v", t.ID, derr))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("benchmark cleanup errors (%d):\n  • %s", len(errs), strings.Join(errs, "\n  • "))
	}
	return nil
}

// DeleteAllClusterBenchmarkArtifacts purges all azrbnkctl-prefixed benchmark records.
func DeleteAllClusterBenchmarkArtifacts(ctx context.Context, restURL string, creds RestCreds, clusterID int) error {
	targetErr := DeleteClusterBenchmarkArtifacts(ctx, restURL, creds, clusterID)

	base := strings.TrimRight(restURL, "/")
	token, err := restLogin(ctx, base, creds.restUsername(), creds.restPassword())
	if err != nil {
		if targetErr != nil {
			return targetErr
		}
		return fmt.Errorf("login: %w", err)
	}

	var agents []BenchmarkAgentResponse
	if err := restGet(ctx, base+BenchmarkAgentEndpoint, token, &agents); err == nil {
		for _, a := range agents {
			if strings.HasPrefix(a.Name, benchmarkArtifactPrefix) ||
				strings.HasPrefix(a.Name, "awsbnkctl-") ||
				strings.HasPrefix(a.Name, "gkebnkctl-") ||
				strings.HasPrefix(a.Name, "bnkctl-") {
				_ = DeleteBenchmarkAgentByName(ctx, restURL, creds, a.Name)
			}
		}
	}

	return targetErr
}
