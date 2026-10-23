package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func extractEC2InstanceID(providerID string) string {
	idx := strings.LastIndex(providerID, "/")
	if idx >= 0 && idx+1 < len(providerID) {
		cand := providerID[idx+1:]
		if strings.HasPrefix(cand, "i-") {
			return cand
		}
	}
	if strings.HasPrefix(providerID, "i-") {
		return providerID
	}
	return ""
}

func selectTMMNode(nodes []corev1.Node, preferredAZ string) *corev1.Node {
	if len(nodes) == 0 {
		return nil
	}
	for i := range nodes {
		if nodes[i].Labels[TMMLabelKey] == TMMLabelValue {
			return &nodes[i]
		}
	}
	for i := range nodes {
		if nodes[i].Labels["bnk.f5.com/role"] == "tmm" {
			return &nodes[i]
		}
	}
	if preferredAZ != "" {
		for i := range nodes {
			if nodes[i].Labels["topology.kubernetes.io/zone"] == preferredAZ {
				return &nodes[i]
			}
		}
	}
	return &nodes[0]
}

// Phase16TMMNodeLabel selects the TMM worker node, labels it app=f5-tmm, and records TMM_NODE_NAME and TMM_INSTANCE_ID in state.
func Phase16TMMNodeLabel(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 16 tmm node label: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 16 tmm node label: state is nil")
	}
	if !cl.HasExternalDataPath() || dryRun {
		return nil
	}
	if c == nil || c.K8s == nil {
		return fmt.Errorf("phase 16 tmm node label: Kubernetes client is nil")
	}

	list, err := c.K8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("phase 16 tmm node label: listing nodes: %w", err)
	}
	if len(list.Items) == 0 {
		return fmt.Errorf("phase 16 tmm node label: cluster has 0 nodes")
	}

	var preferredAZ string
	if cl.Network.DataPath != nil {
		preferredAZ = cl.Network.DataPath.External.AZ
	}
	if preferredAZ == "" && cl.ClusterSpec != nil && len(cl.ClusterSpec.NodeGroups) > 0 {
		preferredAZ = cl.ClusterSpec.NodeGroups[0].AZ
	}

	node := selectTMMNode(list.Items, preferredAZ)
	if node == nil {
		return fmt.Errorf("phase 16 tmm node label: no candidate node found")
	}

	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": map[string]interface{}{
				TMMLabelKey: TMMLabelValue,
			},
		},
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("phase 16 tmm node label: marshaling label patch: %w", err)
	}
	if _, err := c.K8s.CoreV1().Nodes().Patch(ctx, node.Name, types.MergePatchType, raw, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("phase 16 tmm node label: patching node %s: %w", node.Name, err)
	}

	instanceID := extractEC2InstanceID(node.Spec.ProviderID)
	if instanceID == "" {
		return fmt.Errorf("phase 16 tmm node label: could not extract EC2 instance ID from node %s providerID %q", node.Name, node.Spec.ProviderID)
	}

	st.Set("TMM_NODE_NAME", node.Name)
	st.Set("TMM_INSTANCE_ID", instanceID)
	return st.Save()
}

// Phase16TMMNodeLabelDown removes the app=f5-tmm label from the TMM node.
func Phase16TMMNodeLabelDown(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if st == nil || dryRun {
		return nil
	}
	nodeName := st.Get("TMM_NODE_NAME")
	if nodeName != "" && c != nil && c.K8s != nil {
		patch := map[string]interface{}{
			"metadata": map[string]interface{}{
				"labels": map[string]interface{}{
					TMMLabelKey: nil,
				},
			},
		}
		raw, _ := json.Marshal(patch)
		_, _ = c.K8s.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, raw, metav1.PatchOptions{})
	}
	st.Delete("TMM_INSTANCE_ID")
	return st.Save()
}
