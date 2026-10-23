package jumphost_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/jumphost"
)

// --- buildModelsQueryCmd ---

func TestBuildModelsQueryCmd_IncludesVIP(t *testing.T) {
	cmd := jumphost.BuildModelsQueryCmd("10.0.10.100", "")
	if !strings.Contains(cmd, "10.0.10.100/v1/models") {
		t.Errorf("buildModelsQueryCmd missing vip in URL: %q", cmd)
	}
	if !strings.Contains(cmd, "curl") {
		t.Errorf("buildModelsQueryCmd missing curl: %q", cmd)
	}
}

func TestBuildModelsQueryCmd_HostHeader(t *testing.T) {
	cmd := jumphost.BuildModelsQueryCmd("10.0.10.100", "my.host.local")
	if !strings.Contains(cmd, "Host: my.host.local") {
		t.Errorf("buildModelsQueryCmd missing Host header: %q", cmd)
	}
}

func TestBuildModelsQueryCmd_NoHostHeader(t *testing.T) {
	cmd := jumphost.BuildModelsQueryCmd("10.0.10.100", "")
	if strings.Contains(cmd, "Host:") || strings.Contains(cmd, "-H") {
		t.Errorf("buildModelsQueryCmd should not include -H when hostHeader is empty: %q", cmd)
	}
}

// --- CheckServedModel ---

// checkServedModelSeams installs the three EICE/SSH seams for CheckServedModel
// tests and returns a restore function.  modelsJSON is the stdout the SSH exec
// will return; sshErr overrides to return an error instead.
func checkServedModelSeams(t *testing.T, modelsJSON string, sshErr error) func() {
	t.Helper()

	origMint := *jumphost.PrepareEICEKeyFn
	origPush := *jumphost.PushSSHPublicKeyFn
	origExec := *jumphost.AiperfSSHExecFn

	*jumphost.PrepareEICEKeyFn = func(_ context.Context, _, _ string) (string, string, func(), error) {
		return "fake-key", "fake-pub", func() {}, nil
	}
	*jumphost.PushSSHPublicKeyFn = func(_ context.Context, _, _, _ string) error {
		return nil
	}
	*jumphost.AiperfSSHExecFn = func(_ context.Context, _, _, _, _ string) (string, error) {
		if sshErr != nil {
			return "", sshErr
		}
		return modelsJSON, nil
	}

	return func() {
		*jumphost.PrepareEICEKeyFn = origMint
		*jumphost.PushSSHPublicKeyFn = origPush
		*jumphost.AiperfSSHExecFn = origExec
	}
}

func TestCheckServedModel_MatchProceeds(t *testing.T) {
	restore := checkServedModelSeams(t, `{"data":[{"id":"llama3","object":"model"}],"object":"list"}`, nil)
	defer restore()

	err := jumphost.CheckServedModel(context.Background(), jumphost.ProbeOptions{
		Region:     "ap-southeast-2",
		InstanceID: "i-test",
		VIP:        "10.0.10.100",
	}, "llama3")
	if err != nil {
		t.Errorf("CheckServedModel: expected nil for matching model, got: %v", err)
	}
}

func TestCheckServedModel_MismatchFails(t *testing.T) {
	restore := checkServedModelSeams(t, `{"data":[{"id":"llama3","object":"model"}],"object":"list"}`, nil)
	defer restore()

	err := jumphost.CheckServedModel(context.Background(), jumphost.ProbeOptions{
		Region:     "ap-southeast-2",
		InstanceID: "i-test",
		VIP:        "10.0.10.100",
	}, "meta-llama/Meta-Llama-3-8B-Instruct")
	if err == nil {
		t.Fatal("CheckServedModel: expected error for non-served model, got nil")
	}
	// Error must name the served ids and give a hint.
	if !strings.Contains(err.Error(), "llama3") {
		t.Errorf("error should mention served id %q: %v", "llama3", err)
	}
	if !strings.Contains(err.Error(), "not served") {
		t.Errorf("error should say 'not served': %v", err)
	}
	// Single served model → hint with Try: --model "llama3".
	if !strings.Contains(err.Error(), "Try:") {
		t.Errorf("single served model: error should include Try hint: %v", err)
	}
}

