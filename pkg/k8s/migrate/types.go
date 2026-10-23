package migrate

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// API groups and versions of the resources this package reads and writes.
const (
	// LegacyGroup is the 2.3 F5 group that 2.4 still serves (F5SPKVlan,
	// F5SPKStaticRoute, Vrf, Vxlan, F5SPKEgress, F5SPKSnatpool, F5BnkGateway).
	LegacyGroup = "k8s.f5net.com"
	// LegacyPolicyGroup is the 2.3 policy group; 2.4 removed it.
	LegacyPolicyGroup = "gateway.k8s.f5net.com"
	// GatewayF5Group is the 2.4 group for Infra, GatewaySettings,
	// EgressGateway, SecPolicy and NetPolicy.
	GatewayF5Group = "gateway.k8s.f5.com"
	// GatewayF5Version is the served version of every GatewayF5Group kind.
	GatewayF5Version = "v1alpha1"
	// GatewayAPIGroup is the upstream Gateway API group.
	GatewayAPIGroup = "gateway.networking.k8s.io"

	// DefaultInstanceNamespace is where the CNEInstance and the underlay CRs
	// live on an awsbnkctl cluster.
	DefaultInstanceNamespace = "f5-cne-system"
	// DefaultInfraName is the name of the generated Infra CR (one per
	// CNE namespace).
	DefaultInfraName = "infra"
	// FieldManager owns the server-side-applied fields.
	FieldManager = "awsbnkctl-migrate-2.4"
)

// GVRs of the legacy (2.3) resources.
var (
	VlanGVR        = schema.GroupVersionResource{Group: LegacyGroup, Version: "v1", Resource: "f5-spk-vlans"}
	StaticRouteGVR = schema.GroupVersionResource{Group: LegacyGroup, Version: "v1", Resource: "f5-spk-staticroutes"}
	VrfGVR         = schema.GroupVersionResource{Group: LegacyGroup, Version: "v1", Resource: "vrfs"}
	VxlanGVR       = schema.GroupVersionResource{Group: LegacyGroup, Version: "v1", Resource: "vxlans"}
	EgressGVR      = schema.GroupVersionResource{Group: LegacyGroup, Version: "v3", Resource: "f5-spk-egresses"}
	SnatpoolGVR    = schema.GroupVersionResource{Group: LegacyGroup, Version: "v1", Resource: "f5-spk-snatpools"}
	BnkGatewayGVR  = schema.GroupVersionResource{Group: LegacyGroup, Version: "v1", Resource: "f5-bnkgateways"}
	BNKSecPolGVR   = schema.GroupVersionResource{Group: LegacyPolicyGroup, Version: "v1alpha1", Resource: "bnksecpolicies"}
	BNKNetPolGVR   = schema.GroupVersionResource{Group: LegacyPolicyGroup, Version: "v1alpha1", Resource: "bnknetpolicies"}
)

// GVRs of the resources shared by 2.3 and 2.4.
var (
	CNEInstanceGVR = schema.GroupVersionResource{Group: "k8s.f5.com", Version: "v1", Resource: "cneinstances"}
	// CNEControllerGVR is the FLO-owned component CR that renders the
	// f5-cne-controller Deployment.
	CNEControllerGVR = schema.GroupVersionResource{Group: "k8s.f5.com", Version: "v1", Resource: "cnecontrollers"}
	GatewayGVR       = schema.GroupVersionResource{Group: GatewayAPIGroup, Version: "v1", Resource: "gateways"}
	GatewayClassGVR  = schema.GroupVersionResource{Group: GatewayAPIGroup, Version: "v1", Resource: "gatewayclasses"}
)

// GVRs of the 2.4 resources this package writes.
var (
	InfraGVR           = schema.GroupVersionResource{Group: GatewayF5Group, Version: GatewayF5Version, Resource: "infras"}
	GatewaySettingsGVR = schema.GroupVersionResource{Group: GatewayF5Group, Version: GatewayF5Version, Resource: "gatewaysettings"}
	EgressGatewayGVR   = schema.GroupVersionResource{Group: GatewayF5Group, Version: GatewayF5Version, Resource: "egressgateways"}
	SecPolicyGVR       = schema.GroupVersionResource{Group: GatewayF5Group, Version: GatewayF5Version, Resource: "secpolicies"}
	NetPolicyGVR       = schema.GroupVersionResource{Group: GatewayF5Group, Version: GatewayF5Version, Resource: "netpolicies"}
)

// Inventory is what Inspect found on a 2.3.x cluster. Every slice holds the
// live objects as returned by the API server (status included); Translate
// reads only spec and metadata.
type Inventory struct {
	// Namespace is the CNE namespace the underlay CRs were listed from.
	Namespace string
	// CNEInstance is the instance CR in Namespace (nil when absent).
	CNEInstance *unstructured.Unstructured

	Vlans        []*unstructured.Unstructured // F5SPKVlan
	StaticRoutes []*unstructured.Unstructured // F5SPKStaticRoute
	Vrfs         []*unstructured.Unstructured // Vrf
	Vxlans       []*unstructured.Unstructured // Vxlan
	Egresses     []*unstructured.Unstructured // F5SPKEgress
	Snatpools    []*unstructured.Unstructured // F5SPKSnatpool
	BnkGateways  []*unstructured.Unstructured // F5BnkGateway, all namespaces
	SecPolicies  []*unstructured.Unstructured // BNKSecPolicy, all namespaces
	NetPolicies  []*unstructured.Unstructured // BNKNetPolicy, all namespaces

	// Gateways are every Gateway API Gateway in the cluster; Translate keeps
	// the ones whose gatewayClassName belongs to BNK.
	Gateways []*unstructured.Unstructured
	// GatewayClasses are the cluster's GatewayClasses (to find the BNK one).
	GatewayClasses []*unstructured.Unstructured

	// Missing lists the legacy resources whose CRD the API server does not
	// serve (kind names). Not an error: a cluster without egress has no
	// F5SPKEgress CRD.
	Missing []string
}

// Plan is the 2.4 configuration Translate derived from an Inventory, in
// apply order.
type Plan struct {
	// Infra is the single Infra CR (nil when the inventory had no VLAN).
	Infra *unstructured.Unstructured
	// GatewaySettings holds one CR per tenant namespace, sorted by namespace.
	GatewaySettings []*unstructured.Unstructured
	// GatewayPatches are partial Gateway objects (metadata + spec.infrastructure)
	// meant for server-side apply, one per BNK Gateway.
	GatewayPatches []*unstructured.Unstructured
	// EgressGateways holds one CR per (F5SPKEgress, tenant namespace).
	EgressGateways []*unstructured.Unstructured
	// Policies holds the SecPolicy and NetPolicy CRs: converted BNKSecPolicy /
	// BNKNetPolicy plus the SecPolicy that binds a 2.3 firewallEnforcedPolicy
	// to its EgressGateway.
	Policies []*unstructured.Unstructured
	// Warnings are facts the operator should read before applying: values
	// the 2.4 model has no field for, defaults the plan assumed, and legacy
	// CRs left in place.
	Warnings []string
}

// Objects returns every object in the plan in apply order.
func (p *Plan) Objects() []*unstructured.Unstructured {
	var out []*unstructured.Unstructured
	if p.Infra != nil {
		out = append(out, p.Infra)
	}
	out = append(out, p.GatewaySettings...)
	out = append(out, p.GatewayPatches...)
	out = append(out, p.EgressGateways...)
	out = append(out, p.Policies...)
	return out
}

// Empty reports whether the plan changes nothing.
func (p *Plan) Empty() bool {
	return len(p.Objects()) == 0
}
