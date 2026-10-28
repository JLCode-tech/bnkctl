// Package render provides Go text/template rendering helpers for BNK manifest
// templates across AWS, Azure, GCP, and the unified BNK overlay engine.
package render

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"text/template"

	"github.com/JLCode-tech/bnkctl/pkg/bnkconst"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/manifests"
)

// CertChainVars holds the substitution variables for shared/bnk-cert-chain.yaml.
type CertChainVars struct {
	SelfSignedIssuer string // <cluster>-selfsigned-cluster-issuer
	CACertName       string // <cluster>-ca
	CASecretName     string // <cluster>-ca-secret
	CAIssuer         string // <cluster>-ca-cluster-issuer
}

// CertChainVarsFromCluster derives the BNK cert chain template variables from
// the cluster intent.
func CertChainVarsFromCluster(cl *intent.Cluster) CertChainVars {
	name := cl.Metadata.Name
	return CertChainVars{
		SelfSignedIssuer: name + "-selfsigned-cluster-issuer",
		CACertName:       name + "-ca",
		CASecretName:     name + "-ca-secret",
		CAIssuer:         name + "-ca-cluster-issuer",
	}
}

// Render executes a Go text/template given in tmpl with data as the dot-value
// and returns the rendered bytes.
func Render(tmpl []byte, data interface{}) ([]byte, error) {
	t, err := template.New("manifest").Parse(string(tmpl))
	if err != nil {
		return nil, fmt.Errorf("render: parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("render: execute template: %w", err)
	}
	return buf.Bytes(), nil
}

// RenderCertChain renders the BNK cert chain template with vars derived from
// the cluster intent.
func RenderCertChain(tmpl []byte, cl *intent.Cluster) ([]byte, error) {
	vars := CertChainVarsFromCluster(cl)
	return Render(tmpl, vars)
}

// FLOValuesVars holds the substitution variables for shared/flo-values.yaml.tmpl.
type FLOValuesVars struct {
	CAIssuer      string // <cluster>-ca-cluster-issuer
	FARSecretName string // far-secret
	JWT           string // raw JWT token contents (NOT base64)
	ClusterName   string // cl.Metadata.Name
}

// FLOValuesVarsFromCluster derives the FLO values template variables from the
// cluster intent.
func FLOValuesVarsFromCluster(cl *intent.Cluster, jwt string) FLOValuesVars {
	cvars := CertChainVarsFromCluster(cl)
	return FLOValuesVars{
		CAIssuer:      cvars.CAIssuer,
		FARSecretName: "far-secret",
		JWT:           jwt,
		ClusterName:   cl.Metadata.Name,
	}
}

// RenderFLOValues renders the FLO values template with vars derived from
// the cluster intent and the raw JWT string.
func RenderFLOValues(tmpl []byte, cl *intent.Cluster, jwt string) ([]byte, error) {
	vars := FLOValuesVarsFromCluster(cl, jwt)
	return Render(tmpl, vars)
}

// OTELCertsVars holds the substitution variables for shared/otel-certs.yaml.
type OTELCertsVars struct {
	OTELSvrCert     string // external-otelsvr
	OTELSvrSecret   string // external-otelsvr-secret
	OTELF5IngCert   string // external-f5ingotelsvr
	OTELF5IngSecret string // external-f5ingotelsvr-secret
	OperatorNS      string // f5-cne-core
	CAIssuer        string // <cluster>-ca-cluster-issuer
}

// OTELCertsVarsFromCluster derives the OTEL certs template variables from the
// cluster intent.
func OTELCertsVarsFromCluster(cl *intent.Cluster) OTELCertsVars {
	cvars := CertChainVarsFromCluster(cl)
	// #nosec G101 -- Kubernetes Secret and Certificate resource names, not credentials
	return OTELCertsVars{
		OTELSvrCert:     "external-otelsvr",
		OTELSvrSecret:   "external-otelsvr-secret",
		OTELF5IngCert:   "external-f5ingotelsvr",
		OTELF5IngSecret: "external-f5ingotelsvr-secret",
		OperatorNS:      "f5-cne-core",
		CAIssuer:        cvars.CAIssuer,
	}
}

// RenderOTELCerts renders the OTEL certs template with vars derived from
// the cluster intent.
func RenderOTELCerts(tmpl []byte, cl *intent.Cluster) ([]byte, error) {
	vars := OTELCertsVarsFromCluster(cl)
	return Render(tmpl, vars)
}

// ─── cloud-network-mapping ConfigMap ─────────────────────────────────────────

type CloudNetworkMappingAZ struct {
	Name    string
	Subnets []CloudNetworkMappingSubnet
}

type CloudNetworkMappingSubnet struct {
	CIDR     string
	SubnetID string
}

type CloudNetworkMappingVars struct {
	AZs []CloudNetworkMappingAZ
}

// RenderCloudNetworkMapping renders the cloud-network-mapping ConfigMap template.
func RenderCloudNetworkMapping(tmpl []byte, cl *intent.Cluster, getter func(string) string) ([]byte, error) {
	if len(cl.Network.AZs) == 0 {
		return nil, fmt.Errorf("render: network.azs is empty")
	}
	if len(cl.Network.Subnets.PublicList) == 0 {
		return nil, fmt.Errorf("render: network.subnets.public is empty")
	}
	if cl.Network.DataPath == nil {
		return nil, fmt.Errorf("render: network.dataPath is nil (required for BNK patterns)")
	}
	mgmtSubnet := getter("MGMT_SUBNET")
	if mgmtSubnet == "" {
		return nil, fmt.Errorf("render: MGMT_SUBNET not in state (Phase 03 must run first)")
	}
	bnkExtSubnet := getter("BNK_EXT_SUBNET")
	if bnkExtSubnet == "" {
		return nil, fmt.Errorf("render: BNK_EXT_SUBNET not in state (Phase 03 must run first)")
	}

	azSubnets := make(map[string][]CloudNetworkMappingSubnet)

	mgmtAZ := cl.Network.Subnets.PublicList[0].AZ
	azSubnets[mgmtAZ] = append(azSubnets[mgmtAZ], CloudNetworkMappingSubnet{
		CIDR:     cl.Network.Subnets.PublicList[0].CIDR,
		SubnetID: mgmtSubnet,
	})

	extAZ := cl.Network.DataPath.External.AZ
	azSubnets[extAZ] = append(azSubnets[extAZ], CloudNetworkMappingSubnet{
		CIDR:     cl.Network.DataPath.External.CIDR,
		SubnetID: bnkExtSubnet,
	})

	if cl.HasInternalInterface() {
		bnkIntSubnet := getter("BNK_INT_SUBNET")
		if bnkIntSubnet == "" {
			return nil, fmt.Errorf("render: BNK_INT_SUBNET not in state (Phase 03 must run first)")
		}
		intAZ := cl.Network.DataPath.Internal.AZ
		azSubnets[intAZ] = append(azSubnets[intAZ], CloudNetworkMappingSubnet{
			CIDR:     cl.Network.DataPath.Internal.CIDR,
			SubnetID: bnkIntSubnet,
		})
	}

	var vars CloudNetworkMappingVars
	for _, az := range cl.Network.AZs {
		if subnets, ok := azSubnets[az]; ok {
			vars.AZs = append(vars.AZs, CloudNetworkMappingAZ{
				Name:    az,
				Subnets: subnets,
			})
		}
	}

	return Render(tmpl, vars)
}

// ─── NetworkAttachmentDefinitions (host-device) ───────────────────────────────

type NADVars struct {
	Namespace       string
	ExternalNADName string
	InternalNADName string
	ExternalPCI     string
	InternalPCI     string
	HasInternal     bool
}

func orDefault(getter func(string) string, key, def string) string {
	if v := getter(key); v != "" {
		return v
	}
	return def
}

// RenderNADs renders the host-device NADs template for the given namespace.
func RenderNADs(tmpl []byte, namespace string, hasInternal bool, getter func(string) string) ([]byte, error) {
	vars := NADVars{
		Namespace:       namespace,
		ExternalNADName: "external-netdevice",
		InternalNADName: "internal-netdevice",
		ExternalPCI:     orDefault(getter, "EXTERNAL_PCI", "0000:00:08.0"),
		InternalPCI:     orDefault(getter, "INTERNAL_PCI", "0000:00:07.0"),
		HasInternal:     hasInternal,
	}
	return Render(tmpl, vars)
}

// ─── sriov-external dataplane (vfio/DPDK) ─────────────────────────────────────

type SriovVars struct {
	ExternalPCI     string
	Namespace       string
	ExternalNADName string
}

// RenderSriov renders any sriov-external template with the given values.
func RenderSriov(tmpl []byte, externalPCI, namespace, nadName string) ([]byte, error) {
	return Render(tmpl, SriovVars{
		ExternalPCI:     externalPCI,
		Namespace:       namespace,
		ExternalNADName: nadName,
	})
}

// ─── NVIDIA device-plugin (GPU node groups) ────────────────────────────────────

type NvidiaDevicePluginVars struct {
	Version string
}

// RenderNvidiaDevicePlugin renders the NVIDIA device-plugin DaemonSet template.
func RenderNvidiaDevicePlugin(tmpl []byte, version string) ([]byte, error) {
	return Render(tmpl, NvidiaDevicePluginVars{Version: version})
}

// ─── CNEInstance CR ────────────────────────────────────────────────────────

const cneInstanceNamespace = bnkconst.InstanceNamespace

const (
	ServiceCIDRStateKey = "EKS_SERVICE_CIDR"
	TMMK8sRoutesEnv     = "TMM_K8S_ROUTES"
)

type CNEInstanceVars struct {
	DeploymentSize      string
	StorageClassName    string
	ManifestVersion     string
	TmmMtu              int
	TmmCpu              string
	TmmMemory           string
	TmmHugepages        string
	PalCpuSet           string
	InstanceNameCR      string
	InstanceNS          string
	LabName             string
	CAIssuer            string
	FARSecretName       string
	VPCID               string
	AWSRegion           string
	ExternalNAD         string
	InternalNAD         string
	ExternalIFName      string
	InternalIFName      string
	ExternalIFUpper     string
	InternalIFUpper     string
	ExternalPCI         string
	InternalPCI         string
	CloudHostDeviceName string
	CloudHostDeviceTag  string
	HasInternal         bool
	Sriov               bool
	ServiceCIDR         string
	EPP                 bool
	AIGateway           bool
	AIMetrics           bool
}

func isAIGatewayManifestVersion(manifestVersion string) bool {
	v := strings.TrimPrefix(strings.TrimSpace(manifestVersion), "v")
	if v == "" {
		return false
	}
	base := strings.SplitN(v, "-", 2)[0]
	parts := strings.Split(base, ".")
	if len(parts) < 2 {
		return false
	}
	maj, err1 := strconv.Atoi(parts[0])
	min, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	if maj > 2 {
		return true
	}
	if maj < 2 {
		return false
	}
	if min > 4 {
		return true
	}
	if min < 4 {
		return false
	}
	if len(parts) < 3 {
		return false
	}
	patch, err3 := strconv.Atoi(parts[2])
	return err3 == nil && patch >= 1
}

// RenderCNEInstance renders the CNEInstance CR template with vars derived
// from the cluster intent and state.
func RenderCNEInstance(tmpl []byte, cl *intent.Cluster, getter func(string) string) ([]byte, error) {
	if cl.Bnk == nil {
		return nil, fmt.Errorf("render: cl.Bnk is nil — bnk: block required in cluster.yaml")
	}
	vpcID := getter("VPC_ID")
	if vpcID == "" {
		return nil, fmt.Errorf("render: VPC_ID not in state (Phase 02 must run first)")
	}
	cvars := CertChainVarsFromCluster(cl)
	extIFName := orDefault(getter, "EXTERNAL_IFNAME", "ens8")
	intIFName := orDefault(getter, "INTERNAL_IFNAME", "ens7")
	sriov := cl.DataplaneBinding() == "sriov"
	externalNAD := "external-netdevice"
	if sriov {
		externalNAD = "external-sriov"
	}
	aiGateway := isAIGatewayManifestVersion(cl.Bnk.ManifestVersion) || isAIGatewayManifestVersion(cl.Bnk.Version)
	aiMetrics := aiGateway
	if cl.Bnk.AIGateway != nil {
		aiGateway = cl.Bnk.AIGateway.Enabled
		aiMetrics = cl.Bnk.AIGateway.AIMetrics || cl.Bnk.AIGateway.Enabled
	}
	vars := CNEInstanceVars{
		DeploymentSize:      cl.Bnk.DeploymentSize,
		StorageClassName:    cl.Bnk.StorageClassName,
		ManifestVersion:     cl.Bnk.ManifestVersion,
		TmmMtu:              cl.Bnk.TmmMtu.Int(),
		TmmCpu:              cl.Bnk.TmmCpu,
		TmmMemory:           cl.Bnk.TmmMemory,
		TmmHugepages:        cl.Bnk.TmmHugepages,
		PalCpuSet:           cl.Bnk.PalCpuSet,
		InstanceNameCR:      cl.Metadata.Name + "-bnk",
		InstanceNS:          cneInstanceNamespace,
		LabName:             cl.Metadata.Name,
		CAIssuer:            cvars.CAIssuer,
		FARSecretName:       "far-secret",
		VPCID:               vpcID,
		AWSRegion:           cl.Metadata.Region,
		ExternalNAD:         externalNAD,
		InternalNAD:         "internal-netdevice",
		ExternalIFName:      extIFName,
		InternalIFName:      intIFName,
		ExternalIFUpper:     strings.ToUpper(extIFName),
		InternalIFUpper:     strings.ToUpper(intIFName),
		ExternalPCI:         orDefault(getter, "EXTERNAL_PCI", "0000:00:08.0"),
		InternalPCI:         orDefault(getter, "INTERNAL_PCI", "0000:00:07.0"),
		CloudHostDeviceName: orDefault(getter, "CLOUD_HOST_DEVICE_NAME", extIFName),
		CloudHostDeviceTag:  "f5-cne-device",
		HasInternal:         cl.HasInternalInterface(),
		Sriov:               sriov,
		ServiceCIDR:         getter(ServiceCIDRStateKey),
		EPP:                 !strings.HasPrefix(cl.Bnk.ManifestVersion, "2.3"),
		AIGateway:           aiGateway,
		AIMetrics:           aiMetrics,
	}
	return Render(tmpl, vars)
}

// ─── License CR ───────────────────────────────────────────────────────────────

type LicenseCRVars struct {
	LabName string
	JWT     string
}

// RenderLicenseCR renders the License CR template.
func RenderLicenseCR(tmpl []byte, cl *intent.Cluster, jwt string) ([]byte, error) {
	vars := LicenseCRVars{
		LabName: cl.Metadata.Name,
		JWT:     jwt,
	}
	return Render(tmpl, vars)
}

// ─── Iface-discovery pod (phase 17c) ─────────────────────────────────────────

type IfaceDiscoveryPodVars struct {
	Namespace string
	NodeName  string
}

// RenderIfaceDiscoveryPod renders the iface-discovery pod manifest.
func RenderIfaceDiscoveryPod(tmpl []byte, namespace, nodeName string) ([]byte, error) {
	return Render(tmpl, IfaceDiscoveryPodVars{
		Namespace: namespace,
		NodeName:  nodeName,
	})
}

type GatewayClassVars struct {
	GwcName    string
	LabName    string
	InstanceNS string
}

// RenderGatewayClass renders the GatewayClass template for the host-device pattern.
func RenderGatewayClass(tmpl []byte, cl *intent.Cluster) ([]byte, error) {
	return Render(tmpl, GatewayClassVars{
		GwcName:    cl.Metadata.Name + "-gatewayclass",
		LabName:    cl.Metadata.Name,
		InstanceNS: cneInstanceNamespace,
	})
}

// ─── Infra (BNK 2.4 network model) ──────────────────────────────────────────

const (
	InfraName                = "infra"
	InfraExtNetwork          = "ext-vlan-infra"
	InfraIntNetwork          = "int-vlan-infra"
	InfraExtIPAM             = "ext-ipam"
	InfraIntIPAM             = "int-ipam"
	InfraNetAttachments      = "tmm-net-attachments"
	InfraListenerPool        = "listener-pool"
	infraListenerFirst       = 100
	infraListenerLast        = 199
	InfraEgressSubnet        = "192.168.0.0/16"
	InfraEgressPort          = 4789
	InfraRouteVPC            = "vpc"
	InfraRouteVNet           = "vnet"
	InfraRouteDefault        = "default"
	GatewaySettingsName      = "bnk-gateway-settings"
	GatewaySettingsSnatPool  = "egress-snat-pool"
	GatewaySettingsEgressCfg = "default-egress"
	EgressGatewayName        = "egress-gw"
)

const ZebOSConfigMapName = "f5-tmm-dynamic-routing-template"

func InfraTunnelNetwork(hasInternal bool) string {
	if hasInternal {
		return InfraIntNetwork
	}
	return InfraExtNetwork
}

type InfraVars struct {
	InfraName       string
	InstanceNS      string
	LabName         string
	TmmExtSelfIP    string
	TmmExtSelfIPEnd string
	TmmIntSelfIP    string
	TmmIntSelfIPEnd string
	ListenerPool    string
	ListenerStart   string
	ListenerEnd     string
	ExternalNAD     string
	InternalNAD     string
	ExtNetwork      string
	IntNetwork      string
	Mtu             int
	HasInternal     bool
	ExtAZ           string
	IntAZ           string
	TunnelNetwork   string
	EgressSubnet    string
	EgressPort      int
	VpcCidr         string
	ServiceCIDR     string
	PodSubnetA      string
	PodSubnetB      string
	TunnelGateway   string
	ExtGateway      string
	RouteVPC        string
	RouteDefault    string
}

// RenderInfra renders the Infra CR for a BNK pattern.
func RenderInfra(tmpl []byte, cl *intent.Cluster, hasInternal bool) ([]byte, error) {
	if cl.Network.DataPath == nil || cl.Network.DataPath.SelfIPs == nil || cl.Network.DataPath.External.CIDR == "" {
		return nil, fmt.Errorf("render infra: network.dataPath external subnet and self IPs are required")
	}
	sel := cl.Network.DataPath.SelfIPs
	if sel.External == "" {
		return nil, fmt.Errorf("render infra: external self IP not derivable (external subnet must be /24)")
	}
	if hasInternal && sel.Internal == "" {
		return nil, fmt.Errorf("render infra: internal self IP not derivable (internal subnet must be /24)")
	}
	extStart, extEnd, err := intent.SelfIPPoolRange(sel.External)
	if err != nil {
		return nil, fmt.Errorf("render infra: external self IP pool: %w", err)
	}
	var intStart, intEnd string
	if hasInternal {
		if intStart, intEnd, err = intent.SelfIPPoolRange(sel.Internal); err != nil {
			return nil, fmt.Errorf("render infra: internal self IP pool: %w", err)
		}
	}
	start, _ := intent.DeriveSelfIP(cl.Network.DataPath.External.CIDR, infraListenerFirst)
	end, _ := intent.DeriveSelfIP(cl.Network.DataPath.External.CIDR, infraListenerLast)
	if start == "" || end == "" {
		return nil, fmt.Errorf("render infra: listener pool not derivable from external CIDR %q (must be /24)", cl.Network.DataPath.External.CIDR)
	}
	externalNAD := "external-netdevice"
	if cl.DataplaneBinding() == "sriov" {
		externalNAD = "external-sriov"
	}
	mtu := 1500
	if cl.Bnk != nil && cl.Bnk.TmmMtu.Int() > 0 {
		mtu = cl.Bnk.TmmMtu.Int()
	}
	extGW, _ := intent.DeriveSelfIP(cl.Network.DataPath.External.CIDR, 1)
	tunnelGW := extGW
	if hasInternal {
		tunnelGW, _ = intent.DeriveSelfIP(cl.Network.DataPath.Internal.CIDR, 1)
	}
	var podSubnetA, podSubnetB string
	if len(cl.Network.Subnets.PrivateList) > 0 {
		podSubnetA = strings.TrimSpace(cl.Network.Subnets.PrivateList[0].CIDR)
	}
	if len(cl.Network.Subnets.PrivateList) > 1 {
		podSubnetB = strings.TrimSpace(cl.Network.Subnets.PrivateList[1].CIDR)
	}
	vars := InfraVars{
		InfraName:       InfraName,
		InstanceNS:      cneInstanceNamespace,
		LabName:         cl.Metadata.Name,
		TmmExtSelfIP:    extStart,
		TmmExtSelfIPEnd: extEnd,
		TmmIntSelfIP:    intStart,
		TmmIntSelfIPEnd: intEnd,
		ListenerPool:    InfraListenerPool,
		ListenerStart:   start,
		ListenerEnd:     end,
		ExternalNAD:     externalNAD,
		InternalNAD:     "internal-netdevice",
		ExtNetwork:      InfraExtNetwork,
		IntNetwork:      InfraIntNetwork,
		Mtu:             mtu,
		HasInternal:     hasInternal,
		ExtAZ:           cl.Network.DataPath.External.AZ,
		IntAZ:           cl.Network.DataPath.Internal.AZ,
		TunnelNetwork:   InfraTunnelNetwork(hasInternal),
		EgressSubnet:    InfraEgressSubnet,
		EgressPort:      InfraEgressPort,
		VpcCidr:         cl.Network.VPCCidr,
		ServiceCIDR:     "172.20.0.0/16",
		PodSubnetA:      podSubnetA,
		PodSubnetB:      podSubnetB,
		TunnelGateway:   tunnelGW,
		ExtGateway:      extGW,
		RouteVPC:        InfraRouteVPC,
		RouteDefault:    InfraRouteDefault,
	}
	return Render(tmpl, vars)
}

// ─── cne-controller RBAC supplement (BNK 2.4 / FLO 2.30) ─────────────────────

const CNEControllerServiceAccount = "f5-cne-controller"

func CNEControllerRBACName(cl *intent.Cluster) string {
	return cl.Metadata.Name + "-cne-controller-endpointslices"
}

type CNEControllerRBACVars struct {
	Name           string
	LabName        string
	InstanceNS     string
	ServiceAccount string
}

// RenderCNEControllerRBAC renders the EndpointSlice ClusterRole + binding for
// the cne-controller ServiceAccount.
func RenderCNEControllerRBAC(tmpl []byte, cl *intent.Cluster) ([]byte, error) {
	return RenderCNEControllerRBACNamespace(tmpl, cl, cneInstanceNamespace)
}

// RenderCNEControllerRBACNamespace renders the RBAC with an explicit namespace.
func RenderCNEControllerRBACNamespace(tmpl []byte, cl *intent.Cluster, ns string) ([]byte, error) {
	if cl == nil || cl.Metadata.Name == "" {
		return nil, fmt.Errorf("render cne-controller rbac: cluster name is required")
	}
	if ns == "" {
		ns = cneInstanceNamespace
	}
	return Render(tmpl, CNEControllerRBACVars{
		Name:           CNEControllerRBACName(cl),
		LabName:        cl.Metadata.Name,
		InstanceNS:     ns,
		ServiceAccount: CNEControllerServiceAccount,
	})
}

// ─── Azure template helpers (azrbnkctl compatibility) ────────────────────────

type CNEInstanceData struct {
	ManifestVersion    string
	DeploymentSize     string
	TmmCpu             string
	TmmMemory          string
	TmmMtu             int
	ExtPCI             string
	IntPCI             string
	Telemetry          bool
	AIGateway          bool
	ImageRegistry      string
	PalCpuSet          string
	NetworkAttachments []string
	SkipNics           string
	TmmK8sRoutes       string
}

type VlansRoutesData struct {
	TmmMtu            int
	ExtPrefixLen      int
	ExtSelfIP         string
	ExtGwIP           string
	IntPrefixLen      int
	IntSelfIP         string
	IntGwIP           string
	ServiceCidrDest   string
	ServiceCidrPrefix int
	PodCidrDest       string
	PodCidrPrefix     int
}

type InfraData struct {
	TmmMtu            int
	ExtSelfIP         string
	ExtGwIP           string
	IntSelfIP         string
	IntGwIP           string
	ListenerStart     string
	ListenerEnd       string
	ServiceCidrDest   string
	ServiceCidrPrefix int
	PodCidrDest       string
	PodCidrPrefix     int
}

type EgressData struct {
	Namespace        string
	GatewayClassName string
	NetworkRef       string
}

type LicenseData struct {
	JWT string
}

type NADsData struct {
	Namespace string
	ExtPCI    string
	IntPCI    string
}

func renderEmbeddedTemplate(path string, data interface{}) ([]byte, error) {
	tmplBytes, err := manifests.FS.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("render: read embedded template %s: %w", path, err)
	}
	return Render(tmplBytes, data)
}

