// Package jumphost provides SSH probe utilities for the multi-NIC jumphost
// model used by scenarios. It is a pure leaf package — it must NOT import
// internal/cli or internal/scenarios. The CLI layer translates cobra flags into
// ProbeOptions at the boundary.
package jumphost

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// DefaultSSHUser is the default Linux username on Azure jumphost instances.
const DefaultSSHUser = "azureuser"

// DefaultProbePod is the default in-cluster netshoot probe pod name.
const DefaultProbePod = "test-probe"

// DefaultProbeNamespace is the default namespace for the in-cluster probe pod.
const DefaultProbeNamespace = "default"

func isNamedPodRunning(pod, ns string) bool {
	return isNamedPodRunningInProcess(pod, ns)
}

func findTMMProbePod() string {
	return findTMMProbePodInProcess()
}

func probePodEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func probePodName() string {
	if pod := probePodEnv("BNKCTL_PROBE_POD", "AZRBNKCTL_PROBE_POD", "GKEBNKCTL_PROBE_POD"); pod != "" {
		if pod == "auto" || pod == "tmm" || pod == "f5-tmm" {
			if tmmPod := findTMMProbePod(); tmmPod != "" {
				return tmmPod
			}
		}
		return pod
	}
	if !isNamedPodRunning(DefaultProbePod, DefaultProbeNamespace) {
		if tmmPod := findTMMProbePod(); tmmPod != "" {
			return tmmPod
		}
	}
	return DefaultProbePod
}

func probePodNamespace() string {
	if ns := probePodEnv("BNKCTL_PROBE_NAMESPACE", "AZRBNKCTL_PROBE_NAMESPACE", "GKEBNKCTL_PROBE_NAMESPACE"); ns != "" {
		return ns
	}
	if pod := probePodEnv("BNKCTL_PROBE_POD", "AZRBNKCTL_PROBE_POD", "GKEBNKCTL_PROBE_POD"); pod == "auto" || pod == "tmm" || pod == "f5-tmm" {
		return "f5-cne-system"
	}
	if !isNamedPodRunning(DefaultProbePod, DefaultProbeNamespace) && findTMMProbePod() != "" {
		return "f5-cne-system"
	}
	return DefaultProbeNamespace
}

func probePodContainer() string {
	if c := probePodEnv("BNKCTL_PROBE_CONTAINER", "AZRBNKCTL_PROBE_CONTAINER", "GKEBNKCTL_PROBE_CONTAINER"); c != "" {
		return c
	}
	if strings.HasPrefix(probePodName(), "f5-tmm") {
		return "debug"
	}
	return ""
}

// IsProbePodRunning checks if the in-cluster probe pod is available and running using in-process client-go.
func IsProbePodRunning(_ context.Context) bool {
	pod := probePodName()
	ns := probePodNamespace()
	return isNamedPodRunningInProcess(pod, ns)
}

// ProbePodIP returns the IP address of the in-cluster probe pod if running, or "" if unavailable.
func ProbePodIP(ctx context.Context) string {
	pod := probePodName()
	ns := probePodNamespace()
	return probePodIPInProcess(ctx, pod, ns)
}

func execInProbe(ctx context.Context, cmdStr string) (string, error) {
	runner := &PodExecRunner{
		Namespace: probePodNamespace(),
		PodName:   probePodName(),
		Container: probePodContainer(),
	}
	return runner.Run(ctx, cmdStr, nil)
}

func execInProbeWithStdin(ctx context.Context, cmdStr string, stdin io.Reader) (string, error) {
	runner := &PodExecRunner{
		Namespace: probePodNamespace(),
		PodName:   probePodName(),
		Container: probePodContainer(),
	}
	return runner.Run(ctx, cmdStr, stdin)
}

