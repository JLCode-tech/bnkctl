package forge

// target.go — register a benchmark target (the LLM endpoint behind BNK) with
// forge via POST /api/benchmarks/targets.
//
// Forge's target endpoint does NOT support upsert.  On conflict, fall back to
// GET /api/benchmarks/targets and match by name.  Mirrors the accessmethod.go
// and agent.go patterns.
//
// cluster_id is required by the forge schema.  When unavailable (e.g. the
// workspace has no forge_link.json), the caller should skip target registration
// gracefully (best-effort, non-fatal).
//
// Uses the same benchmarkHTTPDoFn transport seam as PushBenchmarkResult.

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// BenchmarkTargetEndpoint is the forge REST path for benchmark targets.
const BenchmarkTargetEndpoint = "/api/benchmarks/targets"

// BenchmarkDiscoverTargetsEndpoint is the forge REST path for cluster target discovery.
const BenchmarkDiscoverTargetsEndpoint = "/api/benchmarks/discover-targets"

// BenchmarkTargetOptions carries all data for registering a benchmark target.
type BenchmarkTargetOptions struct {
	// RestURL is the forge REST base URL (e.g. "http://localhost:8000").
	RestURL string
	// Creds are the forge REST login credentials.
	Creds RestCreds
	// Name is the target name as stored in forge (e.g. "ai-rig-llama3").
	Name string
	// ClusterID is the forge cluster FK (required by schema).
	// When zero, registration is skipped — callers receive ErrTargetNoClusterID.
	ClusterID int
	// ClusterName disambiguates Name when Forge already holds a target of
	// that name for another cluster (Forge keys target names globally): the
	// target is then created as "<Name>-<ClusterName>". Optional; the
	// cluster ID is used when empty.
	ClusterName string
	// LLMBaseURL is the HTTP base URL of the LLM endpoint (e.g. "http://10.0.10.100").
	LLMBaseURL string
	// LLMModel is the model name served by the endpoint (e.g. "meta-llama/Llama-3.1-8B-Instruct").
	LLMModel string
	// LLMNamespace is the k8s namespace where the inference pod runs.
	LLMNamespace string
	// LLMEndpoint is the service/endpoint name in that namespace.
	LLMEndpoint string
	// ProxyNamespace is the k8s namespace where the BNK proxy runs.
	ProxyNamespace string
	// Tags is an optional free-form tag map.
	Tags map[string]string
}

// BenchmarkTargetResponse is the representation of forge's BenchmarkTarget fields.
type BenchmarkTargetResponse struct {
	ID             int            `json:"id"`
	Name           string         `json:"name"`
	ClusterID      int            `json:"cluster_id"`
	LLMBaseURL     string         `json:"llm_base_url,omitempty"`
	LLMModel       string         `json:"llm_model,omitempty"`
	LLMNamespace   string         `json:"llm_namespace,omitempty"`
	LLMEndpoint    string         `json:"llm_endpoint,omitempty"`
	ProxyNamespace string         `json:"proxy_namespace,omitempty"`
	Status         string         `json:"status,omitempty"`
	ProxyCount     int            `json:"proxy_count,omitempty"`
	Tags           map[string]any `json:"tags,omitempty"`
}

// DiscoverTargetsOptions carries parameters for POST /api/benchmarks/discover-targets.
type DiscoverTargetsOptions struct {
	// RestURL is the forge REST base URL (e.g. "http://localhost:8000").
	RestURL string
	// Creds are the forge REST login credentials.
	Creds RestCreds
	// ClusterID is the forge cluster ID to scan.
	ClusterID int
	// AutoCreate automatically creates BenchmarkTarget records for discovered services.
	AutoCreate bool
	// SelectedServices is an optional list of service base_urls to restrict target creation to.
	SelectedServices []string
}

// DiscoveredServiceItem describes an LLM service discovered during a cluster scan.
type DiscoveredServiceItem struct {
	ServiceName string `json:"service_name"`
	Namespace   string `json:"namespace"`
	BaseURL     string `json:"base_url"`
	Port        int    `json:"port"`
	Confidence  string `json:"confidence"`
	Reason      string `json:"reason"`
	ModelHint   string `json:"model_hint,omitempty"`
	GPUCount    int    `json:"gpu_count"`
	Image       string `json:"image,omitempty"`
}

// CreatedTargetItem describes a BenchmarkTarget auto-created by forge discovery.
type CreatedTargetItem struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	LLMBaseURL string `json:"llm_base_url"`
	Status     string `json:"status"`
}

// TargetProxyDiscoveryResult describes proxy discovery outcome for an auto-created target.
type TargetProxyDiscoveryResult struct {
	TargetID          int    `json:"target_id"`
	TargetName        string `json:"target_name"`
	DiscoveredProxies int    `json:"discovered_proxies"`
	TotalScanned      int    `json:"total_scanned"`
	Error             string `json:"error,omitempty"`
}

