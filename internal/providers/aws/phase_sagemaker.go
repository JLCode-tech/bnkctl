package aws

import (
	"context"
	"encoding/json"
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
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sagemaker"
	smtypes "github.com/aws/aws-sdk-go-v2/service/sagemaker/types"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// SageMakerPollInterval is the polling interval when waiting for a SageMaker HyperPod cluster state transition.
var SageMakerPollInterval = 15 * time.Second

// SageMakerPollTimeout is the maximum time to wait for a SageMaker HyperPod cluster state transition.
var SageMakerPollTimeout = 30 * time.Minute

const sagemakerAssumeRolePolicy = `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": { "Service": "sagemaker.amazonaws.com" },
      "Action": "sts:AssumeRole"
    }
  ]
}`

var sagemakerExecutionManagedPolicies = []string{
	"arn:aws:iam::aws:policy/AmazonSageMakerClusterInstanceRolePolicy",
	"arn:aws:iam::aws:policy/AmazonS3FullAccess",
	"arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy",
	"arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy",
	"arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly",
	"arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore",
}

const defaultHyperPodLifecycleScript = `#!/bin/bash
set -ex
echo "HyperPod lifecycle no-op initialized by bnkctl"
`

func ensureSageMakerServiceLinkedRole(ctx context.Context, c *Clients) error {
	if c.PhaseIAM == nil {
		return nil
	}
	_, err := c.PhaseIAM.CreateServiceLinkedRole(ctx, &iam.CreateServiceLinkedRoleInput{
		AWSServiceName: aws.String("sagemaker.amazonaws.com"),
	})
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "InvalidInput") || strings.Contains(msg, "has been taken") || isEntityAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("CreateServiceLinkedRole sagemaker.amazonaws.com: %w", err)
	}
	return nil
}

func ensureSageMakerExecutionRole(ctx context.Context, c *Clients, st *state.State, cl *intent.Cluster) (string, error) {
	if cl.AI != nil && cl.AI.SageMaker != nil && cl.AI.SageMaker.ExecutionRoleARN != "" {
		st.Set("SAGEMAKER_EXECUTION_ROLE_ARN", cl.AI.SageMaker.ExecutionRoleARN)
		return cl.AI.SageMaker.ExecutionRoleARN, st.Save()
	}
	roleName := cl.Metadata.Name + "-sagemaker-hyperpod-role"
	return ensureRole(
		ctx, c, st, cl,
		roleName,
		sagemakerAssumeRolePolicy,
		sagemakerExecutionManagedPolicies,
		"SAGEMAKER_EXECUTION_ROLE_ARN",
		"SAGEMAKER_EXECUTION_ROLE_NAME",
	)
}

func ensureSageMakerLifecycleBucket(ctx context.Context, c *Clients, st *state.State, cl *intent.Cluster) (string, error) {
	if cl.AI != nil && cl.AI.SageMaker != nil && cl.AI.SageMaker.LifecycleScriptS3URI != "" {
		st.Set("SAGEMAKER_LIFECYCLE_S3_URI", cl.AI.SageMaker.LifecycleScriptS3URI)
		return cl.AI.SageMaker.LifecycleScriptS3URI, st.Save()
	}
	if c.PhaseS3 == nil {
		return "", fmt.Errorf("S3 client is nil and ai.sagemaker.lifecycleScriptS3Uri was not provided")
	}

	acct := st.Get("AWS_ACCOUNT_ID")
	if acct == "" {
		acct = "bnk"
	}
	bucket := strings.ToLower(fmt.Sprintf("%s-hyperpod-lcs-%s-%s", cl.Metadata.Name, acct, cl.Metadata.Region))
	if len(bucket) > 63 {
		bucket = bucket[:63]
	}
	bucket = strings.TrimRight(bucket, "-")

	_, err := c.PhaseS3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	if err != nil {
		createIn := &s3.CreateBucketInput{
			Bucket: aws.String(bucket),
		}
		if cl.Metadata.Region != "" && cl.Metadata.Region != "us-east-1" {
			createIn.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
				LocationConstraint: s3types.BucketLocationConstraint(cl.Metadata.Region),
			}
		}
		if _, cerr := c.PhaseS3.CreateBucket(ctx, createIn); cerr != nil &&
			!strings.Contains(cerr.Error(), "BucketAlreadyOwnedByYou") &&
			!strings.Contains(cerr.Error(), "BucketAlreadyExists") {
			return "", fmt.Errorf("CreateBucket %s: %w", bucket, cerr)
		}
	}

	key := "LifeCycleScripts/base-config/on_create.sh"
	body := strings.NewReader(defaultHyperPodLifecycleScript)
	if _, err := c.PhaseS3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   body,
	}); err != nil {
		return "", fmt.Errorf("PutObject s3://%s/%s: %w", bucket, key, err)
	}

	s3URI := fmt.Sprintf("s3://%s/LifeCycleScripts/base-config/", bucket)
	st.Set("SAGEMAKER_LIFECYCLE_BUCKET", bucket)
	st.Set("SAGEMAKER_LIFECYCLE_S3_URI", s3URI)
	st.Set("SAGEMAKER_LIFECYCLE_KEY", key)
	if err := st.Save(); err != nil {
		return "", err
	}
	return s3URI, nil
}

