// Package phases implements the modular, cloud-agnostic Kubernetes installation
// phases for the BNK overlay engine (Namespaces, Cert-Manager & Multus, Certs,
// FLO Operator Helm install, CNEInstance, License & CPCL JWKS sync, Network & Egress CRDs,
// and Activation readiness verification).
//
// Zero cloud SDK imports are permitted in this package.
package phases

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/JLCode-tech/bnkctl/pkg/bnkconst"
	"github.com/JLCode-tech/bnkctl/pkg/k8s"
	"github.com/JLCode-tech/bnkctl/pkg/provider"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// ApplyYAMLFunc is a callback that parses and server-side-applies a multi-doc YAML stream.
type ApplyYAMLFunc func(ctx context.Context, raw []byte) error

// RequiredNamespaces lists all standard BNK namespaces created during PhaseNamespaces.
var RequiredNamespaces = []string{
	bnkconst.NamespaceCneCore,
	bnkconst.NamespaceCneSystem,
	"f5-bnk-instance",
	bnkconst.NamespaceUtils,
	bnkconst.NamespaceMaintenance,
	bnkconst.NamespaceApp,
	bnkconst.NamespaceCertManager,
	"aigateway",
	"bnk-demo",
}

// FARSecretNamespaces lists the namespaces into which far-secret is distributed.
var FARSecretNamespaces = []string{
	bnkconst.NamespaceCneCore,
	bnkconst.NamespaceCneSystem,
	"f5-bnk-instance",
	bnkconst.NamespaceUtils,
	bnkconst.NamespaceMaintenance,
	bnkconst.NamespaceApp,
	"aigateway",
}

// PhaseNamespaces ensures all required BNK namespaces and the test namespace exist.
func PhaseNamespaces(ctx context.Context, k8sClient kubernetes.Interface, applyFn ApplyYAMLFunc, namespacesYAML []byte, extraNamespaces ...string) error {
	if applyFn != nil && len(bytes.TrimSpace(namespacesYAML)) > 0 {
		if err := applyFn(ctx, namespacesYAML); err != nil {
			return fmt.Errorf("apply namespaces YAML: %w", err)
		}
	}
	if k8sClient == nil {
		return nil
	}
	all := append([]string{}, RequiredNamespaces...)
	for _, ns := range extraNamespaces {
		ns = strings.TrimSpace(ns)
		if ns != "" && ns != "default" {
			all = append(all, ns)
		}
	}
	seen := make(map[string]bool, len(all))
	for _, ns := range all {
		if seen[ns] {
			continue
		}
		seen[ns] = true
		nsObj := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		}
		if _, err := k8sClient.CoreV1().Namespaces().Create(ctx, nsObj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create namespace %s: %w", ns, err)
		}
	}
	return nil
}

// ExtractFARPayload normalizes a raw JSON or base64-encoded FAR service account key
// into base64-encoded service account JSON suitable for _json_key_base64 auth.
func ExtractFARPayload(farKey string) string {
	trimmed := strings.TrimSpace(farKey)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "{") {
		var dockerCfg struct {
			Auths map[string]struct {
				Password string `json:"password"`
				Auth     string `json:"auth"`
			} `json:"auths"`
		}
		if json.Unmarshal([]byte(trimmed), &dockerCfg) == nil && len(dockerCfg.Auths) > 0 {
			for _, entry := range dockerCfg.Auths {
				if strings.TrimSpace(entry.Password) != "" {
					pw := strings.TrimSpace(entry.Password)
					if strings.HasPrefix(pw, "{") {
						return base64.StdEncoding.EncodeToString([]byte(pw))
					}
					return pw
				}
				if strings.TrimSpace(entry.Auth) != "" {
					if dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(entry.Auth)); err == nil {
						if idx := strings.IndexByte(string(dec), ':'); idx >= 0 {
							pw := strings.TrimSpace(string(dec[idx+1:]))
							if strings.HasPrefix(pw, "{") {
								return base64.StdEncoding.EncodeToString([]byte(pw))
							}
							return pw
						}
					}
				}
			}
		}
		return base64.StdEncoding.EncodeToString([]byte(trimmed))
	}
	return trimmed
}

