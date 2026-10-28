package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/manifests"
)

func clusterFixture(name string) *intent.Cluster {
	return &intent.Cluster{
		Metadata: intent.Metadata{Name: name, Region: "ap-southeast-2"},
	}
}

func TestRenderCertChain_HappyPath(t *testing.T) {
	tmpl := []byte(`
issuer: {{ .SelfSignedIssuer }}
cert: {{ .CACertName }}
secret: {{ .CASecretName }}
ca: {{ .CAIssuer }}
`)
	cl := clusterFixture("syd-tracer")
	out, err := RenderCertChain(tmpl, cl)
	if err != nil {
		t.Fatalf("RenderCertChain: %v", err)
	}
	checks := []string{
		"syd-tracer-selfsigned-cluster-issuer",
		"syd-tracer-ca",
		"syd-tracer-ca-secret",
		"syd-tracer-ca-cluster-issuer",
	}
	for _, want := range checks {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("rendered output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestCertChainVarsFromCluster_NamesMatchConvention(t *testing.T) {
	cl := clusterFixture("my-cluster")
	v := CertChainVarsFromCluster(cl)

	if v.SelfSignedIssuer != "my-cluster-selfsigned-cluster-issuer" {
		t.Errorf("SelfSignedIssuer: got %q", v.SelfSignedIssuer)
	}
	if v.CACertName != "my-cluster-ca" {
		t.Errorf("CACertName: got %q", v.CACertName)
	}
	if v.CASecretName != "my-cluster-ca-secret" {
		t.Errorf("CASecretName: got %q", v.CASecretName)
	}
	if v.CAIssuer != "my-cluster-ca-cluster-issuer" {
		t.Errorf("CAIssuer: got %q", v.CAIssuer)
	}
}

func TestRender_BadTemplate(t *testing.T) {
	tmpl := []byte("{{ .Unclosed")
	_, err := Render(tmpl, struct{}{})
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse template") {
		t.Errorf("error should mention 'parse template': %v", err)
	}
}

func TestRender_MissingFieldStillRenders(t *testing.T) {
	// Go text/template by default renders <no value> for missing fields.
	// This test ensures we don't panic and documents the behaviour.
	tmpl := []byte("value: {{ .NotAField }}")
	type empty struct{}
	out, err := Render(tmpl, empty{})
	if err != nil {
		// With the default template option (zero-value), missing field is not an error.
		// If it is, we accept that too — document the output.
		t.Logf("Render with missing field returned error (accepted): %v", err)
		return
	}
	t.Logf("Render with missing field produced: %s", out)
}

// ─── FLO values render tests ─────────────────────────────────────────────────

func TestRenderFLOValues_Substitution(t *testing.T) {
	cl := clusterFixture("syd-tracer")
	jwt := "test-jwt-content"
	tmpl := []byte(`caIssuer: {{ .CAIssuer }}
farSecret: {{ .FARSecretName }}
jwt: {{ .JWT }}
cluster: {{ .ClusterName }}`)

	out, err := RenderFLOValues(tmpl, cl, jwt)
	if err != nil {
		t.Fatalf("RenderFLOValues: %v", err)
	}
	rendered := string(out)

	checks := map[string]string{
		"caIssuer":  "syd-tracer-ca-cluster-issuer",
		"farSecret": "far-secret",
		"jwt":       jwt,
		"cluster":   "syd-tracer",
	}
	for field, want := range checks {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered FLO values missing %s=%q:\n%s", field, want, rendered)
		}
	}
}

func TestFLOValuesVarsFromCluster_Names(t *testing.T) {
	cl := clusterFixture("my-cluster")
	v := FLOValuesVarsFromCluster(cl, "my-jwt")

	if v.CAIssuer != "my-cluster-ca-cluster-issuer" {
		t.Errorf("CAIssuer: got %q", v.CAIssuer)
	}
	if v.FARSecretName != "far-secret" {
		t.Errorf("FARSecretName: got %q", v.FARSecretName)
	}
	if v.JWT != "my-jwt" {
		t.Errorf("JWT: got %q", v.JWT)
	}
	if v.ClusterName != "my-cluster" {
		t.Errorf("ClusterName: got %q", v.ClusterName)
	}
}

// ─── OTEL certs render tests ─────────────────────────────────────────────────

