package jumphost

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var interfaceFlagRegex = regexp.MustCompile(`--interface\s+\S+\s*`)

func stripInterfaceFlag(cmd string) string {
	return interfaceFlagRegex.ReplaceAllString(cmd, "")
}

func shouldUseProbe(ctx context.Context) bool {
	if os.Getenv("AZRBNKCTL_FORCE_SSH") == "1" {
		return false
	}
	if os.Getenv("AZRBNKCTL_PROBE_IN_CLUSTER") != "" {
		return true
	}
	return IsProbePodRunning(ctx)
}

// Package-level function-variable seams consumed ONLY by the staging helpers
// (RunStagingCommands, RunStagingCommandWithStdin, CopyFileViaEICE,
// CopyFileViaEICEStdin). Default to the real implementations. Tests override
// these via export_test.go to assert command sequencing without network.
var (
	prepareEICEKeyFn       = prepareEICEKey
	sshRunViaEICEFn        = SSHRunViaEICE
	sshRunViaEICEStdinFn   = SSHRunViaEICEWithStdin
	pushSSHPublicKeyFn     = PushSSHPublicKey
	execInProbeFn          = execInProbe
	execInProbeWithStdinFn = execInProbeWithStdin
)

// RunStagingCommands mints+pushes an ephemeral SSH key internally, then runs each command in order over SSH.
// When an in-cluster probe pod is available (and AZRBNKCTL_FORCE_SSH is not 1), it executes commands via execInProbe.
func RunStagingCommands(ctx context.Context, opts ProbeOptions, commands []string) ([]string, error) {
	if shouldUseProbe(ctx) {
		out := make([]string, 0, len(commands))
		for _, cmd := range commands {
			cleanedCmd := stripInterfaceFlag(cmd)
			stdout, runErr := execInProbeFn(ctx, cleanedCmd)
			out = append(out, stdout)
			if runErr != nil {
				return out, runErr
			}
		}
		return out, nil
	}

	keyPath, pubKeyPath, cleanup, err := prepareProbeKey(ctx, opts)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	out := make([]string, 0, len(commands))
	for _, cmd := range commands {
		if pubKeyPath != "" {
			_ = pushSSHPublicKeyFn(ctx, opts.EffectiveLocation(), opts.EffectiveVMName(), pubKeyPath)
		}

		stdout, runErr := sshRunViaEICEFn(ctx, opts.EffectiveLocation(), opts.EffectiveVMName(), keyPath, cmd)
		out = append(out, stdout)
		if runErr != nil {
			return out, runErr
		}
	}
	return out, nil
}

// RunStagingCommandWithStdin runs a SINGLE command over SSH with stdin attached,
// or via execInProbeWithStdin when using an in-cluster probe pod.
func RunStagingCommandWithStdin(ctx context.Context, opts ProbeOptions, command string, stdin io.Reader) (string, error) {
	if shouldUseProbe(ctx) {
		cleanedCmd := stripInterfaceFlag(command)
		return execInProbeWithStdinFn(ctx, cleanedCmd, stdin)
	}

	keyPath, pubKeyPath, cleanup, err := prepareProbeKey(ctx, opts)
	if err != nil {
		return "", err
	}
	defer cleanup()

	if pubKeyPath != "" {
		_ = pushSSHPublicKeyFn(ctx, opts.EffectiveLocation(), opts.EffectiveVMName(), pubKeyPath)
	}

	return sshRunViaEICEStdinFn(ctx, opts.EffectiveLocation(), opts.EffectiveVMName(), keyPath, command, stdin)
}

