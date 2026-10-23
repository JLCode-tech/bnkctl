package jumphost_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/jumphost"
)

// --- helpers: seam stubs ---

type mintRecord struct{ count int }
type pushRecord struct{ keys []string }
type runRecord struct{ cmds []string }

// installSeams replaces the three package-level fn vars with stubs and returns
// a cleanup function that restores the originals. The stubs record calls so
// tests can assert ordering and counts.
//
// mintErr, if non-nil, is returned from prepareEICEKeyFn instead of the
// real key-mint.
//
// runResponses maps each command substring to the stdout it should return.
// If a command matches a key in errOn, that error is returned instead.
func installSeams(
	t *testing.T,
	mint *mintRecord,
	pushes *pushRecord,
	runs *runRecord,
	runResponses map[string]string,
	errOn map[string]error,
) (restore func()) {
	t.Helper()
	t.Setenv("AZRBNKCTL_FORCE_SSH", "1")

	origMint := *jumphost.PrepareEICEKeyFn
	origSSH := *jumphost.SSHRunViaEICEFn
	origPush := *jumphost.PushSSHPublicKeyFn

	*jumphost.PrepareEICEKeyFn = func(_ context.Context, _, _ string) (string, string, func(), error) {
		mint.count++
		return "fake-key", "fake-pub", func() {}, nil
	}

	*jumphost.PushSSHPublicKeyFn = func(_ context.Context, _, _, pub string) error {
		pushes.keys = append(pushes.keys, pub)
		return nil
	}

	*jumphost.SSHRunViaEICEFn = func(_ context.Context, _, _, _, cmd string) (string, error) {
		runs.cmds = append(runs.cmds, cmd)
		for key, err := range errOn {
			if strings.Contains(cmd, key) {
				return "", err
			}
		}
		for key, out := range runResponses {
			if strings.Contains(cmd, key) {
				return out, nil
			}
		}
		return "ok", nil
	}

	return func() {
		*jumphost.PrepareEICEKeyFn = origMint
		*jumphost.SSHRunViaEICEFn = origSSH
		*jumphost.PushSSHPublicKeyFn = origPush
	}
}

func opts() jumphost.ProbeOptions {
	return jumphost.ProbeOptions{
		Region:     "eastus2",
		InstanceID: "jumphost-vm",
		SourceIP:   "10.0.10.120",
	}
}

// --- RunStagingCommands ---

func TestRunStagingCommands_MintOnce(t *testing.T) {
	mint := &mintRecord{}
	pushes := &pushRecord{}
	runs := &runRecord{}
	restore := installSeams(t, mint, pushes, runs, nil, nil)
	defer restore()

	cmds := []string{"echo a", "echo b", "echo c"}
	out, err := jumphost.RunStagingCommands(context.Background(), opts(), cmds)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mint.count != 1 {
		t.Errorf("prepareEICEKeyFn called %d times, want 1", mint.count)
	}
	if len(out) != 3 {
		t.Errorf("got %d stdout entries, want 3", len(out))
	}
}

func TestRunStagingCommands_RePushPerCommand(t *testing.T) {
	mint := &mintRecord{}
	pushes := &pushRecord{}
	runs := &runRecord{}
	restore := installSeams(t, mint, pushes, runs, nil, nil)
	defer restore()

	cmds := []string{"echo a", "echo b", "echo c"}
	_, err := jumphost.RunStagingCommands(context.Background(), opts(), cmds)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// One PushSSHPublicKey call per command (re-push-per-step, not just once at mint).
	if len(pushes.keys) != len(cmds) {
		t.Errorf("pushSSHPublicKeyFn called %d times, want %d (one per command)", len(pushes.keys), len(cmds))
	}
}

func TestRunStagingCommands_FailFast(t *testing.T) {
	mint := &mintRecord{}
	pushes := &pushRecord{}
	runs := &runRecord{}
	bombErr := errors.New("command failed")
	restore := installSeams(t, mint, pushes, runs, nil, map[string]error{"FAIL": bombErr})
	defer restore()

	cmds := []string{"echo a", "FAIL me", "echo c"}
	out, err := jumphost.RunStagingCommands(context.Background(), opts(), cmds)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, bombErr) {
		t.Errorf("want bombErr, got %v", err)
	}
	// Partial stdout: only the first two commands (echo a ran, FAIL was attempted).
	if len(out) != 2 {
		t.Errorf("got %d stdout entries, want 2 (fail-fast after second cmd)", len(out))
	}
	// Third command must NOT have been run.
	for _, cmd := range runs.cmds {
		if strings.Contains(cmd, "echo c") {
			t.Error("third command was run after fail-fast — should have stopped")
		}
	}
}

