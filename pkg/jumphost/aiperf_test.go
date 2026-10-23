package jumphost_test

import (
	"context"
	"strings"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/jumphost"
)

// ---------------------------------------------------------------------------
// buildAiperfCmd — command construction
// ---------------------------------------------------------------------------

func TestBuildAiperfCmd_DefaultsAndRequiredFlags(t *testing.T) {
	opts := jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{
			VIP: "10.0.10.100",
			// SourceIP intentionally set but should NOT produce --interface
			SourceIP: "10.0.11.50",
		},
		Config: jumphost.AiperfConfig{
			Model:      "llama3",
			HostHeader: "awsbnkctl-aiinference.local",
		},
	}

	cmd := jumphost.BuildAiperfCmd(opts)

	mustContain := []string{
		"aiperf profile",
		"-m", "llama3",
		"-u", "http://10.0.10.100",
		"--endpoint-type", "chat",
		"--concurrency", "1",
		"--request-count", "10",
		"--synthetic-input-tokens-mean", "512",
		"--output-tokens-mean", "128",
		"--tokenizer",
		"NousResearch/Meta-Llama-3-8B-Instruct",
		"--header", "Host:awsbnkctl-aiinference.local",
		"--artifact-dir",
		"--ui", "none",
		"cat",
		"profile_export_aiperf.json",
	}
	for _, want := range mustContain {
		if !strings.Contains(cmd, want) {
			t.Errorf("buildAiperfCmd missing %q; cmd=%q", want, cmd)
		}
	}

	// Must NOT contain removed flags.
	mustNotContain := []string{
		"--base-url", "--num-users", "--num-requests", "--input-len", "--output-len",
		"--output-format", "--interface", "--run-label",
	}
	for _, bad := range mustNotContain {
		if strings.Contains(cmd, bad) {
			t.Errorf("buildAiperfCmd must NOT contain %q (removed flag); cmd=%q", bad, cmd)
		}
	}
}

func TestBuildAiperfCmd_StreamingFlag(t *testing.T) {
	opts := jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{VIP: "10.0.10.100"},
		Config: jumphost.AiperfConfig{
			Model:     "llama3",
			Streaming: true,
		},
	}
	cmd := jumphost.BuildAiperfCmd(opts)
	if !strings.Contains(cmd, "--streaming") {
		t.Errorf("buildAiperfCmd missing --streaming when Streaming=true; cmd=%q", cmd)
	}
	// Must NOT use the old --stream flag
	if strings.Contains(cmd, " --stream ") || strings.Contains(cmd, " --stream\n") {
		t.Errorf("buildAiperfCmd must use --streaming not --stream; cmd=%q", cmd)
	}
}

func TestBuildAiperfCmd_NoStreamByDefault(t *testing.T) {
	opts := jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{VIP: "10.0.10.100"},
		Config:       jumphost.AiperfConfig{Model: "llama3"},
	}
	cmd := jumphost.BuildAiperfCmd(opts)
	if strings.Contains(cmd, "--streaming") {
		t.Errorf("buildAiperfCmd must NOT contain --streaming when Streaming=false; cmd=%q", cmd)
	}
}

func TestBuildAiperfCmd_CustomConcurrencyAndRequests(t *testing.T) {
	opts := jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{VIP: "10.0.10.100"},
		Config: jumphost.AiperfConfig{
			Model:       "llama3",
			Concurrency: 4,
			NumRequests: 100,
			ISL:         256,
			OSL:         64,
		},
	}
	cmd := jumphost.BuildAiperfCmd(opts)
	for _, want := range []string{
		"--concurrency 4",
		"--request-count 100",
		"--synthetic-input-tokens-mean 256",
		"--output-tokens-mean 64",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("buildAiperfCmd missing %q; cmd=%q", want, cmd)
		}
	}
}

func TestBuildAiperfCmd_NoInterfaceEvenWithSourceIP(t *testing.T) {
	// SourceIP is kept on ProbeOptions but must NOT produce --interface
	// (the VIP is on-link on the jumphost external-ENI subnet).
	opts := jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{VIP: "10.0.10.100", SourceIP: "10.0.11.50"},
		Config:       jumphost.AiperfConfig{Model: "llama3"},
	}
	cmd := jumphost.BuildAiperfCmd(opts)
	if strings.Contains(cmd, "--interface") {
		t.Errorf("buildAiperfCmd must NOT include --interface (removed); cmd=%q", cmd)
	}
}

func TestBuildAiperfCmd_HostHeaderWhenSet(t *testing.T) {
	opts := jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{VIP: "10.0.10.100"},
		Config: jumphost.AiperfConfig{
			Model:      "llama3",
			HostHeader: "myroute.local",
		},
	}
	cmd := jumphost.BuildAiperfCmd(opts)
	if !strings.Contains(cmd, "--header") {
		t.Errorf("buildAiperfCmd missing --header when HostHeader set; cmd=%q", cmd)
	}
	if !strings.Contains(cmd, "Host:myroute.local") {
		t.Errorf("buildAiperfCmd missing Host:myroute.local; cmd=%q", cmd)
	}
}

func TestBuildAiperfCmd_NoHostHeaderWhenEmpty(t *testing.T) {
	opts := jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{VIP: "10.0.10.100"},
		Config:       jumphost.AiperfConfig{Model: "llama3"},
	}
	cmd := jumphost.BuildAiperfCmd(opts)
	if strings.Contains(cmd, "--header") {
		t.Errorf("buildAiperfCmd must NOT include --header when HostHeader empty; cmd=%q", cmd)
	}
}

