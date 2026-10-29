// Package aws wraps aws-sdk-go-v2 for awsbnkctl's cloud-side surface.
//
// One Clients struct per CLI invocation; constructed via NewClients with
// either a load-from-default-chain helper or an explicit region/profile.
// Tests inject mocks via the per-service interfaces (STSAPI, EC2API,
// EKSAPI, VPCAPI, PhaseEC2API, PhaseEKSAPI, PhaseIAMAPI) — see *_test.go
// files for the patterns.
//
// The load-bearing contract: never log credentials. SDK constructors take
// `context.Context` so cancellation propagates from the cobra command
// surface; long-running listing calls (DescribeInstanceTypes etc.) honour
// it.
package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sagemaker"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

// ErrRegionEmpty is returned by NewClients when the credential chain
// resolves no region at all. Callers that want "AWS simply isn't
// configured here" to be a benign state (e.g. the doctor's
// green-by-default contract on a stock dev box) match it with
// errors.Is; every other construction failure means broken config.
var ErrRegionEmpty = errors.New("AWS region is empty; set AWS_REGION, --region, or configure a profile")

// ShouldSkipAuth reports whether live AWS authentication should be bypassed.
func ShouldSkipAuth() bool {
	return os.Getenv("BNKCTL_SKIP_AUTH") == "1" || os.Getenv("AWSBNKCTL_SKIP_AUTH") == "1"
}

// Clients bundles the per-service SDK handles awsbnkctl needs.
//
// Each field is an interface (defined alongside its consumer file —
// sts.go, ec2.go, eks.go, vpc.go, phase00_preflight.go) so tests inject a
// fake without requiring the real network-aware client. NewClients constructs
// the real clients via the standard credential resolution chain.
type Clients struct {
	// Region is the AWS region the clients were constructed for. Echoed
	// back in error messages so users debugging cross-region issues see
	// which region the SDK landed on.
	Region string

	// Profile is the shared config profile name resolved for this client,
	// if any. Used when generating kubeconfig exec entries so kubectl
	// inherits the same profile even in shells without AWS_PROFILE exported.
	Profile string

	// AWSConfig is the resolved aws-sdk-go-v2 Config — exposed so callers
	// that need a sub-client awsbnkctl doesn't wrap (e.g., S3)
	// can construct one without re-resolving credentials.
	AWSConfig awssdk.Config

	STS STSAPI
	EC2 EC2API
	EKS EKSAPI
	VPC VPCAPI

	Auth           *AuthValidator
	PhaseEC2       PhaseEC2API
	PhaseEKS       PhaseEKSAPI
	PhaseIAM       PhaseIAMAPI
	PhaseS3        PhaseS3API
	SSM            SSMAPI
	SageMaker      SageMakerAPI
	AutoScaling    AutoScalingAPI
	K8s            kubernetes.Interface
	Dynamic        dynamic.Interface
	RESTMapper     meta.RESTMapper
	Execer         PodExecer
	KubeconfigPath string
	RESTConfig     *rest.Config

	// s3 / iam are lazily constructed on first use via EnsureS3 /
	// EnsureIAM. This keeps NewClients's cost low for the verbs that
	// don't touch S3 or IAM (most reads). Tests inject fakes by setting
	// these directly before invoking the consumer method.
	s3  S3API
	iam IAMAPI

	// serviceQuotas is the optional probe surface — lazily constructed
	// via EnsureServiceQuotas. Only the doctor's feature-flagged Service
	// Quotas row touches this, so the SDK's import cost is kept off
	// every other verb.
	serviceQuotas ServiceQuotasAPI
}

