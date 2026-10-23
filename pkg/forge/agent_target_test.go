package forge_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/forge"
)

// ---------------------------------------------------------------------------
// Shared test server for agent / target / config endpoints
// ---------------------------------------------------------------------------

// objectGraphServer handles POST/GET for agents, targets, and configs so we
// can test the upsert (conflict-→-list-match) paths without a real forge.
type objectGraphServer struct {
	// postStatus controls the POST response status for the primary endpoint.
	postStatus int
	// existingAgents is the list returned by GET /api/benchmarks/agents.
	existingAgents []map[string]any
	// existingTargets is the list returned by GET /api/benchmarks/targets.
	existingTargets []map[string]any
	// existingConfigs is the list returned by GET /api/benchmarks/configs.
	existingConfigs []map[string]any
	// capturedPosts records raw body bytes per POST endpoint.
	capturedPosts map[string][]byte
	// rawAiperfCaptured records the raw body + query of POST /aiperf.
	rawAiperfCaptured struct {
		body        []byte
		queryParams map[string]string
	}
}

func newObjectGraphServer(postStatus int) *objectGraphServer {
	return &objectGraphServer{
		postStatus:    postStatus,
		capturedPosts: make(map[string][]byte),
	}
}

func (s *objectGraphServer) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch {
	// auth
	case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "graph-token"})

	// BenchmarkAgent
	case r.Method == http.MethodPost && r.URL.Path == forge.BenchmarkAgentEndpoint:
		raw, _ := io.ReadAll(r.Body)
		s.capturedPosts[r.URL.Path] = raw
		status := s.postStatus
		if status == 0 {
			status = http.StatusCreated
		}
		w.WriteHeader(status)
		if status == http.StatusCreated {
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":       55,
				"name":     body["name"],
				"hostname": body["hostname"],
			})
		}

	case r.Method == http.MethodGet && r.URL.Path == forge.BenchmarkAgentEndpoint:
		list := s.existingAgents
		if list == nil {
			list = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(list)

	// BenchmarkTarget
	case r.Method == http.MethodPost && r.URL.Path == forge.BenchmarkDiscoverTargetsEndpoint:
		raw, _ := io.ReadAll(r.Body)
		s.capturedPosts[r.URL.Path] = raw
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cluster_id":       2,
			"cluster_name":     "bnk-singapore",
			"discovered_count": 1,
			"discovered_services": []map[string]any{
				{
					"service_name": "vllm",
					"namespace":    "azrbnkctl-scn-aiinference",
					"base_url":     "http://vllm.azrbnkctl-scn-aiinference:80",
					"port":         80,
					"confidence":   "high",
				},
			},
			"created_targets": []map[string]any{
				{
					"id":           2,
					"name":         "vllm-azrbnkctl-scn-aiinference",
					"llm_base_url": "http://vllm.azrbnkctl-scn-aiinference:80",
					"status":       "created",
				},
			},
		})

	case r.Method == http.MethodPost && r.URL.Path == forge.BenchmarkTargetEndpoint:
		raw, _ := io.ReadAll(r.Body)
		s.capturedPosts[r.URL.Path] = raw
		status := s.postStatus
		if status == 0 {
			status = http.StatusCreated
		}
		// A 409 only applies to names Forge already holds (existingTargets),
		// so a retry under another name is created like the real backend does.
		if status == http.StatusConflict {
			var posted map[string]any
			_ = json.Unmarshal(raw, &posted)
			taken := false
			for _, t := range s.existingTargets {
				if t["name"] == posted["name"] {
					taken = true
				}
			}
			if !taken {
				status = http.StatusCreated
			}
		}
		w.WriteHeader(status)

		if status == http.StatusCreated {
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":         66,
				"name":       body["name"],
				"cluster_id": body["cluster_id"],
			})
		}

	case r.Method == http.MethodGet && r.URL.Path == forge.BenchmarkTargetEndpoint:
		// Forge returns a list-response object, NOT a bare array:
		// {"targets":[...],"total":N} per backend/routes/benchmarks.py:317.
		list := s.existingTargets
		if list == nil {
			list = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"targets": list,
			"total":   len(list),
		})

	// BenchmarkConfig
	case r.Method == http.MethodPost && r.URL.Path == forge.BenchmarkConfigEndpoint:
		raw, _ := io.ReadAll(r.Body)
		s.capturedPosts[r.URL.Path] = raw
		status := s.postStatus
		if status == 0 {
			status = http.StatusCreated
		}
		w.WriteHeader(status)
		if status == http.StatusCreated {
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":   77,
				"name": body["name"],
				"tool": "aiperf",
			})
		}

	case r.Method == http.MethodGet && r.URL.Path == forge.BenchmarkConfigEndpoint:
		list := s.existingConfigs
		if list == nil {
			list = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(list)

	// Raw aiperf push
	case r.Method == http.MethodPost && r.URL.Path == "/api/benchmarks/results/aiperf":
		raw, _ := io.ReadAll(r.Body)
		s.rawAiperfCaptured.body = raw
		s.rawAiperfCaptured.queryParams = make(map[string]string)
		for k, vs := range r.URL.Query() {
			if len(vs) > 0 {
				s.rawAiperfCaptured.queryParams[k] = vs[0]
			}
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     88,
			"run_id": 88,
			"proxy":  r.URL.Query().Get("proxy"),
			"model":  r.URL.Query().Get("model"),
			"status": "completed",
		})

	default:
		http.NotFound(w, r)
	}
}

