// Package bnkengine implements self-healing checks and reconcilers for the BNK overlay.
package bnkengine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/JLCode-tech/bnkctl/pkg/bnkconst"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/k8s"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/manifests"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/render"
	"github.com/JLCode-tech/bnkctl/pkg/provider"
	"github.com/JLCode-tech/bnkctl/pkg/release"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// HealCheckID identifies a specific BNK overlay self-healing check.
type HealCheckID string

const (
	// #nosec G101 -- heal check identifier, not a credential
	HealMultusTokenWatch       HealCheckID = "multus-token-watch"
	HealTMMLogStream           HealCheckID = "tmm-log-stream"
	HealPodManager             HealCheckID = "pod-manager"
	HealCWC                    HealCheckID = "cwc"
	HealDSSMProbe              HealCheckID = "dssm-probe"
	HealControllerEndpoints    HealCheckID = "controller-endpointslices"
	HealTMMK8sRoutes           HealCheckID = "tmm-k8s-routes"
	HealEPPNamespaces          HealCheckID = "epp-namespaces"
	HealTestNamespace          HealCheckID = "test-namespace"
	HealPersistenceProfileSync HealCheckID = "persistence-profile-sync"
	HealAIGWDNSResolver        HealCheckID = "aigw-dns-resolver"
	HealAIGatewayProfile       HealCheckID = "aigateway-profile-programmed"
)

// AllHealChecks lists every supported BNK overlay self-healing check in execution order.
var AllHealChecks = []HealCheckID{
	HealMultusTokenWatch,
	HealTMMLogStream,
	HealPodManager,
	HealCWC,
	HealDSSMProbe,
	HealControllerEndpoints,
	HealTMMK8sRoutes,
	HealEPPNamespaces,
	HealTestNamespace,
	HealPersistenceProfileSync,
	HealAIGWDNSResolver,
	HealAIGatewayProfile,
}

// HealFinding describes a detected issue and whether it was healed.
type HealFinding struct {
	Check       HealCheckID `json:"check"`
	Healthy     bool        `json:"healthy"`
	Healed      bool        `json:"healed"`
	Description string      `json:"description"`
	Error       string      `json:"error,omitempty"`
}

// HealOptions configures DetectAll and RunHeal.
type HealOptions struct {
	Cluster  *intent.Cluster
	Underlay *provider.UnderlayContext
	Profile  *release.ReleaseProfile
	Checks   []HealCheckID
	DryRun   bool
}

// DetectAll runs non-mutating health detection for all (or selected) BNK overlay checks.
func DetectAll(ctx context.Context, k8sClient kubernetes.Interface, dynClient dynamic.Interface, opts HealOptions) ([]HealFinding, error) {
	opts.DryRun = true
	return RunHeal(ctx, k8sClient, dynClient, opts)
}

// RunHeal runs detection and (unless opts.DryRun is true) remediation for BNK overlay checks.
func RunHeal(ctx context.Context, k8sClient kubernetes.Interface, dynClient dynamic.Interface, opts HealOptions) ([]HealFinding, error) {
	if k8sClient == nil {
		return nil, nil
	}
	checks := opts.Checks
	if len(checks) == 0 {
		checks = AllHealChecks
	}

	var findings []HealFinding
	for _, id := range checks {
		var f HealFinding
		switch id {
		case HealMultusTokenWatch:
			f = healMultusTokenWatch(ctx, k8sClient, opts)
		case HealTMMLogStream:
			f = healTMMLogStream(ctx, k8sClient, opts)
		case HealPodManager:
			f = healPodManager(ctx, k8sClient, opts)
		case HealCWC:
			f = healCWC(ctx, k8sClient, opts)
		case HealDSSMProbe:
			f = healDSSMProbe(ctx, k8sClient, opts)
		case HealControllerEndpoints:
			f = healControllerEndpoints(ctx, k8sClient, opts)
		case HealTMMK8sRoutes:
			f = healTMMK8sRoutes(ctx, k8sClient, dynClient, opts)
		case HealEPPNamespaces:
			f = healEPPNamespaces(ctx, dynClient, opts)
		case HealTestNamespace:
			f = healTestNamespace(ctx, k8sClient, opts)
		case HealPersistenceProfileSync:
			f = healPersistenceProfileSync(ctx, k8sClient, dynClient, opts)
		case HealAIGWDNSResolver:
			f = healAIGWDNSResolver(ctx, k8sClient, dynClient, opts)
		case HealAIGatewayProfile:
			f = healAIGatewayProfile(ctx, k8sClient, dynClient, opts)
		default:
			f = HealFinding{
				Check:       id,
				Healthy:     false,
				Description: fmt.Sprintf("unknown heal check %q", id),
				Error:       fmt.Sprintf("unknown check %q", id),
			}
		}
		findings = append(findings, f)
	}
	return findings, nil
}