func TestRunStagingCommands_CommandsRunInOrder(t *testing.T) {
	mint := &mintRecord{}
	pushes := &pushRecord{}
	runs := &runRecord{}
	restore := installSeams(t, mint, pushes, runs, nil, nil)
	defer restore()

	cmds := []string{"first", "second", "third"}
	_, err := jumphost.RunStagingCommands(context.Background(), opts(), cmds)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, want := range cmds {
		if i >= len(runs.cmds) || runs.cmds[i] != want {
			t.Errorf("cmd[%d]: got %q, want %q", i, runs.cmds[i], want)
		}
	}
}

// --- CopyFileViaEICE ---

func TestCopyFileViaEICE_Base64InRemoteCmd(t *testing.T) {
	mint := &mintRecord{}
	pushes := &pushRecord{}
	runs := &runRecord{}
	restore := installSeams(t, mint, pushes, runs, nil, nil)
	defer restore()

	content := []byte("hello world\n")
	err := jumphost.CopyFileViaEICE(context.Background(), opts(), content, "/home/ec2-user/test.py")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runs.cmds) != 1 {
		t.Fatalf("expected 1 ssh command, got %d", len(runs.cmds))
	}
	cmd := runs.cmds[0]
	if !strings.Contains(cmd, "base64 -d") {
		t.Errorf("remote cmd missing 'base64 -d': %q", cmd)
	}
	if !strings.Contains(cmd, "/home/ec2-user/test.py") {
		t.Errorf("remote cmd missing remotePath: %q", cmd)
	}
	if !strings.Contains(cmd, "mkdir -p") {
		t.Errorf("remote cmd missing mkdir: %q", cmd)
	}
}

func TestCopyFileViaEICE_MintOnce(t *testing.T) {
	mint := &mintRecord{}
	pushes := &pushRecord{}
	runs := &runRecord{}
	restore := installSeams(t, mint, pushes, runs, nil, nil)
	defer restore()

	err := jumphost.CopyFileViaEICE(context.Background(), opts(), []byte("data"), "/tmp/f.py")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mint.count != 1 {
		t.Errorf("prepareEICEKeyFn called %d times, want 1", mint.count)
	}
	if len(pushes.keys) != 1 {
		t.Errorf("pushSSHPublicKeyFn called %d times, want 1 (single-command copy)", len(pushes.keys))
	}
}

func TestCopyFileViaEICE_PropagatesError(t *testing.T) {
	mint := &mintRecord{}
	pushes := &pushRecord{}
	runs := &runRecord{}
	copyErr := errors.New("ssh failed")
	restore := installSeams(t, mint, pushes, runs, nil, map[string]error{"base64": copyErr})
	defer restore()

	err := jumphost.CopyFileViaEICE(context.Background(), opts(), []byte("x"), "/tmp/x.py")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, copyErr) {
		t.Errorf("want copyErr, got %v", err)
	}
}

// --- stdin-based staging helpers ---

// stdinRunRecord captures commands + stdin payloads passed to the
// SSHRunViaEICEWithStdin seam.
type stdinRunRecord struct {
	cmds   []string
	stdins []string
}

// installStdinSeams stubs the mint/push seams plus the stdin-capable run seam,
// recording each command and its full stdin payload.
func installStdinSeams(t *testing.T, mint *mintRecord, pushes *pushRecord, runs *stdinRunRecord, runErr error) (restore func()) {
	t.Helper()
	t.Setenv("AZRBNKCTL_FORCE_SSH", "1")

	origMint := *jumphost.PrepareEICEKeyFn
	origStdin := *jumphost.SSHRunViaEICEStdinFn
	origPush := *jumphost.PushSSHPublicKeyFn

	*jumphost.PrepareEICEKeyFn = func(_ context.Context, _, _ string) (string, string, func(), error) {
		mint.count++
		return "fake-key", "fake-pub", func() {}, nil
	}
	*jumphost.PushSSHPublicKeyFn = func(_ context.Context, _, _, pub string) error {
		pushes.keys = append(pushes.keys, pub)
		return nil
	}
	*jumphost.SSHRunViaEICEStdinFn = func(_ context.Context, _, _, _, cmd string, stdin io.Reader) (string, error) {
		runs.cmds = append(runs.cmds, cmd)
		payload := ""
		if stdin != nil {
			b, _ := io.ReadAll(stdin)
			payload = string(b)
		}
		runs.stdins = append(runs.stdins, payload)
		if runErr != nil {
			return "", runErr
		}
		return "ok", nil
	}

	return func() {
		*jumphost.PrepareEICEKeyFn = origMint
		*jumphost.SSHRunViaEICEStdinFn = origStdin
		*jumphost.PushSSHPublicKeyFn = origPush
	}
}

