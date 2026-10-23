package forge

// agent_ws.go — persistent WebSocket client for Forge benchmark agents.
//
// Forge coordinates benchmark runs by dispatching run commands over an active
// WebSocket connection at /ws/benchmarks/agents/{agent_id}?token=<jwt>.
//
// The agent worker:
//   1. Authenticates to Forge via REST and gets a JWT bearer token.
//   2. Registers or resolves the agent_id (POST /api/benchmarks/agents).
//   3. Connects via WebSocket to /ws/benchmarks/agents/{agent_id}?token=<jwt>.
//   4. Sends periodic heartbeats (default every 15s) to maintain CONNECTED status in Forge.
//   5. Listens for "run", "cancel", and "ping" commands.
//   6. Dispatches "run" commands to a registered RunHandler and reports
//      "run_completed" or "run_failed" back over the socket.
//   7. Automatically reconnects with exponential backoff on network disconnects.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WSAuthError represents an authentication failure from Forge: an HTTP 401/403
// response during WebSocket dial, or a WebSocket close frame with code 4401/4403.
type WSAuthError struct {
	StatusCode int
	Body       string
	IsClose    bool
}

func (e *WSAuthError) Error() string {
	if e.IsClose {
		return fmt.Sprintf("websocket close code %d: %s", e.StatusCode, e.Body)
	}
	return fmt.Sprintf("dial ws auth rejected (status %d): %s", e.StatusCode, e.Body)
}

// DefaultHeartbeatInterval is how often the agent sends heartbeats to Forge.
const DefaultHeartbeatInterval = 15 * time.Second

// RunHandler is called when Forge sends a "run" command down the WebSocket.
// It receives the Forge run_id and the configuration dict (which maps directly
// to aiperf options). It returns the parsed aiperf result map or an error.
type RunHandler func(ctx context.Context, runID int, config map[string]any) (map[string]any, error)

// CancelHandler is called when Forge sends a "cancel" command.
type CancelHandler func(runID int)

// AgentWorkerOptions configures a Forge BenchmarkAgentWorker.
type AgentWorkerOptions struct {
	// RestURL is the Forge REST base URL (e.g. "http://localhost:8000").
	RestURL string
	// Creds are the Forge REST credentials.
	Creds RestCreds
	// AgentName is the name of the agent in Forge (e.g. "awsbnkctl-jumphost-bnk-singapore").
	AgentName string
	// Hostname is the host/instance-id reported to Forge on registration.
	Hostname string
	// IPAddress is the private IP reported to Forge on registration.
	IPAddress string
	// Tags are metadata tags attached to the agent record.
	Tags map[string]string
	// Capabilities list capabilities (e.g. ["aiperf"]).
	Capabilities []string
	// HeartbeatInterval is the heartbeat cadence (default: 15s).
	HeartbeatInterval time.Duration
	// AgentToken optionally overrides automatic agent token resolution (env > yaml > mint).
	AgentToken string
	// WorkspaceDir is the workspace directory where forge_agent_token.json is persisted.
	WorkspaceDir string
	// RunHandler executes a benchmark run when commanded by Forge.
	RunHandler RunHandler
	// CancelHandler cancels an in-flight run.
	CancelHandler CancelHandler
	// Logger is an optional log sink.
	Logger func(format string, args ...any)
}

// BenchmarkAgentWorker manages the persistent connection to Forge.
type BenchmarkAgentWorker struct {
	opts          AgentWorkerOptions
	agentID       int
	operatorToken string
	token         string
	tokenSource   TokenSource
	wsBase        string

	mu        sync.Mutex
	conn      *websocket.Conn
	connected bool
	cancelMap map[int]context.CancelFunc
}

// NewBenchmarkAgentWorker initializes a new BenchmarkAgentWorker.
func NewBenchmarkAgentWorker(opts AgentWorkerOptions) (*BenchmarkAgentWorker, error) {
	if opts.RestURL == "" {
		return nil, fmt.Errorf("forge.NewBenchmarkAgentWorker: RestURL is required")
	}
	if opts.AgentName == "" {
		return nil, fmt.Errorf("forge.NewBenchmarkAgentWorker: AgentName is required")
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if opts.Logger == nil {
		opts.Logger = func(string, ...any) {}
	}

	return &BenchmarkAgentWorker{
		opts:      opts,
		cancelMap: make(map[int]context.CancelFunc),
	}, nil
}

// AgentID returns the registered Forge agent ID.
func (w *BenchmarkAgentWorker) AgentID() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.agentID
}

