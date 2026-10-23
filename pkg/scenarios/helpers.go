package scenarios

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"text/template"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// HTTPRouteGVR is the GroupVersionResource for Gateway API HTTPRoute objects.
var HTTPRouteGVR = schema.GroupVersionResource{
	Group:    "gateway.networking.k8s.io",
	Version:  "v1",
	Resource: "httproutes",
}

// GatewayGVR is the GroupVersionResource for Gateway API Gateway objects.
var GatewayGVR = schema.GroupVersionResource{
	Group:    "gateway.networking.k8s.io",
	Version:  "v1",
	Resource: "gateways",
}

// RenderTemplate executes a Go text/template with the given data and returns
// the rendered string.
func RenderTemplate(tmpl string, data interface{}) (string, error) {
	t, err := template.New("manifest").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// WaitDeploymentAvailable polls until the named Deployment has the Available
// condition set to True, or timeout elapses.
func WaitDeploymentAvailable(ctx context.Context, sctx *Context, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		dep, err := sctx.Clientset.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			for _, c := range dep.Status.Conditions {
				if string(c.Type) == "Available" && string(c.Status) == "True" {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("deployment %s/%s not Available after %s", ns, name, timeout)
}

// WaitCondition polls a resource (identified by gvr) for a named condition
// with status=True in .status.conditions or .status.parents[*].conditions.
func WaitCondition(ctx context.Context, sctx *Context, gvr schema.GroupVersionResource, ns, name, condType string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		obj, err := sctx.Dynamic.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			// Check standard .status.conditions
			conditions, _, _ := NestedSlice(obj.Object, "status", "conditions")
			for _, cRaw := range conditions {
				c, ok := cRaw.(map[string]interface{})
				if !ok {
					continue
				}
				if c["type"] == condType && c["status"] == "True" {
					return nil
				}
			}

			// Check route-style .status.parents[*].conditions
			parents, _, _ := NestedSlice(obj.Object, "status", "parents")
			for _, pRaw := range parents {
				p, ok := pRaw.(map[string]interface{})
				if !ok {
					continue
				}
				pConditions, _, _ := NestedSlice(p, "conditions")
				for _, cRaw := range pConditions {
					c, ok2 := cRaw.(map[string]interface{})
					if !ok2 {
						continue
					}
					if c["type"] == condType && c["status"] == "True" {
						return nil
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("%s %s/%s condition %s not True after %s", gvr.Resource, ns, name, condType, timeout)
}

// WaitHTTPRouteCondition polls the HTTPRoute's .status.parents[*].conditions
// for the named condition with status=True.
func WaitHTTPRouteCondition(ctx context.Context, sctx *Context, ns, name, condType string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		obj, err := sctx.Dynamic.Resource(HTTPRouteGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			parents, _, _ := NestedSlice(obj.Object, "status", "parents")
			for _, pRaw := range parents {
				p, ok := pRaw.(map[string]interface{})
				if !ok {
					continue
				}
				conditions, _, _ := NestedSlice(p, "conditions")
				for _, cRaw := range conditions {
					c, ok2 := cRaw.(map[string]interface{})
					if !ok2 {
						continue
					}
					if c["type"] == condType && c["status"] == "True" {
						return nil
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("HTTPRoute %s/%s condition %s not True after %s", ns, name, condType, timeout)
}

// BuildProbeParams extracts VIP, iteration count, and per-probe timeout from
// the scenario context options and cluster intent.
func BuildProbeParams(ctx *Context) (vip string, iterations int, timeout time.Duration, err error) {
	vip = ctx.Options["vip"]
	if vip == "" && ctx.Cluster != nil {
		vip, err = ctx.Cluster.DefaultVIP()
		if err != nil {
			return "", 0, 0, fmt.Errorf("deriving VIP: %w", err)
		}
	}
	if vip == "" {
		return "", 0, 0, fmt.Errorf("VIP not set — pass --vip or set network.dataPath.external.cidr")
	}
	iterations = 5
	if v := ctx.Options["iterations"]; v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			iterations = n
		}
	}
	timeout = 10 * time.Second
	if v := ctx.Options["timeout"]; v != "" {
		if d, e := time.ParseDuration(v); e == nil && d > 0 {
			timeout = d
		}
	}
	return vip, iterations, timeout, nil
}

// FinalizeResult stamps Status and Summary onto res based on AllPassed().
func FinalizeResult(res Result) Result {
	if res.AllPassed() {
		res.Status = "ok"
		if res.DataPath {
			res.Summary = "control-plane reconciled + end-to-end data-plane traffic verified"
		} else {
			res.Summary = "control-plane reconciled + assertions passed"
		}
	} else {
		res.Status = "failed"
		var failed []string
		for _, a := range res.Assertions {
			if !a.OK {
				failed = append(failed, a.Description)
			}
		}
		res.Summary = "failed: " + strings.Join(failed, "; ")
	}
	return res
}

// ErrString returns a single-line string representation of err, or "" if nil.
func ErrString(err error) string {
	if err == nil {
		return ""
	}
	return oneLine(err.Error(), 200)
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// IsNotFound returns true when err is a not-found style error.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "not found")
}

// PollMarkers calls fn repeatedly until it returns ok=true or maxWait elapses,
// sleeping interval between attempts. fn is always called at least once.
// Returns the final (ok, detail). Honors ctx cancellation.
func PollMarkers(ctx context.Context, maxWait, interval time.Duration, fn func() (ok bool, detail string)) (bool, string) {
	deadline := time.Now().Add(maxWait)
	ok, detail := fn()
	for !ok && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ok, detail
		case <-time.After(interval):
		}
		ok, detail = fn()
	}
	return ok, detail
}

// WithLastOctet returns ip with its last octet replaced by octet. Every
// scenario owns one fixed last octet in the external data-path subnet so their
// GatewaySettings pools never overlap when `scenarios run --all` leaves earlier
// scenarios applied. The allocation is documented in docs/SCENARIOS.md
// ("VIP plan"); http-routing-e2e alone keeps the cluster default (.100).
// Non-IPv4 input is returned unchanged.
func WithLastOctet(ip, octet string) string {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return ip
	}
	parts[3] = octet
	return strings.Join(parts, ".")
}

// ExecInPod runs command inside the named container of a pod over the exec
// subresource and returns combined stdout (stderr is appended on failure).
// Scenarios use it to drive traffic FROM a pod — the egress direction, which
// no jumphost probe can exercise. Needs both Clientset and RESTConfig on the
// Context.
func ExecInPod(ctx *Context, ns, pod, container string, command ...string) (string, error) {
	if ctx.Clientset == nil || ctx.RESTConfig == nil {
		return "", fmt.Errorf("exec in %s/%s: no Kubernetes client (Clientset/RESTConfig nil)", ns, pod)
	}
	req := ctx.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(ctx.RESTConfig, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("exec in %s/%s: SPDY executor: %w", ns, pod, err)
	}
	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx.Ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		return stdout.String(), fmt.Errorf("exec in %s/%s (%v): %w; stderr: %s", ns, pod, command, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
