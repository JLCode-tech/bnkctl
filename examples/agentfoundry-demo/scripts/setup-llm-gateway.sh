#!/usr/bin/env bash
# ==============================================================================
# setup-llm-gateway.sh — BNK 2.4.1 LLM Gateway 7-Step Provisioning & DSSM Setup
# Cluster: bnk-agentfoundry-demo (Azure AKS australiaeast)
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEMO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${DEMO_DIR}/../.." && pwd)"

if [ -f "${REPO_ROOT}/examples/common/bnk-aigw-lib.sh" ]; then
  # shellcheck disable=SC1091
  source "${REPO_ROOT}/examples/common/bnk-aigw-lib.sh"
fi

CLUSTER="${1:-${CLUSTER:-bnk-agentfoundry-demo}}"
CFG="${CFG:-${DEMO_DIR}/cluster.yaml}"
BNK_NS="${BNK_NS:-f5-cne-system}"
TENANT="${TENANT:-default-tenant}"
ROUTE_NAME="${ROUTE_NAME:-llm-chat-route}"
AIGW_VIP="${AIGW_VIP:-10.0.17.101}"
TMM_EGRESS_GATEWAY="${TMM_EGRESS_GATEWAY:-10.0.17.1}"
FORGE_URL="${FORGE_URL:-http://localhost:8000}"

if [ -z "${KUBECONFIG:-}" ]; then
  for _kc in "${REPO_ROOT}/.azrbnkctl/${CLUSTER}/kubeconfig" \
             "${DEMO_DIR}/.azrbnkctl/${CLUSTER}/kubeconfig" \
             "${HOME}/.kube/config"; do
    if [ -f "${_kc}" ]; then KUBECONFIG="${_kc}"; break; fi
  done
fi
export KUBECONFIG="${KUBECONFIG:-}"

export AZURE_FOUNDRY_PRIMARY_API_KEY="${AZURE_FOUNDRY_PRIMARY_API_KEY:-replace-with-azure-foundry-primary-api-key}"
export AZURE_FOUNDRY_FALLBACK_API_KEY="${AZURE_FOUNDRY_FALLBACK_API_KEY:-replace-with-azure-foundry-fallback-api-key}"

echo "================================================================="
echo " F5 BNK 2.4.1 LLM Gateway Setup — ${CLUSTER} (Azure AKS)"
echo "================================================================="

KUBE_DNS_IP="$(kubectl get svc kube-dns -n kube-system -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)"
KUBE_DNS_IP="${KUBE_DNS_IP:-10.240.0.10}"

# Steps 1-5: Apply 7-Step BNK 2.4.1 LLM Gateway Manifests in f5-cne-system + default
echo "[Steps 1-5] Applying AITenant, AICredential, AIProvider, AIModel, 4-Object Route Bundle (Gateway/HTTPRoute/AIGatewayProfile/AppPolicy) & F5BigDnsCache (${KUBE_DNS_IP})..."
sed \
  -e "s|replace-with-azure-foundry-primary-api-key|${AZURE_FOUNDRY_PRIMARY_API_KEY}|g" \
  -e "s|replace-with-azure-foundry-fallback-api-key|${AZURE_FOUNDRY_FALLBACK_API_KEY}|g" \
  -e "s|\"10.0.17.101\"|\"${AIGW_VIP}\"|g" \
  -e "s|\"10.0.17.1\"|\"${TMM_EGRESS_GATEWAY}\"|g" \
  -e "s|10.240.0.10|${KUBE_DNS_IP}|g" \
  -e "s|10.0.0.10|${KUBE_DNS_IP}|g" \
  "${DEMO_DIR}/k8s/bnk-llm-gateway.yaml" | kubectl apply -f -

if kubectl get deploy azure-foundry-upstream -n default >/dev/null 2>&1; then
  kubectl rollout status deploy/azure-foundry-upstream -n default --timeout=120s || true
  UPSTREAM_POD_IP="$(kubectl get pods -n default -l app=azure-foundry-upstream -o jsonpath='{.items[0].status.podIP}' 2>/dev/null || true)"
  if [ -n "${UPSTREAM_POD_IP}" ] && [ "${AZURE_FOUNDRY_PRIMARY_API_KEY}" = "replace-with-azure-foundry-primary-api-key" ]; then
    echo "Configuring AIProvider endpoints to in-cluster Azure AI Foundry responder (http://${UPSTREAM_POD_IP}:8080)..."
    sed \
      -e "s|replace-with-azure-foundry-primary-api-key|${AZURE_FOUNDRY_PRIMARY_API_KEY}|g" \
      -e "s|replace-with-azure-foundry-fallback-api-key|${AZURE_FOUNDRY_FALLBACK_API_KEY}|g" \
      -e "s|https://australiaeast.api.cognitive.microsoft.com/models|http://${UPSTREAM_POD_IP}:8080/models|g" \
      -e "s|https://eastus2.api.cognitive.microsoft.com/openai/v1|http://${UPSTREAM_POD_IP}:8080/openai/v1|g" \
      -e "s|\"10.0.17.101\"|\"${AIGW_VIP}\"|g" \
      -e "s|\"10.0.17.1\"|\"${TMM_EGRESS_GATEWAY}\"|g" \
      -e "s|10.240.0.10|${KUBE_DNS_IP}|g" \
      -e "s|10.0.0.10|${KUBE_DNS_IP}|g" \
      "${DEMO_DIR}/k8s/bnk-llm-gateway.yaml" | kubectl apply -f -
  fi
