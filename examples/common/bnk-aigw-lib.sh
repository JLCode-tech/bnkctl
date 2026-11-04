#!/usr/bin/env bash
# ==============================================================================
# bnk-aigw-lib.sh — Shared Multi-Cloud BNK 2.4.1 AI Gateway & DSSM Helpers
# Used by examples/{agentcore,agentfoundry,vertex-agent}-demo/scripts/setup-llm-gateway.sh
# ==============================================================================

# bnk_find_cli resolves the unified bnkctl binary (or legacy cloud-specific alias).
bnk_find_cli() {
  local repo_root="${1:-${REPO_ROOT:-.}}"
  local _bin
  for _bin in \
    "${repo_root}/bnkctl" \
    "${repo_root}/bin/bnkctl" \
    "${repo_root}/awsbnkctl" \
    "${repo_root}/bin/awsbnkctl" \
    "${repo_root}/azrbnkctl" \
    "${repo_root}/bin/azrbnkctl" \
    "${repo_root}/gkebnkctl" \
    "${repo_root}/bin/gkebnkctl"; do
    if [ -x "${_bin}" ]; then
      printf '%s' "${_bin}"
      return 0
    fi
  done
  if command -v bnkctl >/dev/null 2>&1; then
    command -v bnkctl
    return 0
  fi
  return 1
}

# bnk_discover_cluster_dns_ip queries kube-system/kube-dns (or coredns) ClusterIP
# and falls back to the supplied default IP when unreachable.
bnk_discover_cluster_dns_ip() {
  local fallback="${1:-10.96.0.10}"
  local ip=""
  ip="$(kubectl get svc kube-dns -n kube-system -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)"
  if [ -z "${ip}" ] || [ "${ip}" = "None" ]; then
    ip="$(kubectl get svc coredns -n kube-system -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)"
  fi
  if [ -z "${ip}" ] || [ "${ip}" = "None" ]; then
    ip="${fallback}"
  fi
  printf '%s' "${ip}"
}

# bnk_get_clickhouse_password retrieves the AI Gateway ClickHouse password from Secret.
bnk_get_clickhouse_password() {
  local bnk_ns="${1:-${BNK_NS:-f5-cne-system}}"
  local ch_pass=""
  ch_pass="$(kubectl get secret f5-aigateway-clickhouse-auth -n "${bnk_ns}" -o jsonpath='{.data.otel-password}' 2>/dev/null | base64 -d 2>/dev/null || true)"
  if [ -z "${ch_pass}" ]; then
    ch_pass="$(kubectl get secret f5-aigateway-clickhouse-auth -n "${bnk_ns}" -o jsonpath='{.data.password}' 2>/dev/null | base64 -d 2>/dev/null || true)"
  fi
  printf '%s' "${ch_pass}"
}

# bnk_get_aitenant_admin_password retrieves the AI Gateway tenant admin password from Secret.
bnk_get_aitenant_admin_password() {
  local bnk_ns="${1:-${BNK_NS:-f5-cne-system}}"
  local tenant="${2:-${TENANT:-f5-ai-gw}}"
  local admin_pass=""
  for _ in $(seq 1 15); do
    admin_pass="$(kubectl get secret "aitenant-${tenant}-admin" -n "${bnk_ns}" -o jsonpath='{.data.password}' 2>/dev/null | base64 -d 2>/dev/null || true)"
    [ -n "${admin_pass}" ] && break
    sleep 2
  done
  printf '%s' "${admin_pass}"
}

# bnk_ensure_toda_fluentd_stdout restores stdout.conf in f5-toda-fluentd-custom
# so bnkgov-collector -> Loki log streaming continues after FLO AI Gateway HSL sync.
bnk_ensure_toda_fluentd_stdout() {
  local core_ns="${1:-f5-cne-core}"
  if kubectl get configmap f5-toda-fluentd-custom -n "${core_ns}" >/dev/null 2>&1; then
    local has_stdout
    has_stdout="$(kubectl get configmap f5-toda-fluentd-custom -n "${core_ns}" -o jsonpath='{.data.stdout\.conf}' 2>/dev/null || true)"
    if [ -z "${has_stdout}" ]; then
      echo "Restoring stdout.conf in ${core_ns}/f5-toda-fluentd-custom for bnkgov-collector -> Loki streaming..."
      kubectl patch configmap f5-toda-fluentd-custom -n "${core_ns}" --type merge \
        -p '{"data":{"stdout.conf":"<store>\n  @type stdout\n</store>\n"}}' >/dev/null 2>&1 || true
      kubectl rollout restart deploy/f5-toda-fluentd -n "${core_ns}" >/dev/null 2>&1 || true
      kubectl rollout status deploy/f5-toda-fluentd -n "${core_ns}" --timeout=90s >/dev/null 2>&1 || true
    fi
  fi
}

