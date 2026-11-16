package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// CredentialTemplateEndpoint is the forge REST path for credential templates.
// #nosec G101 -- REST endpoint URL path, not credentials
const CredentialTemplateEndpoint = "/api/credential-templates"

// DefaultUsername is the default username for local forge development.
const DefaultUsername = "admin"

// DefaultDevForgeSecret is the fallback dev secret for local forge mock server.
// #nosec G101 -- default dev fallback, not production secret
const DefaultDevForgeSecret = "changeme"

// AzureCredentialTemplateOptions carries options for creating or updating an Azure credential template.
type AzureCredentialTemplateOptions struct {
	Name           string
	Description    string
	SubscriptionID string
	TenantID       string
	ClientID       string
	ClientSecret   string
	Credentials    string
	IsDefault      bool
}

// CredentialTemplateResponse represents a credential template returned by forge.
type CredentialTemplateResponse struct {
	ID                    int    `json:"id"`
	Name                  string `json:"name"`
	Description           string `json:"description,omitempty"`
	Provider              string `json:"provider"`
	AWSAuthMethod         string `json:"aws_auth_method,omitempty"`
	AWSProfile            string `json:"aws_profile,omitempty"`
	AWSAccessKeyID        string `json:"aws_access_key_id,omitempty"`
	HasAWSSecretAccessKey bool   `json:"has_aws_secret_access_key,omitempty"`
	HasAWSSessionToken    bool   `json:"has_aws_session_token,omitempty"`
	AWSSSOEnabled         bool   `json:"aws_sso_enabled,omitempty"`
	AWSSSOAuthenticatedAt string `json:"aws_sso_authenticated_at,omitempty"`
	GCPProjectID          string `json:"gcp_project_id,omitempty"`
	HasGCPCredentials     bool   `json:"has_gcp_credentials,omitempty"`
	AzureAuthMethod       string `json:"azure_auth_method,omitempty"`
	SubscriptionID        string `json:"azure_subscription_id,omitempty"`
	TenantID              string `json:"azure_tenant_id,omitempty"`
	ClientID              string `json:"azure_client_id,omitempty"`
	HasAzureClientSecret  bool   `json:"has_azure_client_secret,omitempty"`
	HasAzureCredentials   bool   `json:"has_azure_credentials,omitempty"`
	AzureSSOAuthenticated string `json:"azure_sso_authenticated_at,omitempty"`
	HasIBMCloudAPIKey     bool   `json:"has_ibmcloud_api_key,omitempty"`
	SSHHost               string `json:"ssh_host,omitempty"`
	HasSSHPassword        bool   `json:"has_ssh_password,omitempty"`
	HasSSHKey             bool   `json:"has_ssh_key,omitempty"`
	IsDefault             bool   `json:"is_default,omitempty"`
	LastSuccessfulCallAt  string `json:"last_successful_call_at,omitempty"`
	LastErrorAt           string `json:"last_error_at,omitempty"`
	LastErrorCode         string `json:"last_error_code,omitempty"`
	LastErrorMessage      string `json:"last_error_message,omitempty"`
}

// HasValidCredentials reports whether the credential template has populated
// secret material or SSO session state for its declared provider.
func (t CredentialTemplateResponse) HasValidCredentials() bool {
	if strings.TrimSpace(t.LastSuccessfulCallAt) != "" && strings.TrimSpace(t.LastErrorAt) == "" {
		return true
	}
	switch NormalizeProvider(t.Provider, "") {
	case "azure":
		return t.HasAzureCredentials || t.HasAzureClientSecret ||
			strings.TrimSpace(t.AzureSSOAuthenticated) != ""
	case "aws":
		return t.HasAWSSecretAccessKey || t.HasAWSSessionToken ||
			strings.TrimSpace(t.AWSProfile) != "" ||
			(t.AWSSSOEnabled && strings.TrimSpace(t.AWSSSOAuthenticatedAt) != "") ||
			strings.TrimSpace(t.AWSAccessKeyID) != ""
	case "gcp":
		return t.HasGCPCredentials
	case "ssh":
		return t.HasSSHKey || t.HasSSHPassword
	default:
		return true
	}
}

// CredentialWarning returns a non-empty warning message if the credential
// template is missing required secret material for its cloud provider.
func (t CredentialTemplateResponse) CredentialWarning() string {
	if t.ID <= 0 || t.HasValidCredentials() {
		return ""
	}
	prov := NormalizeProvider(t.Provider, "")
	switch prov {
	case "azure":
		return fmt.Sprintf(
			"credential template %q (id=%d, provider=azure) lacks Azure credentials (has_azure_credentials=false, has_azure_client_secret=false)",
			t.Name, t.ID,
		)
	case "aws":
		return fmt.Sprintf(
			"credential template %q (id=%d, provider=aws) lacks AWS credentials or profile",
			t.Name, t.ID,
		)
	case "gcp":
		return fmt.Sprintf(
			"credential template %q (id=%d, provider=gcp) lacks GCP service account credentials (has_gcp_credentials=false)",
			t.Name, t.ID,
		)
	case "ssh":
		return fmt.Sprintf(
			"credential template %q (id=%d, provider=ssh) lacks SSH key or password",
			t.Name, t.ID,
		)
	default:
		return ""
	}
}

