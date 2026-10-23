package aws

import (
	"context"
	"crypto/sha1" // #nosec G505 -- AWS IAM OIDC thumbprint specification mandates SHA-1
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"

	"github.com/JLCode-tech/bnkctl/internal/providers/aws/awsmw"
	"github.com/JLCode-tech/bnkctl/internal/providers/aws/tags"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
)

const defaultEKSRootCAThumbprint = "9e99a48a9960b14926bb7f3b02e22da2b0ab7280"

// ThumbprintDialer fetches the SHA-1 thumbprint of the TLS root certificate for an HTTPS URL.
var ThumbprintDialer = fetchTLSThumbprint

func fetchTLSThumbprint(issuerURL string) (string, error) {
	u, err := url.Parse(issuerURL)
	if err != nil {
		return "", err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":443"
	}
	conn, err := tls.Dial("tcp", host, &tls.Config{MinVersion: tls.VersionTLS12})
	if err != nil {
		return defaultEKSRootCAThumbprint, nil
	}
	defer func() { _ = conn.Close() }()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return defaultEKSRootCAThumbprint, nil
	}
	root := certs[len(certs)-1]
	// #nosec G401 -- AWS IAM CreateOpenIDConnectProvider requires SHA-1 certificate thumbprint
	sum := sha1.Sum(root.Raw)
	return hex.EncodeToString(sum[:]), nil
}

const cneControllerPermissionsPolicy = `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:Describe*",
        "ec2:AssignPrivateIpAddresses",
        "ec2:UnassignPrivateIpAddresses",
        "ec2:AttachNetworkInterface",
        "ec2:DetachNetworkInterface",
        "ec2:ModifyNetworkInterfaceAttribute",
        "ec2:CreateRoute",
        "ec2:ReplaceRoute",
        "ec2:DeleteRoute"
      ],
      "Resource": "*"
    }
  ]
}`

func buildIRSATrustPolicy(oidcProviderARN, oidcHostPath string) string {
	return fmt.Sprintf(`{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": { "Federated": "%s" },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringLike": {
          "%s:sub": "system:serviceaccount:%s:*",
          "%s:aud": "sts.amazonaws.com"
        }
      }
    }
  ]
}`, oidcProviderARN, oidcHostPath, InstanceNamespace, oidcHostPath)
}

func oidcHostPath(issuerURL string) string {
	issuerURL = strings.TrimSpace(issuerURL)
	if issuerURL == "" {
		return ""
	}
	u, err := url.Parse(issuerURL)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Host+u.Path, "/")
}

func ensureOIDCProvider(ctx context.Context, c *Clients, st *state.State, cl *intent.Cluster, issuerURL string) (string, string, error) {
	u, err := url.Parse(issuerURL)
	if err != nil {
		return "", "", fmt.Errorf("parsing OIDC issuer URL %q: %w", issuerURL, err)
	}
	hostPath := strings.TrimPrefix(u.Host+u.Path, "/")

	listOut, err := c.PhaseIAM.ListOpenIDConnectProviders(ctx, &iam.ListOpenIDConnectProvidersInput{})
	if err != nil {
		return "", "", fmt.Errorf("ListOpenIDConnectProviders: %w", err)
	}
	for _, p := range listOut.OpenIDConnectProviderList {
		if p.Arn != nil && strings.HasSuffix(*p.Arn, hostPath) {
			st.Set("OIDC_PROVIDER_ARN", *p.Arn)
			_ = st.Save()
			return *p.Arn, hostPath, nil
		}
	}

	thumb, err := ThumbprintDialer(issuerURL)
	if err != nil || thumb == "" {
		thumb = defaultEKSRootCAThumbprint
	}

	t := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, map[string]string{
		"Name": cl.Metadata.Name + "-oidc",
	})
	createOut, err := c.PhaseIAM.CreateOpenIDConnectProvider(ctx, &iam.CreateOpenIDConnectProviderInput{
		Url:            aws.String(issuerURL),
		ClientIDList:   []string{"sts.amazonaws.com"},
		ThumbprintList: []string{thumb},
		Tags:           toIAMTags(t),
	})
	if err != nil {
		return "", "", fmt.Errorf("CreateOpenIDConnectProvider %s: %w", issuerURL, err)
	}
	if createOut.OpenIDConnectProviderArn == nil {
		return "", "", fmt.Errorf("CreateOpenIDConnectProvider returned nil ARN")
	}
	arn := *createOut.OpenIDConnectProviderArn
	st.Set("OIDC_PROVIDER_ARN", arn)
	if err := st.Save(); err != nil {
		return "", "", err
	}
	return arn, hostPath, nil
}

