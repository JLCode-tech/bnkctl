package bnkscan

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Generation is the BNK API generation a cluster serves.
type Generation string

const (
	// Gen23 serves gateway.k8s.f5net.com (BNKSecPolicy, BNKNetPolicy) only.
	Gen23 Generation = "2.3"
	// Gen24 serves gateway.k8s.f5.com (Infra, GatewaySettings, ...) only.
	Gen24 Generation = "2.4"
	// GenMixed serves both policy groups: a cluster mid-upgrade.
	GenMixed Generation = "mixed"
	// GenNone serves neither: BNK is not installed or its CRDs are missing.
	GenNone Generation = "none"
)

// API groups. This package deliberately declares its own table (rather than
// importing internal/k8s/migrate) so internal/forge can use it without an
// import cycle through the provisioning phases.
const (
	// GatewayF5Group is the 2.4 group for Infra, GatewaySettings,
	// EgressGateway, SecPolicy and NetPolicy.
	GatewayF5Group = "gateway.k8s.f5.com"
	// GatewayF5Version is the served version of every GatewayF5Group kind.
	GatewayF5Version = "v1alpha1"
	// LegacyPolicyGroup is the 2.3 policy group (BNKSecPolicy, BNKNetPolicy).
	LegacyPolicyGroup = "gateway.k8s.f5net.com"
	// GatewayAPIGroup is the upstream Gateway API group.
	GatewayAPIGroup = "gateway.networking.k8s.io"
	// DataPlaneGroup holds the 2.3-and-2.4 data-plane CRDs (iRules, firewall
	// policies, persistence profiles).
	DataPlaneGroup = "k8s.f5net.com"
	// FLOGroup holds the CNEInstance.
	FLOGroup = "k8s.f5.com"
	// InferenceF5Group is the BNK 2.4.1 Multi-Cluster AI Gateway CRD group.
	InferenceF5Group = "inference.k8s.f5.com"
	// InferenceF5Version is the served version of every InferenceF5Group kind.
	InferenceF5Version = "v1alpha1"

	// DefaultControllerNamespace is where awsbnkctl installs the CNE stack.
	DefaultControllerNamespace = "f5-cne-system"
	// DefaultControllerName is the f5-cne-controller Deployment.
	DefaultControllerName = "f5-cne-controller"
	// DefaultTMMName is the TMM DaemonSet FLO renders next to the controller.
	DefaultTMMName = "f5-tmm"

	// AnnotationProtocol marks an HTTPRoute as an MCP endpoint explicitly
	// (value "mcp"). Routes without it are recognised by path.
	AnnotationProtocol = "bnk.f5.com/protocol"
	// AnnotationAuth names the caller authentication an MCP route expects
	// ("bearer", "none", "oauth", ...). Recorded verbatim in the endpoint.
	AnnotationAuth = "bnk.f5.com/auth"
	// AnnotationTools lists tool names an MCP route serves, comma separated,
	// for clusters where the endpoint cannot be probed.
	AnnotationTools = "bnk.f5.com/mcp-tools"

	// PersistenceTypeMCP is the F5BigPersistenceProfile type that pins MCP
	// sessions.
	PersistenceTypeMCP = "MODEL_CONTEXT_PROTOCOL"
	// PersistenceTypeA2A pins Agent2Agent sessions.
	PersistenceTypeA2A = "AGENT2AGENT"
)

