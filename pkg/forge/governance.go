package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// GovernanceOverviewResponse models AiGovernanceOverviewResponse from
// GET /api/k8s/clusters/{cluster_id}/llm-observability/governance/overview.
type GovernanceOverviewResponse struct {
	Available        bool              `json:"available"`
	ClusterID        int               `json:"cluster_id,omitempty"`
	Endpoint         string            `json:"endpoint,omitempty"`
	UpdatedAt        string            `json:"updated_at,omitempty"`
	Reason           string            `json:"reason,omitempty"`
	Errors           map[string]string `json:"errors,omitempty"`
	Namespace        string            `json:"namespace,omitempty"`
	Tenants          []map[string]any  `json:"tenants,omitempty"`
	Providers        []map[string]any  `json:"providers,omitempty"`
	Models           []map[string]any  `json:"models,omitempty"`
	Routes           []map[string]any  `json:"routes,omitempty"`
	Orgs             []map[string]any  `json:"orgs,omitempty"`
	Organizations    []map[string]any  `json:"organizations,omitempty"`
	Teams            []map[string]any  `json:"teams,omitempty"`
	Principals       []map[string]any  `json:"principals,omitempty"`
	VirtualKeys      []map[string]any  `json:"virtual_keys,omitempty"`
	ReadinessChecks  []map[string]any  `json:"readiness_checks,omitempty"`
	Diagnostics      []map[string]any  `json:"diagnostics,omitempty"`
	RequestAuditLogs []map[string]any  `json:"request_audit_logs,omitempty"`
	TopologyNodes    []map[string]any  `json:"topology_nodes,omitempty"`
	TopologyEdges    []map[string]any  `json:"topology_edges,omitempty"`
}

// Summary returns a single operator-facing summary line for the AI Gateway Dependency Fabric.
func (g GovernanceOverviewResponse) Summary() string {
	orgsCount := len(g.Orgs)
	if len(g.Organizations) > orgsCount {
		orgsCount = len(g.Organizations)
	}
	return fmt.Sprintf(
		"available=%v ns=%s tenants=%d providers=%d models=%d routes=%d orgs=%d teams=%d principals=%d keys=%d checks=%d nodes=%d edges=%d",
		g.Available,
		orDash(g.Namespace),
		len(g.Tenants),
		len(g.Providers),
		len(g.Models),
		len(g.Routes),
		orgsCount,
		len(g.Teams),
		len(g.Principals),
		len(g.VirtualKeys),
		len(g.ReadinessChecks),
		len(g.TopologyNodes),
		len(g.TopologyEdges),
	)
}

// GovernanceMutationResponse models AiGovernanceMutationResponse returned by Forge governance mutations.
type GovernanceMutationResponse struct {
	Status           string           `json:"status,omitempty"`
	Success          bool             `json:"success"`
	Action           string           `json:"action,omitempty"`
	Message          string           `json:"message,omitempty"`
	ClusterID        int              `json:"cluster_id,omitempty"`
	ManifestsApplied []map[string]any `json:"manifests_applied,omitempty"`
	AppliedYAML      string           `json:"applied_yaml,omitempty"`
	Entity           map[string]any   `json:"entity,omitempty"`
	Details          map[string]any   `json:"details,omitempty"`
	ID               string           `json:"id,omitempty"`
	Name             string           `json:"name,omitempty"`
	MaskedKey        string           `json:"masked_key,omitempty"`
	CleartextKey     string           `json:"cleartext_key,omitempty"`
}

