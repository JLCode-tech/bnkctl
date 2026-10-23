package migrate

import (
	"context"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// legacyResource is one legacy kind Inspect lists.
type legacyResource struct {
	kind       string
	gvr        schema.GroupVersionResource
	namespaced bool // list in the CNE namespace only (false = all namespaces)
	dest       func(inv *Inventory) *[]*unstructured.Unstructured
}

var legacyResources = []legacyResource{
	{"F5SPKVlan", VlanGVR, true, func(i *Inventory) *[]*unstructured.Unstructured { return &i.Vlans }},
	{"F5SPKStaticRoute", StaticRouteGVR, true, func(i *Inventory) *[]*unstructured.Unstructured { return &i.StaticRoutes }},
	{"Vrf", VrfGVR, true, func(i *Inventory) *[]*unstructured.Unstructured { return &i.Vrfs }},
	{"Vxlan", VxlanGVR, true, func(i *Inventory) *[]*unstructured.Unstructured { return &i.Vxlans }},
	{"F5SPKEgress", EgressGVR, true, func(i *Inventory) *[]*unstructured.Unstructured { return &i.Egresses }},
	{"F5SPKSnatpool", SnatpoolGVR, true, func(i *Inventory) *[]*unstructured.Unstructured { return &i.Snatpools }},
	{"F5BnkGateway", BnkGatewayGVR, false, func(i *Inventory) *[]*unstructured.Unstructured { return &i.BnkGateways }},
	{"BNKSecPolicy", BNKSecPolGVR, false, func(i *Inventory) *[]*unstructured.Unstructured { return &i.SecPolicies }},
	{"BNKNetPolicy", BNKNetPolGVR, false, func(i *Inventory) *[]*unstructured.Unstructured { return &i.NetPolicies }},
	{"Gateway", GatewayGVR, false, func(i *Inventory) *[]*unstructured.Unstructured { return &i.Gateways }},
	{"GatewayClass", GatewayClassGVR, false, func(i *Inventory) *[]*unstructured.Unstructured { return &i.GatewayClasses }},
}

// Inspect lists the legacy CRs of a BNK 2.3.x cluster. The underlay CRs
// (VLANs, routes, VRFs, VXLANs, egress, SNAT pools) are read from namespace;
// F5BnkGateway, the policies and the Gateway API objects from every
// namespace. A CRD the API server does not serve is recorded in
// Inventory.Missing, not returned as an error.
func Inspect(ctx context.Context, dyn dynamic.Interface, namespace string) (*Inventory, error) {
	if namespace == "" {
		namespace = DefaultInstanceNamespace
	}
	inv := &Inventory{Namespace: namespace}

	for _, r := range legacyResources {
		ri := dyn.Resource(r.gvr)
		var list *unstructured.UnstructuredList
		var err error
		if r.namespaced {
			list, err = ri.Namespace(namespace).List(ctx, metav1.ListOptions{})
		} else {
			list, err = ri.List(ctx, metav1.ListOptions{})
		}
		if err != nil {
			if apierrors.IsNotFound(err) {
				inv.Missing = append(inv.Missing, r.kind)
				continue
			}
			return nil, fmt.Errorf("list %s (%s): %w", r.kind, r.gvr.GroupResource(), err)
		}
		items := make([]*unstructured.Unstructured, 0, len(list.Items))
		for i := range list.Items {
			items = append(items, &list.Items[i])
		}
		sort.Slice(items, func(a, b int) bool {
			if items[a].GetNamespace() != items[b].GetNamespace() {
				return items[a].GetNamespace() < items[b].GetNamespace()
			}
			return items[a].GetName() < items[b].GetName()
		})
		*r.dest(inv) = items
	}

	cne, err := dyn.Resource(CNEInstanceGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	switch {
	case err == nil && len(cne.Items) > 0:
		inv.CNEInstance = &cne.Items[0]
	case err != nil && !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("list CNEInstance in %s: %w", namespace, err)
	}
	return inv, nil
}

// Summary returns one line per legacy kind with its count, for the
// migrate-2.4 report.
func (inv *Inventory) Summary() []string {
	lines := []string{
		fmt.Sprintf("F5SPKVlan: %d", len(inv.Vlans)),
		fmt.Sprintf("F5SPKStaticRoute: %d", len(inv.StaticRoutes)),
		fmt.Sprintf("Vrf: %d", len(inv.Vrfs)),
		fmt.Sprintf("Vxlan: %d", len(inv.Vxlans)),
		fmt.Sprintf("F5SPKEgress: %d", len(inv.Egresses)),
		fmt.Sprintf("F5SPKSnatpool: %d", len(inv.Snatpools)),
		fmt.Sprintf("F5BnkGateway: %d", len(inv.BnkGateways)),
		fmt.Sprintf("BNKSecPolicy: %d", len(inv.SecPolicies)),
		fmt.Sprintf("BNKNetPolicy: %d", len(inv.NetPolicies)),
		fmt.Sprintf("Gateway: %d", len(inv.Gateways)),
	}
	if len(inv.Missing) > 0 {
		lines = append(lines, "CRDs not served: "+joinStrings(inv.Missing, ", "))
	}
	return lines
}

// ManifestVersion returns the CNEInstance spec.manifestVersion ("" when unknown).
func (inv *Inventory) ManifestVersion() string {
	if inv.CNEInstance == nil {
		return ""
	}
	v, _, _ := unstructured.NestedString(inv.CNEInstance.Object, "spec", "manifestVersion")
	return v
}