# bnk_wait_aigw_profile_programmed waits for AIGatewayProfile Programmed=True.
bnk_wait_aigw_profile_programmed() {
  local bnk_ns="${1:-${BNK_NS:-f5-cne-system}}"
  local route_name="${2:-${ROUTE_NAME:-llm-chat-route}}"
  local prog_status=""
  echo "[Verify] Waiting for Gateway and AIGatewayProfile/${route_name} reconciliation in ${bnk_ns}..."
  for _ in $(seq 1 60); do
    prog_status="$(kubectl get aigatewayprofiles.inference.k8s.f5.com "${route_name}" -n "${bnk_ns}" \
      -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null || true)"
    if [ "${prog_status}" = "True" ]; then
      break
    fi
    sleep 2
  done
  kubectl get aigatewayprofiles.inference.k8s.f5.com "${route_name}" -n "${bnk_ns}" \
    -o jsonpath='Accepted={.status.conditions[?(@.type=="Accepted")].status} Programmed={.status.conditions[?(@.type=="Programmed")].status}{"\n"}' 2>/dev/null || true
}

# bnk_cne_resolve_tenant_base probes localhost:18443 for HTTP vs HTTPS and outputs TENANT_BASE URL.
bnk_cne_resolve_tenant_base() {
  local tenant="${1:-${TENANT:-default-tenant}}"
  local port="${2:-18443}"
  for _ in $(seq 1 15); do
    if curl -sS -o /dev/null "http://127.0.0.1:${port}/api/v1/tenants/${tenant}/governance/orgs" 2>/dev/null; then
      break
    fi
    sleep 1
  done
  if [ -n "${CNE_GOV_BASE:-}" ]; then
    printf '%s' "${CNE_GOV_BASE%/governance}"
    return 0
  fi
  local http_code
  http_code="$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:${port}/api/v1/tenants/${tenant}/governance/orgs" 2>/dev/null || echo "000")"
  if [ "${http_code}" != "000" ]; then
    printf 'http://127.0.0.1:%s/api/v1/tenants/%s' "${port}" "${tenant}"
  else
    printf 'https://127.0.0.1:%s/api/v1/tenants/%s' "${port}" "${tenant}"
  fi
}

# bnk_cne_login_bearer_token logs into the CNE controller tenant REST API and returns a bearer token.
bnk_cne_login_bearer_token() {
  local tenant_base="$1"
  local admin_pass="$2"
  local bearer_token=""
  if [ -z "${admin_pass}" ]; then
    return 0
  fi
  for _ in $(seq 1 20); do
    local login_json
    login_json="$(curl -sk -X POST "${tenant_base}/auth/login" -H "Content-Type: application/json" \
      -d "{\"username\":\"admin\",\"password\":\"${admin_pass}\"}" 2>/dev/null || true)"
    bearer_token="$(printf '%s' "${login_json}" | python3 -c "import sys,json; d=json.load(sys.stdin); print((d.get('data') or d).get('token') or (d.get('data') or d).get('access_token') or '')" 2>/dev/null || true)"
    if [ -n "${bearer_token}" ]; then
      break
    fi
    sleep 2
  done
  printf '%s' "${bearer_token}"
}

