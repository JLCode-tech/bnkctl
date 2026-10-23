package forge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadWriteDeleteAgentToken(t *testing.T) {
	tmpDir := t.TempDir()
	forgeURL := "http://forge.example.com:8000"
	agentName := "agent-test-1"
	token := "sample-agent-jwt"
	expiresAt := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	// Initially, reading non-existent token should return empty string, os.ErrNotExist
	gotTok, err := ReadAgentToken(tmpDir, forgeURL, agentName)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected error reading non-existent token: %v", err)
	}
	if gotTok != "" {
		t.Fatalf("expected empty token, got %q", gotTok)
	}

	// Write token
	if err := WriteAgentToken(tmpDir, forgeURL, agentName, token, expiresAt.Format(time.RFC3339)); err != nil {
		t.Fatalf("WriteAgentToken failed: %v", err)
	}

	// Verify file permissions 0600
	tokenFilePath := AgentTokenPath(tmpDir)
	fi, err := os.Stat(tokenFilePath)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("expected token file mode 0600, got %#o", fi.Mode().Perm())
	}

	// Read token back
	gotTok, err = ReadAgentToken(tmpDir, forgeURL, agentName)
	if err != nil {
		t.Fatalf("ReadAgentToken failed: %v", err)
	}
	if gotTok != token {
		t.Fatalf("expected token %q, got %q", token, gotTok)
	}

	// Delete token
	if err := DeleteAgentToken(tmpDir, forgeURL, agentName); err != nil {
		t.Fatalf("DeleteAgentToken failed: %v", err)
	}

	// Read again; should be empty
	gotTok, err = ReadAgentToken(tmpDir, forgeURL, agentName)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadAgentToken after delete failed: %v", err)
	}
	if gotTok != "" {
		t.Fatalf("expected empty token after delete, got %q", gotTok)
	}
}

func TestReadAgentToken_Expired(t *testing.T) {
	tmpDir := t.TempDir()
	forgeURL := "http://forge.example.com:8000"
	agentName := "agent-expired"
	token := "expired-jwt"
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)

	if err := WriteAgentToken(tmpDir, forgeURL, agentName, token, past); err != nil {
		t.Fatalf("WriteAgentToken failed: %v", err)
	}

	gotTok, err := ReadAgentToken(tmpDir, forgeURL, agentName)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadAgentToken failed: %v", err)
	}
	if gotTok != "" {
		t.Fatalf("expected expired token to be ignored, got %q", gotTok)
	}
}

func TestMintAgentToken_Success(t *testing.T) {
	expectedToken := "minted-agent-token-xyz"
	expectedExpires := time.Now().Add(365 * 24 * time.Hour).UTC().Format(time.RFC3339)
	operatorToken := "operator-jwt-secret"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/benchmarks/agents/99/token" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+operatorToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MintAgentTokenResponse{
			Token:     expectedToken,
			ExpiresAt: expectedExpires,
		})
	}))
	defer server.Close()

	ctx := context.Background()
	resp, err := MintAgentToken(ctx, server.URL, operatorToken, 99)
	if err != nil {
		t.Fatalf("MintAgentToken failed: %v", err)
	}
	if resp.Token != expectedToken {
		t.Errorf("expected token %q, got %q", expectedToken, resp.Token)
	}
	if resp.ExpiresAt != expectedExpires {
		t.Errorf("expected expires_at %q, got %q", expectedExpires, resp.ExpiresAt)
	}
}

func TestMintAgentToken_UnsupportedVersions(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "not supported", status)
		}))
		defer server.Close()

		ctx := context.Background()
		_, err := MintAgentToken(ctx, server.URL, "operator-token", 12)
		if err == nil {
			t.Fatalf("expected error for HTTP %d, got nil", status)
		}

		var mintErr *MintUnsupportedError
		if !errors.As(err, &mintErr) {
			t.Fatalf("expected *MintUnsupportedError, got %T: %v", err, err)
		}
		if mintErr.StatusCode != status {
			t.Errorf("expected status %d, got %d", status, mintErr.StatusCode)
		}
		errMsg := err.Error()
		if !filepath.IsAbs("/") { // just a dummy check
			t.Fatal("unreachable")
		}
		if !containsAll(errMsg, "Forge >= 2.11", "POST /api/benchmarks/agents/12/token", "AZRBNKCTL_FORGE_AGENT_TOKEN") {
			t.Errorf("expected error message to contain actionable guidance, got: %s", errMsg)
		}
	}
}

func containsAll(s string, substrings ...string) bool {
	for _, sub := range substrings {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || (len(s) > 0 && len(substr) > 0 && indexOf(s, substr) >= 0))
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func TestAcquireAgentToken_Precedence(t *testing.T) {
	tmpDir := t.TempDir()
	forgeURL := "http://localhost:8000"
	agentName := "agent-precedence"
	agentID := 15

	// Case 1: Explicit token overrides cache and mint
	res, err := AcquireAgentToken(context.Background(), AcquireAgentTokenOptions{
		RestURL:       forgeURL,
		OperatorToken: "op-token",
		AgentID:       agentID,
		AgentName:     agentName,
		ExplicitToken: "explicit-token-123",
		WorkspaceDir:  tmpDir,
	})
	if err != nil {
		t.Fatalf("case 1 failed: %v", err)
	}
	if res.Token != "explicit-token-123" || res.Source != TokenSourceConfigured {
		t.Errorf("case 1 unexpected result: %+v", res)
	}

	// Case 2: Cached token on disk overrides mint
	if err := WriteAgentToken(tmpDir, forgeURL, agentName, "cached-token-456", time.Now().Add(time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatalf("write token failed: %v", err)
	}
	res, err = AcquireAgentToken(context.Background(), AcquireAgentTokenOptions{
		RestURL:       forgeURL,
		OperatorToken: "op-token",
		AgentID:       agentID,
		AgentName:     agentName,
		WorkspaceDir:  tmpDir,
	})
	if err != nil {
		t.Fatalf("case 2 failed: %v", err)
	}
	if res.Token != "cached-token-456" || res.Source != TokenSourceCached {
		t.Errorf("case 2 unexpected result: %+v", res)
	}

	// Delete cache for case 3
	_ = DeleteAgentToken(tmpDir, forgeURL, agentName)

	// Case 3: Mint fallback
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MintAgentTokenResponse{
			Token:     "minted-token-789",
			ExpiresAt: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		})
	}))
	defer server.Close()

	res, err = AcquireAgentToken(context.Background(), AcquireAgentTokenOptions{
		RestURL:       server.URL,
		OperatorToken: "op-token",
		AgentID:       agentID,
		AgentName:     agentName,
		WorkspaceDir:  tmpDir,
	})
	if err != nil {
		t.Fatalf("case 3 failed: %v", err)
	}
	if res.Token != "minted-token-789" || res.Source != TokenSourceMinted {
		t.Errorf("case 3 unexpected result: %+v", res)
	}

	// Verify minted token was written to cache
	cached, err := ReadAgentToken(tmpDir, server.URL, agentName)
	if err != nil || cached != "minted-token-789" {
		t.Errorf("expected minted token to be cached, got %q (err: %v)", cached, err)
	}
}
