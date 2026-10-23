package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"helm.sh/helm/v3/pkg/release"
	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/JLCode-tech/bnkctl/pkg/bnk"
	k8swait "github.com/JLCode-tech/bnkctl/pkg/k8s"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/bnkscan"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/render"
	bnkrelease "github.com/JLCode-tech/bnkctl/pkg/release"
)

// Names the upgrade watches for, as FLO 2.30 creates them in the CNE namespace.
const (
	// ControllerDeployment is the cne-controller Deployment.
	ControllerDeployment = "f5-cne-controller"
	// TMMLabelSelector selects the TMM pods.
	TMMLabelSelector = "app=f5-tmm"
	// InfraCRDName is installed by the 2.4 crd-installer: its arrival proves
	// FLO reconciled the new manifest.
	InfraCRDName = "infras.gateway.k8s.f5.com"

	// UseGatewaySettingsEnv is the cne-controller switch for the Infra and
	// GatewaySettings reconcilers (env default false; FLO 2.30 does not set it).
	UseGatewaySettingsEnv = "USE_GATEWAY_SETTINGS"
	// MaxActiveTMMEnv is what the f5ingress chart sets and FLO 2.30 drops;
	// without it the 2.4 controller keeps every TMM in standby.
	MaxActiveTMMEnv      = "MAX_ACTIVE_TMM_REPLICAS"
	maxActiveTMMDefault  = "32"
	zebosStateEnv        = "ZEBOS_STATE"
	zebosStateLegacy     = "legacy"
	defaultUpgradeWait   = 20 * time.Minute
	defaultUpgradePoll   = 10 * time.Second
	defaultRenderWait    = 3 * time.Minute
	upgradeFieldManager  = "awsbnkctl-bnk-upgrade"
	documentedSuffixExpr = `^(\d+\.\d+\.\d+)-3\.3175\.0[-+]0\.0\.380$`
)

var documentedSuffix = regexp.MustCompile(documentedSuffixExpr)

// CanonicalManifestVersion maps the version string F5's 2.4.0 docs print
// ("2.4.0-3.3175.0+0.0.380") to the tag the registry and FLO use ("2.4.0").
// note is non-empty when a rewrite happened. Other strings pass through.
func CanonicalManifestVersion(v string) (version, note string) {
	if m := documentedSuffix.FindStringSubmatch(v); m != nil {
		return m[1], fmt.Sprintf("manifestVersion %q rewritten to %q: repo.f5.com publishes the release-manifest chart under that tag and FLO matches bigip-k8s-manifest-%s.yaml", v, m[1], m[1])
	}
	return v, ""
}

// UpgradeOptions drives Upgrade.
type UpgradeOptions struct {
	// Namespace is the CNE namespace (default DefaultInstanceNamespace).
	Namespace string
	// CNEInstanceName selects the instance; empty = the only one in Namespace.
	CNEInstanceName string
	// ManifestVersion is the target CNEInstance manifestVersion (default
	// bnk.DefaultManifestVersion after CanonicalManifestVersion).
	ManifestVersion string
	// FLOChart is the f5-lifecycle-operator chart version; empty = the one
	// paired with ManifestVersion in bnk.KnownReleases or resolved via pkg/release.
	FLOChart string
	// SAKeyOrArchive is an optional FAR service-account key JSON/base64 or local
	// .tar.gz/.tgz archive path used by pkg/release.Resolve for uncatalogued versions.
	SAKeyOrArchive string
	// Values are the FLO Helm values. Nil reuses the deployed release's values.
	Values map[string]any
	// DryRun prints the plan and touches nothing.
	DryRun bool
	// Timeout bounds every wait (default 20 min).
	Timeout time.Duration
	// Poll is the wait interval (default 10 s).
	Poll time.Duration
	// RenderWait bounds how long FLO gets to render the controller env into the
	// f5-cne-controller Deployment before the CNEController CR is recreated
	// (default 3 min). FLO 2.30 cannot update a CNEController created by 2.21
	// (its payload carries null for spec.crdUpdater.resources and
	// f5CsmQkview, which the 2.4 CRD rejects); deleting the CR makes FLO
	// recreate it through the create path fresh installs use.
	RenderWait time.Duration
	// ControllerRBAC is the rendered EndpointSlice ClusterRole + binding
	// (render.RenderCNEControllerRBAC). FLO 2.30 grants the controller only
	// list/watch on endpointslices; without get no pool ever gets members, so
	// every Gateway resets connections. Applied when the controller SA cannot
	// get endpointslices, followed by a controller restart. Nil skips the step.
	ControllerRBAC []byte
	// ServiceCIDR is the cluster's Kubernetes service range (EKS serviceIpv4Cidr,
	// state render.ServiceCIDRStateKey). Step 2 adds it to the TMM env as
	// render.TMMK8sRoutesEnv (the f5-tmm chart's add_k8s_routes) so TMM keeps
	// DNS, dSSM and log forwarding reachable over eth0 after the Infra CR gives
	// it a default route. Empty leaves the env alone.
	ServiceCIDR string
	// ReconcileControllerIdentity is an optional cloud hook (e.g. AWS IRSA,
	// Azure Workload Identity, or GKE Workload Identity annotation on
	// f5-cne-controller ServiceAccount) invoked during upgrade.
	ReconcileControllerIdentity func(ctx context.Context) error
	// Log receives progress lines.
	Log io.Writer
}

