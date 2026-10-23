package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

// IAMAPI is the subset of iam.Client surface awsbnkctl uses. It covers
// the OIDC-provider lookup (equivalent to what a Terraform data source
// would do, now expressed directly in Go via the AWS SDK) + the IRSA
// role existence probe (for the doctor row).
type IAMAPI interface {
	GetOpenIDConnectProvider(ctx context.Context, in *iam.GetOpenIDConnectProviderInput, opts ...func(*iam.Options)) (*iam.GetOpenIDConnectProviderOutput, error)
	GetRole(ctx context.Context, in *iam.GetRoleInput, opts ...func(*iam.Options)) (*iam.GetRoleOutput, error)
}

// EnsureIAM constructs a real iam.Client off the resolved aws.Config
// and caches it on the Clients struct (mirrors EnsureS3). Idempotent.
func (c *Clients) EnsureIAM() (IAMAPI, error) {
	if c == nil {
		return nil, fmt.Errorf("aws.Clients is nil")
	}
	if c.iam != nil {
		return c.iam, nil
	}
	if c.AWSConfig.Region == "" && c.Region == "" {
		return nil, errors.New("aws.Clients region is empty; cannot construct IAM client")
	}
	c.iam = iam.NewFromConfig(c.AWSConfig)
	return c.iam, nil
}

// OIDCProviderInfo is the awsbnkctl-shaped projection of
// iam:GetOpenIDConnectProvider. The URL field surfaces the issuer
// (with no scheme — IAM strips https://); ClientIDs are the audiences
// the provider trusts (typically just "sts.amazonaws.com" for EKS
// IRSA). The Go-SDK IAM phase uses both at trust-policy composition
// time.
type OIDCProviderInfo struct {
	ARN       string
	URL       string
	ClientIDs []string
	Tags      map[string]string
}

// GetOIDCProvider calls iam:GetOpenIDConnectProvider. The ARN format is
// arn:aws:iam::<account>:oidc-provider/<host>/<id> — for EKS the host
// is `oidc.eks.<region>.amazonaws.com` and the id is the cluster's
// OIDC suffix. The Go-SDK cluster phase surfaces this ARN as the
// oidc_provider_arn cluster output.
func (c *Clients) GetOIDCProvider(ctx context.Context, arn string) (*OIDCProviderInfo, error) {
	if arn == "" {
		return nil, errors.New("OIDC provider ARN is empty")
	}
	cli, err := c.EnsureIAM()
	if err != nil {
		return nil, err
	}
	out, err := cli.GetOpenIDConnectProvider(ctx, &iam.GetOpenIDConnectProviderInput{
		OpenIDConnectProviderArn: &arn,
	})
	if err != nil {
		return nil, fmt.Errorf("iam:GetOpenIDConnectProvider %s: %w", arn, err)
	}
	info := &OIDCProviderInfo{
		ARN:       arn,
		URL:       aws_string_or_empty(out.Url),
		ClientIDs: append([]string{}, out.ClientIDList...),
	}
	if len(out.Tags) > 0 {
		info.Tags = make(map[string]string, len(out.Tags))
		for _, t := range out.Tags {
			info.Tags[aws_string_or_empty(t.Key)] = aws_string_or_empty(t.Value)
		}
	}
	return info, nil
}

// RoleInfo is the awsbnkctl-shaped projection of iam:GetRole sufficient
// for the doctor's "FLO IRSA role exists" probe.
type RoleInfo struct {
	RoleName string
	ARN      string
	Path     string
}

// HasIRSARole probes whether the named role exists in the caller's
// account. Returns (info, nil) on success, (nil, nil) when the role
// doesn't exist yet (the typical pre-`awsbnkctl up` state), and a
// wrapped error otherwise.
//
// The doctor row treats "role doesn't exist" as informational
// (acceptable pre-apply) rather than failing — full IRSA-role
// reconciliation is performed by the Go-SDK IAM phase during
// `awsbnkctl up`.
func (c *Clients) HasIRSARole(ctx context.Context, roleName string) (*RoleInfo, error) {
	if roleName == "" {
		return nil, errors.New("role name is empty")
	}
	cli, err := c.EnsureIAM()
	if err != nil {
		return nil, err
	}
	out, err := cli.GetRole(ctx, &iam.GetRoleInput{RoleName: &roleName})
	if err != nil {
		if IsIAMNoSuchEntity(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("iam:GetRole %s: %w", roleName, err)
	}
	if out == nil || out.Role == nil {
		return nil, fmt.Errorf("iam:GetRole %s: empty response", roleName)
	}
	return &RoleInfo{
		RoleName: aws_string_or_empty(out.Role.RoleName),
		ARN:      aws_string_or_empty(out.Role.Arn),
		Path:     aws_string_or_empty(out.Role.Path),
	}, nil
}

// IsIAMNoSuchEntity returns true when err is the IAM NoSuchEntity
// (404-equivalent) condition.
func IsIAMNoSuchEntity(err error) bool {
	if err == nil {
		return false
	}
	var nse *iamtypes.NoSuchEntityException
	if errors.As(err, &nse) {
		return true
	}
	return strings.Contains(err.Error(), "NoSuchEntity")
}

// IRSARoleNameForCluster derives the IAM role name the Go-SDK IAM phase
// creates from the cluster name, following the
// "<cluster>-flo-supply-chain-reader" naming convention. Callers can
// override via workspace config; most invocations will exercise this
// default.
//
// Pinned in iam.go so the doctor row can derive the expected name
// without requiring caller knowledge of the naming convention.
func IRSARoleNameForCluster(clusterName string) string {
	return "awsbnkctl-" + clusterName + "-flo-supply-chain-reader"
}
