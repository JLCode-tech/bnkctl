package jumphost

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

// Runner executes commands and probes on a remote jumphost or in-cluster probe pod.
type Runner interface {
	Run(ctx context.Context, cmd string, stdin io.Reader) (string, error)
}

// SSHRunner implements Runner over SSH (direct, AWS EICE, Azure Bastion, or GCP IAP).
type SSHRunner struct {
	Opts ProbeOptions
}

// Run executes cmd over SSH on the configured jumphost.
func (r *SSHRunner) Run(ctx context.Context, cmd string, stdin io.Reader) (string, error) {
	keyPath, _, cleanup, err := prepareProbeKey(ctx, r.Opts)
	if err != nil {
		return "", err
	}
	defer cleanup()
	return SSHRunViaEICEWithStdin(ctx, r.Opts.EffectiveLocation(), r.Opts.EffectiveVMName(), keyPath, cmd, stdin)
}

// PodExecRunner implements Runner using in-process Kubernetes WebSocket/SPDY pod exec
// (no external kubectl binary dependency).
type PodExecRunner struct {
	Kubeconfig string
	Namespace  string
	PodName    string
	Container  string
}

// Run executes cmd inside the target pod via remotecommand WebSocket executor (with SPDY fallback).
func (r *PodExecRunner) Run(ctx context.Context, cmd string, stdin io.Reader) (string, error) {
	restCfg, clientset, err := loadKubeClient(r.Kubeconfig)
	if err != nil {
		return "", fmt.Errorf("probe-exec kube client: %w", err)
	}
	ns := r.Namespace
	if ns == "" {
		ns = probePodNamespace()
	}
	pod := r.PodName
	if pod == "" {
		pod = probePodName()
	}
	container := r.Container
	if container == "" {
		container = probePodContainer()
	}

	shellCmd := cmd
	if stdin != nil {
		shellCmd = cmd + "; status=$?; exit $status"
	}

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(ns).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   []string{"sh", "-c", shellCmd},
			Stdin:     stdin != nil,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	spdyExec, err := remotecommand.NewSPDYExecutor(restCfg, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("probe-exec spdy executor: %w", err)
	}
	exec := spdyExec
	if wsExec, wsErr := remotecommand.NewWebSocketExecutor(restCfg, "GET", req.URL().String()); wsErr == nil {
		if fbExec, fbErr := remotecommand.NewFallbackExecutor(wsExec, spdyExec, func(err error) bool {
			return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
		}); fbErr == nil {
			exec = fbExec
		}
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return strings.TrimSpace(stdout.String()), fmt.Errorf("probe-exec: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func resolveKubeconfigPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if env := os.Getenv("KUBECONFIG"); env != "" {
		return env
	}
	patterns := []string{
		"artifacts/kubeconfig.yaml",
		".bnkctl/*/kubeconfig",
		".awsbnkctl/*/kubeconfig",
		".azrbnkctl/*/kubeconfig",
		".gkebnkctl/*/kubeconfig",
		"workspaces/*/.bnkctl/*/kubeconfig",
		"workspaces/*/.awsbnkctl/*/kubeconfig",
		"workspaces/*/.azrbnkctl/*/kubeconfig",
		"workspaces/*/.gkebnkctl/*/kubeconfig",
	}
	for _, pat := range patterns {
		matches, _ := filepath.Glob(pat)
		if len(matches) == 0 {
			continue
		}
		best := matches[0]
		var bestMod time.Time
		if fi, err := os.Stat(best); err == nil {
			bestMod = fi.ModTime()
		}
		for _, m := range matches[1:] {
			if fi, err := os.Stat(m); err == nil && fi.ModTime().After(bestMod) {
				best = m
				bestMod = fi.ModTime()
			}
		}
		return best
	}
	return ""
}

func loadKubeClient(explicit string) (*rest.Config, *kubernetes.Clientset, error) {
	kcPath := resolveKubeconfigPath(explicit)
	if kcPath == "" {
		return nil, nil, fmt.Errorf("no kubeconfig found")
	}
	restCfg, err := clientcmd.BuildConfigFromFlags("", kcPath)
	if err != nil {
		return nil, nil, err
	}
	restCfg.Timeout = 15 * time.Second
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, nil, err
	}
	return restCfg, cs, nil
}

func isNamedPodRunningInProcess(pod, ns string) bool {
	_, cs, err := loadKubeClient("")
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := cs.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return p.Status.Phase == corev1.PodRunning
}

func findTMMProbePodInProcess() string {
	_, cs, err := loadKubeClient("")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, ns := range []string{"f5-cne-system", "f5-cne-core"} {
		list, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=f5-tmm"})
		if err != nil {
			continue
		}
		for _, p := range list.Items {
			if p.Status.Phase == corev1.PodRunning {
				return p.Name
			}
		}
	}
	return ""
}

func probePodIPInProcess(ctx context.Context, pod, ns string) string {
	_, cs, err := loadKubeClient("")
	if err != nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	p, err := cs.CoreV1().Pods(ns).Get(cctx, pod, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(p.Status.PodIP)
}