func normalizeDeploymentSize(s string) string {
	switch strings.ToLower(s) {
	case "small":
		return "Small"
	case "medium":
		return "Medium"
	case "large":
		return "Large"
	case "max":
		return "Max"
	default:
		if s == "" {
			return "Small"
		}
		return s
	}
}

// RenderAzureCNEInstance renders the Azure CNEInstance custom resource.
func RenderAzureCNEInstance(data CNEInstanceData) ([]byte, error) {
	data.DeploymentSize = normalizeDeploymentSize(data.DeploymentSize)
	if isAIGatewayManifestVersion(data.ManifestVersion) {
		data.AIGateway = true
	}
	return renderEmbeddedTemplate("azure-templates/cneinstance.yaml.tmpl", data)
}

// RenderAzureLicenseCR renders the Azure License custom resource.
func RenderAzureLicenseCR(jwt string) ([]byte, error) {
	return renderEmbeddedTemplate("azure-templates/license-cr.yaml.tmpl", LicenseData{JWT: jwt})
}

// RenderAzureVlansRoutes renders the Azure F5SPKVlan and F5SPKStaticRoute custom resources.
func RenderAzureVlansRoutes(data VlansRoutesData) ([]byte, error) {
	if data.ServiceCidrDest == "" {
		data.ServiceCidrDest = "10.240.0.0"
	}
	if data.ServiceCidrPrefix == 0 {
		data.ServiceCidrPrefix = 16
	}
	if data.PodCidrDest == "" {
		data.PodCidrDest = "10.244.0.0"
	}
	if data.PodCidrPrefix == 0 {
		data.PodCidrPrefix = 16
	}
	return renderEmbeddedTemplate("azure-templates/vlans-routes.yaml.tmpl", data)
}

