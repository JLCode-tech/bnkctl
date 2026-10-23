package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AgentTokenFileName is the on-disk file inside a workspace dir (next to
// forge_link.json) that caches minted Forge benchmark agent JWT tokens.
const AgentTokenFileName = "forge_agent_token.json" // #nosec G101 -- file name constant, not a hardcoded credential

// AgentTokenPath returns the path to forge_agent_token.json inside workspaceDir.
func AgentTokenPath(workspaceDir string) string {
	return filepath.Join(workspaceDir, AgentTokenFileName)
}

// TokenSource describes where an agent token was obtained from.
type TokenSource string

const (
	// TokenSourceConfigured indicates the token came from CLI flag, env, or cluster.yaml.
	TokenSourceConfigured TokenSource = "configured agent"
	// TokenSourceCached indicates the token was read from forge_agent_token.json.
	TokenSourceCached TokenSource = "cached agent"
	// TokenSourceMinted indicates the token was minted via POST /api/benchmarks/agents/{id}/token.
	TokenSourceMinted TokenSource = "minted agent"
)

// StoredAgentToken records an agent JWT and its expiration.
type StoredAgentToken struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// agentTokenFileSchema is the JSON structure stored in forge_agent_token.json.
type agentTokenFileSchema struct {
	Tokens map[string]StoredAgentToken `json:"tokens"`
}

func agentTokenStoreKey(forgeURL, agentName string) string {
	return strings.TrimRight(forgeURL, "/") + "|" + agentName
}

// ReadAgentToken loads a cached agent token for (forgeURL, agentName) from workspaceDir.
// Returns ("", os.ErrNotExist) if the file or key is not found.
func ReadAgentToken(workspaceDir, forgeURL, agentName string) (string, error) {
	if workspaceDir == "" {
		return "", os.ErrNotExist
	}
	data, err := os.ReadFile(filepath.Clean(AgentTokenPath(workspaceDir))) // #nosec G304 -- path is derived from config-managed workspace directory, not user-tainted input
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", os.ErrNotExist
		}
		return "", err
	}

	// Try reading structured schema first
	var schema agentTokenFileSchema
	if err := json.Unmarshal(data, &schema); err == nil && len(schema.Tokens) > 0 {
		key := agentTokenStoreKey(forgeURL, agentName)
		if entry, ok := schema.Tokens[key]; ok && entry.Token != "" {
			if !isTokenExpired(entry.ExpiresAt) {
				return entry.Token, nil
			}
		}
	}

	// Fallback: flat map of key -> string or key -> StoredAgentToken
	var flatMap map[string]any
	if err := json.Unmarshal(data, &flatMap); err == nil {
		key := agentTokenStoreKey(forgeURL, agentName)
		if v, ok := flatMap[key]; ok {
			switch val := v.(type) {
			case string:
				if val != "" {
					return val, nil
				}
			case map[string]any:
				if tok, ok := val["token"].(string); ok && tok != "" {
					exp, _ := val["expires_at"].(string)
					if !isTokenExpired(exp) {
						return tok, nil
					}
				}
			}
		}
	}

	return "", os.ErrNotExist
}

func isTokenExpired(expiresAt string) bool {
	if expiresAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return false
	}
	return time.Now().After(t)
}

// WriteAgentToken atomically persists an agent token with mode 0600 next to forge_link.json.
func WriteAgentToken(workspaceDir, forgeURL, agentName, token, expiresAt string) error {
	if workspaceDir == "" {
		return nil
	}
	if err := os.MkdirAll(workspaceDir, 0o750); err != nil {
		return fmt.Errorf("ensure workspace dir: %w", err)
	}

	// Load existing file to preserve other tokens if present
	schema := agentTokenFileSchema{
		Tokens: make(map[string]StoredAgentToken),
	}
	if data, err := os.ReadFile(filepath.Clean(AgentTokenPath(workspaceDir))); err == nil { // #nosec G304 -- path is derived from config-managed workspace directory, not user-tainted input
		_ = json.Unmarshal(data, &schema)
		if schema.Tokens == nil {
			schema.Tokens = make(map[string]StoredAgentToken)
		}
	}

	key := agentTokenStoreKey(forgeURL, agentName)
	schema.Tokens[key] = StoredAgentToken{
		Token:     token,
		ExpiresAt: expiresAt,
	}

	b, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal agent token store: %w", err)
	}
	b = append(b, '\n')

	final := AgentTokenPath(workspaceDir)
	tmp, err := os.CreateTemp(workspaceDir, "forge_agent_token.*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp token file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName) // #nosec G104 -- best effort cleanup of temp file
	}()

	// Guarantee mode 0600
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp token file: %w", err)
	}

	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp token file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp token file: %w", err)
	}

	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("replace agent token file: %w", err)
	}
	return nil
}

