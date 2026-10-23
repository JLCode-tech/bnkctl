package forge

// mcptarget.go — register the MCP tool servers a BNK 2.4 cluster exposes as
// forge BenchmarkTargets, so the Target Catalog lists them next to the LLM
// endpoints. Forge has no MCP-specific record; the target's llm_* columns hold
// the endpoint and the tags carry the MCP metadata (route, gateway, tools,
// persistence, auth). Idempotent through RegisterBenchmarkTarget.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/JLCode-tech/bnkctl/pkg/k8s/bnkscan"
)

// MCPTargetOptions carries the forge connection for MCP target registration.
type MCPTargetOptions struct {
	RestURL string
	Creds   RestCreds
	// ClusterID is the forge cluster the endpoints belong to (required).
	ClusterID int
	// ClusterName is recorded in the tags for Fleet filtering.
	ClusterName string
	// ProxyNamespace is where TMM runs (default f5-cne-system).
	ProxyNamespace string
}

// MCPTargetResult is the outcome for one endpoint.
type MCPTargetResult struct {
	Endpoint bnkscan.MCPEndpoint
	Target   BenchmarkTargetResponse
	Err      error
}

// maxToolSchemaTag bounds the tool_schemas tag so a large catalogue cannot
// blow up the forge row.
const maxToolSchemaTag = 8 * 1024

// MCPTargetTags renders the tag map forge stores for an MCP endpoint.
func MCPTargetTags(ep bnkscan.MCPEndpoint, clusterName string) map[string]string {
	tags := map[string]string{
		"source":         "azrbnkctl",
		"cloud_provider": "azure",
		"protocol":       "mcp",
		"route":          ep.Namespace + "/" + ep.Route,
		"gateway":        ep.GatewayNamespace + "/" + ep.Gateway,
		"auth":           ep.Auth,
		"discovery":      ep.Reason,
	}
	if clusterName != "" {
		tags["cluster"] = clusterName
	}
	put := func(k string, v []string) {
		if len(v) > 0 {
			tags[k] = strings.Join(v, ",")
		}
	}
	put("hostnames", ep.Hostnames)
	put("paths", ep.Paths)
	put("urls", ep.URLs)
	put("backends", ep.Backends)
	put("irules", ep.IRules)
	put("sec_policies", ep.SecPolicies)
	put("listeners", ep.Listeners)
	if p := ep.Persistence; p != nil {
		tags["persistence_profile"] = p.Profile
		tags["persistence_type"] = p.Type
		if p.Timeout > 0 {
			tags["persistence_timeout_s"] = fmt.Sprint(p.Timeout)
		}
		if p.Listener != "" {
			tags["persistence_listener"] = p.Listener
		}
	}
	if names := ep.ToolNames(); len(names) > 0 {
		tags["tools"] = strings.Join(names, ",")
		sorted := append([]bnkscan.ToolDef(nil), ep.Tools...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
		if b, err := json.Marshal(sorted); err == nil && len(b) <= maxToolSchemaTag {
			tags["tool_schemas"] = string(b)
		}
	}
	return tags
}

// RegisterMCPTarget creates or reuses the BenchmarkTarget for one endpoint.
func RegisterMCPTarget(ctx context.Context, opts MCPTargetOptions, ep bnkscan.MCPEndpoint) (BenchmarkTargetResponse, error) {
	if opts.ClusterID == 0 {
		return BenchmarkTargetResponse{}, ErrTargetNoClusterID
	}
	url := ep.PrimaryURL()
	if url == "" {
		return BenchmarkTargetResponse{}, fmt.Errorf("endpoint %s has no URL (Gateway %s/%s has no address or HTTP listener)", ep.Name(), ep.GatewayNamespace, ep.Gateway)
	}
	ns, svc := "", ""
	if len(ep.Backends) > 0 {
		ns, svc = splitBackend(ep.Backends[0])
	}
	proxyNS := opts.ProxyNamespace
	if proxyNS == "" {
		proxyNS = bnkscan.DefaultControllerNamespace
	}
	return RegisterBenchmarkTarget(ctx, BenchmarkTargetOptions{
		RestURL:        opts.RestURL,
		Creds:          opts.Creds,
		Name:           ep.Name(),
		ClusterID:      opts.ClusterID,
		ClusterName:    opts.ClusterName,
		LLMBaseURL:     url,
		LLMModel:       "mcp:" + ep.Route,
		LLMNamespace:   ns,
		LLMEndpoint:    svc,
		ProxyNamespace: proxyNS,
		Tags:           MCPTargetTags(ep, opts.ClusterName),
	})
}

// RegisterMCPTargets registers every endpoint, best-effort: one failure does
// not stop the others. Results keep the input order.
func RegisterMCPTargets(ctx context.Context, opts MCPTargetOptions, eps []bnkscan.MCPEndpoint) []MCPTargetResult {
	out := make([]MCPTargetResult, 0, len(eps))
	for _, ep := range eps {
		t, err := RegisterMCPTarget(ctx, opts, ep)
		out = append(out, MCPTargetResult{Endpoint: ep, Target: t, Err: err})
	}
	return out
}

// splitBackend turns "namespace/service:port" into its namespace and service.
func splitBackend(b string) (ns, svc string) {
	if i := strings.IndexByte(b, ':'); i >= 0 {
		b = b[:i]
	}
	if i := strings.IndexByte(b, '/'); i >= 0 {
		return b[:i], b[i+1:]
	}
	return "", b
}