func ensureSageMakerEFAVpcSG(ctx context.Context, c *Clients, st *state.State, cl *intent.Cluster) ([]string, error) {
	clusterSG := st.Get("EKS_CLUSTER_SG_ID")
	if c.PhaseEC2 == nil {
		if clusterSG != "" {
			return []string{clusterSG}, nil
		}
		return nil, fmt.Errorf("EKS_CLUSTER_SG_ID not found in state and EC2 client is nil")
	}

	if existing := st.Get("SAGEMAKER_SG_ID"); existing != "" {
		out, err := c.PhaseEC2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
			GroupIds: []string{existing},
		})
		if err == nil && len(out.SecurityGroups) > 0 {
			if clusterSG != "" {
				return []string{clusterSG, existing}, nil
			}
			return []string{existing}, nil
		}
		st.Delete("SAGEMAKER_SG_ID")
	}

	vpcID := st.Get("VPC_ID")
	if vpcID == "" {
		if clusterSG != "" {
			return []string{clusterSG}, nil
		}
		return nil, fmt.Errorf("VPC_ID not found in state")
	}

	sgName := cl.Metadata.Name + "-hyperpod-efa-sg"
	t := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, map[string]string{
		"Name": sgName,
	})
	createOut, err := c.PhaseEC2.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:         aws.String(sgName),
		Description:       aws.String("SageMaker HyperPod EFA + intra-VPC security group"),
		VpcId:             aws.String(vpcID),
		TagSpecifications: tags.ToTagSpec(ec2types.ResourceTypeSecurityGroup, t),
	})
	if err != nil {
		return nil, fmt.Errorf("CreateSecurityGroup %s: %w", sgName, err)
	}
	sgID := aws.ToString(createOut.GroupId)

	_, ierr := c.PhaseEC2.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []ec2types.IpPermission{
			{
				IpProtocol: aws.String("-1"),
				UserIdGroupPairs: []ec2types.UserIdGroupPair{
					{GroupId: aws.String(sgID), Description: aws.String("Allow all self-referenced traffic for EFA/NCCL")},
				},
			},
			{
				IpProtocol: aws.String("-1"),
				IpRanges: []ec2types.IpRange{
					{CidrIp: aws.String(cl.Network.VPCCidr), Description: aws.String("Allow all intra-VPC traffic")},
				},
			},
		},
	})
	if ierr != nil && !strings.Contains(ierr.Error(), "InvalidPermission.Duplicate") {
		return nil, fmt.Errorf("AuthorizeSecurityGroupIngress on %s: %w", sgID, ierr)
	}

	st.Set("SAGEMAKER_SG_ID", sgID)
	if err := st.Save(); err != nil {
		return nil, err
	}
	if clusterSG != "" {
		return []string{clusterSG, sgID}, nil
	}
	return []string{sgID}, nil
}

