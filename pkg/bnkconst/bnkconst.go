// Package bnkconst defines shared Kubernetes namespace, resource, and image constants
// used across BNK lifecycle management, rendering, and diagnostics.
package bnkconst

const (
	// Namespaces
	NamespaceCneCore     = "f5-cne-core"
	NamespaceCneSystem   = "f5-cne-system"
	NamespaceBnkInstance = "f5-cne-system"
	NamespaceUtils       = "f5-utils"
	NamespaceMaintenance = "f5-maintenance"
	NamespaceApp         = "f5-app"
	NamespaceCertManager = "cert-manager"

	// Canonical aliases
	CoreNamespace     = NamespaceCneCore
	SystemNamespace   = NamespaceCneSystem
	InstanceNamespace = NamespaceCneSystem
	UtilsNamespace    = NamespaceUtils

	// ServiceAccounts
	CNEControllerServiceAccount = "f5-cne-controller"

	// Secrets & ConfigMaps
	SecretRegistry = "far-secret"
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

	// Custom Resource names
	CRCNEInstanceName = "f5-bigip-k8s"
	CRLicenseName     = "f5-license"

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
)

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