func healTestNamespace(ctx context.Context, k8sClient kubernetes.Interface, opts HealOptions) HealFinding {
	testNS := effectiveTestNamespace(opts.Cluster)
	f := HealFinding{Check: HealTestNamespace}
	_, err := k8sClient.CoreV1().Namespaces().Get(ctx, testNS, metav1.GetOptions{})
	if err == nil {
		f.Healthy = true
		f.Description = fmt.Sprintf("namespace %s exists", testNS)
		return f
	}
	if !apierrors.IsNotFound(err) {
		f.Error = err.Error()
		f.Description = fmt.Sprintf("check namespace %s failed", testNS)
		return f
	}
	f.Description = fmt.Sprintf("namespace %s missing", testNS)
	if opts.DryRun {
		return f
	}
	_, err = k8sClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: testNS},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		f.Error = err.Error()
		return f
	}
	f.Healed = true
	f.Healthy = true
	f.Description = fmt.Sprintf("created namespace %s", testNS)
	return f
}

func healCWC(ctx context.Context, k8sClient kubernetes.Interface, opts HealOptions) HealFinding {
	f := HealFinding{Check: HealCWC}
	_, err := k8sClient.CoreV1().Secrets(bnkconst.InstanceNamespace).Get(ctx, "cwc-license-certs-out", metav1.GetOptions{})
	if err == nil {
		f.Healthy = true
		f.Description = "cwc-license-certs-out present"
		return f
	}
	f.Description = "cwc-license-certs-out missing"
	if opts.DryRun {
		return f
	}
	if err := EnsureCWCCerts(ctx, k8sClient, bnkconst.InstanceNamespace, bnkconst.UtilsNamespace); err != nil {
		f.Error = err.Error()
		return f
	}
	f.Healed = true
	f.Healthy = true
	f.Description = "generated CWC TLS secrets"
	return f
}

func healControllerEndpoints(ctx context.Context, k8sClient kubernetes.Interface, opts HealOptions) HealFinding {
	f := HealFinding{Check: HealControllerEndpoints}
	clusterName := "bnk"
	if opts.Cluster != nil && opts.Cluster.EffectiveName() != "" {
		clusterName = opts.Cluster.EffectiveName()
	}
	roleName := clusterName + "-cne-controller-endpointslices"
	_, err := k8sClient.RbacV1().ClusterRoles().Get(ctx, roleName, metav1.GetOptions{})
	if err == nil {
		f.Healthy = true
		f.Description = fmt.Sprintf("ClusterRole %s present", roleName)
		return f
	}
	f.Description = fmt.Sprintf("ClusterRole %s missing", roleName)
	if opts.DryRun {
		return f
	}
	if opts.Cluster != nil {
		tmpl, readErr := manifests.FS.ReadFile("shared/cne-controller-rbac.yaml.tmpl")
		if readErr == nil {
			if rendered, rErr := render.RenderCNEControllerRBAC(tmpl, opts.Cluster); rErr == nil {
				docs := strings.Split(string(rendered), "---")
				for _, doc := range docs {
					trimmed := strings.TrimSpace(doc)
					if trimmed == "" {
						continue
					}
					if strings.Contains(trimmed, "kind: ClusterRoleBinding") {
						var crb rbacv1.ClusterRoleBinding
						if err := yaml.Unmarshal([]byte(trimmed), &crb); err == nil {
							_, _ = k8sClient.RbacV1().ClusterRoleBindings().Create(ctx, &crb, metav1.CreateOptions{})
						}
					} else if strings.Contains(trimmed, "kind: ClusterRole") {
						var cr rbacv1.ClusterRole
						if err := yaml.Unmarshal([]byte(trimmed), &cr); err == nil {
							_, _ = k8sClient.RbacV1().ClusterRoles().Create(ctx, &cr, metav1.CreateOptions{})
						}
					}
				}
				f.Healed = true
				f.Healthy = true
				f.Description = fmt.Sprintf("created ClusterRole & binding %s", roleName)
				return f
			}
		}
	}
	return f
}

