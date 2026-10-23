package bnkscan

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeMCP answers initialize / tools/list either as JSON or as an event
// stream, and records the session header and Authorization it received.
type fakeMCP struct {
	sse      bool
	sessions []string
	auth     []string
	hosts    []string
}

func (f *fakeMCP) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal(body, &req)
	f.sessions = append(f.sessions, r.Header.Get(SessionHeader))
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.hosts = append(f.hosts, r.Host)

	var result any
	switch req.Method {
	case "initialize":
		w.Header().Set(SessionHeader, "sess-123")
		result = map[string]any{
			"protocolVersion": ProtocolVersion,
			"serverInfo":      map[string]any{"name": "finance-tool", "version": "1.0"},
		}
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		return
	case "tools/list":
		result = map[string]any{"tools": []map[string]any{
			{"name": "forecast", "description": "30-day forecast", "inputSchema": map[string]any{"type": "object"}},
			{"name": "get_account_balance"},
		}}
	default:
		http.Error(w, "unknown method", http.StatusBadRequest)
		return
	}
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(req.ID), "result": result})
	if f.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, ": keepalive\n\nevent: message\ndata: %s\n\n", msg)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(msg)
}

func TestProbeTools(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			f := &fakeMCP{sse: sse}
			ts := httptest.NewServer(http.HandlerFunc(f.handler))
			defer ts.Close()

			res, err := ProbeTools(context.Background(), ts.Client(), ts.URL+"/v1/mcp/forecast",
				map[string]string{"Authorization": "Bearer demo", "Host": "bnk-ingress.bnk-demo.internal"})
			if err != nil {
				t.Fatal(err)
			}
			if res.SessionID != "sess-123" || res.ServerName != "finance-tool" || res.Protocol != ProtocolVersion {
				t.Errorf("result = %+v", res)
			}
			if len(res.Tools) != 2 || res.Tools[0].Name != "forecast" || res.Tools[0].Description != "30-day forecast" || len(res.Tools[0].InputSchema) == 0 {
				t.Errorf("tools = %+v", res.Tools)
			}
			// initialize has no session yet; the two later calls carry the one the server minted.
			if strings.Join(f.sessions, ",") != ",sess-123,sess-123" {
				t.Errorf("sessions seen = %v", f.sessions)
			}
			for i, a := range f.auth {
				if a != "Bearer demo" {
					t.Errorf("request %d Authorization = %q", i, a)
				}
			}
			if f.hosts[0] != "bnk-ingress.bnk-demo.internal" {
				t.Errorf("Host = %q", f.hosts[0])
			}
		})
	}
}

func TestProbeTools_HTTPErrorAndRPCError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"rate limit exceeded"}}`, http.StatusTooManyRequests)
	}))
	defer ts.Close()
	if _, err := ProbeTools(context.Background(), ts.Client(), ts.URL, nil); err == nil || !strings.Contains(err.Error(), "HTTP 429") {
		t.Errorf("err = %v, want HTTP 429", err)
	}

	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`))
	}))
	defer ts2.Close()
	if _, err := ProbeTools(context.Background(), ts2.Client(), ts2.URL, nil); err == nil || !strings.Contains(err.Error(), "method not found") {
		t.Errorf("err = %v, want JSON-RPC error", err)
	}
}

func TestFirstSSEMessage_SkipsNotifications(t *testing.T) {
	stream := "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\n" +
		"data: \"id\":2,\"result\":{\"tools\":[]}}\n\n"
	msg, err := firstSSEMessage(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if string(msg.Result) != `{"tools":[]}` {
		t.Errorf("result = %s", msg.Result)
	}
	if _, err := firstSSEMessage(strings.NewReader("data: {\"jsonrpc\":\"2.0\",\"method\":\"x\"}\n\n")); err == nil {
		t.Error("expected an error when the stream has no response")
	}
}
