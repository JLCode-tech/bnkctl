#!/usr/bin/env bash
# ==============================================================================
# setup-llm-gateway.sh — BNK 2.4.1 LLM Gateway 7-Step Provisioning & DSSM Setup
# Cluster: bnk-agentcore-demo (AWS EKS ap-southeast-2)
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEMO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${DEMO_DIR}/../.." && pwd)"

if [ -f "${REPO_ROOT}/examples/common/bnk-aigw-lib.sh" ]; then
  # shellcheck disable=SC1091
  source "${REPO_ROOT}/examples/common/bnk-aigw-lib.sh"
fi

CLUSTER="${CLUSTER:-bnk-agentcore-demo}"
CFG="${CFG:-${DEMO_DIR}/cluster.yaml}"
BNK_NS="${BNK_NS:-f5-cne-system}"
TENANT="${TENANT:-default-tenant}"
ROUTE_NAME="${ROUTE_NAME:-llm-chat-route}"
EXT_SELFIP_CIDR="$(kubectl get f5-big-net-vlan external-vlan -n "${BNK_NS}" -o jsonpath='{.spec.interfaces[0].selfIps[0]}' 2>/dev/null || true)"
EXT_OCTETS="${EXT_SELFIP_CIDR%.*}"
if [ -z "${EXT_OCTETS}" ]; then
  EXT_OCTETS="10.0.10"
fi
AIGW_VIP="${AIGW_VIP:-${EXT_OCTETS}.151}"
TMM_EGRESS_GATEWAY="${TMM_EGRESS_GATEWAY:-${EXT_OCTETS}.1}"
FORGE_URL="${FORGE_URL:-http://localhost:8000}"

if [ -z "${BEDROCK_PRIMARY_API_KEY:-}" ] || [ "${BEDROCK_PRIMARY_API_KEY}" = "replace-with-bedrock-primary-api-key" ]; then
  for envfile in /tmp/bedrock_keys.env /tmp/.bedrock-keys.env; do
    if [ -f "${envfile}" ]; then
      # shellcheck disable=SC1090
      source "${envfile}"
      if [ -n "${BEDROCK_PRIMARY_API_KEY:-}" ] && [ "${BEDROCK_PRIMARY_API_KEY}" != "replace-with-bedrock-primary-api-key" ]; then
        break
      fi
    fi
  done
fi
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
KUBE_DNS_IP="$(bnk_discover_cluster_dns_ip "172.20.0.10")"
echo "[Steps 1-5] Applying AITenant, AICredential, AIProvider, AIModel, 4-Object Route Bundle (Gateway/HTTPRoute/AIGatewayProfile/AppPolicy) & F5BigDnsCache (${KUBE_DNS_IP})..."
sed \
  -e "s|replace-with-bedrock-primary-api-key|${BEDROCK_PRIMARY_API_KEY}|g" \
  -e "s|replace-with-bedrock-fallback-api-key|${BEDROCK_FALLBACK_API_KEY}|g" \
  -e "s|\"10.0.10.151\"|\"${AIGW_VIP}\"|g" \
  -e "s|\"10.0.10.1\"|\"${TMM_EGRESS_GATEWAY}\"|g" \
  -e "s|172.20.0.10|${KUBE_DNS_IP}|g" \
  "${DEMO_DIR}/k8s/bnk-llm-gateway.yaml" | kubectl apply -f -

# Step 6: Restart CNE Controller in f5-cne-system to pick up net-resolver DNS + AIGatewayProfile
PROG_STATUS="$(kubectl get aigatewayprofiles.inference.k8s.f5.com "${ROUTE_NAME}" -n "${BNK_NS}" \
  -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null || true)"
if [ "${PROG_STATUS}" != "True" ]; then
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
else
  echo "[Step 6] AIGatewayProfile/${ROUTE_NAME} already Programmed=True in ${BNK_NS}; skipping CNE controller restart."
fi

