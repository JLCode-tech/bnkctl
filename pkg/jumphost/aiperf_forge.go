package jumphost

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ConfigMapToAiperfConfig converts a Forge WebSocket config dict into an AiperfConfig
// and extracts the target VIP/Host.
func ConfigMapToAiperfConfig(m map[string]any) (AiperfConfig, string) {
	cfg := AiperfConfig{
		EndpointType: "chat",
		Tokenizer:    "NousResearch/Meta-Llama-3-8B-Instruct",
		Streaming:    true,
		Concurrency:  1,
		NumRequests:  10,
		ISL:          512,
		OSL:          128,
		Timeout:      5 * time.Minute,
	}

	var targetVIP string

	if v, ok := m["model"].(string); ok && v != "" {
		cfg.Model = v
	}
	if v, ok := m["endpoint_type"].(string); ok && v != "" {
		cfg.EndpointType = v
	}
	if v, ok := m["endpoint"].(string); ok && v != "" {
		cfg.EndpointPath = v
	}
	if v, ok := m["tokenizer"].(string); ok && v != "" {
		cfg.Tokenizer = v
	}
	if v, ok := m["host_header"].(string); ok && v != "" {
		cfg.HostHeader = v
	}

	// Concurrency
	if v := getInt(m, "concurrency"); v > 0 {
		cfg.Concurrency = v
	}

	// Request count
	if v := getInt(m, "request_count"); v > 0 {
		cfg.NumRequests = v
	} else if v := getInt(m, "num_requests"); v > 0 {
		cfg.NumRequests = v
	} else if v := getInt(m, "total_requests"); v > 0 {
		cfg.NumRequests = v
	}

	// ISL / synthetic_input_tokens_mean
	if v := getInt(m, "synthetic_input_tokens_mean"); v > 0 {
		cfg.ISL = v
	} else if v := getInt(m, "isl"); v > 0 {
		cfg.ISL = v
	}

	// OSL / output_tokens_mean
	if v := getInt(m, "output_tokens_mean"); v > 0 {
		cfg.OSL = v
	} else if v := getInt(m, "osl"); v > 0 {
		cfg.OSL = v
	} else if v := getInt(m, "max_tokens"); v > 0 {
		cfg.OSL = v
	}

	// Streaming
	if v, ok := m["streaming"].(bool); ok {
		cfg.Streaming = v
	} else if v, ok := m["stream"].(bool); ok {
		cfg.Streaming = v
	}

	// Timeout
	if v := getInt(m, "request_timeout_seconds"); v > 0 {
		cfg.Timeout = time.Duration(v) * time.Second
	} else if v := getInt(m, "timeout"); v > 0 {
		cfg.Timeout = time.Duration(v) * time.Second
	}

	// Extra inputs
	if extraList, ok := m["extra_inputs"].([]any); ok {
		for _, item := range extraList {
			if str, ok := item.(string); ok && str != "" {
				cfg.ExtraInputs = append(cfg.ExtraInputs, str)
			}
		}
	} else if extraStrList, ok := m["extra_inputs"].([]string); ok {
		cfg.ExtraInputs = append(cfg.ExtraInputs, extraStrList...)
	}

	// Trace URL / mooncake
	if v, ok := m["trace_url"].(string); ok && v != "" {
		cfg.TraceURL = v
	}
	if v, ok := m["custom_dataset_type"].(string); ok && v != "" {
		cfg.CustomDatasetType = v
	}
	if v, ok := m["fixed_schedule"].(bool); ok {
		cfg.FixedSchedule = v
	}

	// URL parsing for VIP
	if rawURL, ok := m["url"].(string); ok && rawURL != "" {
		if !strings.Contains(rawURL, "://") {
			targetVIP = rawURL
		} else {
			u, err := url.Parse(rawURL)
			if err == nil && u.Host != "" {
				targetVIP = u.Host
			} else {
				targetVIP = rawURL
			}
		}
	}

	return cfg, targetVIP
}

func getInt(m map[string]any, key string) int {
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case int64:
		return int(n)
	case string:
		var parsed int
		if _, err := fmt.Sscanf(n, "%d", &parsed); err == nil {
			return parsed
		}
	}
	return 0
}
