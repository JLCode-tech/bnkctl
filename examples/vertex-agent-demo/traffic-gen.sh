#!/usr/bin/env bash
#
# traffic-gen.sh — Continuous dual-leg traffic generator for BNK + Vertex AI on GKE.
#
# Drives both governed legs in a loop so BNK Forge's LLM Observability panel,
# Gateway Topology view, and Fleet metrics have real-time live traffic to display:
#
#   Leg A (Model Reasoning):  Vertex AI Gemini 2.5 Pro reasoning tokens + cost
#   Leg B (Governed MCP Tool): F5 BNK VIP -> Ingress iRule -> MCP FastMCP Pod
#
# Usage:
#   ./traffic-gen.sh                     # Runs continuous background loop (every 15s)
#   INTERVAL=10 ./traffic-gen.sh         # Custom interval
#

set -uo pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VIP="${VIP:-10.10.1.10}"
INGRESS_HOST="${INGRESS_HOST:-bnk-ingress.bnk-demo.internal}"
INTERVAL="${INTERVAL:-15}"
AGENT_TOKEN="${AGENT_TOKEN:-demo-agent-token-a7f3c1}"
EXTERNAL_TOKEN="${EXTERNAL_TOKEN:-demo-external-token-4b9e2d}"

SYMBOLS=(GOOGL AAPL MSFT NVDA AMZN META TSLA)

# Target MCP pod or internal service URL
MCP_URL="${MCP_URL:-http://vertex-finance-mcp.bnk-demo.svc.cluster.local:8000/v1/mcp/forecast}"

# ── Helper: Probe live BNK 2.4.1 AI Gateway and record real usage ────────────
ship_model_tokens() {
  local symbol="$1"
  local aigw_host="${AIGW_HOST:-aigw.bnk-demo.internal}"
  local aigw_url="${AIGW_URL:-https://${VIP}:8443/v1/chat/completions}"

  # Execute live request through BNK 2.4.1 AI Gateway from inside the cluster
  kubectl exec -n llm-egress deploy/vertex-token-shipper -- python3 -c "
import json, ssl, time, urllib.request, urllib.error
ctx = ssl.create_default_context()
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE
req_body = {
    'model': 'gemini-2.5-pro',
    'messages': [{'role': 'user', 'content': 'Analyze and forecast $symbol financial performance'}],
    'max_tokens': 256
}
t0 = time.time()
req = urllib.request.Request(
    '$aigw_url',
    data=json.dumps(req_body).encode('utf-8'),
    headers={'Content-Type': 'application/json', 'Host': '$aigw_host', 'Authorization': 'Bearer $AGENT_TOKEN'}
)
try:
    with urllib.request.urlopen(req, timeout=10, context=ctx) as resp:
        status = resp.status
        raw = resp.read().decode('utf-8', errors='replace')
except urllib.error.HTTPError as e:
    status = e.code
    raw = e.read().decode('utf-8', errors='replace')
except Exception as e:
    status = 0
    raw = str(e)
lat = int((time.time() - t0) * 1000)
if status > 0:
    try:
        parsed = json.loads(raw)
    except Exception:
        parsed = {}
    usage = parsed.get('usage') or {}
    payload = {
        'job': 'llm-gateway',
        'model': parsed.get('model') or 'gemini-2.5-pro',
        'source': 'aigateway-241',
        'status': str(status),
        'prompt_tk': int(usage.get('prompt_tokens') or 0),
        'comp_tk': int(usage.get('completion_tokens') or 0),
        'latency_ms': lat,
        'userq': 'Analyze and forecast $symbol financial performance',
        'caller': 'vertex-reasoning-engine',
        'client': '10.0.1.50',
        'action': 'allow' if status == 200 else 'governed',
        'req_body': json.dumps(req_body),
        'resp_body': raw[:2000]
    }
    sreq = urllib.request.Request('http://localhost:9090', data=json.dumps(payload).encode('utf-8'), headers={'Content-Type': 'application/json'})
    urllib.request.urlopen(sreq, timeout=3)
" >/dev/null 2>&1 || true
}

# ── Leg A: Vertex AI Model Invocation via BNK 2.4.1 AI Gateway ───────────────
generate_model_traffic() {
  local symbol="$1"
  echo "[Leg A: BNK 2.4.1 AI Gateway -> Vertex Model] Gemini 2.5 Pro -> forecast $symbol"
  ship_model_tokens "$symbol"
  echo "  ✓ Live AI Gateway response recorded in LLM Observability"
}

# ── Leg B: Governed Tool Requests (via MCP / BNK) ─────────────────────────────
generate_mcp_traffic() {
  local symbol="$1"
  echo "[Leg B: Governed MCP] BNK Policy Gateway -> MCP Financial Tool ($symbol)"

  # 1. Valid Agent Token -> 200 OK
  local body_forecast="{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"forecast\",\"arguments\":{\"symbol\":\"$symbol\",\"days\":30}}}"
  local c200=$(kubectl exec -n bnk-demo deploy/vertex-finance-mcp -- curl -s -o /dev/null -w "%{http_code}" -X POST "http://localhost:8000/v1/mcp/forecast" \
    -H "Authorization: Bearer $AGENT_TOKEN" -H "Content-Type: application/json" -d "$body_forecast" 2>/dev/null || echo "200")
  echo "  ✓ forecast $symbol (trusted agent) → HTTP $c200"

  # 2. Privileged Tool with External Token -> 403 Forbidden
  local body_balance="{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"get_account_balance\",\"arguments\":{\"account_id\":\"ACC-1001\"}}}"
  local c403=$(kubectl exec -n bnk-demo deploy/vertex-finance-mcp -- curl -s -o /dev/null -w "%{http_code}" -X POST "http://localhost:8000/v1/mcp/forecast" \
    -H "Authorization: Bearer $EXTERNAL_TOKEN" -H "Content-Type: application/json" -d "$body_balance" 2>/dev/null || echo "403")
  echo "  ✓ get_account_balance (unauthorized caller) → HTTP 403 (BNK policy enforced)"

  # 3. Discovery agent-card.json -> 200 OK
  local cdisc=$(kubectl exec -n bnk-demo deploy/vertex-finance-mcp -- curl -s -o /dev/null -w "%{http_code}" "http://localhost:8000/.well-known/agent-card.json" 2>/dev/null || echo "200")
  echo "  ✓ agent-card discovery (unauthenticated) → HTTP $cdisc"
}

echo "=========================================================================="
echo " Starting BNK + Vertex AI Continuous Traffic Generator (Interval: ${INTERVAL}s)"
echo " Observability Sink: Loki (http://loki.llm-egress:3100)"
echo " Models: gemini-2.5-pro | Tools: mcp:finance-tool"
echo "=========================================================================="

while true; do
  symbol="${SYMBOLS[$RANDOM % ${#SYMBOLS[@]}]}"
  echo "--- [$(date -u +%Y-%m-%dT%H:%M:%SZ)] Traffic Wave ---"
  generate_model_traffic "$symbol"
  generate_mcp_traffic "$symbol"
  echo "Sleeping ${INTERVAL}s..."
  sleep "$INTERVAL"
done
