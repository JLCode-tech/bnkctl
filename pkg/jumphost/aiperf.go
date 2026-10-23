package jumphost

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/JLCode-tech/bnkctl/pkg/genai"
)

// AiperfConfig holds the parameters for a single aiperf benchmark run.
// aiperf is installed on the jumphost via python3.11 -m pip install aiperf.
// All fields except Model and VIP have sensible defaults.
type AiperfConfig struct {
	// Model is the LLM model name as served by the endpoint (e.g. "llama3").
	Model string
	// EndpointType is the aiperf --endpoint-type value. Default: "chat".
	EndpointType string
	// EndpointPath is the HTTP path (e.g. "/v1/chat/completions").
	// Only passed as --custom-endpoint when non-default.
	EndpointPath string
	// Concurrency is the number of concurrent users. Default: 1.
	Concurrency int
	// NumRequests is the total number of requests to send. Default: 10.
	NumRequests int
	// ISL is the input sequence length (tokens). Default: 512.
	ISL int
	// OSL is the output sequence length (tokens). Default: 128.
	OSL int
	// ISLStddev is the stddev for synthetic input token length (--synthetic-input-tokens-stddev).
	// Zero means omit the flag (aiperf defaults apply).
	ISLStddev int
	// SeqDist is the sequence-distribution string passed via --seq-dist.
	// When non-empty, --synthetic-input-tokens-mean and --output-tokens-mean are
	// OMITTED (seq-dist drives both input and output length). ISL and OSL are
	// ignored by buildAiperfCmd when SeqDist is set.
	SeqDist string
	// PrefixPromptLength is the prefix prompt length (--prefix-prompt-length).
	// Zero means omit the flag.
	PrefixPromptLength int
	// NumPrefixPrompts is the number of prefix prompts (--num-prefix-prompts).
	// Zero means omit the flag.
	NumPrefixPrompts int
	// NumDatasetEntries is the number of distinct prompts aiperf generates
	// (--num-dataset-entries). Zero: one per request for a timed open-loop run
	// (see buildAiperfCmd), else aiperf's default of 100.
	NumDatasetEntries int
	// ExtraInputs is a list of "key:value" strings forwarded as repeated
	// --extra-inputs flags. Empty means omit.
	ExtraInputs []string
	// VariantLabel is a metadata field carried for per-child run labeling.
	// It is NOT an aiperf flag and is not emitted in the command.
	VariantLabel string
	// Streaming enables streaming mode (--streaming). Default: false.
	Streaming bool
	// Tokenizer is the Hugging Face tokenizer repo used by aiperf for token
	// counting. Required by aiperf 0.10.0.
	// Default: "NousResearch/Meta-Llama-3-8B-Instruct" (ungated, no HF token).
	Tokenizer string
	// HostHeader is the HTTP Host header to inject (--header "Host:<value>").
	// Required when the BNK HTTPRoute has a hostname match; without it every
	// request returns 404.
	HostHeader string
	// Timeout is the per-run total timeout. Default: 5 minutes.
	Timeout time.Duration

	// ── Trace-driven (mooncake) fields ──────────────────────────────────────
	// TraceURL is the URL of the JSONL trace file to download on the jumphost.
	// When non-empty, buildAiperfCmd emits the mooncake shell pipeline instead
	// of the synthetic path. Must be http:// or https:// (SSRF guard).
	TraceURL string
	// TraceDilation is the time-dilation factor: each record's numeric
	// timestamp is divided by this value (< 1 ⇒ slower arrivals).
	// Default 0.80 when TraceURL is set and TraceDilation is zero.
	TraceDilation float64
	// CustomDatasetType maps to aiperf --custom-dataset-type (e.g. "mooncake_trace").
	CustomDatasetType string
	// FixedSchedule sets the bool flag --fixed-schedule.
	FixedSchedule bool
	// WorkersMax maps to aiperf --workers-max.
	WorkersMax int
	// RandomSeed maps to aiperf --random-seed.
	RandomSeed int
	// RequestTimeoutSeconds maps to aiperf --request-timeout-seconds.
	RequestTimeoutSeconds int
	// ProfileExportLevel maps to aiperf --profile-export-level (e.g. "summary").
	ProfileExportLevel string
	// RecordProcessors maps to aiperf --record-processors.
	RecordProcessors int
	// Goodput maps to aiperf --goodput (space-joined string, e.g.
	// "time_to_first_token:5000 inter_token_latency:100").
	Goodput string

	// ── Open-loop load (synthetic path) ─────────────────────────────────────
	// RequestRate maps to aiperf --request-rate (req/s). When set, requests
	// arrive on a schedule and --concurrency is only emitted if given.
	RequestRate float64
	// ArrivalPattern maps to aiperf --arrival-pattern (poisson, constant, gamma).
	ArrivalPattern string
	// WarmupDuration maps to aiperf --warmup-duration (seconds, not measured).
	WarmupDuration float64
	// BenchmarkDuration maps to aiperf --benchmark-duration (seconds). When
	// set, --request-count is only emitted if given.
	BenchmarkDuration float64

	// ── Native scenario / AgentX MVP fields ─────────────────────────────────
	// Scenario maps to aiperf --scenario (e.g. "inferencex-agentx-mvp").
	Scenario string
	// PublicDataset maps to aiperf --public-dataset (e.g. "semianalysis_cc_traces_weka_with_subagents").
	PublicDataset string
	// UseServerTokenCount maps to aiperf --use-server-token-count.
	UseServerTokenCount bool
	// MaxContextLength maps to aiperf --max-context-length.
	MaxContextLength int
	// UnsafeOverride maps to aiperf --unsafe-override.
	UnsafeOverride bool
}