// GVRs this package lists.
var (
	CRDGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

	InfraGVR           = schema.GroupVersionResource{Group: GatewayF5Group, Version: GatewayF5Version, Resource: "infras"}
	GatewaySettingsGVR = schema.GroupVersionResource{Group: GatewayF5Group, Version: GatewayF5Version, Resource: "gatewaysettings"}
	EgressGatewayGVR   = schema.GroupVersionResource{Group: GatewayF5Group, Version: GatewayF5Version, Resource: "egressgateways"}
	SecPolicyGVR       = schema.GroupVersionResource{Group: GatewayF5Group, Version: GatewayF5Version, Resource: "secpolicies"}
	NetPolicyGVR       = schema.GroupVersionResource{Group: GatewayF5Group, Version: GatewayF5Version, Resource: "netpolicies"}
	AppPolicyGVR       = schema.GroupVersionResource{Group: GatewayF5Group, Version: GatewayF5Version, Resource: "apppolicies"}
	L4GlobalPolicyGVR  = schema.GroupVersionResource{Group: GatewayF5Group, Version: GatewayF5Version, Resource: "l4globalpolicies"}

	// BNK 2.4.1 Multi-Cluster AI Gateway CRDs (inference.k8s.f5.com/v1alpha1).
	AITenantGVR         = schema.GroupVersionResource{Group: InferenceF5Group, Version: InferenceF5Version, Resource: "aitenants"}
	AICredentialGVR     = schema.GroupVersionResource{Group: InferenceF5Group, Version: InferenceF5Version, Resource: "aicredentials"}
	AIProviderGVR       = schema.GroupVersionResource{Group: InferenceF5Group, Version: InferenceF5Version, Resource: "aiproviders"}
	AIModelGVR          = schema.GroupVersionResource{Group: InferenceF5Group, Version: InferenceF5Version, Resource: "aimodels"}
	AIGatewayProfileGVR = schema.GroupVersionResource{Group: InferenceF5Group, Version: InferenceF5Version, Resource: "aigatewayprofiles"}
	AIRouteGVR          = schema.GroupVersionResource{Group: InferenceF5Group, Version: InferenceF5Version, Resource: "airoutes"}
	AIRoutePolicyGVR    = schema.GroupVersionResource{Group: InferenceF5Group, Version: InferenceF5Version, Resource: "airoutepolicies"}
	AIGatewayGVR        = schema.GroupVersionResource{Group: InferenceF5Group, Version: InferenceF5Version, Resource: "aigateways"}

	GatewayGVR      = schema.GroupVersionResource{Group: GatewayAPIGroup, Version: "v1", Resource: "gateways"}
	GatewayClassGVR = schema.GroupVersionResource{Group: GatewayAPIGroup, Version: "v1", Resource: "gatewayclasses"}
	HTTPRouteGVR    = schema.GroupVersionResource{Group: GatewayAPIGroup, Version: "v1", Resource: "httproutes"}

	PersistenceProfileGVR = schema.GroupVersionResource{Group: DataPlaneGroup, Version: "v1", Resource: "f5-big-persistence-profiles"}
	IRuleGVR              = schema.GroupVersionResource{Group: DataPlaneGroup, Version: "v1", Resource: "f5-big-cne-irules"}
	CNEInstanceGVR        = schema.GroupVersionResource{Group: FLOGroup, Version: "v1", Resource: "cneinstances"}

	// F5 CNF (Cloud-Native Network Functions) kinds.
	DNSCacheGVR         = schema.GroupVersionResource{Group: DataPlaneGroup, Version: "v1", Resource: "f5-big-dns-caches"}
	DNSAppGVR           = schema.GroupVersionResource{Group: DataPlaneGroup, Version: "v1", Resource: "f5-big-dns-apps"}
	DNSZoneGVR          = schema.GroupVersionResource{Group: DataPlaneGroup, Version: "v1", Resource: "f5-big-dns-zones"}
	CGNATPolicyGVR      = schema.GroupVersionResource{Group: DataPlaneGroup, Version: "v1", Resource: "f5-big-cnat-policies"}
	DiameterEndpointGVR = schema.GroupVersionResource{Group: DataPlaneGroup, Version: "v1", Resource: "f5-diameter-endpoints"}

	// Legacy 2.3 kinds, counted only.
	BnkGatewayGVR = schema.GroupVersionResource{Group: DataPlaneGroup, Version: "v1", Resource: "f5-bnkgateways"}
	BNKSecPolGVR  = schema.GroupVersionResource{Group: LegacyPolicyGroup, Version: "v1alpha1", Resource: "bnksecpolicies"}
	BNKNetPolGVR  = schema.GroupVersionResource{Group: LegacyPolicyGroup, Version: "v1alpha1", Resource: "bnknetpolicies"}
	VlanGVR       = schema.GroupVersionResource{Group: DataPlaneGroup, Version: "v1", Resource: "f5-spk-vlans"}
	EgressGVR     = schema.GroupVersionResource{Group: DataPlaneGroup, Version: "v3", Resource: "f5-spk-egresses"}
)

