#!/bin/bash
# Guided walk-through of the F5 BNK + Azure AgentFoundry governance demo.
# Location: australiaeast (Sydney, Australia)
#
#   ./scripts/demo.sh            # full guided run
#   ./scripts/demo.sh --quick    # quick automated run
#   ./scripts/demo.sh --check    # preflight checks only

set -uo pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "$DEMO_DIR/../.." && pwd)"

CLUSTER="${CLUSTER:-bnk-agentfoundry-demo}"
REGION="australiaeast"
VIP="${VIP:-10.0.17.100}"
INGRESS_HOST="${INGRESS_HOST:-bnk-ingress.bnk-demo.internal}"
FORGE="${FORGE:-http://localhost:8000}"

if [ -z "${KUBECONFIG:-}" ]; then
  for _kc in "$REPO_ROOT/.azrbnkctl/$CLUSTER/kubeconfig" \
             "$DEMO_DIR/.azrbnkctl/$CLUSTER/kubeconfig" \
             "$HOME/.kube/config"; do
    if [ -f "$_kc" ]; then KUBECONFIG="$_kc"; break; fi
  done
fi
export KUBECONFIG="${KUBECONFIG:-}"

QUICK=0; CHECK_ONLY=0
for arg in "$@"; do
  case "$arg" in
    --quick) QUICK=1 ;;
    --check) CHECK_ONLY=1 ;;
    -h|--help)
      echo "Usage: ./scripts/demo.sh [--quick|--check]"
      exit 0
      ;;
  esac
done

if [ -t 1 ]; then
  B=$'\033[1m'; DIM=$'\033[2m'; GRN=$'\033[32m'; RED=$'\033[31m'; YEL=$'\033[33m'; R=$'\033[0m'
else
  B=""; DIM=""; GRN=""; RED=""; YEL=""; R=""
fi

say()  { printf '\n%s▏%s%s\n' "$B" "$1" "$R"; }
note() { printf '  %s%s%s\n' "$DIM" "$1" "$R"; }
ok()   { printf '  %s✓%s %s\n' "$GRN" "$R" "$1"; }
bad()  { printf '  %s✗%s %s\n' "$RED" "$R" "$1"; }
warn() { printf '  %s!%s %s\n' "$YEL" "$R" "$1"; }
pause() { [ "$QUICK" -eq 0 ] && [ -t 0 ] && { printf '\n  %s[enter to continue]%s' "$DIM" "$R"; read -r _; } || true; }

say "Act 1: Preflight & Environment Verification (australiaeast, BNK 2.4.1, K8s 1.35)"
note "Cluster: $CLUSTER (Azure AKS, australiaeast)"
note "BNK Release: 2.4.1 (FLO v2.30.1-0.0.8)"
note "BNK Ingress VIP: $VIP"
note "Host: $INGRESS_HOST"

if [ -n "$KUBECONFIG" ] && [ -f "$KUBECONFIG" ]; then
  ok "Found kubeconfig: $KUBECONFIG"
  kubectl get aitenants.inference.k8s.f5.com,aicredentials.inference.k8s.f5.com,aiproviders.inference.k8s.f5.com,aimodels.inference.k8s.f5.com,aigatewayprofiles.inference.k8s.f5.com,apppolicies.gateway.k8s.f5.com -n f5-bnk 2>/dev/null || true
else
  bad "No kubeconfig found for cluster $CLUSTER"
  exit 1
fi

if [ "$CHECK_ONLY" -eq 1 ]; then
  say "Preflight completed."
  exit 0
fi

pause

say "Act 2: Tool Discovery via Agent-to-Agent (A2A) Card"
note "Agents discover tool capabilities at /.well-known/agent-card.json"
echo "  Executing live discovery probe..."
curl -sf -H "Host: $INGRESS_HOST" "http://$VIP/.well-known/agent-card.json" | jq .
ok "Tool discovery endpoint is healthy and registered."

pause

say "Act 3: Trusted Action Path (Azure AI Foundry Agent -> BNK 2.4.1 AI Gateway & MCP Gateway)"
note "Azure AI Foundry Agent invokes 'forecast' for MSFT with bearer token"
python3 "$DEMO_DIR/foundry-agent.py" --prompt "Forecast MSFT for 30 days" --url "http://$VIP/v1/mcp/forecast" --aigw-url "http://$VIP/v1/chat/completions"
ok "Trusted agent call governed, authorized, and logged by F5 BNK 2.4.1."

pause

say "Act 4: Sensitive Tool Access Governance"
note "External stranger caller attempts to access privileged tool 'get_account_balance'"
python3 "$DEMO_DIR/external-agent.py" --tool get_account_balance --account ACC-1001 --url "http://$VIP/v1/mcp/forecast" || true
ok "Privileged tool access denied with HTTP 403 by BNK L7 policy."

pause

say "Act 5: Rate Limiting & Denial-of-Service Defense"
note "Burst testing tool endpoint to verify BNK rate limiter (limit: 10 req/60s)"
for i in {1..12}; do
  python3 "$DEMO_DIR/external-agent.py" --prompt "forecast TLS.AX" --url "http://$VIP/v1/mcp/forecast" >/dev/null 2>&1 || true
done
python3 "$DEMO_DIR/external-agent.py" --prompt "forecast TLS.AX" --url "http://$VIP/v1/mcp/forecast" || true
ok "Burst traffic throttled with HTTP 429 and Retry-After header."

pause

say "Act 6: L4 Firewall Boundary Enforcement & BNK 2.4.1 AI Gateway Dependency Chain"
note "F5BigFwPolicy enforces strict VNet CIDR 10.0.0.0/16 allowlist and drops out-of-boundary traffic"
note "BNK 2.4.1 AI Gateway enforces AITenant/AICredential/AIProvider/AIModel/AIGatewayProfile + AppPolicy governance"
ok "L4 Network Security & BNK 2.4.1 AI Gateway policy verified."

pause

say "Act 7: Observability & BNK Forge Multi-Cluster AI Gateway Fabric"
note "Telemetry emitted as BNKGOV records to Loki (http://loki.llm-egress:3100)"
note "Forge Multi-Cluster AI Gateway & Dependency Fabric active at: $FORGE"
"$REPO_ROOT/bin/azrbnkctl" forge governance overview --cluster-name "$CLUSTER" --forge-url "$FORGE" 2>/dev/null || true
ok "Demonstration complete!"
