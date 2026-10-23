package bnkscan

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"
)

// A programmed BNK 2.4 cluster with one governed MCP route.
const fixture24 = `
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: infras.gateway.k8s.f5.com}
spec: {group: gateway.k8s.f5.com}
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: f5-big-cne-irules.k8s.f5net.com}
spec: {group: k8s.f5net.com}
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: cneinstances.k8s.f5.com}
spec: {group: k8s.f5.com}
---
apiVersion: gateway.k8s.f5.com/v1alpha1
kind: Infra
metadata: {name: infra, namespace: f5-cne-system}
spec: {}
status:
  conditions:
  - {type: Programmed, status: "True", reason: Programmed}
---
apiVersion: gateway.k8s.f5.com/v1alpha1
kind: GatewaySettings
metadata: {name: bnk-agentcore-demo, namespace: default}
spec: {}
---
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata: {name: demo-gatewayclass}
spec: {controllerName: f5.com/f5-cne-system-f5-cne-controller}
---
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata: {name: other}
spec: {controllerName: example.net/other}
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: bnk-agentcore-demo-gateway, namespace: default}
spec:
  gatewayClassName: demo-gatewayclass
  addresses: [{type: IPAddress, value: 10.0.10.150}]
  listeners:
  - {name: http, protocol: HTTP, port: 80}
  - {name: https, protocol: HTTPS, port: 443}
status:
  conditions:
  - {type: Accepted, status: "True"}
  - {type: Programmed, status: "True"}
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: not-ours, namespace: default}
spec:
  gatewayClassName: other
  listeners: [{name: http, protocol: HTTP, port: 80}]
status:
  conditions:
  - {type: Accepted, status: "False"}
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: mcp-financial-route
  namespace: default
  annotations: {bnk.f5.com/auth: bearer, bnk.f5.com/mcp-tools: "forecast, get_account_balance"}
spec:
  parentRefs: [{name: bnk-agentcore-demo-gateway}]
  hostnames: [bnk-ingress.bnk-demo.internal]
  rules:
  - matches: [{path: {type: PathPrefix, value: /v1/mcp/forecast}}]
    backendRefs: [{name: mcp-financial-tool, port: 80}]
status:
  parents:
  - conditions:
    - {type: Accepted, status: "True"}
    - {type: ResolvedRefs, status: "True"}
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: mcp-discovery-route, namespace: default}
spec:
  parentRefs: [{name: bnk-agentcore-demo-gateway}]
  rules:
  - matches: [{path: {type: Exact, value: /.well-known/agent-card.json}}]
    backendRefs: [{name: mcp-financial-tool, port: 80}]
---
apiVersion: k8s.f5net.com/v1
kind: F5BigPersistenceProfile
metadata: {name: mcp-session, namespace: default}
spec:
  persistenceType: MODEL_CONTEXT_PROTOCOL
  timeout: 600
  mcpEncryptionPassphrase: {secretRef: {name: mcp-session-passphrase}}
status:
  conditions:
  - {type: Programmed, status: "True"}
---
apiVersion: gateway.k8s.f5.com/v1alpha1
kind: NetPolicy
metadata: {name: mcp-net-policy-http, namespace: default}
spec:
  targetRefs:
  - {group: gateway.networking.k8s.io, kind: Gateway, name: bnk-agentcore-demo-gateway, sectionName: http}
  extensionRefs:
  - {group: k8s.f5net.com, kind: F5BigCneIrule, name: mcp-rate-limit-irule}
  - {group: k8s.f5net.com, kind: F5BigPersistenceProfile, name: mcp-session}
---
apiVersion: gateway.k8s.f5.com/v1alpha1
kind: SecPolicy
metadata: {name: mcp-sec-policy, namespace: default}
spec:
  targetRefs:
  - {group: gateway.networking.k8s.io, kind: Gateway, name: bnk-agentcore-demo-gateway}
  extensionRefs:
  - {group: k8s.f5net.com, kind: F5BigFwPolicy, name: mcp-firewall}
`

