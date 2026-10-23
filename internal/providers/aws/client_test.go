package aws

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/state"
)

func TestHasEnvCredentials_Env(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_PROFILE", "")
	if !HasEnvCredentials() {
		t.Fatal("expected true when AWS_ACCESS_KEY_ID is set")
	}
}

func TestHasEnvCredentials_Profile(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_PROFILE", "myprof")
	if !HasEnvCredentials() {
		t.Fatal("expected true when AWS_PROFILE is set")
	}
}

func TestHasEnvCredentials_Neither(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_PROFILE", "")
	if HasEnvCredentials() {
		t.Fatal("expected false when neither is set")
	}
}

func TestNewClients_RegionRequired(t *testing.T) {
	// Strip every region-bearing env var so the chain has nothing to
	// fall back on. Setting AWS_REGION="" overrides the user shell.
	for _, v := range []string{"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_PROFILE"} {
		t.Setenv(v, "")
	}
	// HOME must not point at a profile that ships a region — stub.
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent")

	if _, err := NewClients(context.Background(), Options{}); err == nil {
		// On a CI / dev host AWS_REGION might leak in from elsewhere;
		// only fail when we know region resolution should have come up
		// empty.
		if os.Getenv("AWS_REGION") == "" && os.Getenv("AWS_DEFAULT_REGION") == "" {
			t.Fatal("expected error when no region is configured")
		}
	}
}

func TestResolveAWSProfile_SharedConfigAndAccountMatch(t *testing.T) {
	for _, k := range []string{"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config")
	credFile := filepath.Join(dir, "credentials")

	cfgContent := `[sso-session sso]
sso_start_url = https://example.awsapps.com/start
sso_region = us-east-1

[profile Other-111122223333]
sso_account_id = 111122223333
region = us-west-2

[profile Users-123456789012]
sso_account_id = 123456789012
region = ap-southeast-1
`
	credContent := `[Other-111122223333]
aws_access_key_id = AKIAOTHER11112222333
aws_secret_access_key = secret-other

[Users-123456789012]
aws_access_key_id = AKIAUSERS12345678901
aws_secret_access_key = secret-target
`
	if err := os.WriteFile(cfgFile, []byte(cfgContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credFile, []byte(credContent), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", cfgFile)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credFile)

	// Matching account ID selects Users-123456789012 even though Other-111122223333 is listed first.
	got := ResolveAWSProfile(context.Background(), "ap-southeast-1", "", "123456789012")
	if got != "Users-123456789012" {
		t.Fatalf("expected profile Users-123456789012 for account 123456789012, got %q", got)
	}

	// Explicit profile takes precedence.
	if explicit := ResolveAWSProfile(context.Background(), "ap-southeast-1", "custom-prof", "123456789012"); explicit != "custom-prof" {
		t.Fatalf("expected explicit profile custom-prof, got %q", explicit)
	}

	// When ~/.aws/credentials has a [default] section that is not present in ~/.aws/config
	// and accountID is empty, named profiles from ~/.aws/config take precedence over the
	// credentials-only [default] entry.
	credWithStaleDefault := "[default]\naws_access_key_id = AKIASTALEDEFAULT00000\naws_secret_access_key = stale\n\n" + credContent
	if err := os.WriteFile(credFile, []byte(credWithStaleDefault), 0o600); err != nil {
		t.Fatal(err)
	}
	if gotNoAcct := ResolveAWSProfile(context.Background(), "ap-southeast-1", "", ""); gotNoAcct != "Other-111122223333" {
		t.Fatalf("expected ~/.aws/config profile Other-111122223333 ahead of credentials-only [default], got %q", gotNoAcct)
	}
}

func TestExtractAccountIDFromState(t *testing.T) {
	st, err := state.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st.Set("NODEGROUP_DEFAULT_ARN", "arn:aws:eks:ap-southeast-1:123456789012:nodegroup/agent-core-singapore/ng-default/abc")
	if got := extractAccountIDFromState(st); got != "123456789012" {
		t.Fatalf("expected 123456789012 extracted from ARN, got %q", got)
	}
}
