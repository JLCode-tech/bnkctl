// Package bnkengine implements the unified, cloud-agnostic BNK overlay engine.
//
// It consumes a canonical *provider.UnderlayContext and *release.ReleaseProfile
// to render, install, upgrade, migrate, self-heal, and uninstall BNK 2.3, 2.4,
// and 2.5+ across AWS, Azure, and GCP without any cloud SDK dependencies.
//
// Zero cloud SDK imports are permitted in this package.
package bnkengine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/JLCode-tech/bnkctl/pkg/bnk"
	"github.com/JLCode-tech/bnkctl/pkg/bnkconst"
	"github.com/JLCode-tech/bnkctl/pkg/bnkengine/phases"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/jwt"
	"github.com/JLCode-tech/bnkctl/pkg/k8s"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/manifests"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/migrate"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/render"
	"github.com/JLCode-tech/bnkctl/pkg/provider"
	"github.com/JLCode-tech/bnkctl/pkg/release"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// TEEMCertURL is the F5 telemetry public key endpoint required by License CR validation.
	TEEMCertURL = "https://product.apis.f5.com/ee/v1/keys/public/telemetry.pub"
	// CWCRestNamespace is the namespace where f5-spk-cwc runs.
	CWCRestNamespace = bnkconst.InstanceNamespace
	// CPCLKeySecretName is the secret name synced from CWC JWKS for TMM license validation.
	// #nosec G101 -- Kubernetes Secret resource name, not a credential
	CPCLKeySecretName = "cpcl-key-secret"
)

// Options configures BNK overlay engine operations.
type Options struct {
	DryRun         bool
	JWT            string
	FARKeyJSON     string
	TargetVersion  string
	MigrateInPlace bool
	Log            io.Writer
	Logf           func(format string, args ...any)
}

func (o *Options) logf(format string, args ...any) {
	if o == nil {
		return
	}
	if o.Logf != nil {
		o.Logf(format, args...)
		return
	}
	if o.Log != nil {
		_, _ = fmt.Fprintf(o.Log, format+"\n", args...)
	}
}

// RenderedOverlay holds all rendered YAML manifests for a BNK overlay deployment.
type RenderedOverlay struct {
	Cloud            string
	ReleaseVersion   string
	NetworkModel     string
	NamespacesYAML   []byte
	CertChainYAML    []byte
	OTELCertsYAML    []byte
	FLOValuesYAML    []byte
	NADsYAML         []byte
	RBACYAML         []byte
	CNEInstanceYAML  []byte
	LicenseYAML      []byte
	GatewayClassYAML []byte
	NetworkCRsYAML   []byte
	EgressYAML       []byte
	ExtraYAML        []byte
}

// AllManifests concatenates all Kubernetes manifests in deterministic apply order
// (excluding FLOValuesYAML, which is passed to Helm).
func (r *RenderedOverlay) AllManifests() []byte {
	var parts [][]byte
	for _, b := range [][]byte{
		r.NamespacesYAML,
		r.CertChainYAML,
		r.OTELCertsYAML,
		r.NADsYAML,
		r.RBACYAML,
		r.CNEInstanceYAML,
		r.LicenseYAML,
		r.GatewayClassYAML,
		r.NetworkCRsYAML,
		r.EgressYAML,
		r.ExtraYAML,
	} {
		trimmed := bytes.TrimSpace(b)
		if len(trimmed) > 0 {
			parts = append(parts, trimmed)
		}
	}
	return bytes.Join(parts, []byte("\n---\n"))
}

// Engine orchestrates the cloud-agnostic BNK overlay lifecycle.
type Engine struct {
	Cluster  *intent.Cluster
	State    *state.State
	Underlay *provider.UnderlayContext
	Profile  *release.ReleaseProfile
	K8s      kubernetes.Interface
	Dyn      dynamic.Interface
	Disc     discovery.DiscoveryInterface
	Applier  *k8s.Applier
}

// New constructs a BNK overlay Engine.
func New(cl *intent.Cluster, st *state.State, u *provider.UnderlayContext, prof *release.ReleaseProfile) (*Engine, error) {
	if cl == nil {
		return nil, fmt.Errorf("bnkengine: cluster intent is nil")
	}
	if u == nil {
		var err error
		u, err = provider.BuildUnderlayFromIntent(cl)
		if err != nil {
			return nil, fmt.Errorf("bnkengine: build underlay from intent: %w", err)
		}
	}
	if prof == nil {
		ver := cl.EffectiveBNKVersion()
		var err error
		prof, err = release.Resolve(context.Background(), ver, "", nil)
		if err != nil {
			return nil, fmt.Errorf("bnkengine: resolve release profile: %w", err)
		}
	}
	if cl.Bnk != nil && strings.TrimSpace(cl.Bnk.FLOVersion) != "" {
		prof.FLOChartVersion = strings.TrimSpace(cl.Bnk.FLOVersion)
	}
	return &Engine{
		Cluster:  cl,
		State:    st,
		Underlay: u,
		Profile:  prof,
	}, nil
}

func resolveFARKey(cl *intent.Cluster) string {
	if cl == nil || cl.Bnk == nil {
		return ""
	}
	for _, p := range []string{
		strings.TrimSpace(cl.Derived.FARArchiveFullPath),
		strings.TrimSpace(cl.Bnk.FARArchive),
	} {
		if p == "" {
			continue
		}
		if _, secret, err := bnk.ExtractFARAuth(p); err == nil && strings.TrimSpace(secret) != "" {
			return strings.TrimSpace(secret)
		}
	}
	if p := strings.TrimSpace(cl.Bnk.FarSecret.DockerConfigFile); p != "" {
		// #nosec G304 -- path is validated from cluster intent
		if raw, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(raw)) > 0 {
			return strings.TrimSpace(string(raw))
		}
	}
	return ""
}

func effectiveTestNamespace(cl *intent.Cluster) string {
	if cl == nil {
		return "default"
	}
	if cl.Egress.TestNamespace != "" {
		return cl.Egress.TestNamespace
	}
	if cl.Egress.Namespace != "" {
		return cl.Egress.Namespace
	}
	return "default"
}

func setClusterManifestVersion(cl *intent.Cluster, ver string) {
	if cl == nil {
		return
	}
	if cl.Bnk == nil {
		cl.Bnk = &intent.BnkSpec{}
	}
	cl.Bnk.ManifestVersion = ver
}

