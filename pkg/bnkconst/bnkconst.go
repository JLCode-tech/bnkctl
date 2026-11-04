// Package bnkconst defines shared Kubernetes namespace, resource, and image constants
// used across BNK lifecycle management, rendering, and diagnostics.
package bnkconst

import "time"

const (
	// Namespaces
	NamespaceCneCore       = "f5-cne-core"
	NamespaceCneSystem     = "f5-cne-system"
	NamespaceBnkInstance   = "f5-cne-system"
	NamespaceF5BNKInstance = "f5-bnk-instance"
	NamespaceUtils         = "f5-utils"
	NamespaceMaintenance   = "f5-maintenance"
	NamespaceApp           = "f5-app"
	NamespaceAIGateway     = "aigateway"
	NamespaceCertManager   = "cert-manager"
	NamespaceBNKDemo       = "bnk-demo"

	// Canonical aliases
	CoreNamespace     = NamespaceCneCore
	SystemNamespace   = NamespaceCneSystem
	InstanceNamespace = NamespaceCneSystem
	UtilsNamespace    = NamespaceUtils

	// ServiceAccounts
	CNEControllerServiceAccount = "f5-cne-controller"

	// Secrets & ConfigMaps
	SecretRegistry = "far-secret"
	SecretFAR      = SecretRegistry
	// #nosec G101 -- Kubernetes Secret resource name, not a credential
	SecretLicenseJWT = "bnk-license-jwt"
	// #nosec G101 -- Kubernetes Secret resource name, not a credential
	SecretServerCert = "cwc-license-certs-out"
	// #nosec G101 -- Kubernetes Secret resource name, not a credential
	SecretClientCert = "cwc-license-client-certs-out"
	// #nosec G101 -- Kubernetes Secret resource name, not a credential
	SecretOTELClientCert = "otel-client-secret"
	// #nosec G101 -- Kubernetes Secret resource name, not a credential
	SecretOTELServerCert = "otel-server-secret"
	// #nosec G101 -- Kubernetes Secret resource name, not a credential
	SecretWebhookTLS = "f5-lifecycle-operator-webhook-server-cert"
	// #nosec G101 -- Kubernetes Secret resource name, not a credential
	SecretLicenseServiceKey = "cwc-license-service-key"
	// #nosec G101 -- Kubernetes Secret resource name, not a credential
	SecretLicenseKey = "cwc-license-key"
	// #nosec G101 -- Kubernetes Secret resource name, not a credential
	SecretLicenseCtx = "cwc-license-ctx"
	// #nosec G101 -- Kubernetes Secret resource name, not a credential
	SecretCPCLKey     = "cpcl-key-secret"
	CPCLKeySecretName = SecretCPCLKey
	ConfigMapCPCLKey  = "cpcl-key-cm"
	ConfigMapTMMInit  = "tmm-init"

	// Custom Resource names
	CRCNEInstanceName         = "f5-bigip-k8s"
	CRLicenseName             = "bnk-license"
	CRAIGWDNSResolverName     = "aigw-dns-resolver"
	CanonicalGatewayClassName = "f5-gateway-class"
	GatewayControllerName     = "f5.io/gateway-controller"

	// Core Deployments & DaemonSets
	DeployLifecycleOperator = "f5-lifecycle-operator-controller-manager"
	DeployCWC               = "f5-spk-cwc"
	DeployCNEController     = "f5-cne-controller"
	CNEControllerDeployment = DeployCNEController
	DeployDSSM              = "f5-dssm-db"
	DaemonSetTMM            = "f5-tmm"

	// Container names
	ContainerF5TMM    = "f5-tmm"
	ContainerTMrouted = "tmrouted"
	ContainerDebug    = "debug"

	// Default cert-manager version
	DefaultCertManagerVersion = "v1.21.1"

	// Network, MTU, BGP, VXLAN, TEEM, and DNS defaults
	DefaultMTU                   = 1500
	DefaultGCPMTU                = 1460
	DefaultRouteDomain0MTU       = 1390
	DefaultBGPASN                = 64512
	DefaultVXLANPort             = 4789
	DefaultTEEMCertURL           = "https://product.apis.f5.com/ee/v1/keys/public/telemetry.pub"
	CalicoVXLANLinkLocalGW       = "169.254.1.1"
	TMMBlobdLinkLocalEndpoint    = "169.254.0.253 9222"
	DefaultDNSForwarderPrimary   = "8.8.8.8"
	DefaultDNSForwarderSecondary = "1.1.1.1"
	DefaultDNSPort               = 53
	DefaultAWSClusterDNSIP       = "172.20.0.10"
	DefaultAzureClusterDNSIP     = "10.240.0.10"
	DefaultK8sClusterDNSIP       = "10.96.0.10"

	// Lifecycle timeouts & polling intervals
	DefaultActivationTimeout            = 10 * time.Minute
	DefaultActivationPollInterval       = 10 * time.Second
	DefaultCWCPodReadyTimeout           = 5 * time.Minute
	DefaultCRDEstablishedTimeout        = 2 * time.Minute
	DefaultTMMTerminatingStuckThreshold = 45 * time.Second
	DefaultJWKSHTTPTimeout              = 10 * time.Second
)

// DefaultNamespaces is the canonical ordered list of Kubernetes namespaces
// created by the BNK overlay engine before applying CRDs and workloads.
var DefaultNamespaces = []string{
	NamespaceCneCore,
	NamespaceCneSystem,
	NamespaceF5BNKInstance,
	NamespaceUtils,
	NamespaceMaintenance,
	NamespaceApp,
	NamespaceAIGateway,
	NamespaceCertManager,
	NamespaceBNKDemo,
}

// DefaultPublicDNSForwarders is the canonical fallback DNS resolver list for
// F5BigDnsCache net-resolver when forwarding external queries from TMM.
var DefaultPublicDNSForwarders = []string{
	DefaultDNSForwarderPrimary,
	DefaultDNSForwarderSecondary,
}

// BenchmarkProxyPort is a fixed NodePort Forge pins on a benchmark proxy it
// deploys next to BNK, so the jumphost (which runs aiperf) can reach it.
type BenchmarkProxyPort struct {
	Proxy    string
	NodePort int32
}

// BenchmarkProxyNodePorts lists those NodePorts (clear of BNK's own fixed
// NodePorts, e.g. f5-spk-cwc on 30881).
var BenchmarkProxyNodePorts = []BenchmarkProxyPort{
	{Proxy: "haproxy", NodePort: 30890},
	{Proxy: "nginx", NodePort: 30891},
	{Proxy: "envoy", NodePort: 30892},
	{Proxy: "envoy-ai-gateway", NodePort: 30893},
	{Proxy: "llm-d-router", NodePort: 30894},
}
