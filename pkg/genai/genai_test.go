package genai_test

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/genai"
)

const artifact = `{
  "schema_version": "1.3",
  "request_throughput": {"unit": "requests/sec", "avg": 0.8233},
  "request_latency": {"unit": "ms", "avg": 2413.5, "p50": 2431.75, "p90": 2438.5, "p95": 2439.0, "p99": 2439.45},
  "request_count": {"unit": "requests", "avg": 10.0},
  "time_to_first_token": {"unit": "ms", "avg": 195.84, "p50": 175.05, "p90": 251.45, "p95": 252.9, "p99": 253.45, "count": 10, "sum": 1958.4},
  "inter_token_latency": {"unit": "ms", "avg": 35.2, "p50": 34.73, "p90": 35.95, "p95": 35.955, "p99": 35.96},
  "output_token_throughput": {"unit": "tokens/sec", "avg": 52.69},
  "output_sequence_length": {"unit": "tokens", "avg": 64.0, "count": 10, "sum": 640.0},
  "input_sequence_length":  {"unit": "tokens", "avg": 256.0, "count": 10, "sum": 2560.0},
  "total_output_tokens":    {"unit": "tokens", "avg": 640.0},
  "benchmark_duration":     {"unit": "sec", "avg": 12.14}
}`

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestParseAiperfArtifact_Percentiles(t *testing.T) {
	m, err := genai.ParseAiperfArtifact([]byte(artifact))
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string][2]float64{
		"ttft_p50": {m.TTFTP50Ms, 175.05},
		"ttft_p90": {m.TTFTP90Ms, 251.45},
		"ttft_p95": {m.TTFTP95Ms, 252.9},
		"ttft_p99": {m.TTFTP99Ms, 253.45},
		"itl_p50":  {m.ITLP50Ms, 34.73},
		"itl_p95":  {m.ITLP95Ms, 35.955},
		"itl_p99":  {m.ITLP99Ms, 35.96},
		"in_tps":   {m.InputTokensPerSec, 2560.0 / 12.14},
		"out_tps":  {m.OutputTokensPerSec, 52.69},
		"tot_tps":  {m.TotalTokensPerSec, 2560.0/12.14 + 52.69},
	}
	for k, v := range checks {
		if !near(v[0], v[1]) {
			t.Errorf("%s = %v, want %v", k, v[0], v[1])
		}
	}
	if m.PrefixCacheHitRate != nil || m.PrefillWorkerUtilization != nil || m.DecodeWorkerUtilization != nil {
		t.Errorf("scrape-derived fields must be nil before Attach: %+v", m)
	}
}

