package k8s

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

// Defaults for the iperf3 fixture. Pod and service share a name so the
// teardown logic can find them with a fixed label selector.
const (
	Iperf3Namespace    = "awsbnkctl-test"
	Iperf3PodName      = "awsbnkctl-iperf3"
	Iperf3SvcName      = "awsbnkctl-iperf3"
	Iperf3Port         = 5201
	Iperf3DefaultImage = "networkstatic/iperf3:latest"

	defaultReadyTimeout = 3 * time.Minute
	defaultLBTimeout    = 5 * time.Minute
	pollInterval        = 2 * time.Second
)

// Iperf3Options configures the in-cluster fixture.
type Iperf3Options struct {
	Namespace   string             // default: "awsbnkctl-test"
	Image       string             // default: networkstatic/iperf3:latest
	ServiceType corev1.ServiceType // ClusterIP for east-west, LoadBalancer for north-south
}

// DeployIperf3 ensures the test namespace exists, then creates the
// iperf3 server Pod + Service. Idempotent: existing fixtures are
// re-used (returns success). Caller follows up with WaitIperf3Ready
// and (for LoadBalancer) WaitLoadBalancerEndpoint.
func (c *Client) DeployIperf3(ctx context.Context, opts Iperf3Options) error {
	if opts.Namespace == "" {
		opts.Namespace = Iperf3Namespace
	}
	if opts.Image == "" {
		opts.Image = Iperf3DefaultImage
	}
	if opts.ServiceType == "" {
		opts.ServiceType = corev1.ServiceTypeClusterIP
	}

	if err := c.ensureNamespace(ctx, opts.Namespace); err != nil {
		return err
	}

	pod := BuildIperf3Pod(opts)
	svc := BuildIperf3Service(opts)

	if _, err := c.clientset.CoreV1().Pods(opts.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating iperf3 pod: %w", err)
		}
	}
	if _, err := c.clientset.CoreV1().Services(opts.Namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating iperf3 service: %w", err)
		}
	}
	return nil
}

// BuildIperf3Pod returns the iperf3 server Pod object. Extracted from
// DeployIperf3 so the PSA-compliance contract is unit-testable without
// a clientset.
//
// SCC / Pod Security Admission posture for EKS 1.25+ `restricted`
// profile + OpenShift restricted-v2:
//
//   - PodSecurityContext.RunAsNonRoot=true (admission rejects RunAsUser=0)
//   - PodSecurityContext.SeccompProfile.Type=RuntimeDefault (PSA
//     `restricted` requires a non-Unconfined seccomp profile)
//   - Container.AllowPrivilegeEscalation=false
//   - Container.RunAsNonRoot=true (defence-in-depth; pod-level setting
//     covers but container-level pin is what `restricted` validates
//     against on EKS)
//   - Container.Capabilities.Drop=[ALL] (PSA `restricted` requires the
//     ALL drop to baseline)
//
// This PSA compliance contract is intentional and must be preserved.
//
// RunAsUser is set to 1000 at the pod level for plain Kubernetes (the
// bundled awsbnkctl-tools-iperf3 image's USER directive). OpenShift's
// SCC mutating webhook overrides this with a namespace-allocated UID
// so the pinned 1000 doesn't collide; on EKS the value is honoured as-is.
func BuildIperf3Pod(opts Iperf3Options) *corev1.Pod {
	if opts.Namespace == "" {
		opts.Namespace = Iperf3Namespace
	}
	if opts.Image == "" {
		opts.Image = Iperf3DefaultImage
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      Iperf3PodName,
			Namespace: opts.Namespace,
			Labels:    map[string]string{"app": Iperf3PodName, "awsbnkctl.io/test": "iperf3"},
		},
		Spec: corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptr.To(true),
				RunAsUser:    ptr.To(int64(1000)),
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Containers: []corev1.Container{{
				Name:  "iperf3",
				Image: opts.Image,
				Args:  []string{"-s"},
				Ports: []corev1.ContainerPort{{ContainerPort: Iperf3Port, Protocol: corev1.ProtocolTCP}},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					RunAsNonRoot:             ptr.To(true),
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
					},
				},
			}},
			RestartPolicy: corev1.RestartPolicyAlways,
		},
	}
}

