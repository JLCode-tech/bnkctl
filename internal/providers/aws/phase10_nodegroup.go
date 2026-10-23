package aws

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/JLCode-tech/bnkctl/internal/providers/aws/awsmw"
	"github.com/JLCode-tech/bnkctl/internal/providers/aws/tags"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

// NodeGroupPollInterval is the polling interval when waiting for managed nodegroup state transitions.
var NodeGroupPollInterval = 15 * time.Second

// NodeGroupPollTimeout is the maximum time to wait for a managed nodegroup state transition.
var NodeGroupPollTimeout = 20 * time.Minute

func nodegroupStateKey(index int) string {
	return fmt.Sprintf("NODEGROUP_NAME_%d", index)
}

func launchTemplateStateKey(index int) string {
	return fmt.Sprintf("NODEGROUP_LT_ID_%d", index)
}

func hugepagesMIMEUserData(nrHugepages int) string {
	doc := fmt.Sprintf(`MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="BOUNDARY"

--BOUNDARY
Content-Type: text/x-shellscript; charset="us-ascii"

#!/bin/bash
set -ex
echo "vm.nr_hugepages = %d" > /etc/sysctl.d/99-bnk-hugepages.conf
sysctl -p /etc/sysctl.d/99-bnk-hugepages.conf

--BOUNDARY--
`, nrHugepages)
	return base64.StdEncoding.EncodeToString([]byte(doc))
}

func ensureHugepagesLaunchTemplate(
	ctx context.Context,
	c *Clients,
	cl *intent.Cluster,
	st *state.State,
	ngIndex int,
	ng intent.NodeGroup,
) (string, error) {
	if c.PhaseEC2 == nil {
		return "", fmt.Errorf("EC2 client is nil (required to create Launch Template for hugepages)")
	}
	ltKey := launchTemplateStateKey(ngIndex)
	ltName := fmt.Sprintf("%s-%s-hugepages-lt", cl.Metadata.Name, ng.Name)

	if existingID := st.Get(ltKey); existingID != "" {
		out, err := c.PhaseEC2.DescribeLaunchTemplates(ctx, &ec2.DescribeLaunchTemplatesInput{
			LaunchTemplateIds: []string{existingID},
		})
		if err == nil && len(out.LaunchTemplates) > 0 {
			return existingID, nil
		}
	}

	out, err := c.PhaseEC2.DescribeLaunchTemplates(ctx, &ec2.DescribeLaunchTemplatesInput{
		LaunchTemplateNames: []string{ltName},
	})
	if err == nil && len(out.LaunchTemplates) > 0 && out.LaunchTemplates[0].LaunchTemplateId != nil {
		ltID := *out.LaunchTemplates[0].LaunchTemplateId
		st.Set(ltKey, ltID)
		_ = st.Save()
		return ltID, nil
	}

	t := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, map[string]string{
		"Name": ltName,
	})
	createOut, err := c.PhaseEC2.CreateLaunchTemplate(ctx, &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String(ltName),
		LaunchTemplateData: &ec2types.RequestLaunchTemplateData{
			UserData: aws.String(hugepagesMIMEUserData(ng.Hugepages2MiCount())),
		},
		TagSpecifications: tags.ToTagSpec(ec2types.ResourceTypeLaunchTemplate, t),
	})
	if err != nil {
		return "", fmt.Errorf("CreateLaunchTemplate %s: %w", ltName, err)
	}
	if createOut.LaunchTemplate == nil || createOut.LaunchTemplate.LaunchTemplateId == nil {
		return "", fmt.Errorf("CreateLaunchTemplate %s returned nil LaunchTemplateId", ltName)
	}
	ltID := *createOut.LaunchTemplate.LaunchTemplateId
	st.Set(ltKey, ltID)
	if err := st.Save(); err != nil {
		return "", err
	}
	return ltID, nil
}

func waitForNodeGroupActive(ctx context.Context, c *Clients, clusterName, ngName string) error {
	deadline := time.Now().Add(NodeGroupPollTimeout)
	for {
		out, err := c.PhaseEKS.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{
			ClusterName:   aws.String(clusterName),
			NodegroupName: aws.String(ngName),
		})
		if err != nil {
			return fmt.Errorf("DescribeNodegroup %s/%s: %w", clusterName, ngName, err)
		}
		if out.Nodegroup == nil {
			return fmt.Errorf("DescribeNodegroup %s/%s returned nil Nodegroup", clusterName, ngName)
		}
		switch out.Nodegroup.Status {
		case ekstypes.NodegroupStatusActive:
			return nil
		case ekstypes.NodegroupStatusCreateFailed, ekstypes.NodegroupStatusDeleteFailed, ekstypes.NodegroupStatusDegraded:
			return fmt.Errorf("nodegroup %s/%s entered %s status", clusterName, ngName, out.Nodegroup.Status)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for nodegroup %s/%s to become ACTIVE (current: %s)", clusterName, ngName, out.Nodegroup.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(NodeGroupPollInterval):
		}
	}
}