fi

# Step 6: Restart CNE Controller in f5-cne-system to pick up net-resolver DNS + AIGatewayProfile
PROG_STATUS="$(kubectl get aigatewayprofiles.inference.k8s.f5.com "${ROUTE_NAME}" -n "${BNK_NS}" \
  -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null || true)"
if [ "${PROG_STATUS}" != "True" ]; then
  echo "[Step 6] Restarting CNE controller in ${BNK_NS} to activate net-resolver DNS and AIGatewayProfile..."
  kubectl get pods -n "${BNK_NS}" --no-headers 2>/dev/null | awk '/^f5-cne-controller/{print $1}' | xargs -r kubectl delete pod -n "${BNK_NS}" --wait=true || true
  for _ in $(seq 1 60); do
    if [ "$(kubectl get pods -n "${BNK_NS}" --no-headers 2>/dev/null | grep -c '^f5-cne-controller')" -eq 1 ] && \
       kubectl get pods -n "${BNK_NS}" --no-headers 2>/dev/null | grep -qE '^f5-cne-controller.*3/3.*Running'; then
      break
    fi
    sleep 2
  done
else
  echo "[Step 6] AIGatewayProfile/${ROUTE_NAME} already Programmed=True in ${BNK_NS}; skipping CNE controller restart."
fi

echo "[Verify] Waiting for Gateway and AIGatewayProfile/${ROUTE_NAME} reconciliation in ${BNK_NS}..."
for _ in $(seq 1 60); do
  PROG_STATUS="$(kubectl get aigatewayprofiles.inference.k8s.f5.com "${ROUTE_NAME}" -n "${BNK_NS}" \
    -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null || true)"
  if [ "${PROG_STATUS}" = "True" ]; then
    break
  fi
  sleep 2
done
kubectl get aigatewayprofiles.inference.k8s.f5.com "${ROUTE_NAME}" -n "${BNK_NS}" \
  -o jsonpath='Accepted={.status.conditions[?(@.type=="Accepted")].status} Programmed={.status.conditions[?(@.type=="Programmed")].status}{"\n"}' 2>/dev/null || true

# Step 7: Provision DSSM Organizations (3), Teams (3), Principals (5), Budgets, RateLimits & Virtual Keys (f5vk_...)
echo "[Step 7] Provisioning DSSM Governance hierarchy (3 organizations, 5 principals) & Virtual API Keys (f5vk_...)..."
PF_PID=""
cleanup() {
  if [ -n "${PF_PID}" ]; then kill "${PF_PID}" 2>/dev/null || true; fi
}
trap cleanup EXIT

CNE_POD="$(kubectl get pods -n "${BNK_NS}" --no-headers 2>/dev/null | awk '/cne-controller/ && $2=="3/3" && $3=="Running"{print $1; exit}')"
if [ -z "${CNE_POD}" ]; then
  CNE_POD="$(kubectl get pods -n "${BNK_NS}" --no-headers 2>/dev/null | awk '/cne-controller/ && $3=="Running"{print $1; exit}')"
