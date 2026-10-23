package k8s

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func multusDaemonSet(args ...string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: MultusDaemonSet, Namespace: MultusNamespace, Generation: 1},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: MultusContainer, Args: args,
		}}}}},
		Status: appsv1.DaemonSetStatus{ObservedGeneration: 1, DesiredNumberScheduled: 1, UpdatedNumberScheduled: 1, NumberReady: 1, NumberAvailable: 1},
	}
}

func multusPod(started time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-multus-ds-abc", Namespace: MultusNamespace, Labels: map[string]string{"app": "multus"}},
		Status:     corev1.PodStatus{StartTime: &metav1.Time{Time: started}},
	}
}

func unauthorizedEvent(at time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "f5-tmm-x.1", Namespace: "f5-cne-system"},
		Reason:         "FailedCreatePodSandBox",
		Type:           corev1.EventTypeWarning,
		Message:        `Failed to create pod sandbox: plugin type="multus" name="multus-cni-network" failed (add): Multus: [f5-cne-system/f5-tmm-x/uid]: error waiting for pod: Unauthorized`,
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "f5-cne-system", Name: "f5-tmm-x"},
		LastTimestamp:  metav1.Time{Time: at},
	}
}

var quickstartArgs = []string{"--multus-conf-file=auto", "--multus-autoconfig-dir=/host/etc/cni/net.d", "--cni-conf-dir=/host/etc/cni/net.d"}

func TestMultusWatchEnabled(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"quickstart", quickstartArgs, false},
		{"flag", append(append([]string{}, quickstartArgs...), MultusWatchFlag), true},
		{"bare flag", append(append([]string{}, quickstartArgs...), "--cleanup-config-on-exit"), true},
		{"flag off", append(append([]string{}, quickstartArgs...), "--cleanup-config-on-exit=false"), false},
		{"skip watch", append(append([]string{}, quickstartArgs...), MultusWatchFlag, "--skip-config-watch"), false},
		{"static conf", []string{"--multus-conf-file=/tmp/multus-conf/70-multus.conf", MultusWatchFlag}, false},
	}
	for _, c := range cases {
		if got := MultusWatchEnabled(multusDaemonSet(c.args...)); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
	if MultusWatchEnabled(&appsv1.DaemonSet{}) {
		t.Error("no container must not count as enabled")
	}
}

func TestInspectMultus(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 14, 22, 0, 0, 0, time.UTC)

	t.Run("not installed", func(t *testing.T) {
		st, ds, err := InspectMultus(ctx, k8sfake.NewClientset())
		if err != nil || st.Installed || ds != nil {
			t.Errorf("st=%+v ds=%v err=%v", st, ds, err)
		}
	})
	t.Run("quickstart without events", func(t *testing.T) {
		st, _, err := InspectMultus(ctx, k8sfake.NewClientset(multusDaemonSet(quickstartArgs...), multusPod(now.Add(-time.Hour))))
		if err != nil || !st.Installed || st.WatchEnabled || st.Unauthorized != "" {
			t.Errorf("st=%+v err=%v", st, err)
		}
	})
	t.Run("unauthorized after the newest pod", func(t *testing.T) {
		objs := []runtime.Object{multusDaemonSet(quickstartArgs...), multusPod(now.Add(-2 * time.Hour)), unauthorizedEvent(now.Add(-time.Minute))}
		st, _, err := InspectMultus(ctx, k8sfake.NewClientset(objs...))
		if err != nil || st.Unauthorized != "f5-cne-system/f5-tmm-x" {
			t.Errorf("st=%+v err=%v", st, err)
		}
	})
	t.Run("unauthorized before the newest pod is history", func(t *testing.T) {
		objs := []runtime.Object{multusDaemonSet(quickstartArgs...), multusPod(now.Add(-time.Minute)), unauthorizedEvent(now.Add(-time.Hour))}
		st, _, err := InspectMultus(ctx, k8sfake.NewClientset(objs...))
		if err != nil || st.Unauthorized != "" {
			t.Errorf("st=%+v err=%v", st, err)
		}
	})
}

func TestEnsureMultusTokenWatch(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	t.Run("dry-run reports the missing flag", func(t *testing.T) {
		cs := k8sfake.NewClientset(multusDaemonSet(quickstartArgs...), multusPod(now), unauthorizedEvent(now))
		var log strings.Builder
		res, err := EnsureMultusTokenWatch(ctx, cs, true, &log)
		if err != nil || res.Patched || res.Restarted || res.WatchEnabled || !res.Unauthorized {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if !strings.Contains(res.Reason, "f5-cne-system/f5-tmm-x") || !strings.Contains(res.Reason, MultusWatchFlag) {
			t.Errorf("reason %q", res.Reason)
		}
		ds, _ := cs.AppsV1().DaemonSets(MultusNamespace).Get(ctx, MultusDaemonSet, metav1.GetOptions{})
		if MultusWatchEnabled(ds) {
			t.Error("dry-run must not patch")
		}
	})
	t.Run("adds the flag and keeps the other args", func(t *testing.T) {
		cs := k8sfake.NewClientset(multusDaemonSet(quickstartArgs...), multusPod(now))
		res, err := EnsureMultusTokenWatch(ctx, cs, false, nil)
		if err != nil || !res.Patched || res.Restarted {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		ds, _ := cs.AppsV1().DaemonSets(MultusNamespace).Get(ctx, MultusDaemonSet, metav1.GetOptions{})
		got := ds.Spec.Template.Spec.Containers[0].Args
		want := append(append([]string{}, quickstartArgs...), MultusWatchFlag)
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("args %v", got)
		}
		if !MultusWatchEnabled(ds) {
			t.Error("watch not enabled after patch")
		}
	})
	t.Run("watch on and healthy is a no-op", func(t *testing.T) {
		args := append(append([]string{}, quickstartArgs...), MultusWatchFlag)
		cs := k8sfake.NewClientset(multusDaemonSet(args...), multusPod(now))
		res, err := EnsureMultusTokenWatch(ctx, cs, false, nil)
		if err != nil || res.Patched || res.Restarted || !res.WatchEnabled {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})
	t.Run("watch on but unauthorized rolls the pods", func(t *testing.T) {
		args := append(append([]string{}, quickstartArgs...), MultusWatchFlag)
		cs := k8sfake.NewClientset(multusDaemonSet(args...), multusPod(now.Add(-time.Hour)), unauthorizedEvent(now))
		res, err := EnsureMultusTokenWatch(ctx, cs, false, nil)
		if err != nil || res.Patched || !res.Restarted {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		ds, _ := cs.AppsV1().DaemonSets(MultusNamespace).Get(ctx, MultusDaemonSet, metav1.GetOptions{})
		if ds.Spec.Template.Annotations[multusRestartAnnotation] == "" {
			t.Error("restart annotation missing")
		}
	})
	t.Run("not installed", func(t *testing.T) {
		res, err := EnsureMultusTokenWatch(ctx, k8sfake.NewClientset(), false, nil)
		if err != nil || res.Installed {
			t.Errorf("res=%+v err=%v", res, err)
		}
	})
}