func ensureSageMakerK8sNodeRBAC(ctx context.Context, c *Clients, nodeRoleARN, execRoleARN string) error {
	if c == nil || c.K8s == nil {
		return nil
	}

	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "hyperpod-Bootstrappers",
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "system:node",
		},
		Subjects: []rbacv1.Subject{
			{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "Group",
				Name:     "system:bootstrappers",
			},
			{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "Group",
				Name:     "system:nodes",
			},
		},
	}
	if _, err := c.K8s.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating ClusterRoleBinding hyperpod-Bootstrappers: %w", err)
	}

	var entries []string
	seen := map[string]bool{}
	for _, r := range []string{nodeRoleARN, execRoleARN} {
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		entries = append(entries, fmt.Sprintf(`- groups:
  - system:bootstrappers
  - system:nodes
  rolearn: %s
  username: system:node:{{EC2PrivateDNSName}}`, r))
	}
	if len(entries) == 0 {
		return nil
	}
	mapRolesYAML := strings.Join(entries, "\n") + "\n"

	cm, err := c.K8s.CoreV1().ConfigMaps("kube-system").Get(ctx, "aws-auth", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		newCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "aws-auth",
				Namespace: "kube-system",
			},
			Data: map[string]string{
				"mapRoles": mapRolesYAML,
			},
		}
		if _, cerr := c.K8s.CoreV1().ConfigMaps("kube-system").Create(ctx, newCM, metav1.CreateOptions{}); cerr != nil {
			return fmt.Errorf("creating kube-system/aws-auth ConfigMap: %w", cerr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting kube-system/aws-auth ConfigMap: %w", err)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	if !strings.Contains(cm.Data["mapRoles"], execRoleARN) {
		existing := strings.TrimSpace(cm.Data["mapRoles"])
		if existing != "" {
			cm.Data["mapRoles"] = existing + "\n" + mapRolesYAML
		} else {
			cm.Data["mapRoles"] = mapRolesYAML
		}
		if _, uerr := c.K8s.CoreV1().ConfigMaps("kube-system").Update(ctx, cm, metav1.UpdateOptions{}); uerr != nil {
			return fmt.Errorf("updating kube-system/aws-auth ConfigMap: %w", uerr)
		}
	}
	return nil
}

func resolveSageMakerSubnets(st *state.State) ([]string, error) {
	if id := st.Get("PRIVATE_SUBNET_0"); id != "" {
		return []string{id}, nil
	}
	if id := st.Get("PUBLIC_SUBNET_0"); id != "" {
		return []string{id}, nil
	}
	return nil, fmt.Errorf("no worker subnet found in state for SageMaker HyperPod")
}

func waitForSageMakerClusterInService(ctx context.Context, c *Clients, clusterName string) (*sagemaker.DescribeClusterOutput, error) {
	deadline := time.Now().Add(SageMakerPollTimeout)
	for {
		out, err := c.SageMaker.DescribeCluster(ctx, &sagemaker.DescribeClusterInput{
			ClusterName: aws.String(clusterName),
		})
		if err != nil {
			return nil, fmt.Errorf("DescribeCluster %s: %w", clusterName, err)
		}
		switch string(out.ClusterStatus) {
		case "InService":
			return out, nil
		case "Failed", "RollbackFailed":
			msg := aws.ToString(out.FailureMessage)
			return nil, fmt.Errorf("SageMaker HyperPod cluster %s entered %s: %s", clusterName, out.ClusterStatus, msg)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timeout waiting for SageMaker HyperPod cluster %s to reach InService (current: %s)", clusterName, out.ClusterStatus)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(SageMakerPollInterval):
		}
	}
}