func TestRenderOTELCerts_Substitution(t *testing.T) {
	cl := clusterFixture("syd-tracer")
	tmpl := []byte(`otelSvr: {{ .OTELSvrCert }}
otelSvrSecret: {{ .OTELSvrSecret }}
otelF5Ing: {{ .OTELF5IngCert }}
otelF5IngSecret: {{ .OTELF5IngSecret }}
ns: {{ .OperatorNS }}
issuer: {{ .CAIssuer }}`)

	out, err := RenderOTELCerts(tmpl, cl)
	if err != nil {
		t.Fatalf("RenderOTELCerts: %v", err)
	}
	rendered := string(out)

	checks := map[string]string{
		"otelSvr":         "external-otelsvr",
		"otelSvrSecret":   "external-otelsvr-secret",
		"otelF5Ing":       "external-f5ingotelsvr",
		"otelF5IngSecret": "external-f5ingotelsvr-secret",
		"ns":              "f5-cne-core",
		"issuer":          "syd-tracer-ca-cluster-issuer",
	}
	for field, want := range checks {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered OTEL certs missing %s=%q:\n%s", field, want, rendered)
		}
	}
}

func TestOTELCertsVarsFromCluster_Names(t *testing.T) {
	cl := clusterFixture("my-cluster")
	v := OTELCertsVarsFromCluster(cl)

	if v.OTELSvrCert != "external-otelsvr" {
		t.Errorf("OTELSvrCert: got %q", v.OTELSvrCert)
	}
	if v.OTELSvrSecret != "external-otelsvr-secret" {
		t.Errorf("OTELSvrSecret: got %q", v.OTELSvrSecret)
	}
	if v.OTELF5IngCert != "external-f5ingotelsvr" {
		t.Errorf("OTELF5IngCert: got %q", v.OTELF5IngCert)
	}
	if v.OTELF5IngSecret != "external-f5ingotelsvr-secret" {
		t.Errorf("OTELF5IngSecret: got %q", v.OTELF5IngSecret)
	}
	if v.OperatorNS != "f5-cne-core" {
		t.Errorf("OperatorNS: got %q", v.OperatorNS)
	}
	if v.CAIssuer != "my-cluster-ca-cluster-issuer" {
		t.Errorf("CAIssuer: got %q", v.CAIssuer)
	}
}

func TestRender_DefaultValues(t *testing.T) {
	// Verify that CertChainVarsFromCluster applies the correct naming defaults
	// even when cluster has minimal spec.
	cl := clusterFixture("tracer")
	v := CertChainVarsFromCluster(cl)

	// All names should be derived from cluster name
	if !strings.HasPrefix(v.SelfSignedIssuer, "tracer-") {
		t.Errorf("SelfSignedIssuer should start with 'tracer-': got %q", v.SelfSignedIssuer)
	}
	if !strings.HasPrefix(v.CACertName, "tracer-") {
		t.Errorf("CACertName should start with 'tracer-': got %q", v.CACertName)
	}
}

// ─── CloudNetworkMapping render tests ─────────────────────────────────────────

// cloudNetworkMappingTmpl is a minimal template that exercises all substitution
// variables of cloud-network-mapping.yaml.tmpl.
var cloudNetworkMappingTmpl = []byte(`{{- range .AZs }}
az: {{ .Name }}
{{- range .Subnets }}
subnet: {{ .SubnetID }} cidr: {{ .CIDR }}
{{- end }}
{{- end }}
`)

// hostDeviceClusterForRender returns a cluster fixture with the host-device
// pattern and the required dataPath block.
func hostDeviceClusterForRender(name string) *intent.Cluster {
	return &intent.Cluster{
		Metadata: intent.Metadata{Name: name, Region: "ap-southeast-2"},
		Pattern:  "host-device",
		Network: intent.Network{
			VPCCidr: "10.0.0.0/16",
			AZs:     []string{"ap-southeast-2a", "ap-southeast-2b"},
			Subnets: intent.Subnets{
				PublicList:  []intent.SubnetSpec{{CIDR: "10.0.1.0/24", AZ: "ap-southeast-2a"}},
				PrivateList: []intent.SubnetSpec{{CIDR: "10.0.11.0/24", AZ: "ap-southeast-2a"}},
			},
			DataPath: &intent.DataPathSpec{
				External: intent.SubnetSpec{CIDR: "10.0.20.0/24", AZ: "ap-southeast-2a"},
				Internal: intent.SubnetSpec{CIDR: "10.0.21.0/24", AZ: "ap-southeast-2a"},
			},
		},
	}
}

// populatedStateGetter simulates st.Get for a populated state.
func populatedStateGetter(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

func TestRenderCloudNetworkMapping_Substitution(t *testing.T) {
	cl := hostDeviceClusterForRender("syd-tracer")
	getter := populatedStateGetter(map[string]string{
		"MGMT_SUBNET":    "subnet-pub-001",
		"BNK_EXT_SUBNET": "subnet-ext-001",
		"BNK_INT_SUBNET": "subnet-int-001",
	})

	out, err := RenderCloudNetworkMapping(cloudNetworkMappingTmpl, cl, getter)
	if err != nil {
		t.Fatalf("RenderCloudNetworkMapping: %v", err)
	}
	rendered := string(out)

	checks := []string{
		"az: ap-southeast-2a", // first AZ
		"subnet: subnet-pub-001 cidr: 10.0.1.0/24",
		"subnet: subnet-ext-001 cidr: 10.0.20.0/24",
		"subnet: subnet-int-001 cidr: 10.0.21.0/24",
	}
	for _, want := range checks {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered cloud-network-mapping missing %q:\n%s", want, rendered)
		}
	}
}