func TestBuildAiperfCmd_CustomTokenizer(t *testing.T) {
	opts := jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{VIP: "10.0.10.100"},
		Config: jumphost.AiperfConfig{
			Model:     "llama3",
			Tokenizer: "meta-llama/Llama-3-8B",
		},
	}
	cmd := jumphost.BuildAiperfCmd(opts)
	if !strings.Contains(cmd, "meta-llama/Llama-3-8B") {
		t.Errorf("buildAiperfCmd must use custom tokenizer; cmd=%q", cmd)
	}
}

func TestBuildAiperfCmd_AgentXMvpScenario(t *testing.T) {
	opts := jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{VIP: "10.0.10.100"},
		Config: jumphost.AiperfConfig{
			Model:               "meta-llama/Llama-3.1-70B-Instruct",
			Scenario:            "inferencex-agentx-mvp",
			PublicDataset:       "semianalysis_cc_traces_weka_with_subagents",
			EndpointType:        "chat",
			Streaming:           true,
			UseServerTokenCount: true,
			BenchmarkDuration:   900,
			RandomSeed:          20260707,
			Concurrency:         16,
		},
	}
	cmd := jumphost.BuildAiperfCmd(opts)

	mustContain := []string{
		"--scenario 'inferencex-agentx-mvp'",
		"--public-dataset 'semianalysis_cc_traces_weka_with_subagents'",
		"--endpoint-type 'chat'",
		"--streaming",
		"--use-server-token-count",
		"--benchmark-duration 900",
		"--random-seed 20260707",
		"--concurrency 16",
		"export AIPERF_DATASET_CONFIGURATION_TIMEOUT=1800",
		"AIPERF_SERVICE_PROFILE_CONFIGURE_TIMEOUT=1800",
		"AIPERF_HTTP_X_SESSION_ID_FROM_CORRELATION_ID=1",
	}
	for _, want := range mustContain {
		if !strings.Contains(cmd, want) {
			t.Errorf("buildAiperfCmd missing %q; cmd=%q", want, cmd)
		}
	}

	mustNotContain := []string{
		"--synthetic-input-tokens-mean",
		"--output-tokens-mean",
		"--seq-dist",
		"--request-count",
	}
	for _, bad := range mustNotContain {
		if strings.Contains(cmd, bad) {
			t.Errorf("buildAiperfCmd must NOT contain %q for scenario; cmd=%q", bad, cmd)
		}
	}
}

// ---------------------------------------------------------------------------
// parseAiperfJSON / RunAiperf with stubbed SSH exec
// ---------------------------------------------------------------------------

// minimalAiperfJSON is a real aiperf 0.10.0 profile_export_aiperf.json fixture.
// Pre-pended noise tests that parseAiperfJSON scans for '{'.
const minimalAiperfJSON = `some info line
another info line
{
  "schema_version": "1.3",
  "aiperf_version": "0.10.0",
  "benchmark_id": "f43cfc1c6cce",
  "request_throughput": {"unit": "requests/sec", "avg": 0.8233},
  "request_latency": {
    "unit": "ms",
    "avg": 2413.5,
    "p50": 2431.75,
    "p90": 2438.5,
    "p99": 2439.45,
    "min": 2358.58,
    "max": 2439.55,
    "std": 30.01,
    "count": 10,
    "sum": 24135.8
  },
  "request_count": {"unit": "requests", "avg": 10.0},
  "time_to_first_token": {
    "unit": "ms",
    "avg": 195.84,
    "p50": 175.05,
    "p90": 251.45,
    "p99": 253.45,
    "min": 126.76,
    "max": 253.67,
    "std": 46.21,
    "count": 10,
    "sum": 1958.4
  },
  "inter_token_latency": {
    "unit": "ms",
    "avg": 35.2,
    "p50": 34.73,
    "p90": 35.95,
    "p99": 35.96,
    "min": 34.66,
    "max": 35.96,
    "std": 0.60,
    "count": 10,
    "sum": 352.0
  },
  "output_token_throughput": {"unit": "tokens/sec", "avg": 52.69},
  "output_sequence_length": {"unit": "tokens", "avg": 64.0, "p50": 64.0, "p90": 64.0, "p99": 64.0, "min": 64.0, "max": 64.0, "std": 0.0, "count": 10, "sum": 640.0},
  "input_sequence_length":  {"unit": "tokens", "avg": 256.0, "p50": 256.0, "p90": 256.0, "p99": 256.0, "min": 256.0, "max": 256.0, "std": 0.0, "count": 10, "sum": 2560.0},
  "total_output_tokens":    {"unit": "tokens", "avg": 640.0},
  "benchmark_duration":     {"unit": "sec", "avg": 12.14},
  "start_time": "2026-06-13T05:11:41.465270",
  "end_time":   "2026-06-13T05:11:53.616416",
  "was_cancelled": false,
  "error_summary": []
}`