func waitForSageMakerClusterDeleted(ctx context.Context, c *Clients, clusterName string) error {
	deadline := time.Now().Add(SageMakerPollTimeout)
	for {
		_, err := c.SageMaker.DescribeCluster(ctx, &sagemaker.DescribeClusterInput{
			ClusterName: aws.String(clusterName),
		})
		if err != nil {
			if awsmw.IsNotFound(err) || strings.Contains(err.Error(), "ResourceNotFound") || strings.Contains(err.Error(), "not found") {
				return nil
			}
			return fmt.Errorf("DescribeCluster %s: %w", clusterName, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for SageMaker HyperPod cluster %s deletion", clusterName)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(SageMakerPollInterval):
		}
	}
}

func toSageMakerTags(m map[string]string) []smtypes.Tag {
	out := make([]smtypes.Tag, 0, len(m))
	for _, kv := range tags.SortedPairs(m) {
		out = append(out, smtypes.Tag{
			Key:   aws.String(kv.Key),
			Value: aws.String(kv.Value),
		})
	}
	return out
}

func labelHyperPodNodesInK8s(ctx context.Context, c *Clients, smClusterName string, igs []intent.SageMakerInstanceGroup) error {
	if c == nil || c.K8s == nil {
		return nil
	}
	list, err := c.K8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing k8s nodes for HyperPod labeling: %w", err)
	}

	groupLabels := map[string]map[string]string{}
	for _, ig := range igs {
		lbls := map[string]string{
			"sagemaker.amazonaws.com/hyperpod-cluster": smClusterName,
			"sagemaker.amazonaws.com/instance-group":   ig.Name,
			"bnk.f5.com/ai-backend":                    intent.AIBackendSageMaker,
		}
		for k, v := range ig.Labels {
			lbls[k] = v
		}
		groupLabels[ig.Name] = lbls
	}

	for i := range list.Items {
		n := &list.Items[i]
		igName := n.Labels["sagemaker.amazonaws.com/instance-group-name"]
		if igName == "" {
			if n.Labels["sagemaker.amazonaws.com/node-health-status"] != "" && len(igs) == 1 {
				igName = igs[0].Name
			}
		}
		lbls, ok := groupLabels[igName]
		if !ok {
			continue
		}
		patch := map[string]interface{}{
			"metadata": map[string]interface{}{
				"labels": lbls,
			},
		}
		raw, merr := json.Marshal(patch)
		if merr != nil {
			return merr
		}
		if _, perr := c.K8s.CoreV1().Nodes().Patch(ctx, n.Name, types.MergePatchType, raw, metav1.PatchOptions{}); perr != nil {
			return fmt.Errorf("patching HyperPod labels on node %s: %w", n.Name, perr)
		}
	}
	return nil
}

