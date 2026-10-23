package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestBenchmarkAgentWorker_LoopAndDispatch(t *testing.T) {
	var upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	var mu sync.Mutex
	var receivedHeartbeats int
	var receivedCompleted map[string]any
	var wsConn *websocket.Conn

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/auth/login":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": "fake-jwt-token",
			})
		case r.URL.Path == "/api/benchmarks/agents":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     42,
				"name":   "test-agent",
				"status": "connected",
			})
		case r.URL.Path == "/api/benchmarks/agents/42/token":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "agent-minted-token",
				"expires_at": time.Now().Add(24 * time.Hour).Format(time.RFC3339),
			})
		case strings.HasPrefix(r.URL.Path, "/ws/benchmarks/agents/"):
			if r.URL.Query().Get("token") != "agent-minted-token" {
				t.Errorf("expected token query param 'agent-minted-token', got %q", r.URL.Query().Get("token"))
			}
			if r.Header.Get("Authorization") != "Bearer agent-minted-token" {
				t.Errorf("expected Authorization header 'Bearer agent-minted-token', got %q", r.Header.Get("Authorization"))
			}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("WebSocket upgrade failed: %v", err)
				return
			}
			mu.Lock()
			wsConn = conn
			mu.Unlock()

			go func() {
				defer conn.Close() // #nosec G104
				for {
					_, msg, err := conn.ReadMessage()
					if err != nil {
						return
					}
					var payload map[string]any
					if err := json.Unmarshal(msg, &payload); err != nil {
						continue
					}
					mu.Lock()
					if payload["type"] == "heartbeat" {
						receivedHeartbeats++
					} else if payload["type"] == "run_completed" {
						receivedCompleted = payload
					}
					mu.Unlock()
				}
			}()
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runExecuted := make(chan struct{})

	worker, err := NewBenchmarkAgentWorker(AgentWorkerOptions{
		RestURL:           server.URL,
		Creds:             RestCreds{Username: "admin", Password: "changeme"},
		AgentName:         "test-agent",
		Hostname:          "testhost",
		IPAddress:         "10.0.0.1",
		HeartbeatInterval: 50 * time.Millisecond,
		RunHandler: func(ctx context.Context, runID int, config map[string]any) (map[string]any, error) {
			close(runExecuted)
			return map[string]any{
				"throughput_rps": 45.5,
				"mean_ttft_ms":   12.3,
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewBenchmarkAgentWorker failed: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- worker.Run(ctx)
	}()

	// Wait for connection
	time.Sleep(150 * time.Millisecond)
	if !worker.IsConnected() {
		t.Errorf("expected worker to be connected")
	}

	// Dispatch a run from the server
	mu.Lock()
	conn := wsConn
	mu.Unlock()

	if conn == nil {
		t.Fatalf("server did not receive WebSocket connection")
	}

	err = conn.WriteJSON(map[string]any{
		"type":   "run",
		"run_id": 101,
		"config": map[string]any{
			"model": "meta-llama/Llama-3-8B-Instruct",
			"url":   "http://10.10.10.100:8000/v1/chat/completions",
		},
	})
	if err != nil {
		t.Fatalf("server write run command failed: %v", err)
	}

	// Wait for run execution
	select {
	case <-runExecuted:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for run execution callback")
	}

	// Wait for completion message to reach server
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	hbCount := receivedHeartbeats
	completed := receivedCompleted
	mu.Unlock()

	if hbCount == 0 {
		t.Errorf("expected at least 1 heartbeat, got 0")
	}

	if completed == nil {
		t.Fatalf("expected run_completed payload, got nil")
	}
	if completed["run_id"] != float64(101) {
		t.Errorf("expected run_id 101, got %v", completed["run_id"])
	}

	// Cancel context to cleanly shut down
	cancel()
	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			t.Errorf("unexpected error on shutdown: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Errorf("worker did not shut down cleanly")
	}
}

func TestBenchmarkAgentWorker_FatalAuthClose4401(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	var dials int
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/auth/login":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "fake-jwt-token"})
		case r.URL.Path == "/api/benchmarks/agents":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 10, "name": "fatal-agent"})
		case strings.HasPrefix(r.URL.Path, "/ws/benchmarks/agents/"):
			mu.Lock()
			dials++
			mu.Unlock()
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			// Close with code 4401
			_ = conn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(4401, "agent_id claim mismatch"),
				time.Now().Add(time.Second),
			)
			_ = conn.Close()
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	worker, err := NewBenchmarkAgentWorker(AgentWorkerOptions{
		RestURL:    server.URL,
		Creds:      RestCreds{Username: "admin", Password: "changeme"},
		AgentName:  "fatal-agent",
		AgentToken: "configured-invalid-token",
	})
	if err != nil {
		t.Fatalf("NewBenchmarkAgentWorker failed: %v", err)
	}

	runErr := worker.Run(ctx)
	if runErr == nil {
		t.Fatalf("expected fatal error from Run, got nil")
	}

	var authErr *WSAuthError
	if !errors.As(runErr, &authErr) {
		t.Fatalf("expected *WSAuthError, got %T: %v", runErr, runErr)
	}
	if authErr.StatusCode != 4401 {
		t.Errorf("expected close code 4401, got %d", authErr.StatusCode)
	}
	if !authErr.IsClose {
		t.Errorf("expected IsClose = true")
	}

	mu.Lock()
	totalDials := dials
	mu.Unlock()
	if totalDials != 1 {
		t.Errorf("expected exactly 1 dial (no reconnect loop on fatal auth error), got %d", totalDials)
	}
}

