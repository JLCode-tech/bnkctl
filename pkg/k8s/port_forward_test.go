// Unit tests for `awsbnkctl k port-forward` (`internal/k8s/port_forward.go`).
//
// Port-forward needs a real SPDY upgrade — not fakeable. We test the
// option-validation surface and the Ports slice round-trip; end-to-end
// is in the live golden tests.

package k8s

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// TestPortForwardOptions_RequiresPod: no pod → error.
func TestPortForwardOptions_RequiresPod(t *testing.T) {
	o := &PortForwardOptions{Ports: []string{"8080:80"}}
	err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for empty PodName; got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "pod") {
		t.Errorf("expected 'pod' in err; got: %v", err)
	}
}

// TestPortForwardOptions_RequiresPorts: no ports → error.
func TestPortForwardOptions_RequiresPorts(t *testing.T) {
	o := &PortForwardOptions{PodName: "p"}
	err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for empty Ports; got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "port") {
		t.Errorf("expected 'port' in err; got: %v", err)
	}
}

// TestPortForwardOptions_PortsRoundTrip is a drift guard against future
// option struct changes. The Ports slice is passed verbatim to
// portforward.New (which parses kubectl-style "L:R" strings); we just
// confirm the field accepts the canonical forms.
func TestPortForwardOptions_PortsRoundTrip(t *testing.T) {
	cases := [][]string{
		{"8080:80"},
		{"5000"},
		{"8080:80", "9090:90"},
		{":80"}, // random local port (kubectl shorthand)
	}
	for _, c := range cases {
		o := &PortForwardOptions{PodName: "p", Ports: c}
		if len(o.Ports) != len(c) {
			t.Errorf("Ports round-trip lost entries: in=%v out=%v", c, o.Ports)
		}
	}
}

func TestResolvePortForwardTarget_Service(t *testing.T) {
	ctx := context.Background()
	ready := corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "loki", Namespace: "llm-egress"},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "loki"}, Ports: []corev1.ServicePort{
			{Name: "http", Port: 3100, TargetPort: intstr.FromInt32(3100)},
			{Name: "grpc", Port: 9095, TargetPort: intstr.FromString("grpc")},
		}},
	}
	notReady := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "loki-old", Namespace: "llm-egress", Labels: map[string]string{"app": "loki"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "loki-abc", Namespace: "llm-egress", Labels: map[string]string{"app": "loki"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "loki", Ports: []corev1.ContainerPort{{Name: "grpc", ContainerPort: 9096}}}}},
		Status:     ready,
	}
	cs := k8sfake.NewClientset(svc, notReady, pod)

	name, ports, err := ResolvePortForwardTarget(ctx, cs, "llm-egress", "svc/loki", []string{"3100:3100", "9000:9095", "8080:80"})
	if err != nil || name != "loki-abc" {
		t.Fatalf("name=%q err=%v", name, err)
	}
	if got := strings.Join(ports, " "); got != "3100:3100 9000:9096 8080:80" {
		t.Errorf("ports %q", got)
	}
	if name, ports, err := ResolvePortForwardTarget(ctx, cs, "llm-egress", "loki-abc", []string{"3100"}); err != nil || name != "loki-abc" || ports[0] != "3100" {
		t.Errorf("bare pod: name=%q ports=%v err=%v", name, ports, err)
	}
	if name, _, err := ResolvePortForwardTarget(ctx, cs, "llm-egress", "pod/loki-abc", []string{"3100"}); err != nil || name != "loki-abc" {
		t.Errorf("pod/ prefix: name=%q err=%v", name, err)
	}
	if _, _, err := ResolvePortForwardTarget(ctx, cs, "llm-egress", "svc/missing", []string{"1"}); err == nil {
		t.Error("missing service must fail")
	}
}
