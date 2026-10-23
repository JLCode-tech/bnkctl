package bnkscan

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SessionHeader is the MCP Streamable HTTP session header. BNK's
// MODEL_CONTEXT_PROTOCOL persistence pins on the same value.
const SessionHeader = "Mcp-Session-Id"

// ProtocolVersion is the MCP protocol version ProbeTools negotiates.
const ProtocolVersion = "2025-03-26"

// ProbeResult is what ProbeTools learned from an endpoint.
type ProbeResult struct {
	Tools      []ToolDef `json:"tools"`
	SessionID  string    `json:"sessionId,omitempty"`
	ServerName string    `json:"serverName,omitempty"`
	Version    string    `json:"serverVersion,omitempty"`
	Protocol   string    `json:"protocolVersion,omitempty"`
	Latency    time.Duration
}

// ProbeTools speaks MCP over Streamable HTTP to url: initialize,
// notifications/initialized, tools/list. headers are added to every request
// (Authorization, Host). Responses may be JSON or a text/event-stream carrying
// one JSON-RPC message per data: line. client nil uses a 15s default.
func ProbeTools(ctx context.Context, client *http.Client, url string, headers map[string]string) (ProbeResult, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	var out ProbeResult
	start := time.Now()

	init := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "awsbnkctl", "version": "forge-scan"},
		},
	}
	msg, hdr, err := rpc(ctx, client, url, headers, "", init)
	if err != nil {
		return out, fmt.Errorf("initialize: %w", err)
	}
	out.SessionID = hdr.Get(SessionHeader)
	var initRes struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	_ = json.Unmarshal(msg.Result, &initRes)
	out.Protocol = initRes.ProtocolVersion
	out.ServerName = initRes.ServerInfo.Name
	out.Version = initRes.ServerInfo.Version

	// notifications/initialized has no id and no response body worth reading.
	_, _, _ = rpc(ctx, client, url, headers, out.SessionID, map[string]any{
		"jsonrpc": "2.0", "method": "notifications/initialized",
	})

	msg, _, err = rpc(ctx, client, url, headers, out.SessionID, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{},
	})
	if err != nil {
		return out, fmt.Errorf("tools/list: %w", err)
	}
	var listRes struct {
		Tools []ToolDef `json:"tools"`
	}
	if err := json.Unmarshal(msg.Result, &listRes); err != nil {
		return out, fmt.Errorf("tools/list: decode result: %w", err)
	}
	out.Tools = listRes.Tools
	out.Latency = time.Since(start)
	return out, nil
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// rpc posts one JSON-RPC message and returns the first response message that
// carries a result or error. Notifications return an empty message.
func rpc(ctx context.Context, client *http.Client, url string, headers map[string]string, session string, body any) (rpcMessage, http.Header, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return rpcMessage{}, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return rpcMessage{}, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set(SessionHeader, session)
	}
	for k, v := range headers {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return rpcMessage{}, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		return rpcMessage{}, resp.Header, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return rpcMessage{}, resp.Header, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var msg rpcMessage
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		msg, err = firstSSEMessage(resp.Body)
	} else {
		err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&msg)
	}
	if err != nil {
		return rpcMessage{}, resp.Header, fmt.Errorf("decode response: %w", err)
	}
	if msg.Error != nil {
		return msg, resp.Header, fmt.Errorf("JSON-RPC error %d: %s", msg.Error.Code, msg.Error.Message)
	}
	return msg, resp.Header, nil
}

// firstSSEMessage reads an event stream until a data: payload that is a
// JSON-RPC response (result or error) appears.
func firstSSEMessage(r io.Reader) (rpcMessage, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var data []string
	flush := func() (rpcMessage, bool) {
		if len(data) == 0 {
			return rpcMessage{}, false
		}
		payload := strings.Join(data, "\n")
		data = nil
		var msg rpcMessage
		if err := json.Unmarshal([]byte(payload), &msg); err != nil {
			return rpcMessage{}, false
		}
		if len(msg.Result) > 0 || msg.Error != nil {
			return msg, true
		}
		return rpcMessage{}, false
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if msg, ok := flush(); ok {
				return msg, nil
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if msg, ok := flush(); ok {
		return msg, nil
	}
	if err := sc.Err(); err != nil {
		return rpcMessage{}, err
	}
	return rpcMessage{}, errors.New("event stream ended without a JSON-RPC response")
}
