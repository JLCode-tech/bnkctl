package gcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"

	containerpb "cloud.google.com/go/container/apiv1/containerpb"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"k8s.io/client-go/rest"
)

// cloudPlatformScope is the OAuth2 scope requested for the ADC token source
// used to authenticate against the GKE API server. Matches the scope used to
// construct the GCP SDK clients (compute/container).
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// ShouldSkipAuth reports whether GKEBNKCTL_SKIP_AUTH=1 or BNKCTL_SKIP_AUTH=1 is set.
func ShouldSkipAuth() bool {
	return os.Getenv("GKEBNKCTL_SKIP_AUTH") == "1" || os.Getenv("BNKCTL_SKIP_AUTH") == "1"
}

// CheckCredentials verifies that Application Default Credentials are available.
func CheckCredentials(ctx context.Context) error {
	if _, err := google.FindDefaultCredentials(ctx, cloudPlatformScope); err != nil {
		return fmt.Errorf("application default credentials: %w", err)
	}
	return nil
}

// BuildRESTConfig constructs a Kubernetes rest.Config for a RUNNING GKE
// cluster directly from its container API descriptor + Application Default
// Credentials — no kubeconfig file, no exec-credential plugin, no
// gcloud/kubectl shellout. See docs/SPEC-go-tracer.md §5.
//
// WrapTransport (rather than a static BearerToken) wires an oauth2 transport
// so tokens auto-refresh across long-running waits — the CNEInstance/License
// polls in Task C can run 20+ minutes, well past a single access token's
// lifetime.
func BuildRESTConfig(ctx context.Context, cluster *containerpb.Cluster) (*rest.Config, error) {
	if cluster == nil {
		return nil, fmt.Errorf("gcp: BuildRESTConfig: cluster is nil")
	}
	if cluster.GetEndpoint() == "" {
		return nil, fmt.Errorf("gcp: BuildRESTConfig: cluster %q has no endpoint (not RUNNING yet?)", cluster.GetName())
	}
	caCert := cluster.GetMasterAuth().GetClusterCaCertificate()
	if caCert == "" {
		return nil, fmt.Errorf("gcp: BuildRESTConfig: cluster %q has no master auth CA certificate", cluster.GetName())
	}
	caData, err := base64.StdEncoding.DecodeString(caCert)
	if err != nil {
		return nil, fmt.Errorf("gcp: BuildRESTConfig: decode cluster CA certificate: %w", err)
	}

	ts, err := google.DefaultTokenSource(ctx, cloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("gcp: BuildRESTConfig: ADC token source: %w", err)
	}

	return &rest.Config{
		Host: "https://" + cluster.GetEndpoint(),
		TLSClientConfig: rest.TLSClientConfig{
			CAData: caData,
		},
		WrapTransport: func(rt http.RoundTripper) http.RoundTripper {
			return &oauth2.Transport{Source: ts, Base: rt}
		},
	}, nil
}
