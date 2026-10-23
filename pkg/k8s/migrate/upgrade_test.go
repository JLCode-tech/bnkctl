package migrate

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/JLCode-tech/bnkctl/pkg/k8s/render"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// fakeHelm implements phases.HelmInstaller for the upgrade tests.
type fakeHelm struct {
	deployed  string // chart version of the existing release ("" = none)
	pulled    []string
	upgraded  []string
	upgradeTo string
	values    map[string]interface{}
	failPull  bool
}

func (f *fakeHelm) List(_, _ string) ([]*release.Release, error) {
	if f.deployed == "" {
		return nil, nil
	}
	return []*release.Release{{
		Name:   "f5-lifecycle-operator",
		Chart:  &chart.Chart{Metadata: &chart.Metadata{Version: f.deployed}},
		Config: map[string]interface{}{"license": map[string]interface{}{"jwt": "old"}},
		Info:   &release.Info{Status: release.StatusDeployed},
	}}, nil
}

func (f *fakeHelm) Install(string, string, *chart.Chart, map[string]interface{}) (*release.Release, error) {
	return nil, errors.New("install must not be called by upgrade")
}

func (f *fakeHelm) Upgrade(name, _ string, ch *chart.Chart, values map[string]interface{}) (*release.Release, error) {
	f.upgraded = append(f.upgraded, name)
	f.upgradeTo = ch.Metadata.Version
	f.values = values
	return &release.Release{Name: name}, nil
}

func (f *fakeHelm) Uninstall(string, string) error { return nil }

func (f *fakeHelm) PullAndLoad(ref, version string) (*chart.Chart, error) {
	if f.failPull {
		return nil, errors.New("registry unreachable")
	}
	f.pulled = append(f.pulled, ref+"@"+version)
	return &chart.Chart{Metadata: &chart.Metadata{Name: "f5-lifecycle-operator", Version: version}}, nil
}

// cneReady returns the fixture CNEInstance with both data-path conditions True.
func cneReady(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	for _, o := range parseFixture(t, fixture23) {
		if o.GetKind() == "CNEInstance" {
			_ = unstructured.SetNestedSlice(o.Object, []any{
				map[string]any{"type": "CNEControllerAvailable", "status": "True"},
				map[string]any{"type": "F5TmmAvailable", "status": "True"},
				map[string]any{"type": "Available", "status": "False"},
			}, "status", "conditions")
			return o
		}
	}
	t.Fatal("no CNEInstance in fixture")
	return nil
}

func infraCRD() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": InfraCRDName},
	}}
}

// controllerObjects returns a ready controller Deployment + pod (with or
// without the flag) and n Ready TMM pods.
func controllerObjects(withFlag bool, tmm int) []runtime.Object {
	env := []corev1.EnvVar{{Name: "CLOUD_PROVIDER", Value: "aws"}}
	if withFlag {
		env = append(env, corev1.EnvVar{Name: UseGatewaySettingsEnv, Value: "true"})
	}
	sel := map[string]string{"app": "f5-cne-controller"}
	objs := []runtime.Object{
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: ControllerDeployment, Namespace: DefaultInstanceNamespace},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: sel},
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "f5-cne-controller", Env: env}}}},
			},
			Status: appsv1.DeploymentStatus{Replicas: 1, AvailableReplicas: 1},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "f5-cne-controller-abc", Namespace: DefaultInstanceNamespace, Labels: sel},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "f5-cne-controller", Env: env}}},
		},
	}
	for i := 0; i < tmm; i++ {
		objs = append(objs, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "f5-tmm-" + string(rune('a'+i)), Namespace: DefaultInstanceNamespace, Labels: map[string]string{"app": "f5-tmm"}},
			Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "f5-tmm", Ready: true}, {Name: "f5-tmm-routing", Ready: true}}},
		})
	}
	return objs
}

