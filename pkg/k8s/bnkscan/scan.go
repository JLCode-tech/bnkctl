package bnkscan

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Options tunes Scan.
type Options struct {
	// ControllerNamespace and ControllerName locate the f5-cne-controller
	// Deployment. Empty values use the awsbnkctl defaults.
	ControllerNamespace string
	ControllerName      string
	// TMMName is the TMM DaemonSet in ControllerNamespace (default f5-tmm).
	TMMName string
	// AcceptLegacy makes CheckReadiness treat a 2.3 cluster as ready when its
	// controller is up (an Infra CR is only required on 2.4). Lifecycle gates
	// that run on either generation set it; operator surfaces leave it false.
	AcceptLegacy bool
}

func (o Options) withDefaults() Options {
	if o.ControllerNamespace == "" {
		o.ControllerNamespace = DefaultControllerNamespace
	}
	if o.ControllerName == "" {
		o.ControllerName = DefaultControllerName
	}
	if o.TMMName == "" {
		o.TMMName = DefaultTMMName
	}
	return o
}

// raw holds the live objects Scan read, for DiscoverMCP.
type raw struct {
	gateways     []*unstructured.Unstructured
	classes      []*unstructured.Unstructured
	httpRoutes   []*unstructured.Unstructured
	netPolicies  []*unstructured.Unstructured
	secPolicies  []*unstructured.Unstructured
	persistence  []*unstructured.Unstructured
	gatewayReady map[string]bool // namespace/name → Ready
}

