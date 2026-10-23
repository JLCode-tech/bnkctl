package aws

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

// EKSAPI is the subset of eks.Client awsbnkctl uses.
type EKSAPI interface {
	DescribeCluster(ctx context.Context, in *eks.DescribeClusterInput, opts ...func(*eks.Options)) (*eks.DescribeClusterOutput, error)
}

// ClusterInfo is the awsbnkctl-shaped projection of eks:DescribeCluster.
// Sufficient for kubeconfig generation + the doctor's "cluster reachable"
// check (which probes the API endpoint after this returns).
type ClusterInfo struct {
	Name                 string
	Endpoint             string
	CertificateAuthority string // base64
	OIDCIssuer           string
	Status               string
	Version              string
	Arn                  string
}

// DescribeCluster returns the cluster details for kubeconfig generation
// and post-apply verification. Wraps eks:DescribeCluster.
func (c *Clients) DescribeCluster(ctx context.Context, name string) (*ClusterInfo, error) {
	if c == nil || c.EKS == nil {
		return nil, fmt.Errorf("aws.Clients is nil")
	}
	if name == "" {
		return nil, errors.New("cluster name is empty")
	}
	out, err := c.EKS.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: &name})
	if err != nil {
		return nil, fmt.Errorf("eks:DescribeCluster %s: %w", name, err)
	}
	if out == nil || out.Cluster == nil {
		return nil, fmt.Errorf("eks:DescribeCluster %s: empty response", name)
	}
	ci := &ClusterInfo{
		Name:     aws_string_or_empty(out.Cluster.Name),
		Endpoint: aws_string_or_empty(out.Cluster.Endpoint),
		Status:   string(out.Cluster.Status),
		Version:  aws_string_or_empty(out.Cluster.Version),
		Arn:      aws_string_or_empty(out.Cluster.Arn),
	}
	if out.Cluster.CertificateAuthority != nil {
		ci.CertificateAuthority = aws_string_or_empty(out.Cluster.CertificateAuthority.Data)
	}
	if out.Cluster.Identity != nil && out.Cluster.Identity.Oidc != nil {
		ci.OIDCIssuer = aws_string_or_empty(out.Cluster.Identity.Oidc.Issuer)
	}
	return ci, nil
}

// IsResourceNotFound returns true when the supplied error is the
// EKS "cluster not found" condition. Used by the doctor's
// describe-cluster permission probe to distinguish a successful probe
// against a known-missing cluster (NotFound = creds + permission OK)
// from an actual access denial (AccessDenied = creds insufficient).
func IsResourceNotFound(err error) bool {
	if err == nil {
		return false
	}
	var rnf *ekstypes.ResourceNotFoundException
	if errors.As(err, &rnf) {
		return true
	}
	// SDK occasionally surfaces this as a wrapped operation error;
	// fall back to a substring check for the canonical error code.
	return strings.Contains(err.Error(), "ResourceNotFoundException")
}

// ----------------------------------------------------------------
// Kubeconfig generation (no shell-out to `aws eks update-kubeconfig`).
//
// Kubeconfig generation is performed in-process. EKS API
// authentication is a presigned URL for sts:GetCallerIdentity with an
// `x-k8s-aws-id` header carrying the cluster name; the kubelet client
// passes that URL as the bearer token, EKS resolves it back to an IAM
// identity, and maps it to a kube identity via the configured access
// entries.
//
// The presigning here uses aws-sdk-go-v2/aws/signer/v4 directly
// rather than the older sts.PresignClient — gives us control over the
// `X-K8s-Aws-Id` header (which must be signed) and the expiry (15
// minutes, EKS's max).
// ----------------------------------------------------------------

// EKSAuthTokenPrefix is the magic prefix EKS expects on the bearer
// token. See the aws-iam-authenticator reference implementation.
const EKSAuthTokenPrefix = "k8s-aws-v1." // #nosec G101 -- documented public EKS bearer-token prefix, not a credential