// DiscoverTargetsResponse is returned by POST /api/benchmarks/discover-targets.
type DiscoverTargetsResponse struct {
	ClusterID          int                          `json:"cluster_id"`
	ClusterName        string                       `json:"cluster_name"`
	DiscoveredServices []DiscoveredServiceItem      `json:"discovered_services"`
	DiscoveredCount    int                          `json:"discovered_count"`
	CreatedTargets     []CreatedTargetItem          `json:"created_targets"`
	ProxyResults       []TargetProxyDiscoveryResult `json:"proxy_results"`
}

// ListBenchmarkTargetsOptions carries parameters for GET /api/benchmarks/targets.
type ListBenchmarkTargetsOptions struct {
	// RestURL is the forge REST base URL.
	RestURL string
	// Creds are the forge REST login credentials.
	Creds RestCreds
	// ClusterID optionally filters targets by forge cluster ID.
	ClusterID int
}

// ErrTargetNoClusterID is returned by RegisterBenchmarkTarget when ClusterID
// is zero.  Callers should treat this as a soft skip, not a hard failure.
var ErrTargetNoClusterID = errors.New("forge.RegisterBenchmarkTarget: ClusterID is required (workspace may not have a forge link)")

// DiscoverTargets calls POST /api/benchmarks/discover-targets to scan the cluster
// for LLM inference services, auto-create BenchmarkTargets, and run proxy discovery.
func DiscoverTargets(ctx context.Context, opts DiscoverTargetsOptions) (DiscoverTargetsResponse, error) {
	if opts.RestURL == "" {
		return DiscoverTargetsResponse{}, fmt.Errorf("forge.DiscoverTargets: RestURL is required")
	}
	if opts.ClusterID == 0 {
		return DiscoverTargetsResponse{}, fmt.Errorf("forge.DiscoverTargets: ClusterID is required")
	}

	base := strings.TrimRight(opts.RestURL, "/")

	token, err := restLogin(ctx, base, opts.Creds.restUsername(), opts.Creds.restPassword())
	if err != nil {
		return DiscoverTargetsResponse{}, fmt.Errorf("forge discover targets: login: %w", err)
	}

	body := map[string]any{
		"cluster_id":  opts.ClusterID,
		"auto_create": opts.AutoCreate,
	}
	if len(opts.SelectedServices) > 0 {
		body["selected_services"] = opts.SelectedServices
	}

	var resp DiscoverTargetsResponse
	if err := restPost(ctx, base+BenchmarkDiscoverTargetsEndpoint, token, body, &resp); err != nil {
		return DiscoverTargetsResponse{}, fmt.Errorf("forge discover targets: %w", err)
	}
	return resp, nil
}

// ListBenchmarkTargets calls GET /api/benchmarks/targets and optionally filters by cluster_id.
func ListBenchmarkTargets(ctx context.Context, opts ListBenchmarkTargetsOptions) ([]BenchmarkTargetResponse, error) {
	if opts.RestURL == "" {
		return nil, fmt.Errorf("forge.ListBenchmarkTargets: RestURL is required")
	}

	base := strings.TrimRight(opts.RestURL, "/")

	token, err := restLogin(ctx, base, opts.Creds.restUsername(), opts.Creds.restPassword())
	if err != nil {
		return nil, fmt.Errorf("forge list benchmark targets: login: %w", err)
	}

	url := base + BenchmarkTargetEndpoint
	if opts.ClusterID > 0 {
		url = fmt.Sprintf("%s?cluster_id=%d", url, opts.ClusterID)
	}

	var resp benchmarkTargetListResponse
	if err := restGet(ctx, url, token, &resp); err != nil {
		return nil, fmt.Errorf("forge list benchmark targets: %w", err)
	}
	return resp.Targets, nil
}