// AttachK8s builds Kubernetes typed, dynamic, discovery, and pod exec clients
// from the given kubeconfig file path.
func (c *Clients) AttachK8s(kubeconfigPath string) error {
	if c == nil {
		return fmt.Errorf("aws.Clients is nil")
	}
	rc, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return fmt.Errorf("building rest.Config from %s: %w", kubeconfigPath, err)
	}
	k8sClient, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return fmt.Errorf("building kubernetes client: %w", err)
	}
	dynClient, err := dynamic.NewForConfig(rc)
	if err != nil {
		return fmt.Errorf("building dynamic client: %w", err)
	}
	discClient, err := discovery.NewDiscoveryClientForConfig(rc)
	if err != nil {
		return fmt.Errorf("building discovery client: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discClient))
	c.K8s = k8sClient
	c.Dynamic = dynClient
	c.RESTMapper = mapper
	c.RESTConfig = rc
	c.KubeconfigPath = kubeconfigPath
	c.Execer = &DefaultPodExecer{
		K8s:        k8sClient,
		RESTConfig: rc,
	}
	return nil
}

// SetS3ForTest lets tests inject a fake S3API. Production callers go
// through EnsureS3; this hook keeps test files independent of the
// (private) field name.
func (c *Clients) SetS3ForTest(api S3API) { c.s3 = api }

// SetIAMForTest mirrors SetS3ForTest for the IAM client.
func (c *Clients) SetIAMForTest(api IAMAPI) { c.iam = api }

// Options configures NewClients. Empty Options uses the standard chain:
// env vars, shared config (~/.aws/config + ~/.aws/credentials), profile
// (AWS_PROFILE), EC2 instance role / ECS task role, SSO.
type Options struct {
	// Region overrides AWS_REGION / shared-config region. Empty = use
	// the resolved chain's default. Region is a required CLI input —
	// callers normally pass it through.
	Region string

	// Profile overrides AWS_PROFILE. Empty = whatever the chain
	// resolves. Used by the workspace-driven init flow to pin a
	// specific profile per workspace.
	Profile string
}

// NewClients constructs Clients using the standard aws-sdk-go-v2 chain.
//
// Returns a clear error when credentials cannot be resolved at all
// (no env, no profile, no instance role) — the doctor command's
// caller-identity check surfaces this with actionable remediation.
func NewClients(ctx context.Context, opts Options) (*Clients, error) {
	loadOpts := []func(*config.LoadOptions) error{}
	if opts.Region != "" {
		loadOpts = append(loadOpts, config.WithRegion(opts.Region))
	}
	if opts.Profile != "" {
		loadOpts = append(loadOpts, config.WithSharedConfigProfile(opts.Profile))
	}

	cfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	if cfg.Region == "" {
		return nil, ErrRegionEmpty
	}
	if cfg.Credentials != nil {
		cfg.Credentials = awssdk.NewCredentialsCache(&cliCacheFallbackProvider{primary: cfg.Credentials})
	}

	resolvedProfile := strings.TrimSpace(opts.Profile)
	if resolvedProfile == "" {
		resolvedProfile = strings.TrimSpace(os.Getenv("AWS_PROFILE"))
	}

	stsClient := sts.NewFromConfig(cfg)
	ec2Client := ec2.NewFromConfig(cfg)
	eksClient := eks.NewFromConfig(cfg)
	iamClient := iam.NewFromConfig(cfg)
	s3Client := s3.NewFromConfig(cfg)

	return &Clients{
		Region:      cfg.Region,
		Profile:     resolvedProfile,
		AWSConfig:   cfg,
		STS:         stsClient,
		Auth:        NewAuthValidator(stsClient),
		EC2:         ec2Client,
		PhaseEC2:    ec2Client,
		EKS:         eksClient,
		PhaseEKS:    eksClient,
		VPC:         ec2Client, // VPC API shares the EC2 client
		PhaseIAM:    iamClient,
		PhaseS3:     s3Client,
		SSM:         ssm.NewFromConfig(cfg),
		SageMaker:   sagemaker.NewFromConfig(cfg),
		AutoScaling: autoscaling.NewFromConfig(cfg),
	}, nil
}

// CredentialsConfigured is a fast probe answering "does the standard
// chain resolve to non-empty credentials?" without making a network
// call. Returns the source string (e.g. "Environment", "SharedConfig")
// when credentials resolve. Used by doctor to distinguish "no creds
// configured" from "creds configured but STS rejected them".
func CredentialsConfigured(ctx context.Context, opts Options) (string, error) {
	cfg, err := config.LoadDefaultConfig(ctx,
		buildLoadOpts(opts)...)
	if err != nil {
		return "", fmt.Errorf("loading AWS config: %w", err)
	}
	provider := cfg.Credentials
	if provider != nil {
		provider = &cliCacheFallbackProvider{primary: provider}
	}
	creds, err := provider.Retrieve(ctx)
	if err != nil {
		return "", err
	}
	if creds.AccessKeyID == "" {
		return "", errors.New("credential source returned empty AccessKeyID")
	}
	return creds.Source, nil
}

// cliCacheFallbackProvider wraps an aws.CredentialsProvider and falls back
// to reading unexpired STS role session credentials from ~/.aws/cli/cache/*.json
// when the primary provider fails (for example, when a 1-hour AWS SSO OIDC
// access token in ~/.aws/sso/cache expires or its single-use refreshToken is
// rotated by concurrent AWS CLI / kubectl exec invocations while ~/.aws/cli/cache
// still holds valid multi-hour STS role credentials).
type cliCacheFallbackProvider struct {
	primary awssdk.CredentialsProvider
}

func (p *cliCacheFallbackProvider) Retrieve(ctx context.Context) (awssdk.Credentials, error) {
	creds, err := p.primary.Retrieve(ctx)
	if err == nil && creds.AccessKeyID != "" {
		return creds, nil
	}
	if fallback, fbErr := retrieveFromAWSCLICache(); fbErr == nil {
		return fallback, nil
	}
	return creds, err
}

func retrieveFromAWSCLICache() (awssdk.Credentials, error) {
	cacheDir := strings.TrimSpace(os.Getenv("AWS_CLI_CACHE_DIR"))
	if cacheDir == "" {
		if testing.Testing() && os.Getenv("BNKCTL_FORCE_LIVE_AUTH") != "1" {
			return awssdk.Credentials{}, errors.New("skipping AWS CLI cache in unit test")
		}
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return awssdk.Credentials{}, errors.New("cannot resolve user home dir for AWS CLI cache")
		}
		cacheDir = filepath.Join(home, ".aws", "cli", "cache")
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return awssdk.Credentials{}, err
	}
	var best awssdk.Credentials
	var bestExp time.Time
	now := time.Now().Add(1 * time.Minute)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(cacheDir, entry.Name())) // #nosec G304 -- standard AWS CLI cache directory
		if err != nil {
			continue
		}
		var payload struct {
			Credentials struct {
				AccessKeyID     string `json:"AccessKeyId"`
				SecretAccessKey string `json:"SecretAccessKey"`
				SessionToken    string `json:"SessionToken"`
				Expiration      string `json:"Expiration"`
			} `json:"Credentials"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			continue
		}
		c := payload.Credentials
		if c.AccessKeyID == "" || c.SecretAccessKey == "" || c.Expiration == "" {
			continue
		}
		exp, err := time.Parse(time.RFC3339, c.Expiration)
		if err != nil {
			continue
		}
		if exp.After(now) && exp.After(bestExp) {
			bestExp = exp
			best = awssdk.Credentials{
				AccessKeyID:     c.AccessKeyID,
				SecretAccessKey: c.SecretAccessKey,
				SessionToken:    c.SessionToken,
				Source:          "AWSCLICache",
				CanExpire:       true,
				Expires:         exp,
			}
		}
	}
	if best.AccessKeyID == "" {
		return awssdk.Credentials{}, errors.New("no valid unexpired credentials in AWS CLI cache")
	}
	return best, nil
}

// buildLoadOpts mirrors NewClients's chain so CredentialsConfigured
// resolves against the exact same view of env / profile the real
// clients will use.
func buildLoadOpts(opts Options) []func(*config.LoadOptions) error {
	out := []func(*config.LoadOptions) error{}
	if opts.Region != "" {
		out = append(out, config.WithRegion(opts.Region))
	}
	if opts.Profile != "" {
		out = append(out, config.WithSharedConfigProfile(opts.Profile))
	}
	return out
}

// HasEnvCredentials reports whether AWS_ACCESS_KEY_ID is set in the
// environment. Used by doctor for the "AWS credentials not detected"
// pre-flight message — surfaces the most-common-case answer without
// needing a context.
func HasEnvCredentials() bool {
	return os.Getenv("AWS_ACCESS_KEY_ID") != "" || os.Getenv("AWS_PROFILE") != ""
}

// ResolveAWSProfile resolves the effective AWS shared config profile to use when
// none was explicitly exported in the shell. Order of precedence:
//  1. explicitProfile (e.g. from cluster.yaml metadata.profile or state.env AWS_PROFILE)
//  2. AWS_PROFILE / AWS_DEFAULT_PROFILE environment variables
//  3. Empty string when static/web-identity/container env credentials are set
//  4. Named profiles in ~/.aws/config and ~/.aws/credentials (prioritizing profiles
//     matching accountID, then [default] if present, then other named profiles) that
//     resolve valid credentials via CredentialsConfigured without hanging on EC2 IMDS.
func ResolveAWSProfile(ctx context.Context, region, explicitProfile, accountID string) string {
	if p := strings.TrimSpace(explicitProfile); p != "" && p != "-" {
		return p
	}
	for _, envKey := range []string{"AWS_PROFILE", "AWS_DEFAULT_PROFILE"} {
		if p := strings.TrimSpace(os.Getenv(envKey)); p != "" {
			return p
		}
	}
	for _, envKey := range []string{
		"AWS_ACCESS_KEY_ID",
		"AWS_WEB_IDENTITY_TOKEN_FILE",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI",
	} {
		if strings.TrimSpace(os.Getenv(envKey)) != "" {
			return ""
		}
	}

	candidates, acctMatches := discoverSharedAWSProfiles(strings.TrimSpace(accountID))
	if len(candidates) == 0 {
		return ""
	}
	probeRegion := strings.TrimSpace(region)
	if probeRegion == "" {
		probeRegion = "us-east-1"
	}
	isTest := testing.Testing() && os.Getenv("BNKCTL_FORCE_LIVE_AUTH") != "1"
	firstConfigured := ""
	for _, prof := range candidates {
		if isTest {
			probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, err := CredentialsConfigured(probeCtx, Options{
				Region:  probeRegion,
				Profile: prof,
			})
			cancel()
			if err == nil {
				return prof
			}
			continue
		}
		stsCtx, stsCancel := context.WithTimeout(ctx, 8*time.Second)
		cl, err := NewClients(stsCtx, Options{
			Region:  probeRegion,
			Profile: prof,
		})
		if err == nil && cl != nil && cl.STS != nil {
			_, err = cl.STS.GetCallerIdentity(stsCtx, &sts.GetCallerIdentityInput{})
		}
		stsCancel()
		if err == nil {
			return prof
		}
		if firstConfigured == "" {
			probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			if _, cfgErr := CredentialsConfigured(probeCtx, Options{
				Region:  probeRegion,
				Profile: prof,
			}); cfgErr == nil {
				firstConfigured = prof
			}
			cancel()
		}
	}
	if len(acctMatches) > 0 {
		return acctMatches[0]
	}
	if firstConfigured != "" {
		return firstConfigured
	}
	return ""
}

func discoverSharedAWSProfiles(accountID string) (ordered []string, accountMatches []string) {
	home, _ := os.UserHomeDir()
	cfgPath := strings.TrimSpace(os.Getenv("AWS_CONFIG_FILE"))
	if cfgPath == "" && home != "" {
		cfgPath = filepath.Join(home, ".aws", "config")
	}
	credPath := strings.TrimSpace(os.Getenv("AWS_SHARED_CREDENTIALS_FILE"))
	if credPath == "" && home != "" {
		credPath = filepath.Join(home, ".aws", "credentials")
	}

	var allProfiles []string
	seen := map[string]bool{}
	matched := map[string]bool{}
	hasConfigDefault := false

	parseFile := func(path string, isConfig bool) {
		if path == "" {
			return
		}
		data, err := os.ReadFile(path) // #nosec G304 -- standard AWS shared config/credentials paths
		if err != nil {
			return
		}
		currentProfile := ""
		for _, raw := range strings.Split(string(data), "\n") {
			line := strings.TrimSpace(raw)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
				continue
			}
			if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
				sec := strings.TrimSpace(line[1 : len(line)-1])
				if isConfig {
					if strings.HasPrefix(sec, "profile ") {
						sec = strings.TrimSpace(strings.TrimPrefix(sec, "profile "))
					} else if sec != "default" {
						// Ignore [sso-session ...] or [services ...] blocks in ~/.aws/config
						currentProfile = ""
						continue
					}
				}
				currentProfile = sec
				if isConfig && currentProfile == "default" {
					hasConfigDefault = true
				}
				if currentProfile != "" && !seen[currentProfile] {
					seen[currentProfile] = true
					allProfiles = append(allProfiles, currentProfile)
				}
				if accountID != "" && currentProfile != "" && strings.Contains(currentProfile, accountID) {
					matched[currentProfile] = true
				}
				continue
			}
			if currentProfile != "" && accountID != "" && strings.Contains(line, accountID) {
				matched[currentProfile] = true
			}
		}
	}

	parseFile(cfgPath, true)
	parseFile(credPath, false)

	added := map[string]bool{}
	for _, p := range allProfiles {
		if matched[p] && !added[p] {
			added[p] = true
			ordered = append(ordered, p)
			accountMatches = append(accountMatches, p)
		}
	}
	if hasConfigDefault && !added["default"] {
		added["default"] = true
		ordered = append(ordered, "default")
	}
	for _, p := range allProfiles {
		if !added[p] {
			added[p] = true
			ordered = append(ordered, p)
		}
	}
	return ordered, accountMatches
}
