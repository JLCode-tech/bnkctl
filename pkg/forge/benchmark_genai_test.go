package forge_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/forge"
	"github.com/JLCode-tech/bnkctl/pkg/genai"
	"github.com/JLCode-tech/bnkctl/pkg/jumphost"
)

// forgeBenchmarkResultPush mirrors bnk-forge-v2 backend/schemas/benchmarks.py
// BenchmarkResultPush: required field → JSON type. Forge's pydantic model
// ignores unknown keys, so the GenAI fields are additive; this test pins that
// every required key is still present with the right type.
var forgeBenchmarkResultPush = map[string]string{
	"result_id": "string", "result_version": "string",
	"labels": "object", "tags": "object",
	"run_start": "string", "run_end": "string",
	"duration_seconds": "number", "duration_minutes": "number",
	"config":         "object",
	"total_requests": "number", "successful": "number", "failed": "number",
	"success_rate_pct": "number", "total_input_tokens": "number", "total_output_tokens": "number",
	"avg_input_tokens": "number", "avg_output_tokens": "number",
	"latency": "object", "throughput": "object", "phases": "object",
}

func jsonType(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case float64:
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case bool:
		return "bool"
	}
	return "null"
}

func TestMapAiperfResultToPayload_GenAIFieldsFlattened(t *testing.T) {
	result := sampleAiperfResult()
	g := &genai.Metrics{
		TTFTP50Ms: 100, TTFTP90Ms: 180, TTFTP95Ms: 190, TTFTP99Ms: 240,
		ITLP50Ms: 30, ITLP95Ms: 45, ITLP99Ms: 60,
		PrefixCacheHitRate: genai.Float(0.8), PrefixCacheSource: "vllm:prefix_cache_queries",
		InputTokensPerSec: 113.7, OutputTokensPerSec: 28.4, TotalTokensPerSec: 142.1,
		PrefillWorkerUtilization: genai.Float(0.4),
	}
	payload := forge.MapAiperfResultToPayload(result, forge.BenchmarkPushOptions{GenAI: g})

	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for k, want := range forgeBenchmarkResultPush {
		if got := jsonType(m[k]); got != want {
			t.Errorf("Forge contract: %s is %s, want %s", k, got, want)
		}
	}
	want := map[string]float64{
		"ttft_p50_ms": 100, "ttft_p90_ms": 180, "ttft_p95_ms": 190, "ttft_p99_ms": 240,
		"itl_p50_ms": 30, "itl_p95_ms": 45, "itl_p99_ms": 60,
		"prefix_cache_hit_rate": 0.8,
		"input_tokens_per_sec":  113.7, "output_tokens_per_sec": 28.4, "total_tokens_per_sec": 142.1,
		"prefill_worker_utilization": 0.4,
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("payload[%s] = %v, want %v", k, m[k], v)
		}
	}
	if _, ok := m["decode_worker_utilization"]; ok {
		t.Error("decode_worker_utilization must be omitted when not measured")
	}
	// Mirrors that today's Forge persists in result_json.
	am := m["aiperf_metrics"].(map[string]any)
	if gm, ok := am["genai"].(map[string]any); !ok || gm["prefix_cache_hit_rate"] != 0.8 {
		t.Errorf("aiperf_metrics.genai = %v", am["genai"])
	}
	if ttft := am["ttft"].(map[string]any); ttft["p95"] == nil {
		t.Error("aiperf_metrics.ttft.p95 missing")
	}
	th := m["throughput"].(map[string]any)
	if th["input_tokens_per_sec"] != 113.7 || th["total_tokens_per_sec"] != 142.1 || th["gen_tokens_per_sec"] != 28.4 {
		t.Errorf("throughput = %v", th)
	}
}

