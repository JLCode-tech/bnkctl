package aws

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/manifests"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/render"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	ifaceDiscoveryPodName   = "iface-discovery"
	ifaceDiscoveryNamespace = "kube-system"
)

// IfaceDiscoveryPollInterval is the polling interval when waiting for the bnk-iface-discovery pod to finish.
var IfaceDiscoveryPollInterval = 2 * time.Second

// IfaceDiscoveryPollTimeout is the maximum time to wait for the bnk-iface-discovery pod to succeed.
var IfaceDiscoveryPollTimeout = 2 * time.Minute

func waitForPodSucceeded(ctx context.Context, k kubernetes.Interface, ns, name string) error {
	deadline := time.Now().Add(IfaceDiscoveryPollTimeout)
	for {
		pod, err := k.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get pod %s/%s: %w", ns, name, err)
		}
		switch pod.Status.Phase {
		case corev1.PodSucceeded:
			return nil
		case corev1.PodFailed:
			return fmt.Errorf("pod %s/%s entered Failed phase (%s)", ns, name, pod.Status.Message)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for pod %s/%s to succeed (current: %s)", ns, name, pod.Status.Phase)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(IfaceDiscoveryPollInterval):
		}
	}
}

func readPodLogs(ctx context.Context, k kubernetes.Interface, ns, name string) (string, error) {
	req := k.CoreV1().Pods(ns).GetLogs(name, &corev1.PodLogOptions{})
	rc, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("streaming logs from %s/%s: %w", ns, name, err)
	}
	defer func() { _ = rc.Close() }()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, rc); err != nil {
		return "", fmt.Errorf("reading logs from %s/%s: %w", ns, name, err)
	}
	return buf.String(), nil
}

// PodLogReaderFunc allows unit tests to inject fake pod log output.
var PodLogReaderFunc = readPodLogs

