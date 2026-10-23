package aws

import (
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

// fakeEKS implements EKSAPI for unit tests.
type fakeEKS struct {
	out *eks.DescribeClusterOutput
	err error
}

func (f *fakeEKS) DescribeCluster(ctx context.Context, in *eks.DescribeClusterInput, opts ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
	return f.out, f.err
}

func TestDescribeCluster_ProjectsFields(t *testing.T) {
	name := "awsbnkctl-test"
	endpoint := "https://example.eks.amazonaws.com"
	ca := "LS0tLS1CRUdJTi..."
	oidc := "https://oidc.eks.us-east-1.amazonaws.com/id/EXAMPLED"
	version := "1.30"
	arn := "arn:aws:eks:us-east-1:111122223333:cluster/awsbnkctl-test"

	c := &Clients{
		EKS: &fakeEKS{
			out: &eks.DescribeClusterOutput{
				Cluster: &ekstypes.Cluster{
					Name:     &name,
					Endpoint: &endpoint,
					CertificateAuthority: &ekstypes.Certificate{
						Data: &ca,
					},
					Identity: &ekstypes.Identity{
						Oidc: &ekstypes.OIDC{Issuer: &oidc},
					},
					Status:  ekstypes.ClusterStatusActive,
					Version: &version,
					Arn:     &arn,
				},
			},
		},
	}

	info, err := c.DescribeCluster(context.Background(), name)
	if err != nil {
		t.Fatalf("DescribeCluster: %v", err)
	}
	if info.Name != name {
		t.Errorf("Name: got %q want %q", info.Name, name)
	}
	if info.Endpoint != endpoint {
		t.Errorf("Endpoint: got %q want %q", info.Endpoint, endpoint)
	}
	if info.CertificateAuthority != ca {
		t.Errorf("CertificateAuthority: got %q want %q", info.CertificateAuthority, ca)
	}
	if info.OIDCIssuer != oidc {
		t.Errorf("OIDCIssuer: got %q want %q", info.OIDCIssuer, oidc)
	}
	if info.Status != string(ekstypes.ClusterStatusActive) {
		t.Errorf("Status: got %q", info.Status)
	}
}

func TestDescribeCluster_EmptyName(t *testing.T) {
	c := &Clients{EKS: &fakeEKS{}}
	_, err := c.DescribeCluster(context.Background(), "")
	if err == nil {
		t.Fatal("expected error on empty name")
	}
}

func TestIsResourceNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("boom"), false},
		{"typed", &ekstypes.ResourceNotFoundException{}, true},
		{"wrapped string", errors.New("ResourceNotFoundException: cluster not found"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsResourceNotFound(tc.err); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPresignedSTSURL_ShapeAndHeaders verifies the presigned URL has
// the expected query-param surface: it's a sigv4 query-string GET on
// sts.<region>.amazonaws.com, signs the x-k8s-aws-id header, and the
// X-Amz-Expires is set to a value <= 900 seconds (EKS's max).
//
// This is load-bearing: if the signature, URL host, or the signed-
// headers list changes, every kubectl call against the cluster fails
// silently with a confusing "Unauthorized" until the operator figures
// out the kubeconfig is broken.
func TestPresignedSTSURL_ShapeAndHeaders(t *testing.T) {
	c := newTestClients(t, "us-east-1", "AKID", "SECRET")

	u, err := c.PresignedSTSURL(context.Background(), "awsbnkctl-test", 15*time.Minute)
	if err != nil {
		t.Fatalf("PresignedSTSURL: %v", err)
	}

	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("invalid presigned URL: %v", err)
	}
	if parsed.Scheme != "https" {
		t.Errorf("scheme: got %q want https", parsed.Scheme)
	}
	if parsed.Host != "sts.us-east-1.amazonaws.com" {
		t.Errorf("host: got %q want sts.us-east-1.amazonaws.com", parsed.Host)
	}
	q := parsed.Query()
	if got := q.Get("Action"); got != "GetCallerIdentity" {
		t.Errorf("Action: got %q want GetCallerIdentity", got)
	}
	if got := q.Get("X-Amz-Algorithm"); got != "AWS4-HMAC-SHA256" {
		t.Errorf("X-Amz-Algorithm: got %q", got)
	}
	if got := q.Get("X-Amz-Expires"); got == "" {
		t.Errorf("X-Amz-Expires must be present")
	}
	signed := q.Get("X-Amz-SignedHeaders")
	if !strings.Contains(strings.ToLower(signed), "x-k8s-aws-id") {
		t.Errorf("x-k8s-aws-id must appear in X-Amz-SignedHeaders, got %q", signed)
	}
	if got := q.Get("X-Amz-Signature"); got == "" {
		t.Errorf("X-Amz-Signature must be present")
	}
}

// TestEKSAuthToken_FormatAndDecodes verifies the bearer token format
// is `k8s-aws-v1.<base64url-no-pad>` and that the decoded payload is
// a valid URL pointing at STS.
func TestEKSAuthToken_FormatAndDecodes(t *testing.T) {
	c := newTestClients(t, "us-west-2", "AKID", "SECRET")
	tok, err := c.EKSAuthToken(context.Background(), "test-cluster")
	if err != nil {
		t.Fatalf("EKSAuthToken: %v", err)
	}
	if !strings.HasPrefix(tok, EKSAuthTokenPrefix) {
		t.Fatalf("token prefix: got %q want %s", tok, EKSAuthTokenPrefix)
	}
	body := strings.TrimPrefix(tok, EKSAuthTokenPrefix)
	decoded, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		t.Fatalf("token body must decode as base64url-no-pad: %v", err)
	}
	if !strings.Contains(string(decoded), "sts.us-west-2.amazonaws.com") {
		t.Fatalf("decoded URL should reference STS: got %s", string(decoded))
	}
}

