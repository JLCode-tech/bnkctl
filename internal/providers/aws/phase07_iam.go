package aws

import (
	"context"
	"fmt"
	"strings"

	"github.com/JLCode-tech/bnkctl/internal/providers/aws/awsmw"
	"github.com/JLCode-tech/bnkctl/internal/providers/aws/tags"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

const eksClusterAssumeRolePolicy = `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": { "Service": "eks.amazonaws.com" },
      "Action": "sts:AssumeRole"
    }
  ]
}`

const ec2NodeAssumeRolePolicy = `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": { "Service": "ec2.amazonaws.com" },
      "Action": "sts:AssumeRole"
    }
  ]
}`

var clusterManagedPolicies = []string{
	"arn:aws:iam::aws:policy/AmazonEKSClusterPolicy",
	"arn:aws:iam::aws:policy/AmazonEKSVPCResourceController",
}

var nodeManagedPolicies = []string{
	"arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy",
	"arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy",
	"arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly",
	"arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore",
	"arn:aws:iam::aws:policy/service-role/AmazonEBSCSIDriverPolicy",
}

func isEntityAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	return awsmw.ErrorCode(err) == "EntityAlreadyExists" ||
		strings.Contains(err.Error(), "EntityAlreadyExists")
}

func toIAMTags(m map[string]string) []iamtypes.Tag {
	out := make([]iamtypes.Tag, 0, len(m))
	for _, kv := range tags.SortedPairs(m) {
		out = append(out, iamtypes.Tag{
			Key:   aws.String(kv.Key),
			Value: aws.String(kv.Value),
		})
	}
	return out
}

func ensureRole(
	ctx context.Context,
	c *Clients,
	st *state.State,
	cl *intent.Cluster,
	roleName string,
	assumePolicy string,
	managedPolicies []string,
	arnStateKey string,
	nameStateKey string,
) (string, error) {
	t := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, map[string]string{
		"Name": roleName,
	})

	var roleARN string
	getOut, err := c.PhaseIAM.GetRole(ctx, &iam.GetRoleInput{
		RoleName: aws.String(roleName),
	})
	if err == nil && getOut.Role != nil && getOut.Role.Arn != nil {
		roleARN = *getOut.Role.Arn
	} else if err != nil && !awsmw.IsNotFound(err) {
		return "", fmt.Errorf("GetRole %s: %w", roleName, err)
	}

	if roleARN == "" {
		createOut, cerr := c.PhaseIAM.CreateRole(ctx, &iam.CreateRoleInput{
			RoleName:                 aws.String(roleName),
			AssumeRolePolicyDocument: aws.String(assumePolicy),
			Tags:                     toIAMTags(t),
		})
		if cerr != nil {
			if isEntityAlreadyExists(cerr) {
				g, gerr := c.PhaseIAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(roleName)})
				if gerr != nil || g.Role == nil || g.Role.Arn == nil {
					return "", fmt.Errorf("GetRole after EntityAlreadyExists %s: %w", roleName, gerr)
				}
				roleARN = *g.Role.Arn
			} else {
				return "", fmt.Errorf("CreateRole %s: %w", roleName, cerr)
			}
		} else if createOut.Role != nil && createOut.Role.Arn != nil {
			roleARN = *createOut.Role.Arn
		}
	}

	if roleARN == "" {
		return "", fmt.Errorf("role %s has empty ARN", roleName)
	}

	for _, p := range managedPolicies {
		if _, err := c.PhaseIAM.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
			RoleName:  aws.String(roleName),
			PolicyArn: aws.String(p),
		}); err != nil {
			return "", fmt.Errorf("AttachRolePolicy %s -> %s: %w", p, roleName, err)
		}
	}

	st.Set(arnStateKey, roleARN)
	st.Set(nameStateKey, roleName)
	if err := st.Save(); err != nil {
		return "", err
	}
	return roleARN, nil
}

// Phase07IAM creates (or re-uses) the EKS cluster IAM role and the worker node IAM role.
func Phase07IAM(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 07 iam: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 07 iam: state is nil")
	}
	if dryRun {
		return nil
	}
	if c == nil || c.PhaseIAM == nil {
		return fmt.Errorf("phase 07 iam: IAM client is nil")
	}

	clusterRoleName := cl.Metadata.Name + "-eks-cluster-role"
	if _, err := ensureRole(
		ctx, c, st, cl,
		clusterRoleName,
		eksClusterAssumeRolePolicy,
		clusterManagedPolicies,
		"EKS_CLUSTER_ROLE_ARN",
		"EKS_CLUSTER_ROLE_NAME",
	); err != nil {
		return fmt.Errorf("phase 07 iam: %w", err)
	}

	nodePolicies := append([]string(nil), nodeManagedPolicies...)
	if cl.AI != nil && cl.AI.IsSageMakerHyperPod() {
		nodePolicies = append(nodePolicies,
			"arn:aws:iam::aws:policy/AmazonSageMakerClusterInstanceRolePolicy",
			"arn:aws:iam::aws:policy/AmazonS3FullAccess",
		)
	}

	nodeRoleName := cl.Metadata.Name + "-eks-node-role"
	if _, err := ensureRole(
		ctx, c, st, cl,
		nodeRoleName,
		ec2NodeAssumeRolePolicy,
		nodePolicies,
		"EKS_NODE_ROLE_ARN",
		"EKS_NODE_ROLE_NAME",
	); err != nil {
		return fmt.Errorf("phase 07 iam: %w", err)
	}

	return nil
}

