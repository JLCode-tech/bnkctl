package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
