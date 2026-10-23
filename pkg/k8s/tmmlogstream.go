package k8s

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

// BNK 2.4 TMM log stream.
//
// The TMM pod's f5-fluentbit sidecar forwards every TMM log line (including
// the iRule `log local0.` records the governance pipeline consumes) to
// f5-toda-fluentd in the core namespace. EnableTMMLogStream turns that store on and
// bounces the fluentd pod.
const (
	// TMMLogNamespace is the CNE core namespace f5-toda-fluentd runs in.
	TMMLogNamespace = "f5-cne-core"
	// TMMLogDeployment is the fluentd Deployment.
	TMMLogDeployment = "f5-toda-fluentd"
	// TMMLogPodSelector selects the fluentd pod.
	TMMLogPodSelector = "app=toda-fluentd"
	// TMMLogContainer is the fluentd container whose stdout carries the stream.
	TMMLogContainer = "f5-fluentd"
	// TMMLogCustomConfigMap is F5's user-owned pipeline extension ConfigMap.
	TMMLogCustomConfigMap = "f5-toda-fluentd-custom"
	// TMMLogStdoutKey is the file match.conf includes; F5 ships it with the
	// store commented out.
	TMMLogStdoutKey = "stdout.conf"
	// TMMLogStdoutStore is the enabled store.
	TMMLogStdoutStore = "<store>\n  @type stdout\n</store>\n"
	// TMMLogPodMarker identifies TMM pod lines on the fluentd stdout stream.
	TMMLogPodMarker = `"pod_name":"f5-tmm"`
)

// TMMLogStreamEnabled reports whether the fluentd stdout store is on.
func TMMLogStreamEnabled(ctx context.Context, cs kubernetes.Interface) (bool, error) {
	cm, err := cs.CoreV1().ConfigMaps(TMMLogNamespace).Get(ctx, TMMLogCustomConfigMap, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			cm, err = cs.CoreV1().ConfigMaps("f5-cne-system").Get(ctx, TMMLogCustomConfigMap, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
		} else {
			return false, fmt.Errorf("get configmap %s/%s: %w", TMMLogNamespace, TMMLogCustomConfigMap, err)
		}
	}
	return stdoutStoreOn(cm.Data[TMMLogStdoutKey]), nil
}

func stdoutStoreOn(conf string) bool {
	for _, line := range strings.Split(conf, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "@type stdout") {
			return true
		}
	}
	return false
}

// listTMMPods lists pods matching selector in primaryNS, falling back to "f5-cne-system"
// when primaryNS is TMMLogNamespace ("f5-cne-core") and returns no pods.
func listTMMPods(ctx context.Context, cs kubernetes.Interface, primaryNS, selector string) (*corev1.PodList, string, error) {
	pods, err := cs.CoreV1().Pods(primaryNS).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, primaryNS, err
	}
	if len(pods.Items) == 0 && primaryNS != "f5-cne-system" {
		fallbackPods, fbErr := cs.CoreV1().Pods("f5-cne-system").List(ctx, metav1.ListOptions{LabelSelector: selector})
		if fbErr == nil && len(fallbackPods.Items) > 0 {
			return fallbackPods, "f5-cne-system", nil
		}
	}
	return pods, primaryNS, nil
}

// EnableTMMLogStream turns the stdout store on and bounces the fluentd pod so
// the new config is read.
func EnableTMMLogStream(ctx context.Context, cs kubernetes.Interface, timeout time.Duration) (changed bool, err error) {
	cm, err := cs.CoreV1().ConfigMaps(TMMLogNamespace).Get(ctx, TMMLogCustomConfigMap, metav1.GetOptions{})
	ns := TMMLogNamespace
	if err != nil {
		if apierrors.IsNotFound(err) {
			cm, err = cs.CoreV1().ConfigMaps("f5-cne-system").Get(ctx, TMMLogCustomConfigMap, metav1.GetOptions{})
			ns = "f5-cne-system"
			if err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}
				return false, fmt.Errorf("get configmap f5-cne-system/%s: %w", TMMLogCustomConfigMap, err)
			}
		} else {
			return false, fmt.Errorf("get configmap %s/%s: %w", TMMLogNamespace, TMMLogCustomConfigMap, err)
		}
	}
	if stdoutStoreOn(cm.Data[TMMLogStdoutKey]) {
		return false, nil
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[TMMLogStdoutKey] = TMMLogStdoutStore
	if _, err := cs.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return false, fmt.Errorf("update configmap %s/%s: %w", ns, TMMLogCustomConfigMap, err)
	}
	pods, podNS, err := listTMMPods(ctx, cs, ns, TMMLogPodSelector)
	if err != nil {
		return true, fmt.Errorf("list %s pods: %w", TMMLogPodSelector, err)
	}
	for i := range pods.Items {
		if err := cs.CoreV1().Pods(podNS).Delete(ctx, pods.Items[i].Name, metav1.DeleteOptions{}); err != nil {
			return true, fmt.Errorf("delete pod %s/%s: %w", podNS, pods.Items[i].Name, err)
		}
	}
	if timeout <= 0 {
		return true, nil
	}
	err = wait.PollUntilContextTimeout(ctx, 5*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		list, _, err := listTMMPods(ctx, cs, podNS, TMMLogPodSelector)
		if err != nil {
			return false, nil
		}
		for i := range list.Items {
			if list.Items[i].DeletionTimestamp == nil && podReady(&list.Items[i]) {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return true, fmt.Errorf("waiting for a Ready %s pod in %s: %w", TMMLogPodSelector, podNS, err)
	}
	return true, nil
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