// Scan indexes the cluster. cs may be nil, in which case the controller
// rollout is reported as not found. A CRD the API server does not serve is
// recorded in Index.Missing; any other API error aborts the scan.
func Scan(ctx context.Context, dyn dynamic.Interface, cs kubernetes.Interface, opts Options) (*Index, error) {
	opts = opts.withDefaults()
	idx := &Index{Controller: Controller{Namespace: opts.ControllerNamespace, Name: opts.ControllerName}, TMM: TMM{Namespace: opts.ControllerNamespace, Name: opts.TMMName}}
	r := &raw{gatewayReady: map[string]bool{}}

	groups, servedCRDs, crdsListed, err := f5CRDGroups(ctx, dyn)
	if err != nil {
		return nil, err
	}
	idx.Groups = groups
	idx.Generation = DetectGeneration(groups)

	list := func(kind string, gvr schema.GroupVersionResource) ([]*unstructured.Unstructured, error) {
		items, err := listAll(ctx, dyn, gvr)
		if err != nil {
			if apierrors.IsNotFound(err) {
				idx.Missing = append(idx.Missing, kind)
				return nil, nil
			}
			return nil, fmt.Errorf("list %s (%s): %w", kind, gvr.GroupResource(), err)
		}
		return items, nil
	}

	// 2.4 kinds.
	type kindSpec struct {
		kind  string
		gvr   schema.GroupVersionResource
		ready func(*unstructured.Unstructured) (bool, string)
		dest  *[]Object
	}
	specs := []kindSpec{
		{"Infra", InfraGVR, requireTrue("Programmed"), &idx.Infras},
		{"GatewaySettings", GatewaySettingsGVR, trueIfPresent("Accepted", "Programmed"), &idx.GatewaySettings},
		{"EgressGateway", EgressGatewayGVR, trueIfPresent("Accepted", "Programmed"), &idx.EgressGateways},
		{"SecPolicy", SecPolicyGVR, trueIfPresent("Accepted", "Programmed"), &idx.SecPolicies},
		{"NetPolicy", NetPolicyGVR, trueIfPresent("Accepted", "Programmed"), &idx.NetPolicies},
		{"F5BigPersistenceProfile", PersistenceProfileGVR, requireTrue("Programmed"), &idx.PersistenceProfiles},
	}
	keep := map[string][]*unstructured.Unstructured{}
	for _, s := range specs {
		items, err := list(s.kind, s.gvr)
		if err != nil {
			return nil, err
		}
		keep[s.kind] = items
		for _, it := range items {
			*s.dest = append(*s.dest, toObject(it, s.ready))
		}
	}
	r.netPolicies = keep["NetPolicy"]
	r.secPolicies = keep["SecPolicy"]
	r.persistence = keep["F5BigPersistenceProfile"]

	// F5 CNF kinds (Cloud-Native Network Functions): scanned when CRDs exist, never required.
	type cnfKindSpec struct {
		kind    string
		crdName string
		gvr     schema.GroupVersionResource
		ready   func(*unstructured.Unstructured) (bool, string)
		dest    *[]Object
	}
	cnfSpecs := []cnfKindSpec{
		{"F5BigDnsCache", "f5-big-dns-caches.k8s.f5net.com", DNSCacheGVR, trueIfPresent("Programmed"), &idx.DNSCaches},
		{"F5BigDnsApp", "f5-big-dns-apps.k8s.f5net.com", DNSAppGVR, trueIfPresent("Programmed"), &idx.DNSApps},
		{"F5BigDnsZone", "f5-big-dns-zones.k8s.f5net.com", DNSZoneGVR, trueIfPresent("Programmed"), &idx.DNSZones},
		{"F5BigCnatPolicy", "f5-big-cnat-policies.k8s.f5net.com", CGNATPolicyGVR, trueIfPresent("Programmed"), &idx.CGNATPolicies},
		{"F5DiameterEndpoint", "f5-diameter-endpoints.k8s.f5net.com", DiameterEndpointGVR, trueIfPresent("Programmed"), &idx.DiameterEndpoints},
		{"AppPolicy", "apppolicies.gateway.k8s.f5.com", AppPolicyGVR, trueIfPresent("Accepted", "Programmed"), &idx.AppPolicies},
		{"L4GlobalPolicy", "l4globalpolicies.gateway.k8s.f5.com", L4GlobalPolicyGVR, trueIfPresent("Accepted", "Programmed"), &idx.L4GlobalPolicies},
		{"AITenant", "aitenants.inference.k8s.f5.com", AITenantGVR, trueIfPresent("Accepted", "Programmed"), &idx.AITenants},
		{"AICredential", "aicredentials.inference.k8s.f5.com", AICredentialGVR, trueIfPresent("Accepted", "Programmed"), &idx.AICredentials},
		{"AIProvider", "aiproviders.inference.k8s.f5.com", AIProviderGVR, trueIfPresent("Accepted", "Programmed"), &idx.AIProviders},
		{"AIModel", "aimodels.inference.k8s.f5.com", AIModelGVR, trueIfPresent("Accepted", "Programmed"), &idx.AIModels},
		{"AIGatewayProfile", "aigatewayprofiles.inference.k8s.f5.com", AIGatewayProfileGVR, trueIfPresent("Accepted", "Programmed"), &idx.AIGatewayProfiles},
		{"AIRoute", "airoutes.inference.k8s.f5.com", AIRouteGVR, trueIfPresent("Accepted", "Programmed"), &idx.AIRoutes},
		{"AIRoutePolicy", "airoutepolicies.inference.k8s.f5.com", AIRoutePolicyGVR, trueIfPresent("Accepted", "Programmed"), &idx.AIRoutePolicies},
		{"AIGateway", "aigateways.inference.k8s.f5.com", AIGatewayGVR, trueIfPresent("Accepted", "Programmed"), &idx.AIGateways},
	}
	for _, s := range cnfSpecs {
		// Only list if the CRD was confirmed served, or if CRDs couldn't be listed.
		if crdsListed && !servedCRDs[s.crdName] {
			continue
		}
		items, err := listAll(ctx, dyn, s.gvr)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("list %s (%s): %w", s.kind, s.gvr.GroupResource(), err)
		}
		for _, it := range items {
			*s.dest = append(*s.dest, toObject(it, s.ready))
		}
	}
	for _, dc := range idx.DNSCaches {
		if dc.Ready {
			idx.DNSResolverReady = true
			break
		}
	}

	// Gateway API: keep the Gateways whose class belongs to a BNK controller.
	classes, err := list("GatewayClass", GatewayClassGVR)
	if err != nil {
		return nil, err
	}
	r.classes = classes
	bnkClasses := bnkGatewayClasses(classes)
	gateways, err := list("Gateway", GatewayGVR)
	if err != nil {
		return nil, err
	}
	for _, gw := range gateways {
		cls, _, _ := unstructured.NestedString(gw.Object, "spec", "gatewayClassName")
		if len(bnkClasses) > 0 && !bnkClasses[cls] {
			continue
		}
		o := toObject(gw, requireTrue("Accepted", "Programmed"))
		idx.Gateways = append(idx.Gateways, o)
		r.gateways = append(r.gateways, gw)
		r.gatewayReady[o.ID()] = o.Ready
	}
	routes, err := list("HTTPRoute", HTTPRouteGVR)
	if err != nil {
		return nil, err
	}
	r.httpRoutes = routes
	for _, rt := range routes {
		idx.HTTPRoutes = append(idx.HTTPRoutes, toObject(rt, routeReady))
	}

	// Legacy kinds: counted, never required.
	idx.Legacy.BnkGateways = countAll(ctx, dyn, BnkGatewayGVR)
	idx.Legacy.BNKSecPolicy = countAll(ctx, dyn, BNKSecPolGVR)
	idx.Legacy.BNKNetPolicy = countAll(ctx, dyn, BNKNetPolGVR)
	idx.Legacy.F5SPKVlans = countAll(ctx, dyn, VlanGVR)
	idx.Legacy.F5SPKEgresses = countAll(ctx, dyn, EgressGVR)

	// Controller rollout.
	if cs != nil {
		dep, err := cs.AppsV1().Deployments(opts.ControllerNamespace).Get(ctx, opts.ControllerName, metav1.GetOptions{})
		switch {
		case err == nil:
			idx.Controller.Found = true
			idx.Controller.Desired = 1
			if dep.Spec.Replicas != nil {
				idx.Controller.Desired = *dep.Spec.Replicas
			}
			idx.Controller.Available = dep.Status.AvailableReplicas
			idx.Controller.Ready = idx.Controller.Desired > 0 && idx.Controller.Available >= idx.Controller.Desired
		case apierrors.IsNotFound(err):
		default:
			return nil, fmt.Errorf("get deployment %s/%s: %w", opts.ControllerNamespace, opts.ControllerName, err)
		}
		if err := scanTMM(ctx, cs, idx); err != nil {
			return nil, err
		}
	}

	scanAIGatewayPosture(ctx, dyn, cs, servedCRDs, crdsListed, idx)

	idx.MCP = DiscoverMCP(r)
	sort.Strings(idx.Missing)
	return idx, nil
}

