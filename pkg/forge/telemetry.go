package forge

// telemetry.go — the governance record BNK emits for every MCP decision and
// the checks that keep it aligned with forge's LLM Observability panel.
//
// The record is written by the governance iRule (examples/agentcore-demo/
// mcp-security-policy.yaml) as one syslog line, `BNKGOV {json}`, on the TMM
// pod's f5-fluentbit sidecar. A Fluent Bit DaemonSet extracts the JSON and
// ships it to Loki as stream job="llm-gateway" with labels model and status;
// forge reads that stream. Fields prefixed with an underscore in the panel
// (tokens, cost) are zero for MCP hops: BNK is in the tool path, not the
// model path.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GovernanceMarker prefixes every governance record in the TMM log.
const GovernanceMarker = "BNKGOV "

// LokiJob is the Loki stream label forge's LLM Observability panel queries.
const LokiJob = "llm-gateway"

// Actions the governance iRule records.
const (
	ActionAllow          = "allow"
	ActionRateLimited    = "rate_limited"
	ActionToolForbidden  = "tool_forbidden"
	ActionBackendRefused = "backend_refused"
)

var knownActions = map[string]bool{
	ActionAllow: true, ActionRateLimited: true, ActionToolForbidden: true, ActionBackendRefused: true,
}

// GovernanceRecord is one BNKGOV record. The first block is the schema forge's
// panel reads; the second is the MCP extension the Phase 4 iRule adds.
type GovernanceRecord struct {
	Job          string  `json:"job"`
	Model        string  `json:"model"`
	Status       string  `json:"status"`
	LatencyMS    int64   `json:"latency_ms"`
	PromptTokens int64   `json:"prompt_tk"`
	CompTokens   int64   `json:"comp_tk"`
	TotalTokens  int64   `json:"total_tk"`
	Cached       int64   `json:"cached"`
	Cost         float64 `json:"cost"`
	UserQuery    string  `json:"userq"`
	Client       string  `json:"client"`
	Caller       string  `json:"caller"`
	Action       string  `json:"action"`
	ReqBody      string  `json:"req_body"`
	RespBody     string  `json:"resp_body"`

	// RPCMethod is the JSON-RPC method (initialize, tools/list, tools/call).
	RPCMethod string `json:"rpc_method,omitempty"`
	// Tool is params.name of a tools/call.
	Tool string `json:"tool,omitempty"`
	// Session is the Mcp-Session-Id the client presented, clipped to 36 chars.
	Session string `json:"session,omitempty"`
	// Transport is http, sse or ws as negotiated by the request headers.
	Transport string `json:"transport,omitempty"`
}

// Throttled reports a 429 decision.
func (r GovernanceRecord) Throttled() bool { return r.Status == "429" }

// StatusCode returns the numeric status, 0 when it is not a number.
func (r GovernanceRecord) StatusCode() int {
	n, _ := strconv.Atoi(r.Status)
	return n
}

// Validate lists the schema violations forge's panel or the Fluent Bit filter
// would trip on: missing stream labels, a non-numeric status, an unknown
// action, or an action that contradicts the status.
func (r GovernanceRecord) Validate() []string {
	var out []string
	if r.Job == "" {
		out = append(out, "job is empty (Loki label)")
	} else if r.Job != LokiJob {
		out = append(out, fmt.Sprintf("job=%q, forge queries job=%q", r.Job, LokiJob))
	}
	if r.Model == "" {
		out = append(out, "model is empty (Loki label)")
	}
	if code := r.StatusCode(); code < 100 || code > 599 {
		out = append(out, fmt.Sprintf("status=%q is not an HTTP status", r.Status))
	}
	if r.Client == "" {
		out = append(out, "client is empty (the collector drops records without it)")
	}
	if r.Action == "" {
		out = append(out, "action is empty")
	} else if !knownActions[r.Action] {
		out = append(out, fmt.Sprintf("action=%q is not one of allow, rate_limited, tool_forbidden, backend_refused", r.Action))
	}
	if r.Action == ActionRateLimited && r.Status != "429" {
		out = append(out, "action=rate_limited with status "+r.Status)
	}
	if r.Action == ActionToolForbidden && r.Status != "403" {
		out = append(out, "action=tool_forbidden with status "+r.Status)
	}
	if r.RPCMethod == "tools/call" && r.Tool == "" {
		out = append(out, "rpc_method=tools/call without tool")
	}
	if r.Transport != "" && r.Transport != "http" && r.Transport != "sse" && r.Transport != "ws" {
		out = append(out, fmt.Sprintf("transport=%q is not http, sse or ws", r.Transport))
	}
	return out
}

