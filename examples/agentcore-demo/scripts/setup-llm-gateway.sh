#!/usr/bin/env bash
# ==============================================================================
# setup-llm-gateway.sh — BNK 2.4.1 LLM Gateway 7-Step Provisioning & DSSM Setup
# Cluster: bnk-agentcore-demo (AWS EKS ap-southeast-2)
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEMO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${DEMO_DIR}/../.." && pwd)"

CLUSTER="${CLUSTER:-bnk-agentcore-demo}"
BNK_NS="${BNK_NS:-f5-cne-system}"
TENANT="${TENANT:-default-tenant}"
ROUTE_NAME="${ROUTE_NAME:-llm-chat-route}"
AIGW_VIP="${AIGW_VIP:-10.0.10.151}"
TMM_EGRESS_GATEWAY="${TMM_EGRESS_GATEWAY:-10.0.10.1}"
FORGE_URL="${FORGE_URL:-http://localhost:8000}"

if [ -z "${BEDROCK_PRIMARY_API_KEY:-}" ] || [ "${BEDROCK_PRIMARY_API_KEY}" = "replace-with-bedrock-primary-api-key" ]; then
  for sname in bedrock-primary-api-key bedrock-primary-key; do
    EXISTING_PRIMARY="$(kubectl get secret "${sname}" -n "${BNK_NS}" -o jsonpath='{.data.api-key}' 2>/dev/null | base64 -d 2>/dev/null || true)"
    if [ -n "${EXISTING_PRIMARY}" ] && [ "${EXISTING_PRIMARY}" != "replace-with-bedrock-primary-api-key" ]; then
      BEDROCK_PRIMARY_API_KEY="${EXISTING_PRIMARY}"
      break
    fi
  done
  if { [ -z "${BEDROCK_PRIMARY_API_KEY:-}" ] || [ "${BEDROCK_PRIMARY_API_KEY}" = "replace-with-bedrock-primary-api-key" ]; } && [ -f "${REPO_ROOT}/.awsbnkctl/bnk-agentcore-demo/kubeconfig" ]; then
    for sname in bedrock-primary-api-key bedrock-primary-key; do
      EXISTING_PRIMARY="$(kubectl --kubeconfig "${REPO_ROOT}/.awsbnkctl/bnk-agentcore-demo/kubeconfig" get secret "${sname}" -n "${BNK_NS}" -o jsonpath='{.data.api-key}' 2>/dev/null | base64 -d 2>/dev/null || true)"
      if [ -n "${EXISTING_PRIMARY}" ] && [ "${EXISTING_PRIMARY}" != "replace-with-bedrock-primary-api-key" ]; then
        BEDROCK_PRIMARY_API_KEY="${EXISTING_PRIMARY}"
        break
      fi
    done
  fi
fi
if [ -z "${BEDROCK_FALLBACK_API_KEY:-}" ] || [ "${BEDROCK_FALLBACK_API_KEY}" = "replace-with-bedrock-fallback-api-key" ]; then
  for sname in bedrock-fallback-api-key bedrock-fallback-key; do
    EXISTING_FALLBACK="$(kubectl get secret "${sname}" -n "${BNK_NS}" -o jsonpath='{.data.api-key}' 2>/dev/null | base64 -d 2>/dev/null || true)"
    if [ -n "${EXISTING_FALLBACK}" ] && [ "${EXISTING_FALLBACK}" != "replace-with-bedrock-fallback-api-key" ]; then
      BEDROCK_FALLBACK_API_KEY="${EXISTING_FALLBACK}"
      break
    fi
  done
  if [ -z "${BEDROCK_FALLBACK_API_KEY:-}" ] || [ "${BEDROCK_FALLBACK_API_KEY}" = "replace-with-bedrock-fallback-api-key" ]; then
    BEDROCK_FALLBACK_API_KEY="${BEDROCK_PRIMARY_API_KEY:-replace-with-bedrock-fallback-api-key}"
  fi