// BuildDockerConfigJSON builds a .dockerconfigjson payload for repo.f5.com using _json_key_base64.
func BuildDockerConfigJSON(farKey string) ([]byte, error) {
	b64Key := ExtractFARPayload(farKey)
	if b64Key == "" {
		return nil, fmt.Errorf("empty FAR key")
	}
	authToken := base64.StdEncoding.EncodeToString([]byte("_json_key_base64:" + b64Key))
	cfg := map[string]any{
		"auths": map[string]any{
			"repo.f5.com": map[string]any{
				"username": "_json_key_base64",
				"password": b64Key,
				"auth":     authToken,
			},
		},
	}
	return json.Marshal(cfg)
}

// DistributeFARSecrets creates or updates the far-secret Docker pull secret across all BNK namespaces.
func DistributeFARSecrets(ctx context.Context, k8sClient kubernetes.Interface, farKey string) error {
	if k8sClient == nil || strings.TrimSpace(farKey) == "" {
		return nil
	}
	dockerCfg, err := BuildDockerConfigJSON(farKey)
	if err != nil {
		return err
	}
	for _, ns := range FARSecretNamespaces {
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      bnkconst.SecretRegistry,
				Namespace: ns,
			},
			Type: corev1.SecretTypeDockerConfigJson,
			Data: map[string][]byte{
				corev1.DockerConfigJsonKey: dockerCfg,
			},
		}
		_, err := k8sClient.CoreV1().Secrets(ns).Create(ctx, sec, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			_, err = k8sClient.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{})
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("distribute %s in namespace %s: %w", bnkconst.SecretRegistry, ns, err)
		}
	}
	return nil
}

// PhaseCertManagerAndMultus distributes far-secret across BNK namespaces, installs cert-manager v1.21.1
// via Helm if kubeconfigPath is provided and cert-manager is not already present, and reconciles Multus CNI.
func PhaseCertManagerAndMultus(ctx context.Context, k8sClient kubernetes.Interface, kubeconfigPath, farKey string) error {
	if err := DistributeFARSecrets(ctx, k8sClient, farKey); err != nil {
		return err
	}
	if kubeconfigPath != "" {
		alreadyReady := false
		if k8sClient != nil {
			if dep, err := k8sClient.AppsV1().Deployments(k8s.CertManagerNamespace).Get(ctx, "cert-manager-webhook", metav1.GetOptions{}); err == nil && dep.Status.AvailableReplicas > 0 {
				alreadyReady = true
			}
		}
		if !alreadyReady {
			if cmInstaller, err := k8s.NewCertManagerHelmInstaller(kubeconfigPath); err == nil {
				if ch, pullErr := cmInstaller.PullAndLoad(k8s.CertManagerChartRef, bnkconst.DefaultCertManagerVersion); pullErr == nil {
					vals := map[string]any{
						"crds": map[string]any{"enabled": true},
					}
					rels, _ := cmInstaller.List(k8s.CertManagerNamespace, k8s.CertManagerReleaseName)
					if len(rels) > 0 {
						_, _ = cmInstaller.Upgrade(k8s.CertManagerReleaseName, k8s.CertManagerNamespace, ch, vals)
					} else {
						_, _ = cmInstaller.Install(k8s.CertManagerReleaseName, k8s.CertManagerNamespace, ch, vals)
					}
				}
			}
		}
	}
	return nil
}