// UpgradeDeps are the clients Upgrade needs; tests inject fakes.
type UpgradeDeps struct {
	Helm k8swait.HelmInstaller
	Dyn  dynamic.Interface
	K8s  kubernetes.Interface
	// Applier server-side-applies the RBAC supplement (nil skips it).
	Applier Applier
}

// UpgradeResult summarises what changed.
type UpgradeResult struct {
	Schema          string `json:"schema"`
	FLOFrom         string `json:"floFrom"`
	FLOTo           string `json:"floTo"`
	FLOUpgraded     bool   `json:"floUpgraded"`
	CNEInstance     string `json:"cneInstance"`
	ManifestFrom    string `json:"manifestFrom"`
	ManifestTo      string `json:"manifestTo"`
	Patched         bool   `json:"patched"`
	ControllerReady bool   `json:"controllerReady"`
	TMMReady        int    `json:"tmmReady"`
	TMMTotal        int    `json:"tmmTotal"`
	DryRun          bool   `json:"dryRun"`
	// ControllerRecreated is true when the CNEController CR had to be deleted
	// so FLO would render the 2.4 controller env.
	ControllerRecreated bool `json:"controllerRecreated"`
	// RBACApplied is true when the EndpointSlice ClusterRole supplement had to
	// be applied (and the controller restarted to pick it up).
	RBACApplied bool `json:"rbacApplied"`
	// LogStreamEnabled is true when the CLI had to turn on the f5-toda-fluentd
	// stdout store (k8s.EnableTMMLogStream) after the rollout.
	LogStreamEnabled bool `json:"logStreamEnabled"`
	// Multus reports the Multus kubeconfig-token heal that ran before FLO
	// was upgraded (nil without a typed client or on dry-run).
	Multus *k8swait.MultusHealResult `json:"multus,omitempty"`
	// Readiness is the shared bnkscan verdict taken after the rollout (nil on
	// dry-run or when the scan failed).
	Readiness *bnkscan.Readiness `json:"readiness,omitempty"`
}

