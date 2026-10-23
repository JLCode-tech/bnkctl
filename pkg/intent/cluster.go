// Package intent holds the unified cluster.yaml schema (bnkctl/v1, awsbnkctl/v1,
// azrbnkctl/v1, gkebnkctl/v1) and loader across AWS, Azure, and GCP.
package intent

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/JLCode-tech/bnkctl/pkg/bnk"
	"github.com/JLCode-tech/bnkctl/pkg/netcalc"
)

var (
	clusterNameRE   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,38}[a-z0-9]$`)
	nodeGroupNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$|^[a-z0-9]$`)
	instanceTypeRE  = regexp.MustCompile(`^[a-z][0-9a-z]+\.[a-z0-9]+$`)
	vmSizeRE        = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
)

const (
	APIVersion    = "bnkctl/v1"
	APIVersionAWS = "awsbnkctl/v1"
	APIVersionAZR = "azrbnkctl/v1"
	APIVersionGKE = "gkebnkctl/v1"
	Kind          = "Cluster"

	ProviderAWS   = "aws"
	ProviderAzure = "azure"
	ProviderGCP   = "gcp"

	PatternExternalOnly  = "external-only"
	PatternDualInterface = "dual-interface"
	PatternSRIOVExternal = "sriov-external"
	PatternHostDevice    = "host-device"

	MinKubernetesVersion     = "1.32"
	MinKubernetesVersionAWS  = "1.34"
	DefaultKubernetesVersion = "1.35"
	MaxTestedKubernetesMinor = 35

	EmbeddedCertManagerVersion = "1.21.1"
	DefaultManifestVersion     = bnk.DefaultManifestVersion
	DefaultDeploymentSize      = "small"
	DefaultTmmCpu              = "2"
	DefaultTmmMemory           = "8Gi"
	DefaultTmmHugepages        = "4Gi"
	DefaultPalCpuSet           = "0,2"

	DefaultAzureLocation         = "eastus"
	DefaultAzureTmmCpu           = "4"
	DefaultAzureTmmMemory        = "10Gi"
	DefaultAzureTmmMtu           = 1500
	DefaultSystemVMSize          = "Standard_D4s_v5"
	DefaultTMMVMSize             = "Standard_D8s_v5"
	DefaultNetworkPlugin         = "azure"
	DefaultNetworkPluginMode     = "Overlay"
	DefaultVNetCIDR              = "10.0.0.0/16"
	DefaultSystemSubnetCIDR      = "10.0.0.0/20"
	DefaultPublicSubnetCIDR      = "10.0.16.0/24"
	DefaultExternalSubnetCIDR    = "10.0.17.0/24"
	DefaultInternalSubnetCIDR    = "10.0.18.0/24"
	DefaultGCPImageRegistry      = "us-west1-docker.pkg.dev/f5-gcs-7056-ptg-cne-prod/spk-repo-non-ga"
	DefaultLBControllerVersion   = "1.8.1"
	DefaultDemoTTL               = "24h"
	DemoTagKey                   = "awsbnkctl:demo"
	DemoExpiryTagKey             = "awsbnkctl:demo-expiry"
	DefaultVIPHostOffset         = 100
	DefaultJumphostExtHostOffset = 200
	DefaultTMMSelfIPHostOffset   = 240
	SelfIPPoolPrefix             = 27
	selfIPPoolMask               = byte(1<<(32-SelfIPPoolPrefix) - 1)
)

// DefaultFLOVersion is the FLO chart paired with DefaultManifestVersion.
var DefaultFLOVersion = bnk.DefaultFLOChart()

// MtuValue accepts either an integer (9000, 1500) or a string ("1460") in YAML.
type MtuValue int

// UnmarshalYAML decodes an int or numeric string into MtuValue.
func (m *MtuValue) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("tmmMtu must be an integer or string scalar, got kind %v", value.Kind)
	}
	s := strings.TrimSpace(value.Value)
	if s == "" {
		*m = 0
		return nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("invalid tmmMtu %q: %w", value.Value, err)
	}
	*m = MtuValue(v)
	return nil
}

// MarshalYAML encodes MtuValue as an integer.
func (m MtuValue) MarshalYAML() (any, error) {
	return int(m), nil
}

// Int returns the integer MTU value.
func (m MtuValue) Int() int {
	return int(m)
}

// String returns the string representation of the MTU value (empty if 0).
func (m MtuValue) String() string {
	if m == 0 {
		return ""
	}
	return strconv.Itoa(int(m))
}

// NatGatewaysValue accepts either an integer (e.g. 1, 2 for AWS) or a boolean (true/false for GCP/Azure).
type NatGatewaysValue struct {
	Count   int
	Enabled *bool
}

// UnmarshalYAML decodes either a bool or an int from YAML.
func (n *NatGatewaysValue) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("natGateways must be a boolean or integer scalar")
	}
	s := strings.TrimSpace(strings.ToLower(value.Value))
	if s == "true" || s == "false" {
		b := s == "true"
		n.Enabled = &b
		if b {
			n.Count = 1
		} else {
			n.Count = 0
		}
		return nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("invalid natGateways %q: expected int or bool", value.Value)
	}
	n.Count = v
	b := v > 0
	n.Enabled = &b
	return nil
}

// CloudSpec explicitly selects the target cloud provider in unified bnkctl/v1 manifests.
type CloudSpec struct {
	Provider string `yaml:"provider"` // aws | azure | gcp
}

// Metadata carries cluster identity and cloud location/account parameters.
type Metadata struct {
	Name           string            `yaml:"name"`
	Region         string            `yaml:"region,omitempty"`
	Profile        string            `yaml:"profile,omitempty"`
	Location       string            `yaml:"location,omitempty"`
	SubscriptionID string            `yaml:"subscriptionId,omitempty"`
	ResourceGroup  string            `yaml:"resourceGroup,omitempty"`
	Project        string            `yaml:"project,omitempty"`
	Zone           string            `yaml:"zone,omitempty"`
	Labels         map[string]string `yaml:"labels,omitempty"`
	Tags           map[string]string `yaml:"tags,omitempty"`
}

// SubnetSpec is one CIDR + AZ pair (AWS).
type SubnetSpec struct {
	CIDR string `yaml:"cidr"`
	AZ   string `yaml:"az"`
}

// SubnetsSpec supports both AWS list-based public/private subnets and Azure string-based system/public/external/internal subnets.
type SubnetsSpec struct {
	// AWS subnet lists
	PublicList  []SubnetSpec `yaml:"-"`
	PrivateList []SubnetSpec `yaml:"-"`

	// Azure subnet CIDRs
	System   string `yaml:"-"`
	Public   string `yaml:"-"`
	External string `yaml:"-"`
	Internal string `yaml:"-"`
}

// Subnets is an alias for SubnetsSpec.
type Subnets = SubnetsSpec

// UnmarshalYAML supports both AWS ({public: [{cidr, az}], private: [{cidr, az}]})
// and Azure ({system: cidr, public: cidr, external: cidr, internal: cidr}) subnet layouts.
func (s *SubnetsSpec) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("network.subnets must be a mapping")
	}
	for i := 0; i < len(value.Content); i += 2 {
		keyNode := value.Content[i]
		valNode := value.Content[i+1]
		switch keyNode.Value {
		case "public":
			switch valNode.Kind {
			case yaml.SequenceNode:
				var list []SubnetSpec
				if err := valNode.Decode(&list); err != nil {
					return fmt.Errorf("network.subnets.public: %w", err)
				}
				s.PublicList = list
				if len(list) > 0 {
					s.Public = list[0].CIDR
				}
			case yaml.ScalarNode:
				s.Public = valNode.Value
				if valNode.Value != "" {
					s.PublicList = []SubnetSpec{{CIDR: valNode.Value}}
				}
			default:
				return fmt.Errorf("network.subnets.public must be a sequence or string")
			}
		case "private":
			var list []SubnetSpec
			if err := valNode.Decode(&list); err != nil {
				return fmt.Errorf("network.subnets.private: %w", err)
			}
			s.PrivateList = list
		case "system":
			if err := valNode.Decode(&s.System); err != nil {
				return fmt.Errorf("network.subnets.system: %w", err)
			}
		case "external":
			if err := valNode.Decode(&s.External); err != nil {
				return fmt.Errorf("network.subnets.external: %w", err)
			}
		case "internal":
			if err := valNode.Decode(&s.Internal); err != nil {
				return fmt.Errorf("network.subnets.internal: %w", err)
			}
		default:
			return fmt.Errorf("yaml: unmarshal errors:\n  line %d: field %s not found in type intent.SubnetsSpec", keyNode.Line, keyNode.Value)
		}
	}
	return nil
}

// SelfIPsSpec carries nominal TMM SelfIP addresses.
type SelfIPsSpec struct {
	External  string `yaml:"external,omitempty"`
	Internal  string `yaml:"internal,omitempty"`
	PrefixLen int    `yaml:"prefixLen,omitempty"`
}

// DataPathSpec describes the TMM data-plane subnets (AWS).
type DataPathSpec struct {
	External SubnetSpec   `yaml:"external"`
	Internal SubnetSpec   `yaml:"internal,omitempty"`
	SelfIPs  *SelfIPsSpec `yaml:"selfIPs,omitempty"`
}

// MainNetworkSpec is the cluster-owned VPC (GCP).
type MainNetworkSpec struct {
	VPC           string `yaml:"vpc"`
	Subnet        string `yaml:"subnet"`
	CIDR          string `yaml:"cidr"`
	PodsRangeName string `yaml:"podsRangeName"`
	PodsCIDR      string `yaml:"podsCidr"`
	SvcsRangeName string `yaml:"svcsRangeName"`
	SvcsCIDR      string `yaml:"svcsCidr"`
}

// DataplaneSpec is a shared dataplane VPC (GCP).
type DataplaneSpec struct {
	VPC           string `yaml:"vpc"`
	Subnet        string `yaml:"subnet"`
	CIDR          string `yaml:"cidr"`
	MTU           int    `yaml:"mtu"`
	PodsRangeName string `yaml:"podsRangeName"`
	PodsCIDR      string `yaml:"podsCidr"`
	UnderlayCIDR  string `yaml:"underlayCidr,omitempty"`
}

// Network describes the cloud network topology across AWS, Azure, and GCP.
type Network struct {
	// AWS fields
	VPCCidr  string        `yaml:"vpcCidr,omitempty"`
	AZs      []string      `yaml:"azs,omitempty"`
	DataPath *DataPathSpec `yaml:"dataPath,omitempty"`

	// Azure fields
	VNetCIDR   string `yaml:"vnetCidr,omitempty"`
	NATGateway *bool  `yaml:"natGateway,omitempty"`

	// Shared AWS/Azure Subnets
	Subnets SubnetsSpec `yaml:"subnets,omitempty"`

	// Shared AWS (int) / GCP (*bool) natGateways
	NatGatewaysRaw NatGatewaysValue `yaml:"natGateways,omitempty"`

	// GCP fields
	Main              MainNetworkSpec `yaml:"main,omitempty"`
	External          DataplaneSpec   `yaml:"external,omitempty"`
	Internal          DataplaneSpec   `yaml:"internal,omitempty"`
	AllowInternalCidr string          `yaml:"allowInternalCidr,omitempty"`
}

// NatGatewaysCount returns the integer NAT gateway count (AWS).
func (n Network) NatGatewaysCount() int {
	return n.NatGatewaysRaw.Count
}