func TestRenderCloudNetworkMapping_MultipleAZsUsesFirst(t *testing.T) {
	cl := hostDeviceClusterForRender("multi-az")
	// cl.Network.AZs has two entries — verify first is used.
	getter := populatedStateGetter(map[string]string{
		"MGMT_SUBNET":    "subnet-pub-001",
		"BNK_EXT_SUBNET": "subnet-ext-001",
		"BNK_INT_SUBNET": "subnet-int-001",
	})

	out, err := RenderCloudNetworkMapping(cloudNetworkMappingTmpl, cl, getter)
	if err != nil {
		t.Fatalf("RenderCloudNetworkMapping multi-az: %v", err)
	}
	if !strings.Contains(string(out), "ap-southeast-2a") {
		t.Errorf("expected first AZ ap-southeast-2a in output:\n%s", out)
	}
	if strings.Contains(string(out), "ap-southeast-2b") {
		t.Errorf("second AZ ap-southeast-2b should not appear in output:\n%s", out)
	}
}

func TestRenderCloudNetworkMapping_MissingMGMTSubnet_Errors(t *testing.T) {
	cl := hostDeviceClusterForRender("syd-tracer")
	// Empty state getter — MGMT_SUBNET missing.
	getter := func(string) string { return "" }

	_, err := RenderCloudNetworkMapping(cloudNetworkMappingTmpl, cl, getter)
	if err == nil {
		t.Fatal("expected error when MGMT_SUBNET missing, got nil")
	}
}

func TestRenderCloudNetworkMapping_NilDataPath_Errors(t *testing.T) {
	cl := hostDeviceClusterForRender("syd-tracer")
	cl.Network.DataPath = nil
	getter := populatedStateGetter(map[string]string{
		"MGMT_SUBNET":    "subnet-pub-001",
		"BNK_EXT_SUBNET": "subnet-ext-001",
		"BNK_INT_SUBNET": "subnet-int-001",
	})

	_, err := RenderCloudNetworkMapping(cloudNetworkMappingTmpl, cl, getter)
	if err == nil {
		t.Fatal("expected error when dataPath is nil, got nil")
	}
}

// TestRenderCloudNetworkMapping_EmbeddedShape verifies that the real embedded
// cloud-network-mapping.yaml.tmpl uses the YAML-under-config.yaml schema that
// the CNE controller expects (data key "config.yaml", top-level
// "availability_zones" list).
func TestRenderCloudNetworkMapping_EmbeddedShape(t *testing.T) {
	raw, err := manifests.FS.ReadFile("shared/cloud-network-mapping.yaml.tmpl")
	if err != nil {
		t.Fatalf("read embedded template: %v", err)
	}
	cl := hostDeviceClusterForRender("syd-tracer")
	getter := populatedStateGetter(map[string]string{
		"MGMT_SUBNET":    "subnet-pub-001",
		"BNK_EXT_SUBNET": "subnet-ext-001",
		"BNK_INT_SUBNET": "subnet-int-001",
	})

	out, err := RenderCloudNetworkMapping(raw, cl, getter)
	if err != nil {
		t.Fatalf("RenderCloudNetworkMapping with embedded template: %v", err)
	}
	rendered := string(out)

	// CNE controller reads data["config.yaml"] — assert the key is present.
	if !strings.Contains(rendered, "config.yaml:") {
		t.Errorf("embedded template missing data key 'config.yaml:':\n%s", rendered)
	}
	// CNE controller parses availability_zones[] — assert the structure is present.
	if !strings.Contains(rendered, "availability_zones:") {
		t.Errorf("embedded template missing top-level 'availability_zones:':\n%s", rendered)
	}
	// Verify the old wrong key is not present.
	if strings.Contains(rendered, "cloud-network-mapping.json") {
		t.Errorf("embedded template must not contain old key 'cloud-network-mapping.json':\n%s", rendered)
	}
}

// ─── NADs render tests ─────────────────────────────────────────────────────────

var nadsTmpl = []byte(`ns: {{ .Namespace }}
ext: {{ .ExternalNADName }}
int: {{ .InternalNADName }}
extPCI: {{ .ExternalPCI }}
intPCI: {{ .InternalPCI }}
`)

