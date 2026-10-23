package aws

import (
	"context"
	"fmt"

	"github.com/JLCode-tech/bnkctl/internal/providers/aws/awsmw"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

const metricsServerAddonName = "metrics-server"

// Phase08cMetricsServer installs (or updates) the metrics-server EKS managed addon.
func Phase08cMetricsServer(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 08c metrics-server: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 08c metrics-server: state is nil")
	}
	if dryRun {
		return nil
	}
	if c == nil || c.PhaseEKS == nil {
		return fmt.Errorf("phase 08c metrics-server: EKS client is nil")
	}

	clusterName := st.Get("EKS_CLUSTER_NAME")
	if clusterName == "" {
		clusterName = cl.Metadata.Name
	}
	if clusterName == "" {
		return fmt.Errorf("phase 08c metrics-server: cluster name is empty")
	}

	_, err := c.PhaseEKS.DescribeAddon(ctx, &eks.DescribeAddonInput{
		ClusterName: aws.String(clusterName),
		AddonName:   aws.String(metricsServerAddonName),
	})
	switch {
	case err == nil:
		if _, uerr := c.PhaseEKS.UpdateAddon(ctx, &eks.UpdateAddonInput{
			ClusterName:      aws.String(clusterName),
			AddonName:        aws.String(metricsServerAddonName),
			ResolveConflicts: ekstypes.ResolveConflictsOverwrite,
		}); uerr != nil {
			return fmt.Errorf("phase 08c metrics-server: UpdateAddon %s/%s: %w", clusterName, metricsServerAddonName, uerr)
		}
	case awsmw.IsNotFound(err):
		if _, cerr := c.PhaseEKS.CreateAddon(ctx, &eks.CreateAddonInput{
			ClusterName:      aws.String(clusterName),
			AddonName:        aws.String(metricsServerAddonName),
			ResolveConflicts: ekstypes.ResolveConflictsOverwrite,
		}); cerr != nil {
			return fmt.Errorf("phase 08c metrics-server: CreateAddon %s/%s: %w", clusterName, metricsServerAddonName, cerr)
		}
	default:
		return fmt.Errorf("phase 08c metrics-server: DescribeAddon %s/%s: %w", clusterName, metricsServerAddonName, err)
	}

	if err := waitForAddonActive(ctx, c, clusterName, metricsServerAddonName); err != nil {
		return fmt.Errorf("phase 08c metrics-server: %w", err)
	}

	st.Set("METRICS_SERVER_INSTALLED", "true")
	return st.Save()
}

// Phase08cMetricsServerDown clears the METRICS_SERVER_INSTALLED and METRICS_SERVER_ADDON state keys.
func Phase08cMetricsServerDown(_ context.Context, _ *intent.Cluster, st *state.State, _ *Clients, dryRun bool) error {
	if st == nil || dryRun {
		return nil
	}
	st.Delete("METRICS_SERVER_INSTALLED")
	st.Delete("METRICS_SERVER_ADDON")
	return st.Save()
}