// ---------------------------------------------------------------------------
// RegisterBenchmarkAgent tests
// ---------------------------------------------------------------------------

func TestRegisterBenchmarkAgent_PostsToCorrectPath(t *testing.T) {
	srv := newObjectGraphServer(0)
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	origDo := *forge.BenchmarkHTTPDoFn
	*forge.BenchmarkHTTPDoFn = ts.Client().Do
	defer func() { *forge.BenchmarkHTTPDoFn = origDo }()

	resp, err := forge.RegisterBenchmarkAgent(context.Background(), forge.BenchmarkAgentOptions{
		RestURL:      ts.URL,
		Name:         "azrbnkctl-jumphost-i-001",
		Hostname:     "i-001",
		IPAddress:    "10.0.11.50",
		Capabilities: []string{"aiperf"},
	})
	if err != nil {
		t.Fatalf("RegisterBenchmarkAgent: %v", err)
	}
	if resp.ID != 55 {
		t.Errorf("ID = %d, want 55", resp.ID)
	}
	if resp.Name != "azrbnkctl-jumphost-i-001" {
		t.Errorf("Name = %q, want %q", resp.Name, "azrbnkctl-jumphost-i-001")
	}
	// Verify the body sent to forge.
	var body map[string]any
	_ = json.Unmarshal(srv.capturedPosts[forge.BenchmarkAgentEndpoint], &body)
	if body["name"] != "azrbnkctl-jumphost-i-001" {
		t.Errorf("POST body name = %v, want azrbnkctl-jumphost-i-001", body["name"])
	}
	if body["ip_address"] != "10.0.11.50" {
		t.Errorf("POST body ip_address = %v, want 10.0.11.50", body["ip_address"])
	}
}

func TestRegisterBenchmarkAgent_409UpsertPath(t *testing.T) {
	srv := newObjectGraphServer(http.StatusConflict)
	srv.existingAgents = []map[string]any{
		{"id": float64(42), "name": "azrbnkctl-jumphost-i-001", "hostname": "i-001"},
	}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	origDo := *forge.BenchmarkHTTPDoFn
	*forge.BenchmarkHTTPDoFn = ts.Client().Do
	defer func() { *forge.BenchmarkHTTPDoFn = origDo }()

	resp, err := forge.RegisterBenchmarkAgent(context.Background(), forge.BenchmarkAgentOptions{
		RestURL:  ts.URL,
		Name:     "azrbnkctl-jumphost-i-001",
		Hostname: "i-001",
	})
	if err != nil {
		t.Fatalf("upsert path: %v", err)
	}
	if resp.ID != 42 {
		t.Errorf("upsert ID = %d, want 42 (from existing list)", resp.ID)
	}
}

