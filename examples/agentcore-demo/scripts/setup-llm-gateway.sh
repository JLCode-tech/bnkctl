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

export BEDROCK_PRIMARY_API_KEY="${BEDROCK_PRIMARY_API_KEY:-replace-with-bedrock-primary-api-key}"
export BEDROCK_FALLBACK_API_KEY="${BEDROCK_FALLBACK_API_KEY:-replace-with-bedrock-fallback-api-key}"

echo "================================================================="
echo " F5 BNK 2.4.1 LLM Gateway Setup — ${CLUSTER} (AWS EKS)"
echo "================================================================="

# Steps 1-5: Apply 7-Step BNK 2.4.1 LLM Gateway Manifests in f5-bnk
echo "[Steps 1-5] Applying AITenant, AICredential, AIProvider, AIModel, 4-Object Route Bundle (Gateway/HTTPRoute/AIGatewayProfile/AppPolicy), F5BigDnsCache & F5SPKStaticRoute..."
sed \
  -e "s|replace-with-bedrock-primary-api-key|${BEDROCK_PRIMARY_API_KEY}|g" \
  -e "s|replace-with-bedrock-fallback-api-key|${BEDROCK_FALLBACK_API_KEY}|g" \
  -e "s|10.0.10.151|${AIGW_VIP}|g" \
  -e "s|10.0.10.1|${TMM_EGRESS_GATEWAY}|g" \
  "${DEMO_DIR}/k8s/bnk-llm-gateway.yaml" | kubectl apply -f -

# Step 6: Restart CNE Controller in f5-bnk to pick up net-resolver DNS + AIGatewayProfile
echo "[Step 6] Restarting CNE controller in ${BNK_NS} to activate net-resolver DNS and AIGatewayProfile..."
if kubectl get deploy -n "${BNK_NS}" -l app=cne-controller -o name 2>/dev/null | grep -q .; then
  kubectl rollout restart deploy -n "${BNK_NS}" -l app=cne-controller
  kubectl rollout status deploy -n "${BNK_NS}" -l app=cne-controller --timeout=180s
elif kubectl get pods -n "${BNK_NS}" --no-headers 2>/dev/null | awk '{print $1}' | grep -qE '^f5-cne-controller'; then
  kubectl delete pod -n "${BNK_NS}" $(kubectl get pods -n "${BNK_NS}" --no-headers | awk '/^f5-cne-controller/{print $1}')
  sleep 5
fi

echo "[Verify] Checking AIGatewayProfileAccepted condition on AIGatewayProfile/${ROUTE_NAME} in ${BNK_NS}..."
kubectl get aigatewayprofiles.inference.k8s.f5.com "${ROUTE_NAME}" -n "${BNK_NS}" \
  -o jsonpath='{.status.conditions[?(@.type=="AIGatewayProfileAccepted")].status}{"\n"}' 2>/dev/null || true

# Step 7: Provision DSSM Organization, Team, Principal, Budget, RateLimit & Virtual Key (f5vk_...)
echo "[Step 7] Provisioning DSSM Governance hierarchy & Virtual API Key (f5vk_...)..."
if [ -x "${REPO_ROOT}/bin/awsbnkctl" ]; then
  "${REPO_ROOT}/bin/awsbnkctl" forge governance bootstrap \
    -f "${DEMO_DIR}/cluster.yaml" \
    --forge-url "${FORGE_URL}" \
    --tenant "${TENANT}" \
    --org "f5-agentcore-org" \
    --team "bedrock-agents" \
    --principal "finance-agent-v2" \
    --key-name "finance-agent-v2-vk" \
    --model "claude-sonnet-4-5" \
    --budget-usd 500 \
    --rpm-limit 60 \
    --tpm-limit 120000 && exit 0 || echo "Forge bootstrap unavailable; falling back to direct CNE controller :8443 governance REST API..."
fi

PF_PID=""
cleanup() {
  if [ -n "${PF_PID}" ]; then kill "${PF_PID}" 2>/dev/null || true; fi
}
trap cleanup EXIT

CNE_POD="$(kubectl get pods -n "${BNK_NS}" --no-headers 2>/dev/null | awk '/cne-controller/{print $1; exit}')"
if [ -n "${CNE_POD}" ]; then
  kubectl port-forward -n "${BNK_NS}" "pod/${CNE_POD}" 18443:8443 >/dev/null 2>&1 &
  PF_PID=$!
  sleep 3
  GOV_BASE="${CNE_GOV_BASE:-https://127.0.0.1:18443/api/v1/tenants/${TENANT}/governance}"

  ORG_JSON="$(curl -sk -X POST "${GOV_BASE}/orgs" -H "Content-Type: application/json" \
    -d '{"name":"f5-agentcore-org"}')"
  ORG_ID="$(printf '%s' "${ORG_JSON}" | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d.get("id") or "f5-agentcore-org")' 2>/dev/null || echo "f5-agentcore-org")"

  curl -sk -X POST "${GOV_BASE}/budgets" -H "Content-Type: application/json" \
    -d "{\"name\":\"f5-agentcore-org-monthly\",\"scope\":\"org\",\"targetId\":\"${ORG_ID}\",\"limitUsd\":500.0,\"period\":\"monthly\",\"hardLimit\":true}" >/dev/null || true

  TEAM_JSON="$(curl -sk -X POST "${GOV_BASE}/teams" -H "Content-Type: application/json" \
    -d "{\"orgId\":\"${ORG_ID}\",\"name\":\"bedrock-agents\"}")"
  TEAM_ID="$(printf '%s' "${TEAM_JSON}" | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d.get("id") or "bedrock-agents")' 2>/dev/null || echo "bedrock-agents")"

  PRIN_JSON="$(curl -sk -X POST "${GOV_BASE}/principals" -H "Content-Type: application/json" \
    -d "{\"orgId\":\"${ORG_ID}\",\"teamId\":\"${TEAM_ID}\",\"name\":\"finance-agent-v2\",\"kind\":\"agent\"}")"
  PRIN_ID="$(printf '%s' "${PRIN_JSON}" | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d.get("id") or "finance-agent-v2")' 2>/dev/null || echo "finance-agent-v2")"

  curl -sk -X PUT "${GOV_BASE}/principals/${PRIN_ID}/ratelimit" -H "Content-Type: application/json" \
    -d '{"requestsPerMinute":60,"tokensPerMinute":120000}' >/dev/null || true

  VK_JSON="$(curl -sk -X POST "${GOV_BASE}/virtual-keys" -H "Content-Type: application/json" \
    -d "{\"principalId\":\"${PRIN_ID}\",\"name\":\"finance-agent-v2-vk\",\"allowedModels\":[\"claude-sonnet-4-5\",\"gpt-4o-mini\"]}")"
  VK_CLEARTEXT="$(printf '%s' "${VK_JSON}" | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d.get("key") or d.get("cleartext_key") or d.get("api_key") or "")' 2>/dev/null || true)"
  echo "Provisioned Virtual Key: ${VK_CLEARTEXT:-<see CNE response>}"
fi

echo "================================================================="
echo " BNK 2.4.1 LLM Gateway Setup Complete!"
echo "================================================================="
