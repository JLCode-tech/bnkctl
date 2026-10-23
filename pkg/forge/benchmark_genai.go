package forge

import (
	"encoding/json"
	"fmt"

	"github.com/JLCode-tech/bnkctl/pkg/genai"
	"github.com/JLCode-tech/bnkctl/pkg/jumphost"
)

// DeriveGenAIMetrics builds the GenAI metric set from a parsed AiperfResult
// when no artifact-level parse or metrics scrape is available: TTFT/ITL
// percentiles from the distributions, token throughput from the averages and
// the run duration. Prefix-cache and worker-utilization fields stay unset.
func DeriveGenAIMetrics(result *jumphost.AiperfResult) *genai.Metrics {
	m := &genai.Metrics{}
	if result == nil {
		return m
	}
	if result.RawJSON != "" {
		if parsed, err := genai.ParseAiperfArtifact([]byte(result.RawJSON)); err == nil {
			return parsed
		}
	}
	m.TTFTP50Ms, m.TTFTP90Ms, m.TTFTP95Ms, m.TTFTP99Ms = result.TTFT.P50, result.TTFT.P90, result.TTFT.P95, result.TTFT.P99
	m.ITLP50Ms, m.ITLP95Ms, m.ITLP99Ms = result.ITL.P50, result.ITL.P95, result.ITL.P99
	if result.DurationSeconds > 0 {
		m.InputTokensPerSec = result.AvgInputTokens * float64(result.TotalRequests) / result.DurationSeconds
	}
	m.OutputTokensPerSec = result.OutputTokenThroughput
	if m.OutputTokensPerSec == 0 && result.DurationSeconds > 0 {
		m.OutputTokensPerSec = result.TotalOutputTokens / result.DurationSeconds
	}
	m.TotalTokensPerSec = m.InputTokensPerSec + m.OutputTokensPerSec
	return m
}

// AIPerfResultPayload is the body of POST /api/benchmarks/results/aiperf: the
// verbatim profile_export_aiperf.json object with the GenAI metric fields
// added at the top level under the same names the structured payload uses
// (ttft_p50_ms, itl_p95_ms, prefix_cache_hit_rate, input_tokens_per_sec, …).
// Every aiperf key is passed through byte-for-byte; a GenAI field never
// overrides an aiperf key of the same name.
type AIPerfResultPayload struct {
	Raw   json.RawMessage
	GenAI *genai.Metrics
}

// MarshalJSON merges the GenAI fields into the raw aiperf object.
func (p AIPerfResultPayload) MarshalJSON() ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(p.Raw, &obj); err != nil {
		return nil, fmt.Errorf("aiperf payload: raw body is not a JSON object: %w", err)
	}
	if obj == nil {
		obj = map[string]json.RawMessage{}
	}
	if p.GenAI != nil {
		for k, v := range p.GenAI.ToMap() {
			if _, exists := obj[k]; exists {
				continue
			}
			b, err := json.Marshal(v)
			if err != nil {
				return nil, fmt.Errorf("aiperf payload: field %s: %w", k, err)
			}
			obj[k] = b
		}
	}
	return json.Marshal(obj)
}