// PhaseSageMakerHyperPod provisions an AWS SageMaker HyperPod cluster orchestrated by the workspace's EKS cluster.
func PhaseSageMakerHyperPod(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase sagemaker hyperpod: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase sagemaker hyperpod: state is nil")
	}
	if cl.AI == nil || !cl.AI.IsSageMakerHyperPod() || dryRun {
		return nil
	}
	if c == nil || c.SageMaker == nil {
		return fmt.Errorf("phase sagemaker hyperpod: SageMaker client is nil")
	}

	eksARN := st.Get("EKS_CLUSTER_ARN")
	if eksARN == "" {
		return fmt.Errorf("phase sagemaker hyperpod: EKS_CLUSTER_ARN not found in state (did phase 08 run?)")
	}

	if err := ensureSageMakerServiceLinkedRole(ctx, c); err != nil {
		return fmt.Errorf("phase sagemaker hyperpod: %w", err)
	}

	execRoleARN := st.Get("EKS_NODE_ROLE_ARN")
	if cl.AI.SageMaker != nil && cl.AI.SageMaker.ExecutionRoleARN != "" {
		execRoleARN = cl.AI.SageMaker.ExecutionRoleARN
		st.Set("SAGEMAKER_EXECUTION_ROLE_ARN", execRoleARN)
		_ = st.Save()
	} else if execRoleARN == "" {
		var err error
		execRoleARN, err = ensureSageMakerExecutionRole(ctx, c, st, cl)
		if err != nil {
			return fmt.Errorf("phase sagemaker hyperpod: %w", err)
		}
	} else {
		st.Set("SAGEMAKER_EXECUTION_ROLE_ARN", execRoleARN)
		_ = st.Save()
	}

	if err := ensureSageMakerK8sNodeRBAC(ctx, c, st.Get("EKS_NODE_ROLE_ARN"), execRoleARN); err != nil {
		return fmt.Errorf("phase sagemaker hyperpod: %w", err)
	}

	lcsURI, err := ensureSageMakerLifecycleBucket(ctx, c, st, cl)
	if err != nil {
		return fmt.Errorf("phase sagemaker hyperpod: %w", err)
	}

	smSpec := cl.AI.SageMaker
	smClusterName := smSpec.EffectiveClusterName(cl.Metadata.Name)

	descOut, derr := c.SageMaker.DescribeCluster(ctx, &sagemaker.DescribeClusterInput{
		ClusterName: aws.String(smClusterName),
	})
	if derr == nil && descOut != nil {
		active, werr := waitForSageMakerClusterInService(ctx, c, smClusterName)
		if werr != nil {
			return fmt.Errorf("phase sagemaker hyperpod: %w", werr)
		}
		st.Set("SAGEMAKER_CLUSTER_NAME", smClusterName)
		if active.ClusterArn != nil {
			st.Set("SAGEMAKER_CLUSTER_ARN", *active.ClusterArn)
		}
		if err := labelHyperPodNodesInK8s(ctx, c, smClusterName, smSpec.InstanceGroups); err != nil {
			return fmt.Errorf("phase sagemaker hyperpod: %w", err)
		}
		return st.Save()
	}

	subnets, err := resolveSageMakerSubnets(st)
	if err != nil {
		return fmt.Errorf("phase sagemaker hyperpod: %w", err)
	}
	sgIDs, err := ensureSageMakerEFAVpcSG(ctx, c, st, cl)
	if err != nil {
		return fmt.Errorf("phase sagemaker hyperpod: %w", err)
	}

	igs := make([]smtypes.ClusterInstanceGroupSpecification, 0, len(smSpec.InstanceGroups))
	for _, ig := range smSpec.InstanceGroups {
		// #nosec G115 -- InstanceCount and VolumeSizeGiB are bounded positive integers validated by intent schema
		spec := smtypes.ClusterInstanceGroupSpecification{
			InstanceGroupName: aws.String(ig.Name),
			InstanceType:      smtypes.ClusterInstanceType(ig.InstanceType),
			InstanceCount:     aws.Int32(int32(ig.InstanceCount)),
			ExecutionRole:     aws.String(execRoleARN),
			LifeCycleConfig: &smtypes.ClusterLifeCycleConfig{
				SourceS3Uri: aws.String(lcsURI),
				OnCreate:    aws.String("on_create.sh"),
			},
			InstanceStorageConfigs: []smtypes.ClusterInstanceStorageConfig{
				&smtypes.ClusterInstanceStorageConfigMemberEbsVolumeConfig{
					Value: smtypes.ClusterEbsVolumeConfig{
						VolumeSizeInGB: aws.Int32(int32(ig.EffectiveVolumeSizeGiB())),
					},
				},
			},
		}
		if ig.ThreadsPerCore == 1 || ig.ThreadsPerCore == 2 {
			// #nosec G115 -- ThreadsPerCore is 1 or 2
			spec.ThreadsPerCore = aws.Int32(int32(ig.ThreadsPerCore))
		}
		if ig.EnableStressCheck || ig.EnableConnectivityCheck {
			var checks []smtypes.DeepHealthCheckType
			if ig.EnableStressCheck {
				checks = append(checks, smtypes.DeepHealthCheckTypeInstanceStress)
			}
			if ig.EnableConnectivityCheck {
				checks = append(checks, smtypes.DeepHealthCheckTypeInstanceConnectivity)
			}
			spec.OnStartDeepHealthChecks = checks
		}
		igs = append(igs, spec)
	}

	t := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, map[string]string{
		"Name": smClusterName,
	})

	createIn := &sagemaker.CreateClusterInput{
		ClusterName:    aws.String(smClusterName),
		InstanceGroups: igs,
		Orchestrator: &smtypes.ClusterOrchestrator{
			Eks: &smtypes.ClusterOrchestratorEksConfig{
				ClusterArn: aws.String(eksARN),
			},
		},
		VpcConfig: &smtypes.VpcConfig{
			Subnets:          subnets,
			SecurityGroupIds: sgIDs,
		},
		NodeRecovery: smtypes.ClusterNodeRecovery(smSpec.EffectiveNodeRecovery()),
		Tags:         toSageMakerTags(t),
	}

	createOut, cerr := c.SageMaker.CreateCluster(ctx, createIn)
	if cerr != nil {
		return fmt.Errorf("phase sagemaker hyperpod: CreateCluster %s: %w", smClusterName, cerr)
	}
	st.Set("SAGEMAKER_CLUSTER_NAME", smClusterName)
	if createOut.ClusterArn != nil {
		st.Set("SAGEMAKER_CLUSTER_ARN", *createOut.ClusterArn)
	}
	if err := st.Save(); err != nil {
		return err
	}

	active, err := waitForSageMakerClusterInService(ctx, c, smClusterName)
	if err != nil {
		return fmt.Errorf("phase sagemaker hyperpod: %w", err)
	}
	if active.ClusterArn != nil {
		st.Set("SAGEMAKER_CLUSTER_ARN", *active.ClusterArn)
	}

	if err := labelHyperPodNodesInK8s(ctx, c, smClusterName, smSpec.InstanceGroups); err != nil {
		return fmt.Errorf("phase sagemaker hyperpod: %w", err)
	}

	return st.Save()
}