// Upgrade performs the in-place 2.3.x -> 2.4+ move:
//
//  1. helm upgrade f5-lifecycle-operator to the paired FLO chart (skipped when
//     the release already runs it);
//  2. patch the CNEInstance: spec.manifestVersion and the controller env
//     USE_GATEWAY_SETTINGS=true (plus MAX_ACTIVE_TMM_REPLICAS and the TMM
//     ZEBOS_STATE=legacy the 2.4 examples set), preserving every other entry;
//  3. wait for the Infra CRD, the CNEInstance CNEControllerAvailable and
//     F5TmmAvailable conditions, the controller Deployment, the flag in its
//     pods and Ready TMM pods.
func Upgrade(ctx context.Context, deps UpgradeDeps, opts UpgradeOptions) (*UpgradeResult, error) {
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	if opts.Namespace == "" {
		opts.Namespace = DefaultInstanceNamespace
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultUpgradeWait
	}
	if opts.Poll <= 0 {
		opts.Poll = defaultUpgradePoll
	}
	if opts.RenderWait <= 0 {
		opts.RenderWait = defaultRenderWait
	}
	target, note := CanonicalManifestVersion(opts.ManifestVersion)
	if target == "" {
		target = bnk.DefaultManifestVersion
	}
	if note != "" {
		fmt.Fprintf(opts.Log, "[upgrade] %s\n", note)
	}
	floChart := opts.FLOChart
	if floChart == "" {
		chart, ok := bnk.FLOChartFor(target)
		if ok {
			floChart = chart
		} else {
			prof, err := bnkrelease.Resolve(ctx, target, opts.SAKeyOrArchive, nil)
			if err != nil || prof == nil || strings.TrimSpace(prof.FLOChartVersion) == "" || (prof.Train.Major != 2 && !bnkrelease.IsCatalogued(target) && opts.SAKeyOrArchive == "") {
				return nil, fmt.Errorf("manifestVersion %q is not in the known releases; pass --flo-version with the paired f5-lifecycle-operator chart", target)
			}
			floChart = prof.FLOChartVersion
		}
	}
	res := &UpgradeResult{Schema: "awsbnkctl.bnk-upgrade/v1", FLOTo: floChart, ManifestTo: target, DryRun: opts.DryRun}

	// Step 0: Multus. The FLO and controller pods this upgrade creates need a
	// Multus whose kubeconfig follows the service-account token.
	if deps.K8s != nil && !opts.DryRun {
		mh, err := k8swait.EnsureMultusTokenWatch(ctx, deps.K8s, false, prefixWriter{opts.Log, "[upgrade] "})
		if err != nil {
			fmt.Fprintf(opts.Log, "[upgrade] warning: multus heal: %v\n", err)
		} else {
			res.Multus = &mh
		}
	}

	// Step 1: FLO.
	if deps.Helm == nil {
		return nil, fmt.Errorf("upgrade: no Helm client")
	}
	var existing *release.Release
	var releaseNS string
	for _, ns := range []string{k8swait.FLONamespace, opts.Namespace} {
		if ns == "" {
			continue
		}
		for _, name := range []string{k8swait.FLOReleaseName, "flo"} {
			rels, err := deps.Helm.List(ns, "^"+name+"$")
			if err == nil && len(rels) > 0 {
				existing = rels[0]
				releaseNS = ns
				break
			}
		}
		if existing != nil {
			break
		}
	}

	if existing != nil {
		if existing.Chart != nil && existing.Chart.Metadata != nil {
			res.FLOFrom = existing.Chart.Metadata.Version
		}
		values := opts.Values
		if values == nil {
			values = existing.Config
		}
		switch {
		case res.FLOFrom == floChart:
			fmt.Fprintf(opts.Log, "[upgrade] step 1: FLO release %s already at %s\n", existing.Name, floChart)
		case opts.DryRun:
			fmt.Fprintf(opts.Log, "[upgrade] step 1: dry-run: would helm upgrade %s %s -> %s (%s)\n", existing.Name, res.FLOFrom, floChart, k8swait.FLOChartRef)
		default:
			fmt.Fprintf(opts.Log, "[upgrade] step 1: pulling %s@%s\n", k8swait.FLOChartRef, floChart)
			ch, err := deps.Helm.PullAndLoad(k8swait.FLOChartRef, floChart)
			if err != nil {
				return res, fmt.Errorf("pull FLO chart %s: %w", floChart, err)
			}
			fmt.Fprintf(opts.Log, "[upgrade] step 1: helm upgrade %s in %s %s -> %s\n", existing.Name, releaseNS, res.FLOFrom, floChart)
			if _, err := deps.Helm.Upgrade(existing.Name, releaseNS, ch, values); err != nil {
				return res, fmt.Errorf("helm upgrade %s: %w", existing.Name, err)
			}
			res.FLOUpgraded = true
			fmt.Fprintf(opts.Log, "[upgrade] step 1: helm upgrade complete\n")
		}
	} else {
		// No Helm release found. Check if FLO was installed via static Deployment.
		var floDep *appsv1.Deployment
		var floNS string
		if deps.K8s != nil {
			for _, ns := range []string{opts.Namespace, k8swait.FLONamespace} {
				if ns == "" {
					continue
				}
				for _, name := range []string{"flo-f5-lifecycle-operator", "f5-lifecycle-operator"} {
					d, err := deps.K8s.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
					if err == nil {
						floDep = d
						floNS = ns
						break
					}
				}
				if floDep != nil {
					break
				}
			}
		}
		if floDep == nil {
			return nil, fmt.Errorf("helm release %s not found in %s: bnk upgrade moves an existing install; use bnkctl up for a new cluster", k8swait.FLOReleaseName, k8swait.FLONamespace)
		}

		for _, c := range floDep.Spec.Template.Spec.Containers {
			parts := strings.Split(c.Image, ":")
			if len(parts) > 1 {
				res.FLOFrom = parts[len(parts)-1]
				break
			}
		}

		switch {
		case res.FLOFrom == floChart:
			fmt.Fprintf(opts.Log, "[upgrade] step 1: FLO deployment %s in %s already at %s\n", floDep.Name, floNS, floChart)
		case opts.DryRun:
			fmt.Fprintf(opts.Log, "[upgrade] step 1: dry-run: would upgrade FLO deployment %s in %s %s -> %s (%s)\n", floDep.Name, floNS, res.FLOFrom, floChart, k8swait.FLOChartRef)
		default:
			fmt.Fprintf(opts.Log, "[upgrade] step 1: pulling %s@%s\n", k8swait.FLOChartRef, floChart)
			ch, err := deps.Helm.PullAndLoad(k8swait.FLOChartRef, floChart)
			if err != nil {
				return res, fmt.Errorf("pull FLO chart %s: %w", floChart, err)
			}
			if deps.Applier != nil {
				for _, dep := range ch.Dependencies() {
					if dep.Name() == "crds" {
						for _, t := range dep.Templates {
							if strings.HasSuffix(t.Name, ".yaml") || strings.HasSuffix(t.Name, ".yml") {
								objs, err := k8swait.ParseYAMLStream(t.Data)
								if err == nil {
									for _, obj := range objs {
										_ = deps.Applier.Apply(ctx, obj)
									}
								}
							}
						}
					}
				}
			}
			targetImg := fmt.Sprintf("repo.f5.com/images/f5-lifecycle-operator:%s", floChart)
			fmt.Fprintf(opts.Log, "[upgrade] step 1: updating FLO deployment %s image to %s\n", floDep.Name, targetImg)
			for i := range floDep.Spec.Template.Spec.Containers {
				if floDep.Spec.Template.Spec.Containers[i].Name == "f5-lifecycle-operator" || len(floDep.Spec.Template.Spec.Containers) == 1 {
					floDep.Spec.Template.Spec.Containers[i].Image = targetImg
					floDep.Spec.Template.Spec.Containers[i].ImagePullPolicy = corev1.PullAlways
				}
			}
			if _, err := deps.K8s.AppsV1().Deployments(floNS).Update(ctx, floDep, metav1.UpdateOptions{}); err != nil {
				return res, fmt.Errorf("update FLO deployment %s: %w", floDep.Name, err)
			}
			fmt.Fprintf(opts.Log, "[upgrade] step 1: waiting for FLO deployment %s rollout\n", floDep.Name)
			if err := k8swait.WaitForDeploymentReady(ctx, deps.K8s, floNS, floDep.Name, opts.Timeout); err != nil {
				return res, fmt.Errorf("wait for FLO deployment %s: %w", floDep.Name, err)
			}
			res.FLOUpgraded = true
			fmt.Fprintf(opts.Log, "[upgrade] step 1: FLO deployment upgrade complete\n")
		}
	}

	// Step 2: CNEInstance.
	if deps.Dyn == nil {
		return res, fmt.Errorf("upgrade: no Kubernetes dynamic client")
	}
	cne, err := findCNEInstance(ctx, deps.Dyn, opts.Namespace, opts.CNEInstanceName)
	if err != nil {
		return res, err
	}
	res.CNEInstance = cne.GetName()
	res.ManifestFrom = stringAt(cne.Object, "spec", "manifestVersion")
	patch, changed := cneInstancePatch(cne, target, opts.ServiceCIDR)
	patchJSON, err := json.Marshal(patch)
	if err != nil {
		return res, fmt.Errorf("marshal CNEInstance patch: %w", err)
	}
	switch {
	case !changed:
		fmt.Fprintf(opts.Log, "[upgrade] step 2: CNEInstance %s already at manifestVersion %s with %s=true\n", cne.GetName(), target, UseGatewaySettingsEnv)
	case opts.DryRun:
		fmt.Fprintf(opts.Log, "[upgrade] step 2: dry-run: would patch CNEInstance %s/%s (manifestVersion %q -> %q):\n%s\n", opts.Namespace, cne.GetName(), res.ManifestFrom, target, patchJSON)
	default:
		fmt.Fprintf(opts.Log, "[upgrade] step 2: patching CNEInstance %s/%s (manifestVersion %q -> %q, %s=true)\n", opts.Namespace, cne.GetName(), res.ManifestFrom, target, UseGatewaySettingsEnv)
		if _, err := deps.Dyn.Resource(CNEInstanceGVR).Namespace(opts.Namespace).Patch(ctx, cne.GetName(), types.MergePatchType, patchJSON, metav1.PatchOptions{FieldManager: upgradeFieldManager}); err != nil {
			return res, fmt.Errorf("patch CNEInstance %s: %w", cne.GetName(), err)
		}
		res.Patched = true
	}

	if opts.DryRun {
		fmt.Fprintf(opts.Log, "[upgrade] step 3: dry-run: would wait for CRD %s, CNEInstance CNEControllerAvailable + F5TmmAvailable, deploy %s and Ready TMM pods\n", InfraCRDName, ControllerDeployment)
		return res, nil
	}

	if opts.ReconcileControllerIdentity != nil {
		if err := opts.ReconcileControllerIdentity(ctx); err != nil {
			fmt.Fprintf(opts.Log, "[upgrade] warning: reconcile controller identity: %v\n", err)
		}
	}

	// Step 3: rollout.
	fmt.Fprintf(opts.Log, "[upgrade] step 3: waiting for CRD %s (up to %s)\n", InfraCRDName, opts.Timeout)
	if err := k8swait.WaitForCRDExists(ctx, deps.Dyn, InfraCRDName, opts.Timeout); err != nil {
		return res, fmt.Errorf("the Infra CRD did not appear (FLO did not roll the 2.4 manifest): %w", err)
	}
	if deps.K8s != nil {
		// 3a: FLO must render the env into the Deployment template. The
		// template, not the pods, is the signal: the pre-upgrade pods are
		// still Ready while FLO works, and the stale CNEInstance conditions
		// stay True, so nothing else distinguishes "not yet" from "done".
		fmt.Fprintf(opts.Log, "[upgrade] step 3: waiting for deploy %s/%s to carry %s=true (up to %s)\n", opts.Namespace, ControllerDeployment, UseGatewaySettingsEnv, opts.RenderWait)
		if err := waitControllerRendered(ctx, deps.K8s, opts.Namespace, opts.RenderWait, opts.Poll); err != nil {
			if !errors.Is(err, errControllerNotRendered) {
				return res, err
			}
			fmt.Fprintf(opts.Log, "[upgrade] step 3: FLO did not render the controller env in %s: recreating the CNEController CR so FLO takes the create path (the 2.3-era object rejects FLO 2.30 updates: spec.crdUpdater.resources/f5CsmQkview null)\n", opts.RenderWait)
			if err := recreateCNEController(ctx, deps.Dyn, opts.Namespace, cne.GetName()); err != nil {
				return res, err
			}
			res.ControllerRecreated = true
			if err := waitControllerRendered(ctx, deps.K8s, opts.Namespace, opts.Timeout, opts.Poll); err != nil {
				return res, fmt.Errorf("after recreating the CNEController: %w", err)
			}
			if opts.ReconcileControllerIdentity != nil {
				if err := opts.ReconcileControllerIdentity(ctx); err != nil {
					fmt.Fprintf(opts.Log, "[upgrade] warning: reconcile controller identity after recreate: %v\n", err)
				}
			}
		}
		fmt.Fprintf(opts.Log, "[upgrade] step 3: waiting for deploy %s/%s\n", opts.Namespace, ControllerDeployment)
		if err := k8swait.WaitForDeploymentReady(ctx, deps.K8s, opts.Namespace, ControllerDeployment, opts.Timeout); err != nil {
			return res, err
		}
		ok, err := controllerPodsHaveFlag(ctx, deps.K8s, opts.Namespace)
		if err != nil {
			return res, err
		}
		if !ok {
			return res, fmt.Errorf("deploy %s/%s rolled out but its pods do not carry %s=true", opts.Namespace, ControllerDeployment, UseGatewaySettingsEnv)
		}
		res.ControllerReady = true

		// 3b: EndpointSlice RBAC. Same supplement bnkctl up applies in phase
		// 23b; a cluster upgraded in place never ran it.
		if len(opts.ControllerRBAC) > 0 && deps.Applier != nil {
			applied, err := ensureControllerRBAC(ctx, deps, opts)
			if err != nil {
				return res, err
			}
			res.RBACApplied = applied
		}
	}
	for _, cond := range []string{"CNEControllerAvailable", "F5TmmAvailable"} {
		fmt.Fprintf(opts.Log, "[upgrade] step 3: waiting for CNEInstance %s %s=True\n", cne.GetName(), cond)
		if err := WaitConditionTrue(ctx, deps.Dyn, CNEInstanceGVR, opts.Namespace, cne.GetName(), cond, opts.Timeout, opts.Poll); err != nil {
			return res, err
		}
	}
	if deps.K8s != nil {
		fmt.Fprintf(opts.Log, "[upgrade] step 3: waiting for Ready TMM pods (%s)\n", TMMLabelSelector)
		ready, total, err := waitTMMReady(ctx, deps.K8s, opts.Namespace, opts.Timeout, opts.Poll)
		res.TMMReady, res.TMMTotal = ready, total
		if err != nil {
			return res, err
		}
		fmt.Fprintf(opts.Log, "[upgrade] step 3: controller carries %s=true; TMM pods ready %d/%d\n", UseGatewaySettingsEnv, ready, total)
	} else {
		res.ControllerReady = true
	}
	fmt.Fprintf(opts.Log, "[upgrade] done: FLO %s, CNEInstance %s manifestVersion %s\n", floChart, cne.GetName(), target)
	return res, nil
}