func scanAIGatewayPosture(ctx context.Context, dyn dynamic.Interface, cs kubernetes.Interface, servedCRDs map[string]bool, crdsListed bool, idx *Index) {
	p := &AIGatewayPosture{}
	for _, dc := range idx.DNSCaches {
		if dc.Name == "aigw-dns-resolver" {
			p.AIGWDNSResolverPresent = true
			if dc.Ready {
				idx.DNSResolverReady = true
			}
		}
	}
	if len(idx.AITenants)+len(idx.AICredentials)+len(idx.AIProviders)+len(idx.AIModels)+len(idx.AIGatewayProfiles)+len(idx.AIGateways) > 0 || servedCRDs["aigateways.inference.k8s.f5.com"] || servedCRDs["aigatewayprofiles.inference.k8s.f5.com"] || servedCRDs["aitenants.inference.k8s.f5.com"] {
		p.Enabled = true
	}
	if dyn != nil && (!crdsListed || servedCRDs["cneinstances.k8s.f5.com"]) {
		func() {
			defer func() { _ = recover() }()
			if items, err := listAll(ctx, dyn, CNEInstanceGVR); err == nil {
				for _, it := range items {
					if en, ok, _ := unstructured.NestedBool(it.Object, "spec", "aiGateway", "enabled"); ok && en {
						p.Enabled = true
					}
					if mEn, ok, _ := unstructured.NestedBool(it.Object, "spec", "aiGateway", "aiMetrics"); ok && mEn {
						p.AIMetricsEnabled = true
					} else if mEn, ok, _ := unstructured.NestedBool(it.Object, "spec", "aiGateway", "aiMetrics", "enabled"); ok && mEn {
						p.AIMetricsEnabled = true
					}
				}
			}
		}()
	}
	if cs != nil {
		if scs, err := cs.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{}); err == nil && scs != nil {
			for _, sc := range scs.Items {
				if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" ||
					sc.Annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true" {
					p.DefaultStorageClass = sc.Name
					break
				}
			}
		}
		if _, err := cs.CoreV1().Services("kube-system").Get(ctx, "kube-dns", metav1.GetOptions{}); apierrors.IsNotFound(err) {
			p.KubeDNSAbsent = true
		}
		if ssets, err := cs.AppsV1().StatefulSets(idx.Controller.Namespace).List(ctx, metav1.ListOptions{}); err == nil && ssets != nil {
			for _, ss := range ssets.Items {
				if strings.Contains(ss.Name, "dssm-sentinel") && ss.Status.ReadyReplicas > 0 {
					p.DSSMSentinelReady = true
					break
				}
			}
		}
		if !p.DSSMSentinelReady {
			if pods, err := cs.CoreV1().Pods(idx.Controller.Namespace).List(ctx, metav1.ListOptions{}); err == nil && pods != nil {
				for _, pod := range pods.Items {
					if strings.Contains(pod.Name, "dssm-sentinel") && pod.Status.Phase == corev1.PodRunning {
						p.DSSMSentinelReady = true
						break
					}
				}
			}
		}
		aigwNamespaces := uniqueNonEmptyStrings(idx.Controller.Namespace, "f5-cne-system", "f5-bnk", "aigateway")
		for _, ns := range aigwNamespaces {
			for _, depName := range []string{"f5-aigateway-console", "f5-aigateway"} {
				if dep, err := cs.AppsV1().Deployments(ns).Get(ctx, depName, metav1.GetOptions{}); err == nil && dep != nil {
					p.Enabled = true
					p.F5AIGatewayFound = true
					p.F5AIGatewayDesired = 1
					if dep.Spec.Replicas != nil {
						p.F5AIGatewayDesired = *dep.Spec.Replicas
					}
					p.F5AIGatewayAvailable = dep.Status.AvailableReplicas
					p.F5AIGatewayReady = p.F5AIGatewayDesired > 0 && p.F5AIGatewayAvailable >= p.F5AIGatewayDesired
					break
				}
			}
			if p.F5AIGatewayFound {
				break
			}
		}
		chNamespaces := uniqueNonEmptyStrings(idx.Controller.Namespace, "f5-cne-system", "f5-bnk", "f5-utils")
		for _, ns := range chNamespaces {
			if ssets, err := cs.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{}); err == nil && ssets != nil {
				for _, ss := range ssets.Items {
					if strings.Contains(ss.Name, "clickhouse") && ss.Status.ReadyReplicas > 0 {
						p.ClickHouseReady = true
						break
					}
				}
			}
			if pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{}); err == nil && pods != nil {
				for _, pod := range pods.Items {
					if strings.Contains(pod.Name, "clickhouse") && pod.Status.Phase == corev1.PodRunning {
						p.ClickHouseReady = true
						break
					}
				}
			}
			if pvcs, err := cs.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{}); err == nil && pvcs != nil {
				for _, pvc := range pvcs.Items {
					if strings.Contains(pvc.Name, "clickhouse") && pvc.Status.Phase == corev1.ClaimBound {
						p.ClickHousePVCsBound++
					}
				}
			}
		}
	}
	if p.Enabled || p.F5AIGatewayFound || p.AIGWDNSResolverPresent {
		idx.AIGatewayPosture = p
	}
}

