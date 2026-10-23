package forge

// accessmethod.go — register a jumphost as a forge SSH "access-method" record
// via POST /api/ssh-credentials.

import (
	"context"
	"fmt"
	"strings"
)

// SSHCredentialEndpoint is the forge REST path for SSH credential CRUD.
const SSHCredentialEndpoint = "/api/ssh-credentials" // #nosec G101 — REST URL path, not a credential

// AccessMethodOptions carries all caller-supplied data for registering the
// jumphost as a forge SSH access-method record.
type AccessMethodOptions struct {
	// RestURL is the forge REST base URL (e.g. "http://localhost:8000").
	RestURL string
	// Creds are the forge REST login credentials.
	Creds RestCreds
	// Name is the record name in forge (e.g. "azrbnkctl-jumphost-my-cluster").
	// Required.
	Name string
	// Host is the jumphost VM name, private IP, or public IP stored in the record.
	Host string
	// Port is the SSH port. Defaults to 22 if zero.
	Port int
	// Username is the SSH user. Defaults to "azureuser" if empty.
	Username string
	// AuthType is the authentication type (e.g. "key", "password"). Defaults to "key" if empty.
	AuthType string
	// Description overrides the default auto-generated description if non-empty.
	Description string
	// Region is the Azure region / location — included in the description.
	Region string
	// InstanceID is the VM name or resource ID — included in the description.
	InstanceID string
}

// AccessMethodResponse is the subset of SSHCredentialResponse fields that callers need.
type AccessMethodResponse struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Host string `json:"host"`
}

// RegisterJumphostAccessMethod POSTs the jumphost to forge's
// /api/ssh-credentials endpoint as a named SSH access-method record.
//
// Upsert semantics: if the name already exists (409 or found in list), the
// existing record is PUT-updated with the current description.
func RegisterJumphostAccessMethod(ctx context.Context, opts AccessMethodOptions) (AccessMethodResponse, error) {
	if opts.RestURL == "" {
		return AccessMethodResponse{}, fmt.Errorf("forge.RegisterJumphostAccessMethod: RestURL is required")
	}
	if opts.Name == "" {
		return AccessMethodResponse{}, fmt.Errorf("forge.RegisterJumphostAccessMethod: Name is required")
	}
	if opts.Host == "" {
		return AccessMethodResponse{}, fmt.Errorf("forge.RegisterJumphostAccessMethod: Host is required")
	}

	base := strings.TrimRight(opts.RestURL, "/")

	token, err := restLogin(ctx, base, opts.Creds.restUsername(), opts.Creds.restPassword())
	if err != nil {
		return AccessMethodResponse{}, fmt.Errorf("forge access-method: login: %w", err)
	}

	port := opts.Port
	if port == 0 {
		port = 22
	}
	user := opts.Username
	if user == "" {
		user = "azureuser"
	}
	authType := opts.AuthType
	if authType == "" {
		authType = "key"
	}

	desc := opts.Description
	if desc == "" {
		desc = fmt.Sprintf(
			"azrbnkctl-managed jumphost. Region: %s VM: %s. "+
				"Forge cannot SSH to this host directly without configured keys; record is informational.",
			opts.Region, opts.InstanceID,
		)
	}

	body := map[string]any{
		"name":        opts.Name,
		"description": desc,
		"host":        opts.Host,
		"port":        port,
		"username":    user,
		"auth_type":   authType,
	}

	var created AccessMethodResponse
	err = restPost(ctx, base+SSHCredentialEndpoint, token, body, &created)
	if err == nil {
		return created, nil
	}

	// 409 or 400-with-"already exists" body = name conflict — fall back to
	// list-and-update.
	if !isConflictHTTP(err) {
		return AccessMethodResponse{}, fmt.Errorf("forge access-method: create: %w", err)
	}

	existing, lookupErr := sshCredFindByName(ctx, base, token, opts.Name)
	if lookupErr != nil {
		return AccessMethodResponse{}, fmt.Errorf("forge access-method: conflict + list failed: %w (original: %v)", lookupErr, err)
	}

	updateBody := map[string]any{
		"description": desc,
		"host":        opts.Host,
		"port":        port,
		"username":    user,
		"auth_type":   authType,
	}
	updateURL := fmt.Sprintf("%s%s/%d", base, SSHCredentialEndpoint, existing.ID)
	var updated AccessMethodResponse
	if putErr := restPut(ctx, updateURL, token, updateBody, &updated); putErr != nil {
		return AccessMethodResponse{}, fmt.Errorf("forge access-method: PUT update: %w", putErr)
	}
	if updated.ID == 0 {
		updated = existing
	}
	return updated, nil
}

// sshCredFindByName GETs /api/ssh-credentials and returns the record whose name matches exactly.
func sshCredFindByName(ctx context.Context, base, token, name string) (AccessMethodResponse, error) {
	var list []AccessMethodResponse
	if err := restGet(ctx, base+SSHCredentialEndpoint, token, &list); err != nil {
		return AccessMethodResponse{}, fmt.Errorf("list ssh-credentials: %w", err)
	}
	for _, r := range list {
		if r.Name == name {
			return r, nil
		}
	}
	return AccessMethodResponse{}, fmt.Errorf("ssh-credential %q not found in forge", name)
}
