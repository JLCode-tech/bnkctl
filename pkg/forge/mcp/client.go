// Package mcp is a minimal MCP (Model Context Protocol) client that speaks
// JSON-RPC 2.0 over Streamable HTTP — enough to talk to the BNK-Forge MCP
// server in its stateless mode (FastMCP with stateless_http=True,
// json_response=True).
//
// Each Call performs a fresh initialize → notifications/initialized →
// tools/call cycle, because the server is stateless.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// ProtocolVersion is the MCP protocol revision azrbnkctl targets.
const ProtocolVersion = "2024-11-05"

// Client is a minimal MCP-over-HTTP client.
type Client struct {
	endpoint  string
	http      *http.Client
	clientID  string
	idCounter atomic.Int64
}

// New returns a Client targeting the given MCP endpoint (e.g. "http://localhost:8081/mcp/").
func New(endpoint string, timeout time.Duration) *Client {
	return &Client{
		endpoint: endpoint,
		http:     &http.Client{Timeout: timeout},
		clientID: "azrbnkctl",
	}
}

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcErr) Error() string {
	return fmt.Sprintf("mcp rpc error %d: %s", e.Code, e.Message)
}

// ToolCallResult is the shape of an MCP tools/call result.
type ToolCallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError,omitempty"`
}

// Text returns the concatenated text of all text-typed content blocks.
func (r ToolCallResult) Text() string {
	var b bytes.Buffer
	for _, c := range r.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// UnmarshalToolJSON unmarshals the concatenated text blocks of the tool call result as JSON into dst.
func (r ToolCallResult) UnmarshalToolJSON(dst any) error {
	return json.Unmarshal([]byte(r.Text()), dst)
}

// CallTool performs init+notification+tools/call and returns the tool's result.
func (c *Client) CallTool(ctx context.Context, name string, args any) (ToolCallResult, error) {
	if c.endpoint == "" {
		return ToolCallResult{}, errors.New("mcp: endpoint is empty")
	}

	// 1) initialize handshake
	initParams, _ := json.Marshal(map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]string{
			"name":    c.clientID,
			"version": "1.0",
		},
	})
	_, sessionID, err := c.post(ctx, "initialize", initParams, true, "")
	if err != nil {
		return ToolCallResult{}, fmt.Errorf("mcp init %s: %w", c.endpoint, err)
	}

	// 2) initialized notification
	_ = c.notify(ctx, "notifications/initialized", nil, sessionID)

	// 3) tools/call
	toolParams, err := json.Marshal(map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return ToolCallResult{}, fmt.Errorf("marshal args: %w", err)
	}

	rawResult, _, err := c.post(ctx, "tools/call", toolParams, true, sessionID)
	if err != nil {
		return ToolCallResult{}, fmt.Errorf("mcp %s: %w", name, err)
	}

	var res ToolCallResult
	if err := json.Unmarshal(rawResult, &res); err != nil {
		return ToolCallResult{}, fmt.Errorf("decode tool result: %w", err)
	}
	if res.IsError {
		return res, fmt.Errorf("mcp tool %s returned error: %s", name, res.Text())
	}
	return res, nil
}

func (c *Client) nextID() int64 {
	return c.idCounter.Add(1)
}

func (c *Client) post(ctx context.Context, method string, params json.RawMessage, needResp bool, sessionID string) (json.RawMessage, string, error) {
	reqObj := rpcReq{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	if needResp {
		reqObj.ID = c.nextID()
	}

	b, err := json.Marshal(reqObj)
	if err != nil {
		return nil, sessionID, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, sessionID, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", sessionID)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, sessionID, err
	}
	defer resp.Body.Close()

	if sid := strings.TrimSpace(resp.Header.Get("Mcp-Session-Id")); sid != "" {
		sessionID = sid
	}

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, sessionID, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, sessionID, fmt.Errorf("http %d: %s", resp.StatusCode, string(rawBody))
	}

	if !needResp {
		return nil, sessionID, nil
	}

	var rpcRespObj rpcResp
	if err := decodeRPCResponse(rawBody, &rpcRespObj); err != nil {
		return nil, sessionID, fmt.Errorf("decode json-rpc: %w", err)
	}
	if rpcRespObj.Error != nil {
		return nil, sessionID, rpcRespObj.Error
	}
	return rpcRespObj.Result, sessionID, nil
}

func decodeRPCResponse(rawBody []byte, dst *rpcResp) error {
	trimmed := bytes.TrimSpace(rawBody)
	if len(trimmed) == 0 {
		return errors.New("empty response body")
	}
	if err := json.Unmarshal(trimmed, dst); err == nil && (dst.JSONRPC != "" || len(dst.Result) > 0 || dst.Error != nil) {
		return nil
	}
	// Fallback: parse Server-Sent Events (text/event-stream) lines starting with "data:"
	var lastValid *rpcResp
	for _, line := range strings.Split(string(trimmed), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var candidate rpcResp
		if err := json.Unmarshal([]byte(payload), &candidate); err == nil {
			if len(candidate.Result) > 0 || candidate.Error != nil || candidate.JSONRPC != "" {
				c := candidate
				lastValid = &c
			}
		}
	}
	if lastValid != nil {
		*dst = *lastValid
		return nil
	}
	return json.Unmarshal(trimmed, dst)
}

func (c *Client) notify(ctx context.Context, method string, params json.RawMessage, sessionID string) error {
	_, _, err := c.post(ctx, method, params, false, sessionID)
	return err
}