func TestUpgrade_Full(t *testing.T) {
	helm := &fakeHelm{deployed: "v2.21.13-0.0.64"}
	dyn := newFakeDynamic(t, cneReady(t), infraCRD())
	cs := k8sfake.NewClientset(controllerObjects(true, 2)...)
	var log bytes.Buffer
	res, err := Upgrade(context.Background(), UpgradeDeps{Helm: helm, Dyn: dyn, K8s: cs}, UpgradeOptions{
		ManifestVersion: "2.4.0-3.3175.0+0.0.380",
		Values:          map[string]any{"license": map[string]any{"jwt": "new"}},
		Timeout:         2 * time.Second,
		Poll:            5 * time.Millisecond,
		Log:             &log,
	})
	if err != nil {
		t.Fatalf("Upgrade: %v\n%s", err, log.String())
	}
	if res.ManifestTo != "2.4.0" || res.FLOTo != "v2.30.0-0.5.2" || res.FLOFrom != "v2.21.13-0.0.64" || res.ManifestFrom != "2.3.3-3.2598.3-0.0.509" {
		t.Errorf("result = %+v", res)
	}
	if !res.FLOUpgraded || !res.Patched || !res.ControllerReady || res.TMMReady != 2 || res.TMMTotal != 2 {
		t.Errorf("result flags = %+v", res)
	}
	if len(helm.pulled) != 1 || !strings.HasSuffix(helm.pulled[0], "f5-lifecycle-operator@v2.30.0-0.5.2") || helm.upgradeTo != "v2.30.0-0.5.2" {
		t.Errorf("helm pulled %v upgraded to %q", helm.pulled, helm.upgradeTo)
	}
	if helm.values["license"].(map[string]any)["jwt"] != "new" {
		t.Errorf("helm values = %v (rendered values must win over the release's)", helm.values)
	}
	if !strings.Contains(log.String(), `rewritten to "2.4.0"`) {
		t.Errorf("log lacks the version rewrite note:\n%s", log.String())
	}

	// The CNEInstance carries the new version and the env upserts, keeping
	// the existing entries.
	cne, err := dyn.Resource(CNEInstanceGVR).Namespace(DefaultInstanceNamespace).Get(context.Background(), "lab-bnk", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v := stringAt(cne.Object, "spec", "manifestVersion"); v != "2.4.0" {
		t.Errorf("manifestVersion = %q", v)
	}
	ctrlEnv, _, _ := unstructured.NestedSlice(cne.Object, "spec", "advanced", "cneController", "env")
	want := map[string]string{"TMM_DEFAULT_MTU": "9000", "CLOUD_PROVIDER": "aws", UseGatewaySettingsEnv: "true", MaxActiveTMMEnv: "32"}
	if got := envMap(ctrlEnv); len(got) != len(want) {
		t.Errorf("controller env = %v", got)
	} else {
		for k, v := range want {
			if got[k] != v {
				t.Errorf("controller env %s = %q, want %q", k, got[k], v)
			}
		}
	}
	tmmEnv, _, _ := unstructured.NestedSlice(cne.Object, "spec", "advanced", "tmm", "env")
	if got := envMap(tmmEnv); got["TMM_CALICO_ROUTER"] != "default" || got["ZEBOS_STATE"] != "legacy" {
		t.Errorf("tmm env = %v", got)
	}
}

func envMap(env []any) map[string]string {
	out := map[string]string{}
	for _, e := range env {
		m := e.(map[string]any)
		out[m["name"].(string)] = m["value"].(string)
	}
	return out
}

func TestUpgrade_DryRun(t *testing.T) {
	helm := &fakeHelm{deployed: "v2.21.13-0.0.64"}
	dyn := newFakeDynamic(t, cneReady(t))
	var log bytes.Buffer
	res, err := Upgrade(context.Background(), UpgradeDeps{Helm: helm, Dyn: dyn}, UpgradeOptions{DryRun: true, Log: &log})
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if len(helm.upgraded) != 0 || len(helm.pulled) != 0 || res.Patched || res.FLOUpgraded || !res.DryRun {
		t.Errorf("dry-run changed something: helm=%+v res=%+v", helm, res)
	}
	out := log.String()
	for _, want := range []string{
		"would helm upgrade f5-lifecycle-operator v2.21.13-0.0.64 -> v2.30.0-0.5.2",
		`would patch CNEInstance f5-cne-system/lab-bnk (manifestVersion "2.3.3-3.2598.3-0.0.509" -> "2.4.0")`,
		`"USE_GATEWAY_SETTINGS","value":"true"`,
		"step 3: dry-run: would wait",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
	cne, _ := dyn.Resource(CNEInstanceGVR).Namespace(DefaultInstanceNamespace).Get(context.Background(), "lab-bnk", metav1.GetOptions{})
	if v := stringAt(cne.Object, "spec", "manifestVersion"); v != "2.3.3-3.2598.3-0.0.509" {
		t.Errorf("dry-run patched the CNEInstance: %q", v)
	}
	if res.ManifestTo != "2.4.0" {
		t.Errorf("default target = %q", res.ManifestTo)
	}
}

func TestUpgrade_AlreadyCurrent(t *testing.T) {
	helm := &fakeHelm{deployed: "v2.30.0-0.5.2"}
	cne := cneReady(t)
	_ = unstructured.SetNestedField(cne.Object, "2.4.0", "spec", "manifestVersion")
	env, _, _ := unstructured.NestedSlice(cne.Object, "spec", "advanced", "cneController", "env")
	env = append(env, map[string]any{"name": UseGatewaySettingsEnv, "value": "true"}, map[string]any{"name": MaxActiveTMMEnv, "value": "64"})
	_ = unstructured.SetNestedSlice(cne.Object, env, "spec", "advanced", "cneController", "env")
	tmmEnv, _, _ := unstructured.NestedSlice(cne.Object, "spec", "advanced", "tmm", "env")
	_ = unstructured.SetNestedSlice(cne.Object, append(tmmEnv, map[string]any{"name": "ZEBOS_STATE", "value": "legacy"}), "spec", "advanced", "tmm", "env")
	dyn := newFakeDynamic(t, cne, infraCRD())
	cs := k8sfake.NewClientset(controllerObjects(true, 1)...)
	var log bytes.Buffer
	res, err := Upgrade(context.Background(), UpgradeDeps{Helm: helm, Dyn: dyn, K8s: cs}, UpgradeOptions{Timeout: time.Second, Poll: time.Millisecond, Log: &log})
	if err != nil {
		t.Fatalf("Upgrade: %v\n%s", err, log.String())
	}
	if res.FLOUpgraded || res.Patched || len(helm.upgraded) != 0 {
		t.Errorf("idempotent run changed something: %+v", res)
	}
	if !strings.Contains(log.String(), "already at v2.30.0-0.5.2") || !strings.Contains(log.String(), "already at manifestVersion 2.4.0") {
		t.Errorf("log:\n%s", log.String())
	}
}

func TestUpgrade_Errors(t *testing.T) {
	ctx := context.Background()
	// No release.
	_, err := Upgrade(ctx, UpgradeDeps{Helm: &fakeHelm{}, Dyn: newFakeDynamic(t)}, UpgradeOptions{})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("no release: %v", err)
	}
	// Unknown manifest without --flo-version.
	_, err = Upgrade(ctx, UpgradeDeps{Helm: &fakeHelm{deployed: "x"}, Dyn: newFakeDynamic(t)}, UpgradeOptions{ManifestVersion: "9.9.9"})
	if err == nil || !strings.Contains(err.Error(), "not in the known releases") {
		t.Errorf("unknown manifest: %v", err)
	}
	// Pull failure surfaces.
	_, err = Upgrade(ctx, UpgradeDeps{Helm: &fakeHelm{deployed: "x", failPull: true}, Dyn: newFakeDynamic(t, cneReady(t))}, UpgradeOptions{})
	if err == nil || !strings.Contains(err.Error(), "registry unreachable") {
		t.Errorf("pull failure: %v", err)
	}
	// No CNEInstance.
	_, err = Upgrade(ctx, UpgradeDeps{Helm: &fakeHelm{deployed: "v2.30.0-0.5.2"}, Dyn: newFakeDynamic(t)}, UpgradeOptions{})
	if err == nil || !strings.Contains(err.Error(), "no CNEInstance") {
		t.Errorf("no instance: %v", err)
	}
	// Controller never rendered with the flag and no CNEController to recreate.
	dyn := newFakeDynamic(t, cneReady(t), infraCRD())
	cs := k8sfake.NewClientset(controllerObjects(false, 1)...)
	_, err = Upgrade(ctx, UpgradeDeps{Helm: &fakeHelm{deployed: "v2.30.0-0.5.2"}, Dyn: dyn, K8s: cs}, UpgradeOptions{Timeout: time.Second, Poll: time.Millisecond, RenderWait: 10 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "no CNEController for CNEInstance f5-cne-system/lab-bnk found to recreate") {
		t.Errorf("missing flag: %v", err)
	}
	// No TMM pods.
	dyn = newFakeDynamic(t, cneReady(t), infraCRD())
	cs = k8sfake.NewClientset(controllerObjects(true, 0)...)
	_, err = Upgrade(ctx, UpgradeDeps{Helm: &fakeHelm{deployed: "v2.30.0-0.5.2"}, Dyn: dyn, K8s: cs}, UpgradeOptions{Timeout: time.Second, Poll: time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "TMM data plane not ready: 0/0") {
		t.Errorf("no TMM: %v", err)
	}
	// Condition never True -> the wait names it.
	cne := cneReady(t)
	_ = unstructured.SetNestedSlice(cne.Object, []any{map[string]any{"type": "F5TmmAvailable", "status": "False", "reason": "Pending", "message": "tmm-0 starting"}}, "status", "conditions")
	dyn = newFakeDynamic(t, cne, infraCRD())
	_, err = Upgrade(ctx, UpgradeDeps{Helm: &fakeHelm{deployed: "v2.30.0-0.5.2"}, Dyn: dyn}, UpgradeOptions{Timeout: 30 * time.Millisecond, Poll: 5 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "no CNEControllerAvailable condition yet") {
		t.Errorf("stuck condition: %v", err)
	}
}

func TestCanonicalManifestVersion(t *testing.T) {
	cases := map[string]string{
		"2.4.0-3.3175.0+0.0.380": "2.4.0",
		"2.4.0-3.3175.0-0.0.380": "2.4.0",
		"2.4.0":                  "2.4.0",
		"2.3.3-3.2598.3-0.0.509": "2.3.3-3.2598.3-0.0.509",
		"":                       "",
	}
	for in, want := range cases {
		got, note := CanonicalManifestVersion(in)
		if got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
		if (note != "") != (got != in) {
			t.Errorf("%q: note %q inconsistent with rewrite", in, note)
		}
	}
}

func TestUpsertEnv(t *testing.T) {
	env := []any{map[string]any{"name": "A", "value": "1"}, map[string]any{"name": MaxActiveTMMEnv, "value": "64"}}
	out, changed := upsertEnv(env, UseGatewaySettingsEnv, "true", true)
	if !changed || len(out) != 3 || envMap(out)[UseGatewaySettingsEnv] != "true" {
		t.Errorf("append: %v %v", out, changed)
	}
	out, changed = upsertEnv(out, MaxActiveTMMEnv, "32", false)
	if changed || envMap(out)[MaxActiveTMMEnv] != "64" {
		t.Errorf("no-overwrite must keep 64: %v %v", out, changed)
	}
	out, changed = upsertEnv(out, UseGatewaySettingsEnv, "true", true)
	if changed {
		t.Errorf("same value reported as change")
	}
	out, changed = upsertEnv(append(out, map[string]any{"name": UseGatewaySettingsEnv, "value": "false"}), UseGatewaySettingsEnv, "true", true)
	if !changed || envMap(out)[UseGatewaySettingsEnv] != "true" {
		t.Errorf("overwrite false->true: %v %v", out, changed)
	}
	if len(out) != 4 {
		t.Errorf("entries = %d (order and count preserved)", len(out))
	}
}

// cneControllerCR is the FLO-owned component object for the lab instance.
func cneControllerCR() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "k8s.f5.com/v1", "kind": "CNEController",
		"metadata": map[string]any{
			"name": ControllerDeployment + "-lab-bnk", "namespace": DefaultInstanceNamespace,
			"ownerReferences": []any{map[string]any{"apiVersion": "k8s.f5.com/v1", "kind": "CNEInstance", "name": "lab-bnk", "uid": "u1"}},
		},
		"spec": map[string]any{"crdUpdater": map[string]any{"enabled": true}},
	}}
}

// TestUpgrade_RecreatesController reproduces the live 2026-09-14 upgrade: FLO
// 2.30 cannot update the 2.3-era CNEController, so the Deployment never gets
// USE_GATEWAY_SETTINGS. Deleting the CR (which FLO then recreates) is what
// makes the env land; the fake FLO here is a reactor on the delete.
func TestUpgrade_RecreatesController(t *testing.T) {
	helm := &fakeHelm{deployed: "v2.21.13-0.0.64"}
	dyn := newFakeDynamic(t, cneReady(t), infraCRD(), cneControllerCR())
	cs := k8sfake.NewClientset(controllerObjects(false, 1)...)

	deleted := 0
	dyn.PrependReactor("delete", "cnecontrollers", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deleted++
		// FLO recreates the Deployment with the 2.4 env.
		dep, err := cs.AppsV1().Deployments(DefaultInstanceNamespace).Get(context.Background(), ControllerDeployment, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		flag := corev1.EnvVar{Name: UseGatewaySettingsEnv, Value: "true"}
		dep.Spec.Template.Spec.Containers[0].Env = append(dep.Spec.Template.Spec.Containers[0].Env, flag)
		if _, err := cs.AppsV1().Deployments(DefaultInstanceNamespace).Update(context.Background(), dep, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		pod, err := cs.CoreV1().Pods(DefaultInstanceNamespace).Get(context.Background(), "f5-cne-controller-abc", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, flag)
		if _, err := cs.CoreV1().Pods(DefaultInstanceNamespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		return false, nil, nil
	})

	var log bytes.Buffer
	res, err := Upgrade(context.Background(), UpgradeDeps{Helm: helm, Dyn: dyn, K8s: cs}, UpgradeOptions{
		Timeout:    2 * time.Second,
		Poll:       5 * time.Millisecond,
		RenderWait: 30 * time.Millisecond,
		Log:        &log,
	})
	if err != nil {
		t.Fatalf("Upgrade: %v\n%s", err, log.String())
	}
	if deleted != 1 || !res.ControllerRecreated || !res.ControllerReady || res.TMMReady != 1 {
		t.Errorf("deleted=%d result=%+v\n%s", deleted, res, log.String())
	}
	if !strings.Contains(log.String(), "recreating the CNEController CR") {
		t.Errorf("log:\n%s", log.String())
	}
	if _, err := dyn.Resource(CNEControllerGVR).Namespace(DefaultInstanceNamespace).Get(context.Background(), ControllerDeployment+"-lab-bnk", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("CNEController still present (err=%v); FLO would have recreated it", err)
	}
}

// TestUpgrade_NeverRenderedFails: when even the recreate does not make FLO
// render the env, the upgrade fails naming the Deployment.
func TestUpgrade_NeverRenderedFails(t *testing.T) {
	helm := &fakeHelm{deployed: "v2.30.0-0.5.2"}
	dyn := newFakeDynamic(t, cneReady(t), infraCRD(), cneControllerCR())
	cs := k8sfake.NewClientset(controllerObjects(false, 1)...)
	var log bytes.Buffer
	res, err := Upgrade(context.Background(), UpgradeDeps{Helm: helm, Dyn: dyn, K8s: cs}, UpgradeOptions{
		Timeout: 60 * time.Millisecond, Poll: 5 * time.Millisecond, RenderWait: 20 * time.Millisecond, Log: &log,
	})
	if err == nil || !strings.Contains(err.Error(), "after recreating the CNEController") {
		t.Fatalf("err = %v", err)
	}
	if !res.ControllerRecreated {
		t.Errorf("result = %+v", res)
	}
}

type recordApplier struct{ applied []string }

func (r *recordApplier) Apply(_ context.Context, obj *unstructured.Unstructured) error {
	r.applied = append(r.applied, obj.GetKind()+"/"+obj.GetName())
	return nil
}

const rbacSupplement = `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: lab-cne-controller-endpointslices}
rules:
- apiGroups: [discovery.k8s.io]
  resources: [endpointslices]
  verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: lab-cne-controller-endpointslices}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: lab-cne-controller-endpointslices}
subjects: [{kind: ServiceAccount, name: f5-cne-controller, namespace: f5-cne-system}]
`

// TestUpgrade_AppliesEndpointSliceRBAC: the fake API server denies the
// SubjectAccessReview (zero status), so the supplement is applied and the
// controller restarted; with the review allowed nothing is applied.
func TestUpgrade_AppliesEndpointSliceRBAC(t *testing.T) {
	helm := &fakeHelm{deployed: "v2.30.0-0.5.2"}
	dyn := newFakeDynamic(t, cneReady(t), infraCRD())
	cs := k8sfake.NewClientset(controllerObjects(true, 1)...)
	cs.PrependReactor("create", "subjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SubjectAccessReview{Status: authv1.SubjectAccessReviewStatus{Allowed: false}}, nil
	})
	applier := &recordApplier{}
	var log bytes.Buffer
	res, err := Upgrade(context.Background(), UpgradeDeps{Helm: helm, Dyn: dyn, K8s: cs, Applier: applier}, UpgradeOptions{
		Timeout: 2 * time.Second, Poll: 5 * time.Millisecond, RenderWait: 20 * time.Millisecond,
		ControllerRBAC: []byte(rbacSupplement), Log: &log,
	})
	if err != nil {
		t.Fatalf("Upgrade: %v\n%s", err, log.String())
	}
	if !res.RBACApplied || strings.Join(applier.applied, ",") != "ClusterRole/lab-cne-controller-endpointslices,ClusterRoleBinding/lab-cne-controller-endpointslices" {
		t.Errorf("rbac applied=%v objects=%v", res.RBACApplied, applier.applied)
	}
	dep, _ := cs.AppsV1().Deployments(DefaultInstanceNamespace).Get(context.Background(), ControllerDeployment, metav1.GetOptions{})
	if dep.Spec.Template.Annotations["awsbnkctl.f5.com/restartedAt"] == "" {
		t.Errorf("controller not restarted after the RBAC supplement")
	}

	// Allowed: nothing applied.
	cs2 := k8sfake.NewClientset(controllerObjects(true, 1)...)
	cs2.PrependReactor("create", "subjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SubjectAccessReview{Status: authv1.SubjectAccessReviewStatus{Allowed: true}}, nil
	})
	applier2 := &recordApplier{}
	res, err = Upgrade(context.Background(), UpgradeDeps{Helm: helm, Dyn: newFakeDynamic(t, cneReady(t), infraCRD()), K8s: cs2, Applier: applier2}, UpgradeOptions{
		Timeout: 2 * time.Second, Poll: 5 * time.Millisecond, ControllerRBAC: []byte(rbacSupplement), Log: &log,
	})
	if err != nil || res.RBACApplied || len(applier2.applied) != 0 {
		t.Errorf("allowed: err=%v applied=%v objs=%v", err, res.RBACApplied, applier2.applied)
	}
}

// Step 2 carries the EKS service range into the TMM env as TMM_K8S_ROUTES
// (the f5-tmm chart's add_k8s_routes) and leaves an operator-set value alone.
func TestCNEInstancePatch_TMMK8sRoutes(t *testing.T) {
	cne := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"manifestVersion": "2.3.0-3.2598.3-0.0.170"},
	}}
	patch, changed := cneInstancePatch(cne, "2.4.0", "172.20.0.0/16")
	if !changed {
		t.Fatal("patch must report a change")
	}
	tmmEnv := patch["spec"].(map[string]any)["advanced"].(map[string]any)["tmm"].(map[string]any)["env"].([]any)
	if got := envMap(tmmEnv)[render.TMMK8sRoutesEnv]; got != "172.20.0.0/16" {
		t.Errorf("%s = %q, want 172.20.0.0/16", render.TMMK8sRoutesEnv, got)
	}
	// Existing value kept; no service range → no entry.
	cne.Object["spec"].(map[string]any)["advanced"] = map[string]any{"tmm": map[string]any{"env": []any{map[string]any{"name": render.TMMK8sRoutesEnv, "value": "10.100.0.0/16,10.0.0.0/16"}}}}
	patch, _ = cneInstancePatch(cne, "2.4.0", "172.20.0.0/16")
	tmmEnv = patch["spec"].(map[string]any)["advanced"].(map[string]any)["tmm"].(map[string]any)["env"].([]any)
	if got := envMap(tmmEnv)[render.TMMK8sRoutesEnv]; got != "10.100.0.0/16,10.0.0.0/16" {
		t.Errorf("operator value overwritten: %q", got)
	}
	patch, _ = cneInstancePatch(&unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}, "2.4.0", "")
	tmmEnv = patch["spec"].(map[string]any)["advanced"].(map[string]any)["tmm"].(map[string]any)["env"].([]any)
	if _, ok := envMap(tmmEnv)[render.TMMK8sRoutesEnv]; ok {
		t.Errorf("empty service range must not add %s", render.TMMK8sRoutesEnv)
	}
}