// RenderAzureInfra renders the Azure BNK 2.4 Infra custom resource.
func RenderAzureInfra(data InfraData) ([]byte, error) {
	if data.TmmMtu == 0 {
		data.TmmMtu = 1500
	}
	if data.ServiceCidrDest == "" {
		data.ServiceCidrDest = "10.240.0.0"
	}
	if data.ServiceCidrPrefix == 0 {
		data.ServiceCidrPrefix = 16
	}
	if data.PodCidrDest == "" {
		data.PodCidrDest = "10.244.0.0"
	}
	if data.PodCidrPrefix == 0 {
		data.PodCidrPrefix = 16
	}
	if data.ListenerStart == "" {
		data.ListenerStart = "10.0.17.100"
	}
	if data.ListenerEnd == "" {
		data.ListenerEnd = "10.0.17.199"
	}
	return renderEmbeddedTemplate("azure-templates/infra.yaml.tmpl", data)
}

// RenderAzureEgress renders the legacy BNK 2.3 F5SPKEgress custom resource on Azure.
func RenderAzureEgress(namespace string) ([]byte, error) {
	return renderEmbeddedTemplate("azure-templates/egress.yaml.tmpl", EgressData{Namespace: namespace})
}

// RenderEgressGateway renders the BNK 2.4 GatewaySettings and EgressGateway custom resources.
func RenderEgressGateway(data EgressData) ([]byte, error) {
	if data.Namespace == "" {
		data.Namespace = "bnk-demo"
	}
	if data.GatewayClassName == "" {
		data.GatewayClassName = "f5-cne-gateway-class"
	}
	if data.NetworkRef == "" {
		data.NetworkRef = "internal"
	}
	return renderEmbeddedTemplate("azure-templates/egress.yaml.tmpl", data)
}