// findCNEInstance returns the named instance, or the only one in ns.
func findCNEInstance(ctx context.Context, dyn dynamic.Interface, ns, name string) (*unstructured.Unstructured, error) {
	ri := dyn.Resource(CNEInstanceGVR).Namespace(ns)
	if name != "" {
		obj, err := ri.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get CNEInstance %s/%s: %w", ns, name, err)
		}
		return obj, nil
	}
	list, err := ri.List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("CNEInstance CRD not served: is FLO installed?")
		}
		return nil, fmt.Errorf("list CNEInstances in %s: %w", ns, err)
	}
	switch len(list.Items) {
	case 0:
		return nil, fmt.Errorf("no CNEInstance in %s", ns)
	case 1:
		return &list.Items[0], nil
	default:
		names := make([]string, 0, len(list.Items))
		for i := range list.Items {
			names = append(names, list.Items[i].GetName())
		}
		return nil, fmt.Errorf("%d CNEInstances in %s (%v); pass --instance", len(names), ns, names)
	}
}

// cneInstancePatch builds the JSON merge patch for the target version and
// the env flags. Env lists are replaced whole (merge-patch semantics), so the
// existing entries are carried over. changed is false when nothing differs.
// serviceCIDR, when set, becomes the TMM env render.TMMK8sRoutesEnv (kept when
// already present).
func cneInstancePatch(cne *unstructured.Unstructured, target, serviceCIDR string) (map[string]any, bool) {
	changed := stringAt(cne.Object, "spec", "manifestVersion") != target

	ctrlEnv, _, _ := unstructured.NestedSlice(cne.Object, "spec", "advanced", "cneController", "env")
	ctrlEnv, c1 := upsertEnv(ctrlEnv, UseGatewaySettingsEnv, "true", true)
	ctrlEnv, c2 := upsertEnv(ctrlEnv, MaxActiveTMMEnv, maxActiveTMMDefault, false)
	tmmEnv, _, _ := unstructured.NestedSlice(cne.Object, "spec", "advanced", "tmm", "env")
	tmmEnv, c3 := upsertEnv(tmmEnv, zebosStateEnv, zebosStateLegacy, false)
	c4 := false
	if serviceCIDR != "" {
		tmmEnv, c4 = upsertEnv(tmmEnv, render.TMMK8sRoutesEnv, serviceCIDR, false)
	}
	changed = changed || c1 || c2 || c3 || c4

	return map[string]any{
		"spec": map[string]any{
			"manifestVersion": target,
			"advanced": map[string]any{
				"cneController": map[string]any{"env": ctrlEnv},
				"tmm":           map[string]any{"env": tmmEnv},
			},
		},
	}, changed
}

