package forge_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JLCode-tech/bnkctl/pkg/forge/mcp"
)

func TestMCPClient_CallTool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept := r.Header.Get("Accept")
		if !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
			http.Error(w, "Not Acceptable: Client must accept both application/json and text/event-stream", http.StatusNotAcceptable)
			return
		}

		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		method, _ := req["method"].(string)
		id := req["id"]

		switch method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-test-123")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"protocolVersion": mcp.ProtocolVersion,
					"serverInfo":      map[string]string{"name": "test-forge", "version": "1.0"},
				},
			})
		case "notifications/initialized":
			if r.Header.Get("Mcp-Session-Id") != "sess-test-123" {
				http.Error(w, "missing Mcp-Session-Id", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		case "tools/call":
			if r.Header.Get("Mcp-Session-Id") != "sess-test-123" {
				http.Error(w, "missing Mcp-Session-Id", http.StatusBadRequest)
				return
			}
			params, _ := req["params"].(map[string]any)
			toolName, _ := params["name"].(string)
			if toolName == "system_version" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result": map[string]any{
						"content": []map[string]string{
							{"type": "text", "text": "2.10.0"},
						},
					},
				})
			} else if toolName == "sse_tool" {
				w.Header().Set("Content-Type", "text/event-stream")
				payload, _ := json.Marshal(map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result": map[string]any{
						"content": []map[string]string{
							{"type": "text", "text": "sse-ok"},
						},
					},
				})
				_, _ = w.Write([]byte("event: message\ndata: " + string(payload) + "\n\n"))
			} else if toolName == "error_tool" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result": map[string]any{
						"isError": true,
						"content": []map[string]string{
							{"type": "text", "text": "simulated tool failure"},
						},
					},
				})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"error": map[string]any{
						"code":    -32601,
						"message": "Method not found: " + toolName,
					},
				})
			}
		default:
			http.Error(w, "unknown method", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	client := mcp.New(srv.URL, 5*time.Second)

	t.Run("successful tool call", func(t *testing.T) {
		res, err := client.CallTool(context.Background(), "system_version", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Text() != "2.10.0" {
			t.Errorf("expected 2.10.0, got %q", res.Text())
		}
	})

	t.Run("successful SSE tool call", func(t *testing.T) {
		res, err := client.CallTool(context.Background(), "sse_tool", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Text() != "sse-ok" {
			t.Errorf("expected sse-ok, got %q", res.Text())
		}
	})

	t.Run("tool returns isError", func(t *testing.T) {
		_, err := client.CallTool(context.Background(), "error_tool", nil)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("tool not found", func(t *testing.T) {
		_, err := client.CallTool(context.Background(), "missing_tool", nil)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}