// DeleteAgentToken removes any cached agent token for (forgeURL, agentName) from workspaceDir.
func DeleteAgentToken(workspaceDir, forgeURL, agentName string) error {
	if workspaceDir == "" {
		return nil
	}
	final := filepath.Clean(AgentTokenPath(workspaceDir))
	data, err := os.ReadFile(final) // #nosec G304 -- path is derived from config-managed workspace directory, not user-tainted input
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var schema agentTokenFileSchema
	if err := json.Unmarshal(data, &schema); err != nil {
		// Cannot parse; remove file
		return os.Remove(final)
	}

	key := agentTokenStoreKey(forgeURL, agentName)
	delete(schema.Tokens, key)

	b, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(workspaceDir, "forge_agent_token.*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName) // #nosec G104
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmpName, final)
}

// MintAgentTokenResponse is the response from POST /api/benchmarks/agents/{id}/token.
type MintAgentTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// MintUnsupportedError is returned when Forge returns 404 or 405 on token minting.
type MintUnsupportedError struct {
	StatusCode int
	URL        string
	AgentID    int
}

func (e *MintUnsupportedError) Error() string {
	return fmt.Sprintf("Forge server at %s returned HTTP %d on POST /api/benchmarks/agents/%d/token: agent token minting requires Forge >= 2.11 (or a Forge build with the benchmark agent mint endpoint). Fallback: provide an agent token via AZRBNKCTL_FORGE_AGENT_TOKEN or cluster.yaml forge.agent_token",
		e.URL, e.StatusCode, e.AgentID)
}

// MintAgentToken calls POST /api/benchmarks/agents/{agent_id}/token with the operator bearer token.
func MintAgentToken(ctx context.Context, restURL, operatorToken string, agentID int) (MintAgentTokenResponse, error) {
	base := strings.TrimRight(restURL, "/")
	mintURL := fmt.Sprintf("%s/api/benchmarks/agents/%d/token", base, agentID)

	var resp MintAgentTokenResponse
	err := restPost(ctx, mintURL, operatorToken, nil, &resp)
	if err != nil {
		var herr *restHTTPErr
		if errors.As(err, &herr) {
			if herr.StatusCode == http.StatusNotFound || herr.StatusCode == http.StatusMethodNotAllowed {
				return MintAgentTokenResponse{}, &MintUnsupportedError{
					StatusCode: herr.StatusCode,
					URL:        base,
					AgentID:    agentID,
				}
			}
		}
		return MintAgentTokenResponse{}, fmt.Errorf("forge benchmark agent: mint token: %w", err)
	}

	if resp.Token == "" {
		return MintAgentTokenResponse{}, fmt.Errorf("forge benchmark agent: mint token: received empty token in response")
	}
	return resp, nil
}

// AcquireAgentTokenOptions configures AcquireAgentToken.
type AcquireAgentTokenOptions struct {
	RestURL       string
	OperatorToken string
	AgentID       int
	AgentName     string
	ExplicitToken string
	WorkspaceDir  string
}

// AgentTokenResult contains the resolved agent token and its source.
type AgentTokenResult struct {
	Token  string
	Source TokenSource
}

// AcquireAgentToken acquires a Forge benchmark agent token according to precedence:
//  1. Explicit token (flag, AZRBNKCTL_FORGE_AGENT_TOKEN env, or cluster.yaml forge.agent_token)
//  2. Cached token on disk next to forge_link.json (forge_agent_token.json)
//  3. Mint via POST /api/benchmarks/agents/{id}/token (persisted to disk on success)
func AcquireAgentToken(ctx context.Context, opts AcquireAgentTokenOptions) (AgentTokenResult, error) {
	// 1. Explicit token (flag / env / yaml)
	if opts.ExplicitToken != "" {
		return AgentTokenResult{
			Token:  opts.ExplicitToken,
			Source: TokenSourceConfigured,
		}, nil
	}

	base := strings.TrimRight(opts.RestURL, "/")

	// 2. Cached token on disk
	if opts.WorkspaceDir != "" {
		if cached, err := ReadAgentToken(opts.WorkspaceDir, base, opts.AgentName); err == nil && cached != "" {
			return AgentTokenResult{
				Token:  cached,
				Source: TokenSourceCached,
			}, nil
		}
	}

	// 3. Mint endpoint
	mintResp, err := MintAgentToken(ctx, base, opts.OperatorToken, opts.AgentID)
	if err != nil {
		return AgentTokenResult{}, err
	}

	// Persist newly minted token to disk with mode 0600
	if opts.WorkspaceDir != "" {
		_ = WriteAgentToken(opts.WorkspaceDir, base, opts.AgentName, mintResp.Token, mintResp.ExpiresAt)
	}

	return AgentTokenResult{
		Token:  mintResp.Token,
		Source: TokenSourceMinted,
	}, nil
}
