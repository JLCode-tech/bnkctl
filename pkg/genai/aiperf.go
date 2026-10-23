package genai

import (
	"encoding/json"
	"fmt"
)

// aiperfDist is the distribution object aiperf writes for every metric in
// profile_export_aiperf.json. "mean" is accepted as an alias of "avg" for
// older exports.
type aiperfDist struct {
	Unit  string  `json:"unit"`
	Avg   float64 `json:"avg"`
	Mean  float64 `json:"mean"`
	P50   float64 `json:"p50"`
	P90   float64 `json:"p90"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Count float64 `json:"count"`
	Sum   float64 `json:"sum"`
}

func (d aiperfDist) avg() float64 {
	if d.Avg != 0 {
		return d.Avg
	}
	return d.Mean
}

// aiperfArtifact is the subset of profile_export_aiperf.json genai reads.
type aiperfArtifact struct {
	TimeToFirstToken      *aiperfDist `json:"time_to_first_token"`
	InterTokenLatency     *aiperfDist `json:"inter_token_latency"`
	RequestLatency        *aiperfDist `json:"request_latency"`
	RequestCount          *aiperfDist `json:"request_count"`
	InputSequenceLength   *aiperfDist `json:"input_sequence_length"`
	OutputSequenceLength  *aiperfDist `json:"output_sequence_length"`
	TotalOutputTokens     *aiperfDist `json:"total_output_tokens"`
	OutputTokenThroughput *aiperfDist `json:"output_token_throughput"`
	BenchmarkDuration     *aiperfDist `json:"benchmark_duration"`
}

// ParseAiperfArtifact extracts the GenAI metrics from an aiperf
// profile_export_aiperf.json document. Percentiles are copied as aiperf
// reports them (milliseconds). Token throughput is derived when aiperf does
// not report it directly:
//
//	input_tokens_per_sec  = input_sequence_length.sum / benchmark_duration
//	                        (avg × request_count when sum is absent)
//	output_tokens_per_sec = output_token_throughput.avg
//	                        (total_output_tokens / benchmark_duration when absent)
//	total_tokens_per_sec  = input + output
//
// The prefix-cache and worker-utilization fields are left unset; Attach fills
// them from metrics scrapes.
func ParseAiperfArtifact(raw []byte) (*Metrics, error) {
	var a aiperfArtifact
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("aiperf artifact: %w", err)
	}
	if a.TimeToFirstToken == nil && a.RequestLatency == nil {
		return nil, fmt.Errorf("aiperf artifact: neither time_to_first_token nor request_latency present")
	}

	m := &Metrics{}
	if d := a.TimeToFirstToken; d != nil {
		m.TTFTP50Ms, m.TTFTP90Ms, m.TTFTP95Ms, m.TTFTP99Ms = d.P50, d.P90, d.P95, d.P99
	}
	if d := a.InterTokenLatency; d != nil {
		m.ITLP50Ms, m.ITLP95Ms, m.ITLP99Ms = d.P50, d.P95, d.P99
	}

	duration := 0.0
	if a.BenchmarkDuration != nil {
		duration = a.BenchmarkDuration.avg()
	}
	requests := 0.0
	if a.RequestCount != nil {
		requests = a.RequestCount.avg()
	}

	if duration > 0 {
		inputTokens := 0.0
		if d := a.InputSequenceLength; d != nil {
			switch {
			case d.Sum > 0:
				inputTokens = d.Sum
			case d.Count > 0:
				inputTokens = d.avg() * d.Count
			default:
				inputTokens = d.avg() * requests
			}
		}
		m.InputTokensPerSec = inputTokens / duration
	}

	switch {
	case a.OutputTokenThroughput != nil && a.OutputTokenThroughput.avg() > 0:
		m.OutputTokensPerSec = a.OutputTokenThroughput.avg()
	case a.TotalOutputTokens != nil && duration > 0:
		m.OutputTokensPerSec = a.TotalOutputTokens.avg() / duration
	case a.OutputSequenceLength != nil && duration > 0:
		m.OutputTokensPerSec = a.OutputSequenceLength.avg() * requests / duration
	}
	m.TotalTokensPerSec = m.InputTokensPerSec + m.OutputTokensPerSec
	return m, nil
}