// RegisterBenchmarkTarget creates or reuses a forge BenchmarkTarget record.
//
// Idempotent: on name conflict, falls back to GET list + match by name.
// Returns ErrTargetNoClusterID when opts.ClusterID is zero (caller should skip).
// Best-effort: callers should log on error rather than aborting the run.
func RegisterBenchmarkTarget(ctx context.Context, opts BenchmarkTargetOptions) (BenchmarkTargetResponse, error) {
	if opts.RestURL == "" {
		return BenchmarkTargetResponse{}, fmt.Errorf("forge.RegisterBenchmarkTarget: RestURL is required")
	}
	if opts.Name == "" {
		return BenchmarkTargetResponse{}, fmt.Errorf("forge.RegisterBenchmarkTarget: Name is required")
	}
	if opts.ClusterID == 0 {
		return BenchmarkTargetResponse{}, ErrTargetNoClusterID
	}

	base := strings.TrimRight(opts.RestURL, "/")

	token, err := restLogin(ctx, base, opts.Creds.restUsername(), opts.Creds.restPassword())
	if err != nil {
		return BenchmarkTargetResponse{}, fmt.Errorf("forge benchmark target: login: %w", err)
	}

	body := map[string]any{
		"name":       opts.Name,
		"cluster_id": opts.ClusterID,
	}
	if opts.LLMBaseURL != "" {
		body["llm_base_url"] = opts.LLMBaseURL
	}
	if opts.LLMModel != "" {
		body["llm_model"] = opts.LLMModel
	}
	if opts.LLMNamespace != "" {
		body["llm_namespace"] = opts.LLMNamespace
	}
	if opts.LLMEndpoint != "" {
		body["llm_endpoint"] = opts.LLMEndpoint
	}
	if opts.ProxyNamespace != "" {
		body["proxy_namespace"] = opts.ProxyNamespace
	}
	if len(opts.Tags) > 0 {
		body["tags"] = opts.Tags
	}

	var created BenchmarkTargetResponse
	postErr := restPost(ctx, base+BenchmarkTargetEndpoint, token, body, &created)
	if postErr == nil {
		return created, nil
	}

	// 409 or 400-with-"already exists": fall back to list-and-match.
	if !isConflictHTTP(postErr) {
		return BenchmarkTargetResponse{}, fmt.Errorf("forge benchmark target: create: %w", postErr)
	}

	existing, lookupErr := benchmarkTargetFindByName(ctx, base, token, opts.Name, opts.ClusterID)
	if lookupErr == nil {
		return existing, nil
	}
	if !errors.Is(lookupErr, errTargetOtherCluster) {
		return BenchmarkTargetResponse{}, fmt.Errorf("forge benchmark target: conflict + list failed: %w (original: %v)", lookupErr, postErr)
	}

	// Forge keys target names globally, so the name belongs to another
	// cluster's target. Register this cluster's endpoint under a
	// cluster-qualified name instead of silently reusing the other one.
	qualified := opts.Name + "-" + opts.clusterSuffix()
	body["name"] = qualified
	if postErr = restPost(ctx, base+BenchmarkTargetEndpoint, token, body, &created); postErr == nil {
		return created, nil
	}
	if !isConflictHTTP(postErr) {
		return BenchmarkTargetResponse{}, fmt.Errorf("forge benchmark target: create %q: %w", qualified, postErr)
	}
	existing, lookupErr = benchmarkTargetFindByName(ctx, base, token, qualified, opts.ClusterID)
	if lookupErr != nil {
		return BenchmarkTargetResponse{}, fmt.Errorf("forge benchmark target: conflict + list failed for %q: %w", qualified, lookupErr)
	}
	return existing, nil
}

// clusterSuffix is what disambiguates a target name across clusters.
func (o BenchmarkTargetOptions) clusterSuffix() string {
	if o.ClusterName != "" {
		return o.ClusterName
	}
	return fmt.Sprintf("cluster-%d", o.ClusterID)
}

// errTargetOtherCluster: a target of that name exists, but on another cluster.
var errTargetOtherCluster = errors.New("benchmark target name is taken by another cluster")

// benchmarkTargetListResponse is the wrapper object returned by
// GET /api/benchmarks/targets (backend/routes/benchmarks.py:317,
// response_model=BenchmarkTargetListResponse).
// Shape: {"targets":[...],"total":N}
type benchmarkTargetListResponse struct {
	Targets []BenchmarkTargetResponse `json:"targets"`
	Total   int                       `json:"total"`
}

// benchmarkTargetFindByName GETs /api/benchmarks/targets and returns the record
// whose name matches exactly and belongs to clusterID (0 = any cluster). When
// the only match belongs to another cluster it returns errTargetOtherCluster.
//
// Forge returns a JSON object {"targets":[...],"total":N}, NOT a bare array.
// (GET /api/benchmarks/proxies IS a bare array — only this endpoint uses the
// list-response wrapper.)
func benchmarkTargetFindByName(ctx context.Context, base, token, name string, clusterID int) (BenchmarkTargetResponse, error) {
	var resp benchmarkTargetListResponse
	if err := restGet(ctx, base+BenchmarkTargetEndpoint, token, &resp); err != nil {
		return BenchmarkTargetResponse{}, fmt.Errorf("list benchmark targets: %w", err)
	}
	otherCluster := false
	for _, r := range resp.Targets {
		if r.Name != name {
			continue
		}
		if clusterID == 0 || r.ClusterID == clusterID || r.ClusterID == 0 {
			return r, nil
		}
		otherCluster = true
	}
	if otherCluster {
		return BenchmarkTargetResponse{}, fmt.Errorf("%w: %q (cluster %d)", errTargetOtherCluster, name, clusterID)
	}
	return BenchmarkTargetResponse{}, fmt.Errorf("benchmark target %q not found in forge", name)
}