// RenderAzureNADs renders the Azure NetworkAttachmentDefinition custom resources.
func RenderAzureNADs(data NADsData) ([]byte, error) {
	return renderEmbeddedTemplate("azure-templates/nads.yaml.tmpl", data)
}

// ─── GKE template helpers (gkebnkctl compatibility) ──────────────────────────

type GKENADsVars struct {
	ExtIface  string
	ExtSelfIP string
	IntIface  string
	IntSelfIP string
}

func RenderGKENADs(tmpl []byte, v GKENADsVars) ([]byte, error) {
	return Render(tmpl, v)
}

type GKECNEInstanceVars struct {
	TmmMtu          string
	ManifestVersion string
	ExtPCI          string
	IntPCI          string
	ImageRegistry   string
	AIGateway       bool
}

func RenderGKECNEInstance(tmpl []byte, v GKECNEInstanceVars) ([]byte, error) {
	if isAIGatewayManifestVersion(v.ManifestVersion) {
		v.AIGateway = true
	}
	return Render(tmpl, v)
}

type VlansRoutesVars struct {
	TmmMtu               string
	ExtSelfIP            string
	IntSelfIP            string
	ExtGwIP              string
	IntGwIP              string
	VxlanPort            int
	ExtUnderlayDest      string
	ExtUnderlayPrefixLen int
	MainPodsDest         string
	MainPodsPrefixLen    int
	MainSvcsDest         string
	MainSvcsPrefixLen    int
	TmmPodGW             string
}

