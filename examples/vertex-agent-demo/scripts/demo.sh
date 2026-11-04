#!/usr/bin/env bash
# ==============================================================================
# demo.sh — End-to-End Demo & 6/6 Governance Verification Suite
# Google Cloud Vertex AI + F5 BNK 2.4.1 on GKE (australia-southeast1)
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEMO_DIR="$(dirname "${SCRIPT_DIR}")"

MODE="full"
CLUSTER="${CLUSTER:-bnk-vertex-demo}"
for arg in "$@"; do
  case "${arg}" in
    --check|--quick)
      MODE="${arg#--}"
      ;;
    *)
      CLUSTER="${arg}"
      ;;
  esac
done

REPO_ROOT="$(cd "${DEMO_DIR}/../.." && pwd)"
STATE_DIR="${HOME}/.gkebnkctl/state/${CLUSTER}"
for candidate in \
  "${REPO_ROOT}/.gkebnkctl/${CLUSTER}" \
  "${PWD}/.gkebnkctl/${CLUSTER}" \
  "${HOME}/.gkebnkctl/${CLUSTER}" \
  "${HOME}/.gkebnkctl/state/${CLUSTER}" \
  "${HOME}/.bnkctl/state/${CLUSTER}"; do
  if [ -f "${candidate}/state.env" ] || [ -f "${candidate}/kubeconfig" ]; then
    STATE_DIR="${candidate}"
    break
  fi
done
if [ -f "${STATE_DIR}/kubeconfig" ] && [ -z "${KUBECONFIG:-}" ]; then
  export KUBECONFIG="${STATE_DIR}/kubeconfig"
fi
if [ -f "${STATE_DIR}/state.env" ]; then
  # shellcheck disable=SC1090
  source "${STATE_DIR}/state.env"
fi

if [ "${MODE}" = "full" ]; then
  "${SCRIPT_DIR}/setup-llm-gateway.sh" "${CLUSTER}"
fi

echo "================================================================="
echo " F5 BNK 2.4.1 AI Gateway & MCP Governance Verification (6/6)"
echo "================================================================="

kubectl exec -i -n default deploy/vertex-traffic-generator -- python3 - <<'PY'
import json
import os
import sys
import time
import urllib.error
import urllib.request

vip = os.environ.get("BNK_VIP", "10.10.1.10")
aigw_vip = os.environ.get("AIGW_VIP", "10.10.1.11")
ingress_host = os.environ.get("INGRESS_HOST", "bnk-ingress.bnk-demo.internal")
aigw_host = os.environ.get("AIGW_HOST", "aigw.bnk-demo.internal")
agent_token = os.environ.get("AGENT_TOKEN", "demo-agent-token-a7f3c1")
external_token = os.environ.get("EXTERNAL_TOKEN", "demo-external-token-4b9e2d")

vk_token = os.environ.get("VK_VESSEL_SCHEDULER", "")
if not vk_token and os.environ.get("PROFILES_JSON"):
    try:
        vk_token = json.loads(os.environ["PROFILES_JSON"])[0]["token"]
    except Exception:
        pass

aigw_url = f"http://{aigw_vip}:8080/v1/chat/completions"
mcp_url = f"http://{vip}/v1/mcp/forecast"

def post(url, payload, headers, timeout=60, retries=3):
    for attempt in range(retries):
        req = urllib.request.Request(
            url,
            data=json.dumps(payload).encode("utf-8"),
            headers={"Content-Type": "application/json", **headers},
            method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                return resp.status, resp.read().decode("utf-8", errors="replace")
        except urllib.error.HTTPError as e:
            return e.code, e.read().decode("utf-8", errors="replace")
        except Exception as e:
            if attempt + 1 < retries:
                time.sleep(2)
                continue
            return 0, str(e)

checks = []

# 1. Valid Virtual Key -> 200 OK
st1, body1 = post(
    aigw_url,
    {"model": "gemini-2-5-pro", "messages": [{"role": "user", "content": "Reply with OK"}], "max_tokens": 32},
    {"Host": aigw_host, "Authorization": f"Bearer {vk_token}"},
)
checks.append(("1. Valid Virtual Key (gemini-2-5-pro) -> 200 OK", st1 == 200, f"HTTP {st1}"))

# 2. Unauthenticated Chat Request -> 401 Unauthorized
st2, body2 = post(
    aigw_url,
    {"model": "gemini-2-5-pro", "messages": [{"role": "user", "content": "Reply with OK"}], "max_tokens": 16},
    {"Host": aigw_host},
)
checks.append(("2. Unauthenticated Chat Request -> 401 Unauthorized", st2 == 401, f"HTTP {st2}"))

# 3. Disallowed Model (claude-3-7-sonnet) -> 403 Forbidden
st3, body3 = post(
    aigw_url,
    {"model": "claude-3-7-sonnet", "messages": [{"role": "user", "content": "Reply with OK"}], "max_tokens": 16},
    {"Host": aigw_host, "Authorization": f"Bearer {vk_token}"},
)
checks.append(("3. Disallowed Model (claude-3-7-sonnet) -> 403 Forbidden", st3 == 403, f"HTTP {st3}"))

# 4. Trusted Agent MCP forecast -> 200 OK
sess_ok = f"verify-ok-{int(time.time())}"
st4, body4 = post(
    mcp_url,
    {"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "forecast", "arguments": {"symbol": "GOOGL", "days": 30}}},
    {"Host": ingress_host, "Authorization": f"Bearer {agent_token}", "Mcp-Session-Id": sess_ok},
)
checks.append(("4. Trusted Agent MCP forecast -> 200 OK", st4 == 200, f"HTTP {st4}"))

# 5. External Caller Privileged Tool get_account_balance -> 403 Forbidden (tool_forbidden)
sess_forb = f"verify-forb-{int(time.time())}"
st5, body5 = post(
    mcp_url,
    {"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": {"name": "get_account_balance", "arguments": {"account_id": "ACC-1001"}}},
    {"Host": ingress_host, "Authorization": f"Bearer {external_token}", "Mcp-Session-Id": sess_forb},
)
checks.append(("5. External Caller get_account_balance -> 403 Forbidden (tool_forbidden)", st5 == 403, f"HTTP {st5}"))

# 6. Burst Rate Limit (12x requests) -> 429 Too Many Requests (rate_limited)
sess_rl = f"verify-burst-{int(time.time())}"
statuses = []
for i in range(12):
    st_i, _ = post(
        mcp_url,
        {"jsonrpc": "2.0", "id": 100 + i, "method": "tools/call", "params": {"name": "forecast", "arguments": {"symbol": "GOOGL", "days": 7}}},
        {"Host": ingress_host, "Authorization": f"Bearer {agent_token}", "Mcp-Session-Id": sess_rl},
    )
    statuses.append(st_i)
got_429 = 429 in statuses
checks.append(("6. Burst Rate Limit (12x requests) -> 429 Too Many Requests (rate_limited)", got_429, f"statuses={statuses}"))

passed = 0
for label, ok, detail in checks:
    mark = "PASS" if ok else "FAIL"
    print(f"  [{mark}] {label} ({detail})")
    if ok:
        passed += 1

print("-----------------------------------------------------------------")
print(f"  Result: {passed}/{len(checks)} Governance & Security Checks Passed")
print("-----------------------------------------------------------------")
if passed != len(checks):
    sys.exit(1)
PY