func waitForNodeGroupDeleted(ctx context.Context, c *Clients, clusterName, ngName string) error {
	deadline := time.Now().Add(NodeGroupPollTimeout)
	for {
		_, err := c.PhaseEKS.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{
			ClusterName:   aws.String(clusterName),
			NodegroupName: aws.String(ngName),
		})
		if err != nil {
			if awsmw.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("DescribeNodegroup %s/%s: %w", clusterName, ngName, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for nodegroup %s/%s deletion", clusterName, ngName)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(NodeGroupPollInterval):
		}
	}
}

func resolveNodeGroupSubnets(cl *intent.Cluster, st *state.State, ng intent.NodeGroup) ([]string, error) {
	hasPrivate := len(cl.Network.Subnets.PrivateList) > 0
	var ids []string

	if ng.SingleAZ && ng.AZ != "" {
		if hasPrivate {
			for i, s := range cl.Network.Subnets.PrivateList {
				if s.AZ == ng.AZ {
					if id := st.Get(subnetStateKey("PRIVATE_SUBNET", i)); id != "" {
						ids = append(ids, id)
					}
				}
			}
		} else {
			for i, s := range cl.Network.Subnets.PublicList {
				if s.AZ == ng.AZ {
					if id := st.Get(subnetStateKey("PUBLIC_SUBNET", i)); id != "" {
						ids = append(ids, id)
					}
				}
			}
		}
		if len(ids) > 0 {
			return ids, nil
		}
	}

	if hasPrivate {
		for i := range cl.Network.Subnets.PrivateList {
			if id := st.Get(subnetStateKey("PRIVATE_SUBNET", i)); id != "" {
				ids = append(ids, id)
			}
		}
	} else {
		for i := range cl.Network.Subnets.PublicList {
			if id := st.Get(subnetStateKey("PUBLIC_SUBNET", i)); id != "" {
				ids = append(ids, id)
			}
		}
	}

	if len(ids) == 0 {
		return nil, fmt.Errorf("no worker subnets found in state for nodegroup %s", ng.Name)
	}
	return ids, nil
}

func resolveNodeGroupAMIType(ng intent.NodeGroup) ekstypes.AMITypes {
	switch strings.ToUpper(strings.TrimSpace(ng.AMIType)) {
	case string(ekstypes.AMITypesAl2023X8664Nvidia):
		return ekstypes.AMITypesAl2023X8664Nvidia
	case string(ekstypes.AMITypesAl2X8664Gpu):
		return ekstypes.AMITypesAl2X8664Gpu
	case string(ekstypes.AMITypesAl2023Arm64Standard):
		return ekstypes.AMITypesAl2023Arm64Standard
	case string(ekstypes.AMITypesAl2023X8664Standard):
		return ekstypes.AMITypesAl2023X8664Standard
	default:
		if ng.IsGPU() {
			return ekstypes.AMITypesAl2023X8664Nvidia
		}
		return ekstypes.AMITypesAl2023X8664Standard
	}
}

func toEKSTaints(taints []intent.NodeTaint) ([]ekstypes.Taint, error) {
	if len(taints) == 0 {
		return nil, nil
	}
	out := make([]ekstypes.Taint, 0, len(taints))
	for _, t := range taints {
		var eff ekstypes.TaintEffect
		switch t.Effect {
		case "NoSchedule":
			eff = ekstypes.TaintEffectNoSchedule
		case "NoExecute":
			eff = ekstypes.TaintEffectNoExecute
		case "PreferNoSchedule":
			eff = ekstypes.TaintEffectPreferNoSchedule
		default:
			return nil, fmt.Errorf("unsupported taint effect %q on key %q", t.Effect, t.Key)
		}
		et := ekstypes.Taint{
			Key:    aws.String(t.Key),
			Effect: eff,
		}
		if t.Value != "" {
			et.Value = aws.String(t.Value)
		}
		out = append(out, et)
	}
	return out, nil
}

