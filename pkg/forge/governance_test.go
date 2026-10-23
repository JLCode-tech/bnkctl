package forge_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/forge"
)

func TestGovernanceClientAndF5InferenceScan(t *testing.T) {
	scanJSON := `{
	  "cluster_id": 23,
	  "cluster_name": "agentcore-demo",
	  "bnk_install": {
	    "status": "installed",
	    "crds": {
	      "groups": ["gateway.k8s.f5.com", "inference.k8s.f5.com", "k8s.f5.com", "k8s.f5net.com"],
	      "has_f5_inference": true
	    },
	    "cne_instance": {"manifest_version": "2.4.1-3.2850.1-0.0.9"}
	  }
	}`
	sr, err := forge.ParseScanResult(scanJSON)
	if err != nil {
		t.Fatalf("ParseScanResult: %v", err)
	}
	if !sr.Indexes24() || !sr.IndexesF5Inference() {
		t.Fatalf("expected Indexes24 and IndexesF5Inference to be true: %+v", sr)
	}
	if sr.ManifestVersion() != "2.4.1-3.2850.1-0.0.9" {
		t.Fatalf("unexpected manifest version %q", sr.ManifestVersion())
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/auth/login" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "gov-jwt-token"})
		case r.URL.Path == "/api/k8s/clusters/23/llm-observability/governance/overview" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(forge.GovernanceOverviewResponse{
				Available:       true,
				ClusterID:       23,
				Namespace:       "default",
				Tenants:         []map[string]any{{"name": "default-tenant"}},
				Providers:       []map[string]any{{"name": "bedrock-ap-southeast-1"}},
				Models:          []map[string]any{{"name": "claude-sonnet-4-5"}},
				Routes:          []map[string]any{{"name": "llm-chat-route"}},
				Orgs:            []map[string]any{{"id": "org-123", "name": "f5-org"}},
				Teams:           []map[string]any{{"id": "team-123", "name": "platform-ai"}},
				Principals:      []map[string]any{{"id": "prin-123", "name": "demo-agent"}},
				ReadinessChecks: []map[string]any{{"id": "dns_resolver", "ok": true}},
				VirtualKeys: []map[string]any{
					{"id": "vk-1", "name": "demo-key", "masked_key": "f5vk_demo..."},
				},
				TopologyNodes: []map[string]any{{"id": "tenant:default-tenant"}, {"id": "model:claude-sonnet-4-5"}},
				TopologyEdges: []map[string]any{{"source": "tenant:default-tenant", "target": "model:claude-sonnet-4-5"}},
			})
		case r.URL.Path == "/api/k8s/clusters/23/llm-observability/governance/remediate" && r.Method == http.MethodPost:
			var req forge.GovernanceRemediateRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.NewEncoder(w).Encode(forge.GovernanceRemediateResponse{
				Status:  "ok",
				Success: true,
				Action:  req.Action,
				Message: "patched CNEInstance and restarted f5-cne-controller",
			})
		case r.URL.Path == "/api/k8s/clusters/23/llm-observability/governance/test-request" && r.Method == http.MethodPost:
			var req forge.GovernanceTestRequestPayload
			_ = json.NewDecoder(r.Body).Decode(&req)
			modelUsed := req.ModelID
			if modelUsed == "" {
				modelUsed = req.Model
			}
			_ = json.NewEncoder(w).Encode(forge.GovernanceTestRequestResponse{
				StatusCode:          200,
				Success:             true,
				RouteName:           req.RouteName,
				SelectedProvider:    "bedrock-provider",
				SemanticCacheStatus: "MISS",
				LatencyMS:           412.5,
				Model:               modelUsed,
				PromptTokens:        18,
				CompletionTokens:    42,
				TotalTokens:         60,
				AuthVerified:        true,
				ResponseText:        "Live inference via " + modelUsed,
				ResponseBody: map[string]any{
					"id":     "chatcmpl-123",
					"object": "chat.completion",
					"model":  modelUsed,
				},
			})
		case r.URL.Path == "/api/projects" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"projects": []map[string]any{
					{"id": 11, "name": "azrbnkctl-agentfoundry-demo"},
				},
			})
		case r.URL.Path == "/api/projects/11/k8s/clusters" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"clusters": []map[string]any{
					{"id": 23, "name": "agentfoundry-demo", "project_id": 11},
				},
			})
		case r.URL.Path == "/api/k8s/clusters/23/llm-observability/governance/tenants" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(forge.GovernanceMutationResponse{Status: "created", Success: true, Action: "create_tenant", Name: "default-tenant"})
		case r.URL.Path == "/api/k8s/clusters/23/llm-observability/governance/providers" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(forge.GovernanceMutationResponse{Status: "created", Success: true, Action: "create_provider", Name: "bedrock-provider"})
		case r.URL.Path == "/api/k8s/clusters/23/llm-observability/governance/models" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(forge.GovernanceMutationResponse{Status: "created", Success: true, Action: "create_model", Name: "claude-sonnet-4-5"})
		case r.URL.Path == "/api/k8s/clusters/23/llm-observability/governance/routes" && r.Method == http.MethodPost:
			var req forge.GovernanceRouteCreateRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.SemanticRouterService != "vsr-service" || req.SemanticRouterNamespace != "vllm-semantic-router-system" || req.SemanticRouterPort != 8000 || req.SemanticRouterTimeout != "5s" {
				http.Error(w, "unexpected semantic router fields in create_route", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(forge.GovernanceMutationResponse{Status: "created", Success: true, Action: "create_route", Name: "llm-chat-route"})
		case r.URL.Path == "/api/k8s/clusters/23/llm-observability/governance/routes/llm-chat-route/policies" && r.Method == http.MethodPatch:
			var req forge.GovernanceRoutePoliciesUpdateRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.SemanticRouterService != "vsr-service" || req.SemanticRouterNamespace != "vllm-semantic-router-system" || req.SemanticRouterPort != 8000 || req.SemanticRouterTimeout != "5s" {
				http.Error(w, "unexpected semantic router fields in update_route_policies", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(forge.GovernanceMutationResponse{Status: "updated", Success: true, Action: "update_route_policies", Name: "llm-chat-route"})
		case r.URL.Path == "/api/k8s/clusters/23/llm-observability/governance/orgs" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(forge.GovernanceMutationResponse{Status: "created", Success: true, ID: "org-123", Name: "f5-org"})
		case r.URL.Path == "/api/k8s/clusters/23/llm-observability/governance/teams" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(forge.GovernanceMutationResponse{Status: "created", Success: true, ID: "team-123", Name: "platform-ai"})
		case r.URL.Path == "/api/k8s/clusters/23/llm-observability/governance/principals" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(forge.GovernanceMutationResponse{Status: "created", Success: true, ID: "prin-123", Name: "demo-agent"})
		case r.URL.Path == "/api/k8s/clusters/23/llm-observability/governance/budgets" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(forge.GovernanceMutationResponse{Status: "created", Success: true, ID: "budget-123", Name: "demo-budget"})
		case r.URL.Path == "/api/k8s/clusters/23/llm-observability/governance/ratelimits" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(forge.GovernanceMutationResponse{Status: "created", Success: true, ID: "rl-123", Name: "demo-rl"})
		case r.URL.Path == "/api/k8s/clusters/23/llm-observability/governance/virtual-keys" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(forge.GovernanceVirtualKeyCreateResponse{
				Status:       "created",
				Success:      true,
				KeyID:        "vk-123",
				Name:         "demo-key",
				MaskedKey:    "f5vk_demo...",
				CleartextKey: "f5vk_demo_secret_token_98765",
				PrincipalID:  "prin-123",
			})
		case r.URL.Path == "/api/k8s/clusters/23/llm-observability/governance/virtual-keys/vk-123/rotate" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(forge.GovernanceVirtualKeyCreateResponse{
				Status:       "rotated",
				Success:      true,
				KeyID:        "vk-123",
				Name:         "demo-key",
				MaskedKey:    "f5vk_rot...",
				CleartextKey: "f5vk_rot_secret_token_54321",
				PrincipalID:  "prin-123",
			})
		case r.URL.Path == "/api/k8s/clusters/23/llm-observability/governance/virtual-keys/vk-123" && r.Method == http.MethodDelete:
			_ = json.NewEncoder(w).Encode(forge.GovernanceMutationResponse{Status: "revoked", Success: true, ID: "vk-123"})
		default:
			http.Error(w, "not found: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	creds := forge.RestCreds{Username: "admin", Password: "changeme"}

	resolvedID, err := forge.ResolveClusterIDByName(ctx, srv.URL, "agentfoundry-demo", creds)
	if err != nil || resolvedID != 23 {
		t.Fatalf("ResolveClusterIDByName = %d, %v; want 23, nil", resolvedID, err)
	}

	if _, err := forge.GovernanceCreateTenant(ctx, srv.URL, 23, forge.GovernanceTenantCreateRequest{Name: "default-tenant", DisplayName: "Default Tenant"}, creds); err != nil {
		t.Fatalf("GovernanceCreateTenant: %v", err)
	}
	if _, err := forge.GovernanceCreateProvider(ctx, srv.URL, 23, forge.GovernanceProviderCreateRequest{Name: "bedrock-provider", ProviderType: "bedrock", SecretName: "bedrock-api-secret"}, creds); err != nil {
		t.Fatalf("GovernanceCreateProvider: %v", err)
	}
	if _, err := forge.GovernanceCreateModel(ctx, srv.URL, 23, forge.GovernanceModelCreateRequest{
		Name:    "claude-sonnet-4-5",
		ModelID: "anthropic.claude-sonnet-4-5-20250929-v1:0",
		Deployments: []forge.GovernanceModelDeploymentSpec{
			{Name: "primary", Provider: "bedrock-provider", Upstream: "anthropic.claude-sonnet-4-5-20250929-v1:0"},
		},
	}, creds); err != nil {
		t.Fatalf("GovernanceCreateModel: %v", err)
	}
	modelRouting := true
	if _, err := forge.GovernanceCreateRoute(ctx, srv.URL, 23, forge.GovernanceRouteCreateRequest{
		Name:                    "llm-chat-route",
		ModelRoutingEnabled:     &modelRouting,
		SemanticRouterService:   "vsr-service",
		SemanticRouterNamespace: "vllm-semantic-router-system",
		SemanticRouterPort:      8000,
		SemanticRouterTimeout:   "5s",
		DefaultModelRef:         "claude-sonnet-4-5",
	}, creds); err != nil {
		t.Fatalf("GovernanceCreateRoute: %v", err)
	}
	semCache := true
	if _, err := forge.GovernanceUpdateRoutePolicies(ctx, srv.URL, 23, "llm-chat-route", forge.GovernanceRoutePoliciesUpdateRequest{
		RouteName:               "llm-chat-route",
		SemanticCacheEnabled:    &semCache,
		ModelRoutingEnabled:     &modelRouting,
		SemanticRouterService:   "vsr-service",
		SemanticRouterNamespace: "vllm-semantic-router-system",
		SemanticRouterPort:      8000,
		SemanticRouterTimeout:   "5s",
	}, creds); err != nil {
		t.Fatalf("GovernanceUpdateRoutePolicies: %v", err)
	}

	orgRes, err := forge.GovernanceCreateOrg(ctx, srv.URL, 23, forge.GovernanceOrgCreateRequest{Tenant: "default-tenant", Name: "f5-org", MonthlyBudgetUSD: 500}, creds)
	if err != nil || orgRes.ID != "org-123" {
		t.Fatalf("GovernanceCreateOrg: %+v, %v", orgRes, err)
	}
	teamRes, err := forge.GovernanceCreateTeam(ctx, srv.URL, 23, forge.GovernanceTeamCreateRequest{Tenant: "default-tenant", OrgID: orgRes.ID, Name: "platform-ai"}, creds)
	if err != nil || teamRes.ID != "team-123" {
		t.Fatalf("GovernanceCreateTeam: %+v, %v", teamRes, err)
	}
	prinRes, err := forge.GovernanceCreatePrincipal(ctx, srv.URL, 23, forge.GovernancePrincipalCreateRequest{Tenant: "default-tenant", TeamID: teamRes.ID, Name: "demo-agent"}, creds)
	if err != nil || prinRes.ID != "prin-123" {
		t.Fatalf("GovernanceCreatePrincipal: %+v, %v", prinRes, err)
	}
	if _, err := forge.GovernanceCreateBudget(ctx, srv.URL, 23, forge.GovernanceBudgetCreateRequest{Tenant: "default-tenant", Name: "demo-budget", TargetID: orgRes.ID, LimitUSD: 500}, creds); err != nil {
		t.Fatalf("GovernanceCreateBudget: %v", err)
	}
	if _, err := forge.GovernanceCreateRateLimit(ctx, srv.URL, 23, forge.GovernanceRateLimitCreateRequest{Tenant: "default-tenant", Name: "demo-rl", TargetID: prinRes.ID, RequestsPerMinute: 60, TokensPerMinute: 100000}, creds); err != nil {
		t.Fatalf("GovernanceCreateRateLimit: %v", err)
	}

	vk, err := forge.GovernanceCreateVirtualKey(ctx, srv.URL, 23, forge.GovernanceVirtualKeyCreateRequest{
		Tenant:      "default-tenant",
		PrincipalID: prinRes.ID,
		Name:        "demo-key",
	}, creds)
	if err != nil || !vk.Success || !strings.HasPrefix(vk.CleartextKey, "f5vk_") || vk.APIKey != vk.CleartextKey {
		t.Fatalf("GovernanceCreateVirtualKey = %+v, %v", vk, err)
	}

	rotVK, err := forge.GovernanceRotateVirtualKey(ctx, srv.URL, 23, vk.KeyID, creds)
	if err != nil || !strings.HasPrefix(rotVK.CleartextKey, "f5vk_rot") {
		t.Fatalf("GovernanceRotateVirtualKey = %+v, %v", rotVK, err)
	}
	if _, err := forge.GovernanceRevokeVirtualKey(ctx, srv.URL, 23, vk.KeyID, creds); err != nil {
		t.Fatalf("GovernanceRevokeVirtualKey: %v", err)
	}

	ov, err := forge.GovernanceOverview(ctx, srv.URL, 23, creds)
	if err != nil {
		t.Fatalf("GovernanceOverview: %v", err)
	}
	if !ov.Available || len(ov.Tenants) != 1 || len(ov.TopologyNodes) != 2 || !strings.Contains(ov.Summary(), "available=true") {
		t.Errorf("unexpected overview: %+v (%s)", ov, ov.Summary())
	}

	rem, err := forge.GovernanceRemediate(ctx, srv.URL, 23, forge.GovernanceRemediateRequest{
		Action:    "restart_controller",
		Namespace: "f5-bnk",
	}, creds)
	if err != nil {
		t.Fatalf("GovernanceRemediate: %v", err)
	}
	if !rem.Success || rem.Action != "restart_controller" || !strings.Contains(rem.Message, "restarted f5-cne-controller") {
		t.Errorf("unexpected remediate response: %+v", rem)
	}

	tr, err := forge.GovernanceTestRequest(ctx, srv.URL, 23, forge.GovernanceTestRequestPayload{
		RouteName:  "llm-chat-route",
		VirtualKey: vk.CleartextKey,
		ModelID:    "anthropic.claude-sonnet-4-5-20250929-v1:0",
		Prompt:     "Explain BNK 2.4.1 token accounting.",
	}, creds)
	if err != nil {
		t.Fatalf("GovernanceTestRequest: %v", err)
	}
	if !tr.Success || tr.TotalTokens != 60 || !strings.Contains(tr.Summary(), "status=ok http=200") || !strings.Contains(tr.ResponseBodyString(), "chatcmpl-123") {
		t.Errorf("unexpected test-request response: %+v (%s, body=%s)", tr, tr.Summary(), tr.ResponseBodyString())
	}

	// Edge cases: invalid clusterID or empty action
	if _, err := forge.GovernanceOverview(ctx, srv.URL, 0, creds); err == nil {
		t.Errorf("expected error for clusterID=0 in GovernanceOverview")
	}
	if _, err := forge.GovernanceRemediate(ctx, srv.URL, 23, forge.GovernanceRemediateRequest{}, creds); err == nil {
		t.Errorf("expected error for empty action in GovernanceRemediate")
	}
}

func TestRenderVLLMSemanticRouterManifestAndBNK241Constants(t *testing.T) {
	if len(forge.SupportedAIProviderTypes) != 7 {
		t.Fatalf("expected 7 SupportedAIProviderTypes, got %d: %v", len(forge.SupportedAIProviderTypes), forge.SupportedAIProviderTypes)
	}
	if len(forge.SupportedAIModelDialects) != 2 {
		t.Fatalf("expected 2 SupportedAIModelDialects, got %d: %v", len(forge.SupportedAIModelDialects), forge.SupportedAIModelDialects)
	}

	manifest := forge.RenderVLLMSemanticRouterManifest(forge.GovernanceSemanticRouterSpec{
		IncludeWorkload: true,
	})
	for _, want := range []string{
		"name: vllm-semantic-router-system",
		"name: vsr-service",
		"name: vllmsr",
		"namespace: f5-cne-system",
		"type: ExternalName",
		"externalName: vsr-service.vllm-semantic-router-system.svc.cluster.local",
		"port: 8000",
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("expected manifest to contain %q, got:\n%s", want, manifest)
		}
	}

	bridgeOnly := forge.RenderVLLMSemanticRouterManifest(forge.GovernanceSemanticRouterSpec{
		ControllerNamespace: "f5-bnk",
		ExternalServiceName: "vllmsr",
		RouterNamespace:     "custom-vsr-ns",
		RouterServiceName:   "custom-vsr-svc",
		Port:                8080,
		IncludeWorkload:     false,
	})
	if strings.Contains(bridgeOnly, "kind: Deployment") {
		t.Errorf("did not expect Deployment when IncludeWorkload=false:\n%s", bridgeOnly)
	}
	if !strings.Contains(bridgeOnly, "externalName: custom-vsr-svc.custom-vsr-ns.svc.cluster.local") {
		t.Errorf("expected custom externalName in bridge-only manifest:\n%s", bridgeOnly)
	}
}
