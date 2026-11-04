package gcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/JLCode-tech/bnkctl/pkg/bnkconst"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var (
	staticRouteGVR = schema.GroupVersionResource{
		Group:    "k8s.f5.com",
		Version:  "v1",
		Resource: "f5spkstaticroutes",
	}
	infraV1Alpha1GVR = schema.GroupVersionResource{
		Group:    "gateway.k8s.f5.com",
		Version:  "v1alpha1",
		Resource: "infras",
	}
	infraV1GVR = schema.GroupVersionResource{
		Group:    "gateway.k8s.f5.com",
		Version:  "v1",
		Resource: "infras",
	}
)

// reconcileStaticRouteDrift reconciles F5SPKStaticRoute (BNK 2.3) or Infra
// spec.staticRoutes (BNK 2.4+) gateway IPs when state.env's EXT_GW or INT_GW
// has drifted from what is stored on the cluster CRs.
func reconcileStaticRouteDrift(ctx context.Context, dyn dynamic.Interface, cl *intent.Cluster, st *state.State) error {
	if dyn == nil || st == nil {
		return nil
	}
	extGW := strings.TrimSpace(st.Get("EXT_GW"))
	intGW := strings.TrimSpace(st.Get("INT_GW"))
	podGW := strings.TrimSpace(st.Get("TMM_POD_GW"))
	if extGW == "" && intGW == "" && podGW == "" {
		return nil
	}

	// Reconcile BNK 2.3 F5SPKStaticRoute CRs if present.
	if extGW != "" {
		if sr, err := dyn.Resource(staticRouteGVR).Namespace(bnkconst.InstanceNamespace).Get(ctx, "default-egress-route", metav1.GetOptions{}); err == nil && sr != nil {
			curGW, _, _ := unstructured.NestedString(sr.Object, "spec", "gateway")
			if curGW != "" && curGW != extGW {
				_ = unstructured.SetNestedField(sr.Object, extGW, "spec", "gateway")
				if _, uerr := dyn.Resource(staticRouteGVR).Namespace(bnkconst.InstanceNamespace).Update(ctx, sr, metav1.UpdateOptions{}); uerr != nil && !apierrors.IsNotFound(uerr) {
					return fmt.Errorf("reconcileStaticRouteDrift: update default-egress-route: %w", uerr)
				}
			}
		}
	}
	if intGW != "" && cl != nil && cl.IsDualInterface() {
		if sr, err := dyn.Resource(staticRouteGVR).Namespace(bnkconst.InstanceNamespace).Get(ctx, "internal-vpc-route", metav1.GetOptions{}); err == nil && sr != nil {
			curGW, _, _ := unstructured.NestedString(sr.Object, "spec", "gateway")
			if curGW != "" && curGW != intGW {
				_ = unstructured.SetNestedField(sr.Object, intGW, "spec", "gateway")
				if _, uerr := dyn.Resource(staticRouteGVR).Namespace(bnkconst.InstanceNamespace).Update(ctx, sr, metav1.UpdateOptions{}); uerr != nil && !apierrors.IsNotFound(uerr) {
					return fmt.Errorf("reconcileStaticRouteDrift: update internal-vpc-route: %w", uerr)
				}
			}
		}
	}

	mainCIDR := strings.TrimSpace(st.Get("MAIN_CIDR"))
	rawAgentIPs := strings.TrimSpace(st.Get("AGENT_EXT_IPS"))
	if rawAgentIPs == "" {
		rawAgentIPs = strings.TrimSpace(st.Get("AGENT_EXT_IP"))
	}
	var agentExtIPs []string
	for _, p := range strings.Split(rawAgentIPs, ",") {
		if ip := strings.TrimSpace(p); ip != "" {
			agentExtIPs = append(agentExtIPs, ip)
		}
	}

	// Reconcile BNK 2.4/2.5 Infra CR staticRoutes if present.
	for _, gvr := range []schema.GroupVersionResource{infraV1Alpha1GVR, infraV1GVR} {
		list, err := dyn.Resource(gvr).Namespace(bnkconst.InstanceNamespace).List(ctx, metav1.ListOptions{})
		if err != nil || list == nil {
			continue
		}
		for i := range list.Items {
			item := &list.Items[i]
			routes, found, _ := unstructured.NestedSlice(item.Object, "spec", "staticRoutes")
			if !found || len(routes) == 0 {
				continue
			}
			changed := false
			hasExtGWLink := false
			hasMgmtNodes := false
			existingAgentRoutes := map[string]bool{}
			for j, raw := range routes {
				rm, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				name, _ := rm["name"].(string)
				gw, _ := rm["gateway"].(string)
				nh, _ := rm["nextHop"].(string)
				switch {
				case name == "route-ext-gw":
					hasExtGWLink = true
				case name == "mgmt-nodes" || name == "route-mgmt-nodes":
					hasMgmtNodes = true
					if podGW != "" && nh != podGW {
						rm["nextHop"] = podGW
						routes[j] = rm
						changed = true
					}
				case name == "agent-node" || name == "route-agent-node" || strings.HasPrefix(name, "agent-node-"):
					existingAgentRoutes[name] = true
					if podGW != "" && nh != podGW {
						rm["nextHop"] = podGW
						routes[j] = rm
						changed = true
					}
				case name == "default-egress" || name == "route-default-egress" || name == "default":
					if extGW != "" {
						if gw != "" && gw != extGW {
							rm["gateway"] = extGW
							routes[j] = rm
							changed = true
						}
						if nh != "" && nh != extGW {
							rm["nextHop"] = extGW
							routes[j] = rm
							changed = true
						}
					}
				case name == "internal-vpc":
					if intGW != "" {
						if gw != "" && gw != intGW {
							rm["gateway"] = intGW
							routes[j] = rm
							changed = true
						}
						if nh != "" && nh != intGW {
							rm["nextHop"] = intGW
							routes[j] = rm
							changed = true
						}
					}
				case name == "service-cidr" || name == "pod-cidr" || name == "route-cluster-pods" || name == "route-cluster-svcs":
					if podGW != "" && nh != podGW {
						rm["nextHop"] = podGW
						routes[j] = rm
						changed = true
					}
				}
			}
			if !hasExtGWLink && extGW != "" {
				routes = append([]any{
					map[string]any{
						"name":         "route-ext-gw",
						"destinations": []any{extGW + "/32"},
						"networkRef": map[string]any{
							"name": "external",
						},
					},
				}, routes...)
				changed = true
			}
			if !hasMgmtNodes && mainCIDR != "" && podGW != "" {
				routes = append(routes, map[string]any{
					"name":         "mgmt-nodes",
					"destinations": []any{mainCIDR},
					"nextHop":      podGW,
				})
				changed = true
			}
			if podGW != "" {
				for idx, ip := range agentExtIPs {
					routeName := "agent-node"
					if idx > 0 {
						routeName = fmt.Sprintf("agent-node-%d", idx+1)
					}
					if existingAgentRoutes[routeName] || (idx == 0 && existingAgentRoutes["route-agent-node"]) {
						continue
					}
					routes = append(routes, map[string]any{
						"name":         routeName,
						"destinations": []any{ip + "/32"},
						"nextHop":      podGW,
					})
					changed = true
				}
			}
			if changed {
				_ = unstructured.SetNestedSlice(item.Object, routes, "spec", "staticRoutes")
				_, _ = dyn.Resource(gvr).Namespace(bnkconst.InstanceNamespace).Update(ctx, item, metav1.UpdateOptions{})
			}
		}
	}

	return nil
}
