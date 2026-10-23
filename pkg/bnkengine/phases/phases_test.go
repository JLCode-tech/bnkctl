package phases

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/bnkconst"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestPhaseNamespacesAndFARSecrets(t *testing.T) {
	ctx := context.Background()
	cs := k8sfake.NewClientset()

	var applied [][]byte
	applyFn := func(_ context.Context, raw []byte) error {
		applied = append(applied, raw)
		return nil
	}

	if err := PhaseNamespaces(ctx, cs, applyFn, []byte("apiVersion: v1\nkind: Namespace\n"), "custom-ns"); err != nil {
		t.Fatalf("PhaseNamespaces: %v", err)
	}
	if len(applied) != 1 {
		t.Fatalf("expected 1 applied manifest, got %d", len(applied))
	}
	for _, ns := range append(RequiredNamespaces, "custom-ns") {
		if _, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); err != nil {
			t.Errorf("expected namespace %s to exist: %v", ns, err)
		}
	}

	// Test ExtractFARPayload with JSON and pre-encoded base64
	rawJSON := `{"type":"service_account","project_id":"f5-far"}`
	b64 := ExtractFARPayload(rawJSON)
	if b64 != base64.StdEncoding.EncodeToString([]byte(rawJSON)) {
		t.Fatalf("ExtractFARPayload JSON mismatch: %s", b64)
	}
	if ExtractFARPayload(b64) != b64 {
		t.Fatalf("ExtractFARPayload base64 idempotency failed")
	}

	// Test DistributeFARSecrets
	if err := DistributeFARSecrets(ctx, cs, rawJSON); err != nil {
		t.Fatalf("DistributeFARSecrets: %v", err)
	}
	for _, ns := range FARSecretNamespaces {
		sec, err := cs.CoreV1().Secrets(ns).Get(ctx, bnkconst.SecretRegistry, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("expected %s in %s: %v", bnkconst.SecretRegistry, ns, err)
		}
		if !strings.Contains(string(sec.Data[corev1.DockerConfigJsonKey]), "repo.f5.com") {
			t.Errorf("expected repo.f5.com in .dockerconfigjson")
		}
	}
}

func TestPhaseCertsAndCNEInstance(t *testing.T) {
	ctx := context.Background()
	cs := k8sfake.NewClientset()
	scheme := runtime.NewScheme()
	cneGVR := schema.GroupVersionResource{Group: "k8s.f5.com", Version: "v1", Resource: "cneinstances"}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		cneGVR: "CNEInstanceList",
	})

	var appliedCount int
	applyFn := func(_ context.Context, raw []byte) error {
		appliedCount++
		return nil
	}

	if err := PhaseCerts(ctx, cs, applyFn, []byte("cert-chain"), []byte("otel-certs")); err != nil {
		t.Fatalf("PhaseCerts: %v", err)
	}
	if appliedCount != 2 {
		t.Fatalf("expected 2 applied YAML docs in PhaseCerts, got %d", appliedCount)
	}
	if _, err := cs.CoreV1().Secrets(bnkconst.InstanceNamespace).Get(ctx, bnkconst.SecretServerCert, metav1.GetOptions{}); err != nil {
		t.Fatalf("expected server cert secret in %s: %v", bnkconst.InstanceNamespace, err)
	}

	// Seed a null-env f5-cne-controller deployment to test ReconcileNullCNEController
	_, _ = cs.AppsV1().Deployments(bnkconst.InstanceNamespace).Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bnkconst.DeployCNEController,
			Namespace: bnkconst.InstanceNamespace,
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "controller",
							Env:  []corev1.EnvVar{{Name: "CWC_HOST", Value: "null"}},
						},
					},
				},
			},
		},
	}, metav1.CreateOptions{})

	var hookCalled bool
	hook := func(_ context.Context, _ kubernetes.Interface, _ dynamic.Interface) error {
		hookCalled = true
		return nil
	}

	if err := PhaseCNEInstance(ctx, cs, dyn, applyFn, []byte("nads"), []byte("rbac"), []byte("cne"), hook, true); err != nil {
		t.Fatalf("PhaseCNEInstance: %v", err)
	}
	if !hookCalled {
		t.Fatalf("expected reconcileIdentity hook to be called")
	}
	if _, err := cs.AppsV1().Deployments(bnkconst.InstanceNamespace).Get(ctx, bnkconst.DeployCNEController, metav1.GetOptions{}); err == nil {
		t.Fatalf("expected null-env deployment %s to be deleted by ReconcileNullCNEController", bnkconst.DeployCNEController)
	}
}