func TestUpgrade_UncataloguedReleaseAndReconcileIdentity(t *testing.T) {
	helm := &fakeHelm{deployed: "v2.30.0-0.5.2"}
	dyn := newFakeDynamic(t, cneReady(t), infraCRD())
	cs := k8sfake.NewClientset(controllerObjects(true, 1)...)
	reconciled := 0
	var log bytes.Buffer
	res, err := Upgrade(context.Background(), UpgradeDeps{Helm: helm, Dyn: dyn, K8s: cs}, UpgradeOptions{
		ManifestVersion: "2.5.1",
		Timeout:         2 * time.Second,
		Poll:            5 * time.Millisecond,
		ReconcileControllerIdentity: func(context.Context) error {
			reconciled++
			return nil
		},
		Log: &log,
	})
	if err != nil {
		t.Fatalf("Upgrade(2.5.1): %v\n%s", err, log.String())
	}
	if res.FLOTo != "v2.40.0-0.1.0" || res.ManifestTo != "2.5.1" || !res.FLOUpgraded {
		t.Errorf("expected FLOTo=v2.40.0-0.1.0 and ManifestTo=2.5.1, got %+v", res)
	}
	if reconciled == 0 {
		t.Errorf("expected ReconcileControllerIdentity to be called")
	}
}
