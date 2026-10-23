package migrate

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/yaml"
)

// A BNK 2.3.3 dual-interface cluster as awsbnkctl v1.6.1 built it, plus the
// egress-demo and agentcore examples: two VLANs, two static routes, an
// egress with a firewall policy and a SNAT pool, two tenant Gateways (one
// with an F5BnkGateway pool, one static) and the 2.3 policies.
const fixture23 = `
apiVersion: k8s.f5.com/v1
kind: CNEInstance
metadata: {name: lab-bnk, namespace: f5-cne-system}
spec:
  manifestVersion: "2.3.3-3.2598.3-0.0.509"
  networkAttachments: [external-netdevice, internal-netdevice]
  advanced:
    cneController:
      env:
      - {name: TMM_DEFAULT_MTU, value: "9000"}
      - {name: CLOUD_PROVIDER, value: aws}
    tmm:
      env:
      - {name: TMM_CALICO_ROUTER, value: default}
---
apiVersion: k8s.f5net.com/v1
kind: F5SPKVlan
metadata: {name: ext-vlan, namespace: f5-cne-system}
spec:
  name: ext-vlan
  interfaces: ["1.1"]
  selfip_v4s: ["10.0.10.240"]
  prefixlen_v4: 24
  tag: 0
  auto_lasthop: AUTO_LASTHOP_ENABLED
  allowed_services:
  - {protocol: tcp, port: "179"}
---
apiVersion: k8s.f5net.com/v1
kind: F5SPKVlan
metadata: {name: int-vlan, namespace: f5-cne-system}
spec:
  name: int-vlan
  internal: true
  interfaces: ["1.2"]
  selfip_v4s: ["10.0.20.240"]
  prefixlen_v4: 24
  tag: 0
  mtu: 9000
---
apiVersion: k8s.f5net.com/v1
kind: F5SPKStaticRoute
metadata: {name: bnk-egress-demo-vpc, namespace: f5-cne-system}
spec: {destination: 10.0.0.0, prefixLen: 16, gateway: 10.0.20.1, type: gateway}
---
apiVersion: k8s.f5net.com/v1
kind: F5SPKStaticRoute
metadata: {name: bnk-egress-demo-default, namespace: f5-cne-system}
spec: {destination: 0.0.0.0, prefixLen: 0, gateway: 10.0.10.1, type: gateway}
---
apiVersion: k8s.f5net.com/v1
kind: F5SPKSnatpool
metadata: {name: egress-snat, namespace: f5-cne-system}
spec:
  addressList:
  - ["10.0.10.60", "10.0.10.61"]
---
apiVersion: k8s.f5net.com/v3
kind: F5SPKEgress
metadata: {name: bnk-egress-demo, namespace: f5-cne-system}
spec:
  snatType: SRC_TRANS_SNATPOOL
  egressSnatpool: egress-snat
  firewallEnforcedPolicy: egress-demo-fw
  pseudoCNIConfig:
    namespaces: [bnk-egress-demo]
    appPodInterface: eth0
    vxlan:
      create: true
      tmmInterfaceName: int-vlan
      nodeInterfaceName: ens6
      ipv4Subnet: "192.168.0.0"
      ipv4PrefixLen: 16
      ipv6Subnet: "fd50::"
      ipv6PrefixLen: 112
---
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata: {name: lab-gatewayclass}
spec: {controllerName: f5.com/f5-cne-system-f5-cne-controller}
---
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata: {name: nginx}
spec: {controllerName: gateway.nginx.org/nginx-gateway-controller}
---
apiVersion: k8s.f5net.com/v1
kind: F5BnkGateway
metadata: {name: http2-vip-pool, namespace: http2-scenario}
spec:
  ingressConfig:
    defaultListenerNetworks:
    - name: ext
      ipv4BaseCidr: "10.0.10.0/24"
      startAddress: 10.0.10.100
      endAddress: 10.0.10.110
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: http2-gateway, namespace: http2-scenario}
spec:
  gatewayClassName: lab-gatewayclass
  addresses: [{type: IPAddress, value: 10.0.10.102}]
  listeners: [{name: http, protocol: HTTP, port: 80}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: bnk-agentcore-demo-gateway, namespace: default}
spec:
  gatewayClassName: lab-gatewayclass
  addresses: [{type: IPAddress, value: 10.0.10.121}]
  listeners: [{name: http, protocol: HTTP, port: 80}, {name: https, protocol: HTTPS, port: 443}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: other, namespace: default}
spec:
  gatewayClassName: nginx
  listeners: [{name: http, protocol: HTTP, port: 80}]
---
apiVersion: gateway.k8s.f5net.com/v1alpha1
kind: BNKNetPolicy
metadata:
  name: mcp-net-policy-http
  namespace: default
  labels: {app: agentcore}
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: '{"drop":"me"}'
    note: keep
spec:
  targetRefs:
  - {group: gateway.networking.k8s.io, kind: Gateway, name: bnk-agentcore-demo-gateway, sectionName: http}
  extensionRefs:
  - {group: k8s.f5net.com, kind: F5BigCneIrule, name: mcp-rate-limit-irule}
status: {ancestors: []}
---
apiVersion: gateway.k8s.f5net.com/v1alpha1
kind: BNKSecPolicy
metadata: {name: mcp-sec-policy, namespace: default}
spec:
  targetRefs:
  - {group: gateway.networking.k8s.io, kind: Gateway, name: bnk-agentcore-demo-gateway}
  extensionRefs:
  - {group: k8s.f5net.com, kind: F5BigFwPolicy, name: mcp-firewall}
`