// Condition is one entry of status.conditions.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Object is one indexed resource with its readiness verdict.
type Object struct {
	Kind       string      `json:"kind"`
	APIVersion string      `json:"apiVersion"`
	Namespace  string      `json:"namespace,omitempty"`
	Name       string      `json:"name"`
	Conditions []Condition `json:"conditions,omitempty"`
	// Ready is the kind-specific verdict (see doc.go). Kinds without a
	// readiness rule report Ready=true when the object exists.
	Ready bool `json:"ready"`
	// Detail explains a false Ready in one line.
	Detail string `json:"detail,omitempty"`
}

// ID returns namespace/name, or name for cluster-scoped objects.
func (o Object) ID() string {
	if o.Namespace == "" {
		return o.Name
	}
	return o.Namespace + "/" + o.Name
}

// Condition returns the condition of the given type, if present.
func (o Object) Condition(t string) (Condition, bool) {
	for _, c := range o.Conditions {
		if c.Type == t {
			return c, true
		}
	}
	return Condition{}, false
}

// Controller is the f5-cne-controller rollout state.
type Controller struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Found     bool   `json:"found"`
	Desired   int32  `json:"desired"`
	Available int32  `json:"available"`
	Ready     bool   `json:"ready"`
}

// TMM is the f5-tmm DaemonSet rollout: the data plane every Gateway programs.
type TMM struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Found     bool   `json:"found"`
	Desired   int32  `json:"desired"`
	Ready     int32  `json:"ready"`
	// Pending describes each TMM pod that is not Running, as
	// "<pod> <phase>: <latest Warning event>" (event omitted when none).
	Pending []string `json:"pending,omitempty"`
}

// Complete reports whether every scheduled TMM pod is ready.
func (t TMM) Complete() bool { return t.Found && t.Desired > 0 && t.Ready >= t.Desired }

// Legacy counts the 2.3 kinds still present.
type Legacy struct {
	BnkGateways   int `json:"f5BnkGateways"`
	BNKSecPolicy  int `json:"bnkSecPolicies"`
	BNKNetPolicy  int `json:"bnkNetPolicies"`
	F5SPKVlans    int `json:"f5SpkVlans"`
	F5SPKEgresses int `json:"f5SpkEgresses"`
}

// Any reports whether any legacy CR exists.
func (l Legacy) Any() bool { return l.count() > 0 }

func (l Legacy) count() int {
	return l.BnkGateways + l.BNKSecPolicy + l.BNKNetPolicy + l.F5SPKVlans + l.F5SPKEgresses
}

// PersistenceInfo describes the F5BigPersistenceProfile attached to a Gateway.
type PersistenceInfo struct {
	Profile    string `json:"profile"`
	Namespace  string `json:"namespace"`
	Type       string `json:"type"`
	Timeout    int64  `json:"timeoutSeconds,omitempty"`
	SecretName string `json:"secretName,omitempty"`
	Programmed bool   `json:"programmed"`
	// Listener is the Gateway listener the NetPolicy names, "" for the whole Gateway.
	Listener string `json:"listener,omitempty"`
}

// ToolDef is one entry of an MCP tools/list result.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// MCPEndpoint is an MCP tool server reachable through a BNK Gateway.
type MCPEndpoint struct {
	// Route identifies the HTTPRoute.
	Route     string `json:"route"`
	Namespace string `json:"namespace"`
	// Gateway is the parent BNK Gateway (namespace/name) and its state.
	Gateway          string `json:"gateway"`
	GatewayNamespace string `json:"gatewayNamespace"`
	GatewayReady     bool   `json:"gatewayReady"`
	// Addresses are the Gateway VIPs (spec.addresses, then status.addresses).
	Addresses []string `json:"addresses,omitempty"`
	// Listeners are "name:protocol:port" for every Gateway listener.
	Listeners []string `json:"listeners,omitempty"`
	Hostnames []string `json:"hostnames,omitempty"`
	// Paths are the route's path match values, the MCP ones first.
	Paths []string `json:"paths"`
	// URLs are scheme://address[:port]/path for every VIP, listener and MCP path.
	URLs []string `json:"urls,omitempty"`
	// Backends are namespace/service:port of the backendRefs.
	Backends []string `json:"backends,omitempty"`
	// Auth is the AnnotationAuth value, "bearer" when a governance iRule reads
	// Authorization, else "unknown".
	Auth string `json:"auth"`
	// IRules attached to the Gateway through NetPolicies.
	IRules []string `json:"irules,omitempty"`
	// Persistence is set when a NetPolicy attaches an F5BigPersistenceProfile.
	Persistence *PersistenceInfo `json:"persistence,omitempty"`
	// SecPolicies attached to the Gateway.
	SecPolicies []string `json:"secPolicies,omitempty"`
	// Tools is what AnnotationTools or ProbeTools reported.
	Tools []ToolDef `json:"tools,omitempty"`
	// Reason says why the route was classified as MCP.
	Reason string `json:"reason"`
}

