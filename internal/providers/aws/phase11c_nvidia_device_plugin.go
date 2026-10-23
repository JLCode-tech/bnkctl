package aws

import (
	"context"
	"fmt"
	"time"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/manifests"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/render"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	nvidiaDevicePluginNamespace     = "kube-system"
	nvidiaDevicePluginDaemonSetName = "nvidia-device-plugin-daemonset"
)

// NvidiaPluginPollInterval is the polling interval when waiting for nvidia.com/gpu to appear on GPU nodes.
var NvidiaPluginPollInterval = 10 * time.Second

// NvidiaPluginPollTimeout is the maximum time to wait for nvidia.com/gpu to become allocatable on GPU nodes.
var NvidiaPluginPollTimeout = 5 * time.Minute

func waitForAllocatableGPUs(ctx context.Context, k kubernetes.Interface, minGPUNodes int) (int, error) {
	if minGPUNodes < 1 {
		minGPUNodes = 1
	}
	deadline := time.Now().Add(NvidiaPluginPollTimeout)
	for {
		list, err := k.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err == nil {
			gpuNodes := 0
			totalGPUs := 0
			for i := range list.Items {
				q, ok := list.Items[i].Status.Allocatable["nvidia.com/gpu"]
				if ok && !q.IsZero() {
					if n, convOK := q.AsInt64(); convOK && n > 0 {
						gpuNodes++
						totalGPUs += int(n)
					}
				}
			}
			if gpuNodes >= minGPUNodes {
				return totalGPUs, nil
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return 0, fmt.Errorf("timeout waiting for %d GPU node(s) with allocatable nvidia.com/gpu: last error: %w", minGPUNodes, err)
			}
			return 0, fmt.Errorf("timeout waiting for %d GPU node(s) with allocatable nvidia.com/gpu", minGPUNodes)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(NvidiaPluginPollInterval):
		}
	}
}

// Phase11cNvidiaDevicePlugin deploys the NVIDIA k8s-device-plugin DaemonSet when GPU node groups are present.
func Phase11cNvidiaDevicePlugin(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 11c nvidia-device-plugin: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 11c nvidia-device-plugin: state is nil")
	}
	if !cl.NeedsNvidiaDevicePlugin() || dryRun {
		return nil
	}
	if c == nil || c.Dynamic == nil || c.RESTMapper == nil || c.K8s == nil {
		return fmt.Errorf("phase 11c nvidia-device-plugin: Kubernetes clients not attached")
	}

	tmpl, err := manifests.FS.ReadFile("nvidia-device-plugin/nvidia-device-plugin-v0.17.1.yaml.tmpl")
	if err != nil {
		return fmt.Errorf("phase 11c nvidia-device-plugin: reading template: %w", err)
	}
	raw, err := render.RenderNvidiaDevicePlugin(tmpl, cl.NvidiaDevicePluginVersion())
	if err != nil {
		return fmt.Errorf("phase 11c nvidia-device-plugin: rendering template: %w", err)
	}
	if err := applyRawYAML(ctx, c, raw, nvidiaDevicePluginNamespace); err != nil {
		return fmt.Errorf("phase 11c nvidia-device-plugin: applying DaemonSet: %w", err)
	}

	totalGPUs, err := waitForAllocatableGPUs(ctx, c.K8s, cl.DesiredGPUNodeCount())
	if err != nil {
		return fmt.Errorf("phase 11c nvidia-device-plugin: %w", err)
	}

	st.Set("NVIDIA_DEVICE_PLUGIN_VERSION", cl.NvidiaDevicePluginVersion())
	st.Set("NVIDIA_GPU_ALLOCATABLE", fmt.Sprintf("%d", totalGPUs))
	return st.Save()
}

// Phase11cNvidiaDevicePluginDown deletes the NVIDIA device-plugin DaemonSet when it was installed.
func Phase11cNvidiaDevicePluginDown(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if st == nil || dryRun {
		return nil
	}
	if st.Get("NVIDIA_DEVICE_PLUGIN_VERSION") == "" {
		return nil
	}
	if c != nil && c.K8s != nil {
		_ = c.K8s.AppsV1().DaemonSets(nvidiaDevicePluginNamespace).Delete(ctx, nvidiaDevicePluginDaemonSetName, metav1.DeleteOptions{})
	}
	st.Delete("NVIDIA_DEVICE_PLUGIN_VERSION")
	st.Delete("NVIDIA_GPU_ALLOCATABLE")
	st.Delete("NVIDIA_PLUGIN_INSTALLED_AT")
	return st.Save()
}
