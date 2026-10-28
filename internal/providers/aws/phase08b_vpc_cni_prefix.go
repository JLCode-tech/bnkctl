package aws

import (
	"context"
	"fmt"
	"time"

	"github.com/JLCode-tech/bnkctl/internal/providers/aws/awsmw"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

const (
	vpcCNIAddonName        = "vpc-cni"
	vpcCNIPrefixConfigJSON = `{"env":{"ENABLE_PREFIX_DELEGATION":"true","WARM_PREFIX_TARGET":"1"}}`
)

// AddonPollInterval is the polling interval when waiting for an EKS addon to reach ACTIVE.
var AddonPollInterval = 5 * time.Second

// AddonPollTimeout is the maximum time to wait for an EKS addon to reach ACTIVE.
var AddonPollTimeout = 10 * time.Minute

func waitForAddonActive(ctx context.Context, c *Clients, clusterName, addonName string) error {
	deadline := time.Now().Add(AddonPollTimeout)
	for {
		out, err := c.PhaseEKS.DescribeAddon(ctx, &eks.DescribeAddonInput{
			ClusterName: aws.String(clusterName),
			AddonName:   aws.String(addonName),
		})
		if err != nil {
			return fmt.Errorf("DescribeAddon %s/%s: %w", clusterName, addonName, err)
		}
		if out.Addon == nil {
			return fmt.Errorf("DescribeAddon %s/%s returned nil Addon", clusterName, addonName)
		}
		switch out.Addon.Status {
		case ekstypes.AddonStatusActive:
			return nil
		case ekstypes.AddonStatusCreateFailed, ekstypes.AddonStatusUpdateFailed, ekstypes.AddonStatusDegraded:
			return fmt.Errorf("EKS addon %s/%s entered %s status", clusterName, addonName, out.Addon.Status)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for EKS addon %s/%s to become ACTIVE (current: %s)", clusterName, addonName, out.Addon.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(AddonPollInterval):
		}
	}
}

// Phase08bVPCCNIPrefix enables IPv4 prefix delegation on the vpc-cni EKS managed addon.
func Phase08bVPCCNIPrefix(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 08b vpc-cni prefix: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 08b vpc-cni prefix: state is nil")
	}
	if dryRun {
		return nil
	}
	if c == nil || c.PhaseEKS == nil {
		return fmt.Errorf("phase 08b vpc-cni prefix: EKS client is nil")
	}

	clusterName := st.Get("EKS_CLUSTER_NAME")
	if clusterName == "" {
		clusterName = cl.Metadata.Name
	}
	if clusterName == "" {
		return fmt.Errorf("phase 08b vpc-cni prefix: cluster name is empty")
	}

	_, err := c.PhaseEKS.DescribeAddon(ctx, &eks.DescribeAddonInput{
		ClusterName: aws.String(clusterName),
		AddonName:   aws.String(vpcCNIAddonName),
	})
	switch {
	case err == nil:
		if _, uerr := c.PhaseEKS.UpdateAddon(ctx, &eks.UpdateAddonInput{
			ClusterName:         aws.String(clusterName),
			AddonName:           aws.String(vpcCNIAddonName),
			ConfigurationValues: aws.String(vpcCNIPrefixConfigJSON),
			ResolveConflicts:    ekstypes.ResolveConflictsOverwrite,
		}); uerr != nil {
			return fmt.Errorf("phase 08b vpc-cni prefix: UpdateAddon %s/%s: %w", clusterName, vpcCNIAddonName, uerr)
		}
	case awsmw.IsNotFound(err):
		if _, cerr := c.PhaseEKS.CreateAddon(ctx, &eks.CreateAddonInput{
			ClusterName:         aws.String(clusterName),
			AddonName:           aws.String(vpcCNIAddonName),
			ConfigurationValues: aws.String(vpcCNIPrefixConfigJSON),
			ResolveConflicts:    ekstypes.ResolveConflictsOverwrite,
		}); cerr != nil {
			return fmt.Errorf("phase 08b vpc-cni prefix: CreateAddon %s/%s: %w", clusterName, vpcCNIAddonName, cerr)
		}
	default:
		return fmt.Errorf("phase 08b vpc-cni prefix: DescribeAddon %s/%s: %w", clusterName, vpcCNIAddonName, err)
	}

	if err := waitForAddonActive(ctx, c, clusterName, vpcCNIAddonName); err != nil {
		return fmt.Errorf("phase 08b vpc-cni prefix: %w", err)
	}

	st.Set("VPC_CNI_PREFIX_DELEGATION", "true")
	return st.Save()
}

// Phase08bVPCCNIPrefixDown clears the VPC_CNI_PREFIX_DELEGATION state key.
func Phase08bVPCCNIPrefixDown(_ context.Context, _ *intent.Cluster, st *state.State, _ *Clients, dryRun bool) error {
	if st == nil || dryRun {
		return nil
	}
	st.Delete("VPC_CNI_PREFIX_DELEGATION")
	return st.Save()
}