func RenderVlansRoutes(tmpl []byte, v VlansRoutesVars) ([]byte, error) {
	return Render(tmpl, v)
}

type GKELicenseCRVars struct {
	JWT string
}

func RenderGKELicenseCR(tmpl []byte, v GKELicenseCRVars) ([]byte, error) {
	return Render(tmpl, v)
}

type NamespacesVars struct {
	TestNamespace string
}

func RenderNamespaces(tmpl []byte, v NamespacesVars) ([]byte, error) {
	return Render(tmpl, v)
}

type EgressVars struct {
	TestNamespace string
	VxlanKey      int
	VxlanMtu      int
	AgentExtIface string
	VxlanPort     int
	AgentPoolName string
}

func RenderEgress(tmpl []byte, v EgressVars) ([]byte, error) {
	return Render(tmpl, v)
}

type AgentRouteSetterVars struct {
	AgentPoolName string
	ExtPodsCidr   string
	ExtGwIP       string
	AgentExtIface string
}

func RenderAgentRouteSetter(tmpl []byte, v AgentRouteSetterVars) ([]byte, error) {
	return Render(tmpl, v)
}

type IngressVxlanRouteVars struct {
	Name        string
	Destination string
	PrefixLen   int
	Gateway     string
}

type IngressVxlanRoutesVars struct {
	Routes []IngressVxlanRouteVars
}

func RenderIngressVxlanRoutes(tmpl []byte, v IngressVxlanRoutesVars) ([]byte, error) {
	return Render(tmpl, v)
}