func TestPhaseLicenseAndNetworkAndActivation(t *testing.T) {
	ctx := context.Background()
	cs := k8sfake.NewClientset()

	// Seed blocking quota to test retry
	_, _ = cs.CoreV1().ResourceQuotas(bnkconst.InstanceNamespace).Create(ctx, &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "f5-single-license-quota",
			Namespace: bnkconst.InstanceNamespace,
		},
	}, metav1.CreateOptions{})

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"keys":[{"kty":"RSA","kid":"cpcl-test"}]}`))
	}))
	defer srv.Close()

	attempts := 0
	applyFn := func(_ context.Context, _ []byte) error {
		attempts++
		if attempts == 1 {
			return fmt.Errorf("forbidden: exceeded quota: f5-single-license-quota")
		}
		return nil
	}

	if err := PhaseLicense(ctx, cs, applyFn, []byte("kind: License"), srv.URL); err != nil {
		t.Fatalf("PhaseLicense: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 license apply attempts after quota removal, got %d", attempts)
	}
	cpclSec, err := cs.CoreV1().Secrets(bnkconst.InstanceNamespace).Get(ctx, bnkconst.CPCLKeySecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected synced %s secret: %v", bnkconst.CPCLKeySecretName, err)
	}
	if !strings.Contains(string(cpclSec.Data["jwks.json"]), "cpcl-test") {
		t.Errorf("unexpected jwks.json: %s", string(cpclSec.Data["jwks.json"]))
	}

	var netSteps int
	if err := PhaseNetworkAndEgress(ctx, func(_ context.Context, _ []byte) error {
		netSteps++
		return nil
	}, []byte("gc"), []byte("net"), []byte("egress"), []byte("extra")); err != nil {
		t.Fatalf("PhaseNetworkAndEgress: %v", err)
	}
	if netSteps != 4 {
		t.Fatalf("expected 4 steps applied in PhaseNetworkAndEgress, got %d", netSteps)
	}

	// Test CheckCNEInstanceActivation
	scheme := runtime.NewScheme()
	cneGVR := schema.GroupVersionResource{Group: "k8s.f5.com", Version: "v1", Resource: "cneinstances"}
	cneObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "k8s.f5.com/v1",
			"kind":       "CNEInstance",
			"metadata": map[string]any{
				"name":      "f5-bnk-instance",
				"namespace": bnkconst.InstanceNamespace,
			},
			"status": map[string]any{
				"conditions": []any{
					map[string]any{"type": "CNEControllerAvailable", "status": "True"},
					map[string]any{"type": "F5TmmAvailable", "status": "True"},
				},
			},
		},
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		cneGVR: "CNEInstanceList",
	}, cneObj)

	_, _ = cs.CoreV1().Pods(bnkconst.InstanceNamespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "f5-tmm-abc",
			Namespace: bnkconst.InstanceNamespace,
			Labels:    map[string]string{"app": "f5-tmm"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "f5-tmm-cne", Ready: true},
				{Name: "f5-tmm-clid", Ready: true},
			},
		},
	}, metav1.CreateOptions{})

	act, err := CheckCNEInstanceActivation(ctx, cs, dyn, "f5-bnk-instance")
	if err != nil {
		t.Fatalf("CheckCNEInstanceActivation: %v", err)
	}
	if !act.CNEControllerAvailable || !act.F5TmmAvailable || act.ReadyTMMPods != 1 || act.TotalTMMPods != 1 {
		t.Fatalf("unexpected ActivationStatus: %+v", act)
	}
}