// aiperfMetricDist is the distribution shape used for most aiperf 0.10.0 metrics.
// Scalar metrics only use Avg.
type aiperfMetricDist struct {
	Unit  string  `json:"unit"`
	Avg   float64 `json:"avg"`
	P1    float64 `json:"p1"`
	P5    float64 `json:"p5"`
	P10   float64 `json:"p10"`
	P25   float64 `json:"p25"`
	P50   float64 `json:"p50"`
	P75   float64 `json:"p75"`
	P90   float64 `json:"p90"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Std   float64 `json:"std"`
	Count float64 `json:"count"`
	Sum   float64 `json:"sum"`
}

// aiperfTelemetry is the nested telemetry summary inside the JSON output.
type aiperfTelemetry struct {
	Summary struct {
		StartTime    string `json:"start_time"`
		EndTime      string `json:"end_time"`
		ErrorSummary []any  `json:"error_summary"`
	} `json:"summary"`
}

// aiperfRawOutput is the full JSON schema of aiperf 0.10.0's
// profile_export_aiperf.json. We decode all metrics we need, silently
// ignoring the rest.
type aiperfRawOutput struct {
	SchemaVersion string `json:"schema_version"`
	AiperfVersion string `json:"aiperf_version"`
	BenchmarkID   string `json:"benchmark_id"`

	RequestThroughput     aiperfMetricDist `json:"request_throughput"`
	RequestLatency        aiperfMetricDist `json:"request_latency"`
	RequestCount          aiperfMetricDist `json:"request_count"`
	TimeToFirstToken      aiperfMetricDist `json:"time_to_first_token"`
	InterTokenLatency     aiperfMetricDist `json:"inter_token_latency"`
	OutputTokenThroughput aiperfMetricDist `json:"output_token_throughput"`
	OutputSequenceLength  aiperfMetricDist `json:"output_sequence_length"`
	InputSequenceLength   aiperfMetricDist `json:"input_sequence_length"`
	TotalOutputTokens     aiperfMetricDist `json:"total_output_tokens"`
	BenchmarkDuration     aiperfMetricDist `json:"benchmark_duration"`

	StartTime    string `json:"start_time"`
	EndTime      string `json:"end_time"`
	WasCancelled bool   `json:"was_cancelled"`
	ErrorSummary []any  `json:"error_summary"`

	Telemetry aiperfTelemetry `json:"telemetry_data"`
}

// DistributionStats captures the per-percentile distribution for a metric.
type DistributionStats struct {
	Unit string  `json:"unit"`
	Avg  float64 `json:"avg"`
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
}

// AiperfResult is the Go representation of aiperf 0.10.0 benchmark output.
// Fields map directly from profile_export_aiperf.json top-level metric objects.
type AiperfResult struct {
	// Identity — backfilled from config when absent in JSON
	Model    string `json:"model"`
	BaseURL  string `json:"base_url"`
	Endpoint string `json:"endpoint"`

	// RawJSON is the verbatim profile_export_aiperf.json content captured from
	// the remote `cat` command.  It preserves all aiperf fields (http-timing,
	// telemetry, full percentile distributions) beyond what the typed fields
	// above carry.  Set alongside the parsed fields in RunAiperf; empty when
	// constructed directly (e.g. in tests that build AiperfResult by hand).
	RawJSON string `json:"-"`

	// Metadata
	AiperfVersion string `json:"aiperf_version"`
	SchemaVersion string `json:"schema_version"`
	BenchmarkID   string `json:"benchmark_id"`
	StartTime     string `json:"start_time"`
	EndTime       string `json:"end_time"`
	WasCancelled  bool   `json:"was_cancelled"`
	ErrorSummary  []any  `json:"error_summary"`

	// Request counts (derived)
	TotalRequests int `json:"total_requests"`
	Successful    int `json:"successful"`
	Failed        int `json:"failed"`

	// Duration
	DurationSeconds float64 `json:"duration_seconds"`
	DurationMinutes float64 `json:"duration_minutes"`

	// Throughput
	RequestThroughput     float64 `json:"request_throughput_rps"`      // requests/sec avg
	OutputTokenThroughput float64 `json:"output_token_throughput_tps"` // tokens/sec avg

	// Request latency distribution (ms)
	RequestLatency DistributionStats `json:"request_latency"`

	// Time-to-first-token distribution (ms)
	TTFT DistributionStats `json:"ttft"`

	// Inter-token latency distribution (ms)
	ITL DistributionStats `json:"itl"`

	// Token length averages
	AvgInputTokens  float64 `json:"avg_input_tokens"`
	AvgOutputTokens float64 `json:"avg_output_tokens"`

	// Total tokens across all requests
	TotalOutputTokens float64 `json:"total_output_tokens_sum"`

	// GenAI carries the TTFT/ITL percentiles, token throughput and, when the
	// run scraped model-server metrics, the prefix-cache hit rate and worker
	// utilization. Filled by the CLI after RunAiperf; nil when not computed.
	GenAI *genai.Metrics `json:"genai_metrics,omitempty"`
}

// sshExecFn is the injectable seam for running a remote command over
// EICE-SSH and capturing its stdout.  Default: SSHRunViaEICE.
// Tests replace this via aiperfSSHExecFn (exposed in aiperf_export_test.go).
var aiperfSSHExecFn = func(ctx context.Context, region, instanceID, keyPath, remoteCmd string) (string, error) {
	return SSHRunViaEICE(ctx, region, instanceID, keyPath, remoteCmd)
}

// AiperfRunOptions is the full set of parameters RunAiperf needs.
type AiperfRunOptions struct {
	// ProbeOptions carries the EICE / jumphost identity.
	ProbeOptions ProbeOptions
	// Config is the benchmark configuration.
	Config AiperfConfig
	// RunLabel is a human-readable label stored in the result labels.
	RunLabel string
	// ResultID is a caller-supplied unique ID (UUID or timestamp string).
	// When empty, aiperf generates its own.
	ResultID string
}

// validateTraceURL returns an error when traceURL has a scheme other than
// http or https. This mirrors forge_agent's TRACE_ALLOWED_SCHEMES allowlist
// and prevents SSRF via file://, ftp://, etc.
func validateTraceURL(traceURL string) error {
	if traceURL == "" {
		return fmt.Errorf("trace_url must not be empty")
	}
	// Simple prefix check — avoids importing net/url for a two-case guard.
	if strings.HasPrefix(traceURL, "https://") || strings.HasPrefix(traceURL, "http://") {
		return nil
	}
	// Extract the scheme (up to "://") for the error message.
	scheme := traceURL
	if i := strings.Index(traceURL, "://"); i >= 0 {
		scheme = traceURL[:i]
	} else if i := strings.Index(traceURL, ":"); i >= 0 {
		scheme = traceURL[:i]
	}
	return fmt.Errorf("trace_url scheme %q not allowed; permitted: http, https", scheme)
}

// dilateTimestamp divides a float64 timestamp by the dilation factor.
// This is the pure Go equivalent of forge_agent's _dilate_trace inner
// transform: record["timestamp"] = ts / dilation. A dilation < 1 (e.g. 0.80)
// increases each timestamp (ts / 0.80 = ts * 1.25), stretching inter-arrival
// gaps so requests replay SLOWER — matching the canonical f5-epp harness.
//
// This function is factored out of the embedded shell snippet so it can be
// unit-tested directly (the shell python3.11 -c snippet mirrors this logic).
func dilateTimestamp(ts, dilation float64) float64 {
	return ts / dilation
}

// buildMooncakeCmd constructs the trace-driven remote shell pipeline for the
// mooncake scenario. The pipeline, in order:
//  1. ulimit -n 65536 (best-effort — non-fatal if the hard limit is lower)
//  2. export AIPERF_HTTP_CONNECTION_LIMIT=<workersMax>
//  3. curl -fsSL <traceURL> -o /tmp/aiperf-<id>-raw.jsonl
//  4. python3.11 -c '...' dilation step: divide each record's numeric
//     "timestamp" by dilation, pass through non-numeric/blank lines
//  5. aiperf profile with --input-file, --custom-dataset-type, --fixed-schedule,
//     and all other scalar flags from the config
//  6. cat the newest profile_export_aiperf.json found under output-artifact-dir
//     (mirrors forge_agent's _find_result_json glob)
func buildMooncakeCmd(opts AiperfRunOptions) (string, error) {
	cfg := opts.Config

	if err := validateTraceURL(cfg.TraceURL); err != nil {
		return "", err
	}

	dilation := cfg.TraceDilation
	if dilation == 0 {
		dilation = 0.80
	}

	// Unique working paths per run.
	dirSuffix := "run"
	if opts.ResultID != "" {
		safe := strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
				return r
			}
			return '-'
		}, opts.ResultID)
		dirSuffix = safe
	}
	rawFile := fmt.Sprintf("/tmp/aiperf-%s-raw.jsonl", dirSuffix)
	dilatedFile := fmt.Sprintf("/tmp/aiperf-%s-dilated.jsonl", dirSuffix)
	artifactDir := fmt.Sprintf("/tmp/aiperf-%s-artifacts", dirSuffix)

	workersMax := cfg.WorkersMax
	if workersMax <= 0 {
		workersMax = 200
	}

	// Defaults for required aiperf flags.
	if cfg.Tokenizer == "" {
		cfg.Tokenizer = "NousResearch/Meta-Llama-3-8B-Instruct"
	}
	if cfg.EndpointType == "" {
		cfg.EndpointType = "chat"
	}

	vip := opts.ProbeOptions.VIP

	// ── Python dilation inline script ────────────────────────────────────────
	// Mirrors forge_agent._dilate_trace exactly:
	//   for each non-blank line, parse JSON, if timestamp is numeric divide by
	//   dilation, write back. Pass through records without a numeric timestamp.
	dilationScript := fmt.Sprintf(
		"import json,sys\n"+
			"d=%g\n"+
			"[sys.stdout.write(json.dumps({**r,'timestamp':r['timestamp']/d})+chr(10)) "+
			"if isinstance(r.get('timestamp'),(int,float)) else "+
			"sys.stdout.write(json.dumps(r)+chr(10)) "+
			"for l in sys.stdin if l.strip() "+
			"for r in [json.loads(l)]]",
		dilation,
	)

	// Build the aiperf profile invocation (trace path: --input-file + --output-artifact-dir).
	args := []string{
		"aiperf", "profile",
		"-m", shellSingleQuote(cfg.Model),
		"-u", shellSingleQuote(fmt.Sprintf("http://%s", vip)),
		"--endpoint-type", shellSingleQuote(cfg.EndpointType),
		"--tokenizer", shellSingleQuote(cfg.Tokenizer),
		"--input-file", shellSingleQuote(dilatedFile),
		"--custom-dataset-type", shellSingleQuote(cfg.CustomDatasetType),
	}

	if cfg.FixedSchedule {
		args = append(args, "--fixed-schedule")
	}

	if cfg.WorkersMax > 0 {
		args = append(args, "--workers-max", fmt.Sprintf("%d", cfg.WorkersMax))
	}
	if cfg.RandomSeed != 0 {
		args = append(args, "--random-seed", fmt.Sprintf("%d", cfg.RandomSeed))
	}
	if cfg.RequestTimeoutSeconds > 0 {
		args = append(args, "--request-timeout-seconds", fmt.Sprintf("%d", cfg.RequestTimeoutSeconds))
	}
	if cfg.ProfileExportLevel != "" {
		args = append(args, "--profile-export-level", shellSingleQuote(cfg.ProfileExportLevel))
	}
	if cfg.RecordProcessors > 0 {
		args = append(args, "--record-processors", fmt.Sprintf("%d", cfg.RecordProcessors))
	}
	if cfg.Goodput != "" {
		args = append(args, "--goodput", shellSingleQuote(cfg.Goodput))
	}

	if cfg.Streaming {
		args = append(args, "--streaming")
	}

	if cfg.HostHeader != "" {
		args = append(args, "--header", shellSingleQuote(fmt.Sprintf("Host:%s", cfg.HostHeader)))
	}

	args = append(args,
		"--output-artifact-dir", shellSingleQuote(artifactDir),
		"--ui", "none",
	)

	aiperfInvocation := strings.Join(args, " ")

	// Compose the full remote pipeline:
	//   ulimit (best-effort) → env → download → dilate → aiperf → cat newest JSON
	//
	// The cat uses ls -t to find the newest profile_export_aiperf.json under
	// any subdir of artifactDir, mirroring forge_agent's _find_result_json glob.
	cmd := fmt.Sprintf(
		"ulimit -n 65536 2>/dev/null || true; "+
			"export AIPERF_HTTP_CONNECTION_LIMIT=%d; "+
			"curl -fsSL %s -o %s; "+
			"python3.11 -c %s < %s > %s; "+
			"%s >/tmp/aiperf.stderr 2>&1; "+
			`cat "$(ls -t %s/*/profile_export_aiperf.json 2>/dev/null | head -1)"`,
		workersMax,
		shellSingleQuote(cfg.TraceURL),
		shellSingleQuote(rawFile),
		shellSingleQuote(dilationScript),
		shellSingleQuote(rawFile),
		shellSingleQuote(dilatedFile),
		aiperfInvocation,
		shellSingleQuote(artifactDir),
	)

	return cmd, nil
}

// buildAiperfCmd constructs the remote shell command that:
//  1. Removes any stale artifact dir
//  2. Runs aiperf profile to write artifacts
//  3. Cats the profile_export_aiperf.json to stdout
//
// This is required because aiperf 0.10.0 writes output to files, NOT stdout.
// The VIP is taken from opts.ProbeOptions.VIP.
//
// When cfg.TraceURL is set (mooncake trace path), dispatches to buildMooncakeCmd
// which emits the download+dilation+aiperf pipeline. Panics are not possible
// since buildMooncakeCmd only errors on bad URL schemes, which are validated before
// RunAiperf is called.
//
// aiperf prereq: the jumphost must have aiperf 0.10.0+ installed via
// python3.11 -m pip install aiperf. See EnsureAiperf.
func buildAiperfCmd(opts AiperfRunOptions) string {
	// Dispatch to the trace-driven path when TraceURL is set.
	if opts.Config.TraceURL != "" {
		cmd, err := buildMooncakeCmd(opts)
		if err != nil {
			// Caller should have validated before calling; surface as a detectable string.
			return fmt.Sprintf("echo 'buildAiperfCmd error: %s'; exit 1", err)
		}
		return cmd
	}

	cfg := opts.Config
	if cfg.EndpointType == "" {
		cfg.EndpointType = "chat"
	}
	// Closed-loop defaults apply only when no rate or duration drives the run:
	// a forced --concurrency 1 would cap an open-loop run at one in flight, and
	// a forced --request-count would end a timed step early.
	openLoop := cfg.RequestRate > 0 || cfg.BenchmarkDuration > 0 || cfg.Scenario != ""
	if cfg.Concurrency <= 0 && !openLoop {
		cfg.Concurrency = 1
	}
	if cfg.NumRequests <= 0 && !openLoop {
		cfg.NumRequests = 10
	}
	// aiperf cycles through 100 prompts by default; on a timed run that set fits in a
	// KV cache whole and every request hits. Generate one per request instead.
	if cfg.NumDatasetEntries <= 0 && cfg.NumRequests <= 0 && cfg.RequestRate > 0 && cfg.BenchmarkDuration > 0 {
		cfg.NumDatasetEntries = int(math.Ceil(cfg.RequestRate * (cfg.WarmupDuration + cfg.BenchmarkDuration)))
	}
	if cfg.ISL <= 0 {
		cfg.ISL = 512
	}
	if cfg.OSL <= 0 {
		cfg.OSL = 128
	}
	if cfg.Tokenizer == "" {
		cfg.Tokenizer = "NousResearch/Meta-Llama-3-8B-Instruct"
	}

	vip := opts.ProbeOptions.VIP

	// Unique artifact directory per run to avoid collisions.
	dirSuffix := "run"
	if opts.ResultID != "" {
		// Sanitize resultID to be shell-safe (keep alphanumeric + dashes).
		safe := strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
				return r
			}
			return '-'
		}, opts.ResultID)
		dirSuffix = safe
	}
	artifactDir := fmt.Sprintf("/tmp/aiperf-%s", dirSuffix)

	// Build the aiperf profile invocation.
	args := []string{
		"aiperf", "profile",
		"-m", shellSingleQuote(cfg.Model),
		"-u", shellSingleQuote(fmt.Sprintf("http://%s", vip)),
		"--endpoint-type", shellSingleQuote(cfg.EndpointType),
	}
	if cfg.Concurrency > 0 {
		args = append(args, "--concurrency", fmt.Sprintf("%d", cfg.Concurrency))
	}
	if cfg.NumRequests > 0 {
		args = append(args, "--request-count", fmt.Sprintf("%d", cfg.NumRequests))
	}
	if cfg.RequestRate > 0 {
		args = append(args, "--request-rate", strconv.FormatFloat(cfg.RequestRate, 'g', -1, 64))
	}
	if cfg.ArrivalPattern != "" {
		args = append(args, "--arrival-pattern", shellSingleQuote(cfg.ArrivalPattern))
	}
	if cfg.WarmupDuration > 0 {
		args = append(args, "--warmup-duration", strconv.FormatFloat(cfg.WarmupDuration, 'g', -1, 64))
	}
	if cfg.BenchmarkDuration > 0 {
		args = append(args, "--benchmark-duration", strconv.FormatFloat(cfg.BenchmarkDuration, 'g', -1, 64))
	}
	if cfg.Goodput != "" {
		args = append(args, "--goodput", shellSingleQuote(cfg.Goodput))
	}
	if cfg.RandomSeed > 0 {
		args = append(args, "--random-seed", fmt.Sprintf("%d", cfg.RandomSeed))
	}

	// When Scenario is set, emit --scenario (and scenario-specific options) and omit
	// the synthetic prompt flags.
	if cfg.Scenario != "" {
		args = append(args, "--scenario", shellSingleQuote(cfg.Scenario))
		if cfg.PublicDataset != "" {
			args = append(args, "--public-dataset", shellSingleQuote(cfg.PublicDataset))
		}
		if cfg.UseServerTokenCount {
			args = append(args, "--use-server-token-count")
		}
		if cfg.MaxContextLength > 0 {
			args = append(args, "--max-context-length", fmt.Sprintf("%d", cfg.MaxContextLength))
		}
		if cfg.UnsafeOverride {
			args = append(args, "--unsafe-override")
		}
	} else if cfg.SeqDist != "" {
		// When SeqDist is set, emit --seq-dist and omit the ISL/OSL mean flags
		// (seq-dist drives both input and output length).
		args = append(args, "--seq-dist", shellSingleQuote(cfg.SeqDist))
	} else {
		args = append(args,
			"--synthetic-input-tokens-mean", fmt.Sprintf("%d", cfg.ISL),
			"--output-tokens-mean", fmt.Sprintf("%d", cfg.OSL),
		)
	}

	if cfg.Scenario == "" && cfg.ISLStddev > 0 {
		args = append(args, "--synthetic-input-tokens-stddev", fmt.Sprintf("%d", cfg.ISLStddev))
	}

	if cfg.Scenario == "" && cfg.PrefixPromptLength > 0 {
		args = append(args, "--prefix-prompt-length", fmt.Sprintf("%d", cfg.PrefixPromptLength))
	}

	if cfg.Scenario == "" && cfg.NumPrefixPrompts > 0 {
		args = append(args, "--num-prefix-prompts", fmt.Sprintf("%d", cfg.NumPrefixPrompts))
	}

	if cfg.Scenario == "" && cfg.NumDatasetEntries > 0 {
		args = append(args, "--num-dataset-entries", fmt.Sprintf("%d", cfg.NumDatasetEntries))
	}

	for _, ei := range cfg.ExtraInputs {
		args = append(args, "--extra-inputs", shellSingleQuote(ei))
	}

	args = append(args, "--tokenizer", shellSingleQuote(cfg.Tokenizer))

	if cfg.HostHeader != "" {
		args = append(args, "--header", shellSingleQuote(fmt.Sprintf("Host:%s", cfg.HostHeader)))
	}

	if cfg.Streaming {
		args = append(args, "--streaming")
	}

	args = append(args,
		"--artifact-dir", shellSingleQuote(artifactDir),
		"--ui", "none",
	)

	aiperfInvocation := strings.Join(args, " ")

	// Compose the full remote command:
	// 1. Remove stale artifact dir
	// 2. Run aiperf (redirect its own stdout/stderr to /tmp to keep our stdout clean)
	// 3. Cat the JSON artifact back to stdout for capture
	envPrefix := ""
	if cfg.Scenario != "" {
		envPrefix = "export AIPERF_DATASET_CONFIGURATION_TIMEOUT=1800 AIPERF_SERVICE_PROFILE_CONFIGURE_TIMEOUT=1800 AIPERF_HTTP_X_SESSION_ID_FROM_CORRELATION_ID=1; "
	}
	return fmt.Sprintf(
		"rm -rf %s; %s%s >/tmp/aiperf.stderr 2>&1; cat %s/profile_export_aiperf.json",
		shellSingleQuote(artifactDir),
		envPrefix,
		aiperfInvocation,
		shellSingleQuote(artifactDir),
	)
}

// RunAiperf mints an ephemeral EICE key, SSHes to the jumphost, runs
// aiperf with the given configuration, and parses the JSON result.
//
// Prerequisites on the jumphost: aiperf 0.10.0+ must be installed via
// EnsureAiperf before calling RunAiperf.
//
// The aiperf command runs synchronously over the SSH session; the session
// lifetime is bounded by opts.Config.Timeout (default 5 min).
func RunAiperf(ctx context.Context, opts AiperfRunOptions) (*AiperfResult, error) {
	if opts.Config.Timeout <= 0 {
		opts.Config.Timeout = 5 * time.Minute
	}
	if opts.ProbeOptions.User == "" {
		opts.ProbeOptions.User = DefaultSSHUser
	}

	// Validate trace URL scheme before doing any network operations.
	if opts.Config.TraceURL != "" {
		if err := validateTraceURL(opts.Config.TraceURL); err != nil {
			return nil, fmt.Errorf("aiperf: %w", err)
		}
	}

	keyPath, pubKeyPath, cleanup, err := prepareProbeKey(ctx, opts.ProbeOptions)
	if err != nil {
		return nil, fmt.Errorf("aiperf: prepare EICE key: %w", err)
	}
	defer cleanup()

	// Re-push public key to reset the ~60 s EICE TTL before the
	// long-running aiperf session starts.
	if pubKeyPath != "" {
		_ = pushSSHPublicKeyFn(ctx, opts.ProbeOptions.EffectiveLocation(), opts.ProbeOptions.EffectiveVMName(), pubKeyPath)
	}

	remoteCmd := buildAiperfCmd(opts)

	// Run with a bounded context so a hung aiperf doesn't block forever.
	runCtx, cancel := context.WithTimeout(ctx, opts.Config.Timeout)
	defer cancel()

	stdout, err := aiperfSSHExecFn(runCtx, opts.ProbeOptions.EffectiveLocation(), opts.ProbeOptions.EffectiveVMName(), keyPath, remoteCmd)
	if err != nil {
		return nil, fmt.Errorf("aiperf: remote exec: %w", err)
	}

	result, err := parseAiperfJSON(stdout)
	if err != nil {
		return nil, fmt.Errorf("aiperf: parse output: %w (raw: %.500s)", err, stdout)
	}

	// Capture the raw JSON for callers that need the full aiperf output
	// (e.g. forge's /api/benchmarks/results/aiperf rich-ingest endpoint).
	// Trim leading noise lines so RawJSON always starts at '{'.
	if start := strings.Index(stdout, "{"); start >= 0 {
		result.RawJSON = stdout[start:]
	} else {
		result.RawJSON = stdout
	}

	// Backfill identity fields from config when not present in JSON.
	if result.Model == "" {
		result.Model = opts.Config.Model
	}
	if result.Endpoint == "" {
		ep := opts.Config.EndpointPath
		if ep == "" {
			ep = "/v1/chat/completions"
		}
		result.Endpoint = ep
	}
	if result.BaseURL == "" {
		result.BaseURL = fmt.Sprintf("http://%s", opts.ProbeOptions.VIP)
	}

	return result, nil
}

// EnsureAiperf checks whether aiperf >=0.10.0 is available on the jumphost
// and installs it via python3.11 -m pip install aiperf when absent or outdated.
//
// AL2023 ships Python 3.9; aiperf requires >=3.10. We install python3.11 via
// dnf (available on AL2023) and use it to pip install aiperf.
// Note: on AL2023, `dnf install python3.11` does NOT include pip — we also
// install python3.11-pip explicitly, and fall back to `ensurepip --user`
// in case the package name differs across AL2023 minor versions.
// The console script lands at ~/.local/bin/aiperf which is on the user's PATH.
//
// Guard: runs `aiperf --version` first; only installs if the version is
// missing or is the 0.1.0 placeholder (the spurious PyPI package).
func EnsureAiperf(ctx context.Context, probOpts ProbeOptions) error {
	if probOpts.User == "" {
		probOpts.User = DefaultSSHUser
	}

	keyPath, pubKeyPath, cleanup, err := prepareProbeKey(ctx, probOpts)
	if err != nil {
		return fmt.Errorf("ensure aiperf: prepare EICE key: %w", err)
	}
	defer cleanup()

	if pubKeyPath != "" {
		_ = pushSSHPublicKeyFn(ctx, probOpts.EffectiveLocation(), probOpts.EffectiveVMName(), pubKeyPath)
	}

	// Guarded install sequence:
	// 1. Run `aiperf --version`; if it prints 0.13.x (the version Forge's own
	//    agent pins; open-loop flags such as --arrival-pattern need it), done.
	// 2. Otherwise install python3.11 (AL2023 dnf) + pip install aiperf==0.13.0.
	// 3. Re-verify; fail if still absent or placeholder.
	//
	// We detect the placeholder by checking for "0.1.0" in the version output.
	checkCmd := `
VER=$(aiperf --version 2>/dev/null || true)
if echo "$VER" | grep -q '0\.13\.'; then
  echo "ok:$VER"
else
  if command -v dnf >/dev/null 2>&1; then
    sudo dnf install -y python3.11 python3.11-pip >/dev/null 2>&1
  elif command -v apt-get >/dev/null 2>&1; then
    sudo apt-get update >/dev/null 2>&1 && sudo apt-get install -y python3-pip python3-venv >/dev/null 2>&1
  fi
  python3.11 -m ensurepip --user >/dev/null 2>&1 || true
  python3 -m pip install --user aiperf==0.13.0 >/tmp/aiperf_install.log 2>&1 || python3.11 -m pip install --user aiperf==0.13.0 >/tmp/aiperf_install.log 2>&1
  VER2=$(aiperf --version 2>/dev/null || true)
  if echo "$VER2" | grep -qv '^$' && ! echo "$VER2" | grep -q '0\.1\.0'; then
    echo "installed:$VER2"
  else
    echo "FAILED:$VER2"
    tail -3 /tmp/aiperf_install.log 2>/dev/null
    exit 1
  fi
fi`

	out, err := aiperfSSHExecFn(ctx, probOpts.EffectiveLocation(), probOpts.EffectiveVMName(), keyPath, checkCmd)
	if err != nil {
		return fmt.Errorf("ensure aiperf: install failed: %w (output: %s)", err, strings.TrimSpace(out))
	}
	out = strings.TrimSpace(out)
	if strings.HasPrefix(out, "FAILED") {
		return fmt.Errorf("ensure aiperf: aiperf not available or placeholder after install (output: %s)", out)
	}
	return nil
}

// parseAiperfJSON parses the JSON blob emitted by aiperf 0.10.0's
// profile_export_aiperf.json artifact (captured via `cat`).
// The JSON may be preceded by stray lines — we scan for the first '{'.
func parseAiperfJSON(raw string) (*AiperfResult, error) {
	start := strings.Index(raw, "{")
	if start < 0 {
		return nil, fmt.Errorf("no JSON object found in aiperf output")
	}

	var raw0 aiperfRawOutput
	if err := json.Unmarshal([]byte(raw[start:]), &raw0); err != nil {
		return nil, err
	}

	// Derive request counts from the data we have.
	total := int(raw0.RequestCount.Avg)
	failed := len(raw0.ErrorSummary)
	successful := total - failed
	if successful < 0 {
		successful = 0
	}

	dur := raw0.BenchmarkDuration.Avg
	result := &AiperfResult{
		AiperfVersion: raw0.AiperfVersion,
		SchemaVersion: raw0.SchemaVersion,
		BenchmarkID:   raw0.BenchmarkID,
		StartTime:     raw0.StartTime,
		EndTime:       raw0.EndTime,
		WasCancelled:  raw0.WasCancelled,
		ErrorSummary:  raw0.ErrorSummary,

		TotalRequests:   total,
		Successful:      successful,
		Failed:          failed,
		DurationSeconds: dur,
		DurationMinutes: dur / 60,

		RequestThroughput:     raw0.RequestThroughput.Avg,
		OutputTokenThroughput: raw0.OutputTokenThroughput.Avg,

		RequestLatency: DistributionStats{
			Unit: raw0.RequestLatency.Unit,
			Avg:  raw0.RequestLatency.Avg,
			P50:  raw0.RequestLatency.P50,
			P90:  raw0.RequestLatency.P90,
			P95:  raw0.RequestLatency.P95,
			P99:  raw0.RequestLatency.P99,
			Min:  raw0.RequestLatency.Min,
			Max:  raw0.RequestLatency.Max,
		},
		TTFT: DistributionStats{
			Unit: raw0.TimeToFirstToken.Unit,
			Avg:  raw0.TimeToFirstToken.Avg,
			P50:  raw0.TimeToFirstToken.P50,
			P90:  raw0.TimeToFirstToken.P90,
			P95:  raw0.TimeToFirstToken.P95,
			P99:  raw0.TimeToFirstToken.P99,
			Min:  raw0.TimeToFirstToken.Min,
			Max:  raw0.TimeToFirstToken.Max,
		},
		ITL: DistributionStats{
			Unit: raw0.InterTokenLatency.Unit,
			Avg:  raw0.InterTokenLatency.Avg,
			P50:  raw0.InterTokenLatency.P50,
			P90:  raw0.InterTokenLatency.P90,
			P95:  raw0.InterTokenLatency.P95,
			P99:  raw0.InterTokenLatency.P99,
			Min:  raw0.InterTokenLatency.Min,
			Max:  raw0.InterTokenLatency.Max,
		},

		AvgInputTokens:    raw0.InputSequenceLength.Avg,
		AvgOutputTokens:   raw0.OutputSequenceLength.Avg,
		TotalOutputTokens: raw0.TotalOutputTokens.Avg,
	}

	return result, nil
}

// FitTimeoutToDuration raises Timeout so a timed (open-loop) or scenario step keeps its SSH
// session for warmup + duration + aiperf's 30 s grace period, plus start-up, dataset preparation,
// and export; it never cuts a timed step short.
func (c *AiperfConfig) FitTimeoutToDuration() {
	if c.BenchmarkDuration <= 0 && c.Scenario == "" {
		return
	}
	baseDuration := c.BenchmarkDuration
	if baseDuration <= 0 && c.Scenario != "" {
		baseDuration = 900
	}
	extra := 5 * time.Minute
	if c.Scenario != "" {
		// Scenarios like AgentX MVP download and prepare datasets on the jumphost,
		// which may take significant time on first run.
		extra = 35 * time.Minute
	}
	need := time.Duration(c.WarmupDuration+baseDuration+30)*time.Second + extra
	if c.Timeout < need {
		c.Timeout = need
	}
}
