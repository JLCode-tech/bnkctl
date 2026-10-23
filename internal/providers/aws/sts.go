package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// STSAPI is the subset of the sts.Client surface awsbnkctl exercises.
// Tests inject a fake; production code uses sts.NewFromConfig.
type STSAPI interface {
	GetCallerIdentity(ctx context.Context, in *sts.GetCallerIdentityInput, opts ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// CallerIdentity is the awsbnkctl-shaped projection of sts:GetCallerIdentity.
// Account is load-bearing for OIDC provider ARN derivation (IRSA).
type CallerIdentity struct {
	Account string
	ARN     string
	UserID  string
}

// CallerIdentity calls sts:GetCallerIdentity and projects the response.
//
// STS caller-identity is the cheapest "are credentials live?" probe
// and is used as the doctor pre-flight.
// AccessDenied here means the cred chain resolved a key but the key
// is rejected by AWS — distinct from "no credentials at all", which
// CredentialsConfigured catches earlier.
func (c *Clients) CallerIdentity(ctx context.Context) (*CallerIdentity, error) {
	if c == nil || c.STS == nil {
		return nil, fmt.Errorf("aws.Clients is nil")
	}
	out, err := c.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, fmt.Errorf("sts:GetCallerIdentity: %w", err)
	}
	return &CallerIdentity{
		Account: aws_string_or_empty(out.Account),
		ARN:     aws_string_or_empty(out.Arn),
		UserID:  aws_string_or_empty(out.UserId),
	}, nil
}

// aws_string_or_empty unwraps the SDK's *string fields into a plain
// string. Used pervasively for the projection structs.
func aws_string_or_empty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
