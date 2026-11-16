package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JLCode-tech/bnkctl/pkg/forge"
)

func TestCLIEntrypointsAndMatrixDryRun(t *testing.T) {
	clouds := []string{"aws", "azure", "gcp"}
	versions := []string{"2.3.2-3.2598.3-0.0.464", "2.4.0", "2.5.0"}

	for _, cloud := range clouds {
		for _, ver := range versions {
			t.Run(cloud+"_"+ver, func(t *testing.T) {
				stateDir := t.TempDir()
				tmplPath := filepath.Join(stateDir, "sample.yaml")
				if err := os.WriteFile(tmplPath, []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm-{{ .VIP }}\n"), 0o600); err != nil {
					t.Fatalf("write sample template: %v", err)
				}
				aiperfPath := filepath.Join(stateDir, "aiperf.json")
				if err := os.WriteFile(aiperfPath, []byte(`{"time_to_first_token":{"p50":12.5,"p90":18.0,"p95":21.0,"p99":30.0},"inter_token_latency":{"p50":2.1,"p95":3.5,"p99":5.0},"output_token_throughput":{"avg":145.0},"benchmark_duration":{"avg":10.0},"input_sequence_length":{"sum":500.0}}`), 0o600); err != nil {
					t.Fatalf("write sample aiperf artifact: %v", err)
				}

				for _, subcmd := range [][]string{
					{"up", "--dry-run", "--cloud", cloud, "--bnk-version", ver, "--state-dir", stateDir},
					{"status", "--cloud", cloud, "--bnk-version", ver, "--state-dir", stateDir},
					{"validate", "--cloud", cloud, "--bnk-version", ver, "--state-dir", stateDir},
					{"doctor", "--dry-run", "--cloud", cloud, "--bnk-version", ver, "--state-dir", stateDir},
					{"heal", "--dry-run", "--cloud", cloud, "--bnk-version", ver, "--state-dir", stateDir},
					{"upgrade", "--dry-run", "--cloud", cloud, "--bnk-version", ver, "--to-version", "2.5.0", "--state-dir", stateDir},
					{"upgrade", "--dry-run", "--cloud", cloud, "--bnk-version", ver, "--to", "2.5.0", "--state-dir", stateDir},
					{"migrate", "--dry-run", "--cloud", cloud, "--bnk-version", "2.3.2-3.2598.3-0.0.464", "--to-version", "2.4.0", "--state-dir", stateDir},
					{"migrate-2.4", "--dry-run", "--cloud", cloud, "--bnk-version", "2.3.2-3.2598.3-0.0.464", "--to", "2.4.0", "--state-dir", stateDir},
					{"bnk", "resync", "--dry-run", "--cloud", cloud, "--bnk-version", ver, "--state-dir", stateDir},
					{"bnk", "scan", "--dry-run", "--cloud", cloud, "--bnk-version", ver, "--state-dir", stateDir},
					{"bnk", "migrate-2.4", "--dry-run", "--cloud", cloud, "--bnk-version", "2.3.2-3.2598.3-0.0.464", "--to", "2.4.0", "--state-dir", stateDir},
					{"down", "--dry-run", "--cloud", cloud, "--bnk-version", ver, "--state-dir", stateDir},
					{"topology", "--cloud", cloud, "--state-dir", stateDir},
					{"scenarios", "list"},
					{"scenarios", "show", "http-routing-e2e"},
					{"scenarios", "run", "--dry-run", "--all", "--cloud", cloud, "--state-dir", stateDir},
					{"scenarios", "--dry-run", "--all", "--cloud", cloud, "--state-dir", stateDir},
					{"test", "--cloud", cloud, "--state-dir", stateDir},
					{"test", "dns", "--dry-run", "--cloud", cloud, "--state-dir", stateDir, "example.com"},
					{"test", "connectivity", "--dry-run", "--cloud", cloud, "--state-dir", stateDir, "127.0.0.1:443"},
					{"k", "vars", "--cloud", cloud, "--state-dir", stateDir},
					{"k", "render", "--cloud", cloud, "--state-dir", stateDir, "-f", tmplPath},
					{"k", "get", "--dry-run", "--cloud", cloud, "--state-dir", stateDir, "pods", "-A"},
					{"k", "describe", "--dry-run", "--cloud", cloud, "--state-dir", stateDir, "pods", "-A"},
					{"k", "apply", "--dry-run", "--cloud", cloud, "--state-dir", stateDir, "-f", tmplPath, "--render"},
					{"k", "delete", "--dry-run", "--cloud", cloud, "--state-dir", stateDir, "pod", "demo"},
					{"k", "logs", "--dry-run", "--cloud", cloud, "--state-dir", stateDir, "f5-tmm-abc"},
					{"k", "exec", "--dry-run", "--cloud", cloud, "--state-dir", stateDir, "f5-tmm-abc", "--", "uname", "-a"},
					{"k", "port-forward", "--dry-run", "--cloud", cloud, "--state-dir", stateDir, "f5-tmm-abc", "8443:8443"},
					{"forge", "--cloud", cloud, "--state-dir", stateDir},
					{"forge", "register", "--dry-run", "--cloud", cloud, "--state-dir", stateDir},
					{"forge", "status", "--dry-run", "--cloud", cloud, "--state-dir", stateDir},
					{"forge", "unregister", "--dry-run", "--cloud", cloud, "--state-dir", stateDir},
					{"genai", "--cloud", cloud, "--state-dir", stateDir},
					{"benchmark", "--cloud", cloud, "--state-dir", stateDir},
					{"genai", "parse", "-f", aiperfPath},
					{"jumphost", "exec", "--dry-run", "--cloud", cloud, "--state-dir", stateDir, "--", "uname -a"},
					{"self", "version"},
					{"version"},
				} {
					cmd := NewRootCmd(cloud)
					var out bytes.Buffer
					cmd.SetOut(&out)
					cmd.SetErr(&out)
					cmd.SetArgs(subcmd)
					if err := cmd.Execute(); err != nil {
						t.Fatalf("command %v failed: %v\noutput: %s", subcmd, err, out.String())
					}
				}
			})
		}
	}

	// Verify SilenceErrors is set on root command
	root := NewRootCmd("")
	if !root.SilenceErrors {
		t.Fatalf("expected SilenceErrors=true on root command")
	}

	// Verify scenarios list registers all 15 scenarios
	var scOut bytes.Buffer
	root.SetOut(&scOut)
	root.SetArgs([]string{"scenarios", "list"})
	if err := root.Execute(); err != nil {
		t.Fatalf("scenarios list failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(scOut.String()), "\n")
	if len(lines) < 15 {
		t.Fatalf("expected at least 15 scenarios in list output, got %d:\n%s", len(lines), scOut.String())
	}
}

func TestPositionalConfigAndMissingConfigError(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cluster.yaml")
	cfgYAML := `apiVersion: bnkctl/v1
kind: Cluster
cloud:
  provider: aws
metadata:
  name: positional-test-cluster
  region: ap-southeast-1
pattern: external-only
network:
  vpcCidr: 10.0.0.0/16
  azs:
    - ap-southeast-1a
    - ap-southeast-1b
  subnets:
    public:
      - cidr: 10.0.1.0/24
        az: ap-southeast-1a
      - cidr: 10.0.2.0/24
        az: ap-southeast-1b
    private:
      - cidr: 10.0.11.0/24
        az: ap-southeast-1a
      - cidr: 10.0.12.0/24
        az: ap-southeast-1b
  dataPath:
    external:
      cidr: 10.0.10.0/24
      az: ap-southeast-1a
cluster:
  kubernetesVersion: "1.35"
  nodeGroups:
    - name: default
      instanceType: m5.2xlarge
      desiredSize: 3
      minSize: 3
      maxSize: 4
`
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("write cluster.yaml: %v", err)
	}

	// Positional file path should resolve positional-test-cluster in ap-southeast-1
	for _, args := range [][]string{
		{"status", cfgPath, "--state-dir", dir},
		{"status", dir, "--state-dir", dir},
		{"down", "--dry-run", cfgPath, "--state-dir", dir},
		{"forge", "unregister", "--dry-run", cfgPath, "--state-dir", dir},
	} {
		cmd := NewRootCmd("")
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("command %v failed: %v\noutput: %s", args, err, out.String())
		}
		if args[0] == "status" {
			if !strings.Contains(out.String(), "positional-test-cluster") || !strings.Contains(out.String(), "ap-southeast-1") {
				t.Fatalf("expected positional-test-cluster and ap-southeast-1 in output for %v, got:\n%s", args, out.String())
			}
		}
	}

	// Explicit non-existent config path (via -c or positional) must fail instead of silently falling back to bnk-cluster
	for _, args := range [][]string{
		{"down", "--dry-run", filepath.Join(dir, "nonexistent.yaml")},
		{"down", "--dry-run", "-c", filepath.Join(dir, "nonexistent.yaml")},
	} {
		cmd := NewRootCmd("")
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("expected error for missing config %v, got nil (output: %s)", args, out.String())
		}
	}
}