fi
export BEDROCK_PRIMARY_API_KEY="${BEDROCK_PRIMARY_API_KEY:-replace-with-bedrock-primary-api-key}"
export BEDROCK_FALLBACK_API_KEY="${BEDROCK_FALLBACK_API_KEY:-replace-with-bedrock-fallback-api-key}"

echo "================================================================="
echo " F5 BNK 2.4.1 LLM Gateway Setup — ${CLUSTER} (AWS EKS)"
echo "================================================================="

# Steps 1-5: Apply 7-Step BNK 2.4.1 LLM Gateway Manifests in f5-cne-system
KUBE_DNS_IP="$(kubectl get svc kube-dns -n kube-system -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)"
KUBE_DNS_IP="${KUBE_DNS_IP:-172.20.0.10}"
echo "[Steps 1-5] Applying AITenant, AICredential, AIProvider, AIModel, 4-Object Route Bundle (Gateway/HTTPRoute/AIGatewayProfile/AppPolicy) & F5BigDnsCache (${KUBE_DNS_IP})..."
sed \
  -e "s|replace-with-bedrock-primary-api-key|${BEDROCK_PRIMARY_API_KEY}|g" \
  -e "s|replace-with-bedrock-fallback-api-key|${BEDROCK_FALLBACK_API_KEY}|g" \
  -e "s|\"10.0.10.151\"|\"${AIGW_VIP}\"|g" \
  -e "s|\"10.0.10.1\"|\"${TMM_EGRESS_GATEWAY}\"|g" \
  -e "s|172.20.0.10|${KUBE_DNS_IP}|g" \
  "${DEMO_DIR}/k8s/bnk-llm-gateway.yaml" | kubectl apply -f -

# Step 6: Restart CNE Controller in f5-bnk to pick up net-resolver DNS + AIGatewayProfile
echo "[Step 6] Restarting CNE controller in ${BNK_NS} to activate net-resolver DNS and AIGatewayProfile..."
if kubectl get deploy f5-cne-controller -n "${BNK_NS}" >/dev/null 2>&1; then
  kubectl rollout restart deploy/f5-cne-controller -n "${BNK_NS}"
  kubectl rollout status deploy/f5-cne-controller -n "${BNK_NS}" --timeout=180s
elif kubectl get deploy -n "${BNK_NS}" -l app=cne-controller -o name 2>/dev/null | grep -q .; then
  kubectl rollout restart deploy -n "${BNK_NS}" -l app=cne-controller
  kubectl rollout status deploy -n "${BNK_NS}" -l app=cne-controller --timeout=180s
fi
for _ in $(seq 1 30); do
  if [ "$(kubectl get pods -n "${BNK_NS}" --no-headers 2>/dev/null | grep -c '^f5-cne-controller')" -eq 1 ] && \
     kubectl get pods -n "${BNK_NS}" --no-headers 2>/dev/null | grep -qE '^f5-cne-controller.*3/3.*Running'; then
    break
  fi
  sleep 2
done

echo "[Verify] Checking AIGatewayProfileAccepted condition on AIGatewayProfile/${ROUTE_NAME} in ${BNK_NS}..."
kubectl get aigatewayprofiles.inference.k8s.f5.com "${ROUTE_NAME}" -n "${BNK_NS}" \
  -o jsonpath='{.status.conditions[?(@.type=="AIGatewayProfileAccepted")].status}{"\n"}' 2>/dev/null || true

# Step 7: Provision DSSM Organizations (3), Teams (4), Principals (5), Budgets, RateLimits & Virtual Keys (f5vk_...)
echo "[Step 7] Provisioning DSSM Governance hierarchy (3 organizations, 5 principals) & Virtual API Keys (f5vk_...)..."
PF_PID=""
cleanup() {
  if [ -n "${PF_PID}" ]; then kill "${PF_PID}" 2>/dev/null || true; fi
}
trap cleanup EXIT