var testGVRs = map[string]schema.GroupVersionResource{
	"CustomResourceDefinition": CRDGVR,
	"Infra":                    InfraGVR,
	"GatewaySettings":          GatewaySettingsGVR,
	"EgressGateway":            EgressGatewayGVR,
	"SecPolicy":                SecPolicyGVR,
	"NetPolicy":                NetPolicyGVR,
	"F5BigPersistenceProfile":  PersistenceProfileGVR,
	"GatewayClass":             GatewayClassGVR,
	"Gateway":                  GatewayGVR,
	"HTTPRoute":                HTTPRouteGVR,
	"F5BnkGateway":             BnkGatewayGVR,
	"BNKSecPolicy":             BNKSecPolGVR,
	"BNKNetPolicy":             BNKNetPolGVR,
	"F5SPKVlan":                VlanGVR,
	"F5SPKEgress":              EgressGVR,
	"F5BigDnsCache":            DNSCacheGVR,
	"F5BigDnsApp":              DNSAppGVR,
	"F5BigDnsZone":             DNSZoneGVR,
	"F5BigCnatPolicy":          CGNATPolicyGVR,
	"F5DiameterEndpoint":       DiameterEndpointGVR,
	"AppPolicy":                AppPolicyGVR,
	"AITenant":                 AITenantGVR,
	"AICredential":             AICredentialGVR,
	"AIProvider":               AIProviderGVR,
	"AIModel":                  AIModelGVR,
	"AIGatewayProfile":         AIGatewayProfileGVR,
	"AIGateway":                AIGatewayGVR,
}

func newFakeDynamic(t *testing.T, fixture string) *dynamicfake.FakeDynamicClient {
	t.Helper()
	s := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{}
	for kind, gvr := range testGVRs {
		s.AddKnownTypeWithName(gvr.GroupVersion().WithKind(kind), &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvr.GroupVersion().WithKind(kind+"List"), &unstructured.UnstructuredList{})
		listKinds[gvr] = kind + "List"
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(s, listKinds)
	for _, doc := range strings.Split(fixture, "\n---\n") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var m map[string]any
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatal(err)
		}
		obj := &unstructured.Unstructured{Object: m}
		gvr, ok := testGVRs[obj.GetKind()]
		if !ok {
			t.Fatalf("fixture kind %s has no GVR", obj.GetKind())
		}
		ri := dyn.Resource(gvr)
		var err error
		if obj.GetNamespace() != "" {
			_, err = ri.Namespace(obj.GetNamespace()).Create(context.Background(), obj, metav1.CreateOptions{})
		} else {
			_, err = ri.Create(context.Background(), obj, metav1.CreateOptions{})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return dyn
}

func controllerDeployment(available int32) *appsv1.Deployment {
	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultControllerName, Namespace: DefaultControllerNamespace},
		Spec:       appsv1.DeploymentSpec{Replicas: &one},
		Status:     appsv1.DeploymentStatus{AvailableReplicas: available},
	}
}

func TestDetectGeneration(t *testing.T) {
	cases := []struct {
		groups []string
		want   Generation
	}{
		{nil, GenNone},
		{[]string{"k8s.f5net.com"}, GenNone},
		{[]string{"gateway.k8s.f5net.com", "k8s.f5net.com"}, Gen23},
		{[]string{"gateway.k8s.f5.com", "k8s.f5net.com", "k8s.f5.com"}, Gen24},
		{[]string{"gateway.k8s.f5.com", "gateway.k8s.f5net.com"}, GenMixed},
	}
	for _, c := range cases {
		if got := DetectGeneration(c.groups); got != c.want {
			t.Errorf("DetectGeneration(%v) = %s, want %s", c.groups, got, c.want)
		}
	}
}