// NatGatewaysEnabled reports whether Cloud Router+NAT / NAT Gateway is enabled (default true when omitted).
func (n Network) NatGatewaysEnabled() bool {
	if n.NATGateway != nil {
		return *n.NATGateway
	}
	if n.NatGatewaysRaw.Enabled != nil {
		return *n.NatGatewaysRaw.Enabled
	}
	return true
}

// EndpointAccessSpec controls who can reach the EKS control-plane API endpoint.
type EndpointAccessSpec struct {
	PublicAccessCidrs []string `yaml:"publicAccessCidrs,omitempty"`
}

// NodeTaintSpec is one Kubernetes node taint applied to a managed node group.
type NodeTaintSpec struct {
	Key    string `yaml:"key"`
	Value  string `yaml:"value,omitempty"`
	Effect string `yaml:"effect"` // NoSchedule | PreferNoSchedule | NoExecute
}

// NodeTaint is an alias for NodeTaintSpec.
type NodeTaint = NodeTaintSpec

// NodeGroupSpec configures one AWS EKS managed node group.
type NodeGroupSpec struct {
	Name             string            `yaml:"name"`
	InstanceType     string            `yaml:"instanceType,omitempty"`
	AMIType          string            `yaml:"amiType,omitempty"`
	DesiredSize      int               `yaml:"desiredSize,omitempty"`
	MinSize          int               `yaml:"minSize,omitempty"`
	MaxSize          int               `yaml:"maxSize,omitempty"`
	DiskSize         int               `yaml:"diskSize,omitempty"`
	DiskSizeGiB      int               `yaml:"diskSizeGiB,omitempty"`
	Labels           map[string]string `yaml:"labels,omitempty"`
	GPU              bool              `yaml:"gpu,omitempty"`
	CapacityType     string            `yaml:"capacityType,omitempty"`
	Taints           []NodeTaintSpec   `yaml:"taints,omitempty"`
	SingleAZ         bool              `yaml:"singleAz,omitempty"`
	AZ               string            `yaml:"az,omitempty"`
	AZs              []string          `yaml:"azs,omitempty"`
	OnDemandFallback bool              `yaml:"onDemandFallback,omitempty"`
}

// NodeGroup is an alias for NodeGroupSpec.
type NodeGroup = NodeGroupSpec

// IsGPU reports whether this node group is an NVIDIA GPU inference node group.
func (ng NodeGroupSpec) IsGPU() bool { return ng.GPU }

// NeedsHugepagesLaunchTemplate reports whether this node group hosts TMM pods and needs 2Mi hugepages.
func (ng NodeGroupSpec) NeedsHugepagesLaunchTemplate() bool {
	if ng.IsGPU() {
		return false
	}
	if ng.Labels != nil && ng.Labels["role"] == "bnk" {
		return true
	}
	return true
}

// Hugepages2MiCount returns the number of 2Mi hugepages to allocate via sysctl on BNK worker nodes (4Gi = 2048).
func (ng NodeGroupSpec) Hugepages2MiCount() int {
	return 2048
}

// NodePoolSpec configures an AKS Agent Pool or GKE Node Pool.
type NodePoolSpec struct {
	Name                  string `yaml:"name,omitempty"`
	VMSize                string `yaml:"vmSize,omitempty"`
	Count                 int    `yaml:"count,omitempty"`
	OSDiskSizeGB          int    `yaml:"osDiskSizeGB,omitempty"`
	AcceleratedNetworking *bool  `yaml:"acceleratedNetworking,omitempty"`
	MachineType           string `yaml:"machineType,omitempty"`
	NodeCount             int    `yaml:"nodeCount,omitempty"`
	MaxPodsPerNode        int    `yaml:"maxPodsPerNode,omitempty"`
}

// ClusterSpec holds the managed Kubernetes cluster and node pool/group configuration.
type ClusterSpec struct {
	KubernetesVersion string `yaml:"kubernetesVersion,omitempty"`

	// AWS EKS fields
	NodeGroups     []NodeGroupSpec     `yaml:"nodeGroups,omitempty"`
	EndpointAccess *EndpointAccessSpec `yaml:"endpointAccess,omitempty"`

	// Azure AKS fields
	NetworkPlugin     string       `yaml:"networkPlugin,omitempty"`
	NetworkPluginMode string       `yaml:"networkPluginMode,omitempty"`
	SystemNodePool    NodePoolSpec `yaml:"systemNodePool,omitempty"`
	TMMNodePool       NodePoolSpec `yaml:"tmmNodePool,omitempty"`

	// GCP GKE fields
	DefaultPool NodePoolSpec `yaml:"defaultPool,omitempty"`
	TmmPool     NodePoolSpec `yaml:"tmmPool,omitempty"`
	AgentPool   NodePoolSpec `yaml:"agentPool,omitempty"`
}

// FarSecretSpec configures the far-secret pull-credential source (GCP/unified).
type FarSecretSpec struct {
	Source           string   `yaml:"source,omitempty"`
	SourceContext    string   `yaml:"sourceContext,omitempty"`
	SourceNamespace  string   `yaml:"sourceNamespace,omitempty"`
	DockerConfigFile string   `yaml:"dockerConfigFile,omitempty"`
	Namespaces       []string `yaml:"namespaces,omitempty"`
}