// GovernanceRemediateRequest models RemediateAiGatewayRequest for
// POST /api/k8s/clusters/{cluster_id}/llm-observability/governance/remediate.
// Supported actions: restart_controller | apply_dns_resolver | enable_numa_spanning | apply_default_egress.
type GovernanceRemediateRequest struct {
	ClusterID  int    `json:"cluster_id,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
	Action     string `json:"action"`
	TargetName string `json:"target_name,omitempty"`
}

// GovernanceRemediateResponse models AiGovernanceMutationResponse from the governance remediate endpoint.
type GovernanceRemediateResponse = GovernanceMutationResponse

// GovernanceTestRequestPayload models GatewayInferenceTestRequest for
// POST /api/k8s/clusters/{cluster_id}/llm-observability/governance/test-request.
type GovernanceTestRequestPayload struct {
	ClusterID              int    `json:"cluster_id,omitempty"`
	Namespace              string `json:"namespace,omitempty"`
	RouteName              string `json:"route_name,omitempty"`
	Method                 string `json:"method,omitempty"`
	Path                   string `json:"path,omitempty"`
	VirtualKey             string `json:"virtual_key,omitempty"`
	Model                  string `json:"model,omitempty"`
	ModelID                string `json:"model_id,omitempty"`
	Prompt                 string `json:"prompt,omitempty"`
	SimulatePrimaryFailure bool   `json:"simulate_primary_failure,omitempty"`
}

// GovernanceTestRequestResponse models GatewayInferenceTestResponse from the governance test-request endpoint.
type GovernanceTestRequestResponse struct {
	StatusCode          int            `json:"status_code"`
	Success             bool           `json:"success"`
	RouteName           string         `json:"route_name,omitempty"`
	ListenerPort        int            `json:"listener_port,omitempty"`
	Tenant              string         `json:"tenant,omitempty"`
	Model               string         `json:"model,omitempty"`
	SelectedProvider    string         `json:"selected_provider,omitempty"`
	FailoverTriggered   bool           `json:"failover_triggered,omitempty"`
	CacheStatus         string         `json:"cache_status,omitempty"`
	SemanticCacheStatus string         `json:"semantic_cache_status,omitempty"`
	LatencyMS           float64        `json:"latency_ms"`
	PromptTokens        int            `json:"prompt_tokens"`
	CompletionTokens    int            `json:"completion_tokens"`
	TotalTokens         int            `json:"total_tokens"`
	CostUSD             float64        `json:"cost_usd,omitempty"`
	EstimatedCostUSD    float64        `json:"estimated_cost_usd,omitempty"`
	AuthVerified        bool           `json:"auth_verified,omitempty"`
	PrincipalName       string         `json:"principal_name,omitempty"`
	TeamName            string         `json:"team_name,omitempty"`
	OrgName             string         `json:"org_name,omitempty"`
	ClickhouseLoggedID  string         `json:"clickhouse_logged_id,omitempty"`
	ResponseText        string         `json:"response_text,omitempty"`
	ResponseBody        any            `json:"response_body,omitempty"`
	TroubleshootingHint string         `json:"troubleshooting_hint,omitempty"`
	AuditRecord         map[string]any `json:"audit_record,omitempty"`
}

// ResponseBodyString formats ResponseBody as a string regardless of whether the server
// returned a JSON object (dict) or a plain string.
func (r GovernanceTestRequestResponse) ResponseBodyString() string {
	switch v := r.ResponseBody.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}

// Summary returns a single operator-facing summary line for a live AI Gateway test probe.
func (r GovernanceTestRequestResponse) Summary() string {
	status := "ok"
	if !r.Success {
		status = "failed"
	}
	msg := fmt.Sprintf("status=%s http=%d route=%s provider=%s cache=%s latency=%.1fms tokens=%d (prompt=%d completion=%d) auth=%v",
		status,
		r.StatusCode,
		orDash(r.RouteName),
		orDash(r.SelectedProvider),
		orDash(r.SemanticCacheStatus),
		r.LatencyMS,
		r.TotalTokens,
		r.PromptTokens,
		r.CompletionTokens,
		r.AuthVerified,
	)
	if r.TroubleshootingHint != "" {
		msg += " hint=" + strings.TrimSpace(r.TroubleshootingHint)
	}
	return msg
}

// GovernanceOverview fetches the Multi-Cluster AI Gateway dependency graph & readiness checklist from Forge.
func GovernanceOverview(ctx context.Context, restURL string, clusterID int, creds RestCreds) (GovernanceOverviewResponse, error) {
	if clusterID <= 0 {
		return GovernanceOverviewResponse{}, fmt.Errorf("forge.GovernanceOverview: clusterID must be > 0")
	}
	base := resolveRESTBaseURL(restURL)
	token, err := restLogin(ctx, base, creds.restUsername(), creds.restPassword())
	if err != nil {
		return GovernanceOverviewResponse{}, fmt.Errorf("forge REST login: %w", err)
	}
	endpoint := fmt.Sprintf("%s/api/k8s/clusters/%d/llm-observability/governance/overview", base, clusterID)
	var out GovernanceOverviewResponse
	if err := restGet(ctx, endpoint, token, &out); err != nil {
		return GovernanceOverviewResponse{}, err
	}
	if out.ClusterID == 0 {
		out.ClusterID = clusterID
	}
	return out, nil
}

// GovernanceRemediate triggers a server-side BNK 2.4.1 AI Gateway remediation action in Forge.
func GovernanceRemediate(ctx context.Context, restURL string, clusterID int, req GovernanceRemediateRequest, creds RestCreds) (GovernanceRemediateResponse, error) {
	if clusterID <= 0 {
		return GovernanceRemediateResponse{}, fmt.Errorf("forge.GovernanceRemediate: clusterID must be > 0")
	}
	if strings.TrimSpace(req.Action) == "" {
		return GovernanceRemediateResponse{}, fmt.Errorf("forge.GovernanceRemediate: action is required")
	}
	if req.ClusterID == 0 {
		req.ClusterID = clusterID
	}
	if strings.TrimSpace(req.Namespace) == "" {
		req.Namespace = "f5-bnk"
	}
	base := resolveRESTBaseURL(restURL)
	token, err := restLogin(ctx, base, creds.restUsername(), creds.restPassword())
	if err != nil {
		return GovernanceRemediateResponse{}, fmt.Errorf("forge REST login: %w", err)
	}
	endpoint := fmt.Sprintf("%s/api/k8s/clusters/%d/llm-observability/governance/remediate", base, clusterID)
	var out GovernanceRemediateResponse
	if err := restPost(ctx, endpoint, token, req, &out); err != nil {
		return GovernanceRemediateResponse{}, err
	}
	return out, nil
}

// GovernanceTestRequest sends a live chat-completion test probe through the cluster's AI Gateway route via Forge.
func GovernanceTestRequest(ctx context.Context, restURL string, clusterID int, req GovernanceTestRequestPayload, creds RestCreds) (GovernanceTestRequestResponse, error) {
	if clusterID <= 0 {
		return GovernanceTestRequestResponse{}, fmt.Errorf("forge.GovernanceTestRequest: clusterID must be > 0")
	}
	if req.ClusterID == 0 {
		req.ClusterID = clusterID
	}
	if strings.TrimSpace(req.Namespace) == "" {
		req.Namespace = "f5-bnk"
	}
	if strings.TrimSpace(req.Method) == "" {
		req.Method = "POST"
	}
	if strings.TrimSpace(req.Path) == "" {
		req.Path = "/v1/chat/completions"
	}
	if strings.TrimSpace(req.Prompt) == "" {
		req.Prompt = "Reply with OK and identify the active model in one short sentence."
	}
	base := resolveRESTBaseURL(restURL)
	token, err := restLogin(ctx, base, creds.restUsername(), creds.restPassword())
	if err != nil {
		return GovernanceTestRequestResponse{}, fmt.Errorf("forge REST login: %w", err)
	}
	endpoint := fmt.Sprintf("%s/api/k8s/clusters/%d/llm-observability/governance/test-request", base, clusterID)
	var out GovernanceTestRequestResponse
	if err := restPost(ctx, endpoint, token, req, &out); err != nil {
		return GovernanceTestRequestResponse{}, err
	}
	return out, nil
}

// GovernanceTenantCreateRequest models CreateAiTenantRequest for
// POST /api/k8s/clusters/{cluster_id}/llm-observability/governance/tenants.
type GovernanceTenantCreateRequest struct {
	ClusterID    int    `json:"cluster_id,omitempty"`
	Name         string `json:"name"`
	DisplayName  string `json:"display_name,omitempty"`
	Namespace    string `json:"namespace,omitempty"`
	OIDCIssuer   string `json:"oidc_issuer,omitempty"`
	OIDCClientID string `json:"oidc_client_id,omitempty"`
}

// BNK 2.4.1 AIProvider.spec.type values (Manual Section 4.2).
const (
	AIProviderTypeOpenAI       = "OpenAI"
	AIProviderTypeAnthropic    = "Anthropic"
	AIProviderTypeAzureOpenAI  = "AzureOpenAI"
	AIProviderTypeAzureFoundry = "AzureFoundry"
	AIProviderTypeBedrock      = "Bedrock"
	AIProviderTypeGemini       = "Gemini"
	AIProviderTypeGeneric      = "Generic"
)

// SupportedAIProviderTypes lists all 7 canonical BNK 2.4.1 AIProvider types.
var SupportedAIProviderTypes = []string{
	AIProviderTypeOpenAI,
	AIProviderTypeAnthropic,
	AIProviderTypeAzureOpenAI,
	AIProviderTypeAzureFoundry,
	AIProviderTypeBedrock,
	AIProviderTypeGemini,
	AIProviderTypeGeneric,
}

// BNK 2.4.1 AIModel.spec.deployments[].dialects values (Manual Section 4.3).
const (
	AIModelDialectOpenAIChat        = "OpenAIChat"
	AIModelDialectAnthropicMessages = "AnthropicMessages"
)

// SupportedAIModelDialects lists all supported BNK 2.4.1 AIModel dialects.
var SupportedAIModelDialects = []string{
	AIModelDialectOpenAIChat,
	AIModelDialectAnthropicMessages,
}

// GovernanceProviderCreateRequest models CreateAiProviderRequest for
// POST /api/k8s/clusters/{cluster_id}/llm-observability/governance/providers.
type GovernanceProviderCreateRequest struct {
	ClusterID      int    `json:"cluster_id,omitempty"`
	Namespace      string `json:"namespace,omitempty"`
	Tenant         string `json:"tenant,omitempty"`
	TenantRef      string `json:"tenant_ref,omitempty"`
	Name           string `json:"name"`
	Vendor         string `json:"vendor,omitempty"`
	ProviderType   string `json:"provider_type,omitempty"`
	BaseURL        string `json:"base_url,omitempty"`
	EndpointURL    string `json:"endpoint_url,omitempty"`
	APIKey         string `json:"api_key,omitempty"`
	CredentialName string `json:"credential_name,omitempty"`
	SecretName     string `json:"secret_name,omitempty"`
	Enabled        *bool  `json:"enabled,omitempty"`
}

// GovernanceModelDeploymentSpec models AiModelDeploymentSpec within CreateAiModelRequest.
type GovernanceModelDeploymentSpec struct {
	Name          string   `json:"name"`
	Provider      string   `json:"provider"`
	ProviderRef   string   `json:"provider_ref,omitempty"`
	Upstream      string   `json:"upstream"`
	Dialects      []string `json:"dialects,omitempty"`
	Weight        int      `json:"weight,omitempty"`
	State         string   `json:"state,omitempty"`
	InputPerMtok  float64  `json:"input_per_mtok,omitempty"`
	OutputPerMtok float64  `json:"output_per_mtok,omitempty"`
}

// GovernanceModelCreateRequest models CreateAiModelRequest for
// POST /api/k8s/clusters/{cluster_id}/llm-observability/governance/models.
type GovernanceModelCreateRequest struct {
	ClusterID               int                             `json:"cluster_id,omitempty"`
	Namespace               string                          `json:"namespace,omitempty"`
	Tenant                  string                          `json:"tenant,omitempty"`
	TenantRef               string                          `json:"tenant_ref,omitempty"`
	Name                    string                          `json:"name"`
	PublicName              string                          `json:"public_name,omitempty"`
	ModelID                 string                          `json:"model_id,omitempty"`
	Deployments             []GovernanceModelDeploymentSpec `json:"deployments,omitempty"`
	InputTokenCostMicroUSD  float64                         `json:"input_token_cost_micro_usd,omitempty"`
	OutputTokenCostMicroUSD float64                         `json:"output_token_cost_micro_usd,omitempty"`
}

// GovernanceRouteProviderSpec models an entry in AIGatewayProfile.spec.providers (Manual Section 4.5.6)
// with an optional route-scoped credentialRef override.
type GovernanceRouteProviderSpec struct {
	Name          string `json:"name"`
	CredentialRef string `json:"credential_ref,omitempty"`
}

// GovernanceRouteCreateRequest models CreateAiGatewayRouteRequest for
// POST /api/k8s/clusters/{cluster_id}/llm-observability/governance/routes.
type GovernanceRouteCreateRequest struct {
	ClusterID                        int                           `json:"cluster_id,omitempty"`
	Namespace                        string                        `json:"namespace,omitempty"`
	Tenant                           string                        `json:"tenant,omitempty"`
	TenantRef                        string                        `json:"tenant_ref,omitempty"`
	Name                             string                        `json:"name"`
	GatewayName                      string                        `json:"gateway_name,omitempty"`
	ListenerPort                     int                           `json:"listener_port,omitempty"`
	Port                             int                           `json:"port,omitempty"`
	VIPAddress                       string                        `json:"vip_address,omitempty"`
	PathPrefix                       string                        `json:"path_prefix,omitempty"`
	AuthMode                         string                        `json:"auth_mode,omitempty"`
	VirtualKeysEnabled               *bool                         `json:"virtual_keys_enabled,omitempty"`
	JWTEnabled                       *bool                         `json:"jwt_enabled,omitempty"`
	JWTIssuer                        string                        `json:"jwt_issuer,omitempty"`
	JWTAudiences                     []string                      `json:"jwt_audiences,omitempty"`
	JWKSURI                          string                        `json:"jwks_uri,omitempty"`
	JWKSRefreshInterval              string                        `json:"jwks_refresh_interval,omitempty"`
	JWTSecretRef                     string                        `json:"jwt_secret_ref,omitempty"`
	JWTHeader                        string                        `json:"jwt_header,omitempty"`
	JWTScheme                        string                        `json:"jwt_scheme,omitempty"`
	JWTClockSkew                     string                        `json:"jwt_clock_skew,omitempty"`
	JWTPrincipalClaim                string                        `json:"jwt_principal_claim,omitempty"`
	JWTTeamClaim                     string                        `json:"jwt_team_claim,omitempty"`
	JWTOrgClaim                      string                        `json:"jwt_org_claim,omitempty"`
	FailoverEnabled                  *bool                         `json:"failover_enabled,omitempty"`
	FailoverMaxAttempts              int                           `json:"failover_max_attempts,omitempty"`
	FailoverOn429                    *bool                         `json:"failover_on_429,omitempty"`
	FailoverOn5xx                    *bool                         `json:"failover_on_5xx,omitempty"`
	PromptCacheEnabled               *bool                         `json:"prompt_cache_enabled,omitempty"`
	SemanticCacheEnabled             *bool                         `json:"semantic_cache_enabled,omitempty"`
	SemanticCacheSimilarityThreshold float64                       `json:"semantic_cache_similarity_threshold,omitempty"`
	SemanticCachePoolMaxSize         int                           `json:"semantic_cache_pool_max_size,omitempty"`
	SemanticCachePoolIdleTimeout     string                        `json:"semantic_cache_pool_idle_timeout,omitempty"`
	ModelRoutingEnabled              *bool                         `json:"model_routing_enabled,omitempty"`
	SemanticRouterService            string                        `json:"semantic_router_service,omitempty"`
	SemanticRouterNamespace          string                        `json:"semantic_router_namespace,omitempty"`
	SemanticRouterPort               int                           `json:"semantic_router_port,omitempty"`
	SemanticRouterTimeout            string                        `json:"semantic_router_timeout,omitempty"`
	DefaultModelRef                  string                        `json:"default_model_ref,omitempty"`
	Providers                        []string                      `json:"providers,omitempty"`
	ProviderCredentials              map[string]string             `json:"provider_credentials,omitempty"`
	ProviderOverrides                []GovernanceRouteProviderSpec `json:"provider_overrides,omitempty"`
}

// GovernanceRouteBindRequest aliases GovernanceRouteCreateRequest.
type GovernanceRouteBindRequest = GovernanceRouteCreateRequest

// GovernanceRoutePoliciesUpdateRequest models UpdateAiGatewayRoutePoliciesRequest for
// PATCH /api/k8s/clusters/{cluster_id}/llm-observability/governance/routes/{route_name}/policies.
type GovernanceRoutePoliciesUpdateRequest struct {
	ClusterID                        int                           `json:"cluster_id,omitempty"`
	Namespace                        string                        `json:"namespace,omitempty"`
	RouteName                        string                        `json:"route_name"`
	AuthMode                         string                        `json:"auth_mode,omitempty"`
	VirtualKeysEnabled               *bool                         `json:"virtual_keys_enabled,omitempty"`
	JWTEnabled                       *bool                         `json:"jwt_enabled,omitempty"`
	JWTIssuer                        string                        `json:"jwt_issuer,omitempty"`
	JWTAudiences                     []string                      `json:"jwt_audiences,omitempty"`
	JWKSURI                          string                        `json:"jwks_uri,omitempty"`
	JWKSRefreshInterval              string                        `json:"jwks_refresh_interval,omitempty"`
	JWTSecretRef                     string                        `json:"jwt_secret_ref,omitempty"`
	JWTHeader                        string                        `json:"jwt_header,omitempty"`
	JWTScheme                        string                        `json:"jwt_scheme,omitempty"`
	JWTClockSkew                     string                        `json:"jwt_clock_skew,omitempty"`
	JWTPrincipalClaim                string                        `json:"jwt_principal_claim,omitempty"`
	JWTTeamClaim                     string                        `json:"jwt_team_claim,omitempty"`
	JWTOrgClaim                      string                        `json:"jwt_org_claim,omitempty"`
	FailoverEnabled                  *bool                         `json:"failover_enabled,omitempty"`
	FailoverMaxAttempts              *int                          `json:"failover_max_attempts,omitempty"`
	PromptCacheEnabled               *bool                         `json:"prompt_cache_enabled,omitempty"`
	SemanticCacheEnabled             *bool                         `json:"semantic_cache_enabled,omitempty"`
	SemanticCacheSimilarityThreshold *float64                      `json:"semantic_cache_similarity_threshold,omitempty"`
	SemanticCachePoolMaxSize         *int                          `json:"semantic_cache_pool_max_size,omitempty"`
	SemanticCachePoolIdleTimeout     string                        `json:"semantic_cache_pool_idle_timeout,omitempty"`
	ModelRoutingEnabled              *bool                         `json:"model_routing_enabled,omitempty"`
	SemanticRouterService            string                        `json:"semantic_router_service,omitempty"`
	SemanticRouterNamespace          string                        `json:"semantic_router_namespace,omitempty"`
	SemanticRouterPort               int                           `json:"semantic_router_port,omitempty"`
	SemanticRouterTimeout            string                        `json:"semantic_router_timeout,omitempty"`
	Providers                        []string                      `json:"providers,omitempty"`
	ProviderCredentials              map[string]string             `json:"provider_credentials,omitempty"`
	ProviderOverrides                []GovernanceRouteProviderSpec `json:"provider_overrides,omitempty"`
}

// GovernanceOrgCreateRequest models CreateGovernanceOrgRequest for
// POST /api/k8s/clusters/{cluster_id}/llm-observability/governance/orgs.
type GovernanceOrgCreateRequest struct {
	ClusterID         int     `json:"cluster_id,omitempty"`
	Tenant            string  `json:"tenant,omitempty"`
	TenantRef         string  `json:"tenant_ref,omitempty"`
	Name              string  `json:"name"`
	MonthlyBudgetUSD  float64 `json:"monthly_budget_usd,omitempty"`
	AlertThresholdPct float64 `json:"alert_threshold_pct,omitempty"`
	RateLimitRPM      int     `json:"rate_limit_rpm,omitempty"`
	RateLimitTPM      int     `json:"rate_limit_tpm,omitempty"`
	RPMLimit          int     `json:"rpm_limit,omitempty"`
	TPMLimit          int     `json:"tpm_limit,omitempty"`
}

// GovernanceTeamCreateRequest models CreateGovernanceTeamRequest for
// POST /api/k8s/clusters/{cluster_id}/llm-observability/governance/teams.
type GovernanceTeamCreateRequest struct {
	ClusterID        int      `json:"cluster_id,omitempty"`
	Tenant           string   `json:"tenant,omitempty"`
	OrgID            string   `json:"org_id"`
	Name             string   `json:"name"`
	MonthlyBudgetUSD float64  `json:"monthly_budget_usd,omitempty"`
	AllowedModels    []string `json:"allowed_models,omitempty"`
	RateLimitRPM     int      `json:"rate_limit_rpm,omitempty"`
	RateLimitTPM     int      `json:"rate_limit_tpm,omitempty"`
	RPMLimit         int      `json:"rpm_limit,omitempty"`
	TPMLimit         int      `json:"tpm_limit,omitempty"`
}

// GovernancePrincipalCreateRequest models CreateGovernancePrincipalRequest for
// POST /api/k8s/clusters/{cluster_id}/llm-observability/governance/principals.
type GovernancePrincipalCreateRequest struct {
	ClusterID        int      `json:"cluster_id,omitempty"`
	Tenant           string   `json:"tenant,omitempty"`
	TeamID           string   `json:"team_id"`
	Name             string   `json:"name"`
	Kind             string   `json:"kind,omitempty"`
	PrincipalType    string   `json:"principal_type,omitempty"`
	Role             string   `json:"role,omitempty"`
	MonthlyBudgetUSD float64  `json:"monthly_budget_usd,omitempty"`
	AllowedModels    []string `json:"allowed_models,omitempty"`
	RateLimitRPM     int      `json:"rate_limit_rpm,omitempty"`
	RateLimitTPM     int      `json:"rate_limit_tpm,omitempty"`
	RPMLimit         int      `json:"rpm_limit,omitempty"`
	TPMLimit         int      `json:"tpm_limit,omitempty"`
}

// GovernanceBudgetCreateRequest models POST /api/k8s/clusters/{cluster_id}/llm-observability/governance/budgets.
type GovernanceBudgetCreateRequest struct {
	ClusterID int     `json:"cluster_id,omitempty"`
	Tenant    string  `json:"tenant,omitempty"`
	Name      string  `json:"name"`
	Scope     string  `json:"scope,omitempty"`
	TargetID  string  `json:"target_id,omitempty"`
	LimitUSD  float64 `json:"limit_usd"`
	Period    string  `json:"period,omitempty"`
}

// GovernanceRateLimitCreateRequest models POST /api/k8s/clusters/{cluster_id}/llm-observability/governance/ratelimits.
type GovernanceRateLimitCreateRequest struct {
	ClusterID         int    `json:"cluster_id,omitempty"`
	Tenant            string `json:"tenant,omitempty"`
	Name              string `json:"name"`
	Scope             string `json:"scope,omitempty"`
	TargetID          string `json:"target_id,omitempty"`
	RequestsPerMinute int    `json:"requests_per_minute,omitempty"`
	TokensPerMinute   int    `json:"tokens_per_minute,omitempty"`
}

// GovernanceVirtualKeyCreateRequest models IssueVirtualKeyRequest for
// POST /api/k8s/clusters/{cluster_id}/llm-observability/governance/virtual-keys.
type GovernanceVirtualKeyCreateRequest struct {
	ClusterID        int      `json:"cluster_id,omitempty"`
	Tenant           string   `json:"tenant,omitempty"`
	PrincipalID      string   `json:"principal_id"`
	Name             string   `json:"name"`
	AllowedModels    []string `json:"allowed_models,omitempty"`
	RPMLimit         int      `json:"rpm_limit,omitempty"`
	TPMLimit         int      `json:"tpm_limit,omitempty"`
	MonthlyBudgetUSD float64  `json:"monthly_budget_usd,omitempty"`
}

// GovernanceVirtualKeyCreateResponse models AiGovernanceMutationResponse from the virtual-keys endpoint.
type GovernanceVirtualKeyCreateResponse struct {
	Status       string         `json:"status,omitempty"`
	Success      bool           `json:"success"`
	Action       string         `json:"action,omitempty"`
	Message      string         `json:"message,omitempty"`
	ID           string         `json:"id,omitempty"`
	KeyID        string         `json:"key_id,omitempty"`
	Name         string         `json:"name,omitempty"`
	MaskedKey    string         `json:"masked_key,omitempty"`
	CleartextKey string         `json:"cleartext_key,omitempty"`
	APIKey       string         `json:"api_key,omitempty"`
	PrincipalID  string         `json:"principal_id,omitempty"`
	Tenant       string         `json:"tenant,omitempty"`
	Entity       map[string]any `json:"entity,omitempty"`
	Details      map[string]any `json:"details,omitempty"`
}

// ResolveClusterIDByName resolves a Forge cluster ID by cluster name via REST.
func ResolveClusterIDByName(ctx context.Context, restURL, clusterName string, creds RestCreds) (int, error) {
	clusterName = strings.TrimSpace(clusterName)
	if clusterName == "" {
		return 0, fmt.Errorf("forge.ResolveClusterIDByName: clusterName is empty")
	}
	base := resolveRESTBaseURL(restURL)
	token, err := restLogin(ctx, base, creds.restUsername(), creds.restPassword())
	if err != nil {
		return 0, fmt.Errorf("forge REST login: %w", err)
	}

	candidates := []string{
		defaultProjectName(clusterName),
		"awsbnkctl-" + clusterName,
		"gkebnkctl-" + clusterName,
		"bnkctl-" + clusterName,
		clusterName,
	}
	for _, cand := range candidates {
		proj, findErr := restFindProjectByName(ctx, base, token, cand)
		if findErr != nil {
			continue
		}
		if cl, cErr := restFindClusterByName(ctx, base, token, proj.ID, clusterName); cErr == nil && cl.ID > 0 {
			return cl.ID, nil
		}
	}
	return 0, fmt.Errorf("forge cluster %q not found", clusterName)
}

// GovernanceCreateTenant creates or updates an AITenant CR via Forge.
func GovernanceCreateTenant(ctx context.Context, restURL string, clusterID int, req GovernanceTenantCreateRequest, creds RestCreds) (GovernanceMutationResponse, error) {
	if req.ClusterID == 0 {
		req.ClusterID = clusterID
	}
	return governancePostMutation(ctx, restURL, clusterID, "tenants", req, creds)
}

// GovernanceCreateProvider creates or updates Secret + AICredential + AIProvider CRs via Forge.
func GovernanceCreateProvider(ctx context.Context, restURL string, clusterID int, req GovernanceProviderCreateRequest, creds RestCreds) (GovernanceMutationResponse, error) {
	if req.ClusterID == 0 {
		req.ClusterID = clusterID
	}
	return governancePostMutation(ctx, restURL, clusterID, "providers", req, creds)
}

// GovernanceCreateModel creates or updates an AIModel CR via Forge.
func GovernanceCreateModel(ctx context.Context, restURL string, clusterID int, req GovernanceModelCreateRequest, creds RestCreds) (GovernanceMutationResponse, error) {
	if req.ClusterID == 0 {
		req.ClusterID = clusterID
	}
	return governancePostMutation(ctx, restURL, clusterID, "models", req, creds)
}

// GovernanceCreateRoute creates or updates Gateway + HTTPRoute + AIGatewayProfile + AppPolicy CRs via Forge.
func GovernanceCreateRoute(ctx context.Context, restURL string, clusterID int, req GovernanceRouteCreateRequest, creds RestCreds) (GovernanceMutationResponse, error) {
	if req.ClusterID == 0 {
		req.ClusterID = clusterID
	}
	return governancePostMutation(ctx, restURL, clusterID, "routes", req, creds)
}

// GovernanceBindRoute aliases GovernanceCreateRoute.
func GovernanceBindRoute(ctx context.Context, restURL string, clusterID int, req GovernanceRouteBindRequest, creds RestCreds) (GovernanceMutationResponse, error) {
	return GovernanceCreateRoute(ctx, restURL, clusterID, req, creds)
}

// GovernanceUpdateRoutePolicies patches AIGatewayProfile policies on an existing route via Forge.
func GovernanceUpdateRoutePolicies(ctx context.Context, restURL string, clusterID int, routeName string, req GovernanceRoutePoliciesUpdateRequest, creds RestCreds) (GovernanceMutationResponse, error) {
	if clusterID <= 0 {
		return GovernanceMutationResponse{}, fmt.Errorf("forge.GovernanceUpdateRoutePolicies: clusterID must be > 0")
	}
	routeName = strings.TrimSpace(routeName)
	if routeName == "" {
		routeName = strings.TrimSpace(req.RouteName)
	}
	if routeName == "" {
		return GovernanceMutationResponse{}, fmt.Errorf("forge.GovernanceUpdateRoutePolicies: route_name is required")
	}
	if req.ClusterID == 0 {
		req.ClusterID = clusterID
	}
	if req.RouteName == "" {
		req.RouteName = routeName
	}
	base := resolveRESTBaseURL(restURL)
	token, err := restLogin(ctx, base, creds.restUsername(), creds.restPassword())
	if err != nil {
		return GovernanceMutationResponse{}, fmt.Errorf("forge REST login: %w", err)
	}
	endpoint := fmt.Sprintf("%s/api/k8s/clusters/%d/llm-observability/governance/routes/%s/policies", base, clusterID, url.PathEscape(routeName))
	httpReq, err := newRESTRequest(ctx, http.MethodPatch, endpoint, token, req)
	if err != nil {
		return GovernanceMutationResponse{}, err
	}
	var out GovernanceMutationResponse
	if err := doRESTRequest(httpReq, endpoint, &out); err != nil {
		return GovernanceMutationResponse{}, err
	}
	return out, nil
}

// GovernanceCreateOrg creates a DSSM organization via Forge.
func GovernanceCreateOrg(ctx context.Context, restURL string, clusterID int, req GovernanceOrgCreateRequest, creds RestCreds) (GovernanceMutationResponse, error) {
	if req.ClusterID == 0 {
		req.ClusterID = clusterID
	}
	return governancePostMutation(ctx, restURL, clusterID, "orgs", req, creds)
}

// GovernanceCreateTeam creates a DSSM team via Forge.
func GovernanceCreateTeam(ctx context.Context, restURL string, clusterID int, req GovernanceTeamCreateRequest, creds RestCreds) (GovernanceMutationResponse, error) {
	if req.ClusterID == 0 {
		req.ClusterID = clusterID
	}
	return governancePostMutation(ctx, restURL, clusterID, "teams", req, creds)
}

// GovernanceCreatePrincipal creates a DSSM principal via Forge.
func GovernanceCreatePrincipal(ctx context.Context, restURL string, clusterID int, req GovernancePrincipalCreateRequest, creds RestCreds) (GovernanceMutationResponse, error) {
	if req.ClusterID == 0 {
		req.ClusterID = clusterID
	}
	return governancePostMutation(ctx, restURL, clusterID, "principals", req, creds)
}

// GovernanceCreateBudget creates a DSSM budget via Forge.
func GovernanceCreateBudget(ctx context.Context, restURL string, clusterID int, req GovernanceBudgetCreateRequest, creds RestCreds) (GovernanceMutationResponse, error) {
	if req.ClusterID == 0 {
		req.ClusterID = clusterID
	}
	return governancePostMutation(ctx, restURL, clusterID, "budgets", req, creds)
}

// GovernanceCreateRateLimit creates a DSSM rate limit via Forge.
func GovernanceCreateRateLimit(ctx context.Context, restURL string, clusterID int, req GovernanceRateLimitCreateRequest, creds RestCreds) (GovernanceMutationResponse, error) {
	if req.ClusterID == 0 {
		req.ClusterID = clusterID
	}
	return governancePostMutation(ctx, restURL, clusterID, "ratelimits", req, creds)
}

// GovernanceCreateVirtualKey issues a virtual API key (f5vk_...) in DSSM via Forge.
func GovernanceCreateVirtualKey(ctx context.Context, restURL string, clusterID int, req GovernanceVirtualKeyCreateRequest, creds RestCreds) (GovernanceVirtualKeyCreateResponse, error) {
	if clusterID <= 0 {
		return GovernanceVirtualKeyCreateResponse{}, fmt.Errorf("forge.GovernanceCreateVirtualKey: clusterID must be > 0")
	}
	if req.ClusterID == 0 {
		req.ClusterID = clusterID
	}
	base := resolveRESTBaseURL(restURL)
	token, err := restLogin(ctx, base, creds.restUsername(), creds.restPassword())
	if err != nil {
		return GovernanceVirtualKeyCreateResponse{}, fmt.Errorf("forge REST login: %w", err)
	}
	endpoint := fmt.Sprintf("%s/api/k8s/clusters/%d/llm-observability/governance/virtual-keys", base, clusterID)
	var out GovernanceVirtualKeyCreateResponse
	if err := restPost(ctx, endpoint, token, req, &out); err != nil {
		return GovernanceVirtualKeyCreateResponse{}, err
	}
	out.normalize(req)
	return out, nil
}

// GovernanceRotateVirtualKey rotates a virtual API key in DSSM via Forge.
func GovernanceRotateVirtualKey(ctx context.Context, restURL string, clusterID int, keyID string, creds RestCreds) (GovernanceVirtualKeyCreateResponse, error) {
	if clusterID <= 0 {
		return GovernanceVirtualKeyCreateResponse{}, fmt.Errorf("forge.GovernanceRotateVirtualKey: clusterID must be > 0")
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return GovernanceVirtualKeyCreateResponse{}, fmt.Errorf("forge.GovernanceRotateVirtualKey: key_id is required")
	}
	base := resolveRESTBaseURL(restURL)
	token, err := restLogin(ctx, base, creds.restUsername(), creds.restPassword())
	if err != nil {
		return GovernanceVirtualKeyCreateResponse{}, fmt.Errorf("forge REST login: %w", err)
	}
	endpoint := fmt.Sprintf("%s/api/k8s/clusters/%d/llm-observability/governance/virtual-keys/%s/rotate", base, clusterID, url.PathEscape(keyID))
	var out GovernanceVirtualKeyCreateResponse
	if err := restPost(ctx, endpoint, token, map[string]any{}, &out); err != nil {
		return GovernanceVirtualKeyCreateResponse{}, err
	}
	out.normalize(GovernanceVirtualKeyCreateRequest{})
	return out, nil
}

// GovernanceRevokeVirtualKey revokes a virtual API key in DSSM via Forge.
func GovernanceRevokeVirtualKey(ctx context.Context, restURL string, clusterID int, keyID string, creds RestCreds) (GovernanceMutationResponse, error) {
	if clusterID <= 0 {
		return GovernanceMutationResponse{}, fmt.Errorf("forge.GovernanceRevokeVirtualKey: clusterID must be > 0")
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return GovernanceMutationResponse{}, fmt.Errorf("forge.GovernanceRevokeVirtualKey: key_id is required")
	}
	base := resolveRESTBaseURL(restURL)
	token, err := restLogin(ctx, base, creds.restUsername(), creds.restPassword())
	if err != nil {
		return GovernanceMutationResponse{}, fmt.Errorf("forge REST login: %w", err)
	}
	endpoint := fmt.Sprintf("%s/api/k8s/clusters/%d/llm-observability/governance/virtual-keys/%s", base, clusterID, url.PathEscape(keyID))
	httpReq, err := newRESTRequest(ctx, http.MethodDelete, endpoint, token, nil)
	if err != nil {
		return GovernanceMutationResponse{}, err
	}
	var out GovernanceMutationResponse
	if err := doRESTRequest(httpReq, endpoint, &out); err != nil {
		return GovernanceMutationResponse{}, err
	}
	return out, nil
}

func (r *GovernanceVirtualKeyCreateResponse) normalize(req GovernanceVirtualKeyCreateRequest) {
	if r.ID == "" && r.KeyID != "" {
		r.ID = r.KeyID
	}
	if r.KeyID == "" && r.ID != "" {
		r.KeyID = r.ID
	}
	if r.CleartextKey == "" && r.APIKey != "" {
		r.CleartextKey = r.APIKey
	}
	if r.APIKey == "" && r.CleartextKey != "" {
		r.APIKey = r.CleartextKey
	}
	if r.PrincipalID == "" && req.PrincipalID != "" {
		r.PrincipalID = req.PrincipalID
	}
	if r.Tenant == "" && req.Tenant != "" {
		r.Tenant = req.Tenant
	}
}

func governancePostMutation(ctx context.Context, restURL string, clusterID int, subpath string, req any, creds RestCreds) (GovernanceMutationResponse, error) {
	if clusterID <= 0 {
		return GovernanceMutationResponse{}, fmt.Errorf("forge governance %s: clusterID must be > 0", subpath)
	}
	base := resolveRESTBaseURL(restURL)
	token, err := restLogin(ctx, base, creds.restUsername(), creds.restPassword())
	if err != nil {
		return GovernanceMutationResponse{}, fmt.Errorf("forge REST login: %w", err)
	}
	endpoint := fmt.Sprintf("%s/api/k8s/clusters/%d/llm-observability/governance/%s", base, clusterID, subpath)
	var out GovernanceMutationResponse
	if err := restPost(ctx, endpoint, token, req, &out); err != nil {
		return GovernanceMutationResponse{}, err
	}
	return out, nil
}

// GovernanceSemanticRouterSpec configures a BNK 2.4.1 vLLM Semantic Router deployment
// and its cross-namespace ExternalName Service in the controller namespace (Manual Section 4.5.5).
type GovernanceSemanticRouterSpec struct {
	// ControllerNamespace is the namespace where AIGatewayProfile and Service/vllmsr live (default: "f5-cne-system").
	ControllerNamespace string
	// ExternalServiceName is the Service name referenced in AIGatewayProfile.spec.modelRouting.semanticRouter.serviceRef.name (default: "vllmsr").
	ExternalServiceName string
	//RouterNamespace is the namespace where the vLLM Semantic Router workload runs (default: "vllm-semantic-router-system").
	RouterNamespace string
	// RouterServiceName is the Service name in RouterNamespace (default: "vsr-service").
	RouterServiceName string
	// Port is the HTTP port exposed by the vLLM Semantic Router (default: 8000).
	Port int
	// Image is the container image for the vLLM Semantic Router workload (optional; if empty, only the cross-namespace ExternalName Service is rendered unless IncludeWorkload is true).
	Image string
	// DefaultModel is the fallback model name returned by the semantic router (default: "claude-sonnet-4-5").
	DefaultModel string
	// IncludeWorkload controls whether to render Namespace + Deployment + Service in RouterNamespace in addition to Service/vllmsr in ControllerNamespace.
	IncludeWorkload bool
}

// RenderVLLMSemanticRouterManifest renders the Kubernetes YAML manifest for BNK 2.4.1
// Manual Section 4.5.5:
//  1. Service/<ExternalServiceName> (default "vllmsr") of type ExternalName in ControllerNamespace
//     (default "f5-cne-system") pointing to <RouterServiceName>.<RouterNamespace>.svc.cluster.local.
//  2. Optionally (when IncludeWorkload is true) the Namespace, Deployment, and ClusterIP Service
//     for <RouterServiceName> in <RouterNamespace> on port 8000.
func RenderVLLMSemanticRouterManifest(spec GovernanceSemanticRouterSpec) string {
	ctrlNS := strings.TrimSpace(spec.ControllerNamespace)
	if ctrlNS == "" {
		ctrlNS = "f5-cne-system"
	}
	extSvc := strings.TrimSpace(spec.ExternalServiceName)
	if extSvc == "" {
		extSvc = "vllmsr"
	}
	routerNS := strings.TrimSpace(spec.RouterNamespace)
	if routerNS == "" {
		routerNS = "vllm-semantic-router-system"
	}
	routerSvc := strings.TrimSpace(spec.RouterServiceName)
	if routerSvc == "" {
		routerSvc = "vsr-service"
	}
	port := spec.Port
	if port <= 0 {
		port = 8000
	}
	defaultModel := strings.TrimSpace(spec.DefaultModel)
	if defaultModel == "" {
		defaultModel = "claude-sonnet-4-5"
	}
	image := strings.TrimSpace(spec.Image)
	if image == "" {
		image = "python:3.11-slim"
	}
	externalDNS := fmt.Sprintf("%s.%s.svc.cluster.local", routerSvc, routerNS)

	var b strings.Builder
	if spec.IncludeWorkload {
		fmt.Fprintf(&b, `apiVersion: v1
kind: Namespace
metadata:
  name: %s
  labels:
    app.kubernetes.io/part-of: f5-ai-gateway
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/name: %s
    app.kubernetes.io/part-of: f5-ai-gateway
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: %s
  template:
    metadata:
      labels:
        app.kubernetes.io/name: %s
    spec:
      containers:
        - name: vsr
          image: %s
          ports:
            - name: http
              containerPort: %d
              protocol: TCP
          env:
            - name: VSR_DEFAULT_MODEL
              value: %q
---
apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/name: %s
    app.kubernetes.io/part-of: f5-ai-gateway
spec:
  type: ClusterIP
  selector:
    app.kubernetes.io/name: %s
  ports:
    - name: http
      port: %d
      targetPort: %d
      protocol: TCP
---
`, routerNS, routerSvc, routerNS, routerSvc, routerSvc, routerSvc, image, port, defaultModel, routerSvc, routerNS, routerSvc, routerSvc, port, port)
	}

	fmt.Fprintf(&b, `apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/managed-by: bnk-forge
    app.kubernetes.io/component: semantic-router-bridge
spec:
  type: ExternalName
  externalName: %s
  ports:
    - name: http
      port: %d
      targetPort: %d
      protocol: TCP
`, extSvc, ctrlNS, externalDNS, port, port)

	return b.String()
}