func TestScan_Programmed24Cluster(t *testing.T) {
	dyn := newFakeDynamic(t, fixture24)
	cs := k8sfake.NewClientset(controllerDeployment(1))

	idx, err := Scan(context.Background(), dyn, cs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if idx.Generation != Gen24 {
		t.Errorf("generation = %s, want 2.4 (groups %v)", idx.Generation, idx.Groups)
	}
	if len(idx.Infras) != 1 || !idx.Infras[0].Ready {
		t.Errorf("Infra = %+v, want one Programmed", idx.Infras)
	}
	if len(idx.Gateways) != 1 {
		t.Fatalf("Gateways = %d, want the one BNK Gateway (non-BNK class filtered)", len(idx.Gateways))
	}
	if !idx.Gateways[0].Ready {
		t.Errorf("Gateway not ready: %s", idx.Gateways[0].Detail)
	}
	if !idx.Controller.Ready || idx.Controller.Available != 1 {
		t.Errorf("controller = %+v, want ready 1/1", idx.Controller)
	}
	if len(idx.PersistenceProfiles) != 1 || !idx.PersistenceProfiles[0].Ready {
		t.Errorf("persistence profiles = %+v", idx.PersistenceProfiles)
	}
	if !idx.Ready() {
		t.Errorf("Ready() = false: %v", idx.Problems())
	}

	if len(idx.MCP) != 1 {
		t.Fatalf("MCP endpoints = %d, want 1 (discovery route is not MCP): %+v", len(idx.MCP), idx.MCP)
	}
	ep := idx.MCP[0]
	if ep.Route != "mcp-financial-route" || ep.Gateway != "bnk-agentcore-demo-gateway" || !ep.GatewayReady {
		t.Errorf("endpoint = %+v", ep)
	}
	wantURLs := []string{"http://10.0.10.150/v1/mcp/forecast", "https://10.0.10.150/v1/mcp/forecast"}
	if strings.Join(ep.URLs, ",") != strings.Join(wantURLs, ",") {
		t.Errorf("URLs = %v, want %v", ep.URLs, wantURLs)
	}
	if ep.PrimaryURL() != wantURLs[1] {
		t.Errorf("PrimaryURL = %s", ep.PrimaryURL())
	}
	if ep.Auth != "bearer" {
		t.Errorf("Auth = %s, want bearer (annotation)", ep.Auth)
	}
	if strings.Join(ep.IRules, ",") != "mcp-rate-limit-irule" {
		t.Errorf("IRules = %v", ep.IRules)
	}
	if strings.Join(ep.SecPolicies, ",") != "mcp-sec-policy" {
		t.Errorf("SecPolicies = %v", ep.SecPolicies)
	}
	if ep.Persistence == nil || ep.Persistence.Type != PersistenceTypeMCP || ep.Persistence.Timeout != 600 ||
		ep.Persistence.SecretName != "mcp-session-passphrase" || !ep.Persistence.Programmed || ep.Persistence.Listener != "http" {
		t.Errorf("Persistence = %+v", ep.Persistence)
	}
	if strings.Join(ep.ToolNames(), ",") != "forecast,get_account_balance" {
		t.Errorf("tools = %v", ep.ToolNames())
	}
	if strings.Join(ep.Backends, ",") != "default/mcp-financial-tool:80" {
		t.Errorf("Backends = %v", ep.Backends)
	}
	if ep.Name() != "mcp-default-mcp-financial-route" {
		t.Errorf("Name = %s", ep.Name())
	}
	if !strings.HasPrefix(ep.Reason, "path ") {
		t.Errorf("Reason = %s", ep.Reason)
	}
}

func TestScan_NotReady(t *testing.T) {
	fixture := strings.Replace(fixture24,
		"  - {type: Programmed, status: \"True\"}\n---\napiVersion: gateway.networking.k8s.io/v1\nkind: Gateway\nmetadata: {name: not-ours",
		"  - {type: Programmed, status: \"False\", reason: Pending}\n---\napiVersion: gateway.networking.k8s.io/v1\nkind: Gateway\nmetadata: {name: not-ours", 1)
	if fixture == fixture24 {
		t.Fatal("fixture edit did not apply")
	}
	dyn := newFakeDynamic(t, fixture)
	cs := k8sfake.NewClientset(controllerDeployment(0))

	idx, err := Scan(context.Background(), dyn, cs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if idx.Ready() {
		t.Fatal("Ready() = true, want false")
	}
	problems := strings.Join(idx.Problems(), "\n")
	for _, want := range []string{"Gateway default/bnk-agentcore-demo-gateway: Programmed=False (Pending)", "f5-cne-controller: 0/1 replicas available"} {
		if !strings.Contains(problems, want) {
			t.Errorf("Problems() missing %q:\n%s", want, problems)
		}
	}
	if len(idx.MCP) != 1 || idx.MCP[0].GatewayReady {
		t.Errorf("MCP endpoint should be listed with GatewayReady=false: %+v", idx.MCP)
	}
}

func TestScan_MissingCRDAndNoController(t *testing.T) {
	dyn := newFakeDynamic(t, fixture24)
	dyn.PrependReactor("list", "infras", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: GatewayF5Group, Resource: "infras"}, "")
	})
	idx, err := Scan(context.Background(), dyn, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(idx.Missing, ",") != "Infra" {
		t.Errorf("Missing = %v", idx.Missing)
	}
	problems := strings.Join(idx.Problems(), "\n")
	for _, want := range []string{"no Infra CR", "not found"} {
		if !strings.Contains(problems, want) {
			t.Errorf("Problems() missing %q:\n%s", want, problems)
		}
	}
}