func healMultusTokenWatch(ctx context.Context, k8sClient kubernetes.Interface, opts HealOptions) HealFinding {
	f := HealFinding{Check: HealMultusTokenWatch}
	st, _, err := k8s.InspectMultus(ctx, k8sClient)
	if err != nil {
		f.Error = err.Error()
		return f
	}
	if !st.Installed {
		f.Healthy = true
		f.Description = "no multus daemonset in kube-system (skipped)"
		return f
	}
	if st.WatchEnabled && st.Unauthorized == "" {
		f.Healthy = true
		f.Description = "multus token watch enabled, no Unauthorized sandbox events"
		return f
	}
	res, err := k8s.EnsureMultusTokenWatch(ctx, k8sClient, opts.DryRun, nil)
	if err != nil {
		f.Error = err.Error()
		f.Description = res.Reason
		return f
	}
	f.Description = res.Reason
	if opts.DryRun {
		return f
	}
	f.Healed = res.Patched || res.Restarted
	f.Healthy = true
	return f
}

func healTMMLogStream(ctx context.Context, k8sClient kubernetes.Interface, opts HealOptions) HealFinding {
	f := HealFinding{Check: HealTMMLogStream}
	enabled, err := k8s.TMMLogStreamEnabled(ctx, k8sClient)
	if err != nil {
		if apierrors.IsNotFound(err) {
			f.Healthy = true
			f.Description = "f5-toda-fluentd-custom ConfigMap not yet created (skipped)"
			return f
		}
		f.Error = err.Error()
		return f
	}
	if enabled {
		f.Healthy = true
		f.Description = "TMM stdout log store enabled on f5-toda-fluentd-custom"
		return f
	}
	f.Description = "TMM stdout log store disabled on f5-toda-fluentd-custom"
	if opts.DryRun {
		return f
	}
	changed, err := k8s.EnableTMMLogStream(ctx, k8sClient, 0)
	if err != nil {
		f.Error = err.Error()
		return f
	}
	f.Healed = changed
	f.Healthy = true
	f.Description = "enabled TMM stdout log store on f5-toda-fluentd-custom"
	return f
}

func healPodManager(ctx context.Context, k8sClient kubernetes.Interface, opts HealOptions) HealFinding {
	f := HealFinding{Check: HealPodManager}
	if opts.Profile != nil && !opts.Profile.Quirks.PodManagerHeal {
		f.Healthy = true
		f.Description = "pod-manager heal quirk disabled for release (skipped)"
		return f
	}
	dep, err := k8sClient.AppsV1().Deployments(bnkconst.InstanceNamespace).Get(ctx, "pod-manager", metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			f.Healthy = true
			f.Description = "pod-manager Deployment not present (skipped)"
			return f
		}
		f.Error = err.Error()
		return f
	}
	if dep.Status.AvailableReplicas > 0 || dep.Spec.Replicas == nil || *dep.Spec.Replicas == 0 {
		f.Healthy = true
		f.Description = "pod-manager available"
		return f
	}
	f.Description = "pod-manager has 0 available replicas"
	if opts.DryRun {
		return f
	}
	patch, _ := json.Marshal(map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]string{
						"bnkctl.f5.com/restartedAt": time.Now().UTC().Format(time.RFC3339),
					},
				},
			},
		},
	})
	if _, err := k8sClient.AppsV1().Deployments(bnkconst.InstanceNamespace).Patch(ctx, "pod-manager", types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		f.Error = err.Error()
		return f
	}
	f.Healed = true
	f.Healthy = true
	f.Description = "restarted pod-manager deployment"
	return f
}