// CreateAzureCredentialTemplate creates or updates an Azure credential template in forge.
func CreateAzureCredentialTemplate(ctx context.Context, restURL string, creds RestCreds, opts AzureCredentialTemplateOptions) (CredentialTemplateResponse, error) {
	if opts.Name == "" {
		return CredentialTemplateResponse{}, fmt.Errorf("forge: template Name is required")
	}
	if opts.SubscriptionID == "" {
		return CredentialTemplateResponse{}, fmt.Errorf("forge: Azure SubscriptionID is required")
	}

	base := resolveRESTBaseURL(restURL)
	token, err := restLogin(ctx, base, creds.restUsername(), creds.restPassword())
	if err != nil {
		return CredentialTemplateResponse{}, fmt.Errorf("forge REST login: %w", err)
	}

	body := map[string]any{
		"name":                  opts.Name,
		"description":           opts.Description,
		"provider":              "azure",
		"azure_subscription_id": opts.SubscriptionID,
		"azure_tenant_id":       opts.TenantID,
		"azure_credentials":     opts.Credentials,
		"is_default":            opts.IsDefault,
	}
	if opts.ClientID != "" {
		body["azure_client_id"] = opts.ClientID
	}
	if opts.ClientSecret != "" {
		body["azure_client_secret"] = opts.ClientSecret
	}

	var created CredentialTemplateResponse
	url := base + CredentialTemplateEndpoint
	err = restPost(ctx, url, token, body, &created)
	if err == nil {
		return created, nil
	}

	// 409 Conflict: template with this name already exists -> find and update
	var herr *restHTTPErr
	if errors.As(err, &herr) && (herr.StatusCode == http.StatusConflict || herr.StatusCode == http.StatusBadRequest) {
		existing, findErr := findCredentialTemplateByName(ctx, base, token, opts.Name)
		if findErr != nil {
			return CredentialTemplateResponse{}, fmt.Errorf("template exists but lookup failed: %w", findErr)
		}
		updateURL := fmt.Sprintf("%s/%d", url, existing.ID)
		var updated CredentialTemplateResponse
		if putErr := restPut(ctx, updateURL, token, body, &updated); putErr != nil {
			return CredentialTemplateResponse{}, fmt.Errorf("update template %d: %w", existing.ID, putErr)
		}
		if updated.ID == 0 {
			updated = existing
		}
		return updated, nil
	}

	return CredentialTemplateResponse{}, fmt.Errorf("create credential template: %w", err)
}

// ListCredentialTemplates lists all credential templates in forge.
func ListCredentialTemplates(ctx context.Context, restURL string, creds RestCreds) ([]CredentialTemplateResponse, error) {
	base := resolveRESTBaseURL(restURL)
	token, err := restLogin(ctx, base, creds.restUsername(), creds.restPassword())
	if err != nil {
		return nil, fmt.Errorf("forge REST login: %w", err)
	}

	var list []CredentialTemplateResponse
	url := base + CredentialTemplateEndpoint
	if err := restGet(ctx, url, token, &list); err != nil {
		return nil, fmt.Errorf("list credential templates: %w", err)
	}
	return list, nil
}

// GetCredentialTemplate retrieves a specific credential template by ID.
func GetCredentialTemplate(ctx context.Context, restURL string, creds RestCreds, templateID int) (CredentialTemplateResponse, error) {
	base := resolveRESTBaseURL(restURL)
	token, err := restLogin(ctx, base, creds.restUsername(), creds.restPassword())
	if err != nil {
		return CredentialTemplateResponse{}, fmt.Errorf("forge REST login: %w", err)
	}

	var tpl CredentialTemplateResponse
	url := fmt.Sprintf("%s%s/%d", base, CredentialTemplateEndpoint, templateID)
	if err := restGet(ctx, url, token, &tpl); err != nil {
		return CredentialTemplateResponse{}, fmt.Errorf("get credential template %d: %w", templateID, err)
	}
	return tpl, nil
}

// FindCredentialTemplate resolves a credential template by numeric ID or name.
func FindCredentialTemplate(ctx context.Context, restURL string, creds RestCreds, idOrName string) (CredentialTemplateResponse, error) {
	ref := strings.TrimSpace(idOrName)
	if ref == "" {
		return CredentialTemplateResponse{}, fmt.Errorf("credential template reference is empty")
	}
	if id, err := strconv.Atoi(ref); err == nil && id > 0 {
		if tpl, gerr := GetCredentialTemplate(ctx, restURL, creds, id); gerr == nil && tpl.ID > 0 {
			return tpl, nil
		}
		list, lerr := ListCredentialTemplates(ctx, restURL, creds)
		if lerr != nil {
			return CredentialTemplateResponse{ID: id}, nil
		}
		for _, t := range list {
			if t.ID == id {
				return t, nil
			}
		}
		return CredentialTemplateResponse{}, fmt.Errorf("credential template id=%d not found in Forge", id)
	}

	list, err := ListCredentialTemplates(ctx, restURL, creds)
	if err != nil {
		return CredentialTemplateResponse{}, err
	}
	for _, t := range list {
		if strings.EqualFold(strings.TrimSpace(t.Name), ref) {
			return t, nil
		}
	}
	return CredentialTemplateResponse{}, fmt.Errorf("credential template %q not found in Forge", ref)
}