// ProbeOptions configures a jumphost curl probe run across AWS, Azure, and GCP.
type ProbeOptions struct {
	// Location is the cloud location / region (e.g. "eastus2", "ap-southeast-2", "us-central1").
	Location string
	// VMName is the VM or instance name.
	VMName string
	// ResourceID is the full Azure resource ID of the VM (used for Bastion tunnel).
	ResourceID string
	// JumphostIP is the IP address of the jumphost (public IP or private IP reachable from operator).
	JumphostIP string
	// SourceIP is the jumphost's BNK_EXT interface IP.
	// curl --interface uses this so traffic hits the real data path.
	SourceIP string
	// VIP is the Gateway VIP to curl.
	VIP string
	// Iterations is the number of curl probes. Default 5.
	Iterations int
	// Timeout is the per-curl timeout. Default 10s.
	Timeout time.Duration
	// User is the SSH user on the jumphost. Default DefaultSSHUser ("azureuser").
	User string
	// Hostname sets the HTTP Host header so requests match hostname-scoped
	// HTTPRoutes. Empty means no Host header (matches no-hostname routes).
	Hostname string
	// SSHKeyPath is an optional path to an existing private SSH key.
	// When empty, an ephemeral ED25519 key is generated.
	SSHKeyPath string
	// UseBastionTunnel indicates whether to use Azure Bastion tunneling for SSH.
	UseBastionTunnel bool

	// GCP fields:
	// Project is the GCP project ID.
	Project string
	// Zone is the GCP zone (e.g. "us-central1-a").
	Zone string

	// Backwards-compatible aliases:
	// Region is an alias for Location.
	Region string
	// InstanceID is an alias for VMName.
	InstanceID string
}

// EffectiveLocation returns Location if set, falling back to Region.
func (p ProbeOptions) EffectiveLocation() string {
	if p.Location != "" {
		return p.Location
	}
	return p.Region
}

// EffectiveVMName returns VMName if set, falling back to InstanceID.
func (p ProbeOptions) EffectiveVMName() string {
	if p.VMName != "" {
		return p.VMName
	}
	return p.InstanceID
}

// EffectiveUser returns User if set, falling back to DefaultSSHUser ("azureuser").
func (p ProbeOptions) EffectiveUser() string {
	if p.User != "" {
		return p.User
	}
	return DefaultSSHUser
}

// TargetHost returns the destination host for SSH.
// JumphostIP takes precedence, then EffectiveVMName, then SourceIP.
func (p ProbeOptions) TargetHost() string {
	if p.JumphostIP != "" {
		return p.JumphostIP
	}
	if vm := p.EffectiveVMName(); vm != "" {
		return vm
	}
	return p.SourceIP
}

// ProbeResult records the outcome of one curl iteration.
type ProbeResult struct {
	Iteration int     `json:"iteration"`
	HTTPCode  int     `json:"http_code"`
	Seconds   float64 `json:"seconds"`
	Err       string  `json:"error,omitempty"`
}

// prepareProbeKey checks if an existing key path was supplied; otherwise
// delegates to prepareEICEKeyFn to mint an ephemeral key.
func prepareProbeKey(ctx context.Context, opts ProbeOptions) (keyPath, pubKeyPath string, cleanup func(), err error) {
	if opts.SSHKeyPath != "" {
		pubKey := opts.SSHKeyPath + ".pub"
		if _, statErr := os.Stat(pubKey); statErr != nil {
			pubKey = ""
		}
		return opts.SSHKeyPath, pubKey, func() {}, nil
	}
	return prepareEICEKeyFn(ctx, opts.EffectiveLocation(), opts.EffectiveVMName())
}