// TestRunAiperf_SSHExecStubbed verifies that RunAiperf:
//  1. calls the SSH exec seam with the constructed aiperf command
//  2. parses the JSON result back into an AiperfResult
func TestRunAiperf_SSHExecStubbed(t *testing.T) {
	// Capture what command was passed to the SSH exec seam.
	var capturedCmd string

	// Stub: replace prepareEICEKey, pushSSHPublicKey, and aiperfSSHExec seams.
	origPrepare := *jumphost.PrepareEICEKeyFn
	*jumphost.PrepareEICEKeyFn = func(_ context.Context, _, _ string) (string, string, func(), error) {
		return "/fake/key", "/fake/key.pub", func() {}, nil
	}
	defer func() { *jumphost.PrepareEICEKeyFn = origPrepare }()

	origPush := *jumphost.PushSSHPublicKeyFn
	*jumphost.PushSSHPublicKeyFn = func(_ context.Context, _, _, _ string) error { return nil }
	defer func() { *jumphost.PushSSHPublicKeyFn = origPush }()

	origExec := *jumphost.AiperfSSHExecFn
	*jumphost.AiperfSSHExecFn = func(_ context.Context, _, _, _, cmd string) (string, error) {
		capturedCmd = cmd
		return minimalAiperfJSON, nil
	}
	defer func() { *jumphost.AiperfSSHExecFn = origExec }()

	opts := jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{
			Region:     "ap-southeast-2",
			InstanceID: "i-0abc123",
			VIP:        "10.0.10.100",
			SourceIP:   "10.0.11.50",
		},
		Config: jumphost.AiperfConfig{
			Model:       "llama3",
			Concurrency: 2,
			NumRequests: 20,
		},
	}

	result, err := jumphost.RunAiperf(context.Background(), opts)
	if err != nil {
		t.Fatalf("RunAiperf: %v", err)
	}

	// Assert the command was passed to the SSH exec seam.
	if !strings.Contains(capturedCmd, "aiperf profile") {
		t.Errorf("SSH exec was not called with 'aiperf profile'; got %q", capturedCmd)
	}
	if !strings.Contains(capturedCmd, "--concurrency 2") {
		t.Errorf("command missing --concurrency 2; got %q", capturedCmd)
	}
	if !strings.Contains(capturedCmd, "--endpoint-type") {
		t.Errorf("command missing --endpoint-type; got %q", capturedCmd)
	}

	// Assert the result was parsed.
	if result == nil {
		t.Fatal("RunAiperf returned nil result")
	}
	if result.TotalRequests != 10 {
		t.Errorf("TotalRequests = %d, want 10", result.TotalRequests)
	}
	if result.RequestLatency.P50 != 2431.75 {
		t.Errorf("RequestLatency.P50 = %v, want 2431.75", result.RequestLatency.P50)
	}
	if result.TTFT.P50 != 175.05 {
		t.Errorf("TTFT.P50 = %v, want 175.05", result.TTFT.P50)
	}
	if result.OutputTokenThroughput != 52.69 {
		t.Errorf("OutputTokenThroughput = %v, want 52.69", result.OutputTokenThroughput)
	}
}

// TestParseAiperfJSON_Fixture tests the pure JSON-parsing function against the
// golden fixture above, asserting the mapping of all forwarded fields.
func TestParseAiperfJSON_Fixture(t *testing.T) {
	origPrepare := *jumphost.PrepareEICEKeyFn
	*jumphost.PrepareEICEKeyFn = func(_ context.Context, _, _ string) (string, string, func(), error) {
		return "/k", "/k.pub", func() {}, nil
	}
	defer func() { *jumphost.PrepareEICEKeyFn = origPrepare }()

	origPush := *jumphost.PushSSHPublicKeyFn
	*jumphost.PushSSHPublicKeyFn = func(_ context.Context, _, _, _ string) error { return nil }
	defer func() { *jumphost.PushSSHPublicKeyFn = origPush }()

	origExec := *jumphost.AiperfSSHExecFn
	*jumphost.AiperfSSHExecFn = func(_ context.Context, _, _, _, _ string) (string, error) {
		return minimalAiperfJSON, nil
	}
	defer func() { *jumphost.AiperfSSHExecFn = origExec }()

	result, err := jumphost.RunAiperf(context.Background(), jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{Region: "r", InstanceID: "i", VIP: "10.0.10.100"},
		Config:       jumphost.AiperfConfig{Model: "llama3"},
	})
	if err != nil {
		t.Fatalf("RunAiperf: %v", err)
	}

	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"Model", result.Model, "llama3"},
		{"BaseURL", result.BaseURL, "http://10.0.10.100"},
		{"AiperfVersion", result.AiperfVersion, "0.10.0"},
		{"SchemaVersion", result.SchemaVersion, "1.3"},
		{"BenchmarkID", result.BenchmarkID, "f43cfc1c6cce"},
		{"TotalRequests", result.TotalRequests, 10},
		{"Successful", result.Successful, 10},
		{"Failed", result.Failed, 0},
		{"DurationSeconds", result.DurationSeconds, 12.14},
		{"RequestLatency.P50", result.RequestLatency.P50, 2431.75},
		{"RequestLatency.P99", result.RequestLatency.P99, 2439.45},
		{"TTFT.Avg", result.TTFT.Avg, 195.84},
		{"TTFT.P50", result.TTFT.P50, 175.05},
		{"ITL.Avg", result.ITL.Avg, 35.2},
		{"RequestThroughput", result.RequestThroughput, 0.8233},
		{"OutputTokenThroughput", result.OutputTokenThroughput, 52.69},
		{"AvgInputTokens", result.AvgInputTokens, 256.0},
		{"AvgOutputTokens", result.AvgOutputTokens, 64.0},
		{"TotalOutputTokens", result.TotalOutputTokens, 640.0},
		{"WasCancelled", result.WasCancelled, false},
		{"StartTime", result.StartTime, "2026-06-13T05:11:41.465270"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}
}

