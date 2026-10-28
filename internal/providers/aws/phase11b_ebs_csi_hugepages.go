package aws

import (
	"context"
	"fmt"
	"time"

	"github.com/JLCode-tech/bnkctl/internal/providers/aws/awsmw"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/k8s/manifests"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	asgtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ebsCSIAddonName     = "aws-ebs-csi-driver"
	gp3StorageClassName = "gp3"
)

// InstanceRefreshPollInterval is the polling interval when waiting for an Auto Scaling Group instance refresh.
var InstanceRefreshPollInterval = 15 * time.Second

// InstanceRefreshPollTimeout is the maximum time to wait for an Auto Scaling Group instance refresh.
var InstanceRefreshPollTimeout = 20 * time.Minute

func ensureEBSCSIAddon(ctx context.Context, c *Clients, clusterName string) error {
	out, err := c.PhaseEKS.DescribeAddon(ctx, &eks.DescribeAddonInput{
		ClusterName: aws.String(clusterName),
		AddonName:   aws.String(ebsCSIAddonName),
	})
	switch {
	case err == nil:
		if out != nil && out.Addon != nil && (out.Addon.Status == ekstypes.AddonStatusCreating || out.Addon.Status == ekstypes.AddonStatusUpdating) {
			break
		}
		if _, uerr := c.PhaseEKS.UpdateAddon(ctx, &eks.UpdateAddonInput{
			ClusterName:      aws.String(clusterName),
			AddonName:        aws.String(ebsCSIAddonName),
			ResolveConflicts: ekstypes.ResolveConflictsOverwrite,
		}); uerr != nil {
			return fmt.Errorf("UpdateAddon %s/%s: %w", clusterName, ebsCSIAddonName, uerr)
		}
	case awsmw.IsNotFound(err):
		if _, cerr := c.PhaseEKS.CreateAddon(ctx, &eks.CreateAddonInput{
			ClusterName:      aws.String(clusterName),
			AddonName:        aws.String(ebsCSIAddonName),
			ResolveConflicts: ekstypes.ResolveConflictsOverwrite,
		}); cerr != nil {
			return fmt.Errorf("CreateAddon %s/%s: %w", clusterName, ebsCSIAddonName, cerr)
		}
	default:
		return fmt.Errorf("DescribeAddon %s/%s: %w", clusterName, ebsCSIAddonName, err)
	}
	return waitForAddonActive(ctx, c, clusterName, ebsCSIAddonName)
}

func ensureGP3DefaultStorageClass(ctx context.Context, c *Clients) error {
	raw, err := manifests.FS.ReadFile("shared/gp3-storage-class.yaml")
	if err != nil {
		return fmt.Errorf("reading embedded shared/gp3-storage-class.yaml: %w", err)
	}
	if err := applyRawYAML(ctx, c, raw, ""); err != nil {
		return fmt.Errorf("applying gp3 StorageClass: %w", err)
	}
	if c.K8s != nil {
		sc, gerr := c.K8s.StorageV1().StorageClasses().Get(ctx, "gp2", metav1.GetOptions{})
		if gerr == nil && sc != nil {
			if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
				sc.Annotations["storageclass.kubernetes.io/is-default-class"] = "false"
				if _, uerr := c.K8s.StorageV1().StorageClasses().Update(ctx, sc, metav1.UpdateOptions{}); uerr != nil {
					return fmt.Errorf("clearing default annotation on gp2 StorageClass: %w", uerr)
				}
			}
		}
	}
	return nil
}

func nodesHaveAllocatableHugepages2Mi(ctx context.Context, c *Clients, minNodes int) (bool, error) {
	if c.K8s == nil {
		return false, nil
	}
	if minNodes < 1 {
		minNodes = 1
	}
	list, err := c.K8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("listing nodes for hugepages check: %w", err)
	}
	withHP := 0
	for i := range list.Items {
		q, ok := list.Items[i].Status.Allocatable["hugepages-2Mi"]
		if ok && !q.IsZero() {
			withHP++
		}
	}
	return withHP >= minNodes, nil
}

func waitForInstanceRefresh(ctx context.Context, c *Clients, asgName, refreshID string) error {
	deadline := time.Now().Add(InstanceRefreshPollTimeout)
	for {
		out, err := c.AutoScaling.DescribeInstanceRefreshes(ctx, &autoscaling.DescribeInstanceRefreshesInput{
			AutoScalingGroupName: aws.String(asgName),
			InstanceRefreshIds:   []string{refreshID},
		})
		if err != nil {
			return fmt.Errorf("DescribeInstanceRefreshes %s/%s: %w", asgName, refreshID, err)
		}
		if len(out.InstanceRefreshes) == 0 {
			return fmt.Errorf("DescribeInstanceRefreshes %s/%s returned 0 items", asgName, refreshID)
		}
		ir := out.InstanceRefreshes[0]
		switch ir.Status {
		case asgtypes.InstanceRefreshStatusSuccessful:
			return nil
		case asgtypes.InstanceRefreshStatusFailed, asgtypes.InstanceRefreshStatusCancelled, asgtypes.InstanceRefreshStatusRollbackSuccessful, asgtypes.InstanceRefreshStatusRollbackFailed:
			reason := ""
			if ir.StatusReason != nil {
				reason = *ir.StatusReason
			}
			return fmt.Errorf("instance refresh %s on ASG %s ended with status %s (%s)", refreshID, asgName, ir.Status, reason)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for instance refresh %s on ASG %s (current: %s)", refreshID, asgName, ir.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(InstanceRefreshPollInterval):
		}
	}
}