func TestParseAiperfArtifact_DerivedThroughputAndMeanAlias(t *testing.T) {
	// Older export: "mean" instead of "avg", no sums, no output_token_throughput.
	raw := `{"time_to_first_token":{"mean":50,"p50":40,"p99":90},
	         "request_count":{"mean":20},
	         "input_sequence_length":{"mean":100},
	         "total_output_tokens":{"mean":400},
	         "benchmark_duration":{"mean":10}}`
	m, err := genai.ParseAiperfArtifact([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !near(m.InputTokensPerSec, 200) || !near(m.OutputTokensPerSec, 40) || !near(m.TotalTokensPerSec, 240) {
		t.Errorf("derived throughput = %v/%v/%v", m.InputTokensPerSec, m.OutputTokensPerSec, m.TotalTokensPerSec)
	}
	if m.TTFTP95Ms != 0 {
		t.Errorf("missing p95 must stay 0, got %v", m.TTFTP95Ms)
	}
}

func TestParseAiperfArtifact_Rejects(t *testing.T) {
	for _, raw := range []string{`not json`, `{"foo": 1}`, `[]`} {
		if _, err := genai.ParseAiperfArtifact([]byte(raw)); err == nil {
			t.Errorf("expected error for %q", raw)
		}
	}
}

func TestMetricsJSONShapeAndMerge(t *testing.T) {
	m := &genai.Metrics{TTFTP50Ms: 1, PrefixCacheHitRate: genai.Float(0.5)}
	b, _ := json.Marshal(m)
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	for _, f := range []string{"ttft_p50_ms", "ttft_p90_ms", "ttft_p95_ms", "ttft_p99_ms", "itl_p50_ms", "itl_p95_ms", "itl_p99_ms", "prefix_cache_hit_rate", "input_tokens_per_sec", "output_tokens_per_sec", "total_tokens_per_sec"} {
		if _, ok := got[f]; !ok {
			t.Errorf("missing field %s", f)
		}
	}
	for _, f := range []string{"prefill_worker_utilization", "decode_worker_utilization"} {
		if _, ok := got[f]; ok {
			t.Errorf("%s must be omitted when unset", f)
		}
	}
	dst := map[string]any{"benchmark_id": "x"}
	m.MergeInto(dst)
	if dst["benchmark_id"] != "x" || dst["prefix_cache_hit_rate"] != 0.5 {
		t.Errorf("merge = %v", dst)
	}
	if !genai.IsMetricsJSON(b) || genai.IsMetricsJSON([]byte(artifact)) {
		t.Error("IsMetricsJSON misclassified")
	}
	if len(genai.Fields) != 13 {
		t.Errorf("Fields = %d entries", len(genai.Fields))
	}
}

const vllmBefore = `# HELP vllm:prefix_cache_queries_total Prefix cache queries
# TYPE vllm:prefix_cache_queries_total counter
vllm:prefix_cache_queries_total{model_name="llama3"} 1000.0
vllm:prefix_cache_hits_total{model_name="llama3"} 100.0
vllm:kv_cache_usage_perc{model_name="llama3"} 0.10
`

const vllmAfter = `vllm:prefix_cache_queries_total{model_name="llama3"} 3000.0
vllm:prefix_cache_hits_total{model_name="llama3"} 1700.0
vllm:kv_cache_usage_perc{model_name="llama3"} 0.42
vllm:num_requests_running{model_name="llama3"} 3.0
`

func TestParseExposition(t *testing.T) {
	s := genai.ParseExposition(`a_total{x="1,2",y="q\"z"} 3 1700000000
b 4.5e1
nan_metric NaN
# comment
broken{ 1`)
	if len(s) != 2 {
		t.Fatalf("samples = %+v", s)
	}
	if s[0].Name != "a_total" || s[0].Labels["x"] != "1,2" || s[0].Labels["y"] != `q"z` || s[0].Value != 3 {
		t.Errorf("sample 0 = %+v", s[0])
	}
	if s[1].Name != "b" || s[1].Value != 45 {
		t.Errorf("sample 1 = %+v", s[1])
	}
}

func TestPrefixCacheHitRate_CountersDelta(t *testing.T) {
	b := genai.SnapshotFromText(vllmBefore)
	a := genai.SnapshotFromText(vllmAfter)
	rate, src, ok := genai.PrefixCacheHitRate(b, a)
	if !ok || !near(rate, 0.8) || src != "vllm:prefix_cache_queries" {
		t.Errorf("rate=%v src=%q ok=%v", rate, src, ok)
	}
	// No before → lifetime.
	rate, src, ok = genai.PrefixCacheHitRate(genai.CacheSnapshot{}, a)
	if !ok || !near(rate, 1700.0/3000.0) || src != "vllm:prefix_cache_queries (lifetime)" {
		t.Errorf("lifetime rate=%v src=%q ok=%v", rate, src, ok)
	}
	// Nothing queried during the interval.
	if _, _, ok := genai.PrefixCacheHitRate(a, a); ok {
		t.Error("zero delta must not produce a rate")
	}
}

func TestPrefixCacheHitRate_HistogramAndGauge(t *testing.T) {
	epp := `inference_extension_prefix_indexer_hit_ratio_sum 12.5
inference_extension_prefix_indexer_hit_ratio_count 20
inference_pool_average_kv_cache_utilization 0.33`
	rate, src, ok := genai.PrefixCacheHitRate(genai.CacheSnapshot{}, genai.SnapshotFromText(epp))
	if !ok || !near(rate, 0.625) || src != "inference_extension_prefix_indexer_hit_ratio (lifetime)" {
		t.Errorf("epp rate=%v src=%q ok=%v", rate, src, ok)
	}
	rate, _, ok = genai.PrefixCacheHitRate(genai.CacheSnapshot{}, genai.SnapshotFromText(`vllm:gpu_prefix_cache_hit_rate 0.71`))
	if !ok || !near(rate, 0.71) {
		t.Errorf("gauge rate=%v ok=%v", rate, ok)
	}
	if _, _, ok := genai.PrefixCacheHitRate(genai.CacheSnapshot{}, genai.SnapshotFromText(`vllm:num_requests_running 1`)); ok {
		t.Error("no prefix metric must not produce a rate")
	}
}

func TestAttach_AggregatesEndpointsAndRoles(t *testing.T) {
	m := &genai.Metrics{}
	before := []genai.Scrape{
		{Role: "prefill", Endpoint: "p1", Text: vllmBefore},
		{Role: "decode", Endpoint: "d1", Text: `vllm:prefix_cache_queries_total 10
vllm:prefix_cache_hits_total 0
vllm:gpu_cache_usage_perc 0.5`},
	}
	after := []genai.Scrape{
		{Role: "prefill", Endpoint: "p1", Text: vllmAfter},
		{Role: "decode", Endpoint: "d1", Text: `vllm:prefix_cache_queries_total 10
vllm:prefix_cache_hits_total 0
vllm:gpu_cache_usage_perc 0.9`},
	}
	notes := genai.Attach(m, before, after)
	if m.PrefixCacheHitRate == nil || !near(*m.PrefixCacheHitRate, 0.8) {
		t.Fatalf("hit rate = %v (notes %v)", m.PrefixCacheHitRate, notes)
	}
	if m.PrefillWorkerUtilization == nil || !near(*m.PrefillWorkerUtilization, 0.42) {
		t.Errorf("prefill util = %v", m.PrefillWorkerUtilization)
	}
	if m.DecodeWorkerUtilization == nil || !near(*m.DecodeWorkerUtilization, 0.9) {
		t.Errorf("decode util = %v", m.DecodeWorkerUtilization)
	}
	if len(notes) != 3 {
		t.Errorf("notes = %v", notes)
	}
	// A run with no scrapes leaves the pointers nil.
	m2 := &genai.Metrics{}
	if notes := genai.Attach(m2, nil, nil); notes != nil || m2.PrefixCacheHitRate != nil {
		t.Errorf("empty attach changed metrics: %+v %v", m2, notes)
	}
	// A monolithic (unroled) server contributes to the hit rate but not to utilization.
	m3 := &genai.Metrics{}
	genai.Attach(m3, nil, []genai.Scrape{{Endpoint: "v", Text: vllmAfter}})
	if m3.PrefixCacheHitRate == nil || m3.PrefillWorkerUtilization != nil || m3.DecodeWorkerUtilization != nil {
		t.Errorf("unroled scrape: %+v", m3)
	}
	if len(m3.Summary()) < 4 {
		t.Errorf("summary = %v", m3.Summary())
	}
}

func TestParseRoleSpec(t *testing.T) {
	cases := []struct {
		in, role, val string
		err           bool
	}{
		{"http://10.0.0.1:8000/metrics", "", "http://10.0.0.1:8000/metrics", false},
		{"prefill=http://10.0.0.1:8000/metrics", "prefill", "http://10.0.0.1:8000/metrics", false},
		{"Decode=/tmp/after.prom", "decode", "/tmp/after.prom", false},
		{"http://h/metrics?x=1", "", "http://h/metrics?x=1", false},
		{"gpu=http://h/metrics", "", "gpu=http://h/metrics", false},
		{"app=vllm", "", "app=vllm", false}, // --metrics-pod-selector label selector
		{"prefill=app=vllm", "prefill", "app=vllm", false},
		{"PREFILL=app in (vllm)", "prefill", "app in (vllm)", false},
	}
	for _, c := range cases {
		role, val, err := genai.ParseRoleSpec(c.in)
		if (err != nil) != c.err || role != c.role || val != c.val {
			t.Errorf("ParseRoleSpec(%q) = %q %q %v", c.in, role, val, err)
		}
	}
}
