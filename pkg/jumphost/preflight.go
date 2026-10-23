package jumphost

// preflight.go — served-model preflight for aiperf runs.
//
// Before running aiperf, query GET <vip>/v1/models from the jumphost to obtain
// the model IDs that vLLM actually serves. If the requested model name is not
// among them, fail fast with a clear error that lists the served IDs so the
// operator can correct --model (or --served-model-name in vLLM config).
//
// Uses the same aiperfSSHExecFn / prepareEICEKeyFn / pushSSHPublicKeyFn seams
// as RunAiperf so tests can stub the SSH layer without network access.
//
// Transport errors (network unreachable, SSH failure) are NON-FATAL: we log
// and proceed so a transient /v1/models outage doesn't block a benchmark run.
// A successful preflight that shows a model mismatch IS fatal.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// v1ModelsResponse is the minimal subset of the OpenAI-compatible /v1/models
// response that we need for the served-model preflight check.
type v1ModelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// buildModelsQueryCmd constructs the remote shell command that runs a curl
// against GET http://<vip>/v1/models (with optional Host header) and prints
// the JSON response body to stdout.
func buildModelsQueryCmd(vip, hostHeader string) string {
	// Use curl with -sf so it fails silently on HTTP errors (returns non-zero
	// exit code) without confusing stdout.  We run via the BNK VIP address
	// directly; the Host header is injected when the HTTPRoute has a hostname
	// match (same as aiperf --header Host:<value>).
	curlArgs := []string{
		"curl", "-sf",
		shellSingleQuote(fmt.Sprintf("http://%s/v1/models", vip)),
	}
	if hostHeader != "" {
		curlArgs = append(curlArgs, "-H", shellSingleQuote(fmt.Sprintf("Host: %s", hostHeader)))
	}
	return strings.Join(curlArgs, " ")
}

// CheckServedModel queries GET <vip>/v1/models from the jumphost and verifies
// that requestedModel is among the served model IDs.
//
// Returns an error (fatal — abort the aiperf run) if:
//   - The preflight succeeds AND requestedModel is NOT in the served list.
//
// Returns nil (proceed) if:
//   - requestedModel IS in the served list (happy path).
//   - The preflight fails due to a transport error (SSH failure, curl error,
//     JSON parse error) — non-fatal, logs the reason to stderr.
//
// When exactly one model is served and the request does not match, the error
// message includes a hint suggesting the served name.
func CheckServedModel(ctx context.Context, probOpts ProbeOptions, requestedModel string) error {
	if requestedModel == "" || probOpts.VIP == "" {
		// Nothing to check — skip silently.
		return nil
	}
	if probOpts.User == "" {
		probOpts.User = DefaultSSHUser
	}

	keyPath, pubKeyPath, cleanup, err := prepareProbeKey(ctx, probOpts)
	if err != nil {
		// Non-fatal transport error — log and proceed.
		fmt.Fprintf(os.Stderr, "[preflight] could not prepare EICE key for /v1/models check: %v — proceeding\n", err)
		return nil
	}
	defer cleanup()

	if pubKeyPath != "" {
		_ = pushSSHPublicKeyFn(ctx, probOpts.EffectiveLocation(), probOpts.EffectiveVMName(), pubKeyPath)
	}

	remoteCmd := buildModelsQueryCmd(probOpts.VIP, probOpts.Hostname)

	stdout, sshErr := aiperfSSHExecFn(ctx, probOpts.EffectiveLocation(), probOpts.EffectiveVMName(), keyPath, remoteCmd)
	if sshErr != nil {
		// Non-fatal: SSH or curl failure. Log and let the caller proceed.
		fmt.Fprintf(os.Stderr, "[preflight] /v1/models query failed (non-fatal, proceeding): %v\n", sshErr)
		return nil
	}

	// Locate JSON in output (skip any leading SSH banner lines).
	jsonStart := strings.Index(stdout, "{")
	if jsonStart < 0 {
		fmt.Fprintf(os.Stderr, "[preflight] /v1/models returned non-JSON output (non-fatal, proceeding): %.200s\n", stdout)
		return nil
	}

	var resp v1ModelsResponse
	if parseErr := json.Unmarshal([]byte(stdout[jsonStart:]), &resp); parseErr != nil {
		fmt.Fprintf(os.Stderr, "[preflight] could not parse /v1/models response (non-fatal, proceeding): %v\n", parseErr)
		return nil
	}

	// Build the list of served IDs for the error message.
	servedIDs := make([]string, 0, len(resp.Data))
	for _, m := range resp.Data {
		servedIDs = append(servedIDs, m.ID)
	}

	if len(servedIDs) == 0 {
		// No models reported — maybe the endpoint isn't ready yet. Non-fatal.
		fmt.Fprintf(os.Stderr, "[preflight] /v1/models returned empty data list (non-fatal, proceeding)\n")
		return nil
	}

	// Check if requestedModel is among the served IDs.
	for _, id := range servedIDs {
		if id == requestedModel {
			return nil // match — proceed
		}
	}

	// Mismatch — fatal.  Build a helpful error.
	hint := ""
	if len(servedIDs) == 1 {
		hint = fmt.Sprintf(" Try: --model %q", servedIDs[0])
	}
	return fmt.Errorf(
		"--model %q is not served by the endpoint; served models: %v."+
			" vLLM uses --served-model-name (not the HF repo path) to set the served id.%s",
		requestedModel, servedIDs, hint,
	)
}