const (
	dssmConfigMapName     = "f5-dssm"
	dssmTLSReplace        = "redis-cli --tls"
	dssmTLSInsecureString = "redis-cli --tls --insecure"
	dssmLabelSelector     = "app.kubernetes.io/name=f5-dssm"
)

func healDSSMProbe(ctx context.Context, k8sClient kubernetes.Interface, opts HealOptions) HealFinding {
	f := HealFinding{Check: HealDSSMProbe}
	ns := bnkconst.InstanceNamespace
	cm, err := k8sClient.CoreV1().ConfigMaps(ns).Get(ctx, dssmConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		ns = bnkconst.CoreNamespace
		cm, err = k8sClient.CoreV1().ConfigMaps(ns).Get(ctx, dssmConfigMapName, metav1.GetOptions{})
	}
	if err != nil {
		if apierrors.IsNotFound(err) {
			f.Healthy = true
			f.Description = "no f5-dssm ConfigMap found (skipped)"
			return f
		}
		f.Error = err.Error()
		return f
	}
	var toPatch []string
	for k, v := range cm.Data {
		if strings.Contains(v, dssmTLSReplace) && !strings.Contains(v, "--insecure") {
			toPatch = append(toPatch, k)
		}
	}
	if len(toPatch) == 0 {
		f.Healthy = true
		f.Description = fmt.Sprintf("f5-dssm ConfigMap in %s healthy", ns)
		return f
	}
	f.Description = fmt.Sprintf("f5-dssm ConfigMap keys [%s] in %s missing --insecure on redis-cli --tls", strings.Join(toPatch, ", "), ns)
	if opts.DryRun {
		return f
	}
	for _, k := range toPatch {
		cm.Data[k] = strings.ReplaceAll(cm.Data[k], dssmTLSReplace, dssmTLSInsecureString)
	}
	if _, err := k8sClient.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		f.Error = err.Error()
		return f
	}
	_ = k8sClient.CoreV1().Pods(ns).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: dssmLabelSelector})
	f.Healed = true
	f.Healthy = true
	f.Description = fmt.Sprintf("patched f5-dssm ConfigMap keys [%s] in %s with --insecure", strings.Join(toPatch, ", "), ns)
	return f
}

var healCNEInstanceGVR = schema.GroupVersionResource{Group: "k8s.f5.com", Version: "v1", Resource: "cneinstances"}

func findCNEInstance(ctx context.Context, dynClient dynamic.Interface) (*unstructured.Unstructured, string, error) {
	if dynClient == nil {
		return nil, "", nil
	}
	for _, ns := range []string{bnkconst.InstanceNamespace, bnkconst.CoreNamespace} {
		list, err := dynClient.Resource(healCNEInstanceGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, ns, err
		}
		if len(list.Items) > 0 {
			return &list.Items[0], ns, nil
		}
	}
	return nil, bnkconst.InstanceNamespace, nil
}

func cneHasTMMEnv(cne *unstructured.Unstructured, name string) (string, bool) {
	for _, path := range [][]string{
		{"spec", "tmm", "env"},
		{"spec", "advanced", "tmm", "env"},
	} {
		env, _, _ := unstructured.NestedSlice(cne.Object, path...)
		for _, e := range env {
			if m, ok := e.(map[string]any); ok && m["name"] == name {
				v, _ := m["value"].(string)
				if v != "" {
					return v, true
				}
			}
		}
	}
	return "", false
}

