package forge

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestPrepareKubeconfig_InlinesFilesAndRewritesLoopback(t *testing.T) {
	dir := t.TempDir()
	caBytes := []byte("-----BEGIN CERTIFICATE-----\nFAKE-CA\n-----END CERTIFICATE-----\n")
	certBytes := []byte("-----BEGIN CERTIFICATE-----\nFAKE-CERT\n-----END CERTIFICATE-----\n")
	keyBytes := []byte("-----BEGIN PRIVATE KEY-----\nFAKE-KEY\n-----END PRIVATE KEY-----\n")
	tokBytes := []byte("  my-service-account-token \n")

	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), caBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "client.crt"), certBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "client.key"), keyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token.txt"), tokBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	raw := `apiVersion: v1
kind: Config
clusters:
  - name: crc-cluster
    cluster:
      server: https://127.0.0.1:6443
      certificate-authority: ca.crt
users:
  - name: kubeadmin
    user:
      client-certificate: client.crt
      client-key: client.key
      tokenFile: token.txt
`
	out, warnings, err := PrepareKubeconfig([]byte(raw), KubeconfigOptions{
		BaseDir:            dir,
		ContainerizedForge: true,
	})
	if err != nil {
		t.Fatalf("PrepareKubeconfig: %v", err)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "host.docker.internal") {
		t.Fatalf("expected loopback rewrite warning, got %v", warnings)
	}

	var parsed map[string]any
	if err := yaml.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}

	clusters := parsed["clusters"].([]any)
	clusterMap := clusters[0].(map[string]any)["cluster"].(map[string]any)
	if got := clusterMap["server"]; got != "https://host.docker.internal:6443" {
		t.Errorf("server = %v, want https://host.docker.internal:6443", got)
	}
	if got := clusterMap["tls-server-name"]; got != "127.0.0.1" {
		t.Errorf("tls-server-name = %v, want 127.0.0.1", got)
	}
	if _, ok := clusterMap["certificate-authority"]; ok {
		t.Error("expected certificate-authority file key to be removed after inlining")
	}
	if got := clusterMap["certificate-authority-data"]; got != base64.StdEncoding.EncodeToString(caBytes) {
		t.Errorf("certificate-authority-data = %v", got)
	}

	users := parsed["users"].([]any)
	userMap := users[0].(map[string]any)["user"].(map[string]any)
	for _, k := range []string{"client-certificate", "client-key", "tokenFile"} {
		if _, ok := userMap[k]; ok {
			t.Errorf("expected %s file key to be removed after inlining", k)
		}
	}
	if got := userMap["client-certificate-data"]; got != base64.StdEncoding.EncodeToString(certBytes) {
		t.Errorf("client-certificate-data = %v", got)
	}
	if got := userMap["client-key-data"]; got != base64.StdEncoding.EncodeToString(keyBytes) {
		t.Errorf("client-key-data = %v", got)
	}
	if got := userMap["token"]; got != "my-service-account-token" {
		t.Errorf("token = %v, want my-service-account-token", got)
	}
}

func TestPrepareKubeconfig_ServerURLOverride(t *testing.T) {
	raw := `apiVersion: v1
kind: Config
clusters:
  - name: onprem
    cluster:
      server: https://192.168.122.10:6443
`
	out, _, err := PrepareKubeconfig([]byte(raw), KubeconfigOptions{
		ServerURL: "https://10.10.0.50:6443",
	})
	if err != nil {
		t.Fatalf("PrepareKubeconfig: %v", err)
	}
	if !strings.Contains(string(out), "https://10.10.0.50:6443") {
		t.Fatalf("expected overridden server URL in output:\n%s", string(out))
	}
}

func TestPingKubeconfigServer(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"major":"1","minor":"35","gitVersion":"v1.35.0"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	kc := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
  - name: test
    cluster:
      server: %s
      insecure-skip-tls-verify: true
contexts:
  - name: test
    context:
      cluster: test
      user: test
current-context: test
users:
  - name: test
    user:
      token: dummy
`, srv.URL)

	ver, err := PingKubeconfigServer(context.Background(), []byte(kc), 3*time.Second)
	if err != nil {
		t.Fatalf("PingKubeconfigServer: %v", err)
	}
	if ver != "v1.35.0" {
		t.Errorf("ver = %q, want v1.35.0", ver)
	}
}
