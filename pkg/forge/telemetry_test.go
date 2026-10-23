package forge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A record as the f5-fluentbit sidecar prints it: fluentd wrapper, iRule
// prefix, then the BNKGOV payload.
const wrappedLine = `2026-09-14T03:12:44.101Z stdout F {"log":"Sep 14 03:12:44 f5-tmm-abc tmm[1]: Rule /Common/mcp-rate-limit-irule <HTTP_RESPONSE_DATA>: BNKGOV {\"job\":\"llm-gateway\",\"model\":\"mcp:finance-tool\",\"status\":\"200\",\"latency_ms\":41,\"prompt_tk\":0,\"comp_tk\":0,\"total_tk\":0,\"cached\":0,\"cost\":0,\"userq\":\"tools/call forecast\",\"client\":\"10.0.11.15\",\"caller\":\"agent\",\"action\":\"allow\",\"req_body\":\"{...}\",\"resp_body\":\"{...}\",\"rpc_method\":\"tools/call\",\"tool\":\"forecast\",\"session\":\"9f1c\",\"transport\":\"http\"}"}`

const rateLimited = `BNKGOV {"job":"llm-gateway","model":"mcp:finance-tool","status":"429","latency_ms":0,"prompt_tk":0,"comp_tk":0,"total_tk":0,"cached":0,"cost":0,"userq":"/v1/mcp/forecast","client":"10.0.12.191","caller":"external","action":"rate_limited","req_body":"","resp_body":"rate limit exceeded","rpc_method":"","tool":"","session":"","transport":"sse"}`

func TestParseGovernanceLine(t *testing.T) {
	rec, err := ParseGovernanceLine(wrappedLine)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != "200" || rec.LatencyMS != 41 || rec.Caller != "agent" || rec.RPCMethod != "tools/call" || rec.Tool != "forecast" || rec.Session != "9f1c" {
		t.Errorf("rec = %+v", rec)
	}
	if v := rec.Validate(); len(v) != 0 {
		t.Errorf("valid record flagged: %v", v)
	}
	if rec.Throttled() {
		t.Error("200 is not throttled")
	}

	rl, err := ParseGovernanceLine(rateLimited)
	if err != nil {
		t.Fatal(err)
	}
	if !rl.Throttled() || rl.Action != ActionRateLimited || rl.Transport != "sse" {
		t.Errorf("rl = %+v", rl)
	}

	if _, err := ParseGovernanceLine("Sep 14 tmm[1]: something else"); err != ErrNoGovernanceRecord {
		t.Errorf("non-record: err = %v", err)
	}
	if _, err := ParseGovernanceLine(`BNKGOV {"job":"llm-gateway","status":"200"`); err == nil {
		t.Error("truncated JSON must fail")
	}
}

func TestGovernanceRecord_Validate(t *testing.T) {
	base := GovernanceRecord{Job: LokiJob, Model: "mcp:x", Status: "200", Client: "10.0.0.1", Action: ActionAllow}
	if v := base.Validate(); len(v) != 0 {
		t.Fatalf("base invalid: %v", v)
	}
	cases := map[string]GovernanceRecord{
		`job="other"`:                         {Job: "other", Model: "m", Status: "200", Client: "c", Action: "allow"},
		"model is empty":                      {Job: LokiJob, Status: "200", Client: "c", Action: "allow"},
		`status="oops" is not an HTTP status`: {Job: LokiJob, Model: "m", Status: "oops", Client: "c", Action: "allow"},
		"client is empty":                     {Job: LokiJob, Model: "m", Status: "200", Action: "allow"},
		`action="blocked" is not one of`:      {Job: LokiJob, Model: "m", Status: "200", Client: "c", Action: "blocked"},
		"action=rate_limited with status 200": {Job: LokiJob, Model: "m", Status: "200", Client: "c", Action: ActionRateLimited},
		"tools/call without tool":             {Job: LokiJob, Model: "m", Status: "200", Client: "c", Action: "allow", RPCMethod: "tools/call"},
		`transport="grpc"`:                    {Job: LokiJob, Model: "m", Status: "200", Client: "c", Action: "allow", Transport: "grpc"},
	}
	for want, rec := range cases {
		if v := strings.Join(rec.Validate(), "\n"); !strings.Contains(v, want) {
			t.Errorf("want violation containing %q, got %q", want, v)
		}
	}
}