// EnsureCWCCerts generates self-signed CWC server/client TLS secrets in coreNS, systemNS, and utilsNS
// if they do not already exist.
func EnsureCWCCerts(ctx context.Context, k8sClient kubernetes.Interface, coreNS, utilsNS string) error {
	if k8sClient == nil {
		return nil
	}
	if _, err := k8sClient.CoreV1().Secrets(coreNS).Get(ctx, bnkconst.SecretServerCert, metav1.GetOptions{}); err == nil {
		return nil
	}

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cwc-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	makeLeaf := func(cn string, dnsNames []string) ([]byte, []byte, error) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, nil, err
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(365 * 24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
			DNSNames:     dnsNames,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caTmpl, &key.PublicKey, caKey)
		if err != nil {
			return nil, nil, err
		}
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
		return certPEM, keyPEM, nil
	}

	srvCert, srvKey, err := makeLeaf("f5-spk-cwc", []string{
		"f5-spk-cwc",
		"f5-spk-cwc." + coreNS,
		"f5-spk-cwc." + coreNS + ".svc",
		"f5-spk-cwc." + coreNS + ".svc.cluster.local",
		"f5-spk-cwc." + bnkconst.NamespaceCneCore,
		"f5-spk-cwc." + bnkconst.NamespaceCneCore + ".svc",
		"f5-spk-cwc." + bnkconst.NamespaceCneCore + ".svc.cluster.local",
		"localhost",
	})
	if err != nil {
		return err
	}
	cliCert, cliKey, err := makeLeaf("cwc-client", []string{"cwc-client"})
	if err != nil {
		return err
	}

	type secTarget struct {
		ns   string
		name string
		data map[string][]byte
	}
	targets := []secTarget{
		{
			ns:   coreNS,
			name: bnkconst.SecretServerCert,
			data: map[string][]byte{"ca.crt": caPEM, "tls.crt": srvCert, "tls.key": srvKey},
		},
		{
			ns:   coreNS,
			name: bnkconst.SecretClientCert,
			data: map[string][]byte{"ca.crt": caPEM, "tls.crt": cliCert, "tls.key": cliKey},
		},
		{
			ns:   utilsNS,
			name: bnkconst.SecretClientCert,
			data: map[string][]byte{"ca.crt": caPEM, "tls.crt": cliCert, "tls.key": cliKey},
		},
	}
	if coreNS != bnkconst.NamespaceCneCore {
		targets = append(targets,
			secTarget{
				ns:   bnkconst.NamespaceCneCore,
				name: bnkconst.SecretServerCert,
				data: map[string][]byte{"ca.crt": caPEM, "tls.crt": srvCert, "tls.key": srvKey},
			},
			secTarget{
				ns:   bnkconst.NamespaceCneCore,
				name: bnkconst.SecretClientCert,
				data: map[string][]byte{"ca.crt": caPEM, "tls.crt": cliCert, "tls.key": cliKey},
			},
		)
	}

	for _, s := range targets {
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: s.ns},
			Type:       corev1.SecretTypeTLS,
			Data:       s.data,
		}
		if _, err := k8sClient.CoreV1().Secrets(s.ns).Create(ctx, sec, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

// PhaseCerts ensures CWC TLS secrets exist and applies the cert-manager CA chain and OTEL certs.
func PhaseCerts(ctx context.Context, k8sClient kubernetes.Interface, applyFn ApplyYAMLFunc, certChainYAML, otelCertsYAML []byte) error {
	if err := EnsureCWCCerts(ctx, k8sClient, bnkconst.InstanceNamespace, bnkconst.UtilsNamespace); err != nil {
		return fmt.Errorf("ensure CWC certs: %w", err)
	}
	if applyFn != nil {
		if len(bytes.TrimSpace(certChainYAML)) > 0 {
			if err := applyFn(ctx, certChainYAML); err != nil {
				return fmt.Errorf("apply cert chain: %w", err)
			}
		}
		if len(bytes.TrimSpace(otelCertsYAML)) > 0 {
			if err := applyFn(ctx, otelCertsYAML); err != nil {
				return fmt.Errorf("apply otel certs: %w", err)
			}
		}
	}
	return nil
}

// PhaseFLO installs or upgrades the F5 Lifecycle Operator Helm chart from oci://repo.f5.com/charts/f5-lifecycle-operator.
func PhaseFLO(ctx context.Context, kubeconfigPath, farKeyJSON, floChartVersion string, floValuesYAML []byte) error {
	if kubeconfigPath == "" || strings.TrimSpace(farKeyJSON) == "" {
		return nil
	}
	b64Key := ExtractFARPayload(farKeyJSON)
	installer, err := k8s.NewHelmInstaller(kubeconfigPath, b64Key)
	if err != nil {
		return fmt.Errorf("init helm installer: %w", err)
	}
	candidates := []string{
		floChartVersion,
		"v" + strings.TrimPrefix(floChartVersion, "v"),
		strings.TrimPrefix(floChartVersion, "v"),
		"v2.30.0-0.11.4",
	}
	ch, err := installer.PullAndLoad(k8s.FLOChartRef, candidates[0])
	if err != nil {
		for _, cand := range candidates[1:] {
			if cand == "" {
				continue
			}
			if ch2, err2 := installer.PullAndLoad(k8s.FLOChartRef, cand); err2 == nil {
				ch = ch2
				err = nil
				break
			}
		}
	}
	if err != nil {
		return fmt.Errorf("pull FLO chart: %w", err)
	}
	var vals map[string]any
	if err := yaml.Unmarshal(floValuesYAML, &vals); err != nil {
		return fmt.Errorf("parse FLO values: %w", err)
	}
	rels, _ := installer.List(k8s.FLONamespace, k8s.FLOReleaseName)
	if len(rels) > 0 {
		if _, err := installer.Upgrade(k8s.FLOReleaseName, k8s.FLONamespace, ch, vals); err != nil {
			return fmt.Errorf("FLO helm upgrade: %w", err)
		}
	} else {
		if _, err := installer.Install(k8s.FLOReleaseName, k8s.FLONamespace, ch, vals); err != nil {
			return fmt.Errorf("FLO helm install: %w", err)
		}
	}
	return nil
}

// PhaseCNEInstance applies NADs, optional EndpointSlice RBAC supplement, invokes the cloud
// controller identity hook, applies the CNEInstance CR, and reconciles null controller env vars.
func PhaseCNEInstance(
	ctx context.Context,
	k8sClient kubernetes.Interface,
	dynClient dynamic.Interface,
	applyFn ApplyYAMLFunc,
	nadsYAML, rbacYAML, cneInstanceYAML []byte,
	reconcileIdentity provider.HookFunc,
	recreateOnNull bool,
) error {
	if applyFn != nil {
		if len(bytes.TrimSpace(nadsYAML)) > 0 {
			if err := applyFn(ctx, nadsYAML); err != nil {
				return fmt.Errorf("apply NADs: %w", err)
			}
		}
		if len(bytes.TrimSpace(rbacYAML)) > 0 {
			if err := applyFn(ctx, rbacYAML); err != nil {
				return fmt.Errorf("apply RBAC supplement: %w", err)
			}
		}
	}
	if reconcileIdentity != nil {
		if err := reconcileIdentity(ctx, k8sClient, dynClient); err != nil {
			return fmt.Errorf("reconcile controller identity: %w", err)
		}
	}
	if applyFn != nil && len(bytes.TrimSpace(cneInstanceYAML)) > 0 {
		if err := applyFn(ctx, cneInstanceYAML); err != nil {
			return fmt.Errorf("apply CNEInstance: %w", err)
		}
	}
	if recreateOnNull {
		_ = ReconcileNullCNEController(ctx, k8sClient, dynClient)
	}
	return nil
}

// PurgeStaleCPCLSecrets deletes stale cpcl-key-secret in f5-cne-core and f5-cne-system before CWC restarts.
func PurgeStaleCPCLSecrets(ctx context.Context, k8sClient kubernetes.Interface) error {
	if k8sClient == nil {
		return nil
	}
	for _, ns := range []string{bnkconst.InstanceNamespace, bnkconst.NamespaceCneCore} {
		err := k8sClient.CoreV1().Secrets(ns).Delete(ctx, bnkconst.CPCLKeySecretName, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// SyncCPCLKeySecret fetches the JWKS from CWC's apiserver and writes cpcl-key-secret
// so TMM's f5-tmm-clid sidecar validates the JWT.
func SyncCPCLKeySecret(ctx context.Context, k8sClient kubernetes.Interface, jwksEndpoint string) error {
	if k8sClient == nil || jwksEndpoint == "" {
		return nil
	}
	tr := &http.Transport{
		// #nosec G402 -- internal cluster CWC endpoint uses self-signed cert
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksEndpoint, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bnkconst.CPCLKeySecretName,
			Namespace: bnkconst.InstanceNamespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"jwks.json": body,
		},
	}
	_, err = k8sClient.CoreV1().Secrets(bnkconst.InstanceNamespace).Create(ctx, sec, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = k8sClient.CoreV1().Secrets(bnkconst.InstanceNamespace).Update(ctx, sec, metav1.UpdateOptions{})
	}
	return err
}

// ApplyLicenseWithQuotaRetry applies the License CR YAML, clearing any blocking f5-single-license-quota
// ResourceQuota if creation is rejected by quota.
func ApplyLicenseWithQuotaRetry(ctx context.Context, k8sClient kubernetes.Interface, applyFn ApplyYAMLFunc, licenseYAML []byte) error {
	if applyFn == nil || len(bytes.TrimSpace(licenseYAML)) == 0 {
		return nil
	}
	err := applyFn(ctx, licenseYAML)
	if err == nil {
		return nil
	}
	if k8sClient != nil && strings.Contains(err.Error(), "f5-single-license-quota") {
		for _, ns := range []string{bnkconst.NamespaceCneCore, bnkconst.InstanceNamespace, bnkconst.UtilsNamespace} {
			_ = k8sClient.CoreV1().ResourceQuotas(ns).Delete(ctx, "f5-single-license-quota", metav1.DeleteOptions{})
		}
		return applyFn(ctx, licenseYAML)
	}
	return err
}

// PhaseLicense purges stale CPCL secrets, applies the License CRs (with f5-single-license-quota retry),
// and optionally syncs cpcl-key-secret from CWC's JWKS endpoint.
func PhaseLicense(ctx context.Context, k8sClient kubernetes.Interface, applyFn ApplyYAMLFunc, licenseYAML []byte, jwksEndpoint string) error {
	_ = PurgeStaleCPCLSecrets(ctx, k8sClient)
	if err := ApplyLicenseWithQuotaRetry(ctx, k8sClient, applyFn, licenseYAML); err != nil {
		return fmt.Errorf("apply License: %w", err)
	}
	if jwksEndpoint != "" {
		_ = SyncCPCLKeySecret(ctx, k8sClient, jwksEndpoint)
	}
	return nil
}

// PhaseNetworkAndEgress applies GatewayClass, Network CRs (Infra + GatewaySettings or 2.3 VLANs + Routes),
// EgressGateway (or F5SPKEgress), and any ExtraManifests.
func PhaseNetworkAndEgress(ctx context.Context, applyFn ApplyYAMLFunc, gatewayClassYAML, networkCRsYAML, egressYAML, extraYAML []byte) error {
	if applyFn == nil {
		return nil
	}
	for _, step := range []struct {
		name string
		yaml []byte
	}{
		{"GatewayClass", gatewayClassYAML},
		{"NetworkCRs", networkCRsYAML},
		{"Egress", egressYAML},
		{"ExtraManifests", extraYAML},
	} {
		if len(bytes.TrimSpace(step.yaml)) == 0 {
			continue
		}
		if err := applyFn(ctx, step.yaml); err != nil {
			return fmt.Errorf("apply %s: %w", step.name, err)
		}
	}
	return nil
}

// ReconcileNullCNEController deletes f5-cne-controller deployment if its env vars rendered
// as null before CNEInstance reconciliation (BNK 2.4 quirk).
func ReconcileNullCNEController(ctx context.Context, k8sClient kubernetes.Interface, dynClient dynamic.Interface) error {
	if k8sClient == nil {
		return nil
	}
	dep, err := k8sClient.AppsV1().Deployments(bnkconst.InstanceNamespace).Get(ctx, bnkconst.DeployCNEController, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	hasNull := false
	for _, c := range dep.Spec.Template.Spec.Containers {
		for _, env := range c.Env {
			if env.Value == "null" || env.Value == "<nil>" {
				hasNull = true
				break
			}
		}
	}
	if !hasNull {
		return nil
	}
	if err := k8sClient.AppsV1().Deployments(bnkconst.InstanceNamespace).Delete(ctx, bnkconst.DeployCNEController, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if dynClient != nil {
		gvr := schema.GroupVersionResource{Group: "k8s.f5.com", Version: "v1", Resource: "cneinstances"}
		list, err := dynClient.Resource(gvr).Namespace(bnkconst.InstanceNamespace).List(ctx, metav1.ListOptions{})
		if err == nil {
			for i := range list.Items {
				item := &list.Items[i]
				_ = unstructured.SetNestedField(item.Object, time.Now().UTC().Format(time.RFC3339), "metadata", "annotations", "bnkctl.f5.com/reconcile-at")
				_, _ = dynClient.Resource(gvr).Namespace(bnkconst.InstanceNamespace).Update(ctx, item, metav1.UpdateOptions{})
			}
		}
	}
	return nil
}

// ActivationStatus summarizes the readiness of CNEInstance conditions and TMM pods.
type ActivationStatus struct {
	CNEControllerAvailable bool
	F5TmmAvailable         bool
	ReadyTMMPods           int
	TotalTMMPods           int
}

// CheckCNEInstanceActivation inspects the CNEInstance CR conditions (CNEControllerAvailable,
// F5TmmAvailable) and counts Ready f5-tmm pods (including the f5-tmm-clid sidecar).
func CheckCNEInstanceActivation(ctx context.Context, k8sClient kubernetes.Interface, dynClient dynamic.Interface, cneInstanceName string) (*ActivationStatus, error) {
	st := &ActivationStatus{}
	if dynClient != nil {
		gvr := schema.GroupVersionResource{Group: "k8s.f5.com", Version: "v1", Resource: "cneinstances"}
		if cneInstanceName != "" {
			if obj, err := dynClient.Resource(gvr).Namespace(bnkconst.InstanceNamespace).Get(ctx, cneInstanceName, metav1.GetOptions{}); err == nil {
				st.CNEControllerAvailable = k8s.UnstructuredConditionIsTrue(obj.Object, "CNEControllerAvailable")
				st.F5TmmAvailable = k8s.UnstructuredConditionIsTrue(obj.Object, "F5TmmAvailable")
			}
		} else if list, err := dynClient.Resource(gvr).Namespace(bnkconst.InstanceNamespace).List(ctx, metav1.ListOptions{}); err == nil && len(list.Items) > 0 {
			obj := list.Items[0]
			st.CNEControllerAvailable = k8s.UnstructuredConditionIsTrue(obj.Object, "CNEControllerAvailable")
			st.F5TmmAvailable = k8s.UnstructuredConditionIsTrue(obj.Object, "F5TmmAvailable")
		}
	}
	if k8sClient != nil {
		pods, err := k8sClient.CoreV1().Pods(bnkconst.InstanceNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=f5-tmm",
		})
		if err == nil {
			st.TotalTMMPods = len(pods.Items)
			for _, pod := range pods.Items {
				if pod.Status.Phase != corev1.PodRunning {
					continue
				}
				allReady := len(pod.Status.ContainerStatuses) > 0
				for _, cs := range pod.Status.ContainerStatuses {
					if !cs.Ready {
						allReady = false
						break
					}
				}
				if allReady {
					st.ReadyTMMPods++
				}
			}
		}
	}
	return st, nil
}