// upsertEnv sets name=value in a container env list. With overwrite false an
// existing entry keeps its value. changed reports whether the list differs.
func upsertEnv(env []any, name, value string, overwrite bool) ([]any, bool) {
	out := make([]any, 0, len(env)+1)
	found := false
	changed := false
	for _, e := range env {
		m, ok := e.(map[string]any)
		if !ok {
			out = append(out, e)
			continue
		}
		if m["name"] == name {
			found = true
			if overwrite && m["value"] != value {
				m = map[string]any{"name": name, "value": value}
				changed = true
			}
		}
		out = append(out, m)
	}
	if !found {
		out = append(out, map[string]any{"name": name, "value": value})
		changed = true
	}
	return out, changed
}

// controllerPodsHaveFlag reports whether every pod of the controller
// Deployment has USE_GATEWAY_SETTINGS=true in a container env.
func controllerPodsHaveFlag(ctx context.Context, cs kubernetes.Interface, ns string) (bool, error) {
	dep, err := cs.AppsV1().Deployments(ns).Get(ctx, ControllerDeployment, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("get deploy %s/%s: %w", ns, ControllerDeployment, err)
	}
	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return false, fmt.Errorf("deploy %s selector: %w", ControllerDeployment, err)
	}
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return false, fmt.Errorf("list controller pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return false, nil
	}
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil {
			continue
		}
		has := false
		for _, c := range p.Spec.Containers {
			for _, e := range c.Env {
				if e.Name == UseGatewaySettingsEnv && e.Value == "true" {
					has = true
				}
			}
		}
		if !has {
			return false, nil
		}
	}
	return true, nil
}

