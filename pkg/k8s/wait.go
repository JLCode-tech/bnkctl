package k8s

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const waitPollInterval = 5 * time.Second

// WaitForDeploymentReady polls apps/v1 Deployment in ns/name until AvailableReplicas == Replicas (> 0).
func WaitForDeploymentReady(ctx context.Context, clientset kubernetes.Interface, ns, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return wait.PollUntilContextTimeout(ctx, waitPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		d, err := clientset.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil //nolint:nilerr
		}
		desired := d.Status.Replicas
		available := d.Status.AvailableReplicas
		return desired > 0 && available == desired, nil
	})
}

// WaitForDaemonSetReady polls apps/v1 DaemonSet in ns/name until NumberReady == DesiredNumberScheduled (> 0).
func WaitForDaemonSetReady(ctx context.Context, clientset kubernetes.Interface, ns, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return wait.PollUntilContextTimeout(ctx, waitPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		ds, err := clientset.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil //nolint:nilerr
		}
		desired := ds.Status.DesiredNumberScheduled
		ready := ds.Status.NumberReady
		return desired > 0 && ready == desired, nil
	})
}

// CertificateGVR is the GroupVersionResource for cert-manager Certificate CRs.
var CertificateGVR = schema.GroupVersionResource{
	Group:    "cert-manager.io",
	Version:  "v1",
	Resource: "certificates",
}

// WaitForCertificateReady polls cert-manager Certificate until Ready condition is True.
func WaitForCertificateReady(ctx context.Context, dyn dynamic.Interface, ns, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return wait.PollUntilContextTimeout(ctx, waitPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		obj, err := dyn.Resource(CertificateGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil //nolint:nilerr
		}
		return IsCertificateReady(obj.Object), nil
	})
}

// IsCertificateReady checks if condition Ready is True.
func IsCertificateReady(obj map[string]interface{}) bool {
	return UnstructuredConditionIsTrue(obj, "Ready")
}

// UnstructuredConditionIsTrue checks if status.conditions[] contains condType with status True.
func UnstructuredConditionIsTrue(obj map[string]interface{}, condType string) bool {
	status, ok := obj["status"].(map[string]interface{})
	if !ok {
		return false
	}
	conditions, ok := status["conditions"].([]interface{})
	if !ok {
		return false
	}
	for _, raw := range conditions {
		cond, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := cond["type"].(string); t != condType {
			continue
		}
		s, _ := cond["status"].(string)
		return s == "True"
	}
	return false
}

// CRDGVR is the GroupVersionResource for CustomResourceDefinition.
var CRDGVR = schema.GroupVersionResource{
	Group:    "apiextensions.k8s.io",
	Version:  "v1",
	Resource: "customresourcedefinitions",
}

// WaitForCRDEstablished polls until CRD is Established.
func WaitForCRDEstablished(ctx context.Context, dyn dynamic.Interface, crdName string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return wait.PollUntilContextTimeout(ctx, waitPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		obj, err := dyn.Resource(CRDGVR).Get(ctx, crdName, metav1.GetOptions{})
		if err != nil {
			return false, nil //nolint:nilerr
		}
		return isCRDEstablished(obj.Object), nil
	})
}

func isCRDEstablished(obj map[string]interface{}) bool {
	return UnstructuredConditionIsTrue(obj, "Established")
}

// WaitForCRDExists polls CustomResourceDefinition until it is visible in the API server.
func WaitForCRDExists(ctx context.Context, dyn dynamic.Interface, crdName string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return wait.PollUntilContextTimeout(ctx, waitPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		_, err := dyn.Resource(CRDGVR).Get(ctx, crdName, metav1.GetOptions{})
		if err != nil {
			return false, nil //nolint:nilerr
		}
		return true, nil
	})
}