// IsConnected returns true if the WebSocket is currently open.
func (w *BenchmarkAgentWorker) IsConnected() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.connected
}

// Run blocks and runs the agent loop until ctx is cancelled.
func (w *BenchmarkAgentWorker) Run(ctx context.Context) error {
	base := strings.TrimRight(w.opts.RestURL, "/")

	// 1. Authenticate to Forge. An explicit agent token is enough to
	// re-register its own agent, so no operator login is needed then (the
	// token cannot be re-minted without one; a rejected token ends the daemon).
	var token string
	if w.opts.AgentToken == "" {
		var err error
		token, err = restLogin(ctx, base, w.opts.Creds.restUsername(), w.opts.Creds.restPassword())
		if err != nil {
			return fmt.Errorf("forge agent login: %w", err)
		}
		w.mu.Lock()
		w.operatorToken = token
		w.mu.Unlock()
	}

	// 2. Register/upsert the agent record
	agentResp, err := RegisterBenchmarkAgent(ctx, BenchmarkAgentOptions{
		RestURL:      w.opts.RestURL,
		Creds:        w.opts.Creds,
		Name:         w.opts.AgentName,
		Hostname:     w.opts.Hostname,
		IPAddress:    w.opts.IPAddress,
		Tags:         w.opts.Tags,
		Capabilities: w.opts.Capabilities,
		Token:        w.opts.AgentToken,
	})
	if err != nil {
		return fmt.Errorf("forge agent register: %w", err)
	}

	w.mu.Lock()
	w.agentID = agentResp.ID
	w.mu.Unlock()

	w.opts.Logger("Registered as Forge BenchmarkAgent #%d (%s)", agentResp.ID, agentResp.Name)

	// 3. Acquire agent token (env > yaml > cache > mint)
	tokenRes, err := AcquireAgentToken(ctx, AcquireAgentTokenOptions{
		RestURL:       base,
		OperatorToken: token,
		AgentID:       agentResp.ID,
		AgentName:     w.opts.AgentName,
		ExplicitToken: w.opts.AgentToken,
		WorkspaceDir:  w.opts.WorkspaceDir,
	})
	if err != nil {
		return err
	}

	w.mu.Lock()
	w.token = tokenRes.Token
	w.tokenSource = tokenRes.Source
	w.mu.Unlock()

	// Determine WS base URL
	wsScheme := "ws"
	if strings.HasPrefix(strings.ToLower(base), "https://") {
		wsScheme = "wss"
	}
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("parse Forge base URL: %w", err)
	}
	w.wsBase = fmt.Sprintf("%s://%s", wsScheme, u.Host)

	// 4. Connect loop with backoff
	backoff := 2 * time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		connErr := w.connectAndServe(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if connErr != nil {
			var authErr *WSAuthError
			if errors.As(connErr, &authErr) {
				w.mu.Lock()
				src := w.tokenSource
				w.mu.Unlock()
				if src == TokenSourceCached {
					w.opts.Logger("Forge rejected cached agent token (%v) — re-minting token...", authErr)
					if remintErr := w.remintAgentToken(ctx); remintErr == nil {
						// Re-mint succeeded; reconnect immediately without backoff
						continue
					} else {
						w.opts.Logger("Failed to re-mint agent token: %v", remintErr)
					}
				}
				w.logFatalAuthRejection(authErr)
				return connErr
			}

			w.opts.Logger("Agent WebSocket disconnected (%v), reconnecting in %v...", connErr, backoff)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func (w *BenchmarkAgentWorker) remintAgentToken(ctx context.Context) error {
	base := strings.TrimRight(w.opts.RestURL, "/")
	if w.opts.WorkspaceDir != "" {
		_ = DeleteAgentToken(w.opts.WorkspaceDir, base, w.opts.AgentName)
	}
	w.mu.Lock()
	opToken := w.operatorToken
	agentID := w.agentID
	w.mu.Unlock()

	mintResp, err := MintAgentToken(ctx, base, opToken, agentID)
	if err != nil {
		return err
	}
	if w.opts.WorkspaceDir != "" {
		_ = WriteAgentToken(w.opts.WorkspaceDir, base, w.opts.AgentName, mintResp.Token, mintResp.ExpiresAt)
	}
	w.mu.Lock()
	w.token = mintResp.Token
	w.tokenSource = TokenSourceMinted
	w.mu.Unlock()
	return nil
}

func (w *BenchmarkAgentWorker) logFatalAuthRejection(authErr *WSAuthError) {
	w.mu.Lock()
	src := string(w.tokenSource)
	agentID := w.agentID
	w.mu.Unlock()
	if src == "" {
		src = "unknown"
	}
	codeStr := fmt.Sprintf("HTTP %d", authErr.StatusCode)
	if authErr.IsClose {
		codeStr = fmt.Sprintf("close code %d", authErr.StatusCode)
	}
	msg := fmt.Sprintf(
		"Forge WebSocket authentication rejected (%s): agent #%d rejected using %s token; fix: ensure token has 'agent' role with matching agent_id claim via POST /api/benchmarks/agents/%d/token or set AZRBNKCTL_FORGE_AGENT_TOKEN / cluster.yaml forge.agent_token",
		codeStr, agentID, src, agentID,
	)
	w.opts.Logger("[FATAL] %s", msg)
	fmt.Fprintf(os.Stderr, "Error: %s\n", msg)
}

// connectAndServe handles a single connection session.
func (w *BenchmarkAgentWorker) connectAndServe(ctx context.Context) error {
	wsURL := fmt.Sprintf("%s/ws/benchmarks/agents/%d", w.wsBase, w.agentID)
	if w.token != "" {
		q := url.Values{}
		q.Set("token", w.token)
		wsURL = fmt.Sprintf("%s?%s", wsURL, q.Encode())
	}

	w.opts.Logger("Connecting to Forge WebSocket: %s/ws/benchmarks/agents/%d ...", w.wsBase, w.agentID)

	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		Proxy:            http.ProxyFromEnvironment,
	}

	reqHeaders := http.Header{}
	if w.token != "" {
		reqHeaders.Set("Authorization", "Bearer "+w.token)
	}

	conn, resp, err := dialer.DialContext(ctx, wsURL, reqHeaders)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				return &WSAuthError{
					StatusCode: resp.StatusCode,
					Body:       strings.TrimSpace(string(body)),
					IsClose:    false,
				}
			}
			return fmt.Errorf("dial ws (status %d): %w: %s", resp.StatusCode, err, string(body))
		}
		return fmt.Errorf("dial ws: %w", err)
	}
	defer conn.Close() // #nosec G104

	w.mu.Lock()
	w.conn = conn
	w.connected = true
	w.mu.Unlock()

	w.opts.Logger("Forge WebSocket connected! Agent #%d is now ONLINE (connected)", w.agentID)

	sessionCtx, sessionCancel := context.WithCancel(ctx)
	defer sessionCancel()

	// Start heartbeat goroutine
	hbErrCh := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(w.opts.HeartbeatInterval)
		defer ticker.Stop()

		for {
			select {
			case <-sessionCtx.Done():
				return
			case <-ticker.C:
				hbMsg := map[string]any{
					"type":   "heartbeat",
					"status": "connected",
				}
				if err := w.sendJSON(hbMsg); err != nil {
					hbErrCh <- fmt.Errorf("heartbeat send failed: %w", err)
					return
				}
			}
		}
	}()

	// Message receive loop
	recvErrCh := make(chan error, 1)
	go func() {
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				var closeErr *websocket.CloseError
				if errors.As(err, &closeErr) {
					if closeErr.Code == 4401 || closeErr.Code == 4403 {
						recvErrCh <- &WSAuthError{
							StatusCode: closeErr.Code,
							Body:       closeErr.Text,
							IsClose:    true,
						}
						return
					}
				}
				recvErrCh <- err
				return
			}

			var raw map[string]any
			if err := json.Unmarshal(message, &raw); err != nil {
				w.opts.Logger("Ignoring invalid JSON from Forge: %s", string(message))
				continue
			}

			msgType, _ := raw["type"].(string)
			switch msgType {
			case "run":
				w.handleRunCommand(sessionCtx, raw)
			case "cancel":
				w.handleCancelCommand(raw)
			case "ping":
				_ = w.sendJSON(map[string]any{"type": "pong"})
			default:
				w.opts.Logger("Received unhandled message type '%s' from Forge", msgType)
			}
		}
	}()

	select {
	case err := <-hbErrCh:
		w.setDisconnected()
		return err
	case err := <-recvErrCh:
		w.setDisconnected()
		return err
	case <-ctx.Done():
		w.setDisconnected()
		return ctx.Err()
	}
}

