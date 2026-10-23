package scenarios

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

// EnvDiagramInput carries everything the Render function needs to produce
// the ASCII environment diagram. Clientset and Dynamic are optional;
// when nil, live reads are skipped and "(unknown)" is used as the fallback.
type EnvDiagramInput struct {
	// Cluster is required — provides cluster name, region, external CIDR, VIP.
	Cluster *intent.Cluster
	// State is required — provides JUMPHOST_* keys.
	State *state.State
	// Scenario is optional; renders the "scenario: <name>" footer.
	Scenario string
	// Clientset is optional; used to read TMM pod IP (f5-cne-system, app=f5-tmm).
	Clientset kubernetes.Interface
	// Dynamic is optional; used to read Gateway .status.addresses and
	// HTTPRoute .status.parents[0].conditions.
	Dynamic dynamic.Interface
	// Namespace is the scenario's namespace for Gateway/HTTPRoute lookup.
	Namespace string
	// VIP is the resolved VIP (passed in to avoid re-derivation).
	VIP string
}

var (
	gatewayDiagGVR = schema.GroupVersionResource{
		Group:    "gateway.networking.k8s.io",
		Version:  "v1",
		Resource: "gateways",
	}
	httpRouteDiagGVR = schema.GroupVersionResource{
		Group:    "gateway.networking.k8s.io",
		Version:  "v1",
		Resource: "httproutes",
	}
)

// Render produces an ASCII diagram of the cluster environment exercised
// by the scenario. Live reads fall back to "(unknown)" on error or when
// clients are nil.
//
// Example output:
//
//	┌─────────────────────────────────────────────────────┐
//	│  cluster: syd-tracer   region: ap-southeast-2       │
//	│  ext-cidr: 10.0.10.0/24   vip: 10.0.10.100          │
//	│                                                     │
//	│  jumphost: i-0abc123  (src: 10.0.10.5)              │
//	│      │                                              │
//	│      └─SSH+EICE──► curl ──► TMM (10.0.10.240)       │
//	│                             │                       │
//	│                         Gateway (Programmed=True)   │
//	│                             │                       │
//	│                         HTTPRoute → nginx           │
//	│                                                     │
//	│  tmm-pod: 192.168.1.10                              │
//	│  gateway-addr: 10.0.10.100                          │
//	│  httproute-accepted: True                           │
//	│  scenario: http-routing-e2e                         │
//	└─────────────────────────────────────────────────────┘
func Render(in EnvDiagramInput) string {
	cl := in.Cluster
	st := in.State

	clusterName := "(unknown)"
	region := "(unknown)"
	extCIDR := "(unknown)"
	vip := in.VIP

	if cl != nil {
		clusterName = cl.Metadata.Name
		region = cl.Metadata.Region
		if cl.Network.DataPath != nil {
			extCIDR = cl.Network.DataPath.External.CIDR
		}
		if vip == "" && cl.Network.DataPath != nil {
			if v, err := cl.DefaultVIP(); err == nil {
				vip = v
			}
		}
	}
	if vip == "" {
		vip = "(unknown)"
	}

	jumphostID := "(unknown)"
	jumphostSrc := "(unknown)"
	if st != nil {
		if v := st.Get("JUMPHOST_INSTANCE_ID"); v != "" {
			jumphostID = v
		}
		if v := st.Get("JUMPHOST_BNK_EXT_ENI_IP"); v != "" {
			jumphostSrc = v
		}
	}

	// Live reads (best-effort).
	tmmPodIP := liveReadTMMPodIP(in.Clientset)
	gatewayAddr := liveReadGatewayAddr(in.Dynamic, in.Namespace)
	httprouteAccepted := liveReadHTTProuteAccepted(in.Dynamic, in.Namespace)

	const width = 55
	pad := func(s string) string {
		// Left-pad the line with "│  " and right-pad to width+1 chars + "│"
		content := "│  " + s
		needed := width - len([]rune(content))
		if needed > 0 {
			content += strings.Repeat(" ", needed)
		}
		return content + "│"
	}
	top := "┌" + strings.Repeat("─", width) + "┐"
	bot := "└" + strings.Repeat("─", width) + "┘"
	blank := pad("")

	lines := []string{
		top,
		pad(fmt.Sprintf("cluster: %-15s  region: %s", clusterName, region)),
		pad(fmt.Sprintf("ext-cidr: %-20s  vip: %s", extCIDR, vip)),
		blank,
		pad(fmt.Sprintf("jumphost: %s  (src: %s)", jumphostID, jumphostSrc)),
		pad("    │"),
		pad("    └─SSH+EICE──► curl ──► TMM (" + tmmPodIP + ")"),
		pad("                           │"),
		pad("                       Gateway (addr: " + gatewayAddr + ")"),
		pad("                           │"),
		pad("                       HTTPRoute → nginx"),
		blank,
		pad("tmm-pod:            " + tmmPodIP),
		pad("gateway-addr:       " + gatewayAddr),
		pad("httproute-accepted: " + httprouteAccepted),
	}
	if in.Scenario != "" {
		lines = append(lines, pad("scenario:           "+in.Scenario))
	}
	lines = append(lines, bot)
	return strings.Join(lines, "\n")
}

