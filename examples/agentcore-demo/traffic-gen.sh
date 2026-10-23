#!/bin/bash
# Continuous traffic generator for the BNK + AgentCore demo.
#
# Drives both governed legs on a loop so Forge's LLM Observability panel and
# the Gateway Topology view have something live to show during a walkthrough.
#
#   Leg A (model + tool):  agentcore invoke -> Bedrock -> BNK VIP -> MCP pod
#   Leg B (tool only):     external-agent.py -> BNK VIP -> MCP pod
#
# Usage:
#   AWS_PROFILE=<profile> ./traffic-gen.sh
#
# Leg B talks to the private BNK VIP directly, so it only works from a host
# inside the VPC (the jumphost has a second ENI on the VIP's subnet). Run this
# script there for both legs, or accept that leg B is skipped when off-VPC —
# it is detected and reported, not silently swallowed.

set -uo pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUNTIME="${RUNTIME:-FinanceAgentV2Agent}"
TARGET="${TARGET:-demo-v2}"
VIP="${VIP:-10.0.10.150}"
INTERVAL="${INTERVAL:-300}"
SYMBOLS=(AAPL MSFT NVDA AMZN GOOG META TSLA)

if [ -z "${AWS_PROFILE:-}" ]; then
    echo "AWS_PROFILE is not set — leg A (agentcore invoke) will fail." >&2
fi

# Leg A — the full agent path: Bedrock reasons, then calls the tool via BNK.
generate_agent_traffic() {
    local symbol="$1"
    echo "[leg A] agentcore invoke -> Bedrock -> BNK -> MCP  (forecast $symbol)"
    ( cd "$DEMO_DIR/agent" && \
      ./node_modules/.bin/agentcore invoke \
        --runtime "$RUNTIME" --target "$TARGET" \
        --prompt "forecast $symbol" >/dev/null 2>&1 ) \
      && echo "[leg A] ok" || echo "[leg A] FAILED"
}

# Leg B — an unmanaged external caller hitting the tool through BNK.
generate_external_traffic() {
    local symbol="$1"
    if ! nc -z -w 3 "$VIP" 80 2>/dev/null; then
        echo "[leg B] SKIPPED — $VIP:80 unreachable (run this from inside the VPC)"
        return
    fi
    echo "[leg B] external-agent.py -> BNK -> MCP  (forecast $symbol)"
    python3 "$DEMO_DIR/external-agent.py" --prompt "forecast $symbol" \
      && echo "[leg B] ok" || echo "[leg B] FAILED"
}

echo "traffic-gen: runtime=$RUNTIME target=$TARGET vip=$VIP interval=${INTERVAL}s"
while true; do
    symbol="${SYMBOLS[$RANDOM % ${#SYMBOLS[@]}]}"
    echo "--- $(date -u +%Y-%m-%dT%H:%M:%SZ) ---"
    generate_agent_traffic "$symbol"
    generate_external_traffic "$symbol"
    sleep "$INTERVAL"
done