fi

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

  ADMIN_PASS=""
  for _ in $(seq 1 15); do
    ADMIN_PASS="$(kubectl get secret "aitenant-${TENANT}-admin" -n "${BNK_NS}" -o jsonpath='{.data.password}' 2>/dev/null | base64 -d 2>/dev/null || true)"
    [ -n "${ADMIN_PASS}" ] && break
    sleep 2
  done
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
      -d "{\"id\":\"${prin_name}\",\"orgId\":\"${org_id}\",\"teamId\":\"${team_id}\",\"name\":\"${prin_name}\",\"kind\":\"service\",\"modelScope\":[\"gpt-4o\",\"gpt-4o-mini\"]}")"
    prin_id="$(printf '%s' "${prin_json}" | python3 -c "import sys,json; d=json.load(sys.stdin); print((d.get('data') or d).get('id') or '')" 2>/dev/null || true)"
    if [ -z "${prin_id}" ]; then
      prin_id="$(curl -sk "${GOV_BASE}/principals" -H "Authorization: Bearer ${BEARER_TOKEN}" | python3 -c "import sys,json; d=json.load(sys.stdin); items=(d.get('items') or d.get('data') if isinstance(d,dict) else d) or []; print(next((x.get('id') for x in items if x.get('id')=='${prin_name}' or x.get('name')=='${prin_name}'), '${prin_name}'))" 2>/dev/null || echo "${prin_name}")"
    fi

    curl -sk -X PUT "${GOV_BASE}/principals/${prin_id}/ratelimit" -H "Authorization: Bearer ${BEARER_TOKEN}" -H "Content-Type: application/json" \
      -d '{"requestsPerMinute":60,"tokensPerMinute":120000}' >/dev/null || true

    vk_json="$(curl -sk -X POST "${TENANT_BASE}/virtual-keys" -H "Authorization: Bearer ${BEARER_TOKEN}" -H "Content-Type: application/json" \
      -d "{\"principalId\":\"${prin_id}\",\"name\":\"${vk_name}\",\"allowedModels\":[\"gpt-4o\",\"gpt-4o-mini\"]}")"
    vk_key="$(printf '%s' "${vk_json}" | python3 -c 'import sys,json; d=json.load(sys.stdin); u=d.get("data") if isinstance(d.get("data"),dict) else d; print(u.get("token") or u.get("key") or u.get("cleartext_key") or u.get("api_key") or "")' 2>/dev/null || true)"
    if [ -z "${vk_key}" ]; then
      vk_json="$(curl -sk -X POST "${TENANT_BASE}/virtual-keys" -H "Authorization: Bearer ${BEARER_TOKEN}" -H "Content-Type: application/json" \
        -d "{\"principalId\":\"${prin_id}\",\"name\":\"${vk_name}-$(date +%s)\",\"allowedModels\":[\"gpt-4o\",\"gpt-4o-mini\"]}")"
      vk_key="$(printf '%s' "${vk_json}" | python3 -c 'import sys,json; d=json.load(sys.stdin); u=d.get("data") if isinstance(d.get("data"),dict) else d; print(u.get("token") or u.get("key") or u.get("cleartext_key") or u.get("api_key") or "")' 2>/dev/null || true)"
    fi
    printf '%s' "${vk_key}"
  }

  VK_CLAIMS="$(provision_principal_vk "atlantic-healthcare-group" "clinical-claims-triage" "claims-adjudicator-agent" "claims-adjudicator-prod-vk" "1500.0")"
  VK_PRIOR_AUTH="$(provision_principal_vk "atlantic-healthcare-group" "clinical-claims-triage" "prior-auth-clinical-agent" "prior-auth-clinical-vk" "1500.0")"
  VK_ACTUARIAL="$(provision_principal_vk "azure-insurtech-foundry" "actuarial-cat-modeling" "actuarial-sim-agent" "actuarial-sim-prod-vk" "1200.0")"
  VK_WORKBENCH="$(provision_principal_vk "azure-insurtech-foundry" "actuarial-cat-modeling" "elena-rostova-actuary" "rostova-foundry-workbench-vk" "1200.0")"
  VK_FINANCE="$(provision_principal_vk "f5-foundry-org" "foundry-agents" "foundry-finance-agent" "foundry-finance-agent-vk" "500.0")"

  echo "Provisioned Virtual Keys across 3 organizations and 5 principals:"
  echo "  claims-adjudicator-agent:  ${VK_CLAIMS:-<existing>}"
  echo "  prior-auth-clinical-agent: ${VK_PRIOR_AUTH:-<existing>}"
  echo "  actuarial-sim-agent:       ${VK_ACTUARIAL:-<existing>}"
  echo "  elena-rostova-actuary:     ${VK_WORKBENCH:-<existing>}"
  echo "  foundry-finance-agent:     ${VK_FINANCE:-<existing>}"

  CH_PASS="$(kubectl get secret f5-aigateway-clickhouse-auth -n "${BNK_NS}" -o jsonpath='{.data.otel-password}' 2>/dev/null | base64 -d 2>/dev/null || true)"
  if [ -z "${CH_PASS}" ]; then
    CH_PASS="$(kubectl get secret f5-aigateway-clickhouse-auth -n "${BNK_NS}" -o jsonpath='{.data.password}' 2>/dev/null | base64 -d 2>/dev/null || true)"
  fi

  PRIMARY_VK="${VK_CLAIMS:-${VK_FINANCE}}"
  if [ -n "${VK_CLAIMS}${VK_PRIOR_AUTH}${VK_ACTUARIAL}${VK_WORKBENCH}${VK_FINANCE}" ]; then
    kubectl create secret generic bnk-aigw-virtual-keys -n default \
      --from-literal=AIGW_VIRTUAL_KEY="${PRIMARY_VK}" \
      --from-literal=BNK_VIRTUAL_KEY="${PRIMARY_VK}" \
      --from-literal=F5VK_CLAIMS_TOKEN="${VK_CLAIMS}" \
      --from-literal=F5VK_PRIOR_AUTH_TOKEN="${VK_PRIOR_AUTH}" \
      --from-literal=F5VK_ACTUARIAL_TOKEN="${VK_ACTUARIAL}" \
      --from-literal=F5VK_WORKBENCH_TOKEN="${VK_WORKBENCH}" \
      --from-literal=F5VK_FINANCE_TOKEN="${VK_FINANCE}" \
      --from-literal=CLICKHOUSE_USER="otel" \
      --from-literal=CLICKHOUSE_PASSWORD="${CH_PASS}" \
      --dry-run=client -o yaml | kubectl apply -f -

    if kubectl get deploy agentfoundry-traffic-generator -n default >/dev/null 2>&1; then
      kubectl rollout restart deploy/agentfoundry-traffic-generator -n default
    fi
  fi