// TestRunAiperf_BackfillsIdentityFromConfig verifies that when aiperf omits
// model/base_url/endpoint fields, RunAiperf backfills them from the config.
func TestRunAiperf_BackfillsIdentityFromConfig(t *testing.T) {
	origPrepare := *jumphost.PrepareEICEKeyFn
	*jumphost.PrepareEICEKeyFn = func(_ context.Context, _, _ string) (string, string, func(), error) {
		return "/k", "/k.pub", func() {}, nil
	}
	defer func() { *jumphost.PrepareEICEKeyFn = origPrepare }()

	origPush := *jumphost.PushSSHPublicKeyFn
	*jumphost.PushSSHPublicKeyFn = func(_ context.Context, _, _, _ string) error { return nil }
	defer func() { *jumphost.PushSSHPublicKeyFn = origPush }()

	origExec := *jumphost.AiperfSSHExecFn
	// Return minimal JSON without identity fields.
	*jumphost.AiperfSSHExecFn = func(_ context.Context, _, _, _, _ string) (string, error) {
		return `{
			"request_count": {"unit":"requests","avg":5.0},
			"benchmark_duration": {"unit":"sec","avg":1.0},
			"request_throughput": {"unit":"requests/sec","avg":5.0},
			"request_latency": {"unit":"ms","avg":200.0,"p50":200.0,"p90":200.0,"p99":200.0,"min":100.0,"max":300.0},
			"time_to_first_token": {"unit":"ms","avg":50.0,"p50":50.0,"p90":50.0,"p99":50.0,"min":30.0,"max":70.0},
			"inter_token_latency": {"unit":"ms","avg":10.0,"p50":10.0,"p90":10.0,"p99":10.0,"min":5.0,"max":15.0},
			"output_token_throughput": {"unit":"tokens/sec","avg":50.0},
			"output_sequence_length": {"unit":"tokens","avg":64.0},
			"input_sequence_length": {"unit":"tokens","avg":256.0},
			"total_output_tokens": {"unit":"tokens","avg":320.0},
			"error_summary": [],
			"was_cancelled": false,
			"start_time": "2026-06-13T05:00:00",
			"end_time": "2026-06-13T05:00:01"
		}`, nil
	}
	defer func() { *jumphost.AiperfSSHExecFn = origExec }()

	result, err := jumphost.RunAiperf(context.Background(), jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{Region: "r", InstanceID: "i", VIP: "10.0.11.50"},
		Config: jumphost.AiperfConfig{
			Model:        "my-model",
			EndpointPath: "/v1/chat/completions",
		},
	})
	if err != nil {
		t.Fatalf("RunAiperf: %v", err)
	}
	if result.Model != "my-model" {
		t.Errorf("Model = %q, want %q (backfill)", result.Model, "my-model")
	}
	if result.BaseURL != "http://10.0.11.50" {
		t.Errorf("BaseURL = %q, want %q (backfill)", result.BaseURL, "http://10.0.11.50")
	}
	if result.Endpoint != "/v1/chat/completions" {
		t.Errorf("Endpoint = %q, want %q (backfill)", result.Endpoint, "/v1/chat/completions")
	}
}

// TestEnsureAiperf_UsesPython311 verifies that the install command uses
// python3.11 (NOT python3 -m pip) and installs via dnf + python3.11-pip +
// ensurepip fallback + python3.11 -m pip.
// AL2023 ships Python 3.9 which can't run aiperf; python3.11 is available via dnf.
// AL2023's python3.11 package does NOT bundle pip — python3.11-pip must be
// installed explicitly (with ensurepip as a belt-and-suspenders fallback).
func TestEnsureAiperf_UsesPython311(t *testing.T) {
	var capturedCmd string

	origPrepare := *jumphost.PrepareEICEKeyFn
	*jumphost.PrepareEICEKeyFn = func(_ context.Context, _, _ string) (string, string, func(), error) {
		return "/fake/key", "/fake/key.pub", func() {}, nil
	}
	defer func() { *jumphost.PrepareEICEKeyFn = origPrepare }()

	origPush := *jumphost.PushSSHPublicKeyFn
	*jumphost.PushSSHPublicKeyFn = func(_ context.Context, _, _, _ string) error { return nil }
	defer func() { *jumphost.PushSSHPublicKeyFn = origPush }()

	origExec := *jumphost.AiperfSSHExecFn
	*jumphost.AiperfSSHExecFn = func(_ context.Context, _, _, _, cmd string) (string, error) {
		capturedCmd = cmd
		return "ok:aiperf 0.10.0", nil
	}
	defer func() { *jumphost.AiperfSSHExecFn = origExec }()

	err := jumphost.EnsureAiperf(context.Background(), jumphost.ProbeOptions{
		Region:     "ap-southeast-2",
		InstanceID: "i-0abc123",
	})
	if err != nil {
		t.Fatalf("EnsureAiperf: unexpected error: %v", err)
	}

	// Must reference python3.11 in the install path.
	if !strings.Contains(capturedCmd, "python3.11") {
		t.Errorf("EnsureAiperf command must reference python3.11; got: %q", capturedCmd)
	}
	// Must reference python3.11 -m pip.
	if !strings.Contains(capturedCmd, "python3.11 -m pip install") {
		t.Errorf("EnsureAiperf command must contain %q; got: %q", "python3.11 -m pip install", capturedCmd)
	}
	// Must use dnf to install python3.11 AND python3.11-pip (AL2023 omits pip from base package).
	if !strings.Contains(capturedCmd, "dnf install") {
		t.Errorf("EnsureAiperf command must use dnf install for python3.11; got: %q", capturedCmd)
	}
	if !strings.Contains(capturedCmd, "python3.11-pip") {
		t.Errorf("EnsureAiperf command must install python3.11-pip (AL2023 python3.11 omits pip); got: %q", capturedCmd)
	}
	// Must include ensurepip fallback for belt-and-suspenders pip availability.
	if !strings.Contains(capturedCmd, "ensurepip") {
		t.Errorf("EnsureAiperf command must include ensurepip fallback; got: %q", capturedCmd)
	}
	// Must check for placeholder 0.1.0.
	if !strings.Contains(capturedCmd, "0\\.1\\.0") && !strings.Contains(capturedCmd, "0.1.0") {
		t.Errorf("EnsureAiperf must guard against 0.1.0 placeholder; got: %q", capturedCmd)
	}
}