// TestRegisterBenchmarkAgent_CapabilitiesIsDict asserts that RegisterBenchmarkAgent
// marshals the Capabilities field as a JSON object {"engines":[...]} rather than a
// bare array.  Forge's BenchmarkAgentRegister.capabilities is typed as dict
// (backend/schemas/benchmarks.py:209); a bare array causes HTTP 422.
func TestRegisterBenchmarkAgent_CapabilitiesIsDict(t *testing.T) {
	srv := newObjectGraphServer(0) // 201 on POST
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	origDo := *forge.BenchmarkHTTPDoFn
	*forge.BenchmarkHTTPDoFn = ts.Client().Do
	defer func() { *forge.BenchmarkHTTPDoFn = origDo }()

	_, err := forge.RegisterBenchmarkAgent(context.Background(), forge.BenchmarkAgentOptions{
		RestURL:      ts.URL,
		Name:         "agent-caps-test",
		Hostname:     "i-caps",
		Capabilities: []string{"aiperf"},
	})
	if err != nil {
		t.Fatalf("RegisterBenchmarkAgent: %v", err)
	}

	raw := srv.capturedPosts[forge.BenchmarkAgentEndpoint]
	if len(raw) == 0 {
		t.Fatal("no body captured for POST to agent endpoint")
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}

	// capabilities must be a JSON object (map), not an array.
	caps, ok := body["capabilities"]
	if !ok {
		t.Fatal("capabilities key missing from POST body")
	}
	capsMap, isMap := caps.(map[string]any)
	if !isMap {
		t.Fatalf("capabilities = %T, want map (dict); got %v", caps, caps)
	}
	// Must contain an "engines" key with the list value.
	engines, ok := capsMap["engines"]
	if !ok {
		t.Fatal(`capabilities["engines"] key missing`)
	}
	enginesList, isList := engines.([]any)
	if !isList || len(enginesList) == 0 {
		t.Fatalf(`capabilities["engines"] = %v, want non-empty list`, engines)
	}
	if enginesList[0] != "aiperf" {
		t.Errorf(`capabilities["engines"][0] = %v, want "aiperf"`, enginesList[0])
	}
}

func TestRegisterBenchmarkAgent_MissingRestURL(t *testing.T) {
	_, err := forge.RegisterBenchmarkAgent(context.Background(), forge.BenchmarkAgentOptions{Name: "x"})
	if err == nil {
		t.Error("expected error for empty RestURL")
	}
}

func TestRegisterBenchmarkAgent_MissingName(t *testing.T) {
	_, err := forge.RegisterBenchmarkAgent(context.Background(), forge.BenchmarkAgentOptions{RestURL: "http://localhost"})
	if err == nil {
		t.Error("expected error for empty Name")
	}
}

// ---------------------------------------------------------------------------
// RegisterBenchmarkTarget tests
// ---------------------------------------------------------------------------

func TestRegisterBenchmarkTarget_PostsToCorrectPath(t *testing.T) {
	srv := newObjectGraphServer(0)
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	origDo := *forge.BenchmarkHTTPDoFn
	*forge.BenchmarkHTTPDoFn = ts.Client().Do
	defer func() { *forge.BenchmarkHTTPDoFn = origDo }()

	resp, err := forge.RegisterBenchmarkTarget(context.Background(), forge.BenchmarkTargetOptions{
		RestURL:    ts.URL,
		Name:       "azrbnkctl-ai-rig-llama3",
		ClusterID:  7,
		LLMBaseURL: "http://10.0.10.100",
		LLMModel:   "llama3",
	})
	if err != nil {
		t.Fatalf("RegisterBenchmarkTarget: %v", err)
	}
	if resp.ID != 66 {
		t.Errorf("ID = %d, want 66", resp.ID)
	}
	// Verify body fields.
	var body map[string]any
	_ = json.Unmarshal(srv.capturedPosts[forge.BenchmarkTargetEndpoint], &body)
	if body["name"] != "azrbnkctl-ai-rig-llama3" {
		t.Errorf("POST body name = %v", body["name"])
	}
	if cid, ok := body["cluster_id"].(float64); !ok || int(cid) != 7 {
		t.Errorf("POST body cluster_id = %v, want 7", body["cluster_id"])
	}
}

