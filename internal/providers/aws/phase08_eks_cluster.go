package aws

import (
	"context"
	"fmt"
	"time"

	"github.com/JLCode-tech/bnkctl/internal/providers/aws/awsmw"
	"github.com/JLCode-tech/bnkctl/internal/providers/aws/tags"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

// EKSPollInterval is the polling interval when waiting for EKS control plane state transitions.
var EKSPollInterval = 15 * time.Second

// EKSPollTimeout is the maximum time to wait for an EKS control plane state transition.
var EKSPollTimeout = 20 * time.Minute

func waitForClusterActive(ctx context.Context, c *Clients, clusterName string) (*ekstypes.Cluster, error) {
	deadline := time.Now().Add(EKSPollTimeout)
	for {
		out, err := c.PhaseEKS.DescribeCluster(ctx, &eks.DescribeClusterInput{
			Name: aws.String(clusterName),
		})
		if err != nil {
			return nil, fmt.Errorf("DescribeCluster %s: %w", clusterName, err)
		}
		if out.Cluster == nil {
			return nil, fmt.Errorf("DescribeCluster %s returned nil Cluster", clusterName)
		}
		switch out.Cluster.Status {
		case ekstypes.ClusterStatusActive:
			return out.Cluster, nil
		case ekstypes.ClusterStatusFailed:
			return nil, fmt.Errorf("EKS cluster %s entered FAILED status", clusterName)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timeout waiting for EKS cluster %s to become ACTIVE (current: %s)", clusterName, out.Cluster.Status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(EKSPollInterval):
		}
	}
}

func waitForClusterDeleted(ctx context.Context, c *Clients, clusterName string) error {
	deadline := time.Now().Add(EKSPollTimeout)
	for {
		_, err := c.PhaseEKS.DescribeCluster(ctx, &eks.DescribeClusterInput{
			Name: aws.String(clusterName),
		})
		if err != nil {
			if awsmw.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("DescribeCluster %s: %w", clusterName, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for EKS cluster %s deletion", clusterName)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(EKSPollInterval):
		}
	}
}

func recordClusterOutputs(st *state.State, cl *ekstypes.Cluster) error {
	if cl.Name != nil {
		st.Set("EKS_CLUSTER_NAME", *cl.Name)
	}
	if cl.Arn != nil {
		st.Set("EKS_CLUSTER_ARN", *cl.Arn)
	}
	if cl.Endpoint != nil {
		st.Set("EKS_CLUSTER_ENDPOINT", *cl.Endpoint)
	}
	if cl.CertificateAuthority != nil && cl.CertificateAuthority.Data != nil {
		st.Set("EKS_CLUSTER_CA", *cl.CertificateAuthority.Data)
	}
	if cl.Identity != nil && cl.Identity.Oidc != nil && cl.Identity.Oidc.Issuer != nil {
		st.Set("EKS_OIDC_ISSUER", *cl.Identity.Oidc.Issuer)
	}
	if cl.ResourcesVpcConfig != nil && cl.ResourcesVpcConfig.ClusterSecurityGroupId != nil {
		st.Set("EKS_CLUSTER_SG_ID", *cl.ResourcesVpcConfig.ClusterSecurityGroupId)
	}
	if cl.KubernetesNetworkConfig != nil && cl.KubernetesNetworkConfig.ServiceIpv4Cidr != nil && *cl.KubernetesNetworkConfig.ServiceIpv4Cidr != "" {
		st.Set("EKS_SERVICE_CIDR", *cl.KubernetesNetworkConfig.ServiceIpv4Cidr)
		st.Set("SERVICE_CIDR", *cl.KubernetesNetworkConfig.ServiceIpv4Cidr)
	} else if st.Get("EKS_SERVICE_CIDR") == "" {
		st.Set("EKS_SERVICE_CIDR", "10.100.0.0/16")
	}
	return st.Save()
}

// Phase08EKSCluster creates (or re-uses) the EKS control plane, waits until it reaches ACTIVE, and records cluster outputs.
func Phase08EKSCluster(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 08 eks cluster: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 08 eks cluster: state is nil")
	}
	if dryRun {
		return nil
	}
	if c == nil || c.PhaseEKS == nil {
		return fmt.Errorf("phase 08 eks cluster: EKS client is nil")
	}

	roleARN := st.Get("EKS_CLUSTER_ROLE_ARN")
	if roleARN == "" {
		return fmt.Errorf("phase 08 eks cluster: EKS_CLUSTER_ROLE_ARN not found in state")
	}

	var subnetIDs []string
	for i := range cl.Network.Subnets.PublicList {
		id := st.Get(subnetStateKey("PUBLIC_SUBNET", i))
		if id == "" {
			return fmt.Errorf("phase 08 eks cluster: PUBLIC_SUBNET_%d not found in state", i)
		}
		subnetIDs = append(subnetIDs, id)
	}
	for i := range cl.Network.Subnets.PrivateList {
		id := st.Get(subnetStateKey("PRIVATE_SUBNET", i))
		if id == "" {
			return fmt.Errorf("phase 08 eks cluster: PRIVATE_SUBNET_%d not found in state", i)
		}
		subnetIDs = append(subnetIDs, id)
	}
	if len(subnetIDs) == 0 {
		return fmt.Errorf("phase 08 eks cluster: no control-plane subnets found in state")
	}

	clusterName := cl.Metadata.Name

	descOut, err := c.PhaseEKS.DescribeCluster(ctx, &eks.DescribeClusterInput{
		Name: aws.String(clusterName),
	})
	if err == nil && descOut.Cluster != nil {
		active, werr := waitForClusterActive(ctx, c, clusterName)
		if werr != nil {
			return fmt.Errorf("phase 08 eks cluster: %w", werr)
		}
		return recordClusterOutputs(st, active)
	}
	if err != nil && !awsmw.IsNotFound(err) {
		return fmt.Errorf("phase 08 eks cluster: DescribeCluster %s: %w", clusterName, err)
	}

	k8sVersion := ""
	publicCIDRs := []string{"0.0.0.0/0"}
	if cl.ClusterSpec != nil {
		k8sVersion = cl.ClusterSpec.KubernetesVersion
		if cl.ClusterSpec.EndpointAccess != nil && len(cl.ClusterSpec.EndpointAccess.PublicAccessCidrs) > 0 {
			publicCIDRs = cl.ClusterSpec.EndpointAccess.PublicAccessCidrs
		}
	}

	t := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, map[string]string{
		"Name": clusterName,
	})

	_, cerr := c.PhaseEKS.CreateCluster(ctx, &eks.CreateClusterInput{
		Name:    aws.String(clusterName),
		Version: aws.String(k8sVersion),
		RoleArn: aws.String(roleARN),
		ResourcesVpcConfig: &ekstypes.VpcConfigRequest{
			SubnetIds:             subnetIDs,
			EndpointPublicAccess:  aws.Bool(true),
			EndpointPrivateAccess: aws.Bool(true),
			PublicAccessCidrs:     publicCIDRs,
		},
		Tags: t,
	})
	if cerr != nil {
		return fmt.Errorf("phase 08 eks cluster: CreateCluster %s: %w", clusterName, cerr)
	}

	active, err := waitForClusterActive(ctx, c, clusterName)
	if err != nil {
		return fmt.Errorf("phase 08 eks cluster: %w", err)
	}
	return recordClusterOutputs(st, active)
}