// Phase17cIfaceDiscovery runs the bnk-iface-discovery hostNetwork pod on the TMM node, correlates MACs, and re-tags the external ENI.
func Phase17cIfaceDiscovery(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 17c iface discovery: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 17c iface discovery: state is nil")
	}
	if !cl.HasExternalDataPath() || dryRun {
		return nil
	}
	if c == nil || c.K8s == nil || c.Dynamic == nil || c.RESTMapper == nil {
		return fmt.Errorf("phase 17c iface discovery: Kubernetes clients are nil")
	}

	nodeName := st.Get("TMM_NODE_NAME")
	if nodeName == "" {
		return fmt.Errorf("phase 17c iface discovery: TMM_NODE_NAME not found in state")
	}
	extMAC := st.Get("EXT_ENI_MAC")
	if extMAC == "" {
		return fmt.Errorf("phase 17c iface discovery: EXT_ENI_MAC not found in state")
	}
	primaryMAC := st.Get("PRIMARY_ENI_MAC")
	intMAC := st.Get("INT_ENI_MAC")

	_ = c.K8s.CoreV1().Pods(ifaceDiscoveryNamespace).Delete(ctx, ifaceDiscoveryPodName, metav1.DeleteOptions{})

	tmplBytes, err := manifests.FS.ReadFile("host-device/iface-discovery-pod.yaml.tmpl")
	if err != nil {
		return fmt.Errorf("phase 17c iface discovery: reading pod template: %w", err)
	}
	rendered, err := render.RenderIfaceDiscoveryPod(tmplBytes, ifaceDiscoveryNamespace, nodeName)
	if err != nil {
		return fmt.Errorf("phase 17c iface discovery: rendering pod template: %w", err)
	}
	if err := applyRawYAML(ctx, c, rendered, ifaceDiscoveryNamespace); err != nil {
		return fmt.Errorf("phase 17c iface discovery: creating discovery pod: %w", err)
	}
	defer func() {
		_ = c.K8s.CoreV1().Pods(ifaceDiscoveryNamespace).Delete(ctx, ifaceDiscoveryPodName, metav1.DeleteOptions{})
	}()

	if err := waitForPodSucceeded(ctx, c.K8s, ifaceDiscoveryNamespace, ifaceDiscoveryPodName); err != nil {
		return fmt.Errorf("phase 17c iface discovery: %w", err)
	}

	logs, err := PodLogReaderFunc(ctx, c.K8s, ifaceDiscoveryNamespace, ifaceDiscoveryPodName)
	if err != nil {
		return fmt.Errorf("phase 17c iface discovery: %w", err)
	}

	disc, err := matchInterfaces(logs, primaryMAC, extMAC, intMAC, cl.HasInternalInterface())
	if err != nil {
		cachedExtIf := st.Get("EXT_IFNAME")
		cachedExtPCI := st.Get("EXT_PCI_BDF")
		cachedIntIf := st.Get("INT_IFNAME")
		cachedIntPCI := st.Get("INT_PCI_BDF")
		hasCached := cachedExtIf != "" && cachedExtPCI != "" && (!cl.HasInternalInterface() || (cachedIntIf != "" && cachedIntPCI != ""))
		if !hasCached {
			return fmt.Errorf("phase 17c iface discovery: %w (raw output: %q)", err, strings.TrimSpace(logs))
		}
		disc = DiscoveredInterfaces{
			PrimaryIfName: st.Get("NODE_PRIMARY_IFNAME"),
			ExtIfName:     cachedExtIf,
			ExtPCIBDF:     cachedExtPCI,
			IntIfName:     cachedIntIf,
			IntPCIBDF:     cachedIntPCI,
		}
	}

	if disc.PrimaryIfName != "" {
		st.Set("NODE_PRIMARY_IFNAME", disc.PrimaryIfName)
	}
	st.Set("EXTERNAL_IFNAME", disc.ExtIfName)
	st.Set("EXT_IFNAME", disc.ExtIfName)
	st.Set("EXTERNAL_PCI", disc.ExtPCIBDF)
	st.Set("EXT_PCI_BDF", disc.ExtPCIBDF)
	st.Set("CLOUD_HOST_DEVICE_NAME", disc.ExtIfName)
	if cl.HasInternalInterface() {
		st.Set("INTERNAL_IFNAME", disc.IntIfName)
		st.Set("INT_IFNAME", disc.IntIfName)
		st.Set("INTERNAL_PCI", disc.IntPCIBDF)
		st.Set("INT_PCI_BDF", disc.IntPCIBDF)
	}

	if extENIID := st.Get("EXT_ENI_ID"); extENIID != "" && c.PhaseEC2 != nil && disc.ExtIfName != "" {
		if _, err := c.PhaseEC2.CreateTags(ctx, &ec2.CreateTagsInput{
			Resources: []string{extENIID},
			Tags: []ec2types.Tag{
				{
					Key:   aws.String(hostDeviceTagKey),
					Value: aws.String(disc.ExtIfName),
				},
			},
		}); err != nil {
			return fmt.Errorf("phase 17c iface discovery: CreateTags %s=%s on %s: %w", hostDeviceTagKey, disc.ExtIfName, extENIID, err)
		}
	}

	return st.Save()
}

// Phase17cIfaceDiscoveryDown deletes any leftover bnk-iface-discovery pod and clears discovered interface state keys.
func Phase17cIfaceDiscoveryDown(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if st == nil || dryRun {
		return nil
	}
	if c != nil && c.K8s != nil {
		_ = c.K8s.CoreV1().Pods(ifaceDiscoveryNamespace).Delete(ctx, ifaceDiscoveryPodName, metav1.DeleteOptions{})
	}
	for _, k := range []string{
		"NODE_PRIMARY_IFNAME",
		"EXTERNAL_IFNAME",
		"EXT_IFNAME",
		"EXTERNAL_PCI",
		"EXT_PCI_BDF",
		"INTERNAL_IFNAME",
		"INT_IFNAME",
		"INTERNAL_PCI",
		"INT_PCI_BDF",
		"CLOUD_HOST_DEVICE_NAME",
		"CLOUD_HOST_DEVICE_TAG",
		"IFACE_DISCOVERY_AT",
		"EXTERNAL_NAD",
		"INTERNAL_NAD",
		"TMM_EXT_SELFIP",
		"TMM_INT_SELFIP",
		"TMM_SELFIP_PREFIXLEN",
	} {
		st.Delete(k)
	}
	return st.Save()
}