// Name returns a stable identifier for the endpoint: mcp-<namespace>-<route>.
func (e MCPEndpoint) Name() string {
	return "mcp-" + e.Namespace + "-" + e.Route
}

// PrimaryURL returns the first URL, preferring HTTPS on 443.
func (e MCPEndpoint) PrimaryURL() string {
	for _, u := range e.URLs {
		if strings.HasPrefix(u, "https://") {
			return u
		}
	}
	if len(e.URLs) > 0 {
		return e.URLs[0]
	}
	return ""
}

// ToolNames returns the tool names, sorted.
func (e MCPEndpoint) ToolNames() []string {
	out := make([]string, 0, len(e.Tools))
	for _, t := range e.Tools {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

// Index is the result of Scan.
type Index struct {
	Generation Generation `json:"generation"`
	// Groups are the F5 CRD groups the API server serves.
	Groups []string `json:"crdGroups"`

	Infras              []Object `json:"infras"`
	GatewaySettings     []Object `json:"gatewaySettings"`
	EgressGateways      []Object `json:"egressGateways"`
	SecPolicies         []Object `json:"secPolicies"`
	NetPolicies         []Object `json:"netPolicies"`
	Gateways            []Object `json:"gateways"`
	HTTPRoutes          []Object `json:"httpRoutes"`
	PersistenceProfiles []Object `json:"persistenceProfiles"`

	// F5 CNF resources (Cloud-Native Network Functions)
	DNSCaches         []Object `json:"dnsCaches,omitempty"`
	DNSApps           []Object `json:"dnsApps,omitempty"`
	DNSZones          []Object `json:"dnsZones,omitempty"`
	CGNATPolicies     []Object `json:"cgnatPolicies,omitempty"`
	DiameterEndpoints []Object `json:"diameterEndpoints,omitempty"`

	// BNK 2.4.1 Multi-Cluster AI Gateway & Application Policies
	AppPolicies       []Object          `json:"appPolicies,omitempty"`
	L4GlobalPolicies  []Object          `json:"l4GlobalPolicies,omitempty"`
	AITenants         []Object          `json:"aiTenants,omitempty"`
	AICredentials     []Object          `json:"aiCredentials,omitempty"`
	AIProviders       []Object          `json:"aiProviders,omitempty"`
	AIModels          []Object          `json:"aiModels,omitempty"`
	AIGatewayProfiles []Object          `json:"aiGatewayProfiles,omitempty"`
	AIRoutes          []Object          `json:"aiRoutes,omitempty"`
	AIRoutePolicies   []Object          `json:"aiRoutePolicies,omitempty"`
	AIGateways        []Object          `json:"aiGateways,omitempty"`
	DNSResolverReady  bool              `json:"dnsResolverReady,omitempty"`
	AIGatewayPosture  *AIGatewayPosture `json:"aiGatewayPosture,omitempty"`

	Controller Controller `json:"controller"`
	TMM        TMM        `json:"tmm"`
	Legacy     Legacy     `json:"legacy"`
	// Missing lists the kinds whose CRD the API server does not serve.
	Missing []string `json:"missingCRDs,omitempty"`

	MCP []MCPEndpoint `json:"mcpEndpoints"`
}

// AIGatewayPosture captures BNK 2.4.1 AI Gateway runtime prerequisites and pod readiness.
type AIGatewayPosture struct {
	Enabled                bool   `json:"enabled"`
	AIMetricsEnabled       bool   `json:"aiMetricsEnabled,omitempty"`
	DefaultStorageClass    string `json:"defaultStorageClass,omitempty"`
	DSSMSentinelReady      bool   `json:"dssmSentinelReady,omitempty"`
	KubeDNSAbsent          bool   `json:"kubeDnsAbsent,omitempty"`
	AIGWDNSResolverPresent bool   `json:"aigwDnsResolverPresent,omitempty"`
	F5AIGatewayFound       bool   `json:"f5AiGatewayFound,omitempty"`
	F5AIGatewayReady       bool   `json:"f5AiGatewayReady,omitempty"`
	F5AIGatewayDesired     int32  `json:"f5AiGatewayDesired,omitempty"`
	F5AIGatewayAvailable   int32  `json:"f5AiGatewayAvailable,omitempty"`
	ClickHouseReady        bool   `json:"clickHouseReady,omitempty"`
	ClickHousePVCsBound    int    `json:"clickHousePvcsBound,omitempty"`
}

// Ready reports whether the cluster satisfies every 2.4 readiness check: the
// generation is 2.4 (or mixed), at least one Infra is Programmed, every
// Gateway is Accepted and Programmed, and the controller rollout is complete.
func (idx *Index) Ready() bool {
	return len(idx.Problems()) == 0
}

// Problems lists the readiness failures, one line each.
func (idx *Index) Problems() []string { return idx.problems(false) }

// problems is Problems with the AcceptLegacy relaxation (see Options).
func (idx *Index) problems(acceptLegacy bool) []string {
	var out []string
	switch idx.Generation {
	case GenNone:
		out = append(out, "no BNK policy CRD group served (gateway.k8s.f5.com missing)")
	case Gen23:
		if !acceptLegacy {
			out = append(out, "cluster serves the 2.3 API (gateway.k8s.f5net.com); run awsbnkctl bnk upgrade")
		}
	}
	if !idx.Controller.Found {
		out = append(out, fmt.Sprintf("deployment %s/%s not found", idx.Controller.Namespace, idx.Controller.Name))
	} else if !idx.Controller.Ready {
		out = append(out, fmt.Sprintf("deployment %s/%s: %d/%d replicas available",
			idx.Controller.Namespace, idx.Controller.Name, idx.Controller.Available, idx.Controller.Desired))
	}
	if idx.TMM.Found && !idx.TMM.Complete() {
		msg := fmt.Sprintf("daemonset %s/%s: %d/%d TMM pods ready", idx.TMM.Namespace, idx.TMM.Name, idx.TMM.Ready, idx.TMM.Desired)
		if len(idx.TMM.Pending) > 0 {
			msg += "; " + strings.Join(idx.TMM.Pending, "; ")
		}
		out = append(out, msg)
	}
	if idx.Generation == Gen24 || idx.Generation == GenMixed {
		if len(idx.Infras) == 0 {
			out = append(out, "no Infra CR (gateway.k8s.f5.com) found")
		}
		for _, o := range idx.Infras {
			if !o.Ready {
				out = append(out, fmt.Sprintf("Infra %s: %s", o.ID(), o.Detail))
			}
		}
	}
	for _, o := range idx.Gateways {
		if !o.Ready {
			out = append(out, fmt.Sprintf("Gateway %s: %s", o.ID(), o.Detail))
		}
	}
	for _, o := range idx.PersistenceProfiles {
		if !o.Ready {
			msg := fmt.Sprintf("F5BigPersistenceProfile %s: %s", o.ID(), o.Detail)
			if idx.Controller.Found && o.Namespace != "" && o.Namespace != idx.Controller.Namespace {
				msg += fmt.Sprintf("; the BNK 2.4.0 controller reconciles F5BigPersistenceProfile only in its own namespace (%s) and a NetPolicy resolves the profile in its own namespace, so MCP session persistence needs the Gateway, NetPolicy and profile in %s", idx.Controller.Namespace, idx.Controller.Namespace)
			}
			out = append(out, msg)
		}
	}
	for _, o := range idx.AIGatewayProfiles {
		if !o.Ready {
			out = append(out, fmt.Sprintf("AIGatewayProfile %s: %s", o.ID(), o.Detail))
		}
	}
	for _, o := range idx.AIGateways {
		if !o.Ready {
			out = append(out, fmt.Sprintf("AIGateway %s: %s", o.ID(), o.Detail))
		}
	}
	return out
}

// DetectGeneration classifies a cluster from the F5 CRD groups it serves.
func DetectGeneration(groups []string) Generation {
	has := func(g string) bool {
		for _, x := range groups {
			if x == g {
				return true
			}
		}
		return false
	}
	new24 := has(GatewayF5Group)
	old23 := has(LegacyPolicyGroup)
	switch {
	case new24 && old23:
		return GenMixed
	case new24:
		return Gen24
	case old23:
		return Gen23
	default:
		return GenNone
	}
}