func TestRegisterBenchmarkTarget_NoClusterID(t *testing.T) {
	_, err := forge.RegisterBenchmarkTarget(context.Background(), forge.BenchmarkTargetOptions{
		RestURL: "http://localhost",
		Name:    "target-x",
		// ClusterID intentionally zero
	})
	if err == nil {
		t.Fatal("expected ErrTargetNoClusterID, got nil")
	}
	if !strings.Contains(err.Error(), "ClusterID") {
		t.Errorf("error %q should mention ClusterID", err.Error())
	}
}

func TestRegisterBenchmarkTarget_409UpsertPath(t *testing.T) {
	srv := newObjectGraphServer(http.StatusConflict)
	srv.existingTargets = []map[string]any{
		{"id": float64(33), "name": "azrbnkctl-ai-rig-llama3", "cluster_id": float64(7)},
	}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	origDo := *forge.BenchmarkHTTPDoFn
	*forge.BenchmarkHTTPDoFn = ts.Client().Do
	defer func() { *forge.BenchmarkHTTPDoFn = origDo }()

	resp, err := forge.RegisterBenchmarkTarget(context.Background(), forge.BenchmarkTargetOptions{
		RestURL:   ts.URL,
		Name:      "azrbnkctl-ai-rig-llama3",
		ClusterID: 7,
	})
	if err != nil {
		t.Fatalf("upsert path: %v", err)
	}
	if resp.ID != 33 {
		t.Errorf("upsert ID = %d, want 33", resp.ID)
	}
}

// TestRegisterBenchmarkTarget_ListResponseObjectShape verifies that
// benchmarkTargetFindByName decodes the {"targets":[...],"total":N} object
// correctly. Forge's GET /api/benchmarks/targets returns a list-response object
// (backend/routes/benchmarks.py:317), NOT a bare array — the prior bare-array
// decode silently produced an empty list, so the 409 fallback never resolved the
// existing target.
func TestRegisterBenchmarkTarget_ListResponseObjectShape(t *testing.T) {
	srv := newObjectGraphServer(http.StatusConflict)
	// existingTargets is served as {"targets":[...],"total":N} by the handler.
	srv.existingTargets = []map[string]any{
		{"id": float64(99), "name": "azrbnkctl-ai-rig-llama3", "cluster_id": float64(5)},
	}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	origDo := *forge.BenchmarkHTTPDoFn
	*forge.BenchmarkHTTPDoFn = ts.Client().Do
	defer func() { *forge.BenchmarkHTTPDoFn = origDo }()

	resp, err := forge.RegisterBenchmarkTarget(context.Background(), forge.BenchmarkTargetOptions{
		RestURL:   ts.URL,
		Name:      "azrbnkctl-ai-rig-llama3",
		ClusterID: 5,
	})
	if err != nil {
		t.Fatalf("409 conflict + list-response decode: %v", err)
	}
	// ID 99 comes from the list-response object; 0 would indicate the old
	// bare-array decode silently returned an empty list (bug not fixed).
	if resp.ID != 99 {
		t.Errorf("resolved target ID = %d, want 99 (bare-array decode would return 0)", resp.ID)
	}
}

// ---------------------------------------------------------------------------
// RegisterBenchmarkConfig conflict/idempotent path
// ---------------------------------------------------------------------------

func TestRegisterBenchmarkConfig_409UpsertPath(t *testing.T) {
	srv := newObjectGraphServer(http.StatusConflict)
	srv.existingConfigs = []map[string]any{
		{"id": float64(44), "name": "azrbnkctl-latency", "tool": "aiperf"},
	}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	origDo := *forge.BenchmarkHTTPDoFn
	*forge.BenchmarkHTTPDoFn = ts.Client().Do
	defer func() { *forge.BenchmarkHTTPDoFn = origDo }()

	resp, err := forge.RegisterBenchmarkConfig(context.Background(), forge.BenchmarkConfigOptions{
		RestURL:    ts.URL,
		Name:       "azrbnkctl-latency",
		ConfigJSON: map[string]any{"concurrency": 1},
	})
	if err != nil {
		t.Fatalf("upsert path: %v", err)
	}
	if resp.ID != 44 {
		t.Errorf("upsert ID = %d, want 44 (from existing list)", resp.ID)
	}
}

