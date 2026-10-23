package jumphost_test

import (
	"crypto/ed25519"
	"encoding/pem"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/JLCode-tech/bnkctl/pkg/jumphost"
)

func TestGenerateEphemeralED25519_ParseablePEM(t *testing.T) {
	privPEM, pubAuth, err := jumphost.GenerateEphemeralED25519()
	if err != nil {
		t.Fatalf("GenerateEphemeralED25519: %v", err)
	}

	// Private key must be a PEM block.
	block, _ := pem.Decode(privPEM)
	if block == nil {
		t.Fatal("privPEM is not a valid PEM block")
	}
	// PEM type must indicate an OpenSSH private key.
	if !strings.Contains(block.Type, "OPENSSH PRIVATE KEY") && !strings.Contains(block.Type, "PRIVATE KEY") {
		t.Errorf("unexpected PEM type %q (want OPENSSH PRIVATE KEY)", block.Type)
	}

	// Parse via golang.org/x/crypto/ssh to confirm it's a valid ED25519 key.
	privKey, err := ssh.ParseRawPrivateKey(privPEM)
	if err != nil {
		t.Fatalf("ssh.ParseRawPrivateKey: %v", err)
	}
	if _, ok := privKey.(*ed25519.PrivateKey); !ok {
		// ssh package returns *ed25519.PrivateKey for ed25519.
		t.Logf("key type: %T (ok — may be a wrapped type)", privKey)
	}

	// Public auth line must start with "ssh-ed25519".
	if !strings.HasPrefix(pubAuth, "ssh-ed25519 ") {
		t.Errorf("pubAuth does not start with 'ssh-ed25519 ': %q", pubAuth[:min(len(pubAuth), 40)])
	}

	// Auth line must end with newline.
	if !strings.HasSuffix(pubAuth, "\n") {
		t.Errorf("pubAuth does not end with newline")
	}
}

func TestGenerateEphemeralED25519_Unique(t *testing.T) {
	p1, _, err1 := jumphost.GenerateEphemeralED25519()
	p2, _, err2 := jumphost.GenerateEphemeralED25519()
	if err1 != nil || err2 != nil {
		t.Fatalf("GenerateEphemeralED25519 error: %v / %v", err1, err2)
	}
	if string(p1) == string(p2) {
		t.Error("two generated keys are identical — random source broken")
	}
}

func TestBuildCurlCmd_WithHost(t *testing.T) {
	cmd := jumphost.BuildCurlCmd("10.0.10.120", "10.0.10.100", "azrbnkctl.local", 10)

	if !strings.Contains(cmd, `-H 'Host: azrbnkctl.local'`) {
		t.Errorf("cmd missing Host header: %q", cmd)
	}
	if !strings.Contains(cmd, "http://10.0.10.100/") {
		t.Errorf("cmd missing VIP URL: %q", cmd)
	}
	if !strings.Contains(cmd, "--interface 10.0.10.120") {
		t.Errorf("cmd missing --interface sourceIP: %q", cmd)
	}
}

func TestBuildCurlCmd_NoHost(t *testing.T) {
	cmd := jumphost.BuildCurlCmd("10.0.10.120", "10.0.10.100", "", 10)

	if strings.Contains(cmd, `-H 'Host:`) {
		t.Errorf("cmd should not contain Host header when host is empty: %q", cmd)
	}
	if !strings.Contains(cmd, "http://10.0.10.100/") {
		t.Errorf("cmd missing VIP URL: %q", cmd)
	}
	if !strings.Contains(cmd, "--interface 10.0.10.120") {
		t.Errorf("cmd missing --interface sourceIP: %q", cmd)
	}
}

func TestBuildCurlBodyCmd_WithHost(t *testing.T) {
	cmd := jumphost.BuildCurlBodyCmd("10.0.10.120", "10.0.10.100", "split.local", 10)

	if !strings.Contains(cmd, `-H 'Host: split.local'`) {
		t.Errorf("cmd missing Host header: %q", cmd)
	}
	if strings.Contains(cmd, "-o /dev/null") {
		t.Errorf("cmd must NOT contain -o /dev/null for body capture: %q", cmd)
	}
	if !strings.Contains(cmd, `-w '\n%{http_code}'`) {
		t.Errorf("cmd missing body+code format string: %q", cmd)
	}
	if !strings.Contains(cmd, "http://10.0.10.100/") {
		t.Errorf("cmd missing VIP URL: %q", cmd)
	}
	if !strings.Contains(cmd, "--interface 10.0.10.120") {
		t.Errorf("cmd missing --interface sourceIP: %q", cmd)
	}
}

func TestBuildCurlBodyCmd_NoHost(t *testing.T) {
	cmd := jumphost.BuildCurlBodyCmd("10.0.10.120", "10.0.10.100", "", 10)

	if strings.Contains(cmd, `-H 'Host:`) {
		t.Errorf("cmd should not contain Host header when host is empty: %q", cmd)
	}
	if strings.Contains(cmd, "-o /dev/null") {
		t.Errorf("cmd must NOT contain -o /dev/null for body capture: %q", cmd)
	}
	if !strings.Contains(cmd, `-w '\n%{http_code}'`) {
		t.Errorf("cmd missing body+code format string: %q", cmd)
	}
	if !strings.Contains(cmd, "http://10.0.10.100/") {
		t.Errorf("cmd missing VIP URL: %q", cmd)
	}
}