func uniqueNonEmptyStrings(items ...string) []string {
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, s := range items {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// listAll lists a GVR across every namespace.
func listAll(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource) ([]*unstructured.Unstructured, error) {
	l, err := dyn.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	items := make([]*unstructured.Unstructured, 0, len(l.Items))
	for i := range l.Items {
		items = append(items, &l.Items[i])
	}
	sort.Slice(items, func(a, b int) bool {
		if items[a].GetNamespace() != items[b].GetNamespace() {
			return items[a].GetNamespace() < items[b].GetNamespace()
		}
		return items[a].GetName() < items[b].GetName()
	})
	return items, nil
}

// countAll returns the number of objects of a GVR, 0 when the CRD is absent
// or the list fails: legacy kinds are informational.
func countAll(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource) int {
	l, err := dyn.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0
	}
	return len(l.Items)
}

// f5CRDGroups returns the sorted F5 CRD groups the API server serves and the
// set of served CRD resource names. When CRDs cannot be listed (RBAC), it
// probes the two policy groups directly.
func f5CRDGroups(ctx context.Context, dyn dynamic.Interface) ([]string, map[string]bool, bool, error) {
	seen := map[string]bool{}
	crds := map[string]bool{}
	crdsListed := false
	l, err := dyn.Resource(CRDGVR).List(ctx, metav1.ListOptions{})
	if err == nil {
		crdsListed = true
		for i := range l.Items {
			name := l.Items[i].GetName()
			crds[name] = true
			g, _, _ := unstructured.NestedString(l.Items[i].Object, "spec", "group")
			if strings.HasSuffix(g, "f5.com") || strings.HasSuffix(g, "f5net.com") {
				seen[g] = true
			}
		}
	} else if !apierrors.IsForbidden(err) && !apierrors.IsNotFound(err) {
		return nil, nil, false, fmt.Errorf("list CRDs: %w", err)
	} else {
		for g, gvr := range map[string]schema.GroupVersionResource{
			GatewayF5Group:    NetPolicyGVR,
			LegacyPolicyGroup: BNKNetPolGVR,
			DataPlaneGroup:    IRuleGVR,
			FLOGroup:          CNEInstanceGVR,
			InferenceF5Group:  AIGatewayProfileGVR,
		} {
			if _, err := dyn.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 1}); err == nil {
				seen[g] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for g := range seen {
		out = append(out, g)
	}
	sort.Strings(out)
	return out, crds, crdsListed, nil
}

// bnkGatewayClasses returns the names of the GatewayClasses whose controller
// is an F5 CNE controller. Empty when no class matches, in which case the
// caller keeps every Gateway.
func bnkGatewayClasses(classes []*unstructured.Unstructured) map[string]bool {
	out := map[string]bool{}
	for _, c := range classes {
		ctrl, _, _ := unstructured.NestedString(c.Object, "spec", "controllerName")
		if strings.HasPrefix(ctrl, "f5.com/") || strings.Contains(ctrl, "f5-cne-controller") {
			out[c.GetName()] = true
		}
	}
	return out
}

// toObject converts an unstructured object into an Object with the readiness
// verdict computed by ready.
func toObject(u *unstructured.Unstructured, ready func(*unstructured.Unstructured) (bool, string)) Object {
	o := Object{
		Kind:       u.GetKind(),
		APIVersion: u.GetAPIVersion(),
		Namespace:  u.GetNamespace(),
		Name:       u.GetName(),
		Conditions: conditionsOf(u.Object, "status", "conditions"),
	}
	o.Ready, o.Detail = ready(u)
	return o
}

// conditionsOf reads a []condition at the given path.
func conditionsOf(obj map[string]any, path ...string) []Condition {
	items, found, _ := unstructured.NestedSlice(obj, path...)
	if !found {
		return nil
	}
	var out []Condition
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		c := Condition{}
		c.Type, _ = m["type"].(string)
		c.Status, _ = m["status"].(string)
		c.Reason, _ = m["reason"].(string)
		c.Message, _ = m["message"].(string)
		out = append(out, c)
	}
	return out
}

// requireTrue is ready only when every named condition exists and is True.
func requireTrue(types ...string) func(*unstructured.Unstructured) (bool, string) {
	return func(u *unstructured.Unstructured) (bool, string) {
		conds := conditionsOf(u.Object, "status", "conditions")
		var missing []string
		for _, t := range types {
			c, ok := findCondition(conds, t)
			switch {
			case !ok:
				missing = append(missing, t+" absent")
			case c.Status != "True":
				missing = append(missing, describe(c))
			}
		}
		if len(missing) == 0 {
			return true, ""
		}
		return false, strings.Join(missing, "; ")
	}
}

// trueIfPresent is ready unless a named condition exists and is not True.
func trueIfPresent(types ...string) func(*unstructured.Unstructured) (bool, string) {
	return func(u *unstructured.Unstructured) (bool, string) {
		conds := conditionsOf(u.Object, "status", "conditions")
		var bad []string
		for _, t := range types {
			if c, ok := findCondition(conds, t); ok && c.Status != "True" {
				bad = append(bad, describe(c))
			}
		}
		if len(bad) == 0 {
			return true, ""
		}
		return false, strings.Join(bad, "; ")
	}
}

// routeReady requires every parent in status.parents to report Accepted=True.
func routeReady(u *unstructured.Unstructured) (bool, string) {
	parents, found, _ := unstructured.NestedSlice(u.Object, "status", "parents")
	if !found || len(parents) == 0 {
		return false, "no parent status"
	}
	var bad []string
	for _, p := range parents {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		conds := conditionsOf(pm, "conditions")
		for _, t := range []string{"Accepted", "ResolvedRefs"} {
			if c, ok := findCondition(conds, t); ok && c.Status != "True" {
				bad = append(bad, describe(c))
			}
		}
	}
	if len(bad) == 0 {
		return true, ""
	}
	return false, strings.Join(bad, "; ")
}

func findCondition(conds []Condition, t string) (Condition, bool) {
	for _, c := range conds {
		if c.Type == t {
			return c, true
		}
	}
	return Condition{}, false
}

func describe(c Condition) string {
	s := c.Type + "=" + c.Status
	if c.Reason != "" {
		s += " (" + c.Reason + ")"
	}
	return s
}

// scanTMM records the TMM DaemonSet rollout and, for every pod of it that is
// not Running, the pod phase with its latest Warning event (for example the
// kubelet's FailedCreatePodSandBox naming Multus). A missing DaemonSet is
// not an error: FLO has not rendered it yet.
func scanTMM(ctx context.Context, cs kubernetes.Interface, idx *Index) error {
	ds, err := cs.AppsV1().DaemonSets(idx.TMM.Namespace).Get(ctx, idx.TMM.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("get daemonset %s/%s: %w", idx.TMM.Namespace, idx.TMM.Name, err)
	}
	idx.TMM.Found = true
	idx.TMM.Desired = ds.Status.DesiredNumberScheduled
	idx.TMM.Ready = ds.Status.NumberReady
	if idx.TMM.Complete() || ds.Spec.Selector == nil {
		return nil
	}
	sel := metav1.FormatLabelSelector(ds.Spec.Selector)
	pods, err := cs.CoreV1().Pods(idx.TMM.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil //nolint:nilerr — the tally stands without pod detail
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodRunning {
			continue
		}
		line := fmt.Sprintf("pod %s %s", p.Name, p.Status.Phase)
		if ev := latestWarningEvent(ctx, cs, p.Namespace, p.Name); ev != "" {
			line += ": " + ev
		}
		idx.TMM.Pending = append(idx.TMM.Pending, line)
	}
	sort.Strings(idx.TMM.Pending)
	return nil
}

// latestWarningEvent returns "<reason>: <message>" of the newest Warning
// event about the named pod, trimmed to one line; empty when none.
func latestWarningEvent(ctx context.Context, cs kubernetes.Interface, ns, pod string) string {
	events, err := cs.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.name=" + pod + ",type=Warning"})
	if err != nil {
		return ""
	}
	var best *corev1.Event
	var bestTime time.Time
	for i := range events.Items {
		ev := &events.Items[i]
		if ev.InvolvedObject.Name != pod || ev.Type != corev1.EventTypeWarning {
			continue
		}
		ts := ev.LastTimestamp.Time
		if ts.IsZero() {
			ts = ev.EventTime.Time
		}
		if best == nil || ts.After(bestTime) {
			best, bestTime = ev, ts
		}
	}
	if best == nil {
		return ""
	}
	msg := strings.Join(strings.Fields(best.Message), " ")
	if len(msg) > 240 {
		msg = msg[:240] + "…"
	}
	return best.Reason + ": " + msg
}
