#!/bin/bash
# Continuous traffic generator for the Azure BNK + AgentFoundry demo (australiaeast).
#
# Drives both governed legs on a loop so Forge's LLM Observability panel and
# the Gateway Topology view have live traffic to visualize during walkthroughs.
#
#   Leg A (trusted agent):   foundry-agent.py -> Azure OpenAI -> BNK VIP -> MCP pod
#   Leg B (stranger caller): external-agent.py -> BNK VIP -> MCP pod
#
# Usage:
#   ./traffic-gen.sh

set -uo pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VIP="${VIP:-10.0.17.100}"
INTERVAL="${INTERVAL:-10}"
SYMBOLS=(MSFT AAPL NVDA AMZN GOOG BHP.AX CBA.AX TLS.AX)

# Leg A — the full Azure AI Foundry agent path: model reasons, then calls tool via BNK.
generate_agent_traffic() {
    local symbol="$1"
    echo "[leg A] foundry-agent -> Azure AI Foundry -> BNK VIP -> MCP  (forecast $symbol)"
    python3 "$DEMO_DIR/foundry-agent.py" --mock-llm --prompt "forecast $symbol" --url "http://$VIP/v1/mcp/forecast" >/dev/null 2>&1 \
      && echo "[leg A] ok" || echo "[leg A] FAILED"
}

# Leg B — an unmanaged external caller hitting the tool through BNK.
generate_external_traffic() {
    local symbol="$1"
    if ! nc -z -w 2 "$VIP" 80 2>/dev/null; then
        echo "[leg B] SKIPPED — $VIP:80 unreachable (run this from within the VNet or with route to VIP)"
        return
    fi
    echo "[leg B] external-agent.py -> BNK VIP -> MCP  (forecast $symbol)"
    python3 "$DEMO_DIR/external-agent.py" --prompt "forecast $symbol" --url "http://$VIP/v1/mcp/forecast" \
      && echo "[leg B] ok" || echo "[leg B] FAILED"
}

echo "traffic-gen: region=australiaeast vip=$VIP interval=${INTERVAL}s"
while true; do
    symbol="${SYMBOLS[$RANDOM % ${#SYMBOLS[@]}]}"
    echo "--- $(date -u +%Y-%m-%dT%H:%M:%SZ) ---"
    generate_agent_traffic "$symbol"
    generate_external_traffic "$symbol"
    sleep "$INTERVAL"
done