// RenderAll deterministically renders the entire BNK overlay manifest bundle for the
// given Cluster intent, UnderlayContext, and ReleaseProfile.
func RenderAll(cl *intent.Cluster, u *provider.UnderlayContext, prof *release.ReleaseProfile, jwtToken string) (*RenderedOverlay, error) {
	if cl == nil {
		return nil, fmt.Errorf("render overlay: cluster intent is nil")
	}
	if u == nil {
		var err error
		u, err = provider.BuildUnderlayFromIntent(cl)
		if err != nil {
			return nil, err
		}
	}
	if prof == nil {
		ver := cl.EffectiveBNKVersion()
		var err error
		prof, err = release.Resolve(context.Background(), ver, "", nil)
		if err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(jwtToken) == "" {
		// #nosec G101 -- synthetic placeholder JWT for offline dry-run rendering
		jwtToken = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.dryrun.signature"
	}

	out := &RenderedOverlay{
		Cloud:          u.CloudName,
		ReleaseVersion: prof.Version,
		NetworkModel:   prof.NetworkModel,
	}

	// 1. Namespaces
	testNS := effectiveTestNamespace(cl)
	nsYAML, err := renderNamespacesYAML(testNS)
	if err != nil {
		return nil, err
	}
	out.NamespacesYAML = nsYAML

	// 2. Cert Chain
	certTmpl, err := manifests.FS.ReadFile("shared/bnk-cert-chain.yaml")
	if err != nil {
		return nil, fmt.Errorf("read shared/bnk-cert-chain.yaml: %w", err)
	}
	certYAML, err := render.RenderCertChain(certTmpl, cl)
	if err != nil {
		return nil, err
	}
	out.CertChainYAML = certYAML

	// 3. OTEL Certs
	otelTmpl, err := manifests.FS.ReadFile("shared/otel-certs.yaml")
	if err != nil {
		return nil, fmt.Errorf("read shared/otel-certs.yaml: %w", err)
	}
	otelYAML, err := render.RenderOTELCerts(otelTmpl, cl)
	if err != nil {
		return nil, err
	}
	out.OTELCertsYAML = otelYAML

	// 4. FLO Helm Values (with pass-through overrides)
	floTmpl, err := manifests.FS.ReadFile(k8s.FLOValuesTemplatePath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", k8s.FLOValuesTemplatePath, err)
	}
	floYAML, err := render.RenderFLOValues(floTmpl, cl, jwtToken)
	if err != nil {
		return nil, err
	}
	if cl.Bnk != nil && cl.Bnk.AIGateway != nil && (cl.Bnk.AIGateway.Enabled || cl.Bnk.AIGateway.DSSMSentinel) {
		floYAML, err = mergeYAMLMap(floYAML, map[string]any{
			"dssm": map[string]any{
				"sentinel": map[string]any{
					"enabled": true,
				},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("merge dssm.sentinel.enabled into floValues: %w", err)
		}
	}
	if cl.Bnk != nil && len(cl.Bnk.FLOValues) > 0 {
		floYAML, err = mergeYAMLMap(floYAML, cl.Bnk.FLOValues)
		if err != nil {
			return nil, fmt.Errorf("merge bnk.floValues overrides: %w", err)
		}
	}
	out.FLOValuesYAML = floYAML

	// 5. NADs from UnderlayContext.Networks
	nadsYAML, err := renderUnifiedNADs(u)
	if err != nil {
		return nil, err
	}
	out.NADsYAML = nadsYAML

	// 6. Optional EndpointSlice RBAC supplement (BNK 2.4 quirk)
	if prof.Quirks.EndpointSliceGetRBAC {
		rbacTmpl, err := manifests.FS.ReadFile("shared/cne-controller-rbac.yaml.tmpl")
		if err != nil {
			return nil, fmt.Errorf("read shared/cne-controller-rbac.yaml.tmpl: %w", err)
		}
		rbacYAML, err := render.RenderCNEControllerRBAC(rbacTmpl, cl)
		if err != nil {
			return nil, err
		}
		out.RBACYAML = rbacYAML
	}

	// 7. CNEInstance CR
	cneYAML, err := renderUnifiedCNEInstance(cl, u, prof)
	if err != nil {
		return nil, err
	}
	out.CNEInstanceYAML = cneYAML

	// 8. License CR (both f5-cne-core and f5-utils with teemCertUrl)
	licYAML, err := renderUnifiedLicenseCR(cl, jwtToken)
	if err != nil {
		return nil, err
	}
	out.LicenseYAML = licYAML

	// 9. GatewayClass
	gwcYAML, err := renderUnifiedGatewayClass(cl)
	if err != nil {
		return nil, err
	}
	out.GatewayClassYAML = gwcYAML

	// 10. Network CRs (2.3 legacy-spk vs 2.4+ gateway-infra)
	netYAML, egressYAML, err := renderUnifiedNetworkAndEgress(cl, u, prof)
	if err != nil {
		return nil, err
	}
	out.NetworkCRsYAML = netYAML
	out.EgressYAML = egressYAML

	// 11. Extra Manifests pass-through
	if cl.Bnk != nil && len(cl.Bnk.ExtraManifests) > 0 {
		var extraDocs []string
		for _, m := range cl.Bnk.ExtraManifests {
			trimmed := strings.TrimSpace(m)
			if trimmed != "" {
				extraDocs = append(extraDocs, trimmed)
			}
		}
		if len(extraDocs) > 0 {
			out.ExtraYAML = []byte(strings.Join(extraDocs, "\n---\n") + "\n")
		}
	}

	return out, nil
}

// Install executes the full BNK overlay installation against the target cluster
// (or renders and validates all manifests when opts.DryRun is true).
func (e *Engine) Install(ctx context.Context, opts Options) (*RenderedOverlay, error) {
	if strings.TrimSpace(opts.JWT) == "" && e.Cluster != nil {
		jwtPath := strings.TrimSpace(e.Cluster.Derived.JWTFullPath)
		if jwtPath == "" {
			jwtPath = strings.TrimSpace(e.Cluster.Bnk.EffectiveJWTPath())
		}
		if jwtPath != "" {
			// #nosec G304 -- jwtPath is validated from cluster intent
			if raw, err := os.ReadFile(jwtPath); err == nil {
				opts.JWT = strings.TrimSpace(string(raw))
			}
		}
	}
	if strings.TrimSpace(opts.FARKeyJSON) == "" && e.Cluster != nil {
		opts.FARKeyJSON = resolveFARKey(e.Cluster)
	}
	if strings.TrimSpace(opts.JWT) != "" {
		var warnBuf bytes.Buffer
		_, _ = jwt.CheckPreflight(opts.JWT, &warnBuf)
		if warnBuf.Len() > 0 {
			opts.logf("%s", strings.TrimSpace(warnBuf.String()))
		}
	}
	rendered, err := RenderAll(e.Cluster, e.Underlay, e.Profile, opts.JWT)
	if err != nil {
		return nil, err
	}
	if opts.DryRun {
		opts.logf("[dry-run] rendered BNK %s overlay for cloud=%s model=%s",
			e.Profile.Version, e.Underlay.CloudName, e.Profile.NetworkModel)
		return rendered, nil
	}

	if err := e.ensureClients(); err != nil {
		return nil, err
	}

	// Probe live cluster API versions, quirks, and dataplane resources if clients are available
	if e.Underlay != nil && e.Underlay.DiscoverDataplaneResources != nil && e.K8s != nil {
		_ = e.Underlay.DiscoverDataplaneResources(ctx, e.K8s, e.Dyn)
	}
	if e.Disc != nil {
		release.DiscoverServedAPIVersions(e.Disc, e.Profile)
	}
	if e.K8s != nil {
		release.ProbeLiveQuirks(ctx, e.K8s, e.Profile)
	}
	// Re-render with any updated discovery/quirk state
	rendered, err = RenderAll(e.Cluster, e.Underlay, e.Profile, opts.JWT)
	if err != nil {
		return nil, err
	}

	// Phase 1: Namespaces
	if err := phases.PhaseNamespaces(ctx, e.K8s, e.applyYAML, rendered.NamespacesYAML, effectiveTestNamespace(e.Cluster)); err != nil {
		return nil, err
	}

	// Phase 2: Cert-Manager & FAR pull secrets & Multus
	if err := phases.PhaseCertManagerAndMultus(ctx, e.K8s, e.Underlay.KubeconfigPath, opts.FARKeyJSON); err != nil {
		return nil, err
	}
	if err := e.ensureMultusInstalled(ctx, opts); err != nil {
		opts.logf("warning: ensure Multus CNI: %v", err)
	}

	// Phase 3: CWC Certs, CertChain & OTEL Certs
	if err := phases.PhaseCerts(ctx, e.K8s, e.applyYAML, rendered.CertChainYAML, rendered.OTELCertsYAML); err != nil {
		return nil, err
	}

	// Phase 4: Install / Upgrade FLO via Helm v3 SDK
	if err := phases.PhaseFLO(ctx, e.Underlay.KubeconfigPath, opts.FARKeyJSON, e.Profile.FLOChartVersion, rendered.FLOValuesYAML); err != nil {
		return nil, err
	}

	// Phase 4b: Ensure Gateway API & GIE CRDs are installed before CNEInstance.
	// Note: BNK's own gateway.k8s.f5.com and inference.k8s.f5.com CRDs are installed by
	// FLO's crd-installer when CNEInstance reconciles in Phase 5; pre-creating them with a
	// different storage version breaks crd-installer on BNK 2.4.1.
	for _, crdFile := range []string{
		"static/00-gateway-api-crds.yaml",
		"static/01-gie-crds.yaml",
	} {
		if crdBytes, rErr := manifests.FS.ReadFile(crdFile); rErr == nil && len(bytes.TrimSpace(crdBytes)) > 0 {
			if aErr := e.applyYAML(ctx, crdBytes); aErr != nil {
				opts.logf("warning: apply %s: %v", crdFile, aErr)
			}
		}
	}

	// Phase 5: NADs, RBAC supplement, Controller Identity Hook, CNEInstance
	if err := phases.PhaseCNEInstance(
		ctx,
		e.K8s,
		e.Dyn,
		e.applyYAML,
		rendered.NADsYAML,
		rendered.RBACYAML,
		rendered.CNEInstanceYAML,
		e.Underlay.ReconcileControllerIdentity,
		e.Profile.Quirks.RecreateCNEControllerOnNull,
	); err != nil {
		return nil, err
	}

	// Wait up to 180s for FLO crd-installer to establish licenses.k8s.f5net.com before applying License CRs
	if e.Dyn != nil {
		_ = k8s.WaitForCRDEstablished(ctx, e.Dyn, "licenses.k8s.f5net.com", 180*time.Second)
		// If infras.gateway.k8s.f5.com was omitted by an older 2.4.0 pre-release crd-installer, apply supplemental CRDs
		if err := k8s.WaitForCRDEstablished(ctx, e.Dyn, "infras.gateway.k8s.f5.com", 5*time.Second); err != nil {
			if crdBytes, rErr := manifests.FS.ReadFile("static/02-bnk-gateway-crds.yaml"); rErr == nil && len(bytes.TrimSpace(crdBytes)) > 0 {
				_ = e.applyYAML(ctx, crdBytes)
			}
		}
	}

	// Phase 6: License CRs (f5-cne-core, f5-cne-system, f5-utils) & CPCL JWKS Sync
	if err := phases.PhaseLicense(ctx, e.K8s, e.applyYAML, rendered.LicenseYAML, ""); err != nil {
		return nil, err
	}

	// Phase 7: GatewayClass, Network CRs (Infra + GatewaySettings or 2.3 VLANs + Routes), Egress, ExtraManifests
	if err := phases.PhaseNetworkAndEgress(
		ctx,
		e.applyYAML,
		rendered.GatewayClassYAML,
		rendered.NetworkCRsYAML,
		rendered.EgressYAML,
		rendered.ExtraYAML,
	); err != nil {
		return nil, err
	}

	// Phase 8: Self-Healing pass
	if _, err := RunHeal(ctx, e.K8s, e.Dyn, HealOptions{
		Cluster:  e.Cluster,
		Underlay: e.Underlay,
		Profile:  e.Profile,
	}); err != nil {
		opts.logf("warning: post-install heal reported error: %v", err)
	}

	// Phase 9: Activation readiness check & cloud PostActivationHook
	if _, err := phases.CheckCNEInstanceActivation(ctx, e.K8s, e.Dyn, e.Cluster.EffectiveName()+"-bnk"); err != nil {
		opts.logf("warning: activation check reported error: %v", err)
	}
	if e.Underlay.PostActivationHook != nil {
		if err := e.Underlay.PostActivationHook(ctx, e.K8s, e.Dyn); err != nil {
			return nil, fmt.Errorf("post-activation hook: %w", err)
		}
	}

	if e.State != nil {
		e.State.Set("BNK_MANIFEST_VERSION", e.Profile.Version)
		e.State.Set("BNK_NETWORK_MODEL", e.Profile.NetworkModel)
	}

	return rendered, nil
}

// OverlayStateKeys lists all state.env keys written by BNK overlay installation
// (including legacy bash awsbnkctl/azrbnkctl/gkebnkctl overlay keys).
var OverlayStateKeys = []string{
	"BNK_MANIFEST_VERSION",
	"BNK_NETWORK_MODEL",
	"CERT_MANAGER_INSTALLED",
	"MULTUS_INSTALLED",
	"FLO_INSTALLED",
	"CNE_DEPLOYED",
	"BNK_DEPLOYED",
	"BNK_PREREQS_APPLIED",
	"CERT_MANAGER_VERSION",
	"MULTUS_VERSION",
	"OPERATOR_NS",
	"INSTANCE_NS",
	"BNK_NAMESPACES_CREATED",
	"BNK_FAR_SECRET_NAME",
	"BNK_LICENSE_JWT_SECRET",
	"CNE_SA_NAME",
	"BNK_SELFSIGNED_ISSUER",
	"BNK_CA_CERT_NAME",
	"BNK_CA_SECRET_NAME",
	"BNK_CA_ISSUER",
	"OTEL_SVR_CERT_NAME",
	"OTEL_F5ING_CERT_NAME",
	"FLO_VERSION",
	"FLO_RELEASE_NAME",
	"FLO_NAMESPACE",
	"FLO_INSTALLED_AT",
	"LICENSE_CRD_READY_AT",
	"LICENSE_NAME",
	"LICENSE_APPLIED_AT",
	"BNK_API_GENERATION",
	"NADS_APPLIED_AT",
	"INFRA_APPLIED_AT",
	"CNEINSTANCE_NAME",
	"CNEINSTANCE_APPLIED_AT",
	"CNEINSTANCE_RECONCILE_STARTED_AT",
	"CNEINSTANCE_READY_AT",
	"DSSM_INSECURE_OVERLAY_APPLIED_AT",
	"TMM_LOG_STREAM_ENABLED_AT",
	"GATEWAYCLASS_NAME",
	"EXTERNAL_NAD",
	"INTERNAL_NAD",
	"TMM_EXT_SELFIP",
	"TMM_INT_SELFIP",
	"TMM_SELFIP_PREFIXLEN",
}

// ClearStateKeys removes all BNK overlay state keys from st.
func ClearStateKeys(st *state.State) {
	if st == nil {
		return
	}
	for _, k := range OverlayStateKeys {
		st.Delete(k)
	}
}

// Uninstall reverses BNK overlay resources in reverse dependency order.
func (e *Engine) Uninstall(ctx context.Context, opts Options) error {
	if strings.TrimSpace(opts.FARKeyJSON) == "" && e.Cluster != nil {
		opts.FARKeyJSON = resolveFARKey(e.Cluster)
	}
	_, err := RenderAll(e.Cluster, e.Underlay, e.Profile, opts.JWT)
	if err != nil {
		return err
	}
	if opts.DryRun {
		opts.logf("[dry-run] would uninstall BNK %s overlay on cloud=%s", e.Profile.Version, e.Underlay.CloudName)
		return nil
	}
	if err := e.ensureClients(); err != nil {
		opts.logf("[uninstall] k8s client unavailable (%v); skipping in-cluster BNK overlay cleanup", err)
		if e.State != nil {
			ClearStateKeys(e.State)
			if e.State.Path() != "" {
				_ = e.State.Save()
			}
		}
		return nil
	}
	if e.Dyn != nil {
		e.cleanupBNKOverlayCRs(ctx)
	}
	if e.Underlay.KubeconfigPath != "" {
		farPayload := phases.ExtractFARPayload(opts.FARKeyJSON)
		if installer, err := k8s.NewHelmInstaller(e.Underlay.KubeconfigPath, farPayload); err == nil {
			_ = installer.Uninstall(k8s.FLOReleaseName, k8s.FLONamespace)
			_ = installer.Uninstall("flo", k8s.FLONamespace)
		}
	}
	if e.K8s != nil {
		e.cleanupBNKPVCsAndServices(ctx)
	}
	if e.State != nil {
		ClearStateKeys(e.State)
		if e.State.Path() != "" {
			_ = e.State.Save()
		}
	}
	return nil
}

func bnkOverlayCleanupGVRs() []schema.GroupVersionResource {
	return []schema.GroupVersionResource{
		{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "airoutepolicies"},
		{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "airoutes"},
		{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aigateways"},
		{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aigatewayprofiles"},
		{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aimodels"},
		{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aiproviders"},
		{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aicredentials"},
		{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aitenants"},
		{Group: "fic.f5.com", Version: "v1alpha1", Resource: "l4routes"},
		{Group: "fic.f5.com", Version: "v1alpha1", Resource: "egressgateways"},
		{Group: "fic.f5.com", Version: "v1alpha1", Resource: "gatewaysettings"},
		{Group: "fic.f5.com", Version: "v1alpha1", Resource: "infras"},
		{Group: "fic.f5.com", Version: "v1", Resource: "l4routes"},
		{Group: "fic.f5.com", Version: "v1", Resource: "egressgateways"},
		{Group: "fic.f5.com", Version: "v1", Resource: "gatewaysettings"},
		{Group: "fic.f5.com", Version: "v1", Resource: "infras"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "l4globalpolicies"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "apppolicies"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "l4routes"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "egressgateways"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "gatewaysettings"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "infras"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "l4routes"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "egressgateways"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "gatewaysettings"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "infras"},
		{Group: "gateway.k8s.f5net.com", Version: "v1", Resource: "l4routes"},
		{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"},
		{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "grpcroutes"},
		{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"},
		{Group: "gateway.k8s.f5net.com", Version: "v1", Resource: "gatewaysettings"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "egressgateways"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "infras"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-big-dns-caches"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-big-persistence-profiles"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-spk-egress"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-spk-egresses"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-spk-snatpools"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-spk-staticroutes"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-spk-vlans"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "licenses"},
		{Group: "k8s.f5.com", Version: "v1", Resource: "f5spkvlan"},
		{Group: "k8s.f5.com", Version: "v1", Resource: "f5spkvlans"},
		{Group: "k8s.f5.com", Version: "v1", Resource: "f5spkstaticroutes"},
		{Group: "k8s.f5.com", Version: "v1", Resource: "f5spksnatpools"},
		{Group: "k8s.f5.com", Version: "v1", Resource: "f5spkegresses"},
		{Group: "k8s.f5.com", Version: "v1", Resource: "cneinstances"},
	}
}

func (e *Engine) cleanupBNKOverlayCRs(ctx context.Context) {
	gvrs := bnkOverlayCleanupGVRs()
	patchBytes, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"finalizers": nil,
		},
	})
	namespaces := []string{
		bnkconst.InstanceNamespace,
		bnkconst.CoreNamespace,
		bnkconst.UtilsNamespace,
		"f5-bnk",
		"aigateway",
		"default",
	}
	for _, ns := range namespaces {
		for _, gvr := range gvrs {
			list, err := e.Dyn.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
			if err != nil || list == nil {
				continue
			}
			for i := range list.Items {
				item := &list.Items[i]
				if len(item.GetFinalizers()) > 0 {
					_, _ = e.Dyn.Resource(gvr).Namespace(ns).Patch(
						ctx, item.GetName(), types.MergePatchType, patchBytes, metav1.PatchOptions{},
					)
					item.SetFinalizers(nil)
					_, _ = e.Dyn.Resource(gvr).Namespace(ns).Update(ctx, item, metav1.UpdateOptions{})
				}
				_ = e.Dyn.Resource(gvr).Namespace(ns).Delete(ctx, item.GetName(), metav1.DeleteOptions{})
			}
		}
	}
	// Clean up cluster-scoped AITenant resources
	tenantGVR := schema.GroupVersionResource{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aitenants"}
	if list, err := e.Dyn.Resource(tenantGVR).List(ctx, metav1.ListOptions{}); err == nil && list != nil {
		for i := range list.Items {
			item := &list.Items[i]
			if len(item.GetFinalizers()) > 0 {
				_, _ = e.Dyn.Resource(tenantGVR).Patch(
					ctx, item.GetName(), types.MergePatchType, patchBytes, metav1.PatchOptions{},
				)
				item.SetFinalizers(nil)
				_, _ = e.Dyn.Resource(tenantGVR).Update(ctx, item, metav1.UpdateOptions{})
			}
			_ = e.Dyn.Resource(tenantGVR).Delete(ctx, item.GetName(), metav1.DeleteOptions{})
		}
	}
}

func (e *Engine) cleanupBNKPVCsAndServices(ctx context.Context) {
	if e.K8s == nil {
		return
	}
	patchBytes, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"finalizers": nil,
		},
	})
	namespaces := []string{
		bnkconst.InstanceNamespace,
		bnkconst.CoreNamespace,
		bnkconst.UtilsNamespace,
		"f5-bnk",
		"aigateway",
	}
	for _, ns := range namespaces {
		if svcs, err := e.K8s.CoreV1().Services(ns).List(ctx, metav1.ListOptions{}); err == nil && svcs != nil {
			for i := range svcs.Items {
				svc := &svcs.Items[i]
				if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
					_ = e.K8s.CoreV1().Services(ns).Delete(ctx, svc.Name, metav1.DeleteOptions{})
				}
			}
		}
		if pvcs, err := e.K8s.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{}); err == nil && pvcs != nil {
			for i := range pvcs.Items {
				pvc := &pvcs.Items[i]
				_ = e.K8s.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, pvc.Name, metav1.DeleteOptions{})
				if len(pvc.Finalizers) > 0 {
					_, _ = e.K8s.CoreV1().PersistentVolumeClaims(ns).Patch(
						ctx, pvc.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{},
					)
				}
			}
		}
	}
}

// Upgrade upgrades the BNK overlay from the current version to opts.TargetVersion
// (or e.Profile.Version if TargetVersion is empty), optionally performing 2.3 -> 2.4+
// CRD topology migration when transitioning across network models.
func (e *Engine) Upgrade(ctx context.Context, opts Options) (*RenderedOverlay, error) {
	targetVer := opts.TargetVersion
	if strings.TrimSpace(targetVer) == "" {
		targetVer = e.Profile.Version
	}
	targetProf, err := release.Resolve(ctx, targetVer, opts.FARKeyJSON, e.Disc)
	if err != nil {
		return nil, fmt.Errorf("resolve target release %q: %w", targetVer, err)
	}

	setClusterManifestVersion(e.Cluster, targetProf.Version)
	e.Profile = targetProf

	if opts.MigrateInPlace && targetProf.NetworkModel == release.NetworkModelGatewayInfra {
		if _, err := e.Migrate(ctx, opts); err != nil {
			return nil, fmt.Errorf("migrate topology before upgrade: %w", err)
		}
	}

	return e.Install(ctx, opts)
}

// Migrate translates a BNK 2.3 legacy-spk topology (F5SPKVlan + F5SPKStaticRoute + GatewayConfig)
// into a BNK 2.4+ gateway-infra topology (Infra + GatewaySettings + L4Route).
func (e *Engine) Migrate(ctx context.Context, opts Options) (*RenderedOverlay, error) {
	targetVer := opts.TargetVersion
	if strings.TrimSpace(targetVer) == "" {
		targetVer = "2.4.0"
	}
	targetProf, err := release.Resolve(ctx, targetVer, opts.FARKeyJSON, e.Disc)
	if err != nil {
		return nil, err
	}
	setClusterManifestVersion(e.Cluster, targetProf.Version)
	e.Profile = targetProf

	rendered, err := RenderAll(e.Cluster, e.Underlay, targetProf, opts.JWT)
	if err != nil {
		return nil, err
	}
	if opts.DryRun {
		opts.logf("[dry-run] rendered migrated %s topology for BNK %s", targetProf.NetworkModel, targetProf.Version)
		return rendered, nil
	}

	if err := e.ensureClients(); err != nil {
		return nil, err
	}
	if e.Dyn != nil {
		if inv, err := migrate.Inspect(ctx, e.Dyn, bnkconst.InstanceNamespace); err == nil && inv != nil && len(inv.Vlans) > 0 {
			if plan, err := migrate.Translate(inv, migrate.Options{
				InfraName:               render.InfraName,
				TargetVersion:           targetProf.Version,
				ServedGatewayAPIVersion: targetProf.ServedGatewayAPIVersion,
			}); err == nil && plan != nil && plan.Infra != nil {
				if raw, err := yaml.Marshal(plan.Infra.Object); err == nil {
					rendered.NetworkCRsYAML = raw
				}
			}
		}
	}
	if err := e.applyYAML(ctx, rendered.NetworkCRsYAML); err != nil {
		return nil, fmt.Errorf("apply migrated network CRs: %w", err)
	}
	if len(rendered.EgressYAML) > 0 {
		if err := e.applyYAML(ctx, rendered.EgressYAML); err != nil {
			return nil, fmt.Errorf("apply migrated egress CRs: %w", err)
		}
	}
	return rendered, nil
}

func (e *Engine) ensureClients() error {
	if e.K8s != nil && e.Dyn != nil {
		return nil
	}
	if len(e.Underlay.Kubeconfig) == 0 && e.Underlay.KubeconfigPath != "" {
		raw, err := os.ReadFile(e.Underlay.KubeconfigPath)
		if err != nil {
			return fmt.Errorf("read kubeconfig %s: %w", e.Underlay.KubeconfigPath, err)
		}
		e.Underlay.Kubeconfig = raw
	}
	if len(e.Underlay.Kubeconfig) == 0 {
		return fmt.Errorf("bnkengine: UnderlayContext.Kubeconfig is empty")
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(e.Underlay.Kubeconfig)
	if err != nil {
		return fmt.Errorf("parse kubeconfig: %w", err)
	}
	cs, err := k8s.NewClientsetFromConfig(cfg)
	if err != nil {
		return err
	}
	dc, err := k8s.NewDynamicFromConfig(cfg)
	if err != nil {
		return err
	}
	applier, err := k8s.NewApplier(cfg)
	if err != nil {
		return err
	}
	e.K8s = cs
	e.Dyn = dc
	e.Disc = cs.Discovery()
	e.Applier = applier
	return nil
}

func (e *Engine) applyYAML(ctx context.Context, raw []byte) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if e.Applier == nil {
		return fmt.Errorf("applyYAML: applier is nil")
	}
	objs, err := k8s.ParseYAMLStream(raw)
	if err != nil {
		return err
	}
	return e.Applier.ApplyAll(ctx, objs, true)
}

// ─── Unified Manifest Renderers ──────────────────────────────────────────────

func renderNamespacesYAML(testNS string) ([]byte, error) {
	if strings.TrimSpace(testNS) == "" {
		testNS = "default"
	}
	nsList := []string{
		bnkconst.CoreNamespace,
		bnkconst.InstanceNamespace,
		"f5-bnk-instance",
		bnkconst.UtilsNamespace,
		"f5-maintenance",
		"f5-app",
		"aigateway",
		"cert-manager",
		"bnk-demo",
	}
	hasTestNS := testNS == "default"
	for _, existing := range nsList {
		if existing == testNS {
			hasTestNS = true
			break
		}
	}
	if !hasTestNS {
		nsList = append(nsList, testNS)
	}
	var docs []string
	for _, ns := range nsList {
		doc := fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", ns)
		docs = append(docs, doc)
	}
	return []byte(strings.Join(docs, "---\n")), nil
}

func renderUnifiedNADs(u *provider.UnderlayContext) ([]byte, error) {
	if u == nil || len(u.Networks) == 0 {
		return nil, fmt.Errorf("render NADs: underlay has no networks")
	}
	var docs []string
	for _, netSpec := range u.Networks {
		cniJSON := strings.TrimSpace(netSpec.CNIConfigJSON)
		if cniJSON == "" {
			cniType := "macvlan"
			if u.DataplaneDriver == provider.DriverDPDKPCI {
				cniType = "host-device"
			}
			raw, _ := json.Marshal(map[string]any{
				"cniVersion": "0.3.1",
				"name":       netSpec.Name,
				"type":       cniType,
			})
			cniJSON = string(raw)
		}
		obj := map[string]any{
			"apiVersion": "k8s.cni.cncf.io/v1",
			"kind":       "NetworkAttachmentDefinition",
			"metadata": map[string]any{
				"name":      netSpec.Name,
				"namespace": bnkconst.InstanceNamespace,
			},
			"spec": map[string]any{
				"config": cniJSON,
			},
		}
		b, err := yaml.Marshal(obj)
		if err != nil {
			return nil, fmt.Errorf("marshal NAD %s: %w", netSpec.Name, err)
		}
		docs = append(docs, strings.TrimSpace(string(b)))
	}
	return []byte(strings.Join(docs, "\n---\n") + "\n"), nil
}

func (e *Engine) ensureMultusInstalled(ctx context.Context, opts Options) error {
	if e.K8s == nil {
		return nil
	}
	if crdBytes, err := manifests.FS.ReadFile("multus/multus-crd.yaml"); err == nil && len(bytes.TrimSpace(crdBytes)) > 0 {
		_ = e.applyYAML(ctx, crdBytes)
	}
	if ds, err := e.K8s.AppsV1().DaemonSets("kube-system").Get(ctx, "kube-multus-ds", metav1.GetOptions{}); err == nil && ds.Status.DesiredNumberScheduled > 0 {
		return nil
	}
	var files []string
	if e.Underlay != nil && e.Underlay.DataplaneDriver == provider.DriverVirtio {
		files = []string{
			"multus/02-multus-crd.yaml",
			"multus/02-multus-rbac.yaml",
			"multus/02-multus-configmap.yaml",
			"multus/02-multus-ds.yaml",
		}
	} else {
		files = []string{
			"multus/multus-daemonset-v4.2.4.yaml",
		}
	}
	for _, f := range files {
		raw, err := manifests.FS.ReadFile(f)
		if err != nil || len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		if err := e.applyYAML(ctx, raw); err != nil {
			opts.logf("warning: apply %s: %v", f, err)
		}
	}
	return nil
}

func normalizeDeploymentSize(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "tiny":
		return "Tiny"
	case "small":
		return "Small"
	case "medium":
		return "Medium"
	case "large":
		return "Large"
	case "max", "xlarge", "xxlarge":
		return "Max"
	default:
		return "Small"
	}
}

func renderUnifiedCNEInstance(cl *intent.Cluster, u *provider.UnderlayContext, prof *release.ReleaseProfile) ([]byte, error) {
	depSize := "Small"
	storageClass := u.StorageClass
	if storageClass == "" {
		storageClass = "standard-rwo"
	}
	mtu := 1500
	if ext := u.ExternalNetwork(); ext != nil && ext.MTU > 0 {
		mtu = ext.MTU
	}
	tmmCPU := "2"
	tmmMem := "8Gi"
	tmmHuge := "2Gi"
	palCPUSet := "0-1"
	imageReg := "repo.f5.com"
	if cl.Bnk != nil {
		if cl.Bnk.DeploymentSize != "" {
			depSize = normalizeDeploymentSize(cl.Bnk.DeploymentSize)
		}
		if cl.Bnk.StorageClassName != "" {
			storageClass = cl.Bnk.StorageClassName
		}
		if cl.Bnk.TmmMtuInt() > 0 {
			mtu = cl.Bnk.TmmMtuInt()
		}
		if cl.Bnk.TmmCpu != "" {
			tmmCPU = cl.Bnk.TmmCpu
		}
		if cl.Bnk.TmmMemory != "" {
			tmmMem = cl.Bnk.TmmMemory
		}
		if cl.Bnk.TmmHugepages != "" {
			tmmHuge = cl.Bnk.TmmHugepages
		}
		if cl.Bnk.PalCpuSet != "" {
			palCPUSet = cl.Bnk.PalCpuSet
		}
		if cl.Bnk.ImageRegistry != "" {
			imageReg = cl.Bnk.ImageRegistry
		}
	}
	if u.DataplaneDriver == provider.DriverDPDKPCI && mtu > 1500 {
		mtu = 1500
	}

	var nadNames []string
	var tmmIfaces []string
	for _, n := range u.Networks {
		nadNames = append(nadNames, n.Name)
		if n.TMMInterface != "" {
			tmmIfaces = append(tmmIfaces, n.TMMInterface)
		}
	}
	if len(tmmIfaces) == 0 {
		if u.DataplaneDriver == provider.DriverDPDKPCI || u.DataplaneDriver == provider.DriverVirtio {
			tmmIfaces = []string{"1.1"}
		} else {
			tmmIfaces = []string{"0.2"}
		}
	}

	// Build TMM environment variables
	tmmEnvMap := map[string]string{
		"TMM_DEFAULT_MTU":   fmt.Sprintf("%d", mtu),
		"ZEBOS_STATE":       "legacy",
		"TMM_SOCAT_DISABLE": "1",
	}
	if u.DataplaneDriver != provider.DriverVirtio {
		tmmEnvMap["TMM_MTU"] = fmt.Sprintf("%d", mtu)
		tmmEnvMap["TMM_INTERFACES"] = strings.Join(tmmIfaces, ",")
		tmmEnvMap["PAL_CPU_SET"] = palCPUSet
		tmmEnvMap["TMM_CALICO_ROUTER"] = "default"
		var k8sRoutes []string
		if u.ServiceCIDR != "" {
			k8sRoutes = append(k8sRoutes, u.ServiceCIDR)
		}
		if u.PodCIDR != "" {
			k8sRoutes = append(k8sRoutes, u.PodCIDR)
		}
		if len(k8sRoutes) > 0 {
			tmmEnvMap[render.TMMK8sRoutesEnv] = strings.Join(k8sRoutes, ",")
		}
	}
	claimList := "net1"
	if len(u.Networks) > 1 {
		claimList = "net1,net2"
	}
	switch u.DataplaneDriver {
	case provider.DriverSocket:
		tmmEnvMap["TMM_SKIP_NICS"] = "eth0"
		tmmEnvMap["TMM_GENERIC_SOCKET_DRIVER"] = "true"
		tmmEnvMap["TMM_MAPRES_ADDL_VETHS_ON_DP"] = "TRUE"
		tmmEnvMap["TMM_MAPRES_IFC_CLAIM_LIST"] = claimList
	case provider.DriverVirtio:
		tmmEnvMap["TMM_GENERIC_SOCKET_DRIVER"] = "true"
		tmmEnvMap["TMM_MAPRES_ADDL_VETHS_ON_DP"] = "TRUE"
		tmmEnvMap["TMM_MAPRES_IFC_CLAIM_LIST"] = claimList
		extPCI := "0000:00:05.0"
		intPCI := "0000:00:06.0"
		if len(u.DataplaneNodes) > 0 {
			for _, nic := range u.DataplaneNodes[0].NICs {
				if nic.NICIndex == 1 && nic.PCIAddress != "" {
					extPCI = nic.PCIAddress
				}
				if nic.NICIndex == 2 && nic.PCIAddress != "" {
					intPCI = nic.PCIAddress
				}
			}
		}
		tmmEnvMap["PCIDEVICE_INTEL_COM_EXT"] = extPCI
		if len(u.Networks) > 1 {
			tmmEnvMap["PCIDEVICE_INTEL_COM_INT"] = intPCI
		}
	case provider.DriverDPDKPCI:
		tmmEnvMap["TMM_GENERIC_SOCKET_DRIVER"] = "true"
		tmmEnvMap["TMM_MAPRES_ADDL_VETHS_ON_DP"] = "TRUE"
		tmmEnvMap["TMM_SOCAT_DISABLE"] = "1"
		extIf := "ens8"
		extPCI := "0000:00:08.0"
		intIf := "ens7"
		intPCI := "0000:00:07.0"
		if len(u.DataplaneNodes) > 0 {
			for _, nic := range u.DataplaneNodes[0].NICs {
				if nic.NICIndex == 1 {
					if nic.LinuxIfName != "" {
						extIf = nic.LinuxIfName
					}
					if nic.PCIAddress != "" {
						extPCI = nic.PCIAddress
					}
				}
				if nic.NICIndex == 2 {
					if nic.LinuxIfName != "" {
						intIf = nic.LinuxIfName
					}
					if nic.PCIAddress != "" {
						intPCI = nic.PCIAddress
					}
				}
			}
		}
		extIfUpper := strings.ToUpper(strings.ReplaceAll(extIf, "-", "_"))
		tmmEnvMap["ROBIN_VFIO_RESOURCE_1"] = extIfUpper
		tmmEnvMap["PCIDEVICE_INTEL_COM_"+extIfUpper] = extPCI
		if len(u.Networks) > 1 {
			intIfUpper := strings.ToUpper(strings.ReplaceAll(intIf, "-", "_"))
			tmmEnvMap["ROBIN_VFIO_RESOURCE_2"] = intIfUpper
			tmmEnvMap["PCIDEVICE_INTEL_COM_"+intIfUpper] = intPCI
		}
	}
	if cl.Bnk != nil {
		if cl.Bnk.AIGateway != nil && cl.Bnk.AIGateway.MultiNUMASpanning {
			tmmEnvMap["TMM_ENFORCE_SINGLE_NUMA_NODE"] = "false"
		}
		for k, v := range cl.Bnk.TmmEnv {
			tmmEnvMap[k] = v
		}
	}

	// Build Controller environment variables
	ctrlEnvMap := map[string]string{
		"TMM_DEFAULT_MTU": fmt.Sprintf("%d", mtu),
	}
	if prof.Train.Is24OrLater() {
		ctrlEnvMap["USE_GATEWAY_SETTINGS"] = "true"
		ctrlEnvMap["MAX_ACTIVE_TMM_REPLICAS"] = "32"
		if cl.Bnk != nil && (cl.Bnk.BGP || cl.Bnk.DynamicRouting) {
			ctrlEnvMap["ZEBOS_STATE"] = "legacy"
		}
	}
	if cl.Bnk != nil {
		for k, v := range cl.Bnk.ControllerEnv {
			ctrlEnvMap[k] = v
		}
	}

	tmmEnvList := mapToEnvSlice(tmmEnvMap)
	ctrlEnvList := mapToEnvSlice(ctrlEnvMap)

	cvars := render.CertChainVarsFromCluster(cl)
	dynRoutingEnabled := cl.Bnk != nil && (cl.Bnk.BGP || cl.Bnk.DynamicRouting)

	envDiscSpec := map[string]any{
		"enabled": false,
	}
	demoModeEnabled := false
	if u.DataplaneDriver == provider.DriverSocket || u.DataplaneDriver == provider.DriverVirtio {
		envDiscSpec = map[string]any{
			"runAfterSuccess": true,
			"stopOnFail":      true,
		}
		demoModeEnabled = true
	}

	tmmAdv := map[string]any{
		"env": tmmEnvList,
	}
	hasExplicitTMMRes := cl.Bnk != nil && (cl.Bnk.TmmCpu != "" || cl.Bnk.TmmMemory != "" || cl.Bnk.TmmHugepages != "")
	if u.DataplaneDriver != provider.DriverVirtio || hasExplicitTMMRes {
		tmmAdv["resources"] = map[string]any{
			"limits": map[string]any{
				"cpu":           tmmCPU,
				"memory":        tmmMem,
				"hugepages-2Mi": tmmHuge,
			},
			"requests": map[string]any{
				"cpu":           tmmCPU,
				"memory":        tmmMem,
				"hugepages-2Mi": tmmHuge,
			},
		}
	}
	advancedSpec := map[string]any{
		"coremon": map[string]any{
			"hostPath": true,
		},
		"envDiscovery": envDiscSpec,
		"demoMode": map[string]any{
			"enabled": demoModeEnabled,
		},
		"maintenanceMode": map[string]any{
			"enabled": false,
		},
		"tmm": tmmAdv,
	}
	if len(ctrlEnvList) > 0 {
		advancedSpec["cneController"] = map[string]any{
			"env": ctrlEnvList,
		}
	}

	cneSpec := map[string]any{
		"product": map[string]any{
			"gatewayAPI": true,
			"type":       "BNK",
		},
		"manifestVersion":  prof.Version,
		"deploymentSize":   depSize,
		"wholeCluster":     true,
		"storageClassName": storageClass,
		"certificate": map[string]any{
			"clusterIssuer": cvars.CAIssuer,
		},
		"registry": map[string]any{
			"uri": imageReg,
			"imagePullSecrets": []map[string]any{
				{"name": "far-secret"},
			},
			"imagePullPolicy": "Always",
		},
		"networkAttachments": nadNames,
		"dynamicRouting": map[string]any{
			"enabled": dynRoutingEnabled,
		},
		"firewallACL": map[string]any{
			"enabled": true,
		},
		"pseudoCNI": map[string]any{
			"enabled": true,
		},
		"coreCollection": map[string]any{
			"enabled": true,
		},
		"intelligentLB": map[string]any{
			"enabled": true,
		},
		"tmmReplicas": 1,
		"telemetry": map[string]any{
			"loggingSubsystem": map[string]any{
				"enabled": true,
			},
			"metricSubsystem": map[string]any{
				"enabled": true,
			},
		},
		"advanced": advancedSpec,
	}
	if prof.Train.Is24OrLater() {
		cneSpec["eppNamespaces"] = []string{"All"}
	}
	aiGwEnabled := strings.HasPrefix(prof.Version, "2.4.1") || strings.HasPrefix(prof.Version, "2.4.2") || strings.HasPrefix(prof.Version, "2.5")
	aiMetrics := aiGwEnabled
	consoleURL := ""
	if cl.Bnk != nil && cl.Bnk.AIGateway != nil {
		aiGwEnabled = cl.Bnk.AIGateway.Enabled
		aiMetrics = cl.Bnk.AIGateway.AIMetrics || cl.Bnk.AIGateway.Enabled
		consoleURL = strings.TrimSpace(cl.Bnk.AIGateway.ConsoleURL)
	}
	if aiGwEnabled {
		if consoleURL == "" {
			consoleURL = "http://f5-ai-console-webui.aigateway:8080/v1/services/default"
		}
		cneSpec["aiGateway"] = map[string]any{
			"enabled":    true,
			"aiMetrics":  aiMetrics,
			"consoleURL": consoleURL,
		}
	}

	obj := map[string]any{
		"apiVersion": "k8s.f5.com/v1",
		"kind":       "CNEInstance",
		"metadata": map[string]any{
			"name":      cl.EffectiveName() + "-bnk",
			"namespace": bnkconst.InstanceNamespace,
		},
		"spec": cneSpec,
	}
	b, err := yaml.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal CNEInstance: %w", err)
	}
	return b, nil
}

func renderUnifiedLicenseCR(cl *intent.Cluster, jwtToken string) ([]byte, error) {
	var docs []string
	for _, ns := range []string{bnkconst.InstanceNamespace, bnkconst.CoreNamespace, bnkconst.UtilsNamespace} {
		obj := map[string]any{
			"apiVersion": "k8s.f5net.com/v1",
			"kind":       "License",
			"metadata": map[string]any{
				"name":      "bnk-license",
				"namespace": ns,
			},
			"spec": map[string]any{
				"operationMode": "connected",
				"jwt":           strings.TrimSpace(jwtToken),
				"teemCertUrl":   TEEMCertURL,
			},
		}
		b, err := yaml.Marshal(obj)
		if err != nil {
			return nil, fmt.Errorf("marshal License CR (%s): %w", ns, err)
		}
		docs = append(docs, strings.TrimSpace(string(b)))
	}
	return []byte(strings.Join(docs, "\n---\n") + "\n"), nil
}

func renderUnifiedGatewayClass(cl *intent.Cluster) ([]byte, error) {
	tmpl, err := manifests.FS.ReadFile("host-device/gatewayclass.yaml.tmpl")
	if err != nil {
		return nil, fmt.Errorf("read host-device/gatewayclass.yaml.tmpl: %w", err)
	}
	return render.RenderGatewayClass(tmpl, cl)
}

func renderUnifiedNetworkAndEgress(cl *intent.Cluster, u *provider.UnderlayContext, prof *release.ReleaseProfile) ([]byte, []byte, error) {
	ext := u.ExternalNetwork()
	if ext == nil {
		return nil, nil, fmt.Errorf("render network CRs: external network is nil")
	}
	intNet := u.InternalNetwork()
	mtu := ext.MTU
	if mtu <= 0 {
		mtu = 1500
	}
	if u.DataplaneDriver == provider.DriverDPDKPCI && mtu > 1500 {
		mtu = 1500
	}

	extStart, extEnd, extPrefLen := ipRangeFromSelfIPs(ext.SelfIPs, ext.SubnetCIDR)
	snatStart, _ := snatRangeFromSubnet(ext.SubnetCIDR, extStart, extEnd)

	if prof.NetworkModel == release.NetworkModelLegacySPK {
		var docs []string
		extVLAN := map[string]any{
			"apiVersion": "k8s.f5net.com/v1",
			"kind":       "F5SPKVlan",
			"metadata": map[string]any{
				"name":      "external",
				"namespace": bnkconst.InstanceNamespace,
			},
			"spec": map[string]any{
				"name":      "external",
				"interface": ext.TMMInterface,
				"mtu":       mtu,
				"selfips":   stripMasks(ext.SelfIPs),
				"prefixlen": extPrefLen,
			},
		}
		b, err := yaml.Marshal(extVLAN)
		if err != nil {
			return nil, nil, err
		}
		docs = append(docs, strings.TrimSpace(string(b)))

		if intNet != nil {
			intVLAN := map[string]any{
				"apiVersion": "k8s.f5net.com/v1",
				"kind":       "F5SPKVlan",
				"metadata": map[string]any{
					"name":      "internal",
					"namespace": bnkconst.InstanceNamespace,
				},
				"spec": map[string]any{
					"name":      "internal",
					"interface": intNet.TMMInterface,
					"mtu":       mtu,
					"selfips":   stripMasks(intNet.SelfIPs),
					"prefixlen": prefixLenFromCIDR(intNet.SubnetCIDR, 24),
				},
			}
			ib, err := yaml.Marshal(intVLAN)
			if err != nil {
				return nil, nil, err
			}
			docs = append(docs, strings.TrimSpace(string(ib)))
		}

		defRoute := map[string]any{
			"apiVersion": "k8s.f5net.com/v1",
			"kind":       "F5SPKStaticRoute",
			"metadata": map[string]any{
				"name":      "default-route",
				"namespace": bnkconst.InstanceNamespace,
			},
			"spec": map[string]any{
				"destination": "0.0.0.0",
				"prefixlen":   0,
				"gateway":     ext.GatewayIP,
				"vlanName":    "external",
			},
		}
		rb, err := yaml.Marshal(defRoute)
		if err != nil {
			return nil, nil, err
		}
		docs = append(docs, strings.TrimSpace(string(rb)))

		snatObj := map[string]any{
			"apiVersion": "k8s.f5net.com/v1",
			"kind":       "F5SPKSnatpool",
			"metadata": map[string]any{
				"name":      render.GatewaySettingsSnatPool,
				"namespace": bnkconst.InstanceNamespace,
			},
			"spec": map[string]any{
				"name": render.GatewaySettingsSnatPool,
				"addressList": [][]string{
					{snatStart},
				},
			},
		}
		sb, err := yaml.Marshal(snatObj)
		if err != nil {
			return nil, nil, err
		}
		docs = append(docs, strings.TrimSpace(string(sb)))

		egressObj := map[string]any{
			"apiVersion": "k8s.f5net.com/v1",
			"kind":       "F5SPKEgress",
			"metadata": map[string]any{
				"name":      "egress",
				"namespace": bnkconst.InstanceNamespace,
			},
			"spec": map[string]any{
				"dualStackEnabled": false,
				"snatpoolName":     render.GatewaySettingsSnatPool,
			},
		}
		eb, err := yaml.Marshal(egressObj)
		if err != nil {
			return nil, nil, err
		}
		return []byte(strings.Join(docs, "\n---\n") + "\n"), eb, nil
	}

	// BNK 2.4+ / 2.5+ Gateway-Infra network model (gateway.k8s.f5.com/v1 or v1alpha1)
	gwAPIVer := prof.ServedGatewayAPIVersion
	if gwAPIVer == "" {
		gwAPIVer = "gateway.k8s.f5.com/v1"
	}

	az := ""
	if cl != nil && cl.Bnk != nil && strings.EqualFold(cl.Bnk.ControllerEnv["PLATFORM_TYPE"], "AWS") {
		if cl.Metadata.Zone != "" {
			az = cl.Metadata.Zone
		} else if len(cl.Network.AZs) > 0 {
			az = cl.Network.AZs[0]
		}
	}

	extPool := map[string]any{
		"rangeStart": extStart,
		"rangeEnd":   extEnd,
	}
	if az != "" {
		extPool["availabilityZone"] = az
	}
	listenerStart, listenerEnd := extStart, extEnd
	if len(ext.SelfIPs) > 0 && strings.Contains(ext.SelfIPs[0], "/") {
		if ip, ipNet, err := net.ParseCIDR(ext.SelfIPs[0]); err == nil && ipNet != nil {
			ones, bits := ipNet.Mask.Size()
			if bits == 32 && ones >= 28 {
				if v4 := ip.Mask(ipNet.Mask).To4(); v4 != nil {
					hostCount := byte(1 << (32 - ones))
					listenerStart = net.IPv4(v4[0], v4[1], v4[2], v4[3]+3).String()
					listenerEnd = net.IPv4(v4[0], v4[1], v4[2], v4[3]+hostCount-2).String()
				}
			}
		}
	}
	if listenerStart == extStart && listenerEnd == extEnd {
		if ip, ipNet, err := net.ParseCIDR(ext.SubnetCIDR); err == nil && ipNet != nil {
			if v4 := ip.Mask(ipNet.Mask).To4(); v4 != nil {
				listenerStart = net.IPv4(v4[0], v4[1], v4[2], 100).String()
				listenerEnd = net.IPv4(v4[0], v4[1], v4[2], 199).String()
			}
		}
	}
	listenerPool := map[string]any{
		"rangeStart": listenerStart,
		"rangeEnd":   listenerEnd,
	}
	if az != "" {
		listenerPool["availabilityZone"] = az
	}
	ipams := []map[string]any{
		{
			"name": render.InfraExtIPAM,
			"ipPools": []map[string]any{
				extPool,
			},
		},
		{
			"name": "listener-pool",
			"ipPools": []map[string]any{
				listenerPool,
			},
		},
	}

	netAttachments := []map[string]any{
		{
			"name": "ext-attach",
			"networkAttachmentDefinitions": []map[string]any{
				{"name": ext.Name},
			},
		},
	}

	networks := []map[string]any{
		{
			"name": render.InfraExtNetwork,
			"type": "vlan",
			"vlan": map[string]any{
				"tag": 0,
				"mtu": mtu,
				"ipamRefs": []map[string]any{
					{"name": render.InfraExtIPAM},
				},
				"networkAttachmentRef": map[string]any{
					"name": "ext-attach",
				},
			},
		},
	}

	if intNet != nil {
		intStart, intEnd, _ := ipRangeFromSelfIPs(intNet.SelfIPs, intNet.SubnetCIDR)
		intPool := map[string]any{
			"rangeStart": intStart,
			"rangeEnd":   intEnd,
		}
		if az != "" {
			intPool["availabilityZone"] = az
		}
		ipams = append(ipams, map[string]any{
			"name": render.InfraIntIPAM,
			"ipPools": []map[string]any{
				intPool,
			},
		})
		netAttachments = append(netAttachments, map[string]any{
			"name": "int-attach",
			"networkAttachmentDefinitions": []map[string]any{
				{"name": intNet.Name},
			},
		})
		networks = append(networks, map[string]any{
			"name": render.InfraIntNetwork,
			"type": "vlan",
			"vlan": map[string]any{
				"tag": 0,
				"mtu": mtu,
				"ipamRefs": []map[string]any{
					{"name": render.InfraIntIPAM},
				},
				"networkAttachmentRef": map[string]any{
					"name": "int-attach",
				},
			},
		})
	}

	staticRoutes := []map[string]any{}
	if ext.GatewayIP != "" {
		staticRoutes = append(staticRoutes, map[string]any{
			"name":         render.InfraRouteDefault,
			"destinations": []string{"0.0.0.0/0"},
			"nextHop":      ext.GatewayIP,
		})
	}
	if u.ServiceCIDR != "" {
		gw := ext.GatewayIP
		if intNet != nil && intNet.GatewayIP != "" {
			gw = intNet.GatewayIP
		}
		if gw != "" {
			staticRoutes = append(staticRoutes, map[string]any{
				"name":         "service-cidr",
				"destinations": []string{u.ServiceCIDR},
				"nextHop":      gw,
			})
		}
	}
	var podCIDRs []string
	if cl != nil && len(cl.Network.Subnets.PrivateList) > 0 {
		for _, sub := range cl.Network.Subnets.PrivateList {
			if trimmed := strings.TrimSpace(sub.CIDR); trimmed != "" {
				podCIDRs = append(podCIDRs, trimmed)
			}
		}
	}
	if len(podCIDRs) == 0 && u.PodCIDR != "" {
		for _, part := range strings.Split(u.PodCIDR, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				podCIDRs = append(podCIDRs, trimmed)
			}
		}
	}
	if len(podCIDRs) > 0 {
		gw := ext.GatewayIP
		if intNet != nil && intNet.GatewayIP != "" {
			gw = intNet.GatewayIP
		}
		if gw != "" {
			staticRoutes = append(staticRoutes, map[string]any{
				"name":         "pod-cidr",
				"destinations": podCIDRs,
				"nextHop":      gw,
			})
		}
	}

	egressNet := render.InfraExtNetwork
	if intNet != nil {
		egressNet = render.InfraIntNetwork
	}

	infraSpec := map[string]any{
		"ipams":              ipams,
		"networkAttachments": netAttachments,
		"networks":           networks,
		"egressDefaults": map[string]any{
			"networkRef": map[string]any{
				"name": egressNet,
			},
			"port":   4789,
			"subnet": "192.168.0.0/16",
		},
	}
	if len(staticRoutes) > 0 {
		infraSpec["staticRoutes"] = staticRoutes
	}

	infraObj := map[string]any{
		"apiVersion": gwAPIVer,
		"kind":       "Infra",
		"metadata": map[string]any{
			"name":      render.InfraName,
			"namespace": bnkconst.InstanceNamespace,
		},
		"spec": infraSpec,
	}
	infraBytes, err := yaml.Marshal(infraObj)
	if err != nil {
		return nil, nil, err
	}

	gwSettingsSpec := map[string]any{
		"sourceNATPools": []map[string]any{
			{
				"name": render.GatewaySettingsSnatPool,
				"ipamRefs": []map[string]any{
					{"name": render.InfraExtIPAM},
				},
			},
		},
		"egressConfigs": []map[string]any{
			{
				"name": render.GatewaySettingsEgressCfg,
				"networkRef": map[string]any{
					"name": egressNet,
				},
				"sourceNATConfig": map[string]any{
					"type": "Automap",
				},
			},
		},
		"ingressConfig": map[string]any{
			"defaultListenerNetwork": map[string]any{
				"ipamRefs": []map[string]any{
					{"name": "listener-pool"},
				},
				"networkRefs": []map[string]any{
					{"name": render.InfraExtNetwork},
				},
				"sourceNATConfig": map[string]any{
					"type": "Automap",
				},
			},
		},
	}

	gwSettingsObj := map[string]any{
		"apiVersion": gwAPIVer,
		"kind":       "GatewaySettings",
		"metadata": map[string]any{
			"name":      render.GatewaySettingsName,
			"namespace": bnkconst.InstanceNamespace,
		},
		"spec": gwSettingsSpec,
	}
	gwsBytes, err := yaml.Marshal(gwSettingsObj)
	if err != nil {
		return nil, nil, err
	}

	testNS := effectiveTestNamespace(cl)
	egressObj := map[string]any{
		"apiVersion": gwAPIVer,
		"kind":       "EgressGateway",
		"metadata": map[string]any{
			"name":      render.EgressGatewayName,
			"namespace": testNS,
		},
		"spec": map[string]any{
			"gatewayClassName": cl.EffectiveName() + "-gatewayclass",
			"infrastructure": map[string]any{
				"parametersRef": map[string]any{
					"name":        render.GatewaySettingsName,
					"sectionName": render.GatewaySettingsEgressCfg,
				},
			},
			"sourceSelector": map[string]any{
				"selectionMode": "NamespaceSelector",
				"namespaces": map[string]any{
					"matchNames": []string{testNS},
				},
			},
		},
	}
	egressBytes, err := yaml.Marshal(egressObj)
	if err != nil {
		return nil, nil, err
	}
	if testNS != bnkconst.InstanceNamespace {
		testGWSObj := map[string]any{
			"apiVersion": gwAPIVer,
			"kind":       "GatewaySettings",
			"metadata": map[string]any{
				"name":      render.GatewaySettingsName,
				"namespace": testNS,
			},
			"spec": map[string]any{
				"egressConfigs": []map[string]any{
					{
						"name": render.GatewaySettingsEgressCfg,
						"networkRef": map[string]any{
							"name": egressNet,
						},
						"sourceNATConfig": map[string]any{
							"type": "Automap",
						},
					},
				},
			},
		}
		if testGWSBytes, mErr := yaml.Marshal(testGWSObj); mErr == nil {
			egressBytes = []byte(strings.TrimSpace(string(testGWSBytes)) + "\n---\n" + strings.TrimSpace(string(egressBytes)) + "\n")
		}
	}

	netCombined := strings.TrimSpace(string(infraBytes)) + "\n---\n" + strings.TrimSpace(string(gwsBytes)) + "\n"
	if u.CloudName == intent.ProviderAWS && (u.DataplaneDriver == "" || u.DataplaneDriver == provider.DriverDPDKPCI) {
		tmmInitTcl := `set_from_env  POD_IP  POD_IP  [info hostname]

    profile tcp nagledisable {
      default_name tcp
      nagle disable
    }

    profile tcp dia-tcp {
      default_name tcp
    }

    profile sctp dia-sctp {
      default_name sctp
    }

    profile http http_socket {
      default_name http
    }
    
    pool pool1 {

    }
    
    snatpool overlay_pool {

    }

    file_reload {
      file_name /opt/lib/tmm/user_conf.tcl
      interval 1
    }

    profile genericmsg gm_proto_for_gtp {
      app-service none
      disable-parser yes
      message-terminator none
      no-response yes
      description none
      max-egress-buffer 32768
      max-message-size 32768
    }

    profile messagerouter gm_router_for_gtp {
      name "gm_router_for_gtp"
      app-service none
      irule-scope-message no
    }

	bigdb tm.tcpsegmentationoffload disable

	bigdb tm.tcpwoodsidefasterrecovery disable

	bigdb tm.tcplargereceiveoffload disable

    pool blobd_pool {
      member 169.254.0.253 9222
    }

    profile tcp blobd_tcp {
      default_name tcp
    }

    # Virtual for blobd
    virtual $POD_IP 9222 {
      name blobd_virtual
      profile blobd_tcp
      vlan enable all
      snat_automap
      pool blobd_pool
      auto_lasthop off
    }
`
		tmmInitObj := map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "tmm-init",
				"namespace": bnkconst.InstanceNamespace,
			},
			"data": map[string]string{
				"tmm_init.tcl":  tmmInitTcl,
				"user_conf.tcl": "bigdb tm.tcpsegmentationoffload disable\nbigdb tm.tcplargereceiveoffload disable\npool pool1 {\n}\n",
			},
		}
		if cmBytes, cmErr := yaml.Marshal(tmmInitObj); cmErr == nil {
			netCombined += "---\n" + strings.TrimSpace(string(cmBytes)) + "\n"
		}
	}
	aiGwDNS := strings.HasPrefix(prof.Version, "2.4.1") || strings.HasPrefix(prof.Version, "2.4.2") || strings.HasPrefix(prof.Version, "2.5")
	if cl != nil && cl.Bnk != nil && cl.Bnk.AIGateway != nil {
		aiGwDNS = cl.Bnk.AIGateway.Enabled || cl.Bnk.AIGateway.DNSResolver
	}
	if aiGwDNS {
		dnsIP := "10.96.0.10"
		if u != nil && u.ServiceCIDR != "" {
			if ip, ipNet, err := net.ParseCIDR(u.ServiceCIDR); err == nil && ipNet != nil {
				if v4 := ip.Mask(ipNet.Mask).To4(); v4 != nil {
					dnsIP = net.IPv4(v4[0], v4[1], v4[2], 10).String()
				}
			}
		}
		dnsObj := map[string]any{
			"apiVersion": "k8s.f5net.com/v1",
			"kind":       "F5BigDnsCache",
			"metadata": map[string]any{
				"name":      "aigw-dns-resolver",
				"namespace": bnkconst.InstanceNamespace,
			},
			"spec": map[string]any{
				"cacheType": "net-resolver",
				"netResolver": map[string]any{
					"forwardZones": []map[string]any{
						{
							"forwardZone": ".",
							"nameServers": []map[string]any{
								{
									"ipAddress": dnsIP,
									"port":      53,
								},
							},
						},
					},
				},
			},
		}
		if dnsBytes, mErr := yaml.Marshal(dnsObj); mErr == nil {
			netCombined += "---\n" + strings.TrimSpace(string(dnsBytes)) + "\n"
		}
	}
	// Note: In BNK 2.4+ gateway-infra mode (USE_GATEWAY_SETTINGS=true), default egress
	// (0.0.0.0/0 -> ext.GatewayIP) is programmed via Infra.spec.staticRoutes above.
	// Applying a legacy F5SPKStaticRoute when USE_GATEWAY_SETTINGS=true triggers a nil
	// pointer panic in f5-cne-controller's validation webhook (getStaticrouteList).
	return []byte(netCombined), egressBytes, nil
}