// CopyFileViaEICE writes content to remotePath on the jumphost or probe pod by base64-encoding content.
func CopyFileViaEICE(ctx context.Context, opts ProbeOptions, content []byte, remotePath string) error {
	if shouldUseProbe(ctx) {
		b64 := base64.StdEncoding.EncodeToString(content)
		dir := filepath.Dir(remotePath)
		remoteCmd := fmt.Sprintf(
			"mkdir -p %s && printf '%%s' %s | base64 -d > %s",
			shellSingleQuote(dir),
			shellSingleQuote(b64),
			shellSingleQuote(remotePath),
		)
		_, runErr := execInProbeFn(ctx, remoteCmd)
		if runErr != nil {
			return fmt.Errorf("copy %s: %w", remotePath, runErr)
		}
		return nil
	}

	keyPath, pubKeyPath, cleanup, err := prepareProbeKey(ctx, opts)
	if err != nil {
		return err
	}
	defer cleanup()

	if pubKeyPath != "" {
		_ = pushSSHPublicKeyFn(ctx, opts.EffectiveLocation(), opts.EffectiveVMName(), pubKeyPath)
	}

	b64 := base64.StdEncoding.EncodeToString(content)
	dir := filepath.Dir(remotePath)

	remoteCmd := fmt.Sprintf(
		"mkdir -p %s && printf '%%s' %s | base64 -d > %s",
		shellSingleQuote(dir),
		shellSingleQuote(b64),
		shellSingleQuote(remotePath),
	)

	_, runErr := sshRunViaEICEFn(ctx, opts.EffectiveLocation(), opts.EffectiveVMName(), keyPath, remoteCmd)
	if runErr != nil {
		return fmt.Errorf("copy %s: %w", remotePath, runErr)
	}
	return nil
}

// CopyFileViaEICEStdin writes content to remotePath on the jumphost or probe pod by piping
// the bytes through stdin into a remote `cat > file`.
func CopyFileViaEICEStdin(ctx context.Context, opts ProbeOptions, content []byte, remotePath string) error {
	if shouldUseProbe(ctx) {
		dir := filepath.Dir(remotePath)
		remoteCmd := fmt.Sprintf(
			"mkdir -p %s && cat > %s",
			shellSingleQuote(dir),
			shellSingleQuote(remotePath),
		)
		_, runErr := execInProbeWithStdinFn(ctx, remoteCmd, bytes.NewReader(content))
		if runErr != nil {
			return fmt.Errorf("copy %s: %w", remotePath, runErr)
		}
		return nil
	}

	keyPath, pubKeyPath, cleanup, err := prepareProbeKey(ctx, opts)
	if err != nil {
		return err
	}
	defer cleanup()

	if pubKeyPath != "" {
		_ = pushSSHPublicKeyFn(ctx, opts.EffectiveLocation(), opts.EffectiveVMName(), pubKeyPath)
	}

	dir := filepath.Dir(remotePath)
	remoteCmd := fmt.Sprintf(
		"mkdir -p %s && cat > %s",
		shellSingleQuote(dir),
		shellSingleQuote(remotePath),
	)

	_, runErr := sshRunViaEICEStdinFn(ctx, opts.EffectiveLocation(), opts.EffectiveVMName(), keyPath, remoteCmd, bytes.NewReader(content))
	if runErr != nil {
		return fmt.Errorf("copy %s: %w", remotePath, runErr)
	}
	return nil
}

// ShellSingleQuote wraps s in single quotes safe for /bin/sh, escaping any
// embedded single quotes via the close-quote/escaped-quote/reopen-quote idiom.
//
// Exported for callers (e.g. the BIG-IP onboarding phase, F2-B2) that compose
// nested remote shell commands — an `ssh ... admin@bigip '<cmd>'` run ON the
// jumphost — and must single-quote the inner BIG-IP command safely. Thin wrapper
// over the package-internal shellSingleQuote so the internal helper stays
// untouched.
func ShellSingleQuote(s string) string {
	return shellSingleQuote(s)
}

// GrpcurlInstallCmd returns the idempotent remote shell command that installs
// grpcurl v1.9.3 to /usr/local/bin on an AL2023 x86_64 jumphost.
//
// The command:
//  1. Skips the download if grpcurl is already on PATH (skip-if-present).
//  2. Downloads the pinned v1.9.3 linux x86_64 release tarball from GitHub.
//  3. Extracts grpcurl directly to /usr/local/bin via sudo tar (avoids a temp dir).
//  4. Prints the installed version to confirm success.
//
// Exported so the phase can call it directly; also used in staging_test.go for
// string-level assertions (same pattern as the buildCurlCmd family).
func GrpcurlInstallCmd() string {
	// Pinned release URL: grpcurl v1.9.3, linux x86_64, AL2023 EKS jumphost.
	const releaseURL = "https://github.com/fullstorydev/grpcurl/releases/download/v1.9.3/grpcurl_1.9.3_linux_x86_64.tar.gz"
	return fmt.Sprintf(
		`command -v grpcurl >/dev/null 2>&1 || (curl -sSL %s | sudo tar -xz -C /usr/local/bin grpcurl); grpcurl --version`,
		releaseURL,
	)
}