// PresignedSTSURL returns the presigned sts:GetCallerIdentity URL for
// the given cluster — the same value the bearer token is derived from.
//
// Exposed for testing the signing logic in isolation (the resulting
// URL's signature is what we pin in unit tests). Production callers
// use KubeconfigFromCluster, which wraps this.
func (c *Clients) PresignedSTSURL(ctx context.Context, clusterName string, expiry time.Duration) (string, error) {
	if c == nil {
		return "", fmt.Errorf("aws.Clients is nil")
	}
	if clusterName == "" {
		return "", errors.New("cluster name is empty")
	}
	if expiry <= 0 {
		expiry = 15 * time.Minute
	}
	region := c.Region
	if region == "" {
		region = c.AWSConfig.Region
	}
	if region == "" {
		return "", errors.New("aws.Clients region is empty")
	}

	creds, err := c.AWSConfig.Credentials.Retrieve(ctx)
	if err != nil {
		return "", fmt.Errorf("retrieving credentials: %w", err)
	}

	// Build the unsigned GET request: GetCallerIdentity action.
	// Per the v4 signer docs, X-Amz-Expires MUST be set on the URL
	// before signing — PresignHTTP doesn't auto-populate it.
	endpoint := fmt.Sprintf("https://sts.%s.amazonaws.com/?Action=GetCallerIdentity&Version=2011-06-15", region)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("building sts request: %w", err)
	}
	q := req.URL.Query()
	q.Set("X-Amz-Expires", strconv.FormatInt(int64(expiry/time.Second), 10))
	req.URL.RawQuery = q.Encode()

	// The cluster-name header MUST be in the signed-headers set —
	// EKS rejects tokens whose signature doesn't cover it.
	req.Header.Set("X-K8s-Aws-Id", clusterName)

	signer := v4.NewSigner()
	// EKS bearer tokens are presigned URLs — empty body, GET request,
	// sigv4 query-string variant. The empty-body hash is the
	// well-known constant for "no body".
	payloadHash := sha256OfEmpty()
	signedURL, _, err := signer.PresignHTTP(ctx, creds, req, payloadHash, "sts", region, time.Now())
	if err != nil {
		return "", fmt.Errorf("presigning sts:GetCallerIdentity: %w", err)
	}
	return signedURL, nil
}

// EKSAuthToken returns the bearer token suitable for use in a
// kubeconfig's user.token field. Format: "k8s-aws-v1." +
// base64-url-no-pad(presigned-url).
func (c *Clients) EKSAuthToken(ctx context.Context, clusterName string) (string, error) {
	url, err := c.PresignedSTSURL(ctx, clusterName, 15*time.Minute)
	if err != nil {
		return "", err
	}
	return EKSAuthTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(url)), nil
}

// KubeconfigFromCluster generates a kubeconfig YAML document for the
// given cluster. The user entry uses the `exec` plugin shape so kubectl
// re-acquires a fresh token on each invocation — matches the standard
// `aws eks update-kubeconfig` output but produced entirely in-process.
// If this breaks, no kubectl access works post-apply. Exec args follow
// the AWS
// CLI's `aws eks get-token` contract so existing kubectl + aws-cli
// stacks treat the kubeconfig identically.
func (c *Clients) KubeconfigFromCluster(ci *ClusterInfo) (string, error) {
	if ci == nil {
		return "", errors.New("ClusterInfo is nil")
	}
	if ci.Name == "" || ci.Endpoint == "" || ci.CertificateAuthority == "" {
		return "", errors.New("ClusterInfo is incomplete (need Name + Endpoint + CertificateAuthority)")
	}
	if c == nil {
		return "", errors.New("aws.Clients is nil")
	}
	region := c.Region
	if region == "" {
		return "", errors.New("aws.Clients region is empty")
	}

	// We use the `aws eks get-token` exec plugin shape — kubectl invokes
	// `aws` on each request, which gives users credential rotation for
	// free. An in-binary alternative (no aws CLI dep) lands in v0.x once
	// we publish an `awsbnkctl eks get-token` subcommand. Until then
	// this matches the shape `aws eks update-kubeconfig` produces, which
	// is what every EKS tutorial uses.
	yaml := fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: %s
clusters:
- cluster:
    server: %s
    certificate-authority-data: %s
  name: %s
contexts:
- context:
    cluster: %s
    user: %s
  name: %s
users:
- name: %s
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: aws
      args:
        - --region
        - %s
        - eks
        - get-token
        - --cluster-name
        - %s
      interactiveMode: IfAvailable
      provideClusterInfo: false
`,
		ci.Arn,
		ci.Endpoint, ci.CertificateAuthority, ci.Arn,
		ci.Arn, ci.Arn, ci.Arn,
		ci.Arn,
		region, ci.Name,
	)
	return yaml, nil
}

// --- helpers ---

func sha256OfEmpty() string {
	h := sha256.Sum256(nil)
	return hex.EncodeToString(h[:])
}

// EnsureRegion guards against the SDK's empty-region surprise:
// LoadDefaultConfig succeeds when neither AWS_REGION nor a profile is
// set, but the first API call then errors with "operation error … no
// region configured". The doctor pre-flight runs EnsureRegion to
// surface this earlier and with a friendlier message.
func EnsureRegion(c *Clients) error {
	if c == nil {
		return errors.New("aws.Clients is nil")
	}
	if c.Region == "" {
		return errors.New("AWS region is empty; set AWS_REGION or pass --region")
	}
	return nil
}

// silenceUnused keeps the awssdk import referenced for future extension
// points (e.g., explicit retryer config). Cheaper than a build-tagged
// stub file.
var _ = awssdk.Config{}