func TestBenchmarkAgentWorker_FatalHandshake401(t *testing.T) {
	var dials int
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/auth/login":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "fake-jwt-token"})
		case r.URL.Path == "/api/benchmarks/agents":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 11, "name": "handshake-agent"})
		case strings.HasPrefix(r.URL.Path, "/ws/benchmarks/agents/"):
			mu.Lock()
			dials++
			mu.Unlock()
			http.Error(w, "Unauthorized: invalid agent token", http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	worker, err := NewBenchmarkAgentWorker(AgentWorkerOptions{
		RestURL:    server.URL,
		Creds:      RestCreds{Username: "admin", Password: "changeme"},
		AgentName:  "handshake-agent",
		AgentToken: "configured-bad-token",
	})
	if err != nil {
		t.Fatalf("NewBenchmarkAgentWorker failed: %v", err)
	}

	runErr := worker.Run(ctx)
	if runErr == nil {
		t.Fatalf("expected error from Run, got nil")
	}
	var authErr *WSAuthError
	if !errors.As(runErr, &authErr) {
		t.Fatalf("expected *WSAuthError, got %T: %v", runErr, runErr)
	}
	if authErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", authErr.StatusCode)
	}
	if authErr.IsClose {
		t.Errorf("expected IsClose = false for HTTP handshake failure")
	}
	mu.Lock()
	totalDials := dials
	mu.Unlock()
	if totalDials != 1 {
		t.Errorf("expected exactly 1 dial, got %d", totalDials)
	}
}

func TestBenchmarkAgentWorker_CachedTokenRejection_RemintsAndReconnects(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	tmpDir := t.TempDir()
	agentName := "remint-agent"
	agentID := 12
	cachedToken := "cached-stale-token"
	freshMintedToken := "fresh-minted-token"

	var wsDials int
	var mintCalls int
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/auth/login":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "operator-jwt"})
		case r.URL.Path == "/api/benchmarks/agents":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": agentID, "name": agentName})
		case r.URL.Path == fmt.Sprintf("/api/benchmarks/agents/%d/token", agentID):
			mu.Lock()
			mintCalls++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(MintAgentTokenResponse{
				Token:     freshMintedToken,
				ExpiresAt: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
			})
		case strings.HasPrefix(r.URL.Path, "/ws/benchmarks/agents/"):
			mu.Lock()
			wsDials++
			currentDial := wsDials
			mu.Unlock()

			tokenQuery := r.URL.Query().Get("token")
			if currentDial == 1 {
				// First dial uses the stale cached token: reject with 4401
				if tokenQuery != cachedToken {
					t.Errorf("first dial: expected cached token %q, got %q", cachedToken, tokenQuery)
				}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				_ = conn.WriteControl(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(4401, "expired or revoked token"),
					time.Now().Add(time.Second),
				)
				_ = conn.Close()
				return
			}

			// Second dial should use the fresh minted token: succeed!
			if tokenQuery != freshMintedToken {
				t.Errorf("second dial: expected fresh minted token %q, got %q", freshMintedToken, tokenQuery)
			}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			go func() {
				defer conn.Close() // #nosec G104
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
				}
			}()
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	// Seed cache with stale token
	if err := WriteAgentToken(tmpDir, server.URL, agentName, cachedToken, time.Now().Add(time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatalf("seed cache failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	worker, err := NewBenchmarkAgentWorker(AgentWorkerOptions{
		RestURL:           server.URL,
		Creds:             RestCreds{Username: "admin", Password: "changeme"},
		AgentName:         agentName,
		WorkspaceDir:      tmpDir,
		HeartbeatInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewBenchmarkAgentWorker failed: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- worker.Run(ctx)
	}()

	// Wait for worker to connect successfully on second attempt
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if worker.IsConnected() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !worker.IsConnected() {
		t.Fatalf("worker failed to reconnect and connect with fresh token")
	}

	mu.Lock()
	totalMintCalls := mintCalls
	totalWSDials := wsDials
	mu.Unlock()

	if totalMintCalls != 1 {
		t.Errorf("expected 1 mint call after cached token rejection, got %d", totalMintCalls)
	}
	if totalWSDials < 2 {
		t.Errorf("expected at least 2 WS dials, got %d", totalWSDials)
	}

	// Verify fresh token is now in cache
	cached, err := ReadAgentToken(tmpDir, server.URL, agentName)
	if err != nil || cached != freshMintedToken {
		t.Errorf("expected cache to have fresh token %q, got %q (err: %v)", freshMintedToken, cached, err)
	}

	cancel()
	<-errCh
}

func TestBenchmarkAgentWorker_OrdinaryDialError_Retried(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	var dials int
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/auth/login":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "fake-jwt-token"})
		case r.URL.Path == "/api/benchmarks/agents":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 13, "name": "retry-agent"})
		case strings.HasPrefix(r.URL.Path, "/ws/benchmarks/agents/"):
			mu.Lock()
			dials++
			currentDial := dials
			mu.Unlock()
			if currentDial == 1 {
				// Temporary server error 503
				http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
				return
			}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			go func() {
				defer conn.Close() // #nosec G104
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
				}
			}()
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	worker, err := NewBenchmarkAgentWorker(AgentWorkerOptions{
		RestURL:           server.URL,
		Creds:             RestCreds{Username: "admin", Password: "changeme"},
		AgentName:         "retry-agent",
		AgentToken:        "agent-token-xyz",
		HeartbeatInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewBenchmarkAgentWorker failed: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- worker.Run(ctx)
	}()

	// Wait for worker to connect on retry
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if worker.IsConnected() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !worker.IsConnected() {
		t.Fatalf("worker failed to reconnect after temporary 503 dial error")
	}

	mu.Lock()
	totalDials := dials
	mu.Unlock()
	if totalDials < 2 {
		t.Errorf("expected at least 2 dials, got %d", totalDials)
	}

	cancel()
	<-errCh
}
