package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	Credentials    string
	IsDefault      bool
}

// CredentialTemplateResponse represents a credential template returned by forge.
type CredentialTemplateResponse struct {
	ID             int    `json:"id"`
	Name           string `json:"name"`
	Description    string `json:"description,omitempty"`
	Provider       string `json:"provider"`
	SubscriptionID string `json:"azure_subscription_id,omitempty"`
	TenantID       string `json:"azure_tenant_id,omitempty"`
	IsDefault      bool   `json:"is_default,omitempty"`
}

// CreateAzureCredentialTemplate creates or updates an Azure credential template in forge.
func CreateAzureCredentialTemplate(ctx context.Context, restURL string, creds RestCreds, opts AzureCredentialTemplateOptions) (CredentialTemplateResponse, error) {
	if opts.Name == "" {
		return CredentialTemplateResponse{}, fmt.Errorf("forge: template Name is required")
	}
	if opts.SubscriptionID == "" {
		return CredentialTemplateResponse{}, fmt.Errorf("forge: Azure SubscriptionID is required")
	}

	base := strings.TrimRight(restURL, "/")
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
	base := strings.TrimRight(restURL, "/")
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
	base := strings.TrimRight(restURL, "/")
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

func findCredentialTemplateByName(ctx context.Context, base, token, name string) (CredentialTemplateResponse, error) {
	var list []CredentialTemplateResponse
	url := base + CredentialTemplateEndpoint
	if err := restGet(ctx, url, token, &list); err != nil {
		return CredentialTemplateResponse{}, err
	}
	for _, t := range list {
		if t.Name == name {
			return t, nil
		}
	}
	return CredentialTemplateResponse{}, fmt.Errorf("template %q not found", name)
}