// Phase08EKSClusterDown deletes the EKS control plane and waits for it to be removed.
func Phase08EKSClusterDown(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 08 eks cluster down: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 08 eks cluster down: state is nil")
	}
	if dryRun {
		return nil
	}
	if c == nil || c.PhaseEKS == nil {
		return fmt.Errorf("phase 08 eks cluster down: EKS client is nil")
	}

	clusterName := st.Get("EKS_CLUSTER_NAME")
	if clusterName == "" {
		clusterName = cl.Metadata.Name
	}

	deleteDeadline := time.Now().Add(EKSPollTimeout)
	for {
		_, err := c.PhaseEKS.DeleteCluster(ctx, &eks.DeleteClusterInput{
			Name: aws.String(clusterName),
		})
		if err == nil || awsmw.IsNotFound(err) {
			break
		}
		if !awsmw.IsDependencyViolation(err) || time.Now().After(deleteDeadline) {
			return fmt.Errorf("phase 08 eks cluster down: DeleteCluster %s: %w", clusterName, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("phase 08 eks cluster down: %w", ctx.Err())
		case <-time.After(EKSPollInterval):
		}
	}

	if err := waitForClusterDeleted(ctx, c, clusterName); err != nil {
		return fmt.Errorf("phase 08 eks cluster down: %w", err)
	}

	for _, k := range []string{
		"EKS_CLUSTER_NAME",
		"EKS_CLUSTER_ARN",
		"EKS_CLUSTER_ENDPOINT",
		"EKS_CLUSTER_CA",
		"EKS_OIDC_ISSUER",
		"EKS_OIDC_URL",
		"EKS_CA",
		"EKS_ENDPOINT",
		"EKS_VERSION",
		"EKS_CLUSTER_SG_ID",
		"EKS_SERVICE_CIDR",
		"SERVICE_CIDR",
	} {
		st.Delete(k)
	}
	return st.Save()
}