// ErrNoGovernanceRecord is returned for lines without a BNKGOV payload.
var ErrNoGovernanceRecord = errors.New("no BNKGOV record in line")

// ParseGovernanceLine extracts the record from a log line. The line may be the
// bare JSON, `BNKGOV {json}`, or the whole container-log wrapper around it.
func ParseGovernanceLine(line string) (GovernanceRecord, error) {
	payload := strings.TrimSpace(line)
	if i := strings.Index(payload, GovernanceMarker); i >= 0 {
		payload = payload[i+len(GovernanceMarker):]
	} else if !strings.HasPrefix(payload, "{") {
		return GovernanceRecord{}, ErrNoGovernanceRecord
	}
	if j := strings.LastIndexByte(payload, '}'); j >= 0 {
		payload = payload[:j+1]
	}
	// Decode the first JSON value only: the log wrapper may follow the record.
	var rec GovernanceRecord
	if err := json.NewDecoder(strings.NewReader(payload)).Decode(&rec); err != nil {
		// A container runtime that JSON-quotes the whole log line leaves the
		// record with escaped quotes; undo one level and retry.
		if !strings.Contains(payload, `\"`) {
			return GovernanceRecord{}, fmt.Errorf("governance record: %w", err)
		}
		unescaped := strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(payload)
		if err2 := json.NewDecoder(strings.NewReader(unescaped)).Decode(&rec); err2 != nil {
			return GovernanceRecord{}, fmt.Errorf("governance record: %w", err)
		}
	}
	if rec.Job == "" && rec.Status == "" && rec.Action == "" {
		return GovernanceRecord{}, ErrNoGovernanceRecord
	}
	return rec, nil
}

// TelemetrySummary aggregates a batch of records.
type TelemetrySummary struct {
	Records   int            `json:"records"`
	Invalid   int            `json:"invalid"`
	Unparsed  int            `json:"unparsed"`
	ByStatus  map[string]int `json:"byStatus"`
	ByAction  map[string]int `json:"byAction"`
	ByMethod  map[string]int `json:"byRpcMethod"`
	ByTool    map[string]int `json:"byTool"`
	ByCaller  map[string]int `json:"byCaller"`
	Throttled int            `json:"throttled"`
	Forbidden int            `json:"forbidden"`
	Sessions  int            `json:"distinctSessions"`
	// Latency percentiles over records with latency_ms > 0.
	LatencyP50 int64 `json:"latencyP50Ms"`
	LatencyP95 int64 `json:"latencyP95Ms"`
	LatencyMax int64 `json:"latencyMaxMs"`
	// Violations lists distinct schema problems with their counts.
	Violations map[string]int `json:"violations,omitempty"`
}

// Summarize parses every line, validates each record and aggregates.
func Summarize(lines []string) (TelemetrySummary, []GovernanceRecord) {
	s := TelemetrySummary{
		ByStatus: map[string]int{}, ByAction: map[string]int{}, ByMethod: map[string]int{},
		ByTool: map[string]int{}, ByCaller: map[string]int{}, Violations: map[string]int{},
	}
	sessions := map[string]bool{}
	var latencies []int64
	var recs []GovernanceRecord
	for _, line := range lines {
		rec, err := ParseGovernanceLine(line)
		if err != nil {
			if !errors.Is(err, ErrNoGovernanceRecord) || strings.Contains(line, strings.TrimSpace(GovernanceMarker)) {
				s.Unparsed++
			}
			continue
		}
		recs = append(recs, rec)
		s.Records++
		if v := rec.Validate(); len(v) > 0 {
			s.Invalid++
			for _, p := range v {
				s.Violations[p]++
			}
		}
		s.ByStatus[rec.Status]++
		s.ByAction[rec.Action]++
		if rec.RPCMethod != "" {
			s.ByMethod[rec.RPCMethod]++
		}
		if rec.Tool != "" {
			s.ByTool[rec.Tool]++
		}
		if rec.Caller != "" {
			s.ByCaller[rec.Caller]++
		}
		if rec.Throttled() {
			s.Throttled++
		}
		if rec.Status == "403" {
			s.Forbidden++
		}
		if rec.Session != "" {
			sessions[rec.Session] = true
		}
		if rec.LatencyMS > 0 {
			latencies = append(latencies, rec.LatencyMS)
		}
	}
	s.Sessions = len(sessions)
	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		s.LatencyP50 = percentile(latencies, 50)
		s.LatencyP95 = percentile(latencies, 95)
		s.LatencyMax = latencies[len(latencies)-1]
	}
	if len(s.Violations) == 0 {
		s.Violations = nil
	}
	return s, recs
}

