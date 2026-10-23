package genai

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Metrics is the GenAI metric set attached to every benchmark result pushed to
// Forge. JSON names are the flat field names of the Forge benchmark payloads
// (POST /api/benchmarks/results and POST /api/benchmarks/results/aiperf).
//
// Latencies are milliseconds. prefix_cache_hit_rate is the prompt KV-cache hit
// ratio 0.0–1.0; the worker utilizations are the KV-cache utilization 0.0–1.0
// of the prefill and decode pools at the end of the run. The pointer fields are
// omitted when the run had no metrics source for them.
type Metrics struct {
	TTFTP50Ms float64 `json:"ttft_p50_ms"`
	TTFTP90Ms float64 `json:"ttft_p90_ms"`
	TTFTP95Ms float64 `json:"ttft_p95_ms"`
	TTFTP99Ms float64 `json:"ttft_p99_ms"`

	ITLP50Ms float64 `json:"itl_p50_ms"`
	ITLP95Ms float64 `json:"itl_p95_ms"`
	ITLP99Ms float64 `json:"itl_p99_ms"`

	PrefixCacheHitRate *float64 `json:"prefix_cache_hit_rate,omitempty"`
	// PrefixCacheSource names the metric family the hit rate came from, e.g.
	// "vllm:prefix_cache_queries" or "inference_extension_prefix_indexer_hit_ratio".
	PrefixCacheSource string `json:"prefix_cache_source,omitempty"`

	InputTokensPerSec  float64 `json:"input_tokens_per_sec"`
	OutputTokensPerSec float64 `json:"output_tokens_per_sec"`
	TotalTokensPerSec  float64 `json:"total_tokens_per_sec"`

	PrefillWorkerUtilization *float64 `json:"prefill_worker_utilization,omitempty"`
	DecodeWorkerUtilization  *float64 `json:"decode_worker_utilization,omitempty"`
}

// Fields is the ordered list of the flat JSON field names Metrics contributes
// to the Forge payloads. Used by the payload contract tests and the docs.
var Fields = []string{
	"ttft_p50_ms", "ttft_p90_ms", "ttft_p95_ms", "ttft_p99_ms",
	"itl_p50_ms", "itl_p95_ms", "itl_p99_ms",
	"prefix_cache_hit_rate",
	"input_tokens_per_sec", "output_tokens_per_sec", "total_tokens_per_sec",
	"prefill_worker_utilization", "decode_worker_utilization",
}

// ToMap returns the JSON representation of m as a map, e.g. for merging into
// an existing JSON object.
func (m *Metrics) ToMap() map[string]any {
	out := map[string]any{}
	if m == nil {
		return out
	}
	b, err := json.Marshal(m)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(b, &out)
	return out
}

// MergeInto copies the flat fields of m into dst (a decoded JSON object).
func (m *Metrics) MergeInto(dst map[string]any) {
	for k, v := range m.ToMap() {
		dst[k] = v
	}
}

// IsMetricsJSON reports whether b is a JSON object written by Metrics (a
// --genai-out file) rather than an aiperf artifact.
func IsMetricsJSON(b []byte) bool {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		return false
	}
	_, ok := probe["ttft_p50_ms"]
	return ok
}

// Summary renders m as short human-readable lines.
func (m *Metrics) Summary() []string {
	if m == nil {
		return nil
	}
	lines := []string{
		fmt.Sprintf("TTFT p50/p90/p95/p99 ms: %.1f / %.1f / %.1f / %.1f", m.TTFTP50Ms, m.TTFTP90Ms, m.TTFTP95Ms, m.TTFTP99Ms),
		fmt.Sprintf("ITL  p50/p95/p99 ms:     %.2f / %.2f / %.2f", m.ITLP50Ms, m.ITLP95Ms, m.ITLP99Ms),
		fmt.Sprintf("tokens/s in/out/total:   %.1f / %.1f / %.1f", m.InputTokensPerSec, m.OutputTokensPerSec, m.TotalTokensPerSec),
	}
	switch {
	case m.PrefixCacheHitRate != nil && m.PrefixCacheSource != "":
		lines = append(lines, fmt.Sprintf("prefix cache hit rate:   %.1f%% (%s)", *m.PrefixCacheHitRate*100, m.PrefixCacheSource))
	case m.PrefixCacheHitRate != nil:
		lines = append(lines, fmt.Sprintf("prefix cache hit rate:   %.1f%%", *m.PrefixCacheHitRate*100))
	default:
		lines = append(lines, "prefix cache hit rate:   n/a (no metrics source)")
	}
	var util []string
	if m.PrefillWorkerUtilization != nil {
		util = append(util, fmt.Sprintf("prefill %.1f%%", *m.PrefillWorkerUtilization*100))
	}
	if m.DecodeWorkerUtilization != nil {
		util = append(util, fmt.Sprintf("decode %.1f%%", *m.DecodeWorkerUtilization*100))
	}
	if len(util) > 0 {
		lines = append(lines, "worker KV utilization:   "+strings.Join(util, ", "))
	}
	return lines
}

// Float returns a pointer to v.
func Float(v float64) *float64 { return &v }
