package forge_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/forge"
)

func TestForgeTemplate_CreateAndList(t *testing.T) {
	templates := []map[string]any{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/api/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "test-token"})
		case path == "/api/credential-templates" && r.Method == http.MethodPost:
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			id := len(templates) + 1
			req["id"] = id
			templates = append(templates, req)
			_ = json.NewEncoder(w).Encode(req)
		case path == "/api/credential-templates" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(templates)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	creds := forge.RestCreds{Username: "admin", Password: "changeme"}

	opts := forge.AzureCredentialTemplateOptions{
		Name:           "azure-test-sp",
		Description:    "Test Azure SP template",
		SubscriptionID: "00000000-0000-0000-0000-000000000000",
		TenantID:       "11111111-1111-1111-1111-111111111111",
		Credentials:    "mock-secret",
		IsDefault:      true,
	}

	res, err := forge.CreateAzureCredentialTemplate(ctx, srv.URL, creds, opts)
	if err != nil {
		t.Fatalf("CreateAzureCredentialTemplate: %v", err)
	}
	if res.ID != 1 || res.Name != "azure-test-sp" || res.Provider != "azure" {
		t.Errorf("unexpected template result: %+v", res)
	}

	list, err := forge.ListCredentialTemplates(ctx, srv.URL, creds)
	if err != nil {
		t.Fatalf("ListCredentialTemplates: %v", err)
	}
	if len(list) != 1 || list[0].Name != "azure-test-sp" {
		t.Errorf("unexpected template list: %+v", list)
	}
}

func TestResolveAndValidateCredentialTemplate_WarnsOnMissingSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "test-token"})
		case "/api/credential-templates":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"id":                      5,
					"name":                    "Azure Local - ALM2",
					"provider":                "azure",
					"is_default":              true,
					"has_azure_credentials":   false,
					"has_azure_client_secret": false,
				},
			})
		case "/api/credential-templates/5":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":                      5,
				"name":                    "Azure Local - ALM2",
				"provider":                "azure",
				"is_default":              true,
				"has_azure_credentials":   false,
				"has_azure_client_secret": false,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	creds := forge.RestCreds{Username: "admin", Password: "changeme"}
	id, warnings, err := forge.ResolveAndValidateCredentialTemplate(context.Background(), srv.URL, creds, "azure", 0, "")
	if err != nil {
		t.Fatalf("ResolveAndValidateCredentialTemplate: %v", err)
	}
	if id != 5 {
		t.Errorf("id = %d, want 5", id)
	}
	if len(warnings) == 0 {
		t.Fatal("expected warning when default Azure template lacks credentials, got none")
	}

	// Explicit lookup by name
	idByName, warningsByName, err := forge.ResolveAndValidateCredentialTemplate(context.Background(), srv.URL, creds, "azure", 0, "Azure Local - ALM2")
	if err != nil {
		t.Fatalf("ResolveAndValidateCredentialTemplate by name: %v", err)
	}
	if idByName != 5 || len(warningsByName) == 0 {
		t.Errorf("idByName=%d warnings=%v", idByName, warningsByName)
	}
}
