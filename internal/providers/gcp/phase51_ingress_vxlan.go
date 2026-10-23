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
	if extGW == "" && intGW == "" {
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
			for j, raw := range routes {
				rm, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				name, _ := rm["name"].(string)
				gw, _ := rm["gateway"].(string)
				if name == "default-egress" && extGW != "" && gw != extGW {
					rm["gateway"] = extGW
					routes[j] = rm
					changed = true
				} else if name == "internal-vpc" && intGW != "" && gw != intGW {
					rm["gateway"] = intGW
					routes[j] = rm
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