// Phase18IRSAOIDC registers the cluster OIDC provider in IAM and creates the IRSA IAM role and policy for the BNK CNE controller.
func Phase18IRSAOIDC(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 18 irsa oidc: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 18 irsa oidc: state is nil")
	}
	if !cl.HasExternalDataPath() || dryRun {
		return nil
	}
	if c == nil || c.PhaseIAM == nil {
		return fmt.Errorf("phase 18 irsa oidc: IAM client is nil")
	}

	issuer := st.Get("EKS_OIDC_ISSUER")
	if issuer == "" {
		return fmt.Errorf("phase 18 irsa oidc: EKS_OIDC_ISSUER not found in state")
	}

	oidcARN, hostPath, err := ensureOIDCProvider(ctx, c, st, cl, issuer)
	if err != nil {
		return fmt.Errorf("phase 18 irsa oidc: %w", err)
	}

	policyName := cl.Metadata.Name + "-cne-controller-policy"
	policyARN := st.Get("IRSA_POLICY_ARN")
	if policyARN == "" {
		t := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, map[string]string{
			"Name": policyName,
		})
		pOut, perr := c.PhaseIAM.CreatePolicy(ctx, &iam.CreatePolicyInput{
			PolicyName:     aws.String(policyName),
			PolicyDocument: aws.String(cneControllerPermissionsPolicy),
			Tags:           toIAMTags(t),
		})
		if perr != nil {
			if isEntityAlreadyExists(perr) {
				acct := st.Get("AWS_ACCOUNT_ID")
				if acct == "" {
					acct = "000000000000"
				}
				policyARN = fmt.Sprintf("arn:aws:iam::%s:policy/%s", acct, policyName)
			} else {
				return fmt.Errorf("phase 18 irsa oidc: CreatePolicy %s: %w", policyName, perr)
			}
		} else if pOut.Policy != nil && pOut.Policy.Arn != nil {
			policyARN = *pOut.Policy.Arn
		}
		st.Set("IRSA_POLICY_ARN", policyARN)
		if err := st.Save(); err != nil {
			return err
		}
	}

	roleName := cl.Metadata.Name + "-cne-controller-irsa"
	trustPolicy := buildIRSATrustPolicy(oidcARN, hostPath)
	roleARN, err := ensureRole(
		ctx, c, st, cl,
		roleName,
		trustPolicy,
		[]string{policyARN},
		"IRSA_ROLE_ARN",
		"IRSA_ROLE_NAME",
	)
	if err != nil {
		return fmt.Errorf("phase 18 irsa oidc: %w", err)
	}
	st.Set("CNE_IRSA_ROLE_ARN", roleARN)
	return st.Save()
}

// Phase18IRSAOIDCDown deletes the IRSA role, policy, and OIDC provider recorded in state.
func Phase18IRSAOIDCDown(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 18 irsa oidc down: cluster intent is nil")
	}
	if st == nil || dryRun {
		return nil
	}
	if c == nil || c.PhaseIAM == nil {
		return fmt.Errorf("phase 18 irsa oidc down: IAM client is nil")
	}

	roleName := st.Get("CNE_IRSA_ROLE_NAME")
	if roleName == "" {
		roleName = st.Get("IRSA_ROLE_NAME")
	}
	if roleName == "" {
		if arn := st.Get("CNE_IRSA_ROLE_ARN"); arn != "" {
			if idx := strings.LastIndex(arn, "/"); idx >= 0 {
				roleName = arn[idx+1:]
			}
		}
	}
	if roleName == "" {
		roleName = cl.Metadata.Name + "-cne-controller-irsa"
	}
	if err := deleteRoleWithPolicies(ctx, c, roleName); err != nil {
		return fmt.Errorf("phase 18 irsa oidc down: %w", err)
	}
	st.Delete("IRSA_ROLE_ARN")
	st.Delete("CNE_IRSA_ROLE_ARN")
	st.Delete("IRSA_ROLE_NAME")
	st.Delete("CNE_IRSA_ROLE_NAME")
	st.Delete("IRSA_TRUST_CONFIGURED_AT")

	if policyARN := st.Get("IRSA_POLICY_ARN"); policyARN != "" {
		_, err := c.PhaseIAM.DeletePolicy(ctx, &iam.DeletePolicyInput{
			PolicyArn: aws.String(policyARN),
		})
		if err != nil && !awsmw.IsNotFound(err) {
			return fmt.Errorf("phase 18 irsa oidc down: DeletePolicy %s: %w", policyARN, err)
		}
		st.Delete("IRSA_POLICY_ARN")
	}

	oidcARN := st.Get("OIDC_PROVIDER_ARN")
	if oidcARN == "" {
		issuerURL := st.Get("OIDC_ISSUER_URL")
		if issuerURL == "" {
			issuerURL = st.Get("EKS_OIDC_ISSUER")
		}
		if issuerURL == "" {
			issuerURL = st.Get("EKS_OIDC_URL")
		}
		if hostPath := oidcHostPath(issuerURL); hostPath != "" {
			if listOut, lerr := c.PhaseIAM.ListOpenIDConnectProviders(ctx, &iam.ListOpenIDConnectProvidersInput{}); lerr == nil && listOut != nil {
				for _, entry := range listOut.OpenIDConnectProviderList {
					if entry.Arn != nil && strings.HasSuffix(*entry.Arn, "/"+hostPath) {
						oidcARN = *entry.Arn
						break
					}
				}
			}
			if oidcARN == "" {
				if acct := extractAccountIDFromState(st); acct != "" {
					oidcARN = fmt.Sprintf("arn:aws:iam::%s:oidc-provider/%s", acct, hostPath)
				}
			}
		}
	}
	if oidcARN != "" {
		_, err := c.PhaseIAM.DeleteOpenIDConnectProvider(ctx, &iam.DeleteOpenIDConnectProviderInput{
			OpenIDConnectProviderArn: aws.String(oidcARN),
		})
		if err != nil && !awsmw.IsNotFound(err) {
			return fmt.Errorf("phase 18 irsa oidc down: DeleteOpenIDConnectProvider %s: %w", oidcARN, err)
		}
	}
	st.Delete("OIDC_PROVIDER_ARN")
	st.Delete("OIDC_ISSUER_URL")
	st.Delete("EKS_OIDC_URL")
	st.Delete("EKS_OIDC_ISSUER")
	st.Delete("IRSA_SA_APPLIED_AT")

	return st.Save()
}