func ipRangeFromSelfIPs(selfIPs []string, subnetCIDR string) (string, string, int) {
	prefLen := prefixLenFromCIDR(subnetCIDR, 24)
	clean := stripMasks(selfIPs)
	if len(clean) > 0 {
		if parts := strings.SplitN(clean[0], "-", 2); len(parts) == 2 {
			return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), prefLen
		}
		return clean[0], clean[len(clean)-1], prefLen
	}
	ip, ipNet, err := net.ParseCIDR(subnetCIDR)
	if err == nil && ipNet != nil {
		v4 := ip.Mask(ipNet.Mask).To4()
		if v4 != nil {
			start := net.IPv4(v4[0], v4[1], v4[2], 10).String()
			end := net.IPv4(v4[0], v4[1], v4[2], 20).String()
			return start, end, prefLen
		}
	}
	return "10.0.1.10", "10.0.1.20", prefLen
}

func snatRangeFromSubnet(subnetCIDR, fallbackStart, fallbackEnd string) (string, string) {
	ip, ipNet, err := net.ParseCIDR(subnetCIDR)
	if err == nil && ipNet != nil {
		v4 := ip.Mask(ipNet.Mask).To4()
		if v4 != nil {
			start := net.IPv4(v4[0], v4[1], v4[2], 200).String()
			end := net.IPv4(v4[0], v4[1], v4[2], 210).String()
			return start, end
		}
	}
	if fallbackStart != "" && fallbackEnd != "" {
		return fallbackStart, fallbackEnd
	}
	return "10.0.1.200", "10.0.1.210"
}