// ---------------------------------------------------------------------------
// AiperfResult.RawJSON — verify RunAiperf populates the raw JSON field
// ---------------------------------------------------------------------------

// TestRunAiperf_RawJSONPopulated verifies that RunAiperf sets RawJSON on the
// returned result to the verbatim JSON content (trimmed to start at '{').
func TestRunAiperf_RawJSONPopulated(t *testing.T) {
	origPrepare := *jumphost.PrepareEICEKeyFn
	*jumphost.PrepareEICEKeyFn = func(_ context.Context, _, _ string) (string, string, func(), error) {
		return "/k", "/k.pub", func() {}, nil
	}
	defer func() { *jumphost.PrepareEICEKeyFn = origPrepare }()

	origPush := *jumphost.PushSSHPublicKeyFn
	*jumphost.PushSSHPublicKeyFn = func(_ context.Context, _, _, _ string) error { return nil }
	defer func() { *jumphost.PushSSHPublicKeyFn = origPush }()

	origExec := *jumphost.AiperfSSHExecFn
	*jumphost.AiperfSSHExecFn = func(_ context.Context, _, _, _, _ string) (string, error) {
		// Prepend stale lines that precede the JSON — RunAiperf must trim them.
		return "noise line\nanother noise\n" + minimalAiperfJSON[strings.Index(minimalAiperfJSON, "{"):], nil
	}
	defer func() { *jumphost.AiperfSSHExecFn = origExec }()

	result, err := jumphost.RunAiperf(context.Background(), jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{Region: "r", InstanceID: "i", VIP: "10.0.10.100"},
		Config:       jumphost.AiperfConfig{Model: "llama3"},
	})
	if err != nil {
		t.Fatalf("RunAiperf: %v", err)
	}
	if result.RawJSON == "" {
		t.Fatal("RawJSON must not be empty after RunAiperf")
	}
	if !strings.HasPrefix(result.RawJSON, "{") {
		t.Errorf("RawJSON must start at '{', got prefix %q", result.RawJSON[:min(30, len(result.RawJSON))])
	}
	// Spot-check a known key from the fixture.
	if !strings.Contains(result.RawJSON, `"schema_version"`) {
		t.Error("RawJSON missing 'schema_version' key from fixture")
	}
	if !strings.Contains(result.RawJSON, `"benchmark_id"`) {
		t.Error("RawJSON missing 'benchmark_id' key from fixture")
	}
}

// TestRunAiperf_RawJSONWithNoNoise verifies RawJSON when stdout starts
// directly with '{' (no leading noise lines).
func TestRunAiperf_RawJSONWithNoNoise(t *testing.T) {
	origPrepare := *jumphost.PrepareEICEKeyFn
	*jumphost.PrepareEICEKeyFn = func(_ context.Context, _, _ string) (string, string, func(), error) {
		return "/k", "/k.pub", func() {}, nil
	}
	defer func() { *jumphost.PrepareEICEKeyFn = origPrepare }()

	origPush := *jumphost.PushSSHPublicKeyFn
	*jumphost.PushSSHPublicKeyFn = func(_ context.Context, _, _, _ string) error { return nil }
	defer func() { *jumphost.PushSSHPublicKeyFn = origPush }()

	// Pure JSON — no noise prefix.
	pureJSON := minimalAiperfJSON[strings.Index(minimalAiperfJSON, "{"):]

	origExec := *jumphost.AiperfSSHExecFn
	*jumphost.AiperfSSHExecFn = func(_ context.Context, _, _, _, _ string) (string, error) {
		return pureJSON, nil
	}
	defer func() { *jumphost.AiperfSSHExecFn = origExec }()

	result, err := jumphost.RunAiperf(context.Background(), jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{Region: "r", InstanceID: "i", VIP: "10.0.10.100"},
		Config:       jumphost.AiperfConfig{Model: "llama3"},
	})
	if err != nil {
		t.Fatalf("RunAiperf: %v", err)
	}
	if result.RawJSON != pureJSON {
		t.Errorf("RawJSON mismatch: got len=%d, want len=%d", len(result.RawJSON), len(pureJSON))
	}
}

// ---------------------------------------------------------------------------
// New AiperfConfig fields — flag emission and byte-compat regression guard
// ---------------------------------------------------------------------------