func TestCheckServedModel_TransportErrorNonFatal(t *testing.T) {
	// SSH failure must be non-fatal — CheckServedModel returns nil.
	restore := checkServedModelSeams(t, "", errors.New("ssh: connection refused"))
	defer restore()

	err := jumphost.CheckServedModel(context.Background(), jumphost.ProbeOptions{
		Region:     "ap-southeast-2",
		InstanceID: "i-test",
		VIP:        "10.0.10.100",
	}, "llama3")
	if err != nil {
		t.Errorf("transport error must be non-fatal: got %v", err)
	}
}

func TestCheckServedModel_EmptyModelSkips(t *testing.T) {
	// Empty requestedModel → skip, no SSH calls made.
	restore := checkServedModelSeams(t, "", errors.New("should not be called"))
	defer restore()

	err := jumphost.CheckServedModel(context.Background(), jumphost.ProbeOptions{
		VIP: "10.0.10.100",
	}, "")
	if err != nil {
		t.Errorf("empty model: expected nil, got %v", err)
	}
}

func TestCheckServedModel_EmptyVIPSkips(t *testing.T) {
	// Empty VIP → skip, no SSH calls made.
	restore := checkServedModelSeams(t, "", errors.New("should not be called"))
	defer restore()

	err := jumphost.CheckServedModel(context.Background(), jumphost.ProbeOptions{}, "llama3")
	if err != nil {
		t.Errorf("empty VIP: expected nil, got %v", err)
	}
}

func TestCheckServedModel_NonJSONOutputNonFatal(t *testing.T) {
	// curl returned non-JSON (e.g. HTML error page) → non-fatal.
	restore := checkServedModelSeams(t, "curl: (7) Failed to connect", nil)
	defer restore()

	err := jumphost.CheckServedModel(context.Background(), jumphost.ProbeOptions{
		Region:     "r",
		InstanceID: "i",
		VIP:        "10.0.10.100",
	}, "llama3")
	if err != nil {
		t.Errorf("non-JSON output must be non-fatal: got %v", err)
	}
}

func TestCheckServedModel_MultipleModelsServed(t *testing.T) {
	restore := checkServedModelSeams(t,
		`{"data":[{"id":"llama3","object":"model"},{"id":"phi3","object":"model"}],"object":"list"}`,
		nil,
	)
	defer restore()

	// llama3 is served — proceed.
	err := jumphost.CheckServedModel(context.Background(), jumphost.ProbeOptions{
		Region:     "r",
		InstanceID: "i",
		VIP:        "10.0.10.100",
	}, "llama3")
	if err != nil {
		t.Errorf("model found in multi-model list: expected nil, got %v", err)
	}
}

func TestCheckServedModel_MultipleModelsMismatch(t *testing.T) {
	restore := checkServedModelSeams(t,
		`{"data":[{"id":"llama3","object":"model"},{"id":"phi3","object":"model"}],"object":"list"}`,
		nil,
	)
	defer restore()

	// neither name matches the HF repo path.
	err := jumphost.CheckServedModel(context.Background(), jumphost.ProbeOptions{
		Region:     "r",
		InstanceID: "i",
		VIP:        "10.0.10.100",
	}, "meta-llama/Meta-Llama-3-8B-Instruct")
	if err == nil {
		t.Fatal("expected error for mismatch in multi-model list")
	}
	// Multiple served models → no Try hint (ambiguous which to pick).
	if strings.Contains(err.Error(), "Try:") {
		t.Errorf("multiple served models: should NOT include Try hint: %v", err)
	}
	if !strings.Contains(err.Error(), "llama3") {
		t.Errorf("error should list served ids including llama3: %v", err)
	}
}