func mapToEnvSlice(m map[string]string) []map[string]string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]map[string]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]string{
			"name":  k,
			"value": m[k],
		})
	}
	return out
}

func mergeYAMLMap(baseYAML []byte, overrides map[string]any) ([]byte, error) {
	var base map[string]any
	if err := yaml.Unmarshal(baseYAML, &base); err != nil {
		return nil, err
	}
	if base == nil {
		base = make(map[string]any)
	}
	deepMergeMaps(base, overrides)
	return yaml.Marshal(base)
}

func deepMergeMaps(dst, src map[string]any) {
	for k, v := range src {
		if srcMap, ok := v.(map[string]any); ok {
			if dstMap, ok := dst[k].(map[string]any); ok {
				deepMergeMaps(dstMap, srcMap)
				continue
			}
		}
		dst[k] = v
	}
}

func stripMasks(ips []string) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		if idx := strings.IndexByte(ip, '/'); idx >= 0 {
			out = append(out, ip[:idx])
		} else {
			out = append(out, ip)
		}
	}
	return out
}

func prefixLenFromCIDR(cidr string, fallback int) int {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil || ipNet == nil {
		return fallback
	}
	ones, _ := ipNet.Mask.Size()
	if ones <= 0 {
		return fallback
	}
	return ones
}