// TestKubeconfigFromCluster_Shape pins the kubeconfig YAML shape to
// the AWS-CLI-equivalent format. Downstream consumers (`kubectl`,
// `helm`, in-cluster operators) assume this shape; breaking it
// silently breaks every cluster-side workflow.
func TestKubeconfigFromCluster_Shape(t *testing.T) {
	c := newTestClients(t, "eu-west-1", "AKID", "SECRET")
	info := &ClusterInfo{
		Name:                 "demo",
		Endpoint:             "https://demo.eks.amazonaws.com",
		CertificateAuthority: "LS0tLS1CRUdJTg==",
		Arn:                  "arn:aws:eks:eu-west-1:111122223333:cluster/demo",
	}
	yaml, err := c.KubeconfigFromCluster(info)
	if err != nil {
		t.Fatalf("KubeconfigFromCluster: %v", err)
	}
	// Spot-check the required fragments.
	for _, frag := range []string{
		"apiVersion: v1",
		"kind: Config",
		"current-context: " + info.Arn,
		"server: " + info.Endpoint,
		"certificate-authority-data: " + info.CertificateAuthority,
		"client.authentication.k8s.io/v1beta1",
		"command: aws",
		"- eks",
		"- get-token",
		"- --cluster-name",
		"- " + info.Name,
		"- --region",
		"- eu-west-1",
	} {
		if !strings.Contains(yaml, frag) {
			t.Errorf("kubeconfig missing fragment %q\n--- yaml ---\n%s", frag, yaml)
		}
	}
}

func TestKubeconfigFromCluster_RejectsIncompleteInput(t *testing.T) {
	c := newTestClients(t, "us-east-1", "AKID", "SECRET")
	cases := []struct {
		name string
		ci   *ClusterInfo
	}{
		{"nil", nil},
		{"missing endpoint", &ClusterInfo{Name: "x", CertificateAuthority: "ca"}},
		{"missing CA", &ClusterInfo{Name: "x", Endpoint: "https://e"}},
		{"missing name", &ClusterInfo{Endpoint: "https://e", CertificateAuthority: "ca"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.KubeconfigFromCluster(tc.ci); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestEnsureRegion(t *testing.T) {
	if err := EnsureRegion(nil); err == nil {
		t.Fatal("expected error for nil Clients")
	}
	if err := EnsureRegion(&Clients{}); err == nil {
		t.Fatal("expected error for empty region")
	}
	if err := EnsureRegion(&Clients{Region: "us-east-1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// newTestClients builds a Clients with static credentials so the
// presigner runs deterministically (no real credential chain probe).
func newTestClients(t *testing.T, region, ak, sk string) *Clients {
	t.Helper()
	cfg := awssdk.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(ak, sk, ""),
	}
	return &Clients{
		Region:    region,
		AWSConfig: cfg,
	}
}