CNE_POD="$(kubectl get pods -n "${BNK_NS}" --no-headers 2>/dev/null | awk '/cne-controller/ && $2=="3/3" && $3=="Running"{print $1; exit}')"
if [ -n "${CNE_POD}" ]; then
  kubectl port-forward -n "${BNK_NS}" "pod/${CNE_POD}" 18443:8443 >/dev/null 2>&1 &
  PF_PID=$!
  for _ in $(seq 1 15); do
    if curl -sS -o /dev/null "http://127.0.0.1:18443/api/v1/tenants/${TENANT}/governance/orgs" 2>/dev/null; then
      break
    fi
    sleep 1
  done
  if [ -z "${CNE_GOV_BASE:-}" ]; then
    HTTP_CODE="$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:18443/api/v1/tenants/${TENANT}/governance/orgs" 2>/dev/null || echo "000")"
    if [ "${HTTP_CODE}" != "000" ]; then
      TENANT_BASE="http://127.0.0.1:18443/api/v1/tenants/${TENANT}"
    else
      TENANT_BASE="https://127.0.0.1:18443/api/v1/tenants/${TENANT}"
    fi
    GOV_BASE="${TENANT_BASE}/governance"
  else
    GOV_BASE="${CNE_GOV_BASE}"
    TENANT_BASE="${GOV_BASE%/governance}"
  fi

  ADMIN_PASS="$(kubectl get secret "aitenant-${TENANT}-admin" -n "${BNK_NS}" -o jsonpath='{.data.password}' 2>/dev/null | base64 -d 2>/dev/null || true)"
  BEARER_TOKEN=""
  if [ -n "${ADMIN_PASS}" ]; then
    for _ in $(seq 1 20); do
      LOGIN_JSON="$(curl -sk -X POST "${TENANT_BASE}/auth/login" -H "Content-Type: application/json" \
        -d "{\"username\":\"admin\",\"password\":\"${ADMIN_PASS}\"}" 2>/dev/null || true)"
      BEARER_TOKEN="$(printf '%s' "${LOGIN_JSON}" | python3 -c "import sys,json; d=json.load(sys.stdin); print((d.get('data') or d).get('token') or (d.get('data') or d).get('access_token') or '')" 2>/dev/null || true)"
      if [ -n "${BEARER_TOKEN}" ]; then
        break
      fi
      sleep 2
    done
  fi

  provision_principal_vk() {
    local org_name="$1"
    local team_name="$2"
    local prin_name="$3"
    local vk_name="$4"
    local budget_usd="$5"

    local org_json org_id team_json team_id prin_json prin_id vk_json vk_key
    org_json="$(curl -sk -X POST "${GOV_BASE}/orgs" -H "Authorization: Bearer ${BEARER_TOKEN}" -H "Content-Type: application/json" \
      -d "{\"id\":\"${org_name}\",\"name\":\"${org_name}\"}")"
    org_id="$(printf '%s' "${org_json}" | python3 -c "import sys,json; d=json.load(sys.stdin); print((d.get('data') or d).get('id') or '')" 2>/dev/null || true)"
    if [ -z "${org_id}" ]; then
      org_id="$(curl -sk "${GOV_BASE}/orgs" -H "Authorization: Bearer ${BEARER_TOKEN}" | python3 -c "import sys,json; d=json.load(sys.stdin); items=(d.get('items') or d.get('data') if isinstance(d,dict) else d) or []; print(next((x.get('id') for x in items if x.get('id')=='${org_name}' or x.get('name')=='${org_name}'), '${org_name}'))" 2>/dev/null || echo "${org_name}")"
    fi

    curl -sk -X POST "${GOV_BASE}/budgets" -H "Authorization: Bearer ${BEARER_TOKEN}" -H "Content-Type: application/json" \
      -d "{\"name\":\"${org_name}-monthly\",\"scope\":\"org\",\"targetId\":\"${org_id}\",\"limitUsd\":${budget_usd},\"period\":\"monthly\",\"hardLimit\":true}" >/dev/null || true

    team_json="$(curl -sk -X POST "${GOV_BASE}/teams" -H "Authorization: Bearer ${BEARER_TOKEN}" -H "Content-Type: application/json" \
      -d "{\"orgId\":\"${org_id}\",\"name\":\"${team_name}\"}")"
    team_id="$(printf '%s' "${team_json}" | python3 -c "import sys,json; d=json.load(sys.stdin); print((d.get('data') or d).get('id') or '')" 2>/dev/null || true)"
    if [ -z "${team_id}" ]; then
      team_id="$(curl -sk "${GOV_BASE}/teams" -H "Authorization: Bearer ${BEARER_TOKEN}" | python3 -c "import sys,json; d=json.load(sys.stdin); items=(d.get('items') or d.get('data') if isinstance(d,dict) else d) or []; print(next((x.get('id') for x in items if x.get('name')=='${team_name}' and x.get('orgId')=='${org_id}'), '${team_name}'))" 2>/dev/null || echo "${team_name}")"
    fi

    prin_json="$(curl -sk -X POST "${GOV_BASE}/principals" -H "Authorization: Bearer ${BEARER_TOKEN}" -H "Content-Type: application/json" \
      -d "{\"id\":\"${prin_name}\",\"orgId\":\"${org_id}\",\"teamId\":\"${team_id}\",\"name\":\"${prin_name}\",\"kind\":\"service\",\"modelScope\":[\"claude-sonnet-4-5\",\"gpt-4o-mini\"]}")"
    prin_id="$(printf '%s' "${prin_json}" | python3 -c "import sys,json; d=json.load(sys.stdin); print((d.get('data') or d).get('id') or '')" 2>/dev/null || true)"
    if [ -z "${prin_id}" ]; then
      prin_id="$(curl -sk "${GOV_BASE}/principals" -H "Authorization: Bearer ${BEARER_TOKEN}" | python3 -c "import sys,json; d=json.load(sys.stdin); items=(d.get('items') or d.get('data') if isinstance(d,dict) else d) or []; print(next((x.get('id') for x in items if x.get('id')=='${prin_name}' or x.get('name')=='${prin_name}'), '${prin_name}'))" 2>/dev/null || echo "${prin_name}")"
    fi

    curl -sk -X PUT "${GOV_BASE}/principals/${prin_id}/ratelimit" -H "Authorization: Bearer ${BEARER_TOKEN}" -H "Content-Type: application/json" \
      -d '{"requestsPerMinute":60,"tokensPerMinute":120000}' >/dev/null || true

    vk_json="$(curl -sk -X POST "${TENANT_BASE}/virtual-keys" -H "Authorization: Bearer ${BEARER_TOKEN}" -H "Content-Type: application/json" \
      -d "{\"principalId\":\"${prin_id}\",\"name\":\"${vk_name}\",\"allowedModels\":[\"claude-sonnet-4-5\",\"gpt-4o-mini\"]}")"
    vk_key="$(printf '%s' "${vk_json}" | python3 -c 'import sys,json; d=json.load(sys.stdin); u=d.get("data") if isinstance(d.get("data"),dict) else d; print(u.get("token") or u.get("key") or u.get("cleartext_key") or u.get("api_key") or "")' 2>/dev/null || true)"
    if [ -z "${vk_key}" ]; then
      vk_json="$(curl -sk -X POST "${TENANT_BASE}/virtual-keys" -H "Authorization: Bearer ${BEARER_TOKEN}" -H "Content-Type: application/json" \
        -d "{\"principalId\":\"${prin_id}\",\"name\":\"${vk_name}-$(date +%s)\",\"allowedModels\":[\"claude-sonnet-4-5\",\"gpt-4o-mini\"]}")"
      vk_key="$(printf '%s' "${vk_json}" | python3 -c 'import sys,json; d=json.load(sys.stdin); u=d.get("data") if isinstance(d.get("data"),dict) else d; print(u.get("token") or u.get("key") or u.get("cleartext_key") or u.get("api_key") or "")' 2>/dev/null || true)"
    fi
    printf '%s' "${vk_key}"
  }

  VK_FINANCE="$(provision_principal_vk "f5-agentcore-org" "agentcore-platform-team" "finance-agent-v2" "finance-agent-v2-vk" "500.0")"
  VK_FX="$(provision_principal_vk "sovereign-wealth-apac" "quant-trading-desk" "fx-arbitrage-agent" "fx-arbitrage-prod-vk" "1500.0")"
  VK_RESEARCH="$(provision_principal_vk "sovereign-wealth-apac" "quant-trading-desk" "dr-marcus-vance" "vance-research-notebook-vk" "1500.0")"
  VK_CREDIT="$(provision_principal_vk "sovereign-wealth-apac" "sovereign-risk-ops" "credit-underwriter-agent" "credit-underwriter-prod-vk" "1500.0")"
  VK_COPILOT="$(provision_principal_vk "sydney-retail-banking" "wealth-advisory-ai" "portfolio-copilot-agent" "portfolio-copilot-prod-vk" "800.0")"

  echo "Provisioned Virtual Keys across 3 organizations and 5 principals:"
  echo "  finance-agent-v2:        ${VK_FINANCE:-<existing>}"
  echo "  fx-arbitrage-agent:      ${VK_FX:-<existing>}"
  echo "  dr-marcus-vance:         ${VK_RESEARCH:-<existing>}"
  echo "  credit-underwriter-agent:${VK_CREDIT:-<existing>}"
  echo "  portfolio-copilot-agent: ${VK_COPILOT:-<existing>}"

  CH_PASS="$(kubectl get secret f5-aigateway-clickhouse-auth -n "${BNK_NS}" -o jsonpath='{.data.otel-password}' 2>/dev/null | base64 -d 2>/dev/null || true)"

  if [ -n "${VK_FINANCE}${VK_FX}${VK_RESEARCH}${VK_CREDIT}${VK_COPILOT}" ]; then
    kubectl create secret generic bnk-aigw-virtual-keys -n default \
      --from-literal=AIGW_VIRTUAL_KEY="${VK_FX:-${VK_FINANCE}}" \
      --from-literal=FINANCE_AGENT_V2_VK="${VK_FINANCE}" \
      --from-literal=FX_ARBITRAGE_PROD_VK="${VK_FX}" \
      --from-literal=VANCE_RESEARCH_NOTEBOOK_VK="${VK_RESEARCH}" \
      --from-literal=CREDIT_UNDERWRITER_PROD_VK="${VK_CREDIT}" \
      --from-literal=PORTFOLIO_COPILOT_PROD_VK="${VK_COPILOT}" \
      --from-literal=F5VK_FINANCE_TOKEN="${VK_FINANCE}" \
      --from-literal=F5VK_FX_TOKEN="${VK_FX}" \
      --from-literal=F5VK_RESEARCH_TOKEN="${VK_RESEARCH}" \
      --from-literal=F5VK_CREDIT_TOKEN="${VK_CREDIT}" \
      --from-literal=F5VK_COPILOT_TOKEN="${VK_COPILOT}" \
      --from-literal=CLICKHOUSE_USER="otel" \
      --from-literal=CLICKHOUSE_PASSWORD="${CH_PASS}" \
      --dry-run=client -o yaml | kubectl apply -f -

    if kubectl get deploy agentcore-traffic-generator -n default >/dev/null 2>&1; then
      kubectl rollout restart deploy/agentcore-traffic-generator -n default
    fi
  fi
fi

echo "================================================================="
echo " BNK 2.4.1 LLM Gateway Setup Complete!"
echo "================================================================="