func TestRegisterBenchmarkConfig_CreateSuccess(t *testing.T) {
	srv := newObjectGraphServer(0) // 201 on POST
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	origDo := *forge.BenchmarkHTTPDoFn
	*forge.BenchmarkHTTPDoFn = ts.Client().Do
	defer func() { *forge.BenchmarkHTTPDoFn = origDo }()

	resp, err := forge.RegisterBenchmarkConfig(context.Background(), forge.BenchmarkConfigOptions{
		RestURL:    ts.URL,
		Name:       "azrbnkctl-throughput",
		ConfigJSON: map[string]any{"concurrency": 8},
	})
	if err != nil {
		t.Fatalf("RegisterBenchmarkConfig: %v", err)
	}
	if resp.ID != 77 {
		t.Errorf("ID = %d, want 77", resp.ID)
	}
	if resp.Name != "azrbnkctl-throughput" {
		t.Errorf("Name = %q, want azrbnkctl-throughput", resp.Name)
	}
}

// ---------------------------------------------------------------------------
// PushRawAiperfResult tests
// ---------------------------------------------------------------------------

const sampleRawAiperfJSON = `{"schema_version":"1.3","aiperf_version":"0.10.0","benchmark_id":"abc123","request_throughput":{"avg":0.5},"request_latency":{"avg":2000.0},"request_count":{"avg":10.0}}`

func TestPushRawAiperfResult_PostsRawJSONToAiperfEndpoint(t *testing.T) {
	srv := newObjectGraphServer(0)
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	origDo := *forge.BenchmarkHTTPDoFn
	*forge.BenchmarkHTTPDoFn = ts.Client().Do
	defer func() { *forge.BenchmarkHTTPDoFn = origDo }()

	resp, err := forge.PushRawAiperfResult(context.Background(), forge.RawAiperfPushOptions{
		RestURL:   ts.URL,
		RawJSON:   []byte(sampleRawAiperfJSON),
		Proxy:     "f5-bnk",
		Model:     "llama3",
		URL:       "http://10.0.10.100",
		AgentName: "azrbnkctl-jumphost-i-001",
		RunLabel:  "ci-test",
		TargetID:  66,
		ConfigID:  77,
	})
	if err != nil {
		t.Fatalf("PushRawAiperfResult: %v", err)
	}
	if resp.ID != 88 {
		t.Errorf("ID = %d, want 88", resp.ID)
	}

	// Verify the raw body is forwarded verbatim.
	if string(srv.rawAiperfCaptured.body) != sampleRawAiperfJSON {
		t.Errorf("raw body = %q, want verbatim aiperf JSON", srv.rawAiperfCaptured.body)
	}

	// Verify query parameters.
	q := srv.rawAiperfCaptured.queryParams
	checks := map[string]string{
		"proxy":      "f5-bnk",
		"model":      "llama3",
		"url":        "http://10.0.10.100",
		"agent_name": "azrbnkctl-jumphost-i-001",
		"run_label":  "ci-test",
		"target_id":  "66",
		"config_id":  "77",
	}
	for k, want := range checks {
		if got := q[k]; got != want {
			t.Errorf("query param %q = %q, want %q", k, got, want)
		}
	}
}

func TestPushRawAiperfResult_DefaultProxy(t *testing.T) {
	srv := newObjectGraphServer(0)
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	origDo := *forge.BenchmarkHTTPDoFn
	*forge.BenchmarkHTTPDoFn = ts.Client().Do
	defer func() { *forge.BenchmarkHTTPDoFn = origDo }()

	_, err := forge.PushRawAiperfResult(context.Background(), forge.RawAiperfPushOptions{
		RestURL: ts.URL,
		RawJSON: []byte(sampleRawAiperfJSON),
		// Proxy intentionally empty
	})
	if err != nil {
		t.Fatalf("PushRawAiperfResult: %v", err)
	}
	if got := srv.rawAiperfCaptured.queryParams["proxy"]; got != "f5-bnk" {
		t.Errorf("default proxy = %q, want %q", got, "f5-bnk")
	}
}

