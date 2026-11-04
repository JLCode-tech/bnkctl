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
	"os"
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
	TEEMCertURL = bnkconst.DefaultTEEMCertURL
	// CWCRestNamespace is the namespace where f5-spk-cwc runs.
	CWCRestNamespace = bnkconst.InstanceNamespace
	// CPCLKeySecretName is the secret name synced from CWC JWKS for TMM license validation.
	// #nosec G101 -- Kubernetes Secret resource name, not a credential
	CPCLKeySecretName = bnkconst.CPCLKeySecretName
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

	// Phase 1: Namespaces & CoreDNS forwarder (for GKE kube-dns StatefulSet endpoint_pod_names resolution)
	if err := phases.PhaseNamespaces(ctx, e.K8s, e.applyYAML, rendered.NamespacesYAML, effectiveTestNamespace(e.Cluster)); err != nil {
		return nil, err
	}
	e.ensureCoreDNSForwarder(ctx, opts)

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

	// Phase 4b: Ensure GIE CRDs are installed before CNEInstance.
	// Note: Gateway API (gateway.networking.k8s.io), gateway.k8s.f5.com, and
	// inference.k8s.f5.com CRDs are installed with full OpenAPI v3 schemas and
	// defaults by FLO's crd-installer when CNEInstance reconciles in Phase 5.
	// Pre-creating stub Gateway API CRDs causes crd-installer to skip them,
	// leaving Gateway/HTTPRoute without OpenAPI defaults and triggering nil-pointer
	// panics in f5-cne-controller.
	if crdBytes, rErr := manifests.FS.ReadFile("static/01-gie-crds.yaml"); rErr == nil && len(bytes.TrimSpace(crdBytes)) > 0 {
		if aErr := e.applyYAML(ctx, crdBytes); aErr != nil {
			opts.logf("warning: apply static/01-gie-crds.yaml: %v", aErr)
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

	// Wait up to 300s for FLO crd-installer to establish licenses.k8s.f5net.com before applying License CRs
	if e.Dyn != nil {
		_ = k8s.WaitForCRDEstablished(ctx, e.Dyn, "licenses.k8s.f5net.com", 300*time.Second)
		// Wait up to 180s for FLO crd-installer to install infras.gateway.k8s.f5.com;
		// only fall back to bundled static CRDs on BNK 2.3.x (applying stub static CRDs on 2.4+ breaks OpenAPI defaults).
		if err := k8s.WaitForCRDEstablished(ctx, e.Dyn, "infras.gateway.k8s.f5.com", 180*time.Second); err != nil {
			if e.Profile != nil && strings.HasPrefix(e.Profile.Version, "2.3") {
				for _, fallbackCRD := range []string{
					"static/00-gateway-api-crds.yaml",
					"static/02-bnk-gateway-crds.yaml",
				} {
					if crdBytes, rErr := manifests.FS.ReadFile(fallbackCRD); rErr == nil && len(bytes.TrimSpace(crdBytes)) > 0 {
						_ = e.applyYAML(ctx, crdBytes)
					}
				}
			}
		}
		for _, crdName := range []string{
			"gatewayclasses.gateway.networking.k8s.io",
			"gateways.gateway.networking.k8s.io",
			"httproutes.gateway.networking.k8s.io",
			"grpcroutes.gateway.networking.k8s.io",
			"infras.gateway.k8s.f5.com",
			"l2networks.gateway.k8s.f5.com",
			"l3networks.gateway.k8s.f5.com",
			"gatewaysettings.gateway.k8s.f5.com",
			"egressgateways.gateway.k8s.f5.com",
			"backends.gateway.k8s.f5.com",
			"f5-big-dns-caches.k8s.f5net.com",
		} {
			_ = k8s.WaitForCRDEstablished(ctx, e.Dyn, crdName, 60*time.Second)
		}
	}

	// Re-discover served API versions now that FLO crd-installer has registered Gateway/Inference CRDs
	if e.Disc != nil && e.Profile != nil {
		release.DiscoverServedAPIVersions(e.Disc, e.Profile)
		if netYAML, egressYAML, rErr := renderUnifiedNetworkAndEgress(e.Cluster, e.Underlay, e.Profile); rErr == nil {
			rendered.NetworkCRsYAML = netYAML
			rendered.EgressYAML = egressYAML
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
	e.waitForActivationAndDataplane(ctx, opts)
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

func (e *Engine) waitForActivationAndDataplane(ctx context.Context, opts Options) {
	if e.K8s == nil || e.Dyn == nil {
		return
	}
	cneName := e.Cluster.EffectiveName() + "-bnk"
	deadline := time.Now().Add(bnkconst.DefaultActivationTimeout)
	lastHeal := time.Now()
	for time.Now().Before(deadline) {
		if time.Since(lastHeal) >= 20*time.Second {
			_ = phases.ReconcileNullCNEController(ctx, e.K8s, e.Dyn)
			_, _ = RunHeal(ctx, e.K8s, e.Dyn, HealOptions{
				Cluster:  e.Cluster,
				Underlay: e.Underlay,
				Profile:  e.Profile,
			})
			_ = phases.PhaseLicense(ctx, e.K8s, nil, nil, "")
			lastHeal = time.Now()
		}
		st, err := phases.CheckCNEInstanceActivation(ctx, e.K8s, e.Dyn, cneName)
		if err == nil && st != nil && st.CNEControllerAvailable && st.F5TmmAvailable && st.ReadyTMMPods > 0 {
			opts.logf("[bnkengine] CNEInstance %s active (ReadyTMMPods=%d/%d)", cneName, st.ReadyTMMPods, st.TotalTMMPods)
			break
		}
		phases.CycleStuckTMMPods(ctx, e.K8s)
		select {
		case <-ctx.Done():
			return
		case <-time.After(bnkconst.DefaultActivationPollInterval):
		}
	}

	// Also wait up to 90s for BNK 2.4+ Infra + EgressGateway (or 2.3 F5BigNetVlans) to report Programmed=True
	vlanGVR := schema.GroupVersionResource{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-big-net-vlans"}
	infraVer := "v1alpha1"
	if e.Profile != nil && e.Profile.ServedGatewayAPIVersion != "" {
		infraVer = strings.TrimPrefix(e.Profile.ServedGatewayAPIVersion, "gateway.k8s.f5.com/")
	}
	infraGVR := schema.GroupVersionResource{Group: "gateway.k8s.f5.com", Version: infraVer, Resource: "infras"}
	egressGVR := schema.GroupVersionResource{Group: "gateway.k8s.f5.com", Version: infraVer, Resource: "egressgateways"}
	isGatewayInfra := e.Profile != nil && e.Profile.NetworkModel == release.NetworkModelGatewayInfra

	dpDeadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(dpDeadline) {
		dpOK := false
		if isGatewayInfra {
			infraProg := false
			if infraObj, err := e.Dyn.Resource(infraGVR).Namespace(bnkconst.InstanceNamespace).Get(ctx, "infra", metav1.GetOptions{}); err == nil && infraObj != nil {
				infraProg = k8s.UnstructuredConditionIsTrue(infraObj.Object, "Programmed")
			}
			egressProg := true
			if e.Cluster != nil && e.Cluster.Egress.Enabled {
				egressNS := effectiveTestNamespace(e.Cluster)
				egressProg = false
				if egObj, err := e.Dyn.Resource(egressGVR).Namespace(egressNS).Get(ctx, "egress-gw", metav1.GetOptions{}); err == nil && egObj != nil {
					egressProg = k8s.UnstructuredConditionIsTrue(egObj.Object, "Programmed")
				}
			}
			dpOK = infraProg && egressProg
		} else if list, err := e.Dyn.Resource(vlanGVR).Namespace(bnkconst.InstanceNamespace).List(ctx, metav1.ListOptions{}); err == nil && len(list.Items) >= len(e.Underlay.Networks) {
			allProg := true
			for _, item := range list.Items {
				if !k8s.UnstructuredConditionIsTrue(item.Object, "Programmed") {
					allProg = false
					break
				}
			}
			dpOK = allProg
		}
		if dpOK {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
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
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "netpolicies"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "secpolicies"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "f5epps"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "l4routes"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "egressgateways"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "gatewaysettings"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "backends"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "infras"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "l3networks"},
		{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "l2networks"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "l4globalpolicies"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "apppolicies"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "netpolicies"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "secpolicies"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "f5epps"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "l4routes"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "egressgateways"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "gatewaysettings"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "backends"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "infras"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "l3networks"},
		{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "l2networks"},
		{Group: "gateway.k8s.f5net.com", Version: "v1", Resource: "l4routes"},
		{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"},
		{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "grpcroutes"},
		{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"},
		{Group: "gateway.k8s.f5net.com", Version: "v1", Resource: "gatewaysettings"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "egressgateways"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "infras"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-big-cne-irules"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-big-irules"},
		{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-big-fw-policies"},
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
		bnkconst.NamespaceF5BNKInstance,
		"f5-bnk",
		bnkconst.NamespaceAIGateway,
		"vllm-semantic-router-system",
		"agentfoundry",
		"llm-egress",
		bnkconst.NamespaceBNKDemo,
		"egress-demo",
		"default",
	}
	if testNS := effectiveTestNamespace(e.Cluster); testNS != "" && testNS != "default" && testNS != "llm-egress" {
		namespaces = append(namespaces, testNS)
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
		bnkconst.NamespaceAIGateway,
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

// ─── Unified Manifest Renderers (delegated to pkg/k8s/render) ────────────────

func renderNamespacesYAML(testNS string) ([]byte, error) {
	return render.RenderUnifiedNamespaces(testNS)
}

func renderUnifiedNADs(u *provider.UnderlayContext) ([]byte, error) {
	return render.RenderUnifiedNADs(u)
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
	if e.Underlay != nil && e.Underlay.EnsureTraits().UseSplitMultusManifests {
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

func renderUnifiedCNEInstance(cl *intent.Cluster, u *provider.UnderlayContext, prof *release.ReleaseProfile) ([]byte, error) {
	return render.RenderUnifiedCNEInstance(cl, u, prof)
}

func renderUnifiedLicenseCR(cl *intent.Cluster, jwtToken string) ([]byte, error) {
	return render.RenderUnifiedLicenseCR(cl, jwtToken, TEEMCertURL)
}

func renderUnifiedGatewayClass(cl *intent.Cluster) ([]byte, error) {
	return render.RenderUnifiedGatewayClass(cl)
}

func renderUnifiedNetworkAndEgress(cl *intent.Cluster, u *provider.UnderlayContext, prof *release.ReleaseProfile) ([]byte, []byte, error) {
	return render.RenderUnifiedNetworkAndEgress(cl, u, prof)
}

func mergeYAMLMap(baseYAML []byte, overrides map[string]any) ([]byte, error) {
	return render.MergeYAMLMap(baseYAML, overrides)
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

func (e *Engine) ensureCoreDNSForwarder(ctx context.Context, opts Options) {
	if e.K8s == nil {
		return
	}
	cm, err := e.K8s.CoreV1().ConfigMaps("kube-system").Get(ctx, "kube-dns", metav1.GetOptions{})
	if err != nil || cm == nil {
		return
	}
	fwdBytes, err := manifests.FS.ReadFile("static/03-coredns-forwarder.yaml")
	if err != nil {
		opts.logf("warning: read static/03-coredns-forwarder.yaml: %v", err)
		return
	}
	if err := e.applyYAML(ctx, fwdBytes); err != nil {
		opts.logf("warning: apply static/03-coredns-forwarder.yaml: %v", err)
		return
	}
	svc, err := e.K8s.CoreV1().Services("kube-system").Get(ctx, "coredns-f5", metav1.GetOptions{})
	if err == nil && svc != nil && svc.Spec.ClusterIP != "" {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		stub := fmt.Sprintf(`{"f5-cne-core.svc.cluster.local":["%s"],"f5-cne-system.svc.cluster.local":["%s"]}`, svc.Spec.ClusterIP, svc.Spec.ClusterIP)
		if cm.Data["stubDomains"] != stub {
			cm.Data["stubDomains"] = stub
			_, _ = e.K8s.CoreV1().ConfigMaps("kube-system").Update(ctx, cm, metav1.UpdateOptions{})
		}
	}
}
