package azure

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

// Phase23Kubeconfig retrieves the admin kubeconfig from AKS and writes it locally.
func Phase23Kubeconfig(ctx context.Context, cl *intent.Cluster, st *state.State, clients *Clients, dryRun bool) error {
	ensureAzureDefaults(cl)
	rgName := cl.Derived.ResourceGroupName
	clusterName := cl.Derived.AKSClusterName
	if st != nil {
		if v := st.Get("AZURE_RESOURCE_GROUP"); v != "" {
			rgName = v
		} else if v := st.Get("RESOURCE_GROUP"); v != "" {
			rgName = v
		}
		if v := st.Get("AZURE_AKS_CLUSTER_NAME"); v != "" {
			clusterName = v
		} else if v := st.Get("CLUSTER_NAME"); v != "" {
			clusterName = v
		}
	}

	fmt.Fprintf(os.Stderr, "[phase 23] kubeconfig: retrieving for cluster %s\n", clusterName)

	var kubeconfigPath string
	if st != nil {
		kubeconfigPath = st.KubeconfigPath()
	}
	if dryRun {
		if st != nil && kubeconfigPath != "" {
			st.Set("KUBECONFIG_PATH", kubeconfigPath)
			st.Set("KUBECONFIG", kubeconfigPath)
		}
		fmt.Fprintf(os.Stderr, "[phase 23] [dry-run] would save kubeconfig to %s\n", kubeconfigPath)
		return nil
	}

	if clients == nil || clients.ManagedClusters == nil {
		return nil
	}

	resp, err := clients.ManagedClusters.ListClusterAdminCredentials(ctx, rgName, clusterName, nil)
	if err != nil && isAzureNotFound(err) && !strings.HasPrefix(clusterName, "aks-") {
		altClusterName := "aks-" + clusterName
		if altResp, altErr := clients.ManagedClusters.ListClusterAdminCredentials(ctx, rgName, altClusterName, nil); altErr == nil {
			resp = altResp
			err = nil
			clusterName = altClusterName
		}
	}
	if err != nil {
		return fmt.Errorf("phase23: ListClusterAdminCredentials %s: %w", clusterName, err)
	}

	if len(resp.Kubeconfigs) == 0 || resp.Kubeconfigs[0].Value == nil {
		return fmt.Errorf("phase23: no kubeconfig returned for cluster %s", clusterName)
	}

	kubeconfigData := resp.Kubeconfigs[0].Value

	if kubeconfigPath != "" {
		if err := os.MkdirAll(filepath.Dir(kubeconfigPath), 0o700); err != nil {
			return fmt.Errorf("phase23: creating kubeconfig directory: %w", err)
		}

		if err := os.WriteFile(kubeconfigPath, kubeconfigData, 0o600); err != nil {
			return fmt.Errorf("phase23: writing kubeconfig to %s: %w", kubeconfigPath, err)
		}
	}

	if st != nil {
		if st.Dir() != "" {
			artifactsDir := filepath.Join(st.Dir(), "artifacts")
			_ = os.MkdirAll(artifactsDir, 0o750)
			_ = os.WriteFile(filepath.Join(artifactsDir, "kubeconfig"), kubeconfigData, 0o600)
		}
		if kubeconfigPath != "" {
			st.Set("KUBECONFIG_PATH", kubeconfigPath)
			st.Set("KUBECONFIG", kubeconfigPath)
		}
		if err := st.Save(); err != nil {
			return fmt.Errorf("phase23: saving state: %w", err)
		}
	}

	fmt.Fprintf(os.Stderr, "[phase 23] kubeconfig saved to %s\n", kubeconfigPath)
	return nil
}
