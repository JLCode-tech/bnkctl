package intent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadUnifiedAndLegacyFormats(t *testing.T) {
	t.Run("awsbnkctl legacy YAML", func(t *testing.T) {
		yamlData := `apiVersion: awsbnkctl/v1
kind: Cluster
metadata:
  name: aws-demo
  region: ap-southeast-2
pattern: external-only
network:
  vpcCidr: 10.90.0.0/16
  azs:
    - ap-southeast-2a
  subnets:
    public:
      - cidr: 10.90.1.0/24
        az: ap-southeast-2a
    private:
      - cidr: 10.90.10.0/24
        az: ap-southeast-2a
  dataPath:
    external:
      cidr: 10.90.100.0/24
      az: ap-southeast-2a
  natGateways: 1
cluster:
  kubernetesVersion: "1.34"
  nodeGroups:
    - name: system
      instanceType: "m6i.4xlarge"
      minSize: 3
      maxSize: 4
      desiredSize: 3
bnk:
  farArchive: "./f5-far.tar.gz"
  jwt: "./jwt"
  manifestVersion: "2.4.0"
  deploymentSize: "Small"
  tmmMtu: "1500"
`
		tmp := filepath.Join(t.TempDir(), "cluster.yaml")
		if err := os.WriteFile(tmp, []byte(yamlData), 0o600); err != nil {
			t.Fatal(err)
		}
		cl, err := Load(tmp)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if got := cl.EffectiveProvider(); got != ProviderAWS {
			t.Fatalf("EffectiveProvider = %q, want %q", got, ProviderAWS)
		}
		if got := cl.EffectiveName(); got != "aws-demo" {
			t.Fatalf("EffectiveName = %q, want aws-demo", got)
		}
		if got := cl.Bnk.NormalizedDeploymentSize(); got != "small" {
			t.Fatalf("NormalizedDeploymentSize = %q, want small", got)
		}
		if got := cl.Bnk.TmmMtuInt(); got != 1500 {
			t.Fatalf("TmmMtuInt = %d, want 1500", got)
		}
		if got := cl.Bnk.TmmMtuString(); got != "1500" {
			t.Fatalf("TmmMtuString = %q, want \"1500\"", got)
		}
		if len(cl.Network.Subnets.PublicList) != 1 {
			t.Fatalf("PublicList len = %d, want 1", len(cl.Network.Subnets.PublicList))
		}
	})

	t.Run("azrbnkctl legacy YAML with int MTU and pass-through overrides", func(t *testing.T) {
		yamlData := `apiVersion: azrbnkctl/v1
kind: Cluster
metadata:
  name: azr-demo
  region: australiaeast
  subscriptionId: sub-123
  resourceGroup: rg-demo
bnk:
  farArchive: "./f5-far.tar.gz"
  jwt: "./jwt"
  version: "2.4.0"
  deploymentSize: "Medium"
  tmmMtu: 1450
  controllerEnv:
    CUSTOM_FLAG: "true"
  tmmEnv:
    TMM_DEBUG: "1"
`
		tmp := filepath.Join(t.TempDir(), "cluster.yaml")
		if err := os.WriteFile(tmp, []byte(yamlData), 0o600); err != nil {
			t.Fatal(err)
		}
		cl, err := Load(tmp)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if got := cl.EffectiveProvider(); got != ProviderAzure {
			t.Fatalf("EffectiveProvider = %q, want %q", got, ProviderAzure)
		}
		if got := cl.Bnk.TmmMtuInt(); got != 1450 {
			t.Fatalf("TmmMtuInt = %d, want 1450", got)
		}
		if got := cl.Bnk.NormalizedDeploymentSize(); got != "medium" {
			t.Fatalf("NormalizedDeploymentSize = %q, want medium", got)
		}
		if got := cl.Bnk.ControllerEnv["CUSTOM_FLAG"]; got != "true" {
			t.Fatalf("ControllerEnv[CUSTOM_FLAG] = %q, want true", got)
		}
		if got := cl.Network.Subnets.External; got == "" {
			t.Fatalf("expected default Azure external subnet to be populated")
		}
	})

	t.Run("unified bnkctl/v1 GCP YAML", func(t *testing.T) {
		yamlData := `apiVersion: bnkctl/v1
kind: Cluster
cloud:
  provider: gcp
metadata:
  name: gke-demo
  project: my-gcp-project
  zone: australia-southeast1-a
network:
  main:
    vpc: main-vpc
    subnet: main-sub
    cidr: 10.90.1.0/24
    podsRangeName: pods
    podsCidr: 10.92.0.0/14
    svcsRangeName: svcs
    svcsCidr: 10.96.0.0/20
  external:
    vpc: ext-vpc
    subnet: ext-sub
    cidr: 10.90.2.0/24
    mtu: 1460
    podsRangeName: ext-pods
    podsCidr: 10.90.102.0/24
  internal:
    vpc: int-vpc
    subnet: int-sub
    cidr: 10.90.3.0/24
    mtu: 1460
    podsRangeName: int-pods
    podsCidr: 10.90.103.0/24
bnk:
  jwt: "./jwt"
  farSecret:
    source: file
    dockerConfigFile: "./far.json"
  version: "2.5.0"
  deploymentSize: "large"
`
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "jwt"), []byte("dummy-jwt"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "far.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		tmp := filepath.Join(dir, "cluster.yaml")
		if err := os.WriteFile(tmp, []byte(yamlData), 0o600); err != nil {
			t.Fatal(err)
		}
		cl, err := Load(tmp)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if got := cl.EffectiveProvider(); got != ProviderGCP {
			t.Fatalf("EffectiveProvider = %q, want %q", got, ProviderGCP)
		}
		if got := cl.EffectiveName(); got != "gke-demo" {
			t.Fatalf("EffectiveName = %q, want gke-demo", got)
		}
		if got := cl.EffectiveBNKVersion(); got != "2.5.0" {
			t.Fatalf("EffectiveBNKVersion = %q, want 2.5.0", got)
		}
		if got := cl.Bnk.NormalizedDeploymentSize(); got != "large" {
			t.Fatalf("NormalizedDeploymentSize = %q, want large", got)
		}
	})

	t.Run("Azure defaults KubernetesVersion to 1.35 and GCP rejects <1.32", func(t *testing.T) {
		azYAML := `apiVersion: azrbnkctl/v1
kind: Cluster
metadata:
  name: azr-k8s-default
  region: eastus
  subscriptionId: sub-123
  resourceGroup: rg-demo
bnk:
  farArchive: "./f5-far.tar.gz"
  jwt: "./jwt"
  manifestVersion: "2.4.1-3.2850.1-0.0.9"
`
		tmpAz := filepath.Join(t.TempDir(), "cluster-az.yaml")
		if err := os.WriteFile(tmpAz, []byte(azYAML), 0o600); err != nil {
			t.Fatal(err)
		}
		clAz, err := Load(tmpAz)
		if err != nil {
			t.Fatalf("Load Azure cluster failed: %v", err)
		}
		if clAz.ClusterSpec == nil || clAz.ClusterSpec.KubernetesVersion != DefaultKubernetesVersion {
			t.Fatalf("Azure default KubernetesVersion = %v, want %q", clAz.ClusterSpec, DefaultKubernetesVersion)
		}

		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "jwt"), []byte("dummy-jwt"), 0o600)
		_ = os.WriteFile(filepath.Join(dir, "far.json"), []byte("{}"), 0o600)
		gcpYAML := `apiVersion: bnkctl/v1
kind: Cluster
cloud:
  provider: gcp
metadata:
  name: gke-old-k8s
  project: my-gcp-project
  zone: australia-southeast1-a
cluster:
  kubernetesVersion: "1.31"
bnk:
  jwt: "./jwt"
  farSecret:
    source: file
    dockerConfigFile: "./far.json"
  manifestVersion: "2.4.1-3.2850.1-0.0.9"
`
		tmpGCP := filepath.Join(dir, "cluster-gcp.yaml")
		if err := os.WriteFile(tmpGCP, []byte(gcpYAML), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(tmpGCP); err == nil {
			t.Fatalf("expected Load GCP cluster with kubernetesVersion 1.31 (< %s) to fail", MinKubernetesVersion)
		}
	})
}
