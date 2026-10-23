// Package azure provides authentication and Azure Resource Manager clients.
package azure

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// EnvSkipAuth is the environment variable that bypasses real Azure credential checks
// during dry-run testing.
const EnvSkipAuth = "AZRBNKCTL_SKIP_AUTH"

// ShouldSkipAuth checks if AZRBNKCTL_SKIP_AUTH=1 is set.
func ShouldSkipAuth() bool {
	return os.Getenv(EnvSkipAuth) == "1"
}

// NewCredential creates an Azure TokenCredential using DefaultAzureCredential.
func NewCredential() (azcore.TokenCredential, error) {
	return NewCredentialForSubscription("")
}

// NewCredentialForSubscription creates an Azure TokenCredential using DefaultAzureCredential,
// binding the TenantID associated with subscriptionID in ~/.azure/azureProfile.json when available.
func NewCredentialForSubscription(subscriptionID string) (azcore.TokenCredential, error) {
	if ShouldSkipAuth() {
		return nil, nil
	}
	var opts *azidentity.DefaultAzureCredentialOptions
	if !testing.Testing() && subscriptionID != "" {
		if tenantID := readAzCLITenantForSubscription(subscriptionID); tenantID != "" {
			opts = &azidentity.DefaultAzureCredentialOptions{TenantID: tenantID}
		}
	}
	cred, err := azidentity.NewDefaultAzureCredential(opts)
	if err != nil {
		return nil, fmt.Errorf("azure auth: failed to initialize DefaultAzureCredential: %w", err)
	}
	return cred, nil
}

// ResolveSubscriptionID resolves the Azure Subscription ID from the intent file,
// environment variables (AZURE_SUBSCRIPTION_ID, ARM_SUBSCRIPTION_ID), active Azure CLI
// profile (~/.azure/azureProfile.json), or returns an error.
func ResolveSubscriptionID(explicit string) (string, error) {
	if explicit != "" && explicit != "00000000-0000-0000-0000-000000000000" {
		return explicit, nil
	}
	if env := os.Getenv("AZURE_SUBSCRIPTION_ID"); env != "" {
		return env, nil
	}
	if env := os.Getenv("ARM_SUBSCRIPTION_ID"); env != "" {
		return env, nil
	}
	if ShouldSkipAuth() {
		return "00000000-0000-0000-0000-000000000000", nil
	}
	if !testing.Testing() {
		if subID := readAzCLIDefaultSubscription(); subID != "" {
			return subID, nil
		}
	}
	return "", errors.New("azure auth: subscription ID not specified in cluster.yaml metadata.subscriptionId or AZURE_SUBSCRIPTION_ID environment variable")
}

func readAzCLIDefaultSubscription() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	profilePath := filepath.Join(home, ".azure", "azureProfile.json")
	// #nosec G304 -- reading standard Azure CLI profile in user home directory
	raw, err := os.ReadFile(profilePath)
	if err != nil {
		return ""
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	var prof struct {
		Subscriptions []struct {
			ID        string `json:"id"`
			IsDefault bool   `json:"isDefault"`
		} `json:"subscriptions"`
	}
	if err := json.Unmarshal(raw, &prof); err != nil {
		return ""
	}
	for _, s := range prof.Subscriptions {
		if s.IsDefault && strings.TrimSpace(s.ID) != "" {
			return strings.TrimSpace(s.ID)
		}
	}
	return ""
}

func readAzCLITenantForSubscription(subscriptionID string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	profilePath := filepath.Join(home, ".azure", "azureProfile.json")
	// #nosec G304 -- reading standard Azure CLI profile in user home directory
	raw, err := os.ReadFile(profilePath)
	if err != nil {
		return ""
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	var prof struct {
		Subscriptions []struct {
			ID       string `json:"id"`
			TenantID string `json:"tenantId"`
		} `json:"subscriptions"`
	}
	if err := json.Unmarshal(raw, &prof); err != nil {
		return ""
	}
	for _, s := range prof.Subscriptions {
		if strings.EqualFold(strings.TrimSpace(s.ID), strings.TrimSpace(subscriptionID)) {
			return strings.TrimSpace(s.TenantID)
		}
	}
	return ""
}