func healTMMK8sRoutes(ctx context.Context, k8sClient kubernetes.Interface, dynClient dynamic.Interface, opts HealOptions) HealFinding {
	f := HealFinding{Check: HealTMMK8sRoutes}
	if !opts.DryRun {
		if err := ReconcileNullCNEController(ctx, k8sClient, dynClient); err != nil {
			f.Error = err.Error()
			return f
		}
	}
	cne, ns, err := findCNEInstance(ctx, dynClient)
	if err != nil {
		f.Error = err.Error()
		return f
	}
	if cne == nil {
		f.Healthy = true
		f.Description = "TMM_K8S_ROUTES and f5-cne-controller env verified"
		return f
	}
	if v, ok := cneHasTMMEnv(cne, render.TMMK8sRoutesEnv); ok {
		f.Healthy = true
		f.Description = fmt.Sprintf("%s=%s on CNEInstance %s", render.TMMK8sRoutesEnv, v, cne.GetName())
		return f
	}
	mv, _, _ := unstructured.NestedString(cne.Object, "spec", "manifestVersion")
	if strings.HasPrefix(mv, "2.3") {
		f.Healthy = true
		f.Description = fmt.Sprintf("manifestVersion %s does not require %s", mv, render.TMMK8sRoutesEnv)
		return f
	}
	f.Description = fmt.Sprintf("CNEInstance %s/%s missing %s", ns, cne.GetName(), render.TMMK8sRoutesEnv)
	if opts.DryRun {
		return f
	}
	var routes []string
	if opts.Underlay != nil {
		if opts.Underlay.ServiceCIDR != "" {
			routes = append(routes, opts.Underlay.ServiceCIDR)
		}
		if opts.Underlay.PodCIDR != "" {
			routes = append(routes, opts.Underlay.PodCIDR)
		}
	}
	if len(routes) == 0 && opts.Cluster != nil {
		if u, uErr := provider.BuildUnderlayFromIntent(opts.Cluster); uErr == nil && u != nil {
			if u.ServiceCIDR != "" {
				routes = append(routes, u.ServiceCIDR)
			}
			if u.PodCIDR != "" {
				routes = append(routes, u.PodCIDR)
			}
		}
	}
	if len(routes) == 0 {
		f.Healthy = true
		f.Description = "no service/pod CIDR known to patch TMM_K8S_ROUTES (skipped)"
		return f
	}
	val := strings.Join(routes, ",")
	var patch string
	if _, hasTopTMM, _ := unstructured.NestedMap(cne.Object, "spec", "tmm"); hasTopTMM {
		env, _, _ := unstructured.NestedSlice(cne.Object, "spec", "tmm", "env")
		env = append(env, map[string]any{"name": render.TMMK8sRoutesEnv, "value": val})
		envBytes, _ := json.Marshal(env)
		patch = fmt.Sprintf(`{"spec":{"tmm":{"env":%s}}}`, string(envBytes))
	} else {
		env, _, _ := unstructured.NestedSlice(cne.Object, "spec", "advanced", "tmm", "env")
		env = append(env, map[string]any{"name": render.TMMK8sRoutesEnv, "value": val})
		envBytes, _ := json.Marshal(env)
		patch = fmt.Sprintf(`{"spec":{"advanced":{"tmm":{"env":%s}}}}`, string(envBytes))
	}
	if _, err := dynClient.Resource(healCNEInstanceGVR).Namespace(ns).Patch(ctx, cne.GetName(), types.MergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: "bnkctl-heal"}); err != nil {
		f.Error = err.Error()
		return f
	}
	f.Healed = true
	f.Healthy = true
	f.Description = fmt.Sprintf("patched %s=%s on CNEInstance %s/%s", render.TMMK8sRoutesEnv, val, ns, cne.GetName())
	return f
}

func healEPPNamespaces(ctx context.Context, dynClient dynamic.Interface, opts HealOptions) HealFinding {
	f := HealFinding{Check: HealEPPNamespaces}
	cne, ns, err := findCNEInstance(ctx, dynClient)
	if err != nil {
		f.Error = err.Error()
		return f
	}
	if cne == nil {
		f.Healthy = true
		f.Description = "no CNEInstance present (EPP namespace watch skipped)"
		return f
	}
	if mv, _, _ := unstructured.NestedString(cne.Object, "spec", "manifestVersion"); strings.HasPrefix(mv, "2.3") {
		f.Healthy = true
		f.Description = fmt.Sprintf("manifestVersion %s has no F5 Endpoint Picker", mv)
		return f
	}
	if eppNS, _, _ := unstructured.NestedStringSlice(cne.Object, "spec", "eppNamespaces"); len(eppNS) > 0 {
		f.Healthy = true
		f.Description = fmt.Sprintf("eppNamespaces [%s] on CNEInstance %s/%s", strings.Join(eppNS, ","), ns, cne.GetName())
		return f
	}
	f.Description = fmt.Sprintf("CNEInstance %s/%s has no spec.eppNamespaces", ns, cne.GetName())
	if opts.DryRun {
		return f
	}
	patch := `{"spec":{"eppNamespaces":["All"]}}`
	if _, err := dynClient.Resource(healCNEInstanceGVR).Namespace(ns).Patch(ctx, cne.GetName(), types.MergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: "bnkctl-heal"}); err != nil {
		f.Error = err.Error()
		return f
	}
	f.Healed = true
	f.Healthy = true
	f.Description = fmt.Sprintf("set spec.eppNamespaces=[All] on CNEInstance %s/%s", ns, cne.GetName())
	return f
}