// ─── CWC Certificate & CPCL JWKS Key Sync Helpers ────────────────────────────

// EnsureCWCCerts generates self-signed CWC server/client TLS secrets in coreNS and utilsNS
// if they do not already exist.
func EnsureCWCCerts(ctx context.Context, k8sClient kubernetes.Interface, coreNS, utilsNS string) error {
	return phases.EnsureCWCCerts(ctx, k8sClient, coreNS, utilsNS)
}

// PurgeStaleCPCLSecrets deletes stale cpcl-key-secret in f5-cne-core before CWC restarts.
func PurgeStaleCPCLSecrets(ctx context.Context, k8sClient kubernetes.Interface) error {
	return phases.PurgeStaleCPCLSecrets(ctx, k8sClient)
}

// SyncCPCLKeySecret fetches the JWKS from CWC's apiserver (or extracts the RSA public key)
// and writes cpcl-key-secret in f5-cne-core so TMM's f5-tmm-clid sidecar validates the JWT.
func SyncCPCLKeySecret(ctx context.Context, k8sClient kubernetes.Interface, jwksEndpoint string) error {
	return phases.SyncCPCLKeySecret(ctx, k8sClient, jwksEndpoint)
}

// ReconcileNullCNEController deletes f5-cne-controller deployment if its env vars rendered
// as null before CNEInstance reconciliation (BNK 2.4 quirk).
func ReconcileNullCNEController(ctx context.Context, k8sClient kubernetes.Interface, dynClient dynamic.Interface) error {
	return phases.ReconcileNullCNEController(ctx, k8sClient, dynClient)
}