func TestMapAiperfResultToPayload_GenAIDerivedWhenAbsent(t *testing.T) {
	result := sampleAiperfResult()
	result.TTFT.P95 = 210
	payload := forge.MapAiperfResultToPayload(result, forge.BenchmarkPushOptions{})
	if payload.Metrics == nil {
		t.Fatal("GenAI metrics must always be set")
	}
	if payload.TTFTP50Ms != 110 || payload.TTFTP95Ms != 210 || payload.ITLP99Ms != 60 {
		t.Errorf("derived percentiles = %+v", payload.Metrics)
	}
	// 512 tokens × 20 requests / 90 s.
	if got := payload.InputTokensPerSec; got < 113.7 || got > 113.8 {
		t.Errorf("input_tokens_per_sec = %v", got)
	}
	if payload.OutputTokensPerSec != 28.4 || payload.PrefixCacheHitRate != nil {
		t.Errorf("derived = %+v", payload.Metrics)
	}

	// result.GenAI wins over derivation when set.
	result.GenAI = &genai.Metrics{TTFTP50Ms: 7}
	if p := forge.MapAiperfResultToPayload(result, forge.BenchmarkPushOptions{}); p.TTFTP50Ms != 7 {
		t.Errorf("result.GenAI not used: %v", p.TTFTP50Ms)
	}
}

func TestDeriveGenAIMetrics_PrefersRawArtifact(t *testing.T) {
	result := sampleAiperfResult()
	result.RawJSON = `{"time_to_first_token":{"p50":1,"p90":2,"p95":3,"p99":4},"inter_token_latency":{"p50":5,"p95":6,"p99":7},
	  "request_count":{"avg":2},"input_sequence_length":{"avg":10,"sum":20},"output_token_throughput":{"avg":8},"benchmark_duration":{"avg":4}}`
	m := forge.DeriveGenAIMetrics(result)
	if m.TTFTP95Ms != 3 || m.ITLP95Ms != 6 || m.InputTokensPerSec != 5 || m.OutputTokensPerSec != 8 || m.TotalTokensPerSec != 13 {
		t.Errorf("derived from raw = %+v", m)
	}
	if forge.DeriveGenAIMetrics(nil) == nil {
		t.Error("nil result must yield an empty metric set")
	}
}

func TestAIPerfResultPayload_MergesWithoutOverriding(t *testing.T) {
	raw := `{"benchmark_id":"abc","time_to_first_token":{"avg":195.84,"p50":175.05},"ttft_p50_ms":"aiperf-owned"}`
	body, err := forge.AIPerfResultPayload{Raw: json.RawMessage(raw), GenAI: &genai.Metrics{TTFTP50Ms: 175.05, PrefixCacheHitRate: genai.Float(0.25)}}.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m["benchmark_id"] != "abc" || m["time_to_first_token"].(map[string]any)["avg"] != 195.84 {
		t.Errorf("aiperf keys not preserved: %v", m)
	}
	if m["ttft_p50_ms"] != "aiperf-owned" {
		t.Errorf("GenAI field overrode an aiperf key: %v", m["ttft_p50_ms"])
	}
	if m["prefix_cache_hit_rate"] != 0.25 || m["ttft_p99_ms"] != 0.0 {
		t.Errorf("GenAI fields missing: %v", m)
	}
	if _, err := (forge.AIPerfResultPayload{Raw: json.RawMessage(`[1]`)}).MarshalJSON(); err == nil {
		t.Error("non-object raw must fail")
	}
}

func TestPushRawAiperfResult_MergesGenAIIntoBody(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			_, _ = io.WriteString(w, `{"token":"tok"}`)
		case forge.BenchmarkRawAiperfEndpoint:
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &gotBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":9,"run_id":9,"proxy":"f5-bnk","model":"llama3","status":"completed"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	resp, err := forge.PushRawAiperfResult(context.Background(), forge.RawAiperfPushOptions{
		RestURL: srv.URL,
		Creds:   forge.RestCreds{Username: "u", Password: "p"},
		RawJSON: []byte(`{"benchmark_id":"abc","request_latency":{"avg":1}}`),
		GenAI:   &genai.Metrics{TTFTP99Ms: 42, PrefixCacheHitRate: genai.Float(0.9)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.RunID != 9 {
		t.Errorf("run_id = %d", resp.RunID)
	}
	if gotBody["benchmark_id"] != "abc" || gotBody["ttft_p99_ms"] != 42.0 || gotBody["prefix_cache_hit_rate"] != 0.9 {
		t.Errorf("body = %v", gotBody)
	}
}

// Keep the jumphost import used even if the sample helper moves.
var _ = jumphost.AiperfResult{}