fi

# Sync governance hierarchy to BNK Forge if reachable
AZRBNKCTL_BIN=""
for _bin in "${REPO_ROOT}/bnkctl" "${REPO_ROOT}/bin/bnkctl" "${REPO_ROOT}/azrbnkctl" "${REPO_ROOT}/bin/azrbnkctl"; do
  if [ -x "${_bin}" ]; then AZRBNKCTL_BIN="${_bin}"; break; fi
done

if [ -n "${AZRBNKCTL_BIN}" ] && curl -sS -m 3 -o /dev/null "${FORGE_URL}/api/auth/login" 2>/dev/null; then
  echo "Syncing DSSM Governance hierarchy (3 organizations, 3 teams, 5 principals) to BNK Forge..."
  if [ ! -f "${REPO_ROOT}/.azrbnkctl/${CLUSTER}/forge_link.json" ]; then
    "${AZRBNKCTL_BIN}" forge register --config "${CFG}" >/dev/null 2>&1 || true
  fi
  for spec in \
    "atlantic-healthcare-group|clinical-claims-triage|claims-adjudicator-agent|claims-adjudicator-prod-vk|1500.0" \
    "atlantic-healthcare-group|clinical-claims-triage|prior-auth-clinical-agent|prior-auth-clinical-vk|1500.0" \
    "azure-insurtech-foundry|actuarial-cat-modeling|actuarial-sim-agent|actuarial-sim-prod-vk|1200.0" \
    "azure-insurtech-foundry|actuarial-cat-modeling|elena-rostova-actuary|rostova-foundry-workbench-vk|1200.0" \
    "f5-foundry-org|foundry-agents|foundry-finance-agent|foundry-finance-agent-vk|500.0"
  do
    IFS='|' read -r f_org f_team f_prin f_vk f_budget <<< "${spec}"
    "${AZRBNKCTL_BIN}" forge governance bootstrap --config "${CFG}" \
      --tenant "${TENANT}" --org "${f_org}" --team "${f_team}" \
      --principal "${f_prin}" --key-name "${f_vk}" \
      --model "gpt-4o" \
      --budget-usd "${f_budget}" --rpm-limit 60 --tpm-limit 120000 >/dev/null 2>&1 || true
  done
fi

# Ensure f5-toda-fluentd-custom retains stdout.conf alongside aigw-hsl.pipeline
if declare -F bnk_ensure_toda_fluentd_stdout >/dev/null 2>&1; then
  bnk_ensure_toda_fluentd_stdout "f5-cne-core"
elif kubectl get configmap f5-toda-fluentd-custom -n f5-cne-core >/dev/null 2>&1; then
  HAS_STDOUT="$(kubectl get configmap f5-toda-fluentd-custom -n f5-cne-core -o jsonpath='{.data.stdout\.conf}' 2>/dev/null || true)"
  if [ -z "${HAS_STDOUT}" ]; then
    echo "Restoring stdout.conf in f5-cne-core/f5-toda-fluentd-custom for bnkgov-collector -> Loki streaming..."
    kubectl patch configmap f5-toda-fluentd-custom -n f5-cne-core --type merge \
      -p '{"data":{"stdout.conf":"<store>\n  @type stdout\n</store>\n"}}' >/dev/null 2>&1 || true
    kubectl rollout restart deploy/f5-toda-fluentd -n f5-cne-core >/dev/null 2>&1 || true
    kubectl rollout status deploy/f5-toda-fluentd -n f5-cne-core --timeout=90s >/dev/null 2>&1 || true
  fi
fi

echo "================================================================="
echo " BNK 2.4.1 LLM Gateway Setup Complete!"
echo "================================================================="