// BnkAIGatewaySpec configures the BNK 2.4.1+ Multi-Cluster AI Gateway (inference.k8s.f5.com/v1alpha1).
type BnkAIGatewaySpec struct {
	Enabled            bool   `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	AIMetrics          bool   `yaml:"aiMetrics,omitempty" json:"aiMetrics,omitempty"`
	ConsoleURL         string `yaml:"consoleUrl,omitempty" json:"consoleUrl,omitempty"`
	DefaultEgressRoute bool   `yaml:"defaultEgressRoute,omitempty" json:"defaultEgressRoute,omitempty"`
	DNSResolver        bool   `yaml:"dnsResolver,omitempty" json:"dnsResolver,omitempty"`
	DSSMSentinel       bool   `yaml:"dssmSentinel,omitempty" json:"dssmSentinel,omitempty"`
	MultiNUMASpanning  bool   `yaml:"multiNumaSpanning,omitempty" json:"multiNumaSpanning,omitempty"`
}

// BnkSpec holds BNK supply-chain credentials, sizing, and pass-through overrides.
type BnkSpec struct {
	FARArchive         string            `yaml:"farArchive,omitempty"`
	JWT                string            `yaml:"jwt,omitempty"`
	JwtPath            string            `yaml:"jwtPath,omitempty"`
	FarSecret          FarSecretSpec     `yaml:"farSecret,omitempty"`
	ImageRegistry      string            `yaml:"imageRegistry,omitempty"`
	CertManagerVersion string            `yaml:"certManagerVersion,omitempty"`
	DeploymentSize     string            `yaml:"deploymentSize,omitempty"`
	StorageClassName   string            `yaml:"storageClassName,omitempty"`
	ManifestVersion    string            `yaml:"manifestVersion,omitempty"`
	Version            string            `yaml:"version,omitempty"`
	FLOVersion         string            `yaml:"floVersion,omitempty"`
	TmmReplicas        int               `yaml:"tmmReplicas,omitempty"`
	TmmMtu             MtuValue          `yaml:"tmmMtu,omitempty"`
	TmmCpu             string            `yaml:"tmmCpu,omitempty"`
	TmmMemory          string            `yaml:"tmmMemory,omitempty"`
	TmmHugepages       string            `yaml:"tmmHugepages,omitempty"`
	PalCpuSet          string            `yaml:"palCpuSet,omitempty"`
	BGP                bool              `yaml:"bgp,omitempty"`
	BGPASN             int               `yaml:"bgpAsn,omitempty"`
	DynamicRouting     bool              `yaml:"dynamicRouting,omitempty"`
	Telemetry          bool              `yaml:"telemetry,omitempty"`
	AIGateway          *BnkAIGatewaySpec `yaml:"aiGateway,omitempty" json:"aiGateway,omitempty"`

	// Pass-through override escape hatches for BNK 2.4 / 2.5+ extensibility
	ControllerEnv  map[string]string `yaml:"controllerEnv,omitempty"`
	TmmEnv         map[string]string `yaml:"tmmEnv,omitempty"`
	FLOValues      map[string]any    `yaml:"floValues,omitempty"`
	ExtraManifests []string          `yaml:"extraManifests,omitempty"`
}

// TmmMtuInt returns the TMM MTU as an int.
func (b BnkSpec) TmmMtuInt() int {
	return b.TmmMtu.Int()
}

// TmmMtuString returns the TMM MTU as a string.
func (b BnkSpec) TmmMtuString() string {
	return b.TmmMtu.String()
}

// NormalizedDeploymentSize returns the lowercase deployment size (e.g. "small", "medium", "large").
func (b BnkSpec) NormalizedDeploymentSize() string {
	if b.DeploymentSize == "" {
		return DefaultDeploymentSize
	}
	return strings.ToLower(strings.TrimSpace(b.DeploymentSize))
}

// EffectiveJWTPath returns whichever of JWT or JwtPath is populated.
func (b BnkSpec) EffectiveJWTPath() string {
	if b.JWT != "" {
		return b.JWT
	}
	return b.JwtPath
}

// EgressSpec defines egress/NAT and VXLAN overlay settings.
type EgressSpec struct {
	// Azure egress fields
	Enabled          bool   `yaml:"enabled,omitempty"`
	Namespace        string `yaml:"namespace,omitempty"`
	Port             int    `yaml:"port,omitempty"`
	GatewayClassName string `yaml:"gatewayClassName,omitempty"`

	// GCP egress fields
	TestNamespace string `yaml:"testNamespace,omitempty"`
	VxlanPort     int    `yaml:"vxlanPort,omitempty"`
	VxlanKey      int    `yaml:"vxlanKey,omitempty"`
	VxlanMtu      int    `yaml:"vxlanMtu,omitempty"`
}

// JumphostSpec configures the optional test jumphost VM.
type JumphostSpec struct {
	Enabled         bool   `yaml:"enabled,omitempty"`
	InstanceType    string `yaml:"instanceType,omitempty"`
	MachineType     string `yaml:"machineType,omitempty"`
	MgmtSubnetIndex int    `yaml:"mgmtSubnetIndex,omitempty"`
}

// TestingSpec configures optional test infrastructure.
type TestingSpec struct {
	Enabled    bool          `yaml:"enabled,omitempty"`
	TargetHost string        `yaml:"targetHost,omitempty"`
	Jumphost   *JumphostSpec `yaml:"jumphost,omitempty"`
}

// DemoSpec declares that this cluster is a demo deployment with a TTL.
type DemoSpec struct {
	Enabled bool   `yaml:"enabled"`
	TTL     string `yaml:"ttl,omitempty"`
}

// BigIPVESpec declares an F5 BIG-IP VE appliance for live traffic migration demos.
type BigIPVESpec struct {
	Enabled         bool   `yaml:"enabled"`
	AMIID           string `yaml:"amiId,omitempty"`
	InstanceType    string `yaml:"instanceType,omitempty"`
	MgmtSubnetIndex int    `yaml:"mgmtSubnetIndex,omitempty"`
	VIP             string `yaml:"vip,omitempty"`
	Tier1VIP        string `yaml:"tier1Vip,omitempty"`
	LicenseTier     string `yaml:"licenseTier,omitempty"`
	Version         string `yaml:"version,omitempty"`
	AdminPassword   string `yaml:"adminPassword,omitempty"`
	LocalASN        int    `yaml:"localAsn,omitempty"`
	PeerASN         int    `yaml:"peerAsn,omitempty"`
}

// FloSpec configures the F5 Lifecycle Operator Helm installation.
type FloSpec struct {
	Version string `yaml:"version,omitempty"`
	Enabled *bool  `yaml:"enabled,omitempty"`
}

// FloEnabled returns true when FLO should be installed (default true).
func (f *FloSpec) FloEnabled() bool {
	if f == nil || f.Enabled == nil {
		return true
	}
	return *f.Enabled
}

// FLOVersion returns the explicit override or DefaultFLOVersion.
func (f *FloSpec) FLOVersion() string {
	if f == nil || f.Version == "" {
		return DefaultFLOVersion
	}
	return f.Version
}

// LBControllerSpec configures the AWS Load Balancer Controller installation.
type LBControllerSpec struct {
	EnabledFlag *bool  `yaml:"enabled,omitempty"`
	Version     string `yaml:"version,omitempty"`
}

// Enabled returns true when the AWS Load Balancer Controller should be installed (default false).
func (l *LBControllerSpec) Enabled() bool {
	if l == nil || l.EnabledFlag == nil {
		return false
	}
	return *l.EnabledFlag
}

// LBControllerVersion returns the chart version to install.
func (l *LBControllerSpec) LBControllerVersion() string {
	if l == nil || l.Version == "" {
		return DefaultLBControllerVersion
	}
	return l.Version
}

// AddonsSpec holds optional add-on configuration.
type AddonsSpec struct {
	Flo          *FloSpec          `yaml:"flo,omitempty"`
	LBController *LBControllerSpec `yaml:"lbController,omitempty"`
}

// DefaultForgeRESTURL is the fallback REST base when forge.url is not set.
const DefaultForgeRESTURL = "http://localhost:8000"

// ForgeSpec captures the operator-declared forge integration.
type ForgeSpec struct {
	Enabled              bool   `yaml:"enabled"`
	URL                  string `yaml:"url,omitempty"`
	MCPURL               string `yaml:"mcpUrl,omitempty"`
	Username             string `yaml:"username,omitempty"`
	Password             string `yaml:"password,omitempty"`
	CredentialTemplateID int    `yaml:"credentialTemplateId,omitempty"`
	Environment          string `yaml:"environment,omitempty"`
	ProjectType          string `yaml:"projectType,omitempty"`
	CloudProvider        string `yaml:"cloudProvider,omitempty"`
	ProjectName          string `yaml:"projectName,omitempty"`
	AgentToken           string `yaml:"agent_token,omitempty"`
	AgentTokenCamel      string `yaml:"agentToken,omitempty"`
}

// IsEnabled reports whether forge integration is enabled.
func (f *ForgeSpec) IsEnabled() bool {
	return f != nil && f.Enabled
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// ResolveURL returns the forge REST URL to use.
func (f *ForgeSpec) ResolveURL() string {
	if v := firstEnv("BNKCTL_FORGE_URL", "FORGE_URL", "AWSBNKCTL_FORGE_URL", "AZRBNKCTL_FORGE_URL", "GKEBNKCTL_FORGE_URL"); v != "" {
		return v
	}
	if f != nil && f.URL != "" {
		return f.URL
	}
	return DefaultForgeRESTURL
}

// EffectiveURL is an alias for ResolveURL.
func (f *ForgeSpec) EffectiveURL() string {
	return f.ResolveURL()
}

// ResolveUsername returns the forge REST login username.
func (f *ForgeSpec) ResolveUsername() string {
	if v := firstEnv("BNKCTL_FORGE_USERNAME", "FORGE_USERNAME", "AWSBNKCTL_FORGE_USERNAME", "AZRBNKCTL_FORGE_USERNAME", "GKEBNKCTL_FORGE_USERNAME"); v != "" {
		return v
	}
	if f != nil && f.Username != "" {
		return f.Username
	}
	return "admin"
}

// ResolvePassword returns the forge REST login password and whether the built-in default was used.
func (f *ForgeSpec) ResolvePassword() (password string, usingDefault bool) {
	if v := firstEnv("BNKCTL_FORGE_PASSWORD", "FORGE_PASSWORD", "AWSBNKCTL_FORGE_PASSWORD", "AZRBNKCTL_FORGE_PASSWORD", "GKEBNKCTL_FORGE_PASSWORD"); v != "" {
		return v, false
	}
	if f != nil && f.Password != "" {
		return f.Password, false
	}
	return "changeme", true
}

// ResolveAgentToken returns the benchmark agent JWT token.
func (f *ForgeSpec) ResolveAgentToken() string {
	if v := firstEnv("BNKCTL_FORGE_AGENT_TOKEN", "FORGE_AGENT_TOKEN", "AWSBNKCTL_FORGE_AGENT_TOKEN", "AZRBNKCTL_FORGE_AGENT_TOKEN", "GKEBNKCTL_FORGE_AGENT_TOKEN"); v != "" {
		return v
	}
	if f != nil {
		if f.AgentToken != "" {
			return f.AgentToken
		}
		if f.AgentTokenCamel != "" {
			return f.AgentTokenCamel
		}
	}
	return ""
}

// ResolveEnvironment returns the forge project environment.
func (f *ForgeSpec) ResolveEnvironment() string {
	if v := firstEnv("BNKCTL_FORGE_ENVIRONMENT", "FORGE_ENVIRONMENT", "AWSBNKCTL_FORGE_ENVIRONMENT", "AZRBNKCTL_FORGE_ENVIRONMENT", "GKEBNKCTL_FORGE_ENVIRONMENT"); v != "" {
		return v
	}
	if f != nil && f.Environment != "" {
		return f.Environment
	}
	return "dev"
}

// ResolveProjectType returns the forge project type.
func (f *ForgeSpec) ResolveProjectType() string {
	if f != nil && f.ProjectType != "" {
		return f.ProjectType
	}
	return "cloud-aws"
}

// ResolveCloudProvider returns the forge cloud provider identifier.
func (f *ForgeSpec) ResolveCloudProvider() string {
	if f != nil && f.CloudProvider != "" {
		return f.CloudProvider
	}
	return "aws"
}

// ResolveProjectName returns the forge project name.
func (f *ForgeSpec) ResolveProjectName(workspaceName string) string {
	if v := firstEnv("BNKCTL_FORGE_PROJECT", "FORGE_PROJECT", "AWSBNKCTL_FORGE_PROJECT", "AZRBNKCTL_FORGE_PROJECT", "GKEBNKCTL_FORGE_PROJECT"); v != "" {
		return v
	}
	if f != nil && f.ProjectName != "" {
		return f.ProjectName
	}
	if workspaceName == "" {
		return "bnkctl"
	}
	return "bnkctl-" + workspaceName
}

// Derived holds computed names and IP endpoints derived from cluster.yaml (Azure + shared).
type Derived struct {
	ResourceGroupName      string `yaml:"-"`
	VNetName               string `yaml:"-"`
	AKSClusterName         string `yaml:"-"`
	NSGName                string `yaml:"-"`
	SystemSubnetName       string `yaml:"-"`
	PublicSubnetName       string `yaml:"-"`
	ExternalSubnetName     string `yaml:"-"`
	InternalSubnetName     string `yaml:"-"`
	NatGatewayName         string `yaml:"-"`
	NatGatewayPublicIPName string `yaml:"-"`
	PublicIPName           string `yaml:"-"`
	ExternalGatewayIP      string `yaml:"-"`
	InternalGatewayIP      string `yaml:"-"`
	FARArchiveFullPath     string `yaml:"-"`
	JWTFullPath            string `yaml:"-"`
}

// Cluster is the unified Go representation of cluster.yaml across AWS, Azure, and GCP.
type Cluster struct {
	APIVersion  string            `yaml:"apiVersion"`
	Kind        string            `yaml:"kind"`
	Cloud       *CloudSpec        `yaml:"cloud,omitempty"`
	Metadata    Metadata          `yaml:"metadata"`
	Pattern     string            `yaml:"pattern,omitempty"`
	Network     Network           `yaml:"network"`
	ClusterSpec *ClusterSpec      `yaml:"cluster,omitempty"`
	Bnk         *BnkSpec          `yaml:"bnk,omitempty"`
	Addons      *AddonsSpec       `yaml:"addons,omitempty"`
	Egress      EgressSpec        `yaml:"egress,omitempty"`
	Testing     TestingSpec       `yaml:"testing,omitempty"`
	Demo        *DemoSpec         `yaml:"demo,omitempty"`
	BigIPVE     *BigIPVESpec      `yaml:"bigipVE,omitempty"`
	AI          *AISpec           `yaml:"ai,omitempty"`
	Forge       *ForgeSpec        `yaml:"forge,omitempty"`
	Tags        map[string]string `yaml:"tags,omitempty"`

	// Derived fields (excluded from YAML)
	Derived              Derived `yaml:"-"`
	Region               string  `yaml:"-"`
	ExtGwIP              string  `yaml:"-"`
	IntGwIP              string  `yaml:"-"`
	ExtUnderlayDest      string  `yaml:"-"`
	ExtUnderlayPrefixLen int     `yaml:"-"`
	MainPodsDest         string  `yaml:"-"`
	MainPodsPrefixLen    int     `yaml:"-"`
	MainSvcsDest         string  `yaml:"-"`
	MainSvcsPrefixLen    int     `yaml:"-"`
	SourcePath           string  `yaml:"-"`
}

// EffectiveProvider resolves the target cloud provider ("aws", "azure", or "gcp").
func (c *Cluster) EffectiveProvider() string {
	if c == nil {
		return ""
	}
	if c.Cloud != nil && c.Cloud.Provider != "" {
		return strings.ToLower(strings.TrimSpace(c.Cloud.Provider))
	}
	switch c.APIVersion {
	case APIVersionAWS:
		return ProviderAWS
	case APIVersionAZR:
		return ProviderAzure
	case APIVersionGKE:
		return ProviderGCP
	}
	if c.Metadata.Project != "" || c.Metadata.Zone != "" || c.Network.Main.VPC != "" {
		return ProviderGCP
	}
	if c.Metadata.SubscriptionID != "" || c.Metadata.ResourceGroup != "" || c.Metadata.Location != "" || c.Network.VNetCIDR != "" {
		return ProviderAzure
	}
	if c.Metadata.Region != "" || c.Network.VPCCidr != "" {
		return ProviderAWS
	}
	return ProviderAWS
}

// EffectiveName returns the cluster metadata name.
func (c *Cluster) EffectiveName() string {
	if c == nil {
		return ""
	}
	return c.Metadata.Name
}

// EffectiveRegion returns the cloud region or location across AWS, Azure, and GCP.
func (c *Cluster) EffectiveRegion() string {
	if c == nil {
		return ""
	}
	if c.Metadata.Region != "" {
		return c.Metadata.Region
	}
	if c.Metadata.Location != "" {
		return c.Metadata.Location
	}
	if c.Region != "" {
		return c.Region
	}
	if c.Metadata.Zone != "" {
		return deriveRegion(c.Metadata.Zone)
	}
	return ""
}

// EffectiveBNKVersion returns the configured BNK manifest version or DefaultManifestVersion.
func (c *Cluster) EffectiveBNKVersion() string {
	if c != nil && c.Bnk != nil && c.Bnk.ManifestVersion != "" {
		return c.Bnk.ManifestVersion
	}
	return DefaultManifestVersion
}

// FLOVersion resolves the FLO chart version to install.
func (c *Cluster) FLOVersion() string {
	if c != nil && c.Addons != nil && c.Addons.Flo != nil && c.Addons.Flo.Version != "" {
		return c.Addons.Flo.Version
	}
	if c != nil && c.Bnk != nil {
		if c.Bnk.FLOVersion != "" {
			return c.Bnk.FLOVersion
		}
		if c.Bnk.ManifestVersion != "" {
			if chart, ok := bnk.FLOChartFor(c.Bnk.ManifestVersion); ok {
				return chart
			}
		}
	}
	return DefaultFLOVersion
}

func normalizePattern(p string) string {
	if p == PatternHostDevice {
		return PatternDualInterface
	}
	return p
}

// IsBNKPattern reports whether the pattern provisions a BNK data plane.
func (c *Cluster) IsBNKPattern() bool {
	if c == nil {
		return false
	}
	switch normalizePattern(c.Pattern) {
	case PatternExternalOnly, PatternDualInterface, PatternSRIOVExternal:
		return true
	default:
		return false
	}
}

// HasInternalInterface reports whether a second (internal, server-side) interface is provisioned.
func (c *Cluster) HasInternalInterface() bool {
	if c == nil {
		return false
	}
	if normalizePattern(c.Pattern) == PatternDualInterface {
		return true
	}
	if c.Network.Subnets.Internal != "" || c.Network.Internal.CIDR != "" {
		return true
	}
	if c.Network.DataPath != nil && c.Network.DataPath.Internal.CIDR != "" {
		return true
	}
	return false
}

// IsDualInterface is an alias for HasInternalInterface.
func (c *Cluster) IsDualInterface() bool {
	return c.HasInternalInterface()
}

// ExternalVPCName returns the configured external VPC name or a cluster-derived default.
func (c *Cluster) ExternalVPCName() string {
	if c == nil {
		return ""
	}
	if c.Network.External.VPC != "" {
		return c.Network.External.VPC
	}
	if c.Metadata.Name != "" {
		return c.Metadata.Name + "-ext-vpc"
	}
	return ""
}

// ExternalSubnetName returns the configured external subnet name or a cluster-derived default.
func (c *Cluster) ExternalSubnetName() string {
	if c == nil {
		return ""
	}
	if c.Network.External.Subnet != "" {
		return c.Network.External.Subnet
	}
	if c.Metadata.Name != "" {
		return c.Metadata.Name + "-ext-subnet"
	}
	return ""
}

// InternalVPCName returns the configured internal VPC name or a cluster-derived default.
func (c *Cluster) InternalVPCName() string {
	if c == nil {
		return ""
	}
	if c.Network.Internal.VPC != "" {
		return c.Network.Internal.VPC
	}
	if c.Metadata.Name != "" {
		return c.Metadata.Name + "-int-vpc"
	}
	return ""
}

// InternalSubnetName returns the configured internal subnet name or a cluster-derived default.
func (c *Cluster) InternalSubnetName() string {
	if c == nil {
		return ""
	}
	if c.Network.Internal.Subnet != "" {
		return c.Network.Internal.Subnet
	}
	if c.Metadata.Name != "" {
		return c.Metadata.Name + "-int-subnet"
	}
	return ""
}

// IsExternalVPCShared reports whether the external dataplane VPC may be shared across clusters.
func (c *Cluster) IsExternalVPCShared() bool {
	if c == nil {
		return false
	}
	return c.Network.External.VPC != "" && c.Network.External.VPC != c.Metadata.Name+"-ext-vpc"
}

// IsInternalVPCShared reports whether the internal dataplane VPC may be shared across clusters.
func (c *Cluster) IsInternalVPCShared() bool {
	if c == nil {
		return false
	}
	return c.Network.Internal.VPC != "" && c.Network.Internal.VPC != c.Metadata.Name+"-int-vpc"
}

// DataplaneBinding returns how TMM binds its external interface ("host-device" or "sriov").
func (c *Cluster) DataplaneBinding() string {
	if c == nil {
		return ""
	}
	switch normalizePattern(c.Pattern) {
	case PatternExternalOnly, PatternDualInterface:
		return "host-device"
	case PatternSRIOVExternal:
		return "sriov"
	default:
		return ""
	}
}

// IsBGPEnabled reports whether BGP / dynamic routing is enabled for TMM.
func (c *Cluster) IsBGPEnabled() bool {
	if c == nil || c.Bnk == nil {
		return false
	}
	return c.Bnk.BGP || c.Bnk.DynamicRouting
}

// StateDir returns the path to the workspace state directory for this cluster.
func (c *Cluster) StateDir() string {
	if c == nil {
		return ".bnkctl"
	}
	switch c.APIVersion {
	case APIVersionAWS:
		return ".awsbnkctl/" + c.Metadata.Name
	case APIVersionAZR:
		return filepath.Join(".azrbnkctl", c.Metadata.Name)
	case APIVersionGKE:
		return ".gkebnkctl/" + c.Metadata.Name
	default:
		switch c.EffectiveProvider() {
		case ProviderAzure:
			return filepath.Join(".azrbnkctl", c.Metadata.Name)
		case ProviderGCP:
			return ".gkebnkctl/" + c.Metadata.Name
		case ProviderAWS:
			return ".awsbnkctl/" + c.Metadata.Name
		default:
			return filepath.Join(".bnkctl", c.Metadata.Name)
		}
	}
}

// IsReservedGatewayVIPOffset returns true if the host offset falls in the BNK Gateway VIP plan (.100 to .119).
func IsReservedGatewayVIPOffset(offset int) bool {
	return offset >= 100 && offset <= 119
}

// DefaultVIP returns the default Gateway VIP for this cluster across AWS, Azure, and GCP.
func (c *Cluster) DefaultVIP() (string, error) {
	if c == nil {
		return "", errors.New("nil cluster")
	}
	switch c.EffectiveProvider() {
	case ProviderGCP:
		cidr := c.Network.External.PodsCIDR
		offset := 10
		if cidr == "" {
			cidr = c.Network.External.CIDR
			offset = 100
		}
		if cidr == "" {
			return "", errors.New("network.external.cidr not set; pass --vip explicitly")
		}
		return netcalc.Nth(cidr, offset)
	case ProviderAzure:
		if c.Network.Subnets.External == "" {
			return "", errors.New("network.subnets.external not set; pass --vip explicitly")
		}
		return netcalc.Nth(c.Network.Subnets.External, DefaultVIPHostOffset)
	default:
		if c.Network.DataPath == nil || c.Network.DataPath.External.CIDR == "" {
			return "", errors.New("network.dataPath.external.cidr not set; pass --vip explicitly")
		}
		return netcalc.Nth(c.Network.DataPath.External.CIDR, DefaultVIPHostOffset)
	}
}

// DefaultJumphostExtIP returns the default jumphost external ENI IP (.200).
func (c *Cluster) DefaultJumphostExtIP() (string, error) {
	if c == nil || c.Network.DataPath == nil || c.Network.DataPath.External.CIDR == "" {
		return "", errors.New("network.dataPath.external.cidr not set")
	}
	return netcalc.Nth(c.Network.DataPath.External.CIDR, DefaultJumphostExtHostOffset)
}

// SelfIPPoolRange returns the rangeStart/rangeEnd of the /27 self-IP pool containing selfIP.
func SelfIPPoolRange(selfIP string) (start, end string, err error) {
	ip := net.ParseIP(selfIP).To4()
	if ip == nil {
		return "", "", fmt.Errorf("self IP %q is not an IPv4 address", selfIP)
	}
	first := ip[3] &^ selfIPPoolMask
	if first == 0 {
		return "", "", fmt.Errorf("self IP %s falls in the first /%d of its subnet", selfIP, SelfIPPoolPrefix)
	}
	start = net.IPv4(ip[0], ip[1], ip[2], first).String()
	end = net.IPv4(ip[0], ip[1], ip[2], first+selfIPPoolMask-1).String()
	return start, end, nil
}

// DeriveSelfIP returns the host-offset IP and prefix length for a /24 CIDR.
func DeriveSelfIP(cidr string, hostOffset int) (string, int) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", 0
	}
	prefix, _ := ipnet.Mask.Size()
	base := ipnet.IP.To4()
	if base == nil || prefix != 24 || hostOffset < 1 || hostOffset > 254 {
		return "", prefix
	}
	return net.IPv4(base[0], base[1], base[2], byte(hostOffset)).String(), prefix
}

// DemoEnabled reports whether demo mode is active on this cluster.
func (c *Cluster) DemoEnabled() bool {
	return c != nil && c.Demo != nil && c.Demo.Enabled
}

// EnableDemo forces demo mode on (the --demo flag's effect).
func (c *Cluster) EnableDemo() {
	if c.Demo == nil {
		c.Demo = &DemoSpec{}
	}
	c.Demo.Enabled = true
	if c.Demo.TTL == "" {
		c.Demo.TTL = DefaultDemoTTL
	}
}

// SetDemoTags injects the demo marker tags into c.Tags.
func (c *Cluster) SetDemoTags(expiry time.Time) {
	if c.Tags == nil {
		c.Tags = map[string]string{}
	}
	c.Tags[DemoTagKey] = "true"
	c.Tags[DemoExpiryTagKey] = expiry.UTC().Format(time.RFC3339)
}

// BigIPVEEnabled reports whether the BIG-IP VE appliance is enabled.
func (c *Cluster) BigIPVEEnabled() bool {
	return c != nil && c.BigIPVE != nil && c.BigIPVE.Enabled
}

// HasBIGIP reports whether BIG-IP VE is enabled on this cluster.
func (c *Cluster) HasBIGIP() bool {
	return c.BigIPVEEnabled()
}

// IsBIGIPVEPattern reports whether BIG-IP VE is enabled on this cluster.
func (c *Cluster) IsBIGIPVEPattern() bool {
	return c.BigIPVEEnabled()
}

// HasExternalDataPath reports whether the cluster provisions an external BNK data-plane subnet.
func (c *Cluster) HasExternalDataPath() bool {
	return c != nil && c.IsBNKPattern() && c.Network.DataPath != nil && c.Network.DataPath.External.CIDR != ""
}

// TotalDesiredNodes returns the sum of DesiredSize across all node groups.
func (c *Cluster) TotalDesiredNodes() int {
	if c == nil || c.ClusterSpec == nil {
		return 0
	}
	total := 0
	for _, ng := range c.ClusterSpec.NodeGroups {
		total += ng.DesiredSize
	}
	return total
}

// HasGPUNodeGroup reports whether any node group is a GPU node group.
func (c *Cluster) HasGPUNodeGroup() bool {
	if c == nil || c.ClusterSpec == nil {
		return false
	}
	for _, ng := range c.ClusterSpec.NodeGroups {
		if ng.IsGPU() {
			return true
		}
	}
	return false
}

// HasGPUNodeGroups is an alias for HasGPUNodeGroup.
func (c *Cluster) HasGPUNodeGroups() bool {
	return c.HasGPUNodeGroup()
}

// NeedsNvidiaDevicePlugin reports whether the cluster requires the NVIDIA K8s device plugin DaemonSet.
func (c *Cluster) NeedsNvidiaDevicePlugin() bool {
	return c.HasGPUNodeGroup() || (c != nil && c.AI != nil && c.AI.IsSageMakerHyperPod())
}

// NvidiaDevicePluginVersion returns the NVIDIA K8s device plugin version.
func (c *Cluster) NvidiaDevicePluginVersion() string {
	return "v0.17.0"
}

// DesiredGPUNodeCount returns the total desired GPU node count across GPU node groups and HyperPod instance groups.
func (c *Cluster) DesiredGPUNodeCount() int {
	if c == nil {
		return 0
	}
	count := 0
	if c.ClusterSpec != nil {
		for _, ng := range c.ClusterSpec.NodeGroups {
			if ng.IsGPU() {
				count += ng.DesiredSize
			}
		}
	}
	if c.AI != nil && c.AI.IsSageMakerHyperPod() && c.AI.SageMaker != nil {
		for _, ig := range c.AI.SageMaker.InstanceGroups {
			count += ig.InstanceCount
		}
	}
	return count
}

// Load reads and validates a cluster.yaml file at path.
func Load(path string) (*Cluster, error) {
	cleanPath := filepath.Clean(path)
	data, err := os.ReadFile(cleanPath) // #nosec G304 -- operator-supplied config path
	if err != nil {
		return nil, fmt.Errorf("reading cluster.yaml %s: %w", path, err)
	}
	baseDir := filepath.Dir(cleanPath)
	cl, err := LoadBytes(data, baseDir)
	if err != nil {
		return nil, fmt.Errorf("cluster.yaml %s: %w", path, err)
	}
	cl.SourcePath = cleanPath
	return cl, nil
}

// LoadBytes decodes and validates YAML bytes with strict field checking.
func LoadBytes(data []byte, baseDir string) (*Cluster, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)

	var c Cluster
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("intent: empty document")
		}
		return nil, fmt.Errorf("parsing cluster.yaml: %w", err)
	}

	c.applyDefaults(baseDir)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if err := c.derive(baseDir); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Cluster) applyDefaults(baseDir string) {
	if c.Bnk != nil {
		if c.Bnk.ManifestVersion == "" && c.Bnk.Version != "" {
			c.Bnk.ManifestVersion = c.Bnk.Version
		}
		if c.Bnk.Version == "" && c.Bnk.ManifestVersion != "" {
			c.Bnk.Version = c.Bnk.ManifestVersion
		}
	}
	provider := c.EffectiveProvider()

	switch provider {
	case ProviderAzure:
		c.applyAzureDefaults()
	case ProviderGCP:
		c.applyGCPDefaults(baseDir)
	default:
		c.applyAWSDefaults()
	}

	if c.AI != nil {
		if c.AI.SageMaker != nil {
			applySageMakerDefaults(c.AI.SageMaker)
		}
		if c.AI.Synthetic != nil {
			applySyntheticDefaults(c.AI.Synthetic)
		}
	}
}

func (c *Cluster) applyAWSDefaults() {
	c.Pattern = normalizePattern(c.Pattern)

	if c.Metadata.Tags == nil && len(c.Tags) > 0 {
		c.Metadata.Tags = c.Tags
	}
	if c.Tags == nil && len(c.Metadata.Tags) > 0 {
		c.Tags = c.Metadata.Tags
	}

	if c.ClusterSpec != nil {
		if c.ClusterSpec.KubernetesVersion == "" {
			c.ClusterSpec.KubernetesVersion = DefaultKubernetesVersion
		}
		for i := range c.ClusterSpec.NodeGroups {
			ng := &c.ClusterSpec.NodeGroups[i]
			if ng.InstanceType == "" {
				if ng.IsGPU() {
					ng.InstanceType = "g5.2xlarge"
				} else if c.IsBNKPattern() {
					ng.InstanceType = "m6i.4xlarge"
				} else {
					ng.InstanceType = "t3.medium"
				}
			}
			if ng.DesiredSize == 0 {
				if !ng.IsGPU() && c.IsBNKPattern() {
					ng.DesiredSize = 3
				} else {
					ng.DesiredSize = 1
				}
			}
			if ng.MinSize == 0 {
				if !ng.IsGPU() && c.IsBNKPattern() {
					ng.MinSize = 3
				} else {
					ng.MinSize = 1
				}
			}
			if ng.MaxSize == 0 {
				if !ng.IsGPU() && c.IsBNKPattern() {
					ng.MaxSize = 4
				} else {
					ng.MaxSize = 2
				}
			}
			if ng.DiskSize == 0 && ng.DiskSizeGiB > 0 {
				ng.DiskSize = ng.DiskSizeGiB
			}
			if ng.DiskSize == 0 {
				ng.DiskSize = 50
			}
			if ng.DiskSizeGiB == 0 {
				ng.DiskSizeGiB = ng.DiskSize
			}
			if ng.AZ != "" && len(ng.AZs) == 0 {
				ng.AZs = []string{ng.AZ}
			}
			if ng.AZ == "" && len(ng.AZs) > 0 {
				ng.AZ = ng.AZs[0]
			}
			if ng.CapacityType == "" {
				ng.CapacityType = "on-demand"
			}
		}

		if c.IsBNKPattern() && len(c.ClusterSpec.NodeGroups) > 0 && !c.ClusterSpec.NodeGroups[0].IsGPU() {
			ng := &c.ClusterSpec.NodeGroups[0]
			if ng.Labels == nil {
				ng.Labels = make(map[string]string)
			}
			if _, ok := ng.Labels["role"]; !ok {
				ng.Labels["role"] = "bnk"
			}
			if ng.DesiredSize == 1 {
				ng.DesiredSize = 3
			}
			if ng.MinSize == 1 {
				ng.MinSize = 3
			}
			if ng.MaxSize < ng.DesiredSize {
				ng.MaxSize = ng.DesiredSize
			}
		}
	}

	if c.Bnk != nil {
		if c.Bnk.CertManagerVersion == "" {
			c.Bnk.CertManagerVersion = EmbeddedCertManagerVersion
		}
		if c.Bnk.DeploymentSize == "" {
			c.Bnk.DeploymentSize = "Small"
		}
		if c.Bnk.StorageClassName == "" {
			c.Bnk.StorageClassName = "gp2"
		}
		if c.Bnk.ManifestVersion == "" {
			c.Bnk.ManifestVersion = DefaultManifestVersion
		}
		if c.Bnk.TmmMtu == 0 {
			c.Bnk.TmmMtu = 9000
		}
		if c.Bnk.TmmCpu == "" {
			c.Bnk.TmmCpu = DefaultTmmCpu
		}
		if c.Bnk.TmmMemory == "" {
			c.Bnk.TmmMemory = DefaultTmmMemory
		}
		if c.Bnk.TmmHugepages == "" {
			c.Bnk.TmmHugepages = DefaultTmmHugepages
		}
		if c.Bnk.PalCpuSet == "" {
			c.Bnk.PalCpuSet = DefaultPalCpuSet
		}
	}

	if c.Testing.Jumphost != nil {
		if c.Testing.Jumphost.InstanceType == "" {
			c.Testing.Jumphost.InstanceType = "t3.small"
		}
	}

	if c.Demo != nil && c.Demo.Enabled && c.Demo.TTL == "" {
		c.Demo.TTL = DefaultDemoTTL
	}

	if c.BigIPVE != nil && c.BigIPVE.Enabled {
		if c.BigIPVE.InstanceType == "" {
			c.BigIPVE.InstanceType = "c5n.2xlarge"
		}
		if c.BigIPVE.VIP == "" {
			c.BigIPVE.VIP = "10.0.10.120"
		}
		if c.BigIPVE.LicenseTier == "" {
			c.BigIPVE.LicenseTier = "Good"
		}
	}

	if c.IsBNKPattern() && c.Network.DataPath != nil {
		if c.Network.DataPath.SelfIPs == nil {
			c.Network.DataPath.SelfIPs = &SelfIPsSpec{}
		}
		sip := c.Network.DataPath.SelfIPs
		if sip.External == "" {
			if ip, p := DeriveSelfIP(c.Network.DataPath.External.CIDR, DefaultTMMSelfIPHostOffset); ip != "" {
				sip.External = ip
				if sip.PrefixLen == 0 {
					sip.PrefixLen = p
				}
			}
		}
		if c.HasInternalInterface() && sip.Internal == "" {
			if ip, p := DeriveSelfIP(c.Network.DataPath.Internal.CIDR, DefaultTMMSelfIPHostOffset); ip != "" {
				sip.Internal = ip
				if sip.PrefixLen == 0 {
					sip.PrefixLen = p
				}
			}
		}
		if sip.PrefixLen == 0 {
			sip.PrefixLen = 24
		}
	}
}

func (c *Cluster) applyAzureDefaults() {
	if c.Metadata.Location == "" {
		if c.Metadata.Region != "" {
			c.Metadata.Location = c.Metadata.Region
		} else {
			c.Metadata.Location = DefaultAzureLocation
		}
	}
	if c.Metadata.ResourceGroup == "" && c.Metadata.Name != "" {
		c.Metadata.ResourceGroup = fmt.Sprintf("azrbnkctl-%s-rg", c.Metadata.Name)
	}
	if c.Pattern == "" {
		c.Pattern = PatternHostDevice
	}
	if c.Network.VNetCIDR == "" {
		c.Network.VNetCIDR = DefaultVNetCIDR
	}
	if c.Network.Subnets.System == "" {
		c.Network.Subnets.System = DefaultSystemSubnetCIDR
	}
	if c.Network.Subnets.Public == "" {
		c.Network.Subnets.Public = DefaultPublicSubnetCIDR
	}
	if c.Network.Subnets.External == "" {
		c.Network.Subnets.External = DefaultExternalSubnetCIDR
	}
	if c.Pattern == PatternDualInterface && c.Network.Subnets.Internal == "" {
		c.Network.Subnets.Internal = DefaultInternalSubnetCIDR
	}
	if c.Network.NATGateway == nil {
		t := true
		c.Network.NATGateway = &t
	}

	if c.ClusterSpec == nil {
		c.ClusterSpec = &ClusterSpec{}
	}
	if c.ClusterSpec.KubernetesVersion == "" {
		c.ClusterSpec.KubernetesVersion = DefaultKubernetesVersion
	}
	if c.ClusterSpec.NetworkPlugin == "" {
		c.ClusterSpec.NetworkPlugin = DefaultNetworkPlugin
	}
	if c.ClusterSpec.NetworkPluginMode == "" {
		c.ClusterSpec.NetworkPluginMode = DefaultNetworkPluginMode
	}
	if c.ClusterSpec.SystemNodePool.Name == "" {
		c.ClusterSpec.SystemNodePool.Name = "system"
	}
	if c.ClusterSpec.SystemNodePool.VMSize == "" {
		c.ClusterSpec.SystemNodePool.VMSize = DefaultSystemVMSize
	}
	if c.ClusterSpec.SystemNodePool.Count == 0 {
		c.ClusterSpec.SystemNodePool.Count = 2
	}
	if c.ClusterSpec.SystemNodePool.OSDiskSizeGB == 0 {
		c.ClusterSpec.SystemNodePool.OSDiskSizeGB = 128
	}
	if c.ClusterSpec.TMMNodePool.Name == "" {
		c.ClusterSpec.TMMNodePool.Name = "tmm"
	}
	if c.ClusterSpec.TMMNodePool.VMSize == "" {
		c.ClusterSpec.TMMNodePool.VMSize = DefaultTMMVMSize
	}
	if c.ClusterSpec.TMMNodePool.Count == 0 {
		c.ClusterSpec.TMMNodePool.Count = 2
	}
	if c.ClusterSpec.TMMNodePool.OSDiskSizeGB == 0 {
		c.ClusterSpec.TMMNodePool.OSDiskSizeGB = 128
	}
	if c.ClusterSpec.TMMNodePool.AcceleratedNetworking == nil {
		t := true
		c.ClusterSpec.TMMNodePool.AcceleratedNetworking = &t
	}

	if c.Bnk == nil {
		c.Bnk = &BnkSpec{}
	}
	if c.Bnk.ManifestVersion == "" {
		c.Bnk.ManifestVersion = DefaultManifestVersion
	}
	if c.Bnk.DeploymentSize == "" {
		c.Bnk.DeploymentSize = DefaultDeploymentSize
	}
	if c.Bnk.TmmCpu == "" {
		c.Bnk.TmmCpu = DefaultAzureTmmCpu
	}
	if c.Bnk.TmmMemory == "" {
		c.Bnk.TmmMemory = DefaultAzureTmmMemory
	}
	if c.Bnk.TmmMtu == 0 {
		c.Bnk.TmmMtu = DefaultAzureTmmMtu
	}

	if c.Egress.Enabled && c.Egress.Namespace == "" {
		c.Egress.Namespace = "bnk-demo"
	}
	if c.Egress.Enabled && c.Egress.Port == 0 {
		c.Egress.Port = 4789
	}
}

func (c *Cluster) applyGCPDefaults(baseDir string) {
	if c.APIVersion == APIVersion {
		if c.Pattern == "" {
			c.Pattern = PatternHostDevice
		}
		if c.Network.AllowInternalCidr == "" {
			c.Network.AllowInternalCidr = "10.0.0.0/8"
		}
		if c.Network.External.UnderlayCIDR == "" && c.Network.External.CIDR != "" {
			c.Network.External.UnderlayCIDR = c.Network.External.CIDR
		}
	}
	if c.ClusterSpec == nil {
		c.ClusterSpec = &ClusterSpec{}
	}
	if c.ClusterSpec.KubernetesVersion == "" {
		c.ClusterSpec.KubernetesVersion = DefaultKubernetesVersion
	}
	applyPoolDefaults(&c.ClusterSpec.DefaultPool, "", "e2-standard-4", 3, 0)
	applyPoolDefaults(&c.ClusterSpec.TmmPool, "tmm-pool", "e2-standard-8", 1, 8)
	applyPoolDefaults(&c.ClusterSpec.AgentPool, "agent-pool", "e2-standard-4", 1, 32)

	if c.Bnk == nil {
		c.Bnk = &BnkSpec{}
	}
	if c.Bnk.ManifestVersion == "" && c.Bnk.Version != "" {
		c.Bnk.ManifestVersion = c.Bnk.Version
	}
	if c.Bnk.Version == "" && c.Bnk.ManifestVersion != "" {
		c.Bnk.Version = c.Bnk.ManifestVersion
	}
	if c.Bnk.TmmMtu == 0 {
		c.Bnk.TmmMtu = 1460
	}
	if c.Bnk.ImageRegistry == "" {
		c.Bnk.ImageRegistry = DefaultGCPImageRegistry
	}
	if c.Bnk.JwtPath == "" && c.Bnk.JWT != "" {
		c.Bnk.JwtPath = c.Bnk.JWT
	}
	if c.Bnk.JWT == "" && c.Bnk.JwtPath != "" {
		c.Bnk.JWT = c.Bnk.JwtPath
	}
	if c.Bnk.JwtPath != "" && baseDir != "" && !filepath.IsAbs(c.Bnk.JwtPath) {
		c.Bnk.JwtPath = filepath.Join(baseDir, c.Bnk.JwtPath)
		c.Bnk.JWT = c.Bnk.JwtPath
	}
	if c.Bnk.FARArchive != "" && baseDir != "" && !filepath.IsAbs(c.Bnk.FARArchive) {
		c.Bnk.FARArchive = filepath.Join(baseDir, c.Bnk.FARArchive)
	}
	if c.Bnk.FarSecret.Source == "file" {
		c.Bnk.FarSecret.Source = "dockerconfig"
	}
	if c.Bnk.FarSecret.SourceNamespace == "" {
		c.Bnk.FarSecret.SourceNamespace = "f5-cne-core"
	}
	if len(c.Bnk.FarSecret.Namespaces) == 0 {
		c.Bnk.FarSecret.Namespaces = []string{"f5-cne-core", "f5-cne-system", "f5-bnk-instance", "f5-utils", "default"}
	}
	if c.Bnk.FarSecret.DockerConfigFile != "" && baseDir != "" && !filepath.IsAbs(c.Bnk.FarSecret.DockerConfigFile) {
		c.Bnk.FarSecret.DockerConfigFile = filepath.Join(baseDir, c.Bnk.FarSecret.DockerConfigFile)
	}

	if c.Egress.TestNamespace == "" {
		c.Egress.TestNamespace = "egress-test"
	}
	if c.Egress.VxlanPort == 0 {
		c.Egress.VxlanPort = 5789
	}
	if c.Egress.VxlanKey == 0 {
		c.Egress.VxlanKey = 100
	}
	if c.Egress.VxlanMtu == 0 {
		c.Egress.VxlanMtu = 1410
	}
}

func applyPoolDefaults(p *NodePoolSpec, defaultName, defaultMachineType string, defaultNodeCount, defaultMaxPods int) {
	if p.Name == "" && defaultName != "" {
		p.Name = defaultName
	}
	if p.MachineType == "" {
		p.MachineType = defaultMachineType
	}
	if p.NodeCount == 0 {
		p.NodeCount = defaultNodeCount
	}
	if p.MaxPodsPerNode == 0 && defaultMaxPods != 0 {
		p.MaxPodsPerNode = defaultMaxPods
	}
}

func deriveRegion(zone string) string {
	idx := strings.LastIndex(zone, "-")
	if idx < 0 {
		return zone
	}
	return zone[:idx]
}

func splitCIDR(cidr string) (addr string, prefixLen int, err error) {
	addr, err = netcalc.CIDRAddr(cidr)
	if err != nil {
		return "", 0, err
	}
	prefixLen, err = netcalc.CIDRPrefixLen(cidr)
	if err != nil {
		return "", 0, err
	}
	return addr, prefixLen, nil
}

// Validate checks semantic constraints on the loaded cluster according to its target cloud provider.
func (c *Cluster) Validate() error {
	switch c.APIVersion {
	case APIVersion, APIVersionAWS, APIVersionAZR, APIVersionGKE:
	default:
		return fmt.Errorf("apiVersion must be one of %q, %q, %q, or %q, got %q",
			APIVersion, APIVersionAWS, APIVersionAZR, APIVersionGKE, c.APIVersion)
	}
	if c.Kind != Kind {
		return fmt.Errorf("kind must be %q, got %q", Kind, c.Kind)
	}

	switch c.EffectiveProvider() {
	case ProviderAzure:
		return c.validateAzure()
	case ProviderGCP:
		return c.validateGCP()
	default:
		return c.validateAWS()
	}
}

func (c *Cluster) validateAWS() error {
	if !clusterNameRE.MatchString(c.Metadata.Name) {
		return fmt.Errorf("metadata.name %q does not match required pattern %s", c.Metadata.Name, clusterNameRE.String())
	}
	if c.Metadata.Region == "" {
		return fmt.Errorf("metadata.region is required")
	}
	if c.IsBNKPattern() && c.Network.DataPath != nil && c.Network.DataPath.SelfIPs != nil {
		sip := c.Network.DataPath.SelfIPs
		if sip.External != "" {
			if _, _, err := SelfIPPoolRange(sip.External); err != nil {
				return fmt.Errorf("network.dataPath.selfIPs.external: %w", err)
			}
		}
		if sip.Internal != "" && c.HasInternalInterface() {
			if _, _, err := SelfIPPoolRange(sip.Internal); err != nil {
				return fmt.Errorf("network.dataPath.selfIPs.internal: %w", err)
			}
		}
	}
	if len(c.Network.AZs) == 0 {
		return fmt.Errorf("network.azs must contain at least one availability zone")
	}
	if len(c.Network.Subnets.PublicList) == 0 {
		return fmt.Errorf("network.subnets.public must contain at least one subnet")
	}
	if len(c.Network.Subnets.PrivateList) == 0 {
		return fmt.Errorf("network.subnets.private must contain at least one subnet")
	}
	if c.Network.VPCCidr == "" {
		return fmt.Errorf("network.vpcCidr is required")
	}
	if c.ClusterSpec != nil {
		if err := validateKubernetesVersion(c.ClusterSpec.KubernetesVersion, MinKubernetesVersionAWS); err != nil {
			return err
		}
		if len(c.ClusterSpec.NodeGroups) == 0 {
			return fmt.Errorf("cluster.nodeGroups must contain at least one node group when cluster block is present")
		}
		for _, ng := range c.ClusterSpec.NodeGroups {
			if !nodeGroupNameRE.MatchString(ng.Name) {
				return fmt.Errorf("cluster.nodeGroups[].name %q must be lowercase alphanumeric + hyphens", ng.Name)
			}
		}
	}
	if c.Bnk != nil {
		if err := validateAWSBnk(c.Bnk); err != nil {
			return err
		}
	}
	if err := validateAWSPattern(c); err != nil {
		return err
	}
	if err := validateAWSTesting(c); err != nil {
		return err
	}
	if c.Demo != nil && c.Demo.Enabled {
		if err := ValidateDemo(c); err != nil {
			return err
		}
	}
	if c.BigIPVE != nil && c.BigIPVE.Enabled {
		if err := validateBigIPVE(c); err != nil {
			return err
		}
	}
	if c.AI != nil && c.AI.SageMaker != nil && c.AI.SageMaker.Enabled {
		if err := validateSageMaker(c.AI.SageMaker); err != nil {
			return err
		}
	}
	if c.AI != nil && c.AI.Synthetic != nil && c.AI.Synthetic.Enabled {
		if err := validateSynthetic(c.AI.Synthetic); err != nil {
			return err
		}
	}
	if err := validateNodeGroups(c); err != nil {
		return err
	}
	return nil
}

func (c *Cluster) validateAzure() error {
	var errs []string
	if !clusterNameRE.MatchString(c.Metadata.Name) {
		errs = append(errs, fmt.Sprintf("metadata.name %q must match %s (lowercase alphanumeric + hyphens, 2-40 chars)", c.Metadata.Name, clusterNameRE.String()))
	}
	switch c.Pattern {
	case PatternHostDevice, PatternExternalOnly, PatternDualInterface:
	default:
		errs = append(errs, fmt.Sprintf("pattern %q unsupported: must be %q, %q, or %q", c.Pattern, PatternHostDevice, PatternExternalOnly, PatternDualInterface))
	}

	_, vnetNet, err := net.ParseCIDR(c.Network.VNetCIDR)
	if err != nil {
		errs = append(errs, fmt.Sprintf("network.vnetCidr %q is invalid: %v", c.Network.VNetCIDR, err))
	}

	validateSubnet := func(name, cidr string, required bool) {
		if cidr == "" {
			if required {
				errs = append(errs, fmt.Sprintf("network.subnets.%s is required", name))
			}
			return
		}
		_, subNet, subErr := net.ParseCIDR(cidr)
		if subErr != nil {
			errs = append(errs, fmt.Sprintf("network.subnets.%s %q is invalid: %v", name, cidr, subErr))
			return
		}
		if vnetNet != nil && !vnetNet.Contains(subNet.IP) {
			errs = append(errs, fmt.Sprintf("network.subnets.%s %q must be contained within vnetCidr %q", name, cidr, c.Network.VNetCIDR))
		}
	}

	validateSubnet("system", c.Network.Subnets.System, true)
	validateSubnet("public", c.Network.Subnets.Public, false)
	validateSubnet("external", c.Network.Subnets.External, true)
	if c.Pattern == PatternDualInterface {
		validateSubnet("internal", c.Network.Subnets.Internal, true)
	}

	if c.ClusterSpec != nil {
		if err := validateKubernetesVersion(c.ClusterSpec.KubernetesVersion, MinKubernetesVersion); err != nil {
			errs = append(errs, err.Error())
		}
		if !vmSizeRE.MatchString(c.ClusterSpec.SystemNodePool.VMSize) {
			errs = append(errs, fmt.Sprintf("cluster.systemNodePool.vmSize %q is invalid", c.ClusterSpec.SystemNodePool.VMSize))
		}
		if !vmSizeRE.MatchString(c.ClusterSpec.TMMNodePool.VMSize) {
			errs = append(errs, fmt.Sprintf("cluster.tmmNodePool.vmSize %q is invalid", c.ClusterSpec.TMMNodePool.VMSize))
		}
	}

	if c.AI != nil && c.AI.Synthetic != nil {
		if err := validateSynthetic(c.AI.Synthetic); err != nil {
			errs = append(errs, err.Error())
		}
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (c *Cluster) validateGCP() error {
	if !clusterNameRE.MatchString(c.Metadata.Name) {
		return fmt.Errorf("metadata.name %q does not match required pattern %s", c.Metadata.Name, clusterNameRE.String())
	}
	if c.Metadata.Project == "" {
		return errors.New("metadata.project is required")
	}
	if c.Metadata.Zone == "" {
		return errors.New("metadata.zone is required")
	}
	if c.Pattern != PatternHostDevice && c.Pattern != PatternDualInterface && c.Pattern != PatternExternalOnly {
		return fmt.Errorf("pattern must be %q, got %q", PatternHostDevice, c.Pattern)
	}
	if err := validateGCPNetwork(c); err != nil {
		return err
	}
	if c.ClusterSpec != nil && c.ClusterSpec.KubernetesVersion != "" {
		if err := validateKubernetesVersion(c.ClusterSpec.KubernetesVersion, MinKubernetesVersion); err != nil {
			return err
		}
	}
	if c.Bnk == nil || c.Bnk.ManifestVersion == "" {
		return errors.New("bnk.manifestVersion is required")
	}
	if c.Bnk.JwtPath == "" {
		return errors.New("bnk.jwtPath is required")
	}
	if _, err := os.Stat(c.Bnk.JwtPath); err != nil {
		return fmt.Errorf("bnk.jwtPath %q: %w", c.Bnk.JwtPath, err)
	}
	if c.Bnk.FARArchive != "" {
		if _, err := os.Stat(c.Bnk.FARArchive); err != nil {
			return fmt.Errorf("bnk.farArchive %q: %w", c.Bnk.FARArchive, err)
		}
	}
	if c.Bnk.FarSecret.Source != "" || c.Bnk.FARArchive == "" {
		if err := validateFarSecret(c.Bnk.FarSecret); err != nil {
			return err
		}
	}
	if c.AI != nil && c.AI.Synthetic != nil {
		if err := validateSynthetic(c.AI.Synthetic); err != nil {
			return err
		}
	}
	return nil
}

func validateGCPNetwork(c *Cluster) error {
	required := []struct {
		path string
		val  string
	}{
		{"network.main.vpc", c.Network.Main.VPC},
		{"network.main.subnet", c.Network.Main.Subnet},
		{"network.main.podsRangeName", c.Network.Main.PodsRangeName},
		{"network.main.svcsRangeName", c.Network.Main.SvcsRangeName},
		{"network.external.vpc", c.Network.External.VPC},
		{"network.external.subnet", c.Network.External.Subnet},
		{"network.external.podsRangeName", c.Network.External.PodsRangeName},
		{"network.internal.vpc", c.Network.Internal.VPC},
		{"network.internal.subnet", c.Network.Internal.Subnet},
		{"network.internal.podsRangeName", c.Network.Internal.PodsRangeName},
		{"network.allowInternalCidr", c.Network.AllowInternalCidr},
	}
	for _, r := range required {
		if r.val == "" {
			return fmt.Errorf("%s is required", r.path)
		}
	}
	if c.Network.External.MTU <= 0 {
		return errors.New("network.external.mtu is required and must be positive")
	}
	if c.Network.Internal.MTU <= 0 {
		return errors.New("network.internal.mtu is required and must be positive")
	}

	cidrs := []struct {
		path  string
		value string
	}{
		{"network.main.cidr", c.Network.Main.CIDR},
		{"network.main.podsCidr", c.Network.Main.PodsCIDR},
		{"network.main.svcsCidr", c.Network.Main.SvcsCIDR},
		{"network.external.cidr", c.Network.External.CIDR},
		{"network.external.podsCidr", c.Network.External.PodsCIDR},
		{"network.external.underlayCidr", c.Network.External.UnderlayCIDR},
		{"network.internal.cidr", c.Network.Internal.CIDR},
		{"network.internal.podsCidr", c.Network.Internal.PodsCIDR},
		{"network.allowInternalCidr", c.Network.AllowInternalCidr},
	}
	for _, f := range cidrs {
		if f.value == "" {
			return fmt.Errorf("%s is required", f.path)
		}
		if _, _, err := net.ParseCIDR(f.value); err != nil {
			return fmt.Errorf("%s %q does not parse: %w", f.path, f.value, err)
		}
	}
	return nil
}

func validateFarSecret(f FarSecretSpec) error {
	switch f.Source {
	case "kubecontext":
		if f.SourceContext == "" {
			return errors.New("bnk.farSecret.sourceContext is required when bnk.farSecret.source == \"kubecontext\"")
		}
	case "dockerconfig":
		if f.DockerConfigFile == "" {
			return errors.New("bnk.farSecret.dockerConfigFile is required when bnk.farSecret.source == \"dockerconfig\"")
		}
		if _, err := os.Stat(f.DockerConfigFile); err != nil {
			return fmt.Errorf("bnk.farSecret.dockerConfigFile %q: %w", f.DockerConfigFile, err)
		}
	default:
		return fmt.Errorf("bnk.farSecret.source must be one of {kubecontext, dockerconfig}, got %q", f.Source)
	}
	return nil
}

func (c *Cluster) derive(baseDir string) error {
	switch c.EffectiveProvider() {
	case ProviderAzure:
		return c.deriveAzure(baseDir)
	case ProviderGCP:
		return c.deriveGCP()
	default:
		return c.deriveAWS(baseDir)
	}
}

func (c *Cluster) deriveAWS(baseDir string) error {
	if c.Bnk != nil {
		if c.Bnk.FARArchive != "" {
			if filepath.IsAbs(c.Bnk.FARArchive) || baseDir == "" {
				c.Derived.FARArchiveFullPath = c.Bnk.FARArchive
			} else {
				c.Derived.FARArchiveFullPath = filepath.Join(baseDir, c.Bnk.FARArchive)
			}
		}
		if c.Bnk.JWT != "" {
			if filepath.IsAbs(c.Bnk.JWT) || baseDir == "" {
				c.Derived.JWTFullPath = c.Bnk.JWT
			} else {
				c.Derived.JWTFullPath = filepath.Join(baseDir, c.Bnk.JWT)
			}
		}
	}
	return nil
}

func (c *Cluster) deriveAzure(baseDir string) error {
	c.Derived.ResourceGroupName = c.Metadata.ResourceGroup
	c.Derived.VNetName = fmt.Sprintf("vnet-%s", c.Metadata.Name)
	c.Derived.AKSClusterName = fmt.Sprintf("aks-%s", c.Metadata.Name)
	c.Derived.NSGName = fmt.Sprintf("nsg-%s", c.Metadata.Name)
	c.Derived.SystemSubnetName = "snet-system"
	c.Derived.PublicSubnetName = "snet-public"
	c.Derived.ExternalSubnetName = "snet-external"
	c.Derived.InternalSubnetName = "snet-internal"
	c.Derived.NatGatewayName = fmt.Sprintf("natgw-%s", c.Metadata.Name)
	c.Derived.NatGatewayPublicIPName = fmt.Sprintf("pip-natgw-%s", c.Metadata.Name)
	c.Derived.PublicIPName = fmt.Sprintf("pip-ingress-%s", c.Metadata.Name)

	if c.Network.Subnets.External != "" {
		extGw, err := netcalc.Nth(c.Network.Subnets.External, 1)
		if err != nil {
			return fmt.Errorf("derive external gateway: %w", err)
		}
		c.Derived.ExternalGatewayIP = extGw
	}

	if c.Network.Subnets.Internal != "" {
		intGw, err := netcalc.Nth(c.Network.Subnets.Internal, 1)
		if err != nil {
			return fmt.Errorf("derive internal gateway: %w", err)
		}
		c.Derived.InternalGatewayIP = intGw
	}

	if c.Bnk != nil {
		if c.Bnk.FARArchive != "" {
			if filepath.IsAbs(c.Bnk.FARArchive) || baseDir == "" {
				c.Derived.FARArchiveFullPath = c.Bnk.FARArchive
			} else {
				c.Derived.FARArchiveFullPath = filepath.Join(baseDir, c.Bnk.FARArchive)
			}
		}
		if c.Bnk.JWT != "" {
			if filepath.IsAbs(c.Bnk.JWT) || baseDir == "" {
				c.Derived.JWTFullPath = c.Bnk.JWT
			} else {
				c.Derived.JWTFullPath = filepath.Join(baseDir, c.Bnk.JWT)
			}
		}
	}
	return nil
}

func (c *Cluster) deriveGCP() error {
	c.Region = deriveRegion(c.Metadata.Zone)
	var err error
	if c.ExtGwIP, err = netcalc.Nth(c.Network.External.CIDR, 1); err != nil {
		return fmt.Errorf("network.external.cidr: %w", err)
	}
	if c.IntGwIP, err = netcalc.Nth(c.Network.Internal.CIDR, 1); err != nil {
		return fmt.Errorf("network.internal.cidr: %w", err)
	}
	if c.ExtUnderlayDest, c.ExtUnderlayPrefixLen, err = splitCIDR(c.Network.External.UnderlayCIDR); err != nil {
		return fmt.Errorf("network.external.underlayCidr: %w", err)
	}
	if c.MainPodsDest, c.MainPodsPrefixLen, err = splitCIDR(c.Network.Main.PodsCIDR); err != nil {
		return fmt.Errorf("network.main.podsCidr: %w", err)
	}
	if c.MainSvcsDest, c.MainSvcsPrefixLen, err = splitCIDR(c.Network.Main.SvcsCIDR); err != nil {
		return fmt.Errorf("network.main.svcsCidr: %w", err)
	}
	return nil
}

func validateKubernetesVersion(v, floor string) error {
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return fmt.Errorf("cluster.kubernetesVersion %q must be \"<major>.<minor>\" (e.g. %q)", v, floor)
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return fmt.Errorf("cluster.kubernetesVersion %q contains non-integer components", v)
	}
	minParts := strings.Split(floor, ".")
	minMajor, _ := strconv.Atoi(minParts[0])
	minMinor, _ := strconv.Atoi(minParts[1])
	if major < minMajor || (major == minMajor && minor < minMinor) {
		return fmt.Errorf("cluster.kubernetesVersion %q is below the mandated floor %s", v, floor)
	}
	return nil
}

func validateAWSBnk(b *BnkSpec) error {
	if b.FARArchive == "" {
		return fmt.Errorf("bnk.farArchive is required when the bnk: block is present")
	}
	if b.JWT == "" && b.JwtPath == "" {
		return fmt.Errorf("bnk.jwt is required when the bnk: block is present")
	}
	if b.CertManagerVersion != "" && b.CertManagerVersion != EmbeddedCertManagerVersion {
		return fmt.Errorf("bnk.certManagerVersion %q does not match the embedded cert-manager version %q",
			b.CertManagerVersion, EmbeddedCertManagerVersion)
	}
	return nil
}

func requireSlash24(cidr, field string) error {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("%s %q is not a valid CIDR: %w", field, cidr, err)
	}
	if ones, bits := ipnet.Mask.Size(); bits != 32 || ones != 24 {
		return fmt.Errorf("%s %q: currently only /24 dataPath subnets are supported (got /%d)", field, cidr, ones)
	}
	return nil
}

func validateAWSPattern(c *Cluster) error {
	if c.Pattern == "" {
		return nil
	}
	if !c.IsBNKPattern() {
		return fmt.Errorf("pattern %q is not a recognised value (expected one of: external-only, dual-interface, sriov-external; or the host-device alias)", c.Pattern)
	}
	if c.Network.DataPath == nil {
		return fmt.Errorf("pattern %s requires network.dataPath (external subnet) to be set", c.Pattern)
	}
	dp := c.Network.DataPath
	if dp.External.CIDR == "" || dp.External.AZ == "" {
		return fmt.Errorf("pattern %s requires network.dataPath.external.{cidr,az}", c.Pattern)
	}
	if err := requireSlash24(dp.External.CIDR, "network.dataPath.external.cidr"); err != nil {
		return err
	}
	azSet := make(map[string]bool, len(c.Network.AZs))
	for _, az := range c.Network.AZs {
		azSet[az] = true
	}
	if !azSet[dp.External.AZ] {
		return fmt.Errorf("network.dataPath.external.az %q is not in network.azs %v", dp.External.AZ, c.Network.AZs)
	}
	if normalizePattern(c.Pattern) == PatternDualInterface {
		if dp.Internal.CIDR == "" || dp.Internal.AZ == "" {
			return fmt.Errorf("pattern dual-interface requires network.dataPath.internal.{cidr,az}")
		}
		if err := requireSlash24(dp.Internal.CIDR, "network.dataPath.internal.cidr"); err != nil {
			return err
		}
		if !azSet[dp.Internal.AZ] {
			return fmt.Errorf("network.dataPath.internal.az %q is not in network.azs %v", dp.Internal.AZ, c.Network.AZs)
		}
	} else if dp.Internal.CIDR != "" || dp.Internal.AZ != "" {
		return fmt.Errorf("pattern %s is single-interface; remove network.dataPath.internal", c.Pattern)
	}
	if c.ClusterSpec != nil {
		for i, ng := range c.ClusterSpec.NodeGroups {
			if ng.IsGPU() {
				continue
			}
			if ng.DesiredSize > 0 && ng.DesiredSize < 3 {
				return fmt.Errorf("pattern %s requires cluster.nodeGroups[%d].desiredSize >= 3 (dSSM quorum), got %d", c.Pattern, i, ng.DesiredSize)
			}
		}
	}
	return nil
}

func validateAWSTesting(c *Cluster) error {
	if c.Testing.Jumphost == nil {
		return nil
	}
	jh := c.Testing.Jumphost
	if jh.Enabled {
		if !c.IsBNKPattern() || c.Network.DataPath == nil {
			return fmt.Errorf("testing.jumphost requires network.dataPath (BNK_EXT subnet)")
		}
	}
	if jh.InstanceType != "" && !instanceTypeRE.MatchString(jh.InstanceType) {
		return fmt.Errorf("testing.jumphost.instanceType %q does not match expected pattern", jh.InstanceType)
	}
	if idx := jh.MgmtSubnetIndex; idx < 0 || idx >= len(c.Network.Subnets.PublicList) {
		return fmt.Errorf("testing.jumphost.mgmtSubnetIndex %d is out of range (network.subnets.public has %d entries)",
			idx, len(c.Network.Subnets.PublicList))
	}
	return nil
}

var gpuInstanceAZDeny = map[string][]string{
	"ap-southeast-2": {"ap-southeast-2b"},
}

func validateNodeGroups(c *Cluster) error {
	if c.ClusterSpec == nil {
		return nil
	}
	denyTable := gpuInstanceAZDeny
	if envVal := os.Getenv("AWSBNKCTL_GPU_AZ_DENY"); envVal != "" {
		envTable := parseGPUAZDenyEnv(envVal)
		merged := make(map[string][]string, len(gpuInstanceAZDeny)+len(envTable))
		for r, azs := range gpuInstanceAZDeny {
			merged[r] = append(merged[r], azs...)
		}
		for r, azs := range envTable {
			merged[r] = append(merged[r], azs...)
		}
		denyTable = merged
	}

	azSet := make(map[string]bool, len(c.Network.AZs))
	for _, az := range c.Network.AZs {
		azSet[az] = true
	}
	region := c.Metadata.Region

	for i, ng := range c.ClusterSpec.NodeGroups {
		if ng.CapacityType != "" && ng.CapacityType != "on-demand" && ng.CapacityType != "spot" {
			return fmt.Errorf("cluster.nodeGroups[%d] (%s): capacityType %q is not valid (expected on-demand or spot)", i, ng.Name, ng.CapacityType)
		}
		for j, taint := range ng.Taints {
			switch taint.Effect {
			case "NoSchedule", "NoExecute", "PreferNoSchedule":
			default:
				return fmt.Errorf("cluster.nodeGroups[%d] (%s) taints[%d]: effect %q is not valid", i, ng.Name, j, taint.Effect)
			}
		}
		if !ng.IsGPU() || len(ng.AZs) == 0 {
			continue
		}
		deniedSet := make(map[string]bool)
		for _, az := range denyTable[region] {
			deniedSet[az] = true
		}
		var available []string
		for _, az := range c.Network.AZs {
			if !deniedSet[az] {
				available = append(available, az)
			}
		}
		for _, az := range ng.AZs {
			if !azSet[az] {
				return fmt.Errorf("cluster.nodeGroups[%d] (%s): az %q is not in network.azs %v", i, ng.Name, az, c.Network.AZs)
			}
			if deniedSet[az] {
				return fmt.Errorf("cluster.nodeGroups[%d] (%s) pins az %q which has no g5/GPU capacity in region %s (available: %v)", i, ng.Name, az, region, available)
			}
		}
	}
	return nil
}

func parseGPUAZDenyEnv(val string) map[string][]string {
	result := make(map[string][]string)
	for _, entry := range strings.Split(val, ";") {
		parts := strings.SplitN(strings.TrimSpace(entry), ":", 2)
		if len(parts) != 2 {
			continue
		}
		region := strings.TrimSpace(parts[0])
		for _, az := range strings.Split(parts[1], ",") {
			az = strings.TrimSpace(az)
			if az != "" {
				result[region] = append(result[region], az)
			}
		}
	}
	return result
}

var bigipVEReservedOffsets = []int{50, 100, 110, 111, 112, 113, 200, 240}

func validateBigIPVE(c *Cluster) error {
	ve := c.BigIPVE
	if !c.HasInternalInterface() {
		return fmt.Errorf("bigipVE requires pattern: dual-interface; current pattern %q does not have an internal interface", c.Pattern)
	}
	if c.Testing.Jumphost == nil || !c.Testing.Jumphost.Enabled {
		return fmt.Errorf("bigipVE requires testing.jumphost.enabled: true")
	}
	if c.Demo == nil || !c.Demo.Enabled {
		return fmt.Errorf("bigipVE requires demo.enabled: true")
	}
	if !instanceTypeRE.MatchString(ve.InstanceType) {
		return fmt.Errorf("bigipVE.instanceType %q does not match expected pattern", ve.InstanceType)
	}
	switch ve.LicenseTier {
	case "Good", "Better", "Best":
	default:
		return fmt.Errorf("bigipVE.licenseTier %q is not valid; must be one of: Good, Better, Best", ve.LicenseTier)
	}
	if c.Network.DataPath == nil || c.Network.DataPath.External.CIDR == "" {
		return fmt.Errorf("bigipVE.vip validation requires network.dataPath.external.cidr to be set")
	}
	extCIDR := c.Network.DataPath.External.CIDR
	_, extNet, err := net.ParseCIDR(extCIDR)
	if err != nil {
		return fmt.Errorf("network.dataPath.external.cidr %q is not a valid CIDR: %w", extCIDR, err)
	}
	vipIP := net.ParseIP(ve.VIP)
	if vipIP == nil || vipIP.To4() == nil {
		return fmt.Errorf("bigipVE.vip %q is not a valid IPv4 address", ve.VIP)
	}
	if !extNet.Contains(vipIP) {
		return fmt.Errorf("bigipVE.vip %q is not inside network.dataPath.external.cidr %s", ve.VIP, extCIDR)
	}
	vip4 := vipIP.To4()
	if off := int(vip4[3]); off <= 3 || off == 255 {
		return fmt.Errorf("bigipVE.vip %q uses host offset .%d, which AWS reserves in every subnet", ve.VIP, off)
	}
	base := extNet.IP.To4()
	for _, off := range bigipVEReservedOffsets {
		rip := net.IPv4(base[0], base[1], base[2], byte(off)).String() // #nosec G115
		if vipIP.Equal(net.ParseIP(rip)) {
			return fmt.Errorf("bigipVE.vip %q collides with a reserved address in %s", ve.VIP, extCIDR)
		}
	}
	if idx := ve.MgmtSubnetIndex; idx < 0 || idx >= len(c.Network.Subnets.PublicList) {
		return fmt.Errorf("bigipVE.mgmtSubnetIndex %d is out of range (network.subnets.public has %d entries)", idx, len(c.Network.Subnets.PublicList))
	}
	return nil
}

// ValidateDemo enforces demo-mode invariants.
func ValidateDemo(c *Cluster) error {
	if c.Demo == nil || !c.Demo.Enabled {
		return nil
	}
	if c.Demo.TTL != "" {
		d, err := time.ParseDuration(c.Demo.TTL)
		if err != nil {
			return fmt.Errorf("demo.ttl %q is not a valid duration: %w", c.Demo.TTL, err)
		}
		if d <= 0 {
			return fmt.Errorf("demo.ttl %q must be a positive duration", c.Demo.TTL)
		}
	}
	if c.Testing.Jumphost == nil || !c.Testing.Jumphost.Enabled {
		return fmt.Errorf("--demo requires testing.jumphost.enabled: true")
	}
	return nil
}