bnk_wait_aigw_profile_programmed "${BNK_NS}" "${ROUTE_NAME}"

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
  TENANT_BASE="$(bnk_cne_resolve_tenant_base "${TENANT}" 18443)"
  ADMIN_PASS="$(bnk_get_aitenant_admin_password "${BNK_NS}" "${TENANT}")"
  BEARER_TOKEN="$(bnk_cne_login_bearer_token "${TENANT_BASE}" "${ADMIN_PASS}")"
  MODELS_JSON='["claude-sonnet-4-5","gpt-4o-mini"]'

  VK_FINANCE="$(bnk_provision_principal_vk "${TENANT_BASE}" "${BEARER_TOKEN}" "f5-agentcore-org" "agentcore-platform-team" "finance-agent-v2" "finance-agent-v2-vk" "500.0" "${MODELS_JSON}")"
  VK_FX="$(bnk_provision_principal_vk "${TENANT_BASE}" "${BEARER_TOKEN}" "sovereign-wealth-apac" "quant-trading-desk" "fx-arbitrage-agent" "fx-arbitrage-prod-vk" "1500.0" "${MODELS_JSON}")"
  VK_RESEARCH="$(bnk_provision_principal_vk "${TENANT_BASE}" "${BEARER_TOKEN}" "sovereign-wealth-apac" "quant-trading-desk" "dr-marcus-vance" "vance-research-notebook-vk" "1500.0" "${MODELS_JSON}")"
  VK_CREDIT="$(bnk_provision_principal_vk "${TENANT_BASE}" "${BEARER_TOKEN}" "sovereign-wealth-apac" "sovereign-risk-ops" "credit-underwriter-agent" "credit-underwriter-prod-vk" "1500.0" "${MODELS_JSON}")"
  VK_COPILOT="$(bnk_provision_principal_vk "${TENANT_BASE}" "${BEARER_TOKEN}" "sydney-retail-banking" "wealth-advisory-ai" "portfolio-copilot-agent" "portfolio-copilot-prod-vk" "800.0" "${MODELS_JSON}")"

  echo "Provisioned Virtual Keys across 3 organizations and 5 principals:"
  echo "  finance-agent-v2:        ${VK_FINANCE:-<existing>}"
  echo "  fx-arbitrage-agent:      ${VK_FX:-<existing>}"
  echo "  dr-marcus-vance:         ${VK_RESEARCH:-<existing>}"
  echo "  credit-underwriter-agent:${VK_CREDIT:-<existing>}"
  echo "  portfolio-copilot-agent: ${VK_COPILOT:-<existing>}"

  CH_PASS="$(bnk_get_clickhouse_password "${BNK_NS}")"

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

# Sync governance hierarchy to BNK Forge if reachable and bnkctl (or awsbnkctl) is built
FORGE_URL="${FORGE_URL:-http://localhost:8000}"
BNKCTL_BIN="$(bnk_find_cli "${REPO_ROOT}" || true)"
if [ -n "${BNKCTL_BIN}" ] && curl -sS -m 3 -o /dev/null "${FORGE_URL}/api/auth/login" 2>/dev/null; then
  echo "Syncing DSSM Governance hierarchy (3 organizations, 4 teams, 5 principals) to BNK Forge..."
  if [ ! -f "${REPO_ROOT}/.awsbnkctl/${CLUSTER}/forge_link.json" ] && [ ! -f "${REPO_ROOT}/.bnkctl/state/${CLUSTER}/forge_link.json" ]; then
    "${BNKCTL_BIN}" forge register --config "${CFG}" >/dev/null 2>&1 || true
  fi
  for spec in \
    "f5-agentcore-org|agentcore-platform-team|finance-agent-v2|finance-agent-v2-vk|500.0" \
    "sovereign-wealth-apac|quant-trading-desk|fx-arbitrage-agent|fx-arbitrage-prod-vk|1500.0" \
    "sovereign-wealth-apac|quant-trading-desk|dr-marcus-vance|vance-research-notebook-vk|1500.0" \
    "sovereign-wealth-apac|sovereign-risk-ops|credit-underwriter-agent|credit-underwriter-prod-vk|1500.0" \
    "sydney-retail-banking|wealth-advisory-ai|portfolio-copilot-agent|portfolio-copilot-prod-vk|800.0"
  do
    IFS='|' read -r f_org f_team f_prin f_vk f_budget <<< "${spec}"
    "${BNKCTL_BIN}" forge governance bootstrap --config "${CFG}" \
      --tenant "${TENANT}" --org "${f_org}" --team "${f_team}" \
      --principal "${f_prin}" --key-name "${f_vk}" \
      --budget-usd "${f_budget}" --rpm-limit 60 --tpm-limit 120000 >/dev/null 2>&1 || true
  done
  "${BNKCTL_BIN}" forge governance remediate restart_controller --config "${CFG}" -n "${BNK_NS}" >/dev/null 2>&1 || true
fi

# Ensure f5-toda-fluentd-custom retains stdout.conf alongside aigw-hsl.pipeline
# (f5-lifecycle-operator overwrites f5-toda-fluentd-custom when AI Gateway HSL is reconciled)
bnk_ensure_toda_fluentd_stdout "f5-cne-core"

echo "================================================================="
echo " BNK 2.4.1 LLM Gateway Setup Complete!"
echo "================================================================="