// parseFixture splits a YAML stream into unstructured objects.
func parseFixture(t *testing.T, stream string) []*unstructured.Unstructured {
	t.Helper()
	var out []*unstructured.Unstructured
	for _, doc := range strings.Split(stream, "\n---\n") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var m map[string]any
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatalf("fixture: %v\n%s", err, doc)
		}
		out = append(out, &unstructured.Unstructured{Object: m})
	}
	return out
}

// gvrByKind maps fixture kinds to the GVRs Inspect lists.
var gvrByKind = map[string]schema.GroupVersionResource{
	"CNEInstance":              CNEInstanceGVR,
	"CNEController":            CNEControllerGVR,
	"F5SPKVlan":                VlanGVR,
	"F5SPKStaticRoute":         StaticRouteGVR,
	"Vrf":                      VrfGVR,
	"Vxlan":                    VxlanGVR,
	"F5SPKEgress":              EgressGVR,
	"F5SPKSnatpool":            SnatpoolGVR,
	"F5BnkGateway":             BnkGatewayGVR,
	"BNKSecPolicy":             BNKSecPolGVR,
	"BNKNetPolicy":             BNKNetPolGVR,
	"Gateway":                  GatewayGVR,
	"GatewayClass":             GatewayClassGVR,
	"CustomResourceDefinition": {Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"},
	"Infra":                    InfraGVR,
	"GatewaySettings":          GatewaySettingsGVR,
	"EgressGateway":            EgressGatewayGVR,
	"SecPolicy":                SecPolicyGVR,
	"NetPolicy":                NetPolicyGVR,
}

// inventoryFromFixture builds an Inventory directly (no client).
func inventoryFromFixture(t *testing.T, stream string) *Inventory {
	t.Helper()
	inv := &Inventory{Namespace: DefaultInstanceNamespace}
	for _, o := range parseFixture(t, stream) {
		switch o.GetKind() {
		case "CNEInstance":
			inv.CNEInstance = o
		case "F5SPKVlan":
			inv.Vlans = append(inv.Vlans, o)
		case "F5SPKStaticRoute":
			inv.StaticRoutes = append(inv.StaticRoutes, o)
		case "Vrf":
			inv.Vrfs = append(inv.Vrfs, o)
		case "Vxlan":
			inv.Vxlans = append(inv.Vxlans, o)
		case "F5SPKEgress":
			inv.Egresses = append(inv.Egresses, o)
		case "F5SPKSnatpool":
			inv.Snatpools = append(inv.Snatpools, o)
		case "F5BnkGateway":
			inv.BnkGateways = append(inv.BnkGateways, o)
		case "BNKSecPolicy":
			inv.SecPolicies = append(inv.SecPolicies, o)
		case "BNKNetPolicy":
			inv.NetPolicies = append(inv.NetPolicies, o)
		case "Gateway":
			inv.Gateways = append(inv.Gateways, o)
		case "GatewayClass":
			inv.GatewayClasses = append(inv.GatewayClasses, o)
		default:
			t.Fatalf("fixture kind %s not mapped", o.GetKind())
		}
	}
	return inv
}

// fakeScheme registers every kind the tests list through the fake client.
func fakeScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	for kind, gvr := range gvrByKind {
		gv := gvr.GroupVersion()
		s.AddKnownTypeWithName(gv.WithKind(kind), &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gv.WithKind(kind+"List"), &unstructured.UnstructuredList{})
	}
	return s
}

// newFakeDynamic returns a fake dynamic client holding objs.
func newFakeDynamic(t *testing.T, objs ...*unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	t.Helper()
	listKinds := map[schema.GroupVersionResource]string{}
	for kind, gvr := range gvrByKind {
		listKinds[gvr] = kind + "List"
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(fakeScheme(), listKinds)
	ctx := context.Background()
	for _, o := range objs {
		gvr, ok := gvrByKind[o.GetKind()]
		if !ok {
			t.Fatalf("no GVR for kind %s", o.GetKind())
		}
		var err error
		if o.GetNamespace() != "" {
			_, err = dyn.Resource(gvr).Namespace(o.GetNamespace()).Create(ctx, o, metav1.CreateOptions{})
		} else {
			_, err = dyn.Resource(gvr).Create(ctx, o, metav1.CreateOptions{})
		}
		if err != nil {
			t.Fatalf("seed %s %s: %v", o.GetKind(), o.GetName(), err)
		}
	}
	return dyn
}

// find returns the object of kind with ns/name from a slice, or nil.
func find(objs []*unstructured.Unstructured, kind, ns, name string) *unstructured.Unstructured {
	for _, o := range objs {
		if o.GetKind() == kind && o.GetNamespace() == ns && o.GetName() == name {
			return o
		}
	}
	return nil
}

// hasWarning reports whether any warning contains substr.
func hasWarning(plan *Plan, substr string) bool {
	for _, w := range plan.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}
