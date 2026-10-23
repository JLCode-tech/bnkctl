package k8s

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const shippedStdoutConf = "\n# To enable printing logs to stdout, uncomment the <store> block below:\n#\n# <store>\n#   @type stdout\n# </store>\n"

func TestStdoutStoreOn(t *testing.T) {
	if stdoutStoreOn(shippedStdoutConf) {
		t.Fatal("the shipped, commented-out store must not count as enabled")
	}
	if !stdoutStoreOn(TMMLogStdoutStore) {
		t.Fatal("the enabled store must count as enabled")
	}
	if stdoutStoreOn("") {
		t.Fatal("an empty key is not enabled")
	}
}

func TestEnableTMMLogStream_PatchesAndBounces(t *testing.T) {
	cs := fake.NewClientset(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: TMMLogCustomConfigMap, Namespace: TMMLogNamespace}, Data: map[string]string{TMMLogStdoutKey: shippedStdoutConf}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "f5-toda-fluentd-old", Namespace: TMMLogNamespace, Labels: map[string]string{"app": "toda-fluentd"}}},
	)
	on, err := TMMLogStreamEnabled(context.Background(), cs)
	if err != nil || on {
		t.Fatalf("TMMLogStreamEnabled before = %v, %v; want false, nil", on, err)
	}
	changed, err := EnableTMMLogStream(context.Background(), cs, 0)
	if err != nil || !changed {
		t.Fatalf("EnableTMMLogStream = %v, %v; want true, nil", changed, err)
	}
	cm, _ := cs.CoreV1().ConfigMaps(TMMLogNamespace).Get(context.Background(), TMMLogCustomConfigMap, metav1.GetOptions{})
	if cm.Data[TMMLogStdoutKey] != TMMLogStdoutStore {
		t.Errorf("stdout.conf = %q, want the enabled store", cm.Data[TMMLogStdoutKey])
	}
	pods, _ := cs.CoreV1().Pods(TMMLogNamespace).List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Errorf("fluentd pod not deleted: %d pod(s) left", len(pods.Items))
	}
	on, err = TMMLogStreamEnabled(context.Background(), cs)
	if err != nil || !on {
		t.Fatalf("TMMLogStreamEnabled after = %v, %v; want true, nil", on, err)
	}
	// Second call: nothing to do.
	changed, err = EnableTMMLogStream(context.Background(), cs, time.Second)
	if err != nil || changed {
		t.Fatalf("second EnableTMMLogStream = %v, %v; want false, nil", changed, err)
	}
}

func TestEnableTMMLogStream_WaitsForReadyPod(t *testing.T) {
	cs := fake.NewClientset(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: TMMLogCustomConfigMap, Namespace: TMMLogNamespace}, Data: map[string]string{TMMLogStdoutKey: shippedStdoutConf}},
	)
	go func() {
		time.Sleep(200 * time.Millisecond)
		_, _ = cs.CoreV1().Pods(TMMLogNamespace).Create(context.Background(), &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "f5-toda-fluentd-new", Namespace: TMMLogNamespace, Labels: map[string]string{"app": "toda-fluentd"}},
			Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
		}, metav1.CreateOptions{})
	}()
	changed, err := EnableTMMLogStream(context.Background(), cs, 30*time.Second)
	if err != nil || !changed {
		t.Fatalf("EnableTMMLogStream = %v, %v; want true, nil", changed, err)
	}
}

func TestEnableTMMLogStream_FallbackToF5Bnk(t *testing.T) {
	cs := fake.NewClientset(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: TMMLogCustomConfigMap, Namespace: "f5-cne-system"}, Data: map[string]string{TMMLogStdoutKey: shippedStdoutConf}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "f5-toda-fluentd-bnk", Namespace: "f5-cne-system", Labels: map[string]string{"app": "toda-fluentd"}}},
	)
	on, err := TMMLogStreamEnabled(context.Background(), cs)
	if err != nil || on {
		t.Fatalf("TMMLogStreamEnabled before = %v, %v; want false, nil", on, err)
	}
	changed, err := EnableTMMLogStream(context.Background(), cs, 0)
	if err != nil || !changed {
		t.Fatalf("EnableTMMLogStream = %v, %v; want true, nil", changed, err)
	}
	on, err = TMMLogStreamEnabled(context.Background(), cs)
	if err != nil || !on {
		t.Fatalf("TMMLogStreamEnabled after = %v, %v; want true, nil", on, err)
	}
}