// prepareEICEKey mints an ephemeral ED25519 keypair, writes it to temp files,
// pushes the public half via PushSSHPublicKey, and returns the private-key path,
// the public-key path, and a cleanup func that removes both files.
func prepareEICEKey(ctx context.Context, region, instanceID string) (keyPath, pubKeyPath string, cleanup func(), err error) {
	priv, pubAuth, err := GenerateEphemeralED25519()
	if err != nil {
		return "", "", func() {}, fmt.Errorf("ephemeral key: %w", err)
	}

	keyFile, err := os.CreateTemp("", "azrbnkctl-jumphost-*.key")
	if err != nil {
		return "", "", func() {}, fmt.Errorf("temp key file: %w", err)
	}
	if _, err := keyFile.Write(priv); err != nil {
		_ = keyFile.Close()
		_ = os.Remove(keyFile.Name())
		return "", "", func() {}, fmt.Errorf("write key: %w", err)
	}
	if err := keyFile.Chmod(0o600); err != nil {
		_ = keyFile.Close()
		_ = os.Remove(keyFile.Name())
		return "", "", func() {}, fmt.Errorf("chmod key: %w", err)
	}
	if err := keyFile.Close(); err != nil {
		_ = os.Remove(keyFile.Name())
		return "", "", func() {}, fmt.Errorf("close key: %w", err)
	}

	pubFile, err := os.CreateTemp("", "azrbnkctl-jumphost-*.pub")
	if err != nil {
		_ = os.Remove(keyFile.Name())
		return "", "", func() {}, fmt.Errorf("temp pub file: %w", err)
	}
	if _, err := pubFile.WriteString(pubAuth); err != nil {
		_ = pubFile.Close()
		_ = os.Remove(keyFile.Name())
		_ = os.Remove(pubFile.Name())
		return "", "", func() {}, fmt.Errorf("write pub: %w", err)
	}
	if err := pubFile.Close(); err != nil {
		_ = os.Remove(keyFile.Name())
		_ = os.Remove(pubFile.Name())
		return "", "", func() {}, fmt.Errorf("close pub: %w", err)
	}

	cleanupFn := func() {
		_ = os.Remove(keyFile.Name())
		_ = os.Remove(pubFile.Name())
	}

	if err := pushSSHPublicKeyFn(ctx, region, instanceID, pubFile.Name()); err != nil {
		cleanupFn()
		return "", "", func() {}, fmt.Errorf("send-ssh-public-key: %w", err)
	}

	// Allow the key a moment to settle.
	time.Sleep(2 * time.Second)

	return keyFile.Name(), pubFile.Name(), cleanupFn, nil
}

// buildSSHArgs constructs SSH arguments supporting direct SSH (user@targetHost),
// AWS EC2 Instance Connect tunnel (i-*), and Azure Bastion proxy commands.
func buildSSHArgs(user, targetHost, keyPath string, useBastion bool, resourceID, region string, remoteCmd string) []string {
	var args []string
	if resourceID == "" && strings.HasPrefix(targetHost, "/subscriptions/") {
		resourceID = targetHost
	}

	if useBastion || (resourceID != "" && strings.HasPrefix(resourceID, "/subscriptions/")) {
		if resourceID != "" {
			proxy := fmt.Sprintf("ProxyCommand=az network bastion tunnel --target-resource-id %s --resource-port 22", resourceID)
			args = append(args, "-o", proxy)
		}
	} else if strings.HasPrefix(targetHost, "i-") {
		proxy := fmt.Sprintf("ProxyCommand=aws ec2-instance-connect open-tunnel --instance-id %s", targetHost)
		if region != "" {
			proxy += " --region " + region
		}
		args = append(args, "-o", proxy)
		if user == "" || user == DefaultSSHUser {
			user = "ec2-user"
		}
	}

	args = append(args,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=15",
	)
	if keyPath != "" {
		args = append(args, "-i", keyPath)
	}
	if user == "" {
		user = DefaultSSHUser
	}
	connectHost := targetHost
	if strings.HasPrefix(connectHost, "/subscriptions/") {
		if idx := strings.LastIndex(connectHost, "/"); idx >= 0 {
			connectHost = connectHost[idx+1:]
		}
	}
	args = append(args, fmt.Sprintf("%s@%s", user, connectHost), remoteCmd)
	return args
}

