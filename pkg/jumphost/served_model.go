package jumphost

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// servedModelCmd lists the models the endpoint behind vip serves, from the jumphost.
func servedModelCmd(vip, hostHeader string) string {
	header := ""
	if hostHeader != "" {
		header = "-H " + shellSingleQuote("Host: "+hostHeader) + " "
	}
	return fmt.Sprintf("curl -s -m 15 %s%s", header, shellSingleQuote(fmt.Sprintf("http://%s/v1/models", vip)))
}

// parseServedModel returns the first model id in an OpenAI /v1/models response.
func parseServedModel(out string) string {
	start := strings.Index(out, "{")
	if start < 0 {
		return ""
	}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out[start:]), &resp); err != nil || len(resp.Data) == 0 {
		return ""
	}
	return resp.Data[0].ID
}

// ResolveServedModel asks the endpoint behind opts.VIP (through the gateway,
// with hostHeader) which model it serves. A real vLLM rejects requests for any
// other model name, so a placeholder from target discovery must be replaced.
func ResolveServedModel(ctx context.Context, opts ProbeOptions, hostHeader string) (string, error) {
	outs, err := RunStagingCommands(ctx, opts, []string{servedModelCmd(opts.VIP, hostHeader)})
	if err != nil {
		return "", fmt.Errorf("list served models: %w", err)
	}
	if len(outs) == 0 {
		return "", fmt.Errorf("list served models: no output")
	}
	model := parseServedModel(outs[0])
	if model == "" {
		return "", fmt.Errorf("list served models: no model in %q", strings.TrimSpace(outs[0]))
	}
	return model, nil
}