// TestBuildAiperfCmd_ByteCompat_ZeroNewFields asserts that when all new
// AiperfConfig fields (ISLStddev, SeqDist, PrefixPromptLength, NumPrefixPrompts,
// ExtraInputs, VariantLabel) are zero/empty, the command output is byte-identical
// to the output produced before the new fields were added.
//
// This is the regression guard: existing runs must be unaffected.
func TestBuildAiperfCmd_ByteCompat_ZeroNewFields(t *testing.T) {
	baseOpts := jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{VIP: "10.0.10.100"},
		Config: jumphost.AiperfConfig{
			Model:       "llama3",
			Concurrency: 4,
			NumRequests: 100,
			ISL:         512,
			OSL:         128,
			Streaming:   true,
			Tokenizer:   "NousResearch/Meta-Llama-3-8B-Instruct",
		},
	}

	// Explicit zero values for new fields — should be identical to above.
	newFieldsOpts := jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{VIP: "10.0.10.100"},
		Config: jumphost.AiperfConfig{
			Model:              "llama3",
			Concurrency:        4,
			NumRequests:        100,
			ISL:                512,
			OSL:                128,
			ISLStddev:          0,
			SeqDist:            "",
			PrefixPromptLength: 0,
			NumPrefixPrompts:   0,
			ExtraInputs:        nil,
			VariantLabel:       "",
			Streaming:          true,
			Tokenizer:          "NousResearch/Meta-Llama-3-8B-Instruct",
		},
	}

	const wantCmd = "rm -rf '/tmp/aiperf-run'; aiperf profile -m 'llama3' -u 'http://10.0.10.100' --endpoint-type 'chat' --concurrency 4 --request-count 100 --synthetic-input-tokens-mean 512 --output-tokens-mean 128 --tokenizer 'NousResearch/Meta-Llama-3-8B-Instruct' --streaming --artifact-dir '/tmp/aiperf-run' --ui none >/tmp/aiperf.stderr 2>&1; cat '/tmp/aiperf-run'/profile_export_aiperf.json"

	cmdBase := jumphost.BuildAiperfCmd(baseOpts)
	cmdNew := jumphost.BuildAiperfCmd(newFieldsOpts)

	// Golden-literal assertion: catches flag reorders or additions that would
	// affect both in-test structs identically and slip through the struct-equality check.
	if cmdBase != wantCmd {
		t.Errorf("byte-compat regression: BuildAiperfCmd output diverged from pinned golden\ngot:  %q\nwant: %q", cmdBase, wantCmd)
	}

	if cmdBase != cmdNew {
		t.Errorf("byte-compat regression: new zero fields changed command output\ngot:  %q\nwant: %q", cmdNew, cmdBase)
	}

	// Additionally assert that no new flags appear in the base output.
	for _, flag := range []string{"--synthetic-input-tokens-stddev", "--seq-dist", "--prefix-prompt-length", "--num-prefix-prompts", "--extra-inputs"} {
		if strings.Contains(cmdBase, flag) {
			t.Errorf("regression: %q should not appear when new fields are zero; cmd=%q", flag, cmdBase)
		}
	}
}

// TestBuildAiperfCmd_ISLStddev asserts --synthetic-input-tokens-stddev is emitted
// when ISLStddev > 0.
func TestBuildAiperfCmd_ISLStddev(t *testing.T) {
	opts := jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{VIP: "10.0.10.100"},
		Config: jumphost.AiperfConfig{
			Model:     "llama3",
			ISL:       500,
			OSL:       128,
			ISLStddev: 50,
		},
	}
	cmd := jumphost.BuildAiperfCmd(opts)
	if !strings.Contains(cmd, "--synthetic-input-tokens-stddev 50") {
		t.Errorf("missing --synthetic-input-tokens-stddev 50; cmd=%q", cmd)
	}
	// Must still emit ISL/OSL mean when SeqDist is empty.
	if !strings.Contains(cmd, "--synthetic-input-tokens-mean") {
		t.Errorf("missing --synthetic-input-tokens-mean when SeqDist empty; cmd=%q", cmd)
	}
}

// TestBuildAiperfCmd_SeqDist_OmitsISLOSL asserts that when SeqDist is set:
//   - --seq-dist is emitted
//   - --synthetic-input-tokens-mean is OMITTED
//   - --output-tokens-mean is OMITTED
func TestBuildAiperfCmd_SeqDist_OmitsISLOSL(t *testing.T) {
	const seqDist = "300|100,64|16:70;4000|500,256|64:30"
	opts := jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{VIP: "10.0.10.100"},
		Config: jumphost.AiperfConfig{
			Model:   "llama3",
			ISL:     500, // must be ignored
			OSL:     128, // must be ignored
			SeqDist: seqDist,
		},
	}
	cmd := jumphost.BuildAiperfCmd(opts)

	if !strings.Contains(cmd, "--seq-dist") {
		t.Errorf("missing --seq-dist; cmd=%q", cmd)
	}
	if !strings.Contains(cmd, seqDist) {
		t.Errorf("seq-dist value not in command; cmd=%q", cmd)
	}
	if strings.Contains(cmd, "--synthetic-input-tokens-mean") {
		t.Errorf("--synthetic-input-tokens-mean must be omitted when SeqDist set; cmd=%q", cmd)
	}
	if strings.Contains(cmd, "--output-tokens-mean") {
		t.Errorf("--output-tokens-mean must be omitted when SeqDist set; cmd=%q", cmd)
	}
}

// TestBuildAiperfCmd_PrefixFlags asserts --prefix-prompt-length and --num-prefix-prompts
// are emitted when non-zero, and omitted when zero.
func TestBuildAiperfCmd_PrefixFlags(t *testing.T) {
	opts := jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{VIP: "10.0.10.100"},
		Config: jumphost.AiperfConfig{
			Model:              "llama3",
			PrefixPromptLength: 4000,
			NumPrefixPrompts:   20,
		},
	}
	cmd := jumphost.BuildAiperfCmd(opts)
	if !strings.Contains(cmd, "--prefix-prompt-length 4000") {
		t.Errorf("missing --prefix-prompt-length 4000; cmd=%q", cmd)
	}
	if !strings.Contains(cmd, "--num-prefix-prompts 20") {
		t.Errorf("missing --num-prefix-prompts 20; cmd=%q", cmd)
	}

	// Zero values must be omitted.
	optsZero := jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{VIP: "10.0.10.100"},
		Config:       jumphost.AiperfConfig{Model: "llama3"},
	}
	cmdZero := jumphost.BuildAiperfCmd(optsZero)
	if strings.Contains(cmdZero, "--prefix-prompt-length") {
		t.Errorf("--prefix-prompt-length must be omitted when 0; cmd=%q", cmdZero)
	}
	if strings.Contains(cmdZero, "--num-prefix-prompts") {
		t.Errorf("--num-prefix-prompts must be omitted when 0; cmd=%q", cmdZero)
	}
}

