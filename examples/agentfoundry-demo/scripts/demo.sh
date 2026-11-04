#!/bin/bash
# Guided walk-through of the F5 BNK 2.4.1 + Azure AgentFoundry governance demo.
# Location: australiaeast (Sydney, Australia)
#
#   ./scripts/demo.sh            # full guided run
#   ./scripts/demo.sh --quick    # quick automated run (all 6/6 live checks)
#   ./scripts/demo.sh --check    # preflight checks only

set -uo pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "$DEMO_DIR/../.." && pwd)"

CLUSTER="${CLUSTER:-bnk-agentfoundry-demo}"
GW="${GW:-bnk-agentfoundry-demo-gateway}"
AIGW_GW="${AIGW_GW:-llm-chat-route-gw}"
BNK_NS="${BNK_NS:-f5-cne-system}"
REGION="${AZURE_LOCATION:-australiaeast}"
VIP="${VIP:-10.0.17.100}"
AIGW_VIP="${AIGW_VIP:-10.0.17.101}"
INGRESS_HOST="${INGRESS_HOST:-bnk-ingress.bnk-demo.internal}"
AIGW_HOST="${AIGW_HOST:-aigw.bnk-demo.internal}"
FORGE="${FORGE:-http://localhost:8000}"

AGENT_TOKEN="${AGENT_TOKEN:-demo-agent-token-a7f3c1}"
EXTERNAL_TOKEN="${EXTERNAL_TOKEN:-demo-external-token-4b9e2d}"

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

FAILED=0
PASSED=0
say()  { printf '\n%s▏%s%s\n' "$B" "$1" "$R"; }
note() { printf '  %s%s%s\n' "$DIM" "$1" "$R"; }
ok()   { printf '  %s✓%s %s\n' "$GRN" "$R" "$1"; }
bad()  { printf '  %s✗%s %s\n' "$RED" "$R" "$1"; FAILED=$((FAILED+1)); }
warn() { printf '  %s!%s %s\n' "$YEL" "$R" "$1"; }
pause() { [ "$QUICK" -eq 0 ] && [ -t 0 ] && { printf '\n  %s[enter to continue]%s' "$DIM" "$R"; read -r _; } || true; }

say "Act 1: Preflight & Environment Verification (australiaeast, BNK 2.4.1, K8s 1.34)"
note "Cluster: $CLUSTER (Azure AKS, $REGION)"
note "BNK Release: 2.4.1 (FLO v2.30.1-0.0.8)"
note "BNK Ingress VIP: $VIP ($INGRESS_HOST)"
note "BNK AI Gateway VIP: $AIGW_VIP:8080 ($AIGW_HOST)"

if [ -z "$KUBECONFIG" ] || [ ! -f "$KUBECONFIG" ]; then
  bad "No kubeconfig found for cluster $CLUSTER"
  exit 1
fi
ok "Found kubeconfig: $KUBECONFIG"

if ! kubectl get --raw=/readyz >/dev/null 2>&1; then
  bad "Kubeconfig exists but cluster API server does not answer /readyz"
  exit 1
fi
ok "Kubernetes API server /readyz healthy"

GW_PROG=$(kubectl get gateway "$GW" -n default -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null || true)
[ "$GW_PROG" = "True" ] && ok "BNK MCP Gateway ($GW) Programmed=True on $VIP" || bad "BNK MCP Gateway ($GW) Programmed=$GW_PROG"

IRULE_PROG=$(kubectl get f5-big-cne-irules mcp-rate-limit-irule -n default -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null || true)
[ "$IRULE_PROG" = "True" ] && ok "Governance iRule (mcp-rate-limit-irule) Programmed=True on TMM" || bad "Governance iRule Programmed=$IRULE_PROG"

POLICIES=$(kubectl get netpolicies.gateway.k8s.f5.com -n default --no-headers 2>/dev/null | wc -l | tr -d ' ')
[ "${POLICIES:-0}" -ge 2 ] && ok "$POLICIES listener-scoped NetPolicies attached in default" || bad "Expected >=2 NetPolicies in default, found ${POLICIES:-0}"

AIGW_PROF_PROG=$(kubectl get aigatewayprofiles.inference.k8s.f5.com llm-chat-route -n "$BNK_NS" -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null || true)
[ "$AIGW_PROF_PROG" = "True" ] && ok "BNK 2.4.1 AIGatewayProfile (llm-chat-route) Programmed=True in $BNK_NS" || bad "AIGatewayProfile Programmed=$AIGW_PROF_PROG"

AIGW_GW_PROG=$(kubectl get gateway "$AIGW_GW" -n "$BNK_NS" -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null || true)
[ "$AIGW_GW_PROG" = "True" ] && ok "BNK 2.4.1 AI Gateway ($AIGW_GW) Programmed=True on $AIGW_VIP:8080" || bad "AI Gateway ($AIGW_GW) Programmed=$AIGW_GW_PROG"