// WaitForNodeHugepagesCapacity polls the named Node until its
// .status.capacity["hugepages-2Mi"] is >= want.
func WaitForNodeHugepagesCapacity(ctx context.Context, clientset kubernetes.Interface, nodeName, want string, timeout time.Duration) error {
	wantQty, err := resource.ParseQuantity(want)
	if err != nil {
		return fmt.Errorf("parse hugepages quantity %q: %w", want, err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return wait.PollUntilContextTimeout(ctx, 10*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false, nil //nolint:nilerr
		}
		got, ok := node.Status.Capacity["hugepages-2Mi"]
		if !ok {
			return false, nil
		}
		return got.Cmp(wantQty) >= 0, nil
	})
}

// WaitForPodReady polls a Pod until ready.
func WaitForPodReady(ctx context.Context, clientset kubernetes.Interface, ns, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return wait.PollUntilContextTimeout(ctx, waitPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		pod, err := clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil //nolint:nilerr
		}
		if pod.Status.Phase == corev1.PodFailed {
			return false, fmt.Errorf("pod %s/%s entered Failed phase", ns, name)
		}
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
}

// WaitForUnstructuredCondition polls an unstructured resource for a jsonPath matching expectedValue.
func WaitForUnstructuredCondition(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, namespace, name, jsonPath, expectedValue string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	fields := splitDotPath(jsonPath)
	var lastSeen string
	err := wait.PollUntilContextTimeout(ctx, waitPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		obj, err := dyn.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil //nolint:nilerr
		}
		val, _, _ := unstructured.NestedString(obj.Object, fields...)
		lastSeen = val
		return val == expectedValue, nil
	})
	if err != nil {
		return fmt.Errorf("WaitForUnstructuredCondition: %s/%s %s: timeout waiting for %q, last seen %q: %w",
			namespace, name, jsonPath, expectedValue, lastSeen, err)
	}
	return nil
}

func splitDotPath(path string) []string {
	if path == "" {
		return nil
	}
	var parts []string
	start := 0
	for i := 0; i < len(path); i++ {
		if path[i] == '.' {
			if i > start {
				parts = append(parts, path[start:i])
			}
			start = i + 1
		}
	}
	if start < len(path) {
		parts = append(parts, path[start:])
	}
	return parts
}

// DeploymentReplicaStatus returns (available, desired, error) for a Deployment.
func DeploymentReplicaStatus(ctx context.Context, clientset kubernetes.Interface, ns, name string) (available, desired int32, err error) {
	d, err := clientset.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, 0, fmt.Errorf("get deployment %s/%s: %w", ns, name, err)
	}
	return d.Status.AvailableReplicas, d.Status.Replicas, nil
}

// WaitForDaemonSetRollout polls apps/v1 DaemonSet in ns/name until the
// controller has observed the latest template generation and every scheduled
// pod is updated, ready and available.
func WaitForDaemonSetRollout(ctx context.Context, clientset kubernetes.Interface, ns, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return wait.PollUntilContextTimeout(ctx, waitPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		ds, err := clientset.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil //nolint:nilerr
		}
		st := ds.Status
		if st.ObservedGeneration < ds.Generation || st.DesiredNumberScheduled == 0 {
			return false, nil
		}
		return st.UpdatedNumberScheduled == st.DesiredNumberScheduled &&
			st.NumberReady == st.DesiredNumberScheduled &&
			st.NumberAvailable == st.DesiredNumberScheduled, nil
	})
}

// WaitForNamespaceGone polls until namespace ns no longer exists (for cleanup).
func WaitForNamespaceGone(ctx context.Context, clientset kubernetes.Interface, ns string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return wait.PollUntilContextTimeout(ctx, waitPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		_, err := clientset.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
		if err != nil {
			return true, nil
		}
		return false, nil
	})
}

// WaitForPodSucceeded polls a Pod in ns/name until its Phase is Succeeded.
func WaitForPodSucceeded(ctx context.Context, clientset kubernetes.Interface, ns, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return wait.PollUntilContextTimeout(ctx, waitPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		pod, err := clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil //nolint:nilerr
		}
		switch pod.Status.Phase {
		case corev1.PodSucceeded:
			return true, nil
		case corev1.PodFailed:
			return false, fmt.Errorf("pod %s/%s entered Failed phase", ns, name)
		}
		return false, nil
	})
}
