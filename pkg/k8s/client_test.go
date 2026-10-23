// Unit tests for the client builder helpers in
// `internal/k8s/client.go` (the Build* constructors).
//
// We can exercise the kubeconfig-resolution branches without a real
// cluster by feeding the helpers a minimal but valid kubeconfig file
// and asserting they return non-nil clients with the expected
// transport configuration.

package k8s

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimalKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: stub
  cluster:
    server: https://stub.example.com:6443
    insecure-skip-tls-verify: true
contexts:
- name: stub
  context:
    cluster: stub
    user: stub
current-context: stub
users:
- name: stub
  user:
    token: stub-token
`

// writeKubeconfig drops a minimal kubeconfig into a fresh temp dir and
// returns its path.
func writeKubeconfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(p, []byte(minimalKubeconfig), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	return p
}

// TestBuildRESTConfig_FromFile parses a stub kubeconfig and returns a
// rest.Config with the expected server URL.
func TestBuildRESTConfig_FromFile(t *testing.T) {
	path := writeKubeconfig(t)
	cfg, err := BuildRESTConfig(path)
	if err != nil {
		t.Fatalf("BuildRESTConfig err: %v", err)
	}
	if cfg.Host != "https://stub.example.com:6443" {
		t.Errorf("Host: got %q; want https://stub.example.com:6443", cfg.Host)
	}
}

// TestBuildRESTConfig_EmptyPath_NoEnvNoFile returns a clear error when
// nothing's available rather than panicking.
func TestBuildRESTConfig_EmptyPath_NoEnvNoFile(t *testing.T) {
	t.Setenv("KUBECONFIG", "/dev/null/no-such")
	t.Setenv("HOME", t.TempDir())
	_, err := BuildRESTConfig("")
	if err == nil {
		t.Fatal("expected error when no kubeconfig is reachable; got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "kubeconfig") {
		t.Errorf("expected 'kubeconfig' in err; got: %v", err)
	}
}

// TestBuildClientset_FromFile builds a typed clientset against a stub
// kubeconfig. We don't make any API calls; we just verify the
// constructor wires up.
func TestBuildClientset_FromFile(t *testing.T) {
	path := writeKubeconfig(t)
	cs, err := BuildClientset(path)
	if err != nil {
		t.Fatalf("BuildClientset err: %v", err)
	}
	if cs == nil {
		t.Fatal("nil clientset")
	}
}

// TestBuildDynamicClient_FromFile is the dynamic-client mirror.
func TestBuildDynamicClient_FromFile(t *testing.T) {
	path := writeKubeconfig(t)
	dc, err := BuildDynamicClient(path)
	if err != nil {
		t.Fatalf("BuildDynamicClient err: %v", err)
	}
	if dc == nil {
		t.Fatal("nil dynamic client")
	}
}

// TestInClusterSentinel_Constant guards against accidental rename of
// the magic string the in-cluster K8s execution backend keys off.
func TestInClusterSentinel_Constant(t *testing.T) {
	if InClusterKubeconfigSentinel != "in-cluster" {
		t.Errorf("InClusterKubeconfigSentinel value drift: got %q; want \"in-cluster\"",
			InClusterKubeconfigSentinel)
	}
}