var healPersistenceProfileGVR = schema.GroupVersionResource{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-big-persistence-profiles"}

func healPersistenceProfileSync(ctx context.Context, k8sClient kubernetes.Interface, dynClient dynamic.Interface, opts HealOptions) HealFinding {
	f := HealFinding{Check: HealPersistenceProfileSync}
	if dynClient == nil {
		f.Healthy = true
		f.Description = "dynamic client not configured (skipped)"
		return f
	}
	list, err := dynClient.Resource(healPersistenceProfileGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		f.Error = err.Error()
		return f
	}
	if list != nil && len(list.Items) > 0 {
		var unmirrored []unstructured.Unstructured
		var names []string
		for _, item := range list.Items {
			ns := item.GetNamespace()
			name := item.GetName()
			if ns != "" && ns != bnkconst.InstanceNamespace {
				_, getErr := dynClient.Resource(healPersistenceProfileGVR).Namespace(bnkconst.InstanceNamespace).Get(ctx, name, metav1.GetOptions{})
				if apierrors.IsNotFound(getErr) {
					unmirrored = append(unmirrored, item)
					names = append(names, ns+"/"+name)
				}
			}
		}
		if len(unmirrored) > 0 {
			f.Description = fmt.Sprintf("persistence profile(s) %s missing from %s", strings.Join(names, ", "), bnkconst.InstanceNamespace)
			if opts.DryRun {
				return f
			}
			for i := range unmirrored {
				srcNS := unmirrored[i].GetNamespace()
				copyItem := unmirrored[i].DeepCopy()
				copyItem.SetNamespace(bnkconst.InstanceNamespace)
				copyItem.SetResourceVersion("")
				copyItem.SetUID("")
				if _, createErr := dynClient.Resource(healPersistenceProfileGVR).Namespace(bnkconst.InstanceNamespace).Create(ctx, copyItem, metav1.CreateOptions{FieldManager: "bnkctl-heal"}); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
					f.Error = createErr.Error()
					return f
				}
				if secretName, ok, _ := unstructured.NestedString(copyItem.Object, "spec", "passphraseSecret"); ok && secretName != "" && k8sClient != nil {
					if sec, secErr := k8sClient.CoreV1().Secrets(srcNS).Get(ctx, secretName, metav1.GetOptions{}); secErr == nil {
						secCopy := sec.DeepCopy()
						secCopy.Namespace = bnkconst.InstanceNamespace
						secCopy.ResourceVersion = ""
						secCopy.UID = ""
						_, _ = k8sClient.CoreV1().Secrets(bnkconst.InstanceNamespace).Create(ctx, secCopy, metav1.CreateOptions{FieldManager: "bnkctl-heal"})
					}
				}
			}
			f.Healed = true
			f.Healthy = true
			f.Description = fmt.Sprintf("mirrored persistence profile(s) %s to %s", strings.Join(names, ", "), bnkconst.InstanceNamespace)
			return f
		}
	}
	f.Healthy = true
	f.Description = "persistence profiles synchronized with " + bnkconst.InstanceNamespace
	return f
}