// ResolveAndValidateCredentialTemplate resolves an explicit template ID or name
// (when provided) or inspects the provider's default template in Forge, returning
// the resolved template ID and any credential completeness warnings.
func ResolveAndValidateCredentialTemplate(
	ctx context.Context,
	restURL string,
	creds RestCreds,
	cloudProvider string,
	explicitID int,
	explicitRef string,
) (int, []string, error) {
	var warnings []string
	normProv := NormalizeProvider(cloudProvider, "")

	ref := strings.TrimSpace(explicitRef)
	if ref != "" && explicitID <= 0 {
		if id, err := strconv.Atoi(ref); err == nil && id > 0 {
			explicitID = id
			ref = ""
		}
	}

	// Explicit template by name
	if ref != "" {
		tpl, err := FindCredentialTemplate(ctx, restURL, creds, ref)
		if err != nil {
			return 0, nil, err
		}
		if w := tpl.CredentialWarning(); w != "" {
			warnings = append(warnings, w)
		}
		return tpl.ID, warnings, nil
	}

	// Explicit template by ID
	if explicitID > 0 {
		if strings.TrimSpace(restURL) != "" {
			if tpl, err := GetCredentialTemplate(ctx, restURL, creds, explicitID); err == nil && tpl.ID > 0 {
				if w := tpl.CredentialWarning(); w != "" {
					warnings = append(warnings, w)
				}
			} else if list, lerr := ListCredentialTemplates(ctx, restURL, creds); lerr == nil {
				for _, t := range list {
					if t.ID == explicitID {
						if w := t.CredentialWarning(); w != "" {
							warnings = append(warnings, w)
						}
						break
					}
				}
			}
		}
		return explicitID, warnings, nil
	}

	// No explicit template provided: validate default template for cloud providers when REST URL is set.
	if strings.TrimSpace(restURL) == "" || !isCloudProviderRequiringTemplate(normProv) {
		return 0, nil, nil
	}

	list, err := ListCredentialTemplates(ctx, restURL, creds)
	if err != nil {
		return 0, nil, nil
	}

	var defaultTpl *CredentialTemplateResponse
	var firstValidTpl *CredentialTemplateResponse
	var firstMatchTpl *CredentialTemplateResponse
	for i := range list {
		t := &list[i]
		if !strings.EqualFold(NormalizeProvider(t.Provider, ""), normProv) {
			continue
		}
		if firstMatchTpl == nil {
			firstMatchTpl = t
		}
		if t.IsDefault && defaultTpl == nil {
			defaultTpl = t
		}
		if t.HasValidCredentials() && firstValidTpl == nil {
			firstValidTpl = t
		}
	}

	chosen := defaultTpl
	if chosen == nil {
		chosen = firstMatchTpl
	}
	if chosen == nil {
		warnings = append(warnings, fmt.Sprintf("no Forge credential template configured for provider %q", normProv))
		return 0, warnings, nil
	}

	// Fetch detail in case list omitted secret booleans.
	if detail, derr := GetCredentialTemplate(ctx, restURL, creds, chosen.ID); derr == nil && detail.ID > 0 {
		chosen = &detail
	}

	if w := chosen.CredentialWarning(); w != "" {
		if firstValidTpl != nil && firstValidTpl.ID != chosen.ID {
			warnings = append(warnings, fmt.Sprintf(
				"default %s %s; binding to valid template %q (id=%d) instead",
				normProv, w, firstValidTpl.Name, firstValidTpl.ID,
			))
			return firstValidTpl.ID, warnings, nil
		}
		warnings = append(warnings, fmt.Sprintf("default %s %s", normProv, w))
	}

	return chosen.ID, warnings, nil
}

func isCloudProviderRequiringTemplate(provider string) bool {
	switch provider {
	case "aws", "azure", "gcp":
		return true
	default:
		return false
	}
}

func findCredentialTemplateByName(ctx context.Context, base, token, name string) (CredentialTemplateResponse, error) {
	var list []CredentialTemplateResponse
	url := base + CredentialTemplateEndpoint
	if err := restGet(ctx, url, token, &list); err != nil {
		return CredentialTemplateResponse{}, err
	}
	for _, t := range list {
		if strings.EqualFold(t.Name, name) {
			return t, nil
		}
	}
	return CredentialTemplateResponse{}, fmt.Errorf("template %q not found", name)
}