func TestForgeRegisterHardening_WorkspaceAndFlags(t *testing.T) {
	origPing := forge.PingServerVersionFunc
	defer func() { forge.PingServerVersionFunc = origPing }()
	forge.PingServerVersionFunc = func(_ context.Context, _ []byte, _ time.Duration) (string, error) {
		return "", fmt.Errorf("dial tcp 127.0.0.1:6443: connection refused")
	}

	var receivedProjectName, receivedProjectType, receivedCloudProvider string
	var receivedCredTemplateID int
	mcpCalls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok"})
		case r.URL.Path == "/api/credential-templates":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"id":                      5,
					"name":                    "Azure Local - ALM2",
					"provider":                "azure",
					"is_default":              true,
					"has_azure_credentials":   false,
					"has_azure_client_secret": false,
				},
			})
		case r.URL.Path == "/api/credential-templates/5":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":                      5,
				"name":                    "Azure Local - ALM2",
				"provider":                "azure",
				"is_default":              true,
				"has_azure_credentials":   false,
				"has_azure_client_secret": false,
			})
		case strings.HasPrefix(r.URL.Path, "/mcp"):
			body, _ := io.ReadAll(r.Body)
			var rpc struct {
				ID     int64           `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			_ = json.Unmarshal(body, &rpc)
			if rpc.ID == 0 && rpc.Method == "notifications/initialized" {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			if rpc.Method == "initialize" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      rpc.ID,
					"result":  map[string]any{"protocolVersion": "2024-11-05"},
				})
				return
			}
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(rpc.Params, &p)
			mcpCalls++
			var text string
			switch p.Name {
			case "create_project":
				receivedProjectName, _ = p.Arguments["name"].(string)
				receivedProjectType, _ = p.Arguments["project_type"].(string)
				receivedCloudProvider, _ = p.Arguments["cloud_provider"].(string)
				if v, ok := p.Arguments["credential_template_id"].(float64); ok {
					receivedCredTemplateID = int(v)
				}
				text = fmt.Sprintf(`{"project":{"id":69,"name":%q},"success":true}`, receivedProjectName)
			case "create_cluster":
				text = `{"cluster":{"id":54,"name":"poc-dual-interface"},"success":true}`
			case "scan_cluster":
				text = `{"bnk_install":{"status":"installed","tmm":{"pods":2,"running":2},"controller":{"pods":1,"running":1},"crds":{"groups":["gateway.k8s.f5.com"]}}}`
			case "bnk_health":
				text = `{"overall":"healthy","platform":"healthy","dataPlane":"healthy"}`
			case "get_cluster":
				text = `{"id":54,"name":"poc-dual-interface","status":"connected"}`
			default:
				text = `{}`
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      rpc.ID,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": text}},
				},
			})
		}
	}))
	defer srv.Close()

	baseDir := t.TempDir()
	wsDir := filepath.Join(baseDir, ".azrbnkctl", "poc-dual-interface")
	if err := os.MkdirAll(wsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	kcFile := filepath.Join(wsDir, "kubeconfig")
	if err := os.WriteFile(kcFile, []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: aks\n  cluster:\n    server: https://127.0.0.1:6443\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "state.env"), []byte("AKS_CLUSTER_NAME=aks-poc-dual-interface\nLOCATION=eastus2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := NewRootCmd("")
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"forge", "register",
		"-c", wsDir,
		"--mcp-url", srv.URL + "/mcp/",
		"--forge-url", srv.URL,
		"--credential-template", "Azure Local - ALM2",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("forge register failed: %v\noutput:\n%s", err, out.String())
	}

	output := out.String()
	if receivedProjectName != "bnkctl-poc-dual-interface" {
		t.Errorf("receivedProjectName = %q, want bnkctl-poc-dual-interface", receivedProjectName)
	}
	if receivedCloudProvider != "azure" || receivedProjectType != "cloud-azure" {
		t.Errorf("cloud/type = %q/%q, want azure/cloud-azure", receivedCloudProvider, receivedProjectType)
	}
	if receivedCredTemplateID != 5 {
		t.Errorf("receivedCredTemplateID = %d, want 5", receivedCredTemplateID)
	}
	for _, wantSub := range []string{
		"Warning: pre-flight cluster reachability check failed",
		"lacks Azure credentials",
		"Scan: bnk=installed",
		"Health: overall=healthy",
	} {
		if !strings.Contains(output, wantSub) {
			t.Errorf("expected output to contain %q, got:\n%s", wantSub, output)
		}
	}

	// Second call with --rescan should bypass idempotency and re-run scan_cluster
	mcpCallsBefore := mcpCalls
	cmd2 := NewRootCmd("")
	var out2 bytes.Buffer
	cmd2.SetOut(&out2)
	cmd2.SetErr(&out2)
	cmd2.SetArgs([]string{
		"forge", "register",
		"-c", wsDir,
		"--mcp-url", srv.URL + "/mcp/",
		"--forge-url", srv.URL,
		"--rescan",
	})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("forge register --rescan failed: %v", err)
	}
	if mcpCalls <= mcpCallsBefore+1 {
		t.Errorf("expected --rescan to re-run create_project/create_cluster/scan_cluster, mcpCalls delta = %d", mcpCalls-mcpCallsBefore)
	}
}