// TestBuildAiperfCmd_ExtraInputs asserts repeated --extra-inputs flags are emitted.
func TestBuildAiperfCmd_ExtraInputs(t *testing.T) {
	opts := jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{VIP: "10.0.10.100"},
		Config: jumphost.AiperfConfig{
			Model:       "llama3",
			ExtraInputs: []string{"ignore_eos:true", "temperature:0.0"},
		},
	}
	cmd := jumphost.BuildAiperfCmd(opts)
	if !strings.Contains(cmd, "--extra-inputs") {
		t.Errorf("missing --extra-inputs flag; cmd=%q", cmd)
	}
	if !strings.Contains(cmd, "ignore_eos:true") {
		t.Errorf("missing ignore_eos:true in extra-inputs; cmd=%q", cmd)
	}
	if !strings.Contains(cmd, "temperature:0.0") {
		t.Errorf("missing temperature:0.0 in extra-inputs; cmd=%q", cmd)
	}

	// When nil, must not appear.
	optsNone := jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{VIP: "10.0.10.100"},
		Config:       jumphost.AiperfConfig{Model: "llama3"},
	}
	cmdNone := jumphost.BuildAiperfCmd(optsNone)
	if strings.Contains(cmdNone, "--extra-inputs") {
		t.Errorf("--extra-inputs must not appear when ExtraInputs is nil; cmd=%q", cmdNone)
	}
}

// TestBuildAiperfCmd_VariantLabel_NotInCmd asserts that VariantLabel (a metadata
// field) is NOT emitted as an aiperf flag.
func TestBuildAiperfCmd_VariantLabel_NotInCmd(t *testing.T) {
	opts := jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{VIP: "10.0.10.100"},
		Config: jumphost.AiperfConfig{
			Model:        "llama3",
			VariantLabel: "c150-isl5000",
		},
	}
	cmd := jumphost.BuildAiperfCmd(opts)
	if strings.Contains(cmd, "VariantLabel") || strings.Contains(cmd, "variant-label") || strings.Contains(cmd, "c150-isl5000") {
		t.Errorf("VariantLabel must NOT be emitted as an aiperf flag; cmd=%q", cmd)
	}
}

// ---------------------------------------------------------------------------
// validateTraceURL — SSRF guard
// ---------------------------------------------------------------------------

func TestValidateTraceURL_AllowedSchemes(t *testing.T) {
	cases := []string{
		"https://raw.githubusercontent.com/kvcache-ai/Mooncake/main/trace.jsonl",
		"http://10.0.0.1:8080/trace.jsonl",
	}
	for _, url := range cases {
		if err := jumphost.ValidateTraceURL(url); err != nil {
			t.Errorf("ValidateTraceURL(%q) = %v, want nil", url, err)
		}
	}
}

func TestValidateTraceURL_BlockedSchemes(t *testing.T) {
	cases := []string{
		"file:///etc/passwd",
		"ftp://example.com/trace.jsonl",
		"gopher://example.com/trace.jsonl",
		"data:application/json,{}",
		"/local/path/trace.jsonl",
		"",
	}
	for _, url := range cases {
		if err := jumphost.ValidateTraceURL(url); err == nil {
			t.Errorf("ValidateTraceURL(%q) = nil, want error", url)
		}
	}
}

// ---------------------------------------------------------------------------
// dilateTimestamp — dilation math
// ---------------------------------------------------------------------------

func TestDilateTimestamp_Numeric(t *testing.T) {
	// ts / 0.80 = ts * 1.25 (stretches inter-arrival gaps → slower arrivals)
	got := jumphost.DilateTimestamp(80.0, 0.80)
	want := 100.0
	if got != want {
		t.Errorf("DilateTimestamp(80, 0.80) = %v, want %v", got, want)
	}
}

func TestDilateTimestamp_Zero(t *testing.T) {
	got := jumphost.DilateTimestamp(0, 0.80)
	if got != 0 {
		t.Errorf("DilateTimestamp(0, 0.80) = %v, want 0", got)
	}
}