func TestRenderNADs_Substitution(t *testing.T) {
	out, err := RenderNADs(nadsTmpl, "f5-cne-system", true, func(string) string { return "" })
	if err != nil {
		t.Fatalf("RenderNADs: %v", err)
	}
	rendered := string(out)

	checks := map[string]string{
		"ns":     "f5-cne-system",
		"ext":    "external-netdevice",
		"int":    "internal-netdevice",
		"extPCI": "0000:00:08.0",
		"intPCI": "0000:00:07.0",
	}
	// NADs must use pciBusID selector, not Linux interface name.
	if strings.Contains(rendered, `"device"`) {
		t.Errorf("rendered NADs must not contain \"device\" key (use pciBusID instead):\n%s", rendered)
	}
	if !strings.Contains(rendered, "pciBusID") && !strings.Contains(rendered, "0000:00:08.0") {
		t.Errorf("rendered NADs must reference pciBusID or PCI address:\n%s", rendered)
	}
	for field, want := range checks {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered NADs missing %s=%q:\n%s", field, want, rendered)
		}
	}
}

func TestRenderNADs_DefaultNamespace(t *testing.T) {
	out, err := RenderNADs(nadsTmpl, "default", true, func(string) string { return "" })
	if err != nil {
		t.Fatalf("RenderNADs default ns: %v", err)
	}
	if !strings.Contains(string(out), "default") {
		t.Errorf("expected 'default' namespace in output:\n%s", out)
	}
}

func TestRenderNADs_Constants(t *testing.T) {
	// Verify the hardcoded constants are correct (these are architecture constraints).
	// NADs select by PCI bus address (robust against udev interface-name drift).
	tmpl := []byte(`{{ .ExternalPCI }} {{ .InternalPCI }} {{ .ExternalNADName }} {{ .InternalNADName }}`)
	out, err := RenderNADs(tmpl, "f5-cne-system", true, func(string) string { return "" })
	if err != nil {
		t.Fatalf("RenderNADs constants: %v", err)
	}
	rendered := string(out)
	for _, want := range []string{"0000:00:08.0", "0000:00:07.0", "external-netdevice", "internal-netdevice"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("constant %q missing from output: %s", want, rendered)
		}
	}
}

// ─── RenderCNEInstance tests ──────────────────────────────────────────────────

// cneInstanceCluster returns a cluster with BnkSpec fully populated for CNEInstance tests.
func cneInstanceCluster() *intent.Cluster {
	return &intent.Cluster{
		Metadata: intent.Metadata{Name: "syd-tracer", Region: "ap-southeast-2"},
		// dual-interface so the rendered CR lists both NADs + internal ROBIN/PCIDEVICE env.
		Pattern: intent.PatternDualInterface,
		Bnk: &intent.BnkSpec{
			FARArchive:       "/dev/null",
			JWT:              "/dev/null",
			DeploymentSize:   "Small",
			StorageClassName: "gp3",
			ManifestVersion:  "2.21.13",
			TmmMtu:           9000,
			TmmCpu:           "4",
			TmmMemory:        "16Gi",
			TmmHugepages:     "8Gi",
			PalCpuSet:        "0-3",
		},
	}
}

func TestRenderCNEInstance_HappyPath(t *testing.T) {
	tmplBytes, err := manifests.FS.ReadFile("shared/cneinstance.yaml.tmpl")
	if err != nil {
		t.Fatalf("read cneinstance template: %v", err)
	}
	cl := cneInstanceCluster()
	getter := func(key string) string {
		if key == "VPC_ID" {
			return "vpc-0abc12345"
		}
		if key == ServiceCIDRStateKey {
			return "172.20.0.0/16"
		}
		return ""
	}

	out, err := RenderCNEInstance(tmplBytes, cl, getter)
	if err != nil {
		t.Fatalf("RenderCNEInstance: %v", err)
	}
	rendered := string(out)

	// State-derived substitutions.
	stateChecks := []string{
		"name: syd-tracer-bnk",
		"namespace: f5-cne-system",
		"instance: syd-tracer",
		"clusterIssuer: syd-tracer-ca-cluster-issuer",
		"name: far-secret",
		"- external-netdevice",
		"- internal-netdevice",
		"value: \"vpc-0abc12345\"",
		"value: \"ap-southeast-2\"",
		"value: \"ens8\"",          // CLOUD_HOST_DEVICE_NAME
		"value: \"f5-cne-device\"", // CLOUD_HOST_DEVICE_TAG
		"- name: TMM_K8S_ROUTES",   // the f5-tmm chart's add_k8s_routes, fed from EKS_SERVICE_CIDR
		"value: \"172.20.0.0/16\"",
	}
	for _, want := range stateChecks {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered CNEInstance missing %q:\n%s", want, rendered)
		}
	}

	// Operator-knob substitutions.
	opChecks := []string{
		`deploymentSize: "Small"`,
		"storageClassName: gp3",
		`manifestVersion: "2.21.13"`,
		`cpu: "4"`,
		"memory: \"16Gi\"",
		"hugepages-2Mi: \"8Gi\"",
		`value: "0-3"`, // PAL_CPU_SET
	}
	for _, want := range opChecks {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered CNEInstance missing operator-knob %q:\n%s", want, rendered)
		}
	}

	// Hardcoded constants (NOT substituted — present verbatim in template).
	constChecks := []string{
		"gatewayAPI: true",
		"type: BNK",
		"wholeCluster: true",
		"tmmReplicas: 1",
		"imagePullPolicy: Always",
		`value: "true"`,  // CLOUD_ENV
		"value: \"aws\"", // CLOUD_PROVIDER
		"value: \"AWS\"", // PLATFORM_TYPE
		"- name: TMM_SOCAT_DISABLE\n          value: \"1\"",
		"- name: ROBIN_VFIO_RESOURCE_1\n          value: \"ENS8\"",
		"- name: ROBIN_VFIO_RESOURCE_2\n          value: \"ENS7\"",
	}
	for _, want := range constChecks {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered CNEInstance missing constant %q:\n%s", want, rendered)
		}
	}
}