// liveReadTMMPodIP reads the first TMM pod IP in f5-cne-system.
// Returns "(unknown)" on any error or when the client is nil.
func liveReadTMMPodIP(cs kubernetes.Interface) string {
	if cs == nil {
		return "(unknown)"
	}
	ctx := context.Background()
	pods, err := cs.CoreV1().Pods("f5-cne-system").List(ctx, metav1.ListOptions{
		LabelSelector: "app=f5-tmm",
		Limit:         1,
	})
	if err != nil || len(pods.Items) == 0 {
		return "(unknown)"
	}
	ip := pods.Items[0].Status.PodIP
	if ip == "" {
		return "(unknown)"
	}
	return ip
}

// liveReadGatewayAddr reads the first address from a Gateway's
// .status.addresses in the given namespace. Returns "(unknown)" on error.
func liveReadGatewayAddr(dyn dynamic.Interface, namespace string) string {
	if dyn == nil || namespace == "" {
		return "(unknown)"
	}
	ctx := context.Background()
	list, err := dyn.Resource(gatewayDiagGVR).Namespace(namespace).List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil || len(list.Items) == 0 {
		return "(unknown)"
	}
	gw := list.Items[0]
	addrs, _, _ := NestedSlice(gw.Object, "status", "addresses")
	if len(addrs) == 0 {
		return "(unknown)"
	}
	m, ok := addrs[0].(map[string]interface{})
	if !ok {
		return "(unknown)"
	}
	if v, ok := m["value"].(string); ok && v != "" {
		return v
	}
	return "(unknown)"
}

// liveReadHTTProuteAccepted reads the Accepted condition status from the
// first HTTPRoute in the given namespace. Returns "(unknown)" on error.
func liveReadHTTProuteAccepted(dyn dynamic.Interface, namespace string) string {
	if dyn == nil || namespace == "" {
		return "(unknown)"
	}
	ctx := context.Background()
	list, err := dyn.Resource(httpRouteDiagGVR).Namespace(namespace).List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil || len(list.Items) == 0 {
		return "(unknown)"
	}
	hr := list.Items[0]
	parents, _, _ := NestedSlice(hr.Object, "status", "parents")
	if len(parents) == 0 {
		return "(unknown)"
	}
	p, ok := parents[0].(map[string]interface{})
	if !ok {
		return "(unknown)"
	}
	conditions, _, _ := NestedSlice(p, "conditions")
	for _, cRaw := range conditions {
		c, ok := cRaw.(map[string]interface{})
		if !ok {
			continue
		}
		if c["type"] == "Accepted" {
			if v, ok := c["status"].(string); ok {
				return v
			}
		}
	}
	return "(unknown)"
}

// NestedSlice mirrors unstructured.NestedSlice but accepts an arbitrary depth
// of string keys (avoids importing k8s.io/apimachinery/pkg/apis/meta/v1/unstructured
// in scenario hot paths). Exported so scenario implementations can share it.
func NestedSlice(obj map[string]interface{}, fields ...string) ([]interface{}, bool, error) {
	cur := interface{}(obj)
	for _, f := range fields {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false, nil
		}
		cur = m[f]
	}
	if cur == nil {
		return nil, false, nil
	}
	s, ok := cur.([]interface{})
	return s, ok, nil
}
