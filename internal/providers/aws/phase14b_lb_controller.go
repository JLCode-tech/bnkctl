package aws

import (
	"context"
	"fmt"
	"strings"

	"github.com/JLCode-tech/bnkctl/internal/providers/aws/awsmw"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/k8s"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	lbControllerReleaseName = "aws-load-balancer-controller"
	lbControllerNamespace   = "kube-system"
)

// Phase14bLBControllerDown cleans up the AWS Load Balancer Controller Helm release,
// any Service of type LoadBalancer in the cluster, the subnet elb tag, and the
// LB controller IRSA IAM role and policy.
func Phase14bLBControllerDown(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if st == nil || dryRun || c == nil {
		return nil
	}

	if c.K8s != nil {
		if svcs, err := c.K8s.CoreV1().Services(metav1.NamespaceAll).List(ctx, metav1.ListOptions{}); err == nil && svcs != nil {
			for _, svc := range svcs.Items {
				if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
					_ = c.K8s.CoreV1().Services(svc.Namespace).Delete(ctx, svc.Name, metav1.DeleteOptions{})
				}
			}
		}
	}

	kc := c.KubeconfigPath
	if kc == "" {
		kc = st.KubeconfigPath()
	}
	if kc != "" {
		relName := st.Get("LB_CONTROLLER_RELEASE_NAME")
		if relName == "" {
			relName = lbControllerReleaseName
		}
		if installer, err := k8s.NewHelmInstallerForNamespace(kc, lbControllerNamespace, ""); err == nil {
			_ = installer.Uninstall(relName, lbControllerNamespace)
		}
	}

	if c.PhaseEC2 != nil {
		extSubnet := st.Get("BNK_EXT_SUBNET")
		if extSubnet == "" {
			extSubnet = st.Get("SUBNET_EXT_ID")
		}
		if extSubnet != "" {
			_, _ = c.PhaseEC2.DeleteTags(ctx, &ec2.DeleteTagsInput{
				Resources: []string{extSubnet},
				Tags: []ec2types.Tag{
					{Key: aws.String("kubernetes.io/role/elb")},
				},
			})
		}
	}

	if c.PhaseIAM != nil {
		var roleName string
		if roleARN := st.Get("LB_CONTROLLER_IAM_ROLE_ARN"); roleARN != "" {
			if idx := strings.LastIndex(roleARN, "/"); idx >= 0 {
				roleName = roleARN[idx+1:]
			} else {
				roleName = roleARN
			}
		}
		if roleName == "" && cl != nil && cl.Metadata.Name != "" {
			roleName = cl.Metadata.Name + "-lb-controller-irsa"
		}
		if roleName != "" {
			if err := deleteRoleWithPolicies(ctx, c, roleName); err != nil {
				return fmt.Errorf("phase 14b lb controller down: %w", err)
			}
		}

		policyARN := st.Get("LB_CONTROLLER_POLICY_ARN")
		if policyARN == "" && cl != nil && cl.Metadata.Name != "" {
			if acct := st.Get("AWS_ACCOUNT_ID"); acct != "" {
				policyARN = fmt.Sprintf("arn:aws:iam::%s:policy/%s-lb-controller-iam-policy", acct, cl.Metadata.Name)
			}
		}
		if policyARN != "" {
			_, err := c.PhaseIAM.DeletePolicy(ctx, &iam.DeletePolicyInput{
				PolicyArn: aws.String(policyARN),
			})
			if err != nil && !awsmw.IsNotFound(err) {
				return fmt.Errorf("phase 14b lb controller down: DeletePolicy %s: %w", policyARN, err)
			}
		}
	}

	st.Delete("LB_CONTROLLER_IAM_ROLE_ARN")
	st.Delete("LB_CONTROLLER_POLICY_ARN")
	st.Delete("LB_CONTROLLER_RELEASE_NAME")
	st.Delete("LB_CONTROLLER_VERSION")
	st.Delete("LB_CONTROLLER_INSTALLED_AT")
	return st.Save()
}