func ensureHugepagesDaemonSetAndRollingRefresh(ctx context.Context, cl *intent.Cluster, c *Clients) error {
	raw, err := manifests.FS.ReadFile("shared/hugepages-ds.yaml")
	if err != nil {
		return fmt.Errorf("reading embedded shared/hugepages-ds.yaml: %w", err)
	}
	if err := applyRawYAML(ctx, c, raw, "kube-system"); err != nil {
		return fmt.Errorf("applying hugepages-init DaemonSet: %w", err)
	}

	ok, err := nodesHaveAllocatableHugepages2Mi(ctx, c, cl.TotalDesiredNodes())
	if err != nil {
		return err
	}
	if ok {
		return nil
	}

	if c.AutoScaling == nil || c.PhaseEKS == nil {
		return nil
	}

	clusterName := cl.Metadata.Name
	ngList, err := c.PhaseEKS.ListNodegroups(ctx, &eks.ListNodegroupsInput{
		ClusterName: aws.String(clusterName),
	})
	if err != nil {
		return fmt.Errorf("ListNodegroups %s: %w", clusterName, err)
	}
	for _, ngName := range ngList.Nodegroups {
		ngOut, derr := c.PhaseEKS.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{
			ClusterName:   aws.String(clusterName),
			NodegroupName: aws.String(ngName),
		})
		if derr != nil || ngOut.Nodegroup == nil || ngOut.Nodegroup.Resources == nil {
			continue
		}
		for _, asg := range ngOut.Nodegroup.Resources.AutoScalingGroups {
			if asg.Name == nil || *asg.Name == "" {
				continue
			}
			srOut, serr := c.AutoScaling.StartInstanceRefresh(ctx, &autoscaling.StartInstanceRefreshInput{
				AutoScalingGroupName: asg.Name,
				Preferences: &asgtypes.RefreshPreferences{
					MinHealthyPercentage: aws.Int32(0),
				},
			})
			if serr != nil {
				return fmt.Errorf("StartInstanceRefresh on ASG %s: %w", *asg.Name, serr)
			}
			if srOut.InstanceRefreshId != nil {
				if werr := waitForInstanceRefresh(ctx, c, *asg.Name, *srOut.InstanceRefreshId); werr != nil {
					return werr
				}
			}
		}
	}

	if c.K8s != nil {
		if _, err := waitForNodesReady(ctx, c.K8s, cl.TotalDesiredNodes()); err != nil {
			return fmt.Errorf("waiting for nodes Ready after hugepages refresh: %w", err)
		}
	}
	return nil
}

// Phase11bEBSCSIHugepages installs the EBS CSI addon, sets gp3 as default StorageClass, and ensures hugepages-2Mi are allocatable.
func Phase11bEBSCSIHugepages(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 11b ebs-csi/hugepages: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 11b ebs-csi/hugepages: state is nil")
	}
	if !cl.HasExternalDataPath() || dryRun {
		return nil
	}
	if c == nil || c.PhaseEKS == nil {
		return fmt.Errorf("phase 11b ebs-csi/hugepages: EKS client is nil")
	}

	clusterName := st.Get("EKS_CLUSTER_NAME")
	if clusterName == "" {
		clusterName = cl.Metadata.Name
	}

	if err := ensureEBSCSIAddon(ctx, c, clusterName); err != nil {
		return fmt.Errorf("phase 11b ebs-csi/hugepages: %w", err)
	}
	st.Set("EBS_CSI_INSTALLED", "true")

	if c.Dynamic != nil && c.RESTMapper != nil {
		if err := ensureGP3DefaultStorageClass(ctx, c); err != nil {
			return fmt.Errorf("phase 11b ebs-csi/hugepages: %w", err)
		}
		st.Set("DEFAULT_STORAGE_CLASS", gp3StorageClassName)

		if err := ensureHugepagesDaemonSetAndRollingRefresh(ctx, cl, c); err != nil {
			return fmt.Errorf("phase 11b ebs-csi/hugepages: %w", err)
		}
		st.Set("HUGEPAGES_2MI_READY", "true")
	}

	return st.Save()
}

// Phase11bEBSCSIHugepagesDown deletes the EBS CSI addon if installed and clears Phase 11b state keys.
func Phase11bEBSCSIHugepagesDown(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if st == nil || dryRun {
		return nil
	}
	if c != nil && c.PhaseEKS != nil {
		clusterName := st.Get("EKS_CLUSTER_NAME")
		if clusterName == "" && cl != nil {
			clusterName = cl.Metadata.Name
		}
		if clusterName != "" {
			_, _ = c.PhaseEKS.DeleteAddon(ctx, &eks.DeleteAddonInput{
				ClusterName: aws.String(clusterName),
				AddonName:   aws.String(ebsCSIAddonName),
			})
		}
	}
	for _, k := range []string{
		"EBS_CSI_INSTALLED",
		"EBS_CSI_ADDON_ARN",
		"EBS_CSI_ADDON_STATUS",
		"DEFAULT_STORAGE_CLASS",
		"GP3_STORAGE_CLASS",
		"HUGEPAGES_2MI_READY",
		"HUGEPAGES_DAEMONSET",
		"HUGEPAGES_DS_INSTALLED_AT",
	} {
		st.Delete(k)
	}
	return st.Save()
}