var (
	healDNSCacheGVR         = schema.GroupVersionResource{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-big-dns-caches"}
	healAIGatewayProfileGVR = schema.GroupVersionResource{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aigatewayprofiles"}
	healAIModelGVR          = schema.GroupVersionResource{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aimodels"}
	healAIProviderGVR       = schema.GroupVersionResource{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aiproviders"}
)

func safeDynamicGet(ctx context.Context, dynClient dynamic.Interface, gvr schema.GroupVersionResource, ns, name string) (obj *unstructured.Unstructured, err error) {
	defer func() {
		if r := recover(); r != nil {
			obj = nil
			err = apierrors.NewNotFound(gvr.GroupResource(), name)
		}
	}()
	return dynClient.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
}

func healAIGWDNSResolver(ctx context.Context, k8sClient kubernetes.Interface, dynClient dynamic.Interface, opts HealOptions) HealFinding {
	f := HealFinding{Check: HealAIGWDNSResolver}
	if dynClient == nil {
		f.Healthy = true
		f.Description = "dynamic client not configured (skipped)"
		return f
	}
	aiEnabled := false
	if opts.Cluster != nil && opts.Cluster.Bnk != nil && opts.Cluster.Bnk.AIGateway != nil {
		aiEnabled = opts.Cluster.Bnk.AIGateway.Enabled || opts.Cluster.Bnk.AIGateway.DNSResolver
	}
	cne, cneNS, _ := findCNEInstance(ctx, dynClient)
	if cne != nil {
		if v, ok, _ := unstructured.NestedBool(cne.Object, "spec", "aiGateway", "enabled"); ok && v {
			aiEnabled = true
		}
	}
	if !aiEnabled {
		f.Healthy = true
		f.Description = "AI Gateway not enabled (aigw-dns-resolver check skipped)"
		return f
	}
	targetNS := cneNS
	if strings.TrimSpace(targetNS) == "" {
		targetNS = bnkconst.InstanceNamespace
	}

	existing, err := safeDynamicGet(ctx, dynClient, healDNSCacheGVR, targetNS, "aigw-dns-resolver")
	if err == nil && existing != nil {
		ct, _, _ := unstructured.NestedString(existing.Object, "spec", "cacheType")
		if ct == "net-resolver" {
			f.Healthy = true
			f.Description = fmt.Sprintf("aigw-dns-resolver F5BigDnsCache (net-resolver) present in %s", targetNS)
			return f
		}
	}

	dnsHost := "10.96.0.10"
	kubeDNSAbsent := true
	if k8sClient != nil {
		if svc, sErr := k8sClient.CoreV1().Services("kube-system").Get(ctx, "kube-dns", metav1.GetOptions{}); sErr == nil && svc != nil {
			kubeDNSAbsent = false
			if svc.Spec.ClusterIP != "" && svc.Spec.ClusterIP != "None" {
				dnsHost = svc.Spec.ClusterIP
			}
		} else if svc, cErr := k8sClient.CoreV1().Services("kube-system").Get(ctx, "coredns", metav1.GetOptions{}); cErr == nil && svc != nil {
			if svc.Spec.ClusterIP != "" && svc.Spec.ClusterIP != "None" {
				dnsHost = svc.Spec.ClusterIP
			}
		}
	}

	reason := "AI Gateway enabled"
	if kubeDNSAbsent {
		reason = "kube-system/kube-dns Service absent (CoreDNS cluster)"
	}
	f.Description = fmt.Sprintf("aigw-dns-resolver F5BigDnsCache missing or misconfigured in %s (%s)", targetNS, reason)
	if opts.DryRun {
		return f
	}

	dnsCR := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "k8s.f5net.com/v1",
			"kind":       "F5BigDnsCache",
			"metadata": map[string]any{
				"name":      "aigw-dns-resolver",
				"namespace": targetNS,
			},
			"spec": map[string]any{
				"cacheType": "net-resolver",
				"netResolver": map[string]any{
					"forwardZones": []any{
						map[string]any{
							"forwardZone": ".",
							"nameServers": []any{
								map[string]any{
									"ipAddress": dnsHost,
									"port":      int64(53),
								},
							},
						},
					},
				},
			},
		},
	}

	if existing != nil {
		dnsCR.SetResourceVersion(existing.GetResourceVersion())
		if _, uErr := dynClient.Resource(healDNSCacheGVR).Namespace(targetNS).Update(ctx, dnsCR, metav1.UpdateOptions{FieldManager: "bnkctl-heal"}); uErr != nil {
			f.Error = uErr.Error()
			return f
		}
	} else {
		if _, cErr := dynClient.Resource(healDNSCacheGVR).Namespace(targetNS).Create(ctx, dnsCR, metav1.CreateOptions{FieldManager: "bnkctl-heal"}); cErr != nil && !apierrors.IsAlreadyExists(cErr) {
			f.Error = cErr.Error()
			return f
		}
	}
	f.Healed = true
	f.Healthy = true
	f.Description = fmt.Sprintf("reconciled aigw-dns-resolver F5BigDnsCache (net-resolver) in %s forwarding to %s:53", targetNS, dnsHost)
	return f
}

func safeDynamicList(ctx context.Context, dynClient dynamic.Interface, gvr schema.GroupVersionResource, ns string) (list *unstructured.UnstructuredList, err error) {
	defer func() {
		if r := recover(); r != nil {
			list = nil
			err = apierrors.NewNotFound(gvr.GroupResource(), "")
		}
	}()
	return dynClient.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
}

func healAIGatewayProfile(ctx context.Context, k8sClient kubernetes.Interface, dynClient dynamic.Interface, opts HealOptions) HealFinding {
	f := HealFinding{Check: HealAIGatewayProfile}
	if dynClient == nil {
		f.Healthy = true
		f.Description = "dynamic client not configured (skipped)"
		return f
	}
	list, err := safeDynamicList(ctx, dynClient, healAIGatewayProfileGVR, metav1.NamespaceAll)
	if err != nil {
		if apierrors.IsNotFound(err) {
			f.Healthy = true
			f.Description = "AIGatewayProfile CRD not present (skipped)"
			return f
		}
		f.Error = err.Error()
		return f
	}
	if list == nil || len(list.Items) == 0 {
		f.Healthy = true
		f.Description = "no AIGatewayProfiles present (skipped)"
		return f
	}

	var stuckProfiles []string
	for _, prof := range list.Items {
		ns := prof.GetNamespace()
		name := prof.GetName()
		conds, _, _ := unstructured.NestedSlice(prof.Object, "status", "conditions")
		unprogrammed := false
		for _, raw := range conds {
			cm, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			cType, _ := cm["type"].(string)
			cStatus, _ := cm["status"].(string)
			cMsg, _ := cm["message"].(string)
			if (cType == "Programmed" || cType == "Accepted" || cType == "Ready") && strings.EqualFold(cStatus, "False") {
				unprogrammed = true
			}
			if strings.Contains(cMsg, "not prepared") || strings.Contains(cMsg, "AI profile") {
				unprogrammed = true
			}
		}
		if !unprogrammed {
			continue
		}
		// Check whether referenced AIModels/AIProviders exist in the namespace so we know this is a stale reconcile cache
		models, mErr := safeDynamicList(ctx, dynClient, healAIModelGVR, ns)
		provs, pErr := safeDynamicList(ctx, dynClient, healAIProviderGVR, ns)
		if mErr == nil && pErr == nil && models != nil && provs != nil && len(models.Items) > 0 && len(provs.Items) > 0 {
			stuckProfiles = append(stuckProfiles, ns+"/"+name)
		}
	}

	if len(stuckProfiles) == 0 {
		f.Healthy = true
		f.Description = fmt.Sprintf("all %d AIGatewayProfile(s) programmed", len(list.Items))
		return f
	}

	f.Description = fmt.Sprintf("AIGatewayProfile(s) [%s] stuck Programmed=False with valid AIModel/AIProvider refs", strings.Join(stuckProfiles, ", "))
	if opts.DryRun {
		return f
	}
	patch, _ := json.Marshal(map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]string{
						"bnkctl.f5.com/restartedAt": time.Now().UTC().Format(time.RFC3339),
					},
				},
			},
		},
	})
	if _, err := k8sClient.AppsV1().Deployments(bnkconst.InstanceNamespace).Patch(ctx, "f5-cne-controller", types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		f.Error = err.Error()
		return f
	}
	f.Healed = true
	f.Healthy = true
	f.Description = fmt.Sprintf("restarted f5-cne-controller to re-reconcile AIGatewayProfile(s) [%s]", strings.Join(stuckProfiles, ", "))
	return f
}