func TestRunStagingCommandWithStdin_PassesStdinAndCommand(t *testing.T) {
	mint := &mintRecord{}
	pushes := &pushRecord{}
	runs := &stdinRunRecord{}
	defer installStdinSeams(t, mint, pushes, runs, nil)()

	out, err := jumphost.RunStagingCommandWithStdin(
		context.Background(), opts(), "cat > /var/tmp/secret", strings.NewReader("s3cret\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "ok" {
		t.Errorf("stdout: got %q, want %q", out, "ok")
	}
	if mint.count != 1 || len(pushes.keys) != 1 {
		t.Errorf("expected 1 mint + 1 push, got mint=%d push=%d", mint.count, len(pushes.keys))
	}
	if len(runs.cmds) != 1 || runs.cmds[0] != "cat > /var/tmp/secret" {
		t.Errorf("commands: got %v", runs.cmds)
	}
	if len(runs.stdins) != 1 || runs.stdins[0] != "s3cret\n" {
		t.Errorf("stdin payloads: got %q", runs.stdins)
	}
	// The secret must NOT leak into the command string (argv).
	if strings.Contains(runs.cmds[0], "s3cret") {
		t.Errorf("secret leaked into the command string: %q", runs.cmds[0])
	}
}

func TestCopyFileViaEICEStdin_ContentViaStdinNotArgv(t *testing.T) {
	mint := &mintRecord{}
	pushes := &pushRecord{}
	runs := &stdinRunRecord{}
	defer installStdinSeams(t, mint, pushes, runs, nil)()

	content := []byte("dummy-test-key-content-for-test-transferred-via-stdin\n")
	err := jumphost.CopyFileViaEICEStdin(context.Background(), opts(), content, "/home/ec2-user/bigip.pem")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(runs.cmds) != 1 {
		t.Fatalf("expected 1 ssh command, got %d", len(runs.cmds))
	}
	cmd := runs.cmds[0]
	if !strings.Contains(cmd, "mkdir -p") || !strings.Contains(cmd, "cat > '/home/ec2-user/bigip.pem'") {
		t.Errorf("remote cmd must mkdir + cat-to-path: %q", cmd)
	}
	// The content must travel ONLY on stdin — never in the command string
	// (which becomes a local ssh argv element, visible in ps), and never
	// base64-smuggled either.
	if strings.Contains(cmd, "dummy-test-key-content") || strings.Contains(cmd, "base64") {
		t.Errorf("file content (or a base64 of it) leaked into the command string: %q", cmd)
	}
	if runs.stdins[0] != string(content) {
		t.Errorf("stdin payload: got %q, want the exact file content", runs.stdins[0])
	}
}

func TestCopyFileViaEICEStdin_PropagatesError(t *testing.T) {
	mint := &mintRecord{}
	pushes := &pushRecord{}
	runs := &stdinRunRecord{}
	copyErr := errors.New("ssh failed")
	defer installStdinSeams(t, mint, pushes, runs, copyErr)()

	err := jumphost.CopyFileViaEICEStdin(context.Background(), opts(), []byte("x"), "/tmp/x.pem")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, copyErr) {
		t.Errorf("want copyErr, got %v", err)
	}
}

// --- GrpcurlInstallCmd (string-level, no network) ---

func TestGrpcurlInstallCmd_SkipIfPresent(t *testing.T) {
	cmd := jumphost.GrpcurlInstallCmd()
	if !strings.Contains(cmd, "command -v grpcurl") {
		t.Errorf("cmd missing skip-if-present check: %q", cmd)
	}
}

func TestGrpcurlInstallCmd_PinnedVersion(t *testing.T) {
	cmd := jumphost.GrpcurlInstallCmd()
	if !strings.Contains(cmd, "v1.9.3") {
		t.Errorf("cmd does not pin v1.9.3: %q", cmd)
	}
	if !strings.Contains(cmd, "linux_x86_64") {
		t.Errorf("cmd does not target linux_x86_64: %q", cmd)
	}
}

func TestGrpcurlInstallCmd_InstallsToUsrLocalBin(t *testing.T) {
	cmd := jumphost.GrpcurlInstallCmd()
	if !strings.Contains(cmd, "/usr/local/bin") {
		t.Errorf("cmd does not install to /usr/local/bin: %q", cmd)
	}
}

func TestGrpcurlInstallCmd_PrintsVersion(t *testing.T) {
	cmd := jumphost.GrpcurlInstallCmd()
	if !strings.Contains(cmd, "grpcurl --version") {
		t.Errorf("cmd does not print version after install: %q", cmd)
	}
}
