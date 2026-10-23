package jumphost

import (
	"testing"
	"time"
)

func TestConfigMapToAiperfConfig(t *testing.T) {
	input := map[string]any{
		"url":                         "http://10.10.10.100:8000/v1/chat/completions",
		"model":                       "meta-llama/Meta-Llama-3-8B-Instruct",
		"endpoint_type":               "chat",
		"endpoint":                    "/v1/chat/completions",
		"streaming":                   true,
		"request_timeout_seconds":     float64(300),
		"concurrency":                 float64(25),
		"request_count":               float64(100),
		"synthetic_input_tokens_mean": float64(256),
		"output_tokens_mean":          float64(64),
		"extra_inputs":                []any{"ignore_eos:true"},
		"host_header":                 "vllm.example.com",
	}

	cfg, vip := ConfigMapToAiperfConfig(input)

	if vip != "10.10.10.100:8000" {
		t.Errorf("expected VIP 10.10.10.100:8000, got %q", vip)
	}
	if cfg.Model != "meta-llama/Meta-Llama-3-8B-Instruct" {
		t.Errorf("unexpected model: %q", cfg.Model)
	}
	if cfg.Concurrency != 25 {
		t.Errorf("unexpected concurrency: %d", cfg.Concurrency)
	}
	if cfg.NumRequests != 100 {
		t.Errorf("unexpected num requests: %d", cfg.NumRequests)
	}
	if cfg.ISL != 256 {
		t.Errorf("unexpected ISL: %d", cfg.ISL)
	}
	if cfg.OSL != 64 {
		t.Errorf("unexpected OSL: %d", cfg.OSL)
	}
	if !cfg.Streaming {
		t.Errorf("expected streaming to be true")
	}
	if cfg.Timeout != 300*time.Second {
		t.Errorf("unexpected timeout: %v", cfg.Timeout)
	}
	if len(cfg.ExtraInputs) != 1 || cfg.ExtraInputs[0] != "ignore_eos:true" {
		t.Errorf("unexpected extra inputs: %v", cfg.ExtraInputs)
	}
	if cfg.HostHeader != "vllm.example.com" {
		t.Errorf("unexpected host header: %q", cfg.HostHeader)
	}
}