TOOL_READY=$(kubectl get deploy mcp-financial-tool -n default -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)
[ "${TOOL_READY:-0}" -ge 1 ] && ok "MCP tool pod ready (replicas=$TOOL_READY)" || bad "MCP tool pod not ready"

OBS_RUNNING=$(kubectl get pods -n llm-egress --no-headers 2>/dev/null | grep -c Running || echo 0)
[ "${OBS_RUNNING:-0}" -ge 3 ] && ok "Observability stack up ($OBS_RUNNING Running pods in llm-egress)" || warn "Observability stack has $OBS_RUNNING Running pods in llm-egress"

TGEN_READY=$(kubectl get deploy agentfoundry-traffic-generator -n default -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)
[ "${TGEN_READY:-0}" -ge 1 ] && ok "Traffic generator (deploy/agentfoundry-traffic-generator) Running" || bad "Traffic generator not ready"

if [ "$FAILED" -gt 0 ]; then
  echo
  echo "  ${RED}Preflight failed ($FAILED error(s))${R}"
  exit 1
fi

if [ "$CHECK_ONLY" -eq 1 ]; then
  say "Preflight completed (all checks passed)."
  exit 0
fi

pause

# Helper to execute HTTP probes from inside the VNet/TMM node via deploy/agentfoundry-traffic-generator
vnet_python() {
  kubectl exec -i -n default deploy/agentfoundry-traffic-generator -- python3 -c "$1" 2>/dev/null
}

expect_check() {
  local num="$1" want="$2" label="$3" out="$4"
  local got snippet
  got=$(printf '%s' "$out" | head -1 | tr -d '[:space:]')
  snippet=$(printf '%s' "$out" | tail -n +2 | head -c 160)
  if [ "$got" = "$want" ]; then
    ok "Check $num/6 PASS: $label → HTTP $got"
    PASSED=$((PASSED+1))
  else
    bad "Check $num/6 FAIL: $label → got HTTP '$got', wanted '$want'"
  fi
  [ -n "$snippet" ] && note "$snippet"
}

FORECAST_BODY='{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"forecast","arguments":{"symbol":"MSFT","days":30}}}'
BALANCE_BODY='{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_account_balance","arguments":{"account_id":"ACC-1001"}}}'