// BuildIperf3Service returns the iperf3 Service object. Extracted from
// DeployIperf3 for parity with BuildIperf3Pod — keeps the spec-builder
// surface unit-testable.
//
// When opts.ServiceType=LoadBalancer on EKS, the AWS Load Balancer
// Controller (or the in-tree provider) provisions an NLB by default.
// Operators can set the workspace's
// test.throughput.lb_annotations map to add AWS-specific annotations
// such as `service.beta.kubernetes.io/aws-load-balancer-type: external`
// + `service.beta.kubernetes.io/aws-load-balancer-nlb-target-type: ip`
// for the modern NLB shape. v0.x reads no such annotations; the
// AWS-default NLB is acceptable for north-south throughput testing.
func BuildIperf3Service(opts Iperf3Options) *corev1.Service {
	if opts.Namespace == "" {
		opts.Namespace = Iperf3Namespace
	}
	if opts.ServiceType == "" {
		opts.ServiceType = corev1.ServiceTypeClusterIP
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      Iperf3SvcName,
			Namespace: opts.Namespace,
			Labels:    map[string]string{"app": Iperf3PodName, "awsbnkctl.io/test": "iperf3"},
		},
		Spec: corev1.ServiceSpec{
			Type:     opts.ServiceType,
			Selector: map[string]string{"app": Iperf3PodName},
			Ports: []corev1.ServicePort{{
				Port:       Iperf3Port,
				TargetPort: intstr.FromInt(Iperf3Port),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// ensureNamespace creates the namespace if it doesn't already exist.
func (c *Client) ensureNamespace(ctx context.Context, name string) error {
	_, err := c.clientset.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("checking namespace %s: %w", name, err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"awsbnkctl.io/test": "true"}}}
	if _, err := c.clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating namespace %s: %w", name, err)
		}
	}
	return nil
}

// WaitIperf3Ready polls until the pod is Running with all containers
// reporting Ready. Default timeout 3 minutes — generous because a
// cold image pull on slow nodes can take a while.
func (c *Client) WaitIperf3Ready(ctx context.Context, namespace string, timeout time.Duration) error {
	if namespace == "" {
		namespace = Iperf3Namespace
	}
	if timeout == 0 {
		timeout = defaultReadyTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		pod, err := c.clientset.CoreV1().Pods(namespace).Get(ctx, Iperf3PodName, metav1.GetOptions{})
		if err == nil && pod.Status.Phase == corev1.PodRunning && allContainersReady(pod) {
			return nil
		}
		if time.Now().After(deadline) {
			phase := "unknown"
			if pod != nil {
				phase = string(pod.Status.Phase)
			}
			return fmt.Errorf("timeout waiting for iperf3 pod ready (phase=%s)", phase)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func allContainersReady(pod *corev1.Pod) bool {
	if len(pod.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if !cs.Ready {
			return false
		}
	}
	return true
}

// WaitLoadBalancerEndpoint polls until the iperf3 Service has an
// external IP or hostname assigned. Returns the address. Default
// timeout 5 minutes — IBM Cloud LB provisioning is typically 30–90s
// but spike loads can stretch.
func (c *Client) WaitLoadBalancerEndpoint(ctx context.Context, namespace string, timeout time.Duration) (string, error) {
	if namespace == "" {
		namespace = Iperf3Namespace
	}
	if timeout == 0 {
		timeout = defaultLBTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		svc, err := c.clientset.CoreV1().Services(namespace).Get(ctx, Iperf3SvcName, metav1.GetOptions{})
		if err == nil {
			for _, ing := range svc.Status.LoadBalancer.Ingress {
				if ing.IP != "" {
					return ing.IP, nil
				}
				if ing.Hostname != "" {
					return ing.Hostname, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timeout waiting for LoadBalancer endpoint on %s/%s", namespace, Iperf3SvcName)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// ClusterIPEndpoint returns the iperf3 Service's ClusterIP. For
// east-west tests where the client is also a Pod, this is the address
// to point iperf3 -c at.
func (c *Client) ClusterIPEndpoint(ctx context.Context, namespace string) (string, error) {
	if namespace == "" {
		namespace = Iperf3Namespace
	}
	svc, err := c.clientset.CoreV1().Services(namespace).Get(ctx, Iperf3SvcName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading service %s/%s: %w", namespace, Iperf3SvcName, err)
	}
	if svc.Spec.ClusterIP == "" {
		return "", fmt.Errorf("service %s/%s has no ClusterIP", namespace, Iperf3SvcName)
	}
	return svc.Spec.ClusterIP, nil
}

// TeardownIperf3 deletes the pod + service. Best-effort: errors on
// individual deletes are returned but each step still attempts.
func (c *Client) TeardownIperf3(ctx context.Context, namespace string) error {
	if namespace == "" {
		namespace = Iperf3Namespace
	}
	var firstErr error
	if err := c.clientset.CoreV1().Services(namespace).Delete(ctx, Iperf3SvcName, metav1.DeleteOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			firstErr = fmt.Errorf("deleting service: %w", err)
		}
	}
	if err := c.clientset.CoreV1().Pods(namespace).Delete(ctx, Iperf3PodName, metav1.DeleteOptions{}); err != nil {
		if !apierrors.IsNotFound(err) && firstErr == nil {
			firstErr = fmt.Errorf("deleting pod: %w", err)
		}
	}
	return firstErr
}
