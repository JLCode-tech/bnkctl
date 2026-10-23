package migrate

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"
)

// recordingApplier records what ApplyPlan hands it and can fail on a kind.
type recordingApplier struct {
	applied []string
	failOn  string
}

func (r *recordingApplier) Apply(_ context.Context, obj *unstructured.Unstructured) error {
	if obj.GetKind() == r.failOn {
		return errors.New("rejected " + obj.GetKind())
	}
	r.applied = append(r.applied, describe(obj))
	return nil
}

func TestApplyPlan_OrderAndLog(t *testing.T) {
	plan := translateFixture(t, Options{ZoneByNetwork: fixtureZones})
	rec := &recordingApplier{}
	var log bytes.Buffer
	if err := ApplyPlan(context.Background(), rec, plan, &log); err != nil {
		t.Fatal(err)
	}
	if len(rec.applied) != len(plan.Objects()) {
		t.Fatalf("applied %d, want %d", len(rec.applied), len(plan.Objects()))
	}
	// Infra first, then GatewaySettings, Gateway patches, EgressGateways, policies.
	if !strings.HasPrefix(rec.applied[0], "Infra ") {
		t.Errorf("first apply = %s", rec.applied[0])
	}
	order := []string{"Infra", "GatewaySettings", "Gateway ", "EgressGateway", "SecPolicy", "NetPolicy"}
	last := -1
	for _, a := range rec.applied {
		for i, k := range order {
			if strings.HasPrefix(a, k) {
				if i < last {
					t.Errorf("%s applied after a later phase", a)
				}
				if i > last {
					last = i
				}
			}
		}
	}
	if !strings.Contains(log.String(), "[migrate-2.4] applied Infra f5-cne-system/infra") {
		t.Errorf("log = %s", log.String())
	}
}

func TestApplyPlan_StopsAtFirstError(t *testing.T) {
	plan := translateFixture(t, Options{})
	rec := &recordingApplier{failOn: "GatewaySettings"}
	err := ApplyPlan(context.Background(), rec, plan, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "rejected GatewaySettings") {
		t.Fatalf("err = %v", err)
	}
	if len(rec.applied) != 1 {
		t.Errorf("applied %v after the failure", rec.applied)
	}
}

func TestDynamicApplier_ServerSideApply(t *testing.T) {
	dyn := newFakeDynamic(t)
	var got k8stesting.PatchAction
	dyn.PrependReactor("patch", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		got = a.(k8stesting.PatchAction)
		var m map[string]any
		if err := yaml.Unmarshal(got.GetPatch(), &m); err != nil {
			return true, nil, err
		}
		return true, &unstructured.Unstructured{Object: m}, nil
	})
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(InfraGVR.GroupVersion().WithKind("Infra"), meta.RESTScopeNamespace)
	mapper.Add(GatewayClassGVR.GroupVersion().WithKind("GatewayClass"), meta.RESTScopeRoot)
	a := NewDynamicApplierWith(dyn, mapper)

	plan := translateFixture(t, Options{})
	if err := a.Apply(context.Background(), plan.Infra); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got.GetPatchType() != types.ApplyPatchType {
		t.Errorf("patch type = %s", got.GetPatchType())
	}
	if got.GetNamespace() != "f5-cne-system" || got.GetName() != "infra" || got.GetResource() != InfraGVR {
		t.Errorf("patched %s %s/%s", got.GetResource(), got.GetNamespace(), got.GetName())
	}
	if !strings.Contains(string(got.GetPatch()), "kind: Infra") {
		t.Errorf("patch body = %s", got.GetPatch())
	}

	// Unknown kind -> mapping error, no call.
	bogus := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "x.io/v1", "kind": "Nope", "metadata": map[string]any{"name": "n"}}}
	if err := a.Apply(context.Background(), bogus); err == nil || !strings.Contains(err.Error(), "REST mapping") {
		t.Errorf("err = %v", err)
	}
}

func TestWaitConditionTrue(t *testing.T) {
	infra := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.k8s.f5.com/v1alpha1", "kind": "Infra",
		"metadata": map[string]any{"name": "infra", "namespace": "f5-cne-system"},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Accepted", "status": "True"},
			map[string]any{"type": "Programmed", "status": "False", "reason": "Pending", "message": "waiting for TMM"},
		}},
	}}
	dyn := newFakeDynamic(t, infra)
	err := WaitConditionTrue(context.Background(), dyn, InfraGVR, "f5-cne-system", "infra", "Programmed", 30*time.Millisecond, 5*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "Programmed=False reason=Pending message=waiting for TMM") {
		t.Errorf("err = %v", err)
	}
	if err := WaitConditionTrue(context.Background(), dyn, InfraGVR, "f5-cne-system", "infra", "Accepted", time.Second, 5*time.Millisecond); err != nil {
		t.Errorf("Accepted: %v", err)
	}
	err = WaitConditionTrue(context.Background(), dyn, InfraGVR, "f5-cne-system", "missing", "Programmed", 20*time.Millisecond, 5*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing: %v", err)
	}
}