func TestDilateTimestamp_LargeFactor(t *testing.T) {
	// ts / 2.0 = ts * 0.5 (compresses gaps → faster arrivals)
	got := jumphost.DilateTimestamp(100.0, 2.0)
	want := 50.0
	if got != want {
		t.Errorf("DilateTimestamp(100, 2.0) = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// buildMooncakeCmd — command construction
// ---------------------------------------------------------------------------

const mooncakeTraceURL = "https://raw.githubusercontent.com/kvcache-ai/Mooncake/refs/heads/main/FAST25-release/traces/toolagent_trace.jsonl"

func mooncakeOpts() jumphost.AiperfRunOptions {
	return jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{VIP: "10.0.10.100"},
		Config: jumphost.AiperfConfig{
			Model:                 "llama3",
			Tokenizer:             "NousResearch/Meta-Llama-3-8B-Instruct",
			TraceURL:              mooncakeTraceURL,
			TraceDilation:         0.80,
			CustomDatasetType:     "mooncake_trace",
			FixedSchedule:         true,
			WorkersMax:            200,
			RandomSeed:            42,
			RequestTimeoutSeconds: 1000,
			ProfileExportLevel:    "summary",
			RecordProcessors:      8,
			Goodput:               "time_to_first_token:5000 inter_token_latency:100",
			Streaming:             true,
		},
	}
}

func TestBuildMooncakeCmd_AllCanonicalFlags(t *testing.T) {
	cmd, err := jumphost.BuildMooncakeCmd(mooncakeOpts())
	if err != nil {
		t.Fatalf("BuildMooncakeCmd: %v", err)
	}

	mustContain := []string{
		"ulimit -n 65536",
		"AIPERF_HTTP_CONNECTION_LIMIT=200",
		"curl -fsSL",
		mooncakeTraceURL,
		"python3.11 -c",
		"aiperf profile",
		"-m", "llama3",
		"-u", "http://10.0.10.100",
		"--tokenizer", "NousResearch/Meta-Llama-3-8B-Instruct",
		"--input-file",
		"--custom-dataset-type", "mooncake_trace",
		"--fixed-schedule",
		"--workers-max", "200",
		"--random-seed", "42",
		"--request-timeout-seconds", "1000",
		"--profile-export-level", "summary",
		"--record-processors", "8",
		"--goodput", "time_to_first_token:5000 inter_token_latency:100",
		"--streaming",
		"--output-artifact-dir",
		"--ui", "none",
		"profile_export_aiperf.json",
	}
	for _, want := range mustContain {
		if !strings.Contains(cmd, want) {
			t.Errorf("BuildMooncakeCmd missing %q; cmd=%q", want, cmd)
		}
	}
}

func TestBuildMooncakeCmd_NoDilationInRequest(t *testing.T) {
	// Verify dilation=0.80 is embedded in the python script (divides by 0.8)
	cmd, err := jumphost.BuildMooncakeCmd(mooncakeOpts())
	if err != nil {
		t.Fatalf("BuildMooncakeCmd: %v", err)
	}
	// The python inline script must reference the dilation factor.
	if !strings.Contains(cmd, "0.8") {
		t.Errorf("BuildMooncakeCmd python script must reference dilation 0.8; cmd=%q", cmd)
	}
	// Must not use --artifact-dir (mooncake uses --output-artifact-dir).
	if strings.Contains(cmd, " --artifact-dir ") {
		t.Errorf("BuildMooncakeCmd must NOT use --artifact-dir (trace uses --output-artifact-dir); cmd=%q", cmd)
	}
}

func TestBuildMooncakeCmd_SSRFReject(t *testing.T) {
	opts := mooncakeOpts()
	opts.Config.TraceURL = "file:///etc/passwd"
	_, err := jumphost.BuildMooncakeCmd(opts)
	if err == nil {
		t.Error("BuildMooncakeCmd must reject file:// TraceURL; got nil error")
	}
}

func TestBuildMooncakeCmd_DefaultDilation(t *testing.T) {
	// TraceDilation=0 should default to 0.80.
	opts := mooncakeOpts()
	opts.Config.TraceDilation = 0
	cmd, err := jumphost.BuildMooncakeCmd(opts)
	if err != nil {
		t.Fatalf("BuildMooncakeCmd: %v", err)
	}
	if !strings.Contains(cmd, "0.8") {
		t.Errorf("BuildMooncakeCmd with TraceDilation=0 should default to 0.8; cmd=%q", cmd)
	}
}

// TestBuildAiperfCmd_ByteCompat_MooncakeZeroFields asserts that zero mooncake
// fields do NOT change the synthetic command output (guard the byte-compat test).
func TestBuildAiperfCmd_ByteCompat_MooncakeZeroFields(t *testing.T) {
	// Same as TestBuildAiperfCmd_ByteCompat_ZeroNewFields but also includes
	// the new mooncake fields at zero. Command must be identical to the golden.
	opts := jumphost.AiperfRunOptions{
		ProbeOptions: jumphost.ProbeOptions{VIP: "10.0.10.100"},
		Config: jumphost.AiperfConfig{
			Model:       "llama3",
			Concurrency: 4,
			NumRequests: 100,
			ISL:         512,
			OSL:         128,
			Streaming:   true,
			Tokenizer:   "NousResearch/Meta-Llama-3-8B-Instruct",
			// Mooncake fields all zero/empty/false
			TraceURL:              "",
			TraceDilation:         0,
			CustomDatasetType:     "",
			FixedSchedule:         false,
			WorkersMax:            0,
			RandomSeed:            0,
			RequestTimeoutSeconds: 0,
			ProfileExportLevel:    "",
			RecordProcessors:      0,
			Goodput:               "",
		},
	}

	const wantCmd = "rm -rf '/tmp/aiperf-run'; aiperf profile -m 'llama3' -u 'http://10.0.10.100' --endpoint-type 'chat' --concurrency 4 --request-count 100 --synthetic-input-tokens-mean 512 --output-tokens-mean 128 --tokenizer 'NousResearch/Meta-Llama-3-8B-Instruct' --streaming --artifact-dir '/tmp/aiperf-run' --ui none >/tmp/aiperf.stderr 2>&1; cat '/tmp/aiperf-run'/profile_export_aiperf.json"

	cmd := jumphost.BuildAiperfCmd(opts)
	if cmd != wantCmd {
		t.Errorf("byte-compat regression with mooncake zero fields:\ngot:  %q\nwant: %q", cmd, wantCmd)
	}
}

// TestBuildAiperfCmd_MooncakeDispatch verifies that BuildAiperfCmd dispatches
// to the mooncake path when TraceURL is set (returns a pipeline, not synthetic).
func TestBuildAiperfCmd_MooncakeDispatch(t *testing.T) {
	cmd := jumphost.BuildAiperfCmd(mooncakeOpts())
	// Mooncake path emits curl and python3.11, not rm -rf.
	if strings.Contains(cmd, "rm -rf") {
		t.Errorf("mooncake dispatch should NOT produce rm -rf (synthetic path); cmd=%q", cmd)
	}
	if !strings.Contains(cmd, "curl -fsSL") {
		t.Errorf("mooncake dispatch must produce curl -fsSL; cmd=%q", cmd)
	}
	if !strings.Contains(cmd, "python3.11 -c") {
		t.Errorf("mooncake dispatch must produce python3.11 -c; cmd=%q", cmd)
	}
}
