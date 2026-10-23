// Package genai derives GenAI inference metrics for a benchmark run: TTFT and
// ITL percentiles and token throughput from an aiperf profile_export_aiperf.json
// artifact, and the prompt prefix-cache hit rate plus prefill/decode worker
// utilization from Prometheus scrapes of the model servers (vLLM, LMI) or the
// endpoint picker (EPP) taken before and after the run.
//
// The package has no dependencies on the rest of the binary so that
// internal/jumphost, internal/forge and internal/cli can all import it.
package genai