func TestSummarize(t *testing.T) {
	lines := []string{
		wrappedLine,
		rateLimited,
		`BNKGOV {"job":"llm-gateway","model":"mcp:finance-tool","status":"403","latency_ms":0,"client":"10.0.12.191","caller":"external","action":"tool_forbidden","rpc_method":"tools/call","tool":"get_account_balance","session":"9f1c"}`,
		`BNKGOV {"job":"llm-gateway","model":"mcp:finance-tool","status":"200","latency_ms":120,"client":"10.0.11.15","caller":"agent","action":"allow","rpc_method":"tools/list","session":"77aa"}`,
		`BNKGOV {"job":"llm-gateway","model":"mcp:finance-tool","status":"200","latency_ms":9,"client":"","caller":"agent","action":"allow"}`,
		"BNKGOV {garbage",
		"unrelated log line",
	}
	s, recs := Summarize(lines)
	if s.Records != 5 || len(recs) != 5 {
		t.Fatalf("records = %d (%d parsed)", s.Records, len(recs))
	}
	if s.Unparsed != 1 {
		t.Errorf("unparsed = %d, want 1 (the garbage BNKGOV line; unrelated lines are ignored)", s.Unparsed)
	}
	if s.Invalid != 1 || s.Violations["client is empty (the collector drops records without it)"] != 1 {
		t.Errorf("invalid = %d violations = %v", s.Invalid, s.Violations)
	}
	if s.Throttled != 1 || s.Forbidden != 1 || s.Sessions != 2 {
		t.Errorf("throttled=%d forbidden=%d sessions=%d", s.Throttled, s.Forbidden, s.Sessions)
	}
	if s.ByStatus["200"] != 3 || s.ByAction["allow"] != 3 || s.ByMethod["tools/call"] != 2 || s.ByTool["forecast"] != 1 || s.ByCaller["external"] != 2 {
		t.Errorf("counts: status=%v action=%v method=%v tool=%v caller=%v", s.ByStatus, s.ByAction, s.ByMethod, s.ByTool, s.ByCaller)
	}
	// latencies 9, 41, 120 → p50=41, p95=120, max=120
	if s.LatencyP50 != 41 || s.LatencyP95 != 120 || s.LatencyMax != 120 {
		t.Errorf("latency p50=%d p95=%d max=%d", s.LatencyP50, s.LatencyP95, s.LatencyMax)
	}
	var out strings.Builder
	s.Write(&out)
	for _, want := range []string{"records: 5", "throttled (429): 1", "by rpc_method:", "tools/call", "schema violations:"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary missing %q:\n%s", want, out.String())
		}
	}
}

func TestQueryLoki(t *testing.T) {
	var gotQuery, gotLimit, gotDir string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" {
			http.NotFound(w, r)
			return
		}
		gotQuery, gotLimit, gotDir = r.URL.Query().Get("query"), r.URL.Query().Get("limit"), r.URL.Query().Get("direction")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "streams",
				"result": []map[string]any{
					{"stream": map[string]string{"job": "llm-gateway"}, "values": [][]string{{"1", rateLimited}, {"2", wrappedLine}}},
				},
			},
		})
	}))
	defer ts.Close()

	lines, err := QueryLoki(context.Background(), LokiQueryOptions{URL: ts.URL + "/", Since: 10 * time.Minute, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || gotQuery != `{job="llm-gateway"}` || gotLimit != "50" || gotDir != "backward" {
		t.Errorf("lines=%d query=%q limit=%q dir=%q", len(lines), gotQuery, gotLimit, gotDir)
	}
	s, _ := Summarize(lines)
	if s.Records != 2 || s.Throttled != 1 {
		t.Errorf("summary = %+v", s)
	}

	if _, err := QueryLoki(context.Background(), LokiQueryOptions{}); err == nil {
		t.Error("missing URL must fail")
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 500) }))
	defer bad.Close()
	if _, err := QueryLoki(context.Background(), LokiQueryOptions{URL: bad.URL}); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("err = %v", err)
	}
}

func TestReadLines(t *testing.T) {
	lines, err := ReadLines(strings.NewReader("a\n\n  \nb\n"))
	if err != nil || len(lines) != 2 {
		t.Errorf("lines=%v err=%v", lines, err)
	}
}