func roleNameFromARN(arn string) string {
	arn = strings.TrimSpace(arn)
	if arn == "" {
		return ""
	}
	if idx := strings.LastIndex(arn, "/"); idx >= 0 {
		return arn[idx+1:]
	}
	return arn
}

func deleteRoleWithPolicies(ctx context.Context, c *Clients, roleName string) error {
	if roleName == "" {
		return nil
	}

	if profOut, perr := c.PhaseIAM.ListInstanceProfilesForRole(ctx, &iam.ListInstanceProfilesForRoleInput{
		RoleName: aws.String(roleName),
	}); perr == nil && profOut != nil {
		for _, ip := range profOut.InstanceProfiles {
			if ip.InstanceProfileName == nil {
				continue
			}
			_, _ = c.PhaseIAM.RemoveRoleFromInstanceProfile(ctx, &iam.RemoveRoleFromInstanceProfileInput{
				InstanceProfileName: ip.InstanceProfileName,
				RoleName:            aws.String(roleName),
			})
			_, _ = c.PhaseIAM.DeleteInstanceProfile(ctx, &iam.DeleteInstanceProfileInput{
				InstanceProfileName: ip.InstanceProfileName,
			})
		}
	}

	if inlineOut, ierr := c.PhaseIAM.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{
		RoleName: aws.String(roleName),
	}); ierr == nil && inlineOut != nil {
		for _, pName := range inlineOut.PolicyNames {
			_, derr := c.PhaseIAM.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{
				RoleName:   aws.String(roleName),
				PolicyName: aws.String(pName),
			})
			if derr != nil && !awsmw.IsNotFound(derr) {
				return fmt.Errorf("DeleteRolePolicy %s from %s: %w", pName, roleName, derr)
			}
		}
	}

	listOut, err := c.PhaseIAM.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{
		RoleName: aws.String(roleName),
	})
	if err != nil {
		if awsmw.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("ListAttachedRolePolicies %s: %w", roleName, err)
	}
	for _, ap := range listOut.AttachedPolicies {
		if ap.PolicyArn == nil {
			continue
		}
		_, derr := c.PhaseIAM.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{
			RoleName:  aws.String(roleName),
			PolicyArn: ap.PolicyArn,
		})
		if derr != nil && !awsmw.IsNotFound(derr) {
			return fmt.Errorf("DetachRolePolicy %s from %s: %w", *ap.PolicyArn, roleName, derr)
		}
	}

	_, err = c.PhaseIAM.DeleteRole(ctx, &iam.DeleteRoleInput{
		RoleName: aws.String(roleName),
	})
	if err != nil && !awsmw.IsNotFound(err) {
		return fmt.Errorf("DeleteRole %s: %w", roleName, err)
	}
	return nil
}

// Phase07IAMDown detaches managed/inline policies, removes instance profiles, and deletes the node and cluster IAM roles.
func Phase07IAMDown(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 07 iam down: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 07 iam down: state is nil")
	}
	if dryRun {
		return nil
	}
	if c == nil || c.PhaseIAM == nil {
		return fmt.Errorf("phase 07 iam down: IAM client is nil")
	}

	nodeRole := st.Get("EKS_NODE_ROLE_NAME")
	if nodeRole == "" {
		nodeRole = roleNameFromARN(st.Get("EKS_NODE_ROLE_ARN"))
	}
	if nodeRole == "" {
		nodeRole = cl.Metadata.Name + "-eks-node-role"
	}

	if ipName := st.Get("NODE_INSTANCE_PROFILE_NAME"); ipName != "" {
		_, _ = c.PhaseIAM.RemoveRoleFromInstanceProfile(ctx, &iam.RemoveRoleFromInstanceProfileInput{
			InstanceProfileName: aws.String(ipName),
			RoleName:            aws.String(nodeRole),
		})
		_, _ = c.PhaseIAM.DeleteInstanceProfile(ctx, &iam.DeleteInstanceProfileInput{
			InstanceProfileName: aws.String(ipName),
		})
		st.Delete("NODE_INSTANCE_PROFILE_NAME")
		st.Delete("NODE_INSTANCE_PROFILE_ARN")
	}

	if err := deleteRoleWithPolicies(ctx, c, nodeRole); err != nil {
		return fmt.Errorf("phase 07 iam down: %w", err)
	}
	st.Delete("EKS_NODE_ROLE_ARN")
	st.Delete("EKS_NODE_ROLE_NAME")

	clusterRole := st.Get("EKS_CLUSTER_ROLE_NAME")
	if clusterRole == "" {
		clusterRole = roleNameFromARN(st.Get("EKS_CLUSTER_ROLE_ARN"))
	}
	if clusterRole == "" {
		clusterRole = cl.Metadata.Name + "-eks-cluster-role"
	}
	if err := deleteRoleWithPolicies(ctx, c, clusterRole); err != nil {
		return fmt.Errorf("phase 07 iam down: %w", err)
	}
	st.Delete("EKS_CLUSTER_ROLE_ARN")
	st.Delete("EKS_CLUSTER_ROLE_NAME")

	return st.Save()
}