func TestScan_LegacyClusterIsReportedNotFailed(t *testing.T) {
	fixture := `
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: bnknetpolicies.gateway.k8s.f5net.com}
spec: {group: gateway.k8s.f5net.com}
---
apiVersion: k8s.f5net.com/v1
kind: F5BnkGateway
metadata: {name: web, namespace: apps}
spec: {}
---
apiVersion: gateway.k8s.f5net.com/v1alpha1
kind: BNKNetPolicy
metadata: {name: web, namespace: apps}
spec: {}
`
	dyn := newFakeDynamic(t, fixture)
	idx, err := Scan(context.Background(), dyn, k8sfake.NewClientset(controllerDeployment(1)), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if idx.Generation != Gen23 {
		t.Errorf("generation = %s, want 2.3", idx.Generation)
	}
	if idx.Legacy.BnkGateways != 1 || idx.Legacy.BNKNetPolicy != 1 {
		t.Errorf("legacy = %+v", idx.Legacy)
	}
	if problems := strings.Join(idx.Problems(), "\n"); !strings.Contains(problems, "2.3 API") {
		t.Errorf("Problems() = %q, want the 2.3 hint", problems)
	}
}

func TestEndpointURLs_NonDefaultPortsAndIPv6(t *testing.T) {
	gw := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"listeners": []any{
				map[string]any{"name": "alt", "protocol": "HTTP", "port": int64(8080)},
				map[string]any{"name": "tls", "protocol": "HTTPS", "port": int64(8443)},
				map[string]any{"name": "tcp", "protocol": "TCP", "port": int64(9000)},
			},
		},
	}}
	got := endpointURLs([]string{"10.0.10.150", "fd00::150"}, gw, "", []string{"/tools", "/v1/mcp"})
	want := []string{
		"http://10.0.10.150:8080/v1/mcp", "https://10.0.10.150:8443/v1/mcp",
		"http://[fd00::150]:8080/v1/mcp", "https://[fd00::150]:8443/v1/mcp",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v\nwant %v", got, want)
	}
	if got := endpointURLs([]string{"10.0.10.150"}, gw, "alt", []string{"/tools"}); strings.Join(got, ",") != "http://10.0.10.150:8080/tools" {
		t.Errorf("sectionName restriction: got %v", got)
	}
}

func TestReport_TextAndJSON(t *testing.T) {
	dyn := newFakeDynamic(t, fixture24)
	idx, err := Scan(context.Background(), dyn, k8sfake.NewClientset(controllerDeployment(1)), Options{})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	WriteReport(&text, idx)
	for _, want := range []string{"BNK API generation: 2.4", "ok   f5-cne-system/infra", "MCP endpoints (1)", "persistence: mcp-session (MODEL_CONTEXT_PROTOCOL, programmed, listener http)", "ready: every 2.4 readiness check passed"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("report missing %q:\n%s", want, text.String())
		}
	}
	var js strings.Builder
	if err := WriteJSON(&js, idx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(js.String(), `"generation": "2.4"`) || !strings.Contains(js.String(), `"mcpEndpoints"`) {
		t.Errorf("json = %s", js.String())
	}
}

