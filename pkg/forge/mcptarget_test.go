package forge_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/forge"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/bnkscan"
)

func demoEndpoint() bnkscan.MCPEndpoint {
	return bnkscan.MCPEndpoint{
		Route: "mcp-financial-route", Namespace: "default",
		Gateway: "bnk-agentcore-demo-gateway", GatewayNamespace: "default", GatewayReady: true,
		Addresses: []string{"10.0.10.150"},
		Listeners: []string{"http:HTTP:80", "https:HTTPS:443"},
		Hostnames: []string{"bnk-ingress.bnk-demo.internal"},
		Paths:     []string{"/v1/mcp/forecast"},
		URLs:      []string{"http://10.0.10.150/v1/mcp/forecast", "https://10.0.10.150/v1/mcp/forecast"},
		Backends:  []string{"default/mcp-financial-tool:80"},
		Auth:      "bearer",
		IRules:    []string{"mcp-rate-limit-irule"},
		Persistence: &bnkscan.PersistenceInfo{Profile: "mcp-session", Namespace: "default",
			Type: bnkscan.PersistenceTypeMCP, Timeout: 600, Programmed: true, Listener: "http"},
		SecPolicies: []string{"mcp-sec-policy"},
		Tools: []bnkscan.ToolDef{
			{Name: "get_account_balance"},
			{Name: "forecast", Description: "30-day forecast", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
		Reason: "path /v1/mcp/forecast",
	}
}

func TestMCPTargetTags(t *testing.T) {
	tags := forge.MCPTargetTags(demoEndpoint(), "bnk-agentcore-demo")
	want := map[string]string{
		"source":                "azrbnkctl",
		"cloud_provider":        "azure",
		"protocol":              "mcp",
		"cluster":               "bnk-agentcore-demo",
		"route":                 "default/mcp-financial-route",
		"gateway":               "default/bnk-agentcore-demo-gateway",
		"auth":                  "bearer",
		"hostnames":             "bnk-ingress.bnk-demo.internal",
		"paths":                 "/v1/mcp/forecast",
		"irules":                "mcp-rate-limit-irule",
		"sec_policies":          "mcp-sec-policy",
		"persistence_profile":   "mcp-session",
		"persistence_type":      "MODEL_CONTEXT_PROTOCOL",
		"persistence_timeout_s": "600",
		"persistence_listener":  "http",
		"tools":                 "forecast,get_account_balance",
	}
	for k, v := range want {
		if tags[k] != v {
			t.Errorf("tags[%s] = %q, want %q", k, tags[k], v)
		}
	}
	var schemas []bnkscan.ToolDef
	if err := json.Unmarshal([]byte(tags["tool_schemas"]), &schemas); err != nil {
		t.Fatalf("tool_schemas: %v (%s)", err, tags["tool_schemas"])
	}
	if len(schemas) != 2 || schemas[0].Name != "forecast" || string(schemas[0].InputSchema) != `{"type":"object"}` {
		t.Errorf("tool_schemas = %+v", schemas)
	}

	// No persistence, no tools: the keys are absent rather than empty.
	ep := demoEndpoint()
	ep.Persistence, ep.Tools = nil, nil
	tags = forge.MCPTargetTags(ep, "")
	for _, k := range []string{"persistence_profile", "tools", "tool_schemas", "cluster"} {
		if _, ok := tags[k]; ok {
			t.Errorf("tags[%s] should be absent", k)
		}
	}
}

func TestRegisterMCPTargets_PostsAndUpserts(t *testing.T) {
	srv := newObjectGraphServer(0)
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	opts := forge.MCPTargetOptions{RestURL: ts.URL, Creds: forge.RestCreds{Username: "u", Password: "p"},
		ClusterID: 23, ClusterName: "bnk-agentcore-demo"}
	results := forge.RegisterMCPTargets(context.Background(), opts, []bnkscan.MCPEndpoint{demoEndpoint()})
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("results = %+v", results)
	}
	if results[0].Target.ID != 66 || results[0].Target.Name != "mcp-default-mcp-financial-route" {
		t.Errorf("target = %+v", results[0].Target)
	}

	var body map[string]any
	if err := json.Unmarshal(srv.capturedPosts[forge.BenchmarkTargetEndpoint], &body); err != nil {
		t.Fatal(err)
	}
	checks := map[string]any{
		"name":            "mcp-default-mcp-financial-route",
		"cluster_id":      float64(23),
		"llm_base_url":    "https://10.0.10.150/v1/mcp/forecast",
		"llm_model":       "mcp:mcp-financial-route",
		"llm_namespace":   "default",
		"llm_endpoint":    "mcp-financial-tool",
		"proxy_namespace": "f5-cne-system",
	}
	for k, v := range checks {
		if body[k] != v {
			t.Errorf("body[%s] = %v, want %v", k, body[k], v)
		}
	}
	tags, _ := body["tags"].(map[string]any)
	if tags["protocol"] != "mcp" || tags["persistence_type"] != "MODEL_CONTEXT_PROTOCOL" {
		t.Errorf("tags = %v", tags)
	}

	// Conflict → list and match by name.
	srv409 := newObjectGraphServer(http.StatusConflict)
	srv409.existingTargets = []map[string]any{{"id": 9, "name": "mcp-default-mcp-financial-route", "cluster_id": 23}}
	ts409 := httptest.NewServer(http.HandlerFunc(srv409.handler))
	defer ts409.Close()
	opts.RestURL = ts409.URL
	got, err := forge.RegisterMCPTarget(context.Background(), opts, demoEndpoint())
	if err != nil || got.ID != 9 {
		t.Errorf("upsert: id=%d err=%v", got.ID, err)
	}
}

func TestRegisterMCPTarget_Guards(t *testing.T) {
	_, err := forge.RegisterMCPTarget(context.Background(), forge.MCPTargetOptions{RestURL: "http://x"}, demoEndpoint())
	if err != forge.ErrTargetNoClusterID {
		t.Errorf("no cluster id: err = %v", err)
	}
	ep := demoEndpoint()
	ep.URLs = nil
	_, err = forge.RegisterMCPTarget(context.Background(), forge.MCPTargetOptions{RestURL: "http://x", ClusterID: 1}, ep)
	if err == nil || !strings.Contains(err.Error(), "has no URL") {
		t.Errorf("no url: err = %v", err)
	}
}

// A target of the same name that belongs to another cluster must not be
// reused: Forge keys names globally, so the endpoint is registered under a
// cluster-qualified name instead.
func TestRegisterMCPTarget_NameTakenByOtherCluster(t *testing.T) {
	srv := newObjectGraphServer(http.StatusConflict)
	srv.existingTargets = []map[string]any{{"id": 5, "name": "mcp-default-mcp-financial-route", "cluster_id": 18}}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	opts := forge.MCPTargetOptions{RestURL: ts.URL, Creds: forge.RestCreds{Username: "u", Password: "p"},
		ClusterID: 21, ClusterName: "agent-core-singapore"}
	got, err := forge.RegisterMCPTarget(context.Background(), opts, demoEndpoint())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if got.ID == 5 || got.Name != "mcp-default-mcp-financial-route-agent-core-singapore" || got.ClusterID != 21 {
		t.Errorf("got %+v, want a new target named mcp-default-mcp-financial-route-agent-core-singapore on cluster 21", got)
	}
	var body map[string]any
	if err := json.Unmarshal(srv.capturedPosts[forge.BenchmarkTargetEndpoint], &body); err != nil {
		t.Fatal(err)
	}
	if body["name"] != "mcp-default-mcp-financial-route-agent-core-singapore" || body["cluster_id"] != float64(21) {
		t.Errorf("second POST body = %v", body)
	}
}