// Phase10NodeGroup creates (or re-uses) all managed node groups declared in the cluster intent and waits for each to reach ACTIVE.
func Phase10NodeGroup(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 10 nodegroup: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 10 nodegroup: state is nil")
	}
	if dryRun || cl.ClusterSpec == nil {
		return nil
	}
	if c == nil || c.PhaseEKS == nil {
		return fmt.Errorf("phase 10 nodegroup: EKS client is nil")
	}

	clusterName := st.Get("EKS_CLUSTER_NAME")
	if clusterName == "" {
		clusterName = cl.Metadata.Name
	}
	nodeRoleARN := st.Get("EKS_NODE_ROLE_ARN")
	if nodeRoleARN == "" {
		return fmt.Errorf("phase 10 nodegroup: EKS_NODE_ROLE_ARN not found in state")
	}

	for i, ng := range cl.ClusterSpec.NodeGroups {
		descOut, err := c.PhaseEKS.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{
			ClusterName:   aws.String(clusterName),
			NodegroupName: aws.String(ng.Name),
		})
		if err == nil && descOut.Nodegroup != nil {
			if err := waitForNodeGroupActive(ctx, c, clusterName, ng.Name); err != nil {
				return fmt.Errorf("phase 10 nodegroup: %w", err)
			}
			st.Set(nodegroupStateKey(i), ng.Name)
			if err := st.Save(); err != nil {
				return err
			}
			continue
		}
		if err != nil && !awsmw.IsNotFound(err) {
			return fmt.Errorf("phase 10 nodegroup: DescribeNodegroup %s/%s: %w", clusterName, ng.Name, err)
		}

		subnets, err := resolveNodeGroupSubnets(cl, st, ng)
		if err != nil {
			return fmt.Errorf("phase 10 nodegroup: %w", err)
		}

		t := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, map[string]string{
			"Name": fmt.Sprintf("%s-%s", clusterName, ng.Name),
		})

		eksTaints, err := toEKSTaints(ng.Taints)
		if err != nil {
			return fmt.Errorf("phase 10 nodegroup: %w", err)
		}

		// #nosec G115 -- nodegroup sizing and disk values are bounded positive integers validated by intent schema
		createIn := &eks.CreateNodegroupInput{
			ClusterName:   aws.String(clusterName),
			NodegroupName: aws.String(ng.Name),
			NodeRole:      aws.String(nodeRoleARN),
			Subnets:       subnets,
			InstanceTypes: []string{ng.InstanceType},
			AmiType:       resolveNodeGroupAMIType(ng),
			ScalingConfig: &ekstypes.NodegroupScalingConfig{
				DesiredSize: aws.Int32(int32(ng.DesiredSize)),
				MinSize:     aws.Int32(int32(ng.MinSize)),
				MaxSize:     aws.Int32(int32(ng.MaxSize)),
			},
			Labels: ng.Labels,
			Taints: eksTaints,
			Tags:   t,
		}
		if ng.NeedsHugepagesLaunchTemplate() {
			ltID, ltErr := ensureHugepagesLaunchTemplate(ctx, c, cl, st, i, ng)
			if ltErr != nil {
				return fmt.Errorf("phase 10 nodegroup: %w", ltErr)
			}
			createIn.LaunchTemplate = &ekstypes.LaunchTemplateSpecification{
				Id:      aws.String(ltID),
				Version: aws.String("$Latest"),
			}
		} else if ng.DiskSizeGiB > 0 {
			// #nosec G115 -- DiskSizeGiB is a small positive integer
			createIn.DiskSize = aws.Int32(int32(ng.DiskSizeGiB))
		}

		if _, cerr := c.PhaseEKS.CreateNodegroup(ctx, createIn); cerr != nil {
			return fmt.Errorf("phase 10 nodegroup: CreateNodegroup %s/%s: %w", clusterName, ng.Name, cerr)
		}

		st.Set(nodegroupStateKey(i), ng.Name)
		if err := st.Save(); err != nil {
			return err
		}

		if err := waitForNodeGroupActive(ctx, c, clusterName, ng.Name); err != nil {
			return fmt.Errorf("phase 10 nodegroup: %w", err)
		}
	}

	return nil
}