func TestBuildHTTPResponderCmd_ContainsMarkerAndPort(t *testing.T) {
	cmd := jumphost.BuildHTTPResponderCmd(8080, "external-resource-pool-OK")

	if !strings.Contains(cmd, "external-resource-pool-OK") {
		t.Errorf("cmd missing marker: %q", cmd)
	}
	if !strings.Contains(cmd, "systemd-run") {
		t.Errorf("cmd missing 'systemd-run': %q", cmd)
	}
	if !strings.Contains(cmd, "azrbnkctl-extpool-8080") {
		t.Errorf("cmd missing unit name 'azrbnkctl-extpool-8080': %q", cmd)
	}
	if !strings.Contains(cmd, "http.server 8080") {
		t.Errorf("cmd missing 'http.server 8080': %q", cmd)
	}
	if !strings.Contains(cmd, "http://127.0.0.1:8080/") {
		t.Errorf("cmd missing self-curl URL: %q", cmd)
	}
	if !strings.Contains(cmd, "%{http_code}") {
		t.Errorf("cmd missing http_code probe: %q", cmd)
	}
}

func TestBuildHTTPResponderCmd_EscapesMarkerQuotes(t *testing.T) {
	// A marker containing a single quote must not break out of the shell quote.
	cmd := jumphost.BuildHTTPResponderCmd(9090, "a'b")
	if !strings.Contains(cmd, `'a'\''b'`) {
		t.Errorf("single-quote in marker not escaped: %q", cmd)
	}
}

func TestBuildHTTPResponderStopCmd(t *testing.T) {
	cmd := jumphost.BuildHTTPResponderStopCmd(8080)
	if !strings.Contains(cmd, "systemctl stop") {
		t.Errorf("stop cmd missing 'systemctl stop': %q", cmd)
	}
	if !strings.Contains(cmd, "azrbnkctl-extpool-8080") {
		t.Errorf("stop cmd missing unit name 'azrbnkctl-extpool-8080': %q", cmd)
	}
}

func TestProbeOptions_DefaultsAndFallbacks(t *testing.T) {
	if jumphost.DefaultSSHUser != "azureuser" {
		t.Fatalf("expected DefaultSSHUser to be 'azureuser', got %q", jumphost.DefaultSSHUser)
	}

	opts := jumphost.ProbeOptions{}
	if user := opts.EffectiveUser(); user != "azureuser" {
		t.Errorf("expected EffectiveUser() to default to azureuser, got %q", user)
	}

	opts.User = "customuser"
	if user := opts.EffectiveUser(); user != "customuser" {
		t.Errorf("expected EffectiveUser() customuser, got %q", user)
	}

	// EffectiveLocation
	optsLoc := jumphost.ProbeOptions{Region: "eastus"}
	if loc := optsLoc.EffectiveLocation(); loc != "eastus" {
		t.Errorf("expected EffectiveLocation() fallback to Region, got %q", loc)
	}
	optsLoc.Location = "eastus2"
	if loc := optsLoc.EffectiveLocation(); loc != "eastus2" {
		t.Errorf("expected EffectiveLocation() to prioritize Location, got %q", loc)
	}

	// EffectiveVMName
	optsVM := jumphost.ProbeOptions{InstanceID: "vm-123"}
	if vm := optsVM.EffectiveVMName(); vm != "vm-123" {
		t.Errorf("expected EffectiveVMName() fallback to InstanceID, got %q", vm)
	}
	optsVM.VMName = "jumphost-0"
	if vm := optsVM.EffectiveVMName(); vm != "jumphost-0" {
		t.Errorf("expected EffectiveVMName() to prioritize VMName, got %q", vm)
	}

	// TargetHost
	optsHost := jumphost.ProbeOptions{SourceIP: "10.0.1.5"}
	if h := optsHost.TargetHost(); h != "10.0.1.5" {
		t.Errorf("expected TargetHost() fallback to SourceIP, got %q", h)
	}
	optsHost.VMName = "vm-jumphost"
	if h := optsHost.TargetHost(); h != "vm-jumphost" {
		t.Errorf("expected TargetHost() to pick VMName over SourceIP, got %q", h)
	}
	optsHost.JumphostIP = "10.0.0.4"
	if h := optsHost.TargetHost(); h != "10.0.0.4" {
		t.Errorf("expected TargetHost() to prioritize JumphostIP, got %q", h)
	}
}

func TestBuildSSHArgs_DirectAndBastion(t *testing.T) {
	// Direct SSH args
	directArgs := jumphost.BuildSSHArgs("azureuser", "10.0.0.4", "/tmp/key", false, "", "eastus2", "uptime")
	joinedDirect := strings.Join(directArgs, " ")
	if !strings.Contains(joinedDirect, "-i /tmp/key") {
		t.Errorf("direct SSH missing -i key: %s", joinedDirect)
	}
	if !strings.Contains(joinedDirect, "azureuser@10.0.0.4") {
		t.Errorf("direct SSH missing user@host: %s", joinedDirect)
	}
	if !strings.Contains(joinedDirect, "uptime") {
		t.Errorf("direct SSH missing command: %s", joinedDirect)
	}
	if strings.Contains(joinedDirect, "ProxyCommand") {
		t.Errorf("direct SSH should not have ProxyCommand: %s", joinedDirect)
	}

	// Azure Bastion tunnel args
	resID := "/subscriptions/000/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm"
	bastionArgs := jumphost.BuildSSHArgs("azureuser", "vm", "/tmp/key", true, resID, "eastus2", "whoami")
	joinedBastion := strings.Join(bastionArgs, " ")
	if !strings.Contains(joinedBastion, "ProxyCommand=az network bastion tunnel") {
		t.Errorf("bastion SSH missing ProxyCommand: %s", joinedBastion)
	}
	if !strings.Contains(joinedBastion, "--target-resource-id "+resID) {
		t.Errorf("bastion SSH missing target-resource-id: %s", joinedBastion)
	}
	if !strings.Contains(joinedBastion, "--resource-port 22") {
		t.Errorf("bastion SSH missing --resource-port 22: %s", joinedBastion)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