func TestScan_CNFResources(t *testing.T) {
	fixtureCNF := fixture24 + `
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: f5-big-dns-caches.k8s.f5net.com}
spec: {group: k8s.f5net.com}
---
apiVersion: k8s.f5net.com/v1
kind: F5BigDnsCache
metadata: {name: edge-resolver, namespace: default}
spec: {}
status:
  conditions:
  - {type: Programmed, status: "True"}
`
	dyn := newFakeDynamic(t, fixtureCNF)
	idx, err := Scan(context.Background(), dyn, k8sfake.NewClientset(controllerDeployment(1)), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.DNSCaches) != 1 {
		t.Fatalf("DNSCaches count = %d, want 1", len(idx.DNSCaches))
	}
	cache := idx.DNSCaches[0]
	if cache.Name != "edge-resolver" || !cache.Ready {
		t.Errorf("DNSCache = %+v", cache)
	}
}

func TestScan_AIGateway241Chain(t *testing.T) {
	fixtureAI := fixture24 + `
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: apppolicies.gateway.k8s.f5.com}
spec: {group: gateway.k8s.f5.com}
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: aitenants.inference.k8s.f5.com}
spec: {group: inference.k8s.f5.com}
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: aicredentials.inference.k8s.f5.com}
spec: {group: inference.k8s.f5.com}
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: aiproviders.inference.k8s.f5.com}
spec: {group: inference.k8s.f5.com}
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: aimodels.inference.k8s.f5.com}
spec: {group: inference.k8s.f5.com}
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: aigatewayprofiles.inference.k8s.f5.com}
spec: {group: inference.k8s.f5.com}
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: aigateways.inference.k8s.f5.com}
spec: {group: inference.k8s.f5.com}
---
apiVersion: inference.k8s.f5.com/v1alpha1
kind: AITenant
metadata: {name: bedrock-tenant, namespace: default}
spec: {}
status:
  conditions:
  - {type: Programmed, status: "True"}
---
apiVersion: inference.k8s.f5.com/v1alpha1
kind: AICredential
metadata: {name: bedrock-cred, namespace: default}
spec: {}
status:
  conditions:
  - {type: Programmed, status: "True"}
---
apiVersion: inference.k8s.f5.com/v1alpha1
kind: AIProvider
metadata: {name: bedrock-provider, namespace: default}
spec: {}
status:
  conditions:
  - {type: Programmed, status: "True"}
---
apiVersion: inference.k8s.f5.com/v1alpha1
kind: AIModel
metadata: {name: claude-sonnet-4-5, namespace: default}
spec: {}
status:
  conditions:
  - {type: Programmed, status: "True"}
---
apiVersion: inference.k8s.f5.com/v1alpha1
kind: AIGatewayProfile
metadata: {name: bedrock-profile, namespace: default}
spec: {}
status:
  conditions:
  - {type: Programmed, status: "True"}
---
apiVersion: inference.k8s.f5.com/v1alpha1
kind: AIGateway
metadata: {name: bedrock-gw, namespace: default}
spec: {}
status:
  conditions:
  - {type: Programmed, status: "True"}
---
apiVersion: gateway.k8s.f5.com/v1alpha1
kind: AppPolicy
metadata: {name: bedrock-app-policy, namespace: default}
spec: {}
status:
  conditions:
  - {type: Programmed, status: "True"}
`
	dyn := newFakeDynamic(t, fixtureAI)
	idx, err := Scan(context.Background(), dyn, k8sfake.NewClientset(controllerDeployment(1)), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.AITenants) != 1 || len(idx.AICredentials) != 1 || len(idx.AIProviders) != 1 ||
		len(idx.AIModels) != 1 || len(idx.AIGatewayProfiles) != 1 || len(idx.AIGateways) != 1 || len(idx.AppPolicies) != 1 {
		t.Fatalf("unexpected AI Gateway 2.4.1 chain counts: tenants=%d creds=%d provs=%d models=%d profiles=%d gws=%d apppol=%d",
			len(idx.AITenants), len(idx.AICredentials), len(idx.AIProviders),
			len(idx.AIModels), len(idx.AIGatewayProfiles), len(idx.AIGateways), len(idx.AppPolicies))
	}
	if !idx.Ready() {
		t.Errorf("expected Ready()=true, got problems: %v", idx.Problems())
	}
	var text strings.Builder
	WriteReport(&text, idx)
	for _, want := range []string{
		"AppPolicy (1)",
		"AITenant (1)",
		"AICredential (1)",
		"AIProvider (1)",
		"AIModel (1)",
		"AIGatewayProfile (1)",
		"ok   default/bedrock-profile",
		"AIGateway (1)",
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("report missing %q:\n%s", want, text.String())
		}
	}

	// Edge case: AIGatewayProfile with Programmed=False must fail Ready() and surface in Problems()
	unprogFixture := strings.Replace(fixtureAI,
		"kind: AIGatewayProfile\nmetadata: {name: bedrock-profile, namespace: default}\nspec: {}\nstatus:\n  conditions:\n  - {type: Programmed, status: \"True\"}",
		"kind: AIGatewayProfile\nmetadata: {name: bedrock-profile, namespace: default}\nspec: {}\nstatus:\n  conditions:\n  - {type: Programmed, status: \"False\", reason: NotPrepared}",
		1,
	)
	dynUnprog := newFakeDynamic(t, unprogFixture)
	idxUnprog, err := Scan(context.Background(), dynUnprog, k8sfake.NewClientset(controllerDeployment(1)), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if idxUnprog.Ready() {
		t.Errorf("expected Ready()=false when AIGatewayProfile is Programmed=False")
	}
	if !strings.Contains(strings.Join(idxUnprog.Problems(), "\n"), "AIGatewayProfile default/bedrock-profile") {
		t.Errorf("expected AIGatewayProfile problem in Problems(), got %v", idxUnprog.Problems())
	}

	// Verify BNK 2.4.1 f5-aigateway-console and f5-aigateway-clickhouse in f5-cne-system are detected
	one := int32(1)
	cs241 := k8sfake.NewClientset(
		controllerDeployment(1),
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "f5-aigateway-console", Namespace: DefaultControllerNamespace},
			Spec:       appsv1.DeploymentSpec{Replicas: &one},
			Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
		},
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "f5-aigateway-clickhouse", Namespace: DefaultControllerNamespace},
			Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1},
		},
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "data-f5-aigateway-clickhouse-0", Namespace: DefaultControllerNamespace},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
		},
	)
	idx241, err := Scan(context.Background(), dyn, cs241, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if idx241.AIGatewayPosture == nil {
		t.Fatalf("expected AIGatewayPosture to be non-nil")
	}
	if !idx241.AIGatewayPosture.F5AIGatewayFound || !idx241.AIGatewayPosture.F5AIGatewayReady {
		t.Errorf("expected F5AIGatewayFound and F5AIGatewayReady for f5-aigateway-console in %s, got %+v", DefaultControllerNamespace, idx241.AIGatewayPosture)
	}
	if !idx241.AIGatewayPosture.ClickHouseReady || idx241.AIGatewayPosture.ClickHousePVCsBound != 1 {
		t.Errorf("expected ClickHouseReady=true and ClickHousePVCsBound=1 for f5-aigateway-clickhouse in %s, got %+v", DefaultControllerNamespace, idx241.AIGatewayPosture)
	}

	// Verify BNK 2.4.1 cluster with inference CRDs installed (before CR instances exist) sets Enabled=true
	fixtureCRDOnly := fixture24 + `
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: aigatewayprofiles.inference.k8s.f5.com}
spec: {group: inference.k8s.f5.com}
`
	idxCRDOnly, err := Scan(context.Background(), newFakeDynamic(t, fixtureCRDOnly), k8sfake.NewClientset(controllerDeployment(1)), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if idxCRDOnly.AIGatewayPosture == nil || !idxCRDOnly.AIGatewayPosture.Enabled {
		t.Errorf("expected AIGatewayPosture.Enabled=true when aigatewayprofiles.inference.k8s.f5.com CRD is served without instances, got %+v", idxCRDOnly.AIGatewayPosture)
	}
}