// PhaseSageMakerHyperPodDown deletes the SageMaker HyperPod cluster, its S3 lifecycle bucket, and its execution IAM role.
func PhaseSageMakerHyperPodDown(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase sagemaker hyperpod down: cluster intent is nil")
	}
	if st == nil || dryRun {
		return nil
	}

	smClusterName := st.Get("SAGEMAKER_CLUSTER_NAME")
	if smClusterName == "" && cl.AI != nil && cl.AI.IsSageMakerHyperPod() && cl.AI.SageMaker != nil {
		smClusterName = cl.AI.SageMaker.EffectiveClusterName(cl.Metadata.Name)
	}

	if smClusterName != "" && c != nil && c.SageMaker != nil {
		_, err := c.SageMaker.DeleteCluster(ctx, &sagemaker.DeleteClusterInput{
			ClusterName: aws.String(smClusterName),
		})
		if err != nil && !awsmw.IsNotFound(err) && !strings.Contains(err.Error(), "ResourceNotFound") && !strings.Contains(err.Error(), "not found") {
			return fmt.Errorf("phase sagemaker hyperpod down: DeleteCluster %s: %w", smClusterName, err)
		}
		if err := waitForSageMakerClusterDeleted(ctx, c, smClusterName); err != nil {
			return fmt.Errorf("phase sagemaker hyperpod down: %w", err)
		}
	}
	st.Delete("SAGEMAKER_CLUSTER_NAME")
	st.Delete("SAGEMAKER_CLUSTER_ARN")

	if bucket := st.Get("SAGEMAKER_LIFECYCLE_BUCKET"); bucket != "" && c != nil && c.PhaseS3 != nil {
		key := st.Get("SAGEMAKER_LIFECYCLE_KEY")
		if key == "" {
			key = "LifeCycleScripts/base-config/on_create.sh"
		}
		_, _ = c.PhaseS3.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		})
		_, _ = c.PhaseS3.DeleteBucket(ctx, &s3.DeleteBucketInput{
			Bucket: aws.String(bucket),
		})
	}
	st.Delete("SAGEMAKER_LIFECYCLE_BUCKET")
	st.Delete("SAGEMAKER_LIFECYCLE_S3_URI")
	st.Delete("SAGEMAKER_LIFECYCLE_KEY")

	if sgID := st.Get("SAGEMAKER_SG_ID"); sgID != "" && c != nil && c.PhaseEC2 != nil {
		_, _ = c.PhaseEC2.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{
			GroupId: aws.String(sgID),
		})
	}
	st.Delete("SAGEMAKER_SG_ID")

	if roleName := st.Get("SAGEMAKER_EXECUTION_ROLE_NAME"); roleName != "" && c != nil && c.PhaseIAM != nil {
		if err := deleteRoleWithPolicies(ctx, c, roleName); err != nil {
			return fmt.Errorf("phase sagemaker hyperpod down: %w", err)
		}
	}
	st.Delete("SAGEMAKER_EXECUTION_ROLE_NAME")
	st.Delete("SAGEMAKER_EXECUTION_ROLE_ARN")

	return st.Save()
}
