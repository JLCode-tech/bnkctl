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
