package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// PortForwardOptions captures the flag-parsed inputs to
// `awsbnkctl k port-forward`.
//
// Ports is the same slice form as kubectl ("8080:80", "5000",
// "5000:5000"). StopCh closes the tunnel; cancelling the context also
// closes it. ReadyCh receives a single empty struct when the tunnel
// is wired up and accepting connections; useful for tests / orchestration.
type PortForwardOptions struct {
	PodName        string
	Namespace      string
	Ports          []string
	KubeconfigPath string

	IOStreams genericiooptions.IOStreams

	// StopCh + ReadyCh: optional. If StopCh is nil, the helper
	// allocates one and closes it on context cancel.
	StopCh  chan struct{}
	ReadyCh chan struct{}
}

// Run opens a port-forward tunnel against the pod and blocks until
// either the context is cancelled or the tunnel errors out.
func (o *PortForwardOptions) Run(ctx context.Context) error {
	if o.PodName == "" {
		return errors.New("pod name required")
	}
	if len(o.Ports) == 0 {
		return errors.New("at least one <local>:<remote> port mapping required")
	}
	if o.IOStreams.Out == nil {
		o.IOStreams.Out = io.Discard
	}
	if o.IOStreams.ErrOut == nil {
		o.IOStreams.ErrOut = io.Discard
	}

	cfg, err := BuildRESTConfig(o.KubeconfigPath)
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	ns := o.Namespace
	if ns == "" {
		ns = "default"
	}
	podName, ports, err := ResolvePortForwardTarget(ctx, cs, ns, o.PodName, o.Ports)
	if err != nil {
		return err
	}
	if podName != o.PodName {
		fmt.Fprintf(o.IOStreams.ErrOut, "Forwarding to pod %s/%s for %s\n", ns, podName, o.PodName)
	}

	roundTripper, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		return fmt.Errorf("setting up SPDY round-tripper: %w", err)
	}

	req := cs.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(podName).
		Namespace(ns).
		SubResource("portforward")

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, "POST", req.URL())

	stopCh := o.StopCh
	if stopCh == nil {
		stopCh = make(chan struct{})
	}
	readyCh := o.ReadyCh
	if readyCh == nil {
		readyCh = make(chan struct{})
	}

	// Cancel on ctx done — the cobra root wires SIGINT into ctx so
	// Ctrl+C closes the tunnel cleanly.
	go func() {
		<-ctx.Done()
		select {
		case <-stopCh:
		default:
			close(stopCh)
		}
	}()

	fwd, err := portforward.New(dialer, ports, stopCh, readyCh, o.IOStreams.Out, o.IOStreams.ErrOut)
	if err != nil {
		return fmt.Errorf("creating port forwarder: %w", err)
	}
	return fwd.ForwardPorts()
}

// ResolvePortForwardTarget turns a kubectl-style target into the pod the
// tunnel must dial. "pod/<name>" and a bare name are the pod itself;
// "svc/<name>" or "service/<name>" picks a Ready pod behind the Service and
// rewrites each remote port that names a Service port to that port's numeric
// targetPort, as kubectl port-forward does.
func ResolvePortForwardTarget(ctx context.Context, cs kubernetes.Interface, ns, target string, ports []string) (string, []string, error) {
	switch {
	case strings.HasPrefix(target, "pod/"):
		return strings.TrimPrefix(target, "pod/"), ports, nil
	case strings.HasPrefix(target, "svc/"), strings.HasPrefix(target, "service/"):
	default:
		return target, ports, nil
	}
	name := target[strings.Index(target, "/")+1:]
	svc, err := cs.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", nil, fmt.Errorf("service %s/%s: %w", ns, name, err)
	}
	if len(svc.Spec.Selector) == 0 {
		return "", nil, fmt.Errorf("service %s/%s has no selector; port-forward needs a pod-backed Service", ns, name)
	}
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(svc.Spec.Selector).String()})
	if err != nil {
		return "", nil, fmt.Errorf("list pods for service %s/%s: %w", ns, name, err)
	}
	var pick *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				pick = p
			}
		}
		if pick != nil {
			break
		}
	}
	if pick == nil {
		return "", nil, fmt.Errorf("service %s/%s has no Ready pod", ns, name)
	}
	mapped := make([]string, len(ports))
	for i, spec := range ports {
		local, remote := spec, spec
		if j := strings.Index(spec, ":"); j >= 0 {
			local, remote = spec[:j], spec[j+1:]
		}
		for _, sp := range svc.Spec.Ports {
			if strconv.Itoa(int(sp.Port)) == remote || sp.Name == remote {
				if tp := sp.TargetPort; tp.Type == intstr.Int && tp.IntVal > 0 {
					remote = strconv.Itoa(int(tp.IntVal))
				} else if tp.Type == intstr.String && tp.StrVal != "" {
					remote = containerPortByName(pick, tp.StrVal, remote)
				}
				break
			}
		}
		if strings.Contains(spec, ":") {
			mapped[i] = local + ":" + remote
		} else {
			mapped[i] = spec + ":" + remote
		}
	}
	return pick.Name, mapped, nil
}

// containerPortByName resolves a named container port on pod; fallback when
// absent.
func containerPortByName(pod *corev1.Pod, name, fallback string) string {
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.Name == name {
				return strconv.Itoa(int(p.ContainerPort))
			}
		}
	}
	return fallback
}