// tmmReady counts TMM pods whose containers are all Ready.
func tmmReady(ctx context.Context, cs kubernetes.Interface, ns string) (ready, total int, err error) {
	sel, err := labels.Parse(TMMLabelSelector)
	if err != nil {
		return 0, 0, err
	}
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return 0, 0, fmt.Errorf("list TMM pods: %w", err)
	}
	for _, p := range pods.Items {
		total++
		if len(p.Status.ContainerStatuses) == 0 {
			continue
		}
		all := true
		for _, cs := range p.Status.ContainerStatuses {
			if !cs.Ready {
				all = false
			}
		}
		if all {
			ready++
		}
	}
	return ready, total, nil
}

// errControllerNotRendered is returned by waitControllerRendered on timeout.
var errControllerNotRendered = errors.New("controller Deployment does not carry " + UseGatewaySettingsEnv + "=true")

// deploymentHasFlag reports whether the controller Deployment template has
// USE_GATEWAY_SETTINGS=true. found is false when the Deployment is absent.
func deploymentHasFlag(ctx context.Context, cs kubernetes.Interface, ns string) (found, has bool, err error) {
	dep, err := cs.AppsV1().Deployments(ns).Get(ctx, ControllerDeployment, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("get deploy %s/%s: %w", ns, ControllerDeployment, err)
	}
	for _, c := range dep.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == UseGatewaySettingsEnv && e.Value == "true" {
				return true, true, nil
			}
		}
	}
	return true, false, nil
}