// Phase10NodeGroupDown deletes all managed nodegroups in reverse order, launch templates, and orphaned cluster EBS volumes.
func Phase10NodeGroupDown(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 10 nodegroup down: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 10 nodegroup down: state is nil")
	}
	if dryRun {
		return nil
	}
	if c == nil || c.PhaseEKS == nil {
		return fmt.Errorf("phase 10 nodegroup down: EKS client is nil")
	}

	clusterName := st.Get("EKS_CLUSTER_NAME")
	if clusterName == "" {
		clusterName = cl.Metadata.Name
	}

	var ngCount int
	if cl.ClusterSpec != nil {
		ngCount = len(cl.ClusterSpec.NodeGroups)
	}

	var ngNames []string
	seenNG := map[string]bool{}
	addNG := func(name string) {
		if name != "" && !seenNG[name] {
			seenNG[name] = true
			ngNames = append(ngNames, name)
		}
	}

	for i := ngCount - 1; i >= 0; i-- {
		key := nodegroupStateKey(i)
		addNG(st.Get(key))
		ngSpecName := cl.ClusterSpec.NodeGroups[i].Name
		addNG(ngSpecName)
		if ngSpecName != "" && clusterName != "" {
			addNG(clusterName + "-ng-" + ngSpecName)
		}
	}
	addNG(st.Get("NODEGROUP_DEFAULT_NAME"))

	if listOut, lerr := c.PhaseEKS.ListNodegroups(ctx, &eks.ListNodegroupsInput{
		ClusterName: aws.String(clusterName),
	}); lerr == nil && listOut != nil {
		for _, n := range listOut.Nodegroups {
			addNG(n)
		}
	}

	for _, ngName := range ngNames {
		_, err := c.PhaseEKS.DeleteNodegroup(ctx, &eks.DeleteNodegroupInput{
			ClusterName:   aws.String(clusterName),
			NodegroupName: aws.String(ngName),
		})
		if err != nil && !awsmw.IsNotFound(err) {
			return fmt.Errorf("phase 10 nodegroup down: DeleteNodegroup %s/%s: %w", clusterName, ngName, err)
		}
		if err := waitForNodeGroupDeleted(ctx, c, clusterName, ngName); err != nil {
			return fmt.Errorf("phase 10 nodegroup down: %w", err)
		}
	}

	for i := ngCount - 1; i >= 0; i-- {
		st.Delete(nodegroupStateKey(i))
	}
	st.Delete("NODEGROUP_DEFAULT_NAME")
	st.Delete("NODEGROUP_DEFAULT_STATUS")
	st.Delete("NODEGROUP_DEFAULT_ARN")
	for k := range st.All() {
		if strings.HasPrefix(k, "NODEGROUP_") {
			st.Delete(k)
		}
	}

	if c.PhaseEC2 != nil {
		ltIDs := map[string]bool{}
		for i := ngCount - 1; i >= 0; i-- {
			ltKey := launchTemplateStateKey(i)
			if id := st.Get(ltKey); id != "" {
				ltIDs[id] = true
			}
			st.Delete(ltKey)
		}
		for _, k := range []string{"LT_ID", "GPU_LT_ID"} {
			if id := st.Get(k); id != "" {
				ltIDs[id] = true
			}
			st.Delete(k)
		}
		st.Delete("LT_VERSION")
		st.Delete("GPU_LT_VERSION")

		for ltID := range ltIDs {
			_, lerr := c.PhaseEC2.DeleteLaunchTemplate(ctx, &ec2.DeleteLaunchTemplateInput{
				LaunchTemplateId: aws.String(ltID),
			})
			if lerr != nil && !awsmw.IsNotFound(lerr) {
				return fmt.Errorf("phase 10 nodegroup down: DeleteLaunchTemplate %s: %w", ltID, lerr)
			}
		}

		// Delete orphaned available EBS volumes belonging to the cluster (e.g. dynamic PVCs from Prometheus/BNK).
		if clusterName != "" {
			vOut, verr := c.PhaseEC2.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
				Filters: []ec2types.Filter{
					{
						Name:   aws.String("tag:kubernetes.io/cluster/" + clusterName),
						Values: []string{"owned"},
					},
					{
						Name:   aws.String("status"),
						Values: []string{"available"},
					},
				},
			})
			if verr == nil && vOut != nil {
				for _, v := range vOut.Volumes {
					if v.VolumeId != nil {
						_, _ = c.PhaseEC2.DeleteVolume(ctx, &ec2.DeleteVolumeInput{
							VolumeId: v.VolumeId,
						})
					}
				}
			}
		}
	}

	for _, k := range []string{
		"PRIMARY_NODE_INSTANCE_ID",
		"PRIMARY_NODE_NAME",
		"PRIMARY_NODE_AZ",
		"PRIMARY_NODE_PRIVATE_IP",
	} {
		st.Delete(k)
	}

	return st.Save()
}
