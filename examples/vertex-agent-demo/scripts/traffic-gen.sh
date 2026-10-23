#!/usr/bin/env bash
#
# traffic-gen.sh — High-rate traffic generator testing F5 BNK rate limits.
#
set -euo pipefail

TARGET_URL="${MCP_TOOL_URL:-http://10.10.1.50/v1/mcp/forecast}"
TOKEN="${MCP_AGENT_TOKEN:-vertex-agent-demo-secret-key-1}"
REQUESTS="${1:-50}"

echo "Emitting $REQUESTS requests to F5 BNK Ingress ($TARGET_URL) to trigger rate-limiting (20 req/s threshold)..."

PASSED=0
LIMITED=0
FAILED=0

for i in $(seq 1 "$REQUESTS"); do
  STATUS=$(curl -s -o /dev/null -w "%{http_code}" \
    -X POST "$TARGET_URL" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Host: bnk-ingress.bnk-demo.internal" \
    -H "Content-Type: application/json" \
    -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"forecast","arguments":{"symbol":"GOOGL"}}}' || echo "000")

  if [ "$STATUS" -eq 200 ]; then
    PASSED=$((PASSED + 1))
    echo -n "✓"
  elif [ "$STATUS" -eq 429 ]; then
    LIMITED=$((LIMITED + 1))
    echo -n "L"
  else
    FAILED=$((FAILED + 1))
    echo -n "x"
  fi
done

echo ""
echo "Traffic test summary:"
echo "  Successful (200 OK):      $PASSED"
echo "  Rate Limited (429):       $LIMITED"
echo "  Failed/Unreachable (Err): $FAILED"