// waitControllerRendered polls until the controller Deployment template
// carries the flag. Timeout returns errControllerNotRendered.
func waitControllerRendered(ctx context.Context, cs kubernetes.Interface, ns string, timeout, poll time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, has, err := deploymentHasFlag(ctx, cs, ns)
		if err != nil {
			return err
		}
		if has {
			return nil
		}
		if time.Now().After(deadline) {
			return errControllerNotRendered
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// recreateCNEController deletes the CNEController CR FLO owns for the
// instance (f5-cne-controller-<instance>, falling back to the one owned by
// the CNEInstance). FLO recreates it, and the Deployment it owns, from the
// 2.4 manifest.
func recreateCNEController(ctx context.Context, dyn dynamic.Interface, ns, cneName string) error {
	name := ControllerDeployment + "-" + cneName
	err := dyn.Resource(CNEControllerGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete CNEController %s/%s: %w", ns, name, err)
	}
	list, lerr := dyn.Resource(CNEControllerGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if lerr != nil {
		return fmt.Errorf("list CNEControllers in %s: %w", ns, lerr)
	}
	for i := range list.Items {
		for _, o := range list.Items[i].GetOwnerReferences() {
			if o.Kind == "CNEInstance" && o.Name == cneName {
				if err := dyn.Resource(CNEControllerGVR).Namespace(ns).Delete(ctx, list.Items[i].GetName(), metav1.DeleteOptions{}); err != nil {
					return fmt.Errorf("delete CNEController %s/%s: %w", ns, list.Items[i].GetName(), err)
				}
				return nil
			}
		}
	}
	return fmt.Errorf("no CNEController for CNEInstance %s/%s found to recreate", ns, cneName)
}

// waitTMMReady polls tmmReady until every TMM pod has all containers Ready.
func waitTMMReady(ctx context.Context, cs kubernetes.Interface, ns string, timeout, poll time.Duration) (ready, total int, err error) {
	deadline := time.Now().Add(timeout)
	for {
		ready, total, err = tmmReady(ctx, cs, ns)
		if err != nil {
			return ready, total, err
		}
		if total > 0 && ready == total {
			return ready, total, nil
		}
		if time.Now().After(deadline) {
			return ready, total, fmt.Errorf("TMM data plane not ready: %d/%d pods (%s) have every container Ready after %s", ready, total, TMMLabelSelector, timeout)
		}
		select {
		case <-ctx.Done():
			return ready, total, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// prefixWriter prefixes every write with a tag; it is used to route helper
// logs into the upgrade log.
type prefixWriter struct {
	w      io.Writer
	prefix string
}

func (p prefixWriter) Write(b []byte) (int, error) {
	if _, err := io.WriteString(p.w, p.prefix); err != nil {
		return 0, err
	}
	return p.w.Write(b)
}

// controllerCanGetEndpointSlices asks the API server whether the controller
// Deployment's ServiceAccount may get discovery.k8s.io endpointslices.
func controllerCanGetEndpointSlices(ctx context.Context, cs kubernetes.Interface, ns string) (bool, error) {
	dep, err := cs.AppsV1().Deployments(ns).Get(ctx, ControllerDeployment, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("get deploy %s/%s: %w", ns, ControllerDeployment, err)
	}
	sa := dep.Spec.Template.Spec.ServiceAccountName
	if sa == "" {
		sa = "default"
	}
	sar := &authv1.SubjectAccessReview{Spec: authv1.SubjectAccessReviewSpec{
		User: "system:serviceaccount:" + ns + ":" + sa,
		ResourceAttributes: &authv1.ResourceAttributes{
			Group: "discovery.k8s.io", Resource: "endpointslices", Verb: "get", Namespace: "default",
		},
	}}
	resp, err := cs.AuthorizationV1().SubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
	if err != nil {
		return false, fmt.Errorf("SubjectAccessReview for %s: %w", sa, err)
	}
	return resp.Status.Allowed, nil
}

// ensureControllerRBAC applies opts.ControllerRBAC when the controller SA
// cannot get endpointslices, then restarts the controller so its informers
// re-list with the new permission. Returns whether anything was applied.
func ensureControllerRBAC(ctx context.Context, deps UpgradeDeps, opts UpgradeOptions) (bool, error) {
	ok, err := controllerCanGetEndpointSlices(ctx, deps.K8s, opts.Namespace)
	if err != nil {
		return false, err
	}
	if ok {
		fmt.Fprintf(opts.Log, "[upgrade] step 3: controller ServiceAccount can get endpointslices\n")
		return false, nil
	}
	objs, err := decodeYAMLDocs(opts.ControllerRBAC)
	if err != nil {
		return false, fmt.Errorf("controller rbac: %w", err)
	}
	for _, obj := range objs {
		if err := deps.Applier.Apply(ctx, obj); err != nil {
			return false, err
		}
		fmt.Fprintf(opts.Log, "[upgrade] step 3: applied %s (EndpointSlice get for the controller; FLO 2.30 omits it)\n", describe(obj))
	}
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"awsbnkctl.f5.com/restartedAt":%q}}}}}`, time.Now().UTC().Format(time.RFC3339))
	if _, err := deps.K8s.AppsV1().Deployments(opts.Namespace).Patch(ctx, ControllerDeployment, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return true, fmt.Errorf("restart deploy %s/%s: %w", opts.Namespace, ControllerDeployment, err)
	}
	fmt.Fprintf(opts.Log, "[upgrade] step 3: restarted deploy %s/%s so the controller re-lists EndpointSlices\n", opts.Namespace, ControllerDeployment)
	if err := k8swait.WaitForDeploymentReady(ctx, deps.K8s, opts.Namespace, ControllerDeployment, opts.Timeout); err != nil {
		return true, err
	}
	return true, nil
}

// decodeYAMLDocs splits a YAML stream into unstructured objects.
func decodeYAMLDocs(data []byte) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	for _, doc := range strings.Split(string(data), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		m := map[string]any{}
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			return nil, err
		}
		if len(m) == 0 {
			continue
		}
		out = append(out, &unstructured.Unstructured{Object: m})
	}
	return out, nil
}