func (w *BenchmarkAgentWorker) setDisconnected() {
	w.mu.Lock()
	w.connected = false
	w.conn = nil
	w.mu.Unlock()
}

func (w *BenchmarkAgentWorker) sendJSON(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn == nil {
		return errors.New("websocket not connected")
	}
	return w.conn.WriteJSON(v)
}

// SendProgress sends a progress event to Forge for an active run.
func (w *BenchmarkAgentWorker) SendProgress(runID int, phase string, details map[string]any) error {
	payload := map[string]any{
		"type":    "progress",
		"run_id":  runID,
		"phase":   phase,
		"details": details,
	}
	return w.sendJSON(payload)
}

// SendCompleted sends the final benchmark result JSON to Forge.
func (w *BenchmarkAgentWorker) SendCompleted(runID int, result map[string]any) error {
	payload := map[string]any{
		"type":   "run_completed",
		"run_id": runID,
		"result": result,
	}
	return w.sendJSON(payload)
}

// SendFailed reports a run failure to Forge.
func (w *BenchmarkAgentWorker) SendFailed(runID int, errMsg string) error {
	payload := map[string]any{
		"type":   "run_failed",
		"run_id": runID,
		"error":  errMsg,
	}
	return w.sendJSON(payload)
}

func (w *BenchmarkAgentWorker) handleRunCommand(parentCtx context.Context, raw map[string]any) {
	runIDRaw, ok := raw["run_id"]
	if !ok {
		w.opts.Logger("Received 'run' command without run_id: %+v", raw)
		return
	}
	var runID int
	switch v := runIDRaw.(type) {
	case float64:
		runID = int(v)
	case int:
		runID = v
	default:
		w.opts.Logger("Invalid run_id format: %v", runIDRaw)
		return
	}

	config, _ := raw["config"].(map[string]any)
	if config == nil {
		config = make(map[string]any)
	}

	w.opts.Logger("Forge UI dispatched Benchmark Run #%d (model=%v, url=%v)", runID, config["model"], config["url"])

	if w.opts.RunHandler == nil {
		_ = w.SendFailed(runID, "Agent has no RunHandler configured")
		return
	}

	// Create run context with cancellation support
	runCtx, runCancel := context.WithCancel(parentCtx)
	w.mu.Lock()
	w.cancelMap[runID] = runCancel
	w.mu.Unlock()

	go func() {
		defer func() {
			w.mu.Lock()
			delete(w.cancelMap, runID)
			w.mu.Unlock()
			runCancel()
		}()

		_ = w.SendProgress(runID, "starting", map[string]any{"status": "Agent preparing execution environment"})

		result, err := w.opts.RunHandler(runCtx, runID, config)
		if err != nil {
			w.opts.Logger("Run #%d failed: %v", runID, err)
			_ = w.SendFailed(runID, err.Error())
			return
		}

		w.opts.Logger("Run #%d completed successfully — sending results to Forge", runID)
		if err := w.SendCompleted(runID, result); err != nil {
			w.opts.Logger("Failed to send run_completed for run #%d: %v", runID, err)
		}
	}()
}

func (w *BenchmarkAgentWorker) handleCancelCommand(raw map[string]any) {
	runIDRaw, ok := raw["run_id"]
	if !ok {
		return
	}
	var runID int
	switch v := runIDRaw.(type) {
	case float64:
		runID = int(v)
	case int:
		runID = v
	default:
		return
	}

	w.opts.Logger("Forge requested cancellation of Run #%d", runID)

	w.mu.Lock()
	cancelFn, exists := w.cancelMap[runID]
	w.mu.Unlock()

	if exists && cancelFn != nil {
		cancelFn()
	}

	if w.opts.CancelHandler != nil {
		w.opts.CancelHandler(runID)
	}
}