# bnk_provision_principal_vk provisions a DSSM org, monthly budget, team, principal, rate limit, and virtual key.
bnk_provision_principal_vk() {
  local tenant_base="$1"
  local bearer_token="$2"
  local org_name="$3"
  local team_name="$4"
  local prin_name="$5"
  local vk_name="$6"
  local budget_usd="$7"
  local models_json="${8:-[\"claude-sonnet-4-5\",\"gpt-4o-mini\"]}"
  local gov_base="${tenant_base}/governance"

  local org_json org_id team_json team_id prin_json prin_id vk_json vk_key
  org_json="$(curl -sk -X POST "${gov_base}/orgs" -H "Authorization: Bearer ${bearer_token}" -H "Content-Type: application/json" \
    -d "{\"id\":\"${org_name}\",\"name\":\"${org_name}\"}")"
  org_id="$(printf '%s' "${org_json}" | python3 -c "import sys,json; d=json.load(sys.stdin); print((d.get('data') or d).get('id') or '')" 2>/dev/null || true)"
  if [ -z "${org_id}" ]; then
    org_id="$(curl -sk "${gov_base}/orgs" -H "Authorization: Bearer ${bearer_token}" | python3 -c "import sys,json; d=json.load(sys.stdin); items=(d.get('items') or d.get('data') if isinstance(d,dict) else d) or []; print(next((x.get('id') for x in items if x.get('id')=='${org_name}' or x.get('name')=='${org_name}'), '${org_name}'))" 2>/dev/null || echo "${org_name}")"
  fi

  curl -sk -X POST "${gov_base}/budgets" -H "Authorization: Bearer ${bearer_token}" -H "Content-Type: application/json" \
    -d "{\"name\":\"${org_name}-monthly\",\"scope\":\"org\",\"targetId\":\"${org_id}\",\"limitUsd\":${budget_usd},\"period\":\"monthly\",\"hardLimit\":true}" >/dev/null || true

  team_json="$(curl -sk -X POST "${gov_base}/teams" -H "Authorization: Bearer ${bearer_token}" -H "Content-Type: application/json" \
    -d "{\"orgId\":\"${org_id}\",\"name\":\"${team_name}\"}")"
  team_id="$(printf '%s' "${team_json}" | python3 -c "import sys,json; d=json.load(sys.stdin); print((d.get('data') or d).get('id') or '')" 2>/dev/null || true)"
  if [ -z "${team_id}" ]; then
    team_id="$(curl -sk "${gov_base}/teams" -H "Authorization: Bearer ${bearer_token}" | python3 -c "import sys,json; d=json.load(sys.stdin); items=(d.get('items') or d.get('data') if isinstance(d,dict) else d) or []; print(next((x.get('id') for x in items if x.get('name')=='${team_name}' and x.get('orgId')=='${org_id}'), '${team_name}'))" 2>/dev/null || echo "${team_name}")"
  fi

  prin_json="$(curl -sk -X POST "${gov_base}/principals" -H "Authorization: Bearer ${bearer_token}" -H "Content-Type: application/json" \
    -d "{\"id\":\"${prin_name}\",\"orgId\":\"${org_id}\",\"teamId\":\"${team_id}\",\"name\":\"${prin_name}\",\"kind\":\"service\",\"modelScope\":${models_json}}")"
  prin_id="$(printf '%s' "${prin_json}" | python3 -c "import sys,json; d=json.load(sys.stdin); print((d.get('data') or d).get('id') or '')" 2>/dev/null || true)"
  if [ -z "${prin_id}" ]; then
    prin_id="$(curl -sk "${gov_base}/principals" -H "Authorization: Bearer ${bearer_token}" | python3 -c "import sys,json; d=json.load(sys.stdin); items=(d.get('items') or d.get('data') if isinstance(d,dict) else d) or []; print(next((x.get('id') for x in items if x.get('id')=='${prin_name}' or x.get('name')=='${prin_name}'), '${prin_name}'))" 2>/dev/null || echo "${prin_name}")"
  fi

  curl -sk -X PUT "${gov_base}/principals/${prin_id}/ratelimit" -H "Authorization: Bearer ${bearer_token}" -H "Content-Type: application/json" \
    -d '{"requestsPerMinute":60,"tokensPerMinute":120000}' >/dev/null || true

  for old_vk_id in $(curl -sk "${tenant_base}/virtual-keys" -H "Authorization: Bearer ${bearer_token}" | python3 -c "import sys,json; d=json.load(sys.stdin); items=(d.get('items') or d.get('data') if isinstance(d,dict) else d) or []; print(' '.join(str(x.get('id')) for x in items if isinstance(x,dict) and x.get('id') and x.get('name')=='${vk_name}' and (not x.get('principalId') or x.get('principalId')=='${prin_id}')))" 2>/dev/null || true); do
    curl -sk -X DELETE "${tenant_base}/virtual-keys/${old_vk_id}" -H "Authorization: Bearer ${bearer_token}" >/dev/null 2>&1 || true
  done

  vk_json="$(curl -sk -X POST "${tenant_base}/virtual-keys" -H "Authorization: Bearer ${bearer_token}" -H "Content-Type: application/json" \
    -d "{\"principalId\":\"${prin_id}\",\"name\":\"${vk_name}\",\"allowedModels\":${models_json}}")"
  vk_key="$(printf '%s' "${vk_json}" | python3 -c 'import sys,json; d=json.load(sys.stdin); u=d.get("data") if isinstance(d.get("data"),dict) else d; print(u.get("token") or u.get("key") or u.get("cleartext_key") or u.get("api_key") or "")' 2>/dev/null || true)"
  if [ -z "${vk_key}" ]; then
    vk_json="$(curl -sk -X POST "${tenant_base}/virtual-keys" -H "Authorization: Bearer ${bearer_token}" -H "Content-Type: application/json" \
      -d "{\"principalId\":\"${prin_id}\",\"name\":\"${vk_name}-$(date +%s)\",\"allowedModels\":${models_json}}")"
    vk_key="$(printf '%s' "${vk_json}" | python3 -c 'import sys,json; d=json.load(sys.stdin); u=d.get("data") if isinstance(d.get("data"),dict) else d; print(u.get("token") or u.get("key") or u.get("cleartext_key") or u.get("api_key") or "")' 2>/dev/null || true)"
  fi
  printf '%s' "${vk_key}"
}