// RunCurlProbes runs opts.Iterations curls. If an in-cluster probe pod is available,
// curls execute via in-process SPDY exec on the probe pod; otherwise it curls from the jumphost.
func RunCurlProbes(ctx context.Context, opts ProbeOptions) ([]ProbeResult, error) {
	if opts.Iterations <= 0 {
		opts.Iterations = 5
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	user := opts.EffectiveUser()
	loc := opts.EffectiveLocation()
	vm := opts.EffectiveVMName()
	targetHost := opts.TargetHost()

	useProbe := os.Getenv("AZRBNKCTL_FORCE_SSH") != "1" && (os.Getenv("AZRBNKCTL_PROBE_IN_CLUSTER") != "" || os.Getenv("BNKCTL_PROBE_IN_CLUSTER") != "" || IsProbePodRunning(ctx))
	if useProbe {
		results := make([]ProbeResult, 0, opts.Iterations)
		remoteCmd := buildCurlCmd("", opts.VIP, opts.Hostname, int(opts.Timeout.Seconds()))
		for i := 1; i <= opts.Iterations; i++ {
			out, perr := execInProbe(ctx, remoteCmd)
			res := ProbeResult{Iteration: i}
			if perr != nil {
				res.Err = perr.Error()
			}
			parts := strings.Fields(out)
			if len(parts) >= 2 {
				var code int
				var secs float64
				_, _ = fmt.Sscanf(parts[0], "%d", &code)
				_, _ = fmt.Sscanf(parts[1], "%f", &secs)
				res.HTTPCode = code
				res.Seconds = secs
			} else if len(parts) == 1 {
				var code int
				_, _ = fmt.Sscanf(parts[0], "%d", &code)
				res.HTTPCode = code
			}
			results = append(results, res)
		}
		return results, nil
	}

	keyPath, pubKeyPath, cleanup, err := prepareProbeKey(ctx, opts)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	results := make([]ProbeResult, 0, opts.Iterations)
	for i := 1; i <= opts.Iterations; i++ {
		if pubKeyPath != "" {
			_ = pushSSHPublicKeyFn(ctx, loc, vm, pubKeyPath)
		}
		code, secs, perr := sshCurlProbe(ctx, opts, keyPath, targetHost, user)
		res := ProbeResult{Iteration: i, HTTPCode: code, Seconds: secs}
		if perr != nil {
			res.Err = perr.Error()
		}
		results = append(results, res)
	}
	return results, nil
}

// PushSSHPublicKey sends/updates the public key on the target jumphost (supporting AWS EICE and Azure VM).
func PushSSHPublicKey(ctx context.Context, region, instanceID, pubKeyPath string) error {
	if strings.HasPrefix(instanceID, "i-") {
		args := []string{
			"ec2-instance-connect", "send-ssh-public-key",
			"--instance-id", instanceID,
			"--instance-os-user", "ec2-user",
			"--ssh-public-key", "file://" + pubKeyPath,
			"--region", region,
		}
		// #nosec G204 -- aws ec2-instance-connect CLI invoked with workspace instance ID and region
		cmd := exec.CommandContext(ctx, "aws", args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("aws %s: %w (stderr: %s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}

	if _, err := exec.LookPath("az"); err != nil {
		return nil
	}

	// #nosec G304 -- pubKeyPath is workspace-managed SSH public key file
	pubBytes, err := os.ReadFile(pubKeyPath)
	if err != nil {
		return nil
	}
	pubKey := strings.TrimSpace(string(pubBytes))
	if pubKey == "" {
		return nil
	}

	var args []string
	if strings.HasPrefix(instanceID, "/subscriptions/") {
		args = []string{
			"vm", "user", "update",
			"--ids", instanceID,
			"--username", DefaultSSHUser,
			"--ssh-key-value", pubKey,
		}
	} else if instanceID != "" {
		args = []string{
			"vm", "user", "update",
			"--name", instanceID,
			"--username", DefaultSSHUser,
			"--ssh-key-value", pubKey,
		}
	} else {
		return nil
	}

	// #nosec G204 -- az vm user update CLI invoked with workspace VM identifier and SSH public key
	cmd := exec.CommandContext(ctx, "az", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	_ = cmd.Run()
	return nil
}

// BuildCurlCmd returns the remote curl command. When host is non-empty it
// adds -H 'Host: <host>' so requests match hostname-scoped HTTPRoutes.
func BuildCurlCmd(sourceIP, vip, host string, timeoutSecs int) string {
	return buildCurlCmd(sourceIP, vip, host, timeoutSecs)
}

// buildCurlCmd returns the remote curl command. When host is non-empty it
// adds -H 'Host: <host>' so requests match hostname-scoped HTTPRoutes.
func buildCurlCmd(sourceIP, vip, host string, timeoutSecs int) string {
	hostHdr := ""
	if host != "" {
		hostHdr = fmt.Sprintf(`-H 'Host: %s' `, host)
	}
	ifaceHdr := ""
	if sourceIP != "" {
		ifaceHdr = fmt.Sprintf(`--interface %s `, sourceIP)
	}
	return fmt.Sprintf(`curl -s -o /dev/null -w '%%{http_code} %%{time_total}' %s%s--max-time %d http://%s/`,
		hostHdr, ifaceHdr, timeoutSecs, vip)
}

// SSHCurlViaEICE opens an SSH connection to the jumphost and runs a single
// curl --interface <sourceIP> http://<vip>/ with the given timeout.
func SSHCurlViaEICE(ctx context.Context, region, instanceID, keyPath, sourceIP, vip, host string, timeout time.Duration) (int, float64, error) {
	opts := ProbeOptions{
		Region:     region,
		InstanceID: instanceID,
		SourceIP:   sourceIP,
		VIP:        vip,
		Hostname:   host,
		Timeout:    timeout,
	}
	return sshCurlProbe(ctx, opts, keyPath, opts.TargetHost(), opts.EffectiveUser())
}

func sshCurlProbe(ctx context.Context, opts ProbeOptions, keyPath, targetHost, user string) (int, float64, error) {
	remoteCmd := buildCurlCmd(opts.SourceIP, opts.VIP, opts.Hostname, int(opts.Timeout.Seconds()))
	args := buildSSHArgs(user, targetHost, keyPath, opts.UseBastionTunnel, opts.ResourceID, opts.EffectiveLocation(), remoteCmd)
	cmd := exec.CommandContext(ctx, "ssh", args...) // #nosec G204 -- ssh arguments built from validated parameters
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, 0, fmt.Errorf("ssh-curl: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	out := strings.TrimSpace(stdout.String())
	parts := strings.Fields(out)
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("unexpected ssh-curl output %q", out)
	}
	var code int
	var secs float64
	if _, err := fmt.Sscanf(parts[0], "%d", &code); err != nil {
		return 0, 0, fmt.Errorf("parsing http_code %q: %w", parts[0], err)
	}
	if _, err := fmt.Sscanf(parts[1], "%f", &secs); err != nil {
		return code, 0, fmt.Errorf("parsing time_total %q: %w", parts[1], err)
	}
	return code, secs, nil
}

// BodyProbeResult records the outcome of one body-capturing curl iteration.
type BodyProbeResult struct {
	Iteration int    `json:"iteration"`
	HTTPCode  int    `json:"http_code"`
	Body      string `json:"body,omitempty"`
	Err       string `json:"error,omitempty"`
}

// buildCurlBodyCmd returns the remote curl command that captures the response body.
func buildCurlBodyCmd(sourceIP, vip, host string, timeoutSecs int) string {
	hostHdr := ""
	if host != "" {
		hostHdr = fmt.Sprintf(`-H 'Host: %s' `, host)
	}
	ifaceHdr := ""
	if sourceIP != "" {
		ifaceHdr = fmt.Sprintf(`--interface %s `, sourceIP)
	}
	return fmt.Sprintf(`curl -s -w '\n%%{http_code}' %s%s--max-time %d http://%s/`,
		hostHdr, ifaceHdr, timeoutSecs, vip)
}

// SSHCurlBodyViaEICE opens an SSH connection to the jumphost and runs a single body-capturing curl.
func SSHCurlBodyViaEICE(ctx context.Context, region, instanceID, keyPath, sourceIP, vip, host string, timeout time.Duration) (body string, code int, err error) {
	opts := ProbeOptions{
		Region:     region,
		InstanceID: instanceID,
		SourceIP:   sourceIP,
		VIP:        vip,
		Hostname:   host,
		Timeout:    timeout,
	}
	return sshCurlBodyProbe(ctx, opts, keyPath, opts.TargetHost(), opts.EffectiveUser())
}

func sshCurlBodyProbe(ctx context.Context, opts ProbeOptions, keyPath, targetHost, user string) (string, int, error) {
	remoteCmd := buildCurlBodyCmd(opts.SourceIP, opts.VIP, opts.Hostname, int(opts.Timeout.Seconds()))
	args := buildSSHArgs(user, targetHost, keyPath, opts.UseBastionTunnel, opts.ResourceID, opts.EffectiveLocation(), remoteCmd)
	cmd := exec.CommandContext(ctx, "ssh", args...) // #nosec G204 -- ssh arguments built from validated parameters
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", 0, fmt.Errorf("ssh-curl-body: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	raw := stdout.String()
	lastNL := strings.LastIndex(raw, "\n")
	if lastNL < 0 {
		return "", 0, fmt.Errorf("unexpected ssh-curl-body output %q", raw)
	}
	bodyStr := strings.TrimSpace(raw[:lastNL])
	codeStr := strings.TrimSpace(raw[lastNL+1:])
	var httpCode int
	if _, parseErr := fmt.Sscanf(codeStr, "%d", &httpCode); parseErr != nil {
		return "", 0, fmt.Errorf("parsing http_code %q: %w", codeStr, parseErr)
	}
	return bodyStr, httpCode, nil
}

// RunCurlBodyProbes runs opts.Iterations body-capturing curls from the jumphost or probe pod.
func RunCurlBodyProbes(ctx context.Context, opts ProbeOptions) ([]BodyProbeResult, error) {
	if opts.Iterations <= 0 {
		opts.Iterations = 5
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	user := opts.EffectiveUser()
	loc := opts.EffectiveLocation()
	vm := opts.EffectiveVMName()
	targetHost := opts.TargetHost()

	useProbe := os.Getenv("AZRBNKCTL_FORCE_SSH") != "1" && (os.Getenv("AZRBNKCTL_PROBE_IN_CLUSTER") != "" || IsProbePodRunning(ctx))
	if useProbe {
		results := make([]BodyProbeResult, 0, opts.Iterations)
		remoteCmd := buildCurlBodyCmd("", opts.VIP, opts.Hostname, int(opts.Timeout.Seconds()))
		for i := 1; i <= opts.Iterations; i++ {
			raw, perr := execInProbe(ctx, remoteCmd)
			res := BodyProbeResult{Iteration: i}
			if perr != nil {
				res.Err = perr.Error()
			}
			lastNL := strings.LastIndex(raw, "\n")
			if lastNL >= 0 {
				res.Body = strings.TrimSpace(raw[:lastNL])
				codeStr := strings.TrimSpace(raw[lastNL+1:])
				var httpCode int
				if _, parseErr := fmt.Sscanf(codeStr, "%d", &httpCode); parseErr == nil {
					res.HTTPCode = httpCode
				}
			} else if raw != "" {
				var httpCode int
				if _, parseErr := fmt.Sscanf(strings.TrimSpace(raw), "%d", &httpCode); parseErr == nil {
					res.HTTPCode = httpCode
				} else {
					res.Body = strings.TrimSpace(raw)
				}
			}
			results = append(results, res)
		}
		return results, nil
	}

	keyPath, pubKeyPath, cleanup, err := prepareProbeKey(ctx, opts)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	results := make([]BodyProbeResult, 0, opts.Iterations)
	for i := 1; i <= opts.Iterations; i++ {
		if pubKeyPath != "" {
			_ = pushSSHPublicKeyFn(ctx, loc, vm, pubKeyPath)
		}
		body, code, perr := sshCurlBodyProbe(ctx, opts, keyPath, targetHost, user)
		res := BodyProbeResult{Iteration: i, HTTPCode: code, Body: body}
		if perr != nil {
			res.Err = perr.Error()
		}
		results = append(results, res)
	}
	return results, nil
}

// SSHRunViaEICE opens an SSH connection to the jumphost and runs an arbitrary
// remote command, returning its stdout (trimmed).
func SSHRunViaEICE(ctx context.Context, region, instanceID, keyPath, remoteCmd string) (string, error) {
	return SSHRunViaEICEWithStdin(ctx, region, instanceID, keyPath, remoteCmd, nil)
}

// SSHRunViaEICEWithStdin is SSHRunViaEICE with the local ssh process's stdin
// attached to the given reader.
func SSHRunViaEICEWithStdin(ctx context.Context, region, instanceID, keyPath, remoteCmd string, stdin io.Reader) (string, error) {
	opts := ProbeOptions{
		Region:     region,
		InstanceID: instanceID,
	}
	user := opts.EffectiveUser()
	targetHost := opts.TargetHost()
	args := buildSSHArgs(user, targetHost, keyPath, opts.UseBastionTunnel, opts.ResourceID, opts.EffectiveLocation(), remoteCmd)
	cmd := exec.CommandContext(ctx, "ssh", args...) // #nosec G204 -- ssh arguments built from validated parameters
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(stdout.String()), fmt.Errorf("ssh-run: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// shellSingleQuote wraps s in single quotes safe for /bin/sh.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildHTTPResponderCmd returns the remote shell command that writes marker to
// /tmp/azrbnkctl-extpool/index.html and (re)starts a systemd transient unit.
func buildHTTPResponderCmd(port int, marker string) string {
	q := shellSingleQuote(marker)
	unit := fmt.Sprintf("azrbnkctl-extpool-%d", port)
	return fmt.Sprintf(
		`mkdir -p /tmp/azrbnkctl-extpool && printf '%%s' %s > /tmp/azrbnkctl-extpool/index.html; sudo systemctl stop %s 2>/dev/null; sudo systemctl reset-failed %s 2>/dev/null; sudo systemd-run --unit=%s --working-directory=/tmp/azrbnkctl-extpool python3 -m http.server %d >/dev/null 2>&1; sleep 2; curl -s -o /dev/null -w '%%{http_code}' http://127.0.0.1:%d/`,
		q, unit, unit, unit, port, port,
	)
}

// buildHTTPResponderStopCmd returns the remote command that stops the systemd transient unit.
func buildHTTPResponderStopCmd(port int) string {
	unit := fmt.Sprintf("azrbnkctl-extpool-%d", port)
	return fmt.Sprintf(`sudo systemctl stop %s 2>/dev/null; sudo systemctl reset-failed %s 2>/dev/null; true`, unit, unit)
}

// buildProbeHTTPResponderCmd returns the command that writes marker and runs python3 http.server inside a probe container.
func buildProbeHTTPResponderCmd(port int, marker string) string {
	q := shellSingleQuote(marker)
	dir := fmt.Sprintf("/tmp/azrbnkctl-extpool-%d", port)
	return fmt.Sprintf(
		`mkdir -p %s && printf '%%s' %s > %s/index.html && pkill -f '\[p\]ython3 -m http.server %d' 2>/dev/null || true; nohup python3 -m http.server %d --directory %s </dev/null >/dev/null 2>&1 & sleep 1; curl -s -o /dev/null -w '%%{http_code}' http://127.0.0.1:%d/`,
		dir, q, dir, port, port, dir, port,
	)
}

// buildProbeHTTPResponderStopCmd returns the command that stops python3 http.server inside a probe container.
func buildProbeHTTPResponderStopCmd(port int) string {
	return fmt.Sprintf(`pkill -f '\[p\]ython3 -m http.server %d' 2>/dev/null || true`, port)
}

// StartHTTPResponder starts a backgrounded python3 HTTP server on the jumphost or in-cluster probe pod.
func StartHTTPResponder(ctx context.Context, opts ProbeOptions, port int, marker string) error {
	useProbe := os.Getenv("AZRBNKCTL_FORCE_SSH") != "1" && (os.Getenv("AZRBNKCTL_PROBE_IN_CLUSTER") != "" || IsProbePodRunning(ctx))
	if useProbe {
		out, runErr := execInProbe(ctx, buildProbeHTTPResponderCmd(port, marker))
		if runErr != nil {
			return fmt.Errorf("starting HTTP responder on :%d: %w (probe stdout: %q)", port, runErr, out)
		}
		if strings.TrimSpace(out) != "200" {
			return fmt.Errorf("HTTP responder on :%d did not return 200 (probe stdout: %q)", port, out)
		}
		return nil
	}

	keyPath, _, cleanup, err := prepareProbeKey(ctx, opts)
	if err != nil {
		return err
	}
	defer cleanup()

	out, runErr := sshRunViaEICEFn(ctx, opts.EffectiveLocation(), opts.EffectiveVMName(), keyPath, buildHTTPResponderCmd(port, marker))
	if runErr != nil {
		return fmt.Errorf("starting HTTP responder on :%d: %w (remote stdout: %q)", port, runErr, out)
	}
	if strings.TrimSpace(out) != "200" {
		return fmt.Errorf("HTTP responder on :%d did not return 200 (remote stdout: %q)", port, out)
	}
	return nil
}

// StopHTTPResponder kills any python3 http.server on port on the jumphost or in-cluster probe pod.
func StopHTTPResponder(ctx context.Context, opts ProbeOptions, port int) error {
	useProbe := os.Getenv("AZRBNKCTL_FORCE_SSH") != "1" && (os.Getenv("AZRBNKCTL_PROBE_IN_CLUSTER") != "" || IsProbePodRunning(ctx))
	if useProbe {
		_, _ = execInProbe(ctx, buildProbeHTTPResponderStopCmd(port))
		return nil
	}

	keyPath, _, cleanup, err := prepareProbeKey(ctx, opts)
	if err != nil {
		return err
	}
	defer cleanup()

	_, _ = sshRunViaEICEFn(ctx, opts.EffectiveLocation(), opts.EffectiveVMName(), keyPath, buildHTTPResponderStopCmd(port))
	return nil
}

// GenerateEphemeralED25519 mints a fresh ED25519 keypair and returns
// (private-key in OpenSSH PEM, public-key in authorized_keys format, error).
func GenerateEphemeralED25519() (privPEM []byte, pubAuthLine string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	pemBlock, err := ssh.MarshalPrivateKey(priv, "azrbnkctl-jumphost-ephemeral")
	if err != nil {
		return nil, "", err
	}
	pemBytes := pem.EncodeToMemory(pemBlock)

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, "", err
	}
	auth := fmt.Sprintf("ssh-ed25519 %s azrbnkctl-jumphost-ephemeral\n",
		base64.StdEncoding.EncodeToString(sshPub.Marshal()))
	return pemBytes, auth, nil
}