func TestPushRawAiperfResult_ZeroIDsOmittedFromQuery(t *testing.T) {
	srv := newObjectGraphServer(0)
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	origDo := *forge.BenchmarkHTTPDoFn
	*forge.BenchmarkHTTPDoFn = ts.Client().Do
	defer func() { *forge.BenchmarkHTTPDoFn = origDo }()

	_, err := forge.PushRawAiperfResult(context.Background(), forge.RawAiperfPushOptions{
		RestURL: ts.URL,
		RawJSON: []byte(sampleRawAiperfJSON),
		// TargetID, ConfigID, ProxyDeploymentID all zero
	})
	if err != nil {
		t.Fatalf("PushRawAiperfResult: %v", err)
	}
	q := srv.rawAiperfCaptured.queryParams
	for _, param := range []string{"target_id", "config_id", "proxy_deployment_id"} {
		if _, ok := q[param]; ok {
			t.Errorf("query should NOT contain %q when value is zero, got %q", param, q[param])
		}
	}
}

func TestPushRawAiperfResult_MissingRestURL(t *testing.T) {
	_, err := forge.PushRawAiperfResult(context.Background(), forge.RawAiperfPushOptions{
		RawJSON: []byte(sampleRawAiperfJSON),
	})
	if err == nil {
		t.Error("expected error for empty RestURL")
	}
}

func TestPushRawAiperfResult_MissingRawJSON(t *testing.T) {
	_, err := forge.PushRawAiperfResult(context.Background(), forge.RawAiperfPushOptions{
		RestURL: "http://localhost",
	})
	if err == nil {
		t.Error("expected error for empty RawJSON")
	}
}

// ---------------------------------------------------------------------------
// DiscoverTargets & ListBenchmarkTargets tests
// ---------------------------------------------------------------------------

func TestDiscoverTargets_Success(t *testing.T) {
	srv := newObjectGraphServer(0)
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	origDo := *forge.BenchmarkHTTPDoFn
	*forge.BenchmarkHTTPDoFn = ts.Client().Do
	defer func() { *forge.BenchmarkHTTPDoFn = origDo }()

	resp, err := forge.DiscoverTargets(context.Background(), forge.DiscoverTargetsOptions{
		RestURL:    ts.URL,
		ClusterID:  2,
		AutoCreate: true,
	})
	if err != nil {
		t.Fatalf("DiscoverTargets: %v", err)
	}
	if resp.ClusterID != 2 {
		t.Errorf("ClusterID = %d, want 2", resp.ClusterID)
	}
	if resp.DiscoveredCount != 1 {
		t.Errorf("DiscoveredCount = %d, want 1", resp.DiscoveredCount)
	}
	if len(resp.CreatedTargets) != 1 || resp.CreatedTargets[0].Name != "vllm-azrbnkctl-scn-aiinference" {
		t.Errorf("CreatedTargets = %+v", resp.CreatedTargets)
	}
}

func TestListBenchmarkTargets_Success(t *testing.T) {
	srv := newObjectGraphServer(0)
	srv.existingTargets = []map[string]any{
		{
			"id":            2,
			"name":          "vllm-azrbnkctl-scn-aiinference",
			"cluster_id":    2,
			"llm_base_url":  "http://vllm.azrbnkctl-scn-aiinference:80",
			"llm_model":     "meta-llama/Llama-3.1-8B-Instruct",
			"llm_namespace": "azrbnkctl-scn-aiinference",
			"proxy_count":   1,
		},
	}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	origDo := *forge.BenchmarkHTTPDoFn
	*forge.BenchmarkHTTPDoFn = ts.Client().Do
	defer func() { *forge.BenchmarkHTTPDoFn = origDo }()

	targets, err := forge.ListBenchmarkTargets(context.Background(), forge.ListBenchmarkTargetsOptions{
		RestURL:   ts.URL,
		ClusterID: 2,
	})
	if err != nil {
		t.Fatalf("ListBenchmarkTargets: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(targets))
	}
	if targets[0].ID != 2 || targets[0].Name != "vllm-azrbnkctl-scn-aiinference" {
		t.Errorf("targets[0] = %+v", targets[0])
	}
}