// BNK 2.4 gates the Infra and GatewaySettings reconcilers behind
// USE_GATEWAY_SETTINGS in the controller container. The template must set
// it in advanced.cneController.env (not the TMM env block).
func TestRenderCNEInstance_SetsUseGatewaySettings(t *testing.T) {
	tmplBytes, err := manifests.FS.ReadFile("shared/cneinstance.yaml.tmpl")
	if err != nil {
		t.Fatalf("read cneinstance template: %v", err)
	}
	out, err := RenderCNEInstance(tmplBytes, cneInstanceCluster(), func(key string) string {
		if key == "VPC_ID" {
			return "vpc-0abc12345"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("RenderCNEInstance: %v", err)
	}
	rendered := string(out)

	ctrlStart := strings.Index(rendered, "cneController:")
	tmmStart := strings.Index(rendered, "    tmm:")
	if ctrlStart < 0 || tmmStart < 0 || ctrlStart > tmmStart {
		t.Fatalf("unexpected advanced block layout (cneController=%d tmm=%d):\n%s", ctrlStart, tmmStart, rendered)
	}
	ctrlEnv := rendered[ctrlStart:tmmStart]
	if !strings.Contains(ctrlEnv, "- name: USE_GATEWAY_SETTINGS\n          value: \"true\"") {
		t.Errorf("cneController.env missing USE_GATEWAY_SETTINGS=true:\n%s", ctrlEnv)
	}
	if strings.Count(rendered, "USE_GATEWAY_SETTINGS") != 1 {
		t.Errorf("USE_GATEWAY_SETTINGS must appear exactly once, got %d", strings.Count(rendered, "USE_GATEWAY_SETTINGS"))
	}
	if !strings.Contains(ctrlEnv, "- name: MAX_ACTIVE_TMM_REPLICAS\n          value: \"32\"") {
		t.Errorf("cneController.env missing MAX_ACTIVE_TMM_REPLICAS=32 (FLO 2.30 drops the chart default):\n%s", ctrlEnv)
	}
}

func TestRenderCNEInstance_NilBnk_ReturnsError(t *testing.T) {
	tmpl := []byte(`{{ .InstanceNameCR }}`)
	cl := clusterFixture("syd-tracer")
	// cl.Bnk is nil.
	_, err := RenderCNEInstance(tmpl, cl, func(string) string { return "" })
	if err == nil {
		t.Fatal("expected error for nil Bnk, got nil")
	}
}

func TestRenderCNEInstance_MissingVPCID_ReturnsError(t *testing.T) {
	tmpl := []byte(`{{ .VPCID }}`)
	cl := cneInstanceCluster()
	// getter returns empty for VPC_ID.
	_, err := RenderCNEInstance(tmpl, cl, func(string) string { return "" })
	if err == nil {
		t.Fatal("expected error for missing VPC_ID, got nil")
	}
}

// TestRenderCNEInstance_EmbeddedShape verifies the real embedded template
// renders without errors and contains no leftover $VAR placeholders.
func TestRenderCNEInstance_EmbeddedShape(t *testing.T) {
	tmplBytes, err := manifests.FS.ReadFile("shared/cneinstance.yaml.tmpl")
	if err != nil {
		t.Fatalf("read cneinstance template: %v", err)
	}
	cl := cneInstanceCluster()
	getter := func(key string) string {
		switch key {
		case "VPC_ID":
			return "vpc-test"
		}
		return ""
	}

	out, err := RenderCNEInstance(tmplBytes, cl, getter)
	if err != nil {
		t.Fatalf("RenderCNEInstance (embedded): %v", err)
	}
	if strings.Contains(string(out), "$") {
		t.Errorf("rendered output still contains $ placeholder:\n%s", out)
	}
}

// ─── RenderNADs getter-sourced PCI tests ──────────────────────────────────────

func TestRenderNADs_GetterPCI_Populated(t *testing.T) {
	// When getter returns values, those PCI addresses must appear in the output.
	tmpl := []byte(`extPCI: {{ .ExternalPCI }}
intPCI: {{ .InternalPCI }}
`)
	getter := populatedStateGetter(map[string]string{
		"EXTERNAL_PCI": "0000:00:0a.0",
		"INTERNAL_PCI": "0000:00:09.0",
	})
	out, err := RenderNADs(tmpl, "f5-cne-system", true, getter)
	if err != nil {
		t.Fatalf("RenderNADs (populated getter): %v", err)
	}
	rendered := string(out)
	if !strings.Contains(rendered, "0000:00:0a.0") {
		t.Errorf("expected discovered external PCI in output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "0000:00:09.0") {
		t.Errorf("expected discovered internal PCI in output:\n%s", rendered)
	}
}

func TestRenderNADs_GetterPCI_EmptyFallsBackToConstants(t *testing.T) {
	// When getter returns "", the constant defaults must be used.
	tmpl := []byte(`extPCI: {{ .ExternalPCI }}
intPCI: {{ .InternalPCI }}
`)
	getter := func(string) string { return "" }
	out, err := RenderNADs(tmpl, "f5-cne-system", true, getter)
	if err != nil {
		t.Fatalf("RenderNADs (empty getter): %v", err)
	}
	rendered := string(out)
	if !strings.Contains(rendered, "0000:00:08.0") {
		t.Errorf("expected constant external PCI 0000:00:08.0 in output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "0000:00:07.0") {
		t.Errorf("expected constant internal PCI 0000:00:07.0 in output:\n%s", rendered)
	}
}

// ─── RenderCNEInstance dynamic env-var name tests ─────────────────────────────

func TestRenderCNEInstance_DynamicIFName_ENS9(t *testing.T) {
	// When EXTERNAL_IFNAME=ens9 is in state, the env-var name in the CNEInstance
	// template must be PCIDEVICE_INTEL_COM_ENS9 (not hardcoded ENS8).
	tmplBytes, err := manifests.FS.ReadFile("shared/cneinstance.yaml.tmpl")
	if err != nil {
		t.Fatalf("read cneinstance template: %v", err)
	}
	cl := cneInstanceCluster()
	getter := populatedStateGetter(map[string]string{
		"VPC_ID":          "vpc-test",
		"EXTERNAL_IFNAME": "ens9",
		"INTERNAL_IFNAME": "ens6",
		"EXTERNAL_PCI":    "0000:00:0a.0",
		"INTERNAL_PCI":    "0000:00:09.0",
	})

	out, err := RenderCNEInstance(tmplBytes, cl, getter)
	if err != nil {
		t.Fatalf("RenderCNEInstance (dynamic ifname): %v", err)
	}
	rendered := string(out)

	// Dynamic env-var names.
	if !strings.Contains(rendered, "PCIDEVICE_INTEL_COM_ENS9") {
		t.Errorf("expected PCIDEVICE_INTEL_COM_ENS9 in output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "PCIDEVICE_INTEL_COM_ENS6") {
		t.Errorf("expected PCIDEVICE_INTEL_COM_ENS6 in output:\n%s", rendered)
	}
	// Must NOT contain the hardcoded constants.
	if strings.Contains(rendered, "PCIDEVICE_INTEL_COM_ENS8") {
		t.Errorf("hardcoded PCIDEVICE_INTEL_COM_ENS8 must not appear when override set:\n%s", rendered)
	}
	// Discovered PCI values.
	if !strings.Contains(rendered, "0000:00:0a.0") {
		t.Errorf("expected discovered external PCI 0000:00:0a.0:\n%s", rendered)
	}
}

func TestRenderCNEInstance_EmptyGetterFallsBackToConstants(t *testing.T) {
	// When getter only provides VPC_ID (no ifname/PCI), constants must be used.
	tmplBytes, err := manifests.FS.ReadFile("shared/cneinstance.yaml.tmpl")
	if err != nil {
		t.Fatalf("read cneinstance template: %v", err)
	}
	cl := cneInstanceCluster()
	getter := func(key string) string {
		if key == "VPC_ID" {
			return "vpc-test"
		}
		return ""
	}

	out, err := RenderCNEInstance(tmplBytes, cl, getter)
	if err != nil {
		t.Fatalf("RenderCNEInstance (empty getter): %v", err)
	}
	rendered := string(out)

	// Should fall back to constant names.
	if !strings.Contains(rendered, "PCIDEVICE_INTEL_COM_ENS8") {
		t.Errorf("expected constant PCIDEVICE_INTEL_COM_ENS8 with empty getter:\n%s", rendered)
	}
	if !strings.Contains(rendered, "PCIDEVICE_INTEL_COM_ENS7") {
		t.Errorf("expected constant PCIDEVICE_INTEL_COM_ENS7 with empty getter:\n%s", rendered)
	}
}

// ─── RenderLicenseCR tests ────────────────────────────────────────────────────

func TestRenderLicenseCR_HappyPath(t *testing.T) {
	tmplBytes, err := manifests.FS.ReadFile("shared/license-cr.yaml.tmpl")
	if err != nil {
		t.Fatalf("read license-cr template: %v", err)
	}
	cl := clusterFixture("syd-tracer")
	jwt := "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.test-payload.test-sig"

	out, err := RenderLicenseCR(tmplBytes, cl, jwt)
	if err != nil {
		t.Fatalf("RenderLicenseCR: %v", err)
	}
	rendered := string(out)

	checks := []string{
		"name: bnk-license",
		"namespace: f5-cne-core",
		"instance: syd-tracer",
		"operationMode: connected",
		jwt, // JWT inlined verbatim
	}
	for _, want := range checks {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered License CR missing %q:\n%s", want, rendered)
		}
	}
}

func TestRenderLicenseCR_JWTInlinedVerbatim(t *testing.T) {
	// Verify the raw JWT string (not base64, not escaped) is inlined.
	tmplBytes, err := manifests.FS.ReadFile("shared/license-cr.yaml.tmpl")
	if err != nil {
		t.Fatalf("read license-cr template: %v", err)
	}
	cl := clusterFixture("syd-test")
	// #nosec G101 -- this is a test JWT placeholder value, not a credential
	jwt := "test-jwt-raw-string-12345"

	out, err := RenderLicenseCR(tmplBytes, cl, jwt)
	if err != nil {
		t.Fatalf("RenderLicenseCR: %v", err)
	}
	if !strings.Contains(string(out), jwt) {
		t.Errorf("JWT not inlined verbatim: %s", out)
	}
}

// ─── NVIDIA device-plugin render tests ───────────────────────────────────────

// TestRenderNvidiaDevicePlugin_InlineTemplate verifies version substitution,
// nodeSelector, and toleration with an inline template.
func TestRenderNvidiaDevicePlugin_InlineTemplate(t *testing.T) {
	tmpl := []byte(`image: nvcr.io/nvidia/k8s-device-plugin:{{ .Version }}
nodeSelector:
  awsbnkctl.io/gpu: "true"
tolerations:
  - key: nvidia.com/gpu
    operator: Exists
    effect: NoSchedule
`)
	out, err := RenderNvidiaDevicePlugin(tmpl, "v0.17.1")
	if err != nil {
		t.Fatalf("RenderNvidiaDevicePlugin: %v", err)
	}
	rendered := string(out)

	checks := []string{
		"nvcr.io/nvidia/k8s-device-plugin:v0.17.1", // version substituted
		`awsbnkctl.io/gpu: "true"`,                 // GPU nodeSelector
		"nvidia.com/gpu",                           // taint toleration key
		"NoSchedule",                               // taint effect
	}
	for _, want := range checks {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered NVIDIA plugin missing %q:\n%s", want, rendered)
		}
	}
	// Must NOT contain un-rendered template tokens.
	if strings.Contains(rendered, "{{ .Version }}") {
		t.Errorf("rendered output still contains template token {{ .Version }}:\n%s", rendered)
	}
}

// TestRenderNvidiaDevicePlugin_EmbeddedTemplate verifies the real embedded
// template renders correctly with the pinned version.
func TestRenderNvidiaDevicePlugin_EmbeddedTemplate(t *testing.T) {
	tmplBytes, err := manifests.FS.ReadFile("nvidia-device-plugin/nvidia-device-plugin-v0.17.1.yaml.tmpl")
	if err != nil {
		t.Fatalf("read embedded NVIDIA device-plugin template: %v", err)
	}

	const version = "v0.17.1"
	out, err := RenderNvidiaDevicePlugin(tmplBytes, version)
	if err != nil {
		t.Fatalf("RenderNvidiaDevicePlugin (embedded): %v", err)
	}
	rendered := string(out)

	// Image tag is substituted.
	if !strings.Contains(rendered, "nvcr.io/nvidia/k8s-device-plugin:v0.17.1") {
		t.Errorf("image tag not substituted; want nvcr.io/nvidia/k8s-device-plugin:v0.17.1:\n%s", rendered)
	}
	// nodeSelector targets GPU nodes only.
	if !strings.Contains(rendered, `awsbnkctl.io/gpu: "true"`) {
		t.Errorf("missing nodeSelector awsbnkctl.io/gpu=true:\n%s", rendered)
	}
	// Taint toleration present.
	if !strings.Contains(rendered, "nvidia.com/gpu") {
		t.Errorf("missing nvidia.com/gpu toleration key:\n%s", rendered)
	}
	if !strings.Contains(rendered, "NoSchedule") {
		t.Errorf("missing NoSchedule toleration effect:\n%s", rendered)
	}
	// Namespace is kube-system.
	if !strings.Contains(rendered, "namespace: kube-system") {
		t.Errorf("missing namespace kube-system:\n%s", rendered)
	}
	// DaemonSet kind present.
	if !strings.Contains(rendered, "kind: DaemonSet") {
		t.Errorf("missing kind: DaemonSet:\n%s", rendered)
	}
	// No leftover template tokens.
	if strings.Contains(rendered, "{{") || strings.Contains(rendered, "}}") {
		t.Errorf("rendered output still contains template tokens:\n%s", rendered)
	}
}

// Without the EKS service range in state the CNEInstance carries no
// TMM_K8S_ROUTES entry: an empty value would make TMM parse "" as a route.
func TestRenderCNEInstance_OmitsK8sRoutesWithoutServiceCIDR(t *testing.T) {
	tmplBytes, err := manifests.FS.ReadFile("shared/cneinstance.yaml.tmpl")
	if err != nil {
		t.Fatalf("read cneinstance template: %v", err)
	}
	out, err := RenderCNEInstance(tmplBytes, cneInstanceCluster(), func(key string) string {
		if key == "VPC_ID" {
			return "vpc-0abc12345"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("RenderCNEInstance: %v", err)
	}
	if strings.Contains(string(out), TMMK8sRoutesEnv) {
		t.Errorf("rendered CNEInstance must not carry %s without %s in state:\n%s", TMMK8sRoutesEnv, ServiceCIDRStateKey, out)
	}
}

func TestRenderCNEInstance_AIGateway241(t *testing.T) {
	tmplBytes, err := manifests.FS.ReadFile("shared/cneinstance.yaml.tmpl")
	if err != nil {
		t.Fatalf("read cneinstance template: %v", err)
	}
	cl := cneInstanceCluster()
	cl.Bnk.ManifestVersion = "2.4.1-3.2850.1-0.0.9"
	out, err := RenderCNEInstance(tmplBytes, cl, func(key string) string {
		if key == "VPC_ID" {
			return "vpc-0abc12345"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("RenderCNEInstance 2.4.1: %v", err)
	}
	rendered := string(out)
	for _, want := range []string{
		"aiGateway:",
		"enabled: true",
		"aiMetrics: true",
		"loggingSubsystem:\n      enabled: true",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered shared CNEInstance 2.4.1 missing %q:\n%s", want, rendered)
		}
	}

	azOut, err := RenderAzureCNEInstance(CNEInstanceData{
		ManifestVersion: "2.4.1-3.2850.1-0.0.9",
		DeploymentSize:  "Small",
		TmmMtu:          1500,
	})
	if err != nil {
		t.Fatalf("RenderAzureCNEInstance 2.4.1: %v", err)
	}
	azRendered := string(azOut)
	for _, want := range []string{
		"aiGateway:",
		"enabled: true",
		"aiMetrics: true",
		"loggingSubsystem:\n      enabled: true",
	} {
		if !strings.Contains(azRendered, want) {
			t.Errorf("rendered Azure CNEInstance 2.4.1 missing %q:\n%s", want, azRendered)
		}
	}

	gkeTmpl := readTemplate(t, "cneinstance.yaml.tmpl")
	gkeOut, err := RenderGKECNEInstance(gkeTmpl, GKECNEInstanceVars{
		TmmMtu:          "1460",
		ManifestVersion: "v2.4.1-3.2850.1-0.0.9",
		ExtPCI:          "0000:00:05.0",
		IntPCI:          "0000:00:06.0",
	})
	if err != nil {
		t.Fatalf("RenderGKECNEInstance 2.4.1: %v", err)
	}
	gkeRendered := string(gkeOut)
	for _, want := range []string{
		"aiGateway:",
		"enabled: true",
		"aiMetrics: true",
		"loggingSubsystem:\n      enabled: true",
	} {
		if !strings.Contains(gkeRendered, want) {
			t.Errorf("rendered GKE CNEInstance 2.4.1 missing %q:\n%s", want, gkeRendered)
		}
	}

	for _, v := range []string{"2.4.1", "v2.4.1-3.2850.1-0.0.9", "2.4.3", "2.5.0", "3.0.0"} {
		if !isAIGatewayManifestVersion(v) {
			t.Errorf("expected isAIGatewayManifestVersion(%q) = true", v)
		}
	}
	for _, v := range []string{"", "2.3.0", "2.4.0", "2.4.0-3.2882.0", "v2.4.0"} {
		if isAIGatewayManifestVersion(v) {
			t.Errorf("expected isAIGatewayManifestVersion(%q) = false", v)
		}
	}
}
