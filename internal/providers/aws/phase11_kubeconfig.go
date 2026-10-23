package aws

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// NodeReadyPollInterval is the polling interval when waiting for all worker nodes to report Ready.
var NodeReadyPollInterval = 10 * time.Second

// NodeReadyPollTimeout is the maximum time to wait for all worker nodes to report Ready.
var NodeReadyPollTimeout = 10 * time.Minute

func isNodeReady(n *corev1.Node) bool {
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func waitForNodesReady(ctx context.Context, k kubernetes.Interface, minReady int) ([]string, error) {
	if minReady < 1 {
		minReady = 1
	}
	deadline := time.Now().Add(NodeReadyPollTimeout)
	for {
		list, err := k.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err == nil {
			var readyNames []string
			for i := range list.Items {
				if isNodeReady(&list.Items[i]) {
					readyNames = append(readyNames, list.Items[i].Name)
				}
			}
			if len(readyNames) >= minReady {
				return readyNames, nil
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return nil, fmt.Errorf("timeout waiting for %d Ready node(s): last error: %w", minReady, err)
			}
			return nil, fmt.Errorf("timeout waiting for %d Ready node(s)", minReady)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(NodeReadyPollInterval):
		}
	}
}

// Phase11Kubeconfig writes the cluster kubeconfig to the workspace, attaches in-process client-go clients, and waits for worker nodes to report Ready.
func Phase11Kubeconfig(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 11 kubeconfig: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 11 kubeconfig: state is nil")
	}
	if dryRun {
		return nil
	}
	if c == nil {
		return fmt.Errorf("phase 11 kubeconfig: clients bundle is nil")
	}

	clusterName := st.Get("EKS_CLUSTER_NAME")
	if clusterName == "" {
		clusterName = cl.Metadata.Name
	}
	endpoint := st.Get("EKS_CLUSTER_ENDPOINT")
	caData := st.Get("EKS_CLUSTER_CA")
	if endpoint == "" || caData == "" {
		return fmt.Errorf("phase 11 kubeconfig: EKS_CLUSTER_ENDPOINT / EKS_CLUSTER_CA missing from state")
	}

	kcPath := st.KubeconfigPath()
	kcData := KubeconfigTemplateData{
		ClusterName: clusterName,
		Endpoint:    endpoint,
		CAData:      caData,
		Region:      cl.Metadata.Region,
	}
	if err := WriteKubeconfig(kcPath, kcData); err != nil {
		return fmt.Errorf("phase 11 kubeconfig: %w", err)
	}

	if !testing.Testing() {
		if globalPath, gErr := BackupAndInstallGlobalKubeconfig(kcData); gErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not update global kubeconfig: %v\n", gErr)
		} else {
			st.Set("GLOBAL_KUBECONFIG_PATH", globalPath)
		}
	}

	if c.K8s == nil {
		if err := c.AttachK8s(kcPath); err != nil {
			return fmt.Errorf("phase 11 kubeconfig: %w", err)
		}
	}

	minNodes := cl.TotalDesiredNodes()
	readyNodes, err := waitForNodesReady(ctx, c.K8s, minNodes)
	if err != nil {
		return fmt.Errorf("phase 11 kubeconfig: %w", err)
	}

	st.Set("KUBECONFIG", kcPath)
	st.Set("KUBECONFIG_PATH", kcPath)
	if len(readyNodes) > 0 {
		st.Set("TMM_NODE_NAME", readyNodes[0])
	}
	return st.Save()
}

// Phase11KubeconfigDown removes the workspace kubeconfig file and clears the related state keys.
func Phase11KubeconfigDown(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if st == nil || dryRun {
		return nil
	}
	for _, p := range []string{
		st.Get("KUBECONFIG_PATH"),
		st.Get("KUBECONFIG"),
		st.KubeconfigPath(),
	} {
		if p != "" {
			_ = os.Remove(p)
		}
	}
	if st.Dir() != "" {
		_ = os.Remove(filepath.Join(st.Dir(), "kubeconfig"))
	}
	if !testing.Testing() {
		if restored, rErr := RestoreGlobalKubeconfig(); rErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not restore global kubeconfig backup: %v\n", rErr)
		} else if restored {
			fmt.Println("Restored ~/.kube/config from pre-up backup")
		}
	}
	st.Delete("KUBECONFIG")
	st.Delete("KUBECONFIG_PATH")
	st.Delete("GLOBAL_KUBECONFIG_PATH")
	st.Delete("TMM_NODE_NAME")
	return st.Save()
}
