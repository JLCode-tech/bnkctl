package migrate

import (
	"context"
	"fmt"
	"io"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/yaml"
)

// Applier writes one object to the cluster. The production implementation
// server-side-applies through the dynamic client; tests record.
type Applier interface {
	Apply(ctx context.Context, obj *unstructured.Unstructured) error
}

// DynamicApplier server-side-applies unstructured objects, resolving kinds
// through a RESTMapper so it needs no static GVR table for the Gateway API
// version served by the cluster.
type DynamicApplier struct {
	dyn    dynamic.Interface
	mapper meta.RESTMapper
}

// NewDynamicApplier builds a DynamicApplier from a REST config (live cluster).
func NewDynamicApplier(cfg *rest.Config) (*DynamicApplier, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("discovery client: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(noCache{disc})
	return &DynamicApplier{dyn: dyn, mapper: mapper}, nil
}

// NewDynamicApplierWith builds a DynamicApplier from an existing client and
// mapper (tests, or callers that already hold both).
func NewDynamicApplierWith(dyn dynamic.Interface, mapper meta.RESTMapper) *DynamicApplier {
	return &DynamicApplier{dyn: dyn, mapper: mapper}
}

// Apply server-side-applies obj with FieldManager, forcing conflicts: the
// Gateway parametersRef patch touches objects kubectl or a GitOps tool owns.
func (a *DynamicApplier) Apply(ctx context.Context, obj *unstructured.Unstructured) error {
	gvk := obj.GroupVersionKind()
	mapping, err := a.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return fmt.Errorf("REST mapping for %s: %w", gvk, err)
	}
	body, err := yaml.Marshal(obj.Object)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", describe(obj), err)
	}
	var ri dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ri = a.dyn.Resource(mapping.Resource).Namespace(obj.GetNamespace())
	} else {
		ri = a.dyn.Resource(mapping.Resource)
	}
	force := true
	_, err = ri.Patch(ctx, obj.GetName(), types.ApplyPatchType, body, metav1.PatchOptions{
		FieldManager: FieldManager,
		Force:        &force,
	})
	if err != nil {
		return fmt.Errorf("server-side apply %s: %w", describe(obj), err)
	}
	return nil
}

// Apply applies every object of the plan in order and logs each one.
func Apply(ctx context.Context, a Applier, plan *Plan, log io.Writer) error {
	return ApplyPlan(ctx, a, plan, log)
}

// ApplyPlan applies every object of the plan in order and logs each one.
// It stops at the first error so a rejected Infra CR never leaves half the
// tenants pointing at GatewaySettings the controller cannot serve.
func ApplyPlan(ctx context.Context, a Applier, plan *Plan, log io.Writer) error {
	for _, obj := range plan.Objects() {
		if err := a.Apply(ctx, obj); err != nil {
			return err
		}
		fmt.Fprintf(log, "[migrate-2.4] applied %s\n", describe(obj))
	}
	return nil
}

// CleanupLegacy deletes the migrated legacy 2.3 CRs from the cluster after
// the 2.4+ Infra / GatewaySettings / EgressGateway objects are programmed.
func CleanupLegacy(ctx context.Context, dyn dynamic.Interface, inv *Inventory, log io.Writer) error {
	if dyn == nil || inv == nil {
		return nil
	}
	if log == nil {
		log = io.Discard
	}
	type batch struct {
		gvr     schema.GroupVersionResource
		objects []*unstructured.Unstructured
	}
	batches := []batch{
		{BNKSecPolGVR, inv.SecPolicies},
		{BNKNetPolGVR, inv.NetPolicies},
		{BnkGatewayGVR, inv.BnkGateways},
		{EgressGVR, inv.Egresses},
		{SnatpoolGVR, inv.Snatpools},
		{StaticRouteGVR, inv.StaticRoutes},
		{VxlanGVR, inv.Vxlans},
		{VrfGVR, inv.Vrfs},
		{VlanGVR, inv.Vlans},
	}
	for _, b := range batches {
		for _, obj := range b.objects {
			if obj == nil {
				continue
			}
			ns := obj.GetNamespace()
			if ns == "" {
				ns = inv.Namespace
			}
			err := dyn.Resource(b.gvr).Namespace(ns).Delete(ctx, obj.GetName(), metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete legacy %s %s/%s: %w", b.gvr.Resource, ns, obj.GetName(), err)
			}
			fmt.Fprintf(log, "[migrate-2.4] deleted legacy %s %s/%s\n", obj.GetKind(), ns, obj.GetName())
		}
	}
	return nil
}

// WaitConditionTrue polls a namespaced CR until status.conditions carries
// condType with status True. The error names the last observed condition so
// a stuck Infra is diagnosable from the log.
func WaitConditionTrue(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, name, condType string, timeout, poll time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := "not found"
	for {
		obj, err := dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			if ok, desc := conditionTrue(obj.Object, condType); ok {
				return nil
			} else {
				last = desc
			}
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get %s %s/%s: %w", gvr.Resource, ns, name, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s %s/%s: %s after %s", gvr.Resource, ns, name, last, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// conditionTrue reports whether status.conditions has condType=True and
// describes the last observed state otherwise.
func conditionTrue(obj map[string]any, condType string) (bool, string) {
	conds, _, _ := unstructured.NestedSlice(obj, "status", "conditions")
	last := "no " + condType + " condition yet"
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != condType {
			continue
		}
		if m["status"] == "True" {
			return true, ""
		}
		last = fmt.Sprintf("%s=%v reason=%v message=%v", condType, m["status"], m["reason"], m["message"])
	}
	return false, last
}

// noCache adapts a discovery client to the cached interface the deferred
// mapper wants (no caching: one-shot CLI).
type noCache struct{ discovery.DiscoveryInterface }

func (noCache) Fresh() bool { return true }
func (noCache) Invalidate() {}