func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := (len(sorted)*p + 99) / 100
	if idx < 1 {
		idx = 1
	}
	if idx > len(sorted) {
		idx = len(sorted)
	}
	return sorted[idx-1]
}

// Write prints the summary as operator-facing text.
func (s TelemetrySummary) Write(w io.Writer) {
	fmt.Fprintf(w, "records: %d  (invalid %d, unparsed %d)\n", s.Records, s.Invalid, s.Unparsed)
	fmt.Fprintf(w, "throttled (429): %d   forbidden (403): %d   sessions: %d\n", s.Throttled, s.Forbidden, s.Sessions)
	if s.LatencyMax > 0 {
		fmt.Fprintf(w, "latency ms: p50=%d p95=%d max=%d\n", s.LatencyP50, s.LatencyP95, s.LatencyMax)
	}
	writeCounts(w, "by status", s.ByStatus)
	writeCounts(w, "by action", s.ByAction)
	writeCounts(w, "by rpc_method", s.ByMethod)
	writeCounts(w, "by tool", s.ByTool)
	writeCounts(w, "by caller", s.ByCaller)
	if len(s.Violations) > 0 {
		fmt.Fprintln(w, "schema violations:")
		writeCounts(w, "", s.Violations)
	}
}

func writeCounts(w io.Writer, title string, m map[string]int) {
	if len(m) == 0 {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if title != "" {
		fmt.Fprintf(w, "%s:\n", title)
	}
	for _, k := range keys {
		fmt.Fprintf(w, "  %-32s %d\n", k, m[k])
	}
}

// LokiQueryOptions selects the governance stream to read.
type LokiQueryOptions struct {
	// URL is the Loki base, e.g. http://localhost:3100 after a port-forward.
	URL string
	// Query is the LogQL selector; empty uses {job="llm-gateway"}.
	Query string
	// Since bounds the range ending now; zero means one hour.
	Since time.Duration
	// Limit caps the number of lines; zero means 1000.
	Limit int
	// HTTPClient nil uses a 30s default.
	HTTPClient *http.Client
}

// QueryLoki reads the raw log lines of the governance stream, newest first.
func QueryLoki(ctx context.Context, opts LokiQueryOptions) ([]string, error) {
	if opts.URL == "" {
		return nil, errors.New("loki: URL is required")
	}
	if opts.Query == "" {
		opts.Query = fmt.Sprintf(`{job=%q}`, LokiJob)
	}
	if opts.Since <= 0 {
		opts.Since = time.Hour
	}
	if opts.Limit <= 0 {
		opts.Limit = 1000
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	end := time.Now()
	q := url.Values{}
	q.Set("query", opts.Query)
	q.Set("start", strconv.FormatInt(end.Add(-opts.Since).UnixNano(), 10))
	q.Set("end", strconv.FormatInt(end.UnixNano(), 10))
	q.Set("limit", strconv.Itoa(opts.Limit))
	q.Set("direction", "backward")
	u := strings.TrimRight(opts.URL, "/") + "/loki/api/v1/query_range?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loki query: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("loki query: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var body struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Values [][]string `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("loki query: decode: %w", err)
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("loki query: status %q", body.Status)
	}
	var lines []string
	for _, stream := range body.Data.Result {
		for _, v := range stream.Values {
			if len(v) == 2 {
				lines = append(lines, v[1])
			}
		}
	}
	return lines, nil
}

// ReadLines splits a log file into lines, dropping blanks.
func ReadLines(r io.Reader) ([]string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out, nil
}