say "Act 2: Tool Discovery via Agent-to-Agent (A2A) Card [Check 1/6]"
note "Unauthenticated GET http://$VIP/.well-known/agent-card.json (Host: $INGRESS_HOST)"
DISC_OUT=$(vnet_python "
import urllib.request, urllib.error
req = urllib.request.Request('http://$VIP/.well-known/agent-card.json', headers={'Host': '$INGRESS_HOST'})
try:
    with urllib.request.urlopen(req, timeout=10) as r:
        print(r.status)
        print(r.read().decode('utf-8', errors='replace')[:160])
except urllib.error.HTTPError as e:
    print(e.code)
except Exception as e:
    print(0); print(e)
")
expect_check 1 200 "Discovery (/.well-known/agent-card.json)" "$DISC_OUT"

pause

say "Act 3: Trusted Action Path — Authorized MCP Tool Call [Check 2/6]"
note "Azure AI Foundry Agent invokes 'forecast' for MSFT with trusted agent bearer token"
TRUSTED_OUT=$(vnet_python "
import urllib.request, urllib.error
body = '''$FORECAST_BODY'''.encode('utf-8')
req = urllib.request.Request('http://$VIP/v1/mcp/forecast', data=body, headers={
    'Host': '$INGRESS_HOST',
    'Content-Type': 'application/json',
    'Accept': 'application/json',
    'Authorization': 'Bearer $AGENT_TOKEN',
    'Mcp-Session-Id': 'demo-trusted-session'
}, method='POST')
try:
    with urllib.request.urlopen(req, timeout=10) as r:
        print(r.status)
        print(r.read().decode('utf-8', errors='replace')[:160])
except urllib.error.HTTPError as e:
    print(e.code); print(e.read().decode('utf-8', errors='replace')[:160])
except Exception as e:
    print(0); print(e)
")
expect_check 2 200 "Trusted forecast tool call (Bearer $AGENT_TOKEN)" "$TRUSTED_OUT"

pause

say "Act 4: Sensitive Tool Access Governance — Blocked Privileged Tool [Check 3/6]"
note "External caller attempts to invoke privileged tool 'get_account_balance' → blocked by BNK iRule"
BLOCKED_OUT=$(vnet_python "
import urllib.request, urllib.error
body = '''$BALANCE_BODY'''.encode('utf-8')
req = urllib.request.Request('http://$VIP/v1/mcp/forecast', data=body, headers={
    'Host': '$INGRESS_HOST',
    'Content-Type': 'application/json',
    'Accept': 'application/json',
    'Authorization': 'Bearer $EXTERNAL_TOKEN',
    'Mcp-Session-Id': 'demo-external-session'
}, method='POST')
try:
    with urllib.request.urlopen(req, timeout=10) as r:
        print(r.status)
        print(r.read().decode('utf-8', errors='replace')[:160])
except urllib.error.HTTPError as e:
    print(e.code); print(e.read().decode('utf-8', errors='replace')[:160])
except Exception as e:
    print(0); print(e)
")
expect_check 3 403 "Blocked get_account_balance call (Bearer $EXTERNAL_TOKEN)" "$BLOCKED_OUT"

pause

say "Act 5: Unauthenticated MCP Call Rejection [Check 4/6]"
note "Caller with no Authorization header attempts to invoke MCP tool"
UNAUTH_OUT=$(vnet_python "
import urllib.request, urllib.error
body = '''$FORECAST_BODY'''.encode('utf-8')
req = urllib.request.Request('http://$VIP/v1/mcp/forecast', data=body, headers={
    'Host': '$INGRESS_HOST',
    'Content-Type': 'application/json',
    'Accept': 'application/json'
}, method='POST')
try:
    with urllib.request.urlopen(req, timeout=10) as r:
        print(r.status)
        print(r.read().decode('utf-8', errors='replace')[:160])
except urllib.error.HTTPError as e:
    print(e.code); print(e.read().decode('utf-8', errors='replace')[:160])
except Exception as e:
    print(0); print(e)
")
expect_check 4 401 "Unauthenticated MCP call (no bearer token)" "$UNAUTH_OUT"

pause

say "Act 6: BNK 2.4.1 AI Gateway — DSSM Virtual Key Model Discovery & Chat Completion [Checks 5/6 & 6/6]"
VK_TOKEN=$(kubectl get secret bnk-aigw-virtual-keys -n default -o jsonpath='{.data.AIGW_VIRTUAL_KEY}' 2>/dev/null | base64 -d 2>/dev/null || true)
note "Using DSSM Virtual Key: ${VK_TOKEN:0:18}... against BNK AI Gateway VIP http://$AIGW_VIP:8080"

MODELS_OUT=$(vnet_python "
import os, urllib.request, urllib.error
vk = '$VK_TOKEN' or os.environ.get('AIGW_VIRTUAL_KEY', '')
req = urllib.request.Request('http://$AIGW_VIP:8080/v1/models', headers={
    'Host': '$AIGW_HOST',
    'Accept': 'application/json',
    'Authorization': 'Bearer ' + vk
})
try:
    with urllib.request.urlopen(req, timeout=10) as r:
        print(r.status)
        print(r.read().decode('utf-8', errors='replace')[:160])
except urllib.error.HTTPError as e:
    print(e.code); print(e.read().decode('utf-8', errors='replace')[:160])
except Exception as e:
    print(0); print(e)
")
expect_check 5 200 "AI Gateway /v1/models with DSSM Virtual Key (f5vk_...)" "$MODELS_OUT"

CHAT_OUT=$(vnet_python "
import json, os, urllib.request, urllib.error
vk = '$VK_TOKEN' or os.environ.get('AIGW_VIRTUAL_KEY', '')
payload = json.dumps({
    'model': 'gpt-4o',
    'messages': [{'role': 'user', 'content': 'Forecast MSFT performance over 30 days in Sydney australiaeast'}],
    'max_tokens': 128
}).encode('utf-8')
req = urllib.request.Request('http://$AIGW_VIP:8080/v1/chat/completions', data=payload, headers={
    'Host': '$AIGW_HOST',
    'Content-Type': 'application/json',
    'Accept': 'application/json',
    'Authorization': 'Bearer ' + vk
}, method='POST')
try:
    with urllib.request.urlopen(req, timeout=15) as r:
        print(r.status)
        print(r.read().decode('utf-8', errors='replace')[:160])
except urllib.error.HTTPError as e:
    print(e.code); print(e.read().decode('utf-8', errors='replace')[:160])
except Exception as e:
    print(0); print(e)
")
expect_check 6 200 "AI Gateway /v1/chat/completions with DSSM Virtual Key (f5vk_...)" "$CHAT_OUT"

pause

say "Act 7: Observability & BNK Forge Multi-Cluster AI Gateway Fabric"
note "Telemetry emitted as BNKGOV records to Loki (http://loki.llm-egress:3100) and ClickHouse (otel.aigw_requests)"
note "Forge Multi-Cluster AI Gateway & Dependency Fabric active at: $FORGE"
for _bin in "$REPO_ROOT/azrbnkctl" "$REPO_ROOT/bin/azrbnkctl"; do
  if [ -x "$_bin" ]; then
    "$_bin" forge governance overview --cluster-name "$CLUSTER" --forge-url "$FORGE" 2>/dev/null || true
    break
  fi
done

say "Verification Summary: $PASSED/6 checks PASSED ($FAILED failed)"
if [ "$FAILED" -gt 0 ] || [ "$PASSED" -lt 6 ]; then
  bad "Expected 6/6 checks to PASS, got $PASSED/6"
  exit 1
fi
ok "All 6/6 AgentFoundry MCP + BNK 2.4.1 AI Gateway checks PASSED!"
exit 0
