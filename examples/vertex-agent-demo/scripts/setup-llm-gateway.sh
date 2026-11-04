#!/usr/bin/env bash
# ==============================================================================
# setup-llm-gateway.sh — BNK 2.4.1 LLM Gateway & MCP Stack Provisioning (GKE)
# Dynamically discovers GCP project, state.env VIPs, DSSM virtual keys, and Forge
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEMO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${DEMO_DIR}/../.." && pwd)"

if [ -f "${REPO_ROOT}/examples/common/bnk-aigw-lib.sh" ]; then
  # shellcheck disable=SC1091
  source "${REPO_ROOT}/examples/common/bnk-aigw-lib.sh"
fi

CLUSTER="${1:-${CLUSTER:-bnk-vertex-demo}}"
BNK_NS="${BNK_NS:-f5-cne-system}"
CORE_NS="${CORE_NS:-f5-cne-core}"
TENANT="${TENANT:-default-tenant}"
ROUTE_NAME="${ROUTE_NAME:-llm-chat-route}"
FORGE_URL="${FORGE_URL:-http://localhost:8000}"
FORGE_USER="${FORGE_USER:-admin}"
FORGE_PASS="${FORGE_PASS:-admin123}"
FORGE_PROJECT="${FORGE_PROJECT:-gkebnkctl-${CLUSTER}}"

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

GCP_PROJECT="${GCP_PROJECT:-$(gcloud config get-value project 2>/dev/null | tr -d '\r\n')}"
if [ -z "${GCP_PROJECT}" ] || [ "${GCP_PROJECT}" = "(unset)" ]; then
  echo "ERROR: GCP_PROJECT is not set and gcloud config get-value project returned empty" >&2
  exit 1
fi

EXT_ALIAS_CIDR="${TMM_EXT_SELF_CIDR:-${TMM_EXT_ALIAS_CIDR:-10.10.1.0/28}}"
read -r BNK_VIP AIGW_VIP < <(python3 - "${EXT_ALIAS_CIDR}" "${BNK_VIP:-}" "${AIGW_VIP:-}" <<'PY'
import ipaddress, sys
cidr, bnk_override, aigw_override = sys.argv[1], sys.argv[2], sys.argv[3]
net = ipaddress.ip_network(cidr, strict=False)
bnk = bnk_override or str(net.network_address + 10)
aigw = aigw_override or str(net.network_address + 11)
print(f"{bnk} {aigw}")
PY
)

EXT_NET_NAME="$(kubectl get infra infra -n "${BNK_NS}" -o jsonpath='{.spec.networks[0].name}' 2>/dev/null || true)"
EXT_NET_NAME="${EXT_NET_NAME:-external}"

echo "================================================================="
echo " F5 BNK 2.4.1 LLM Gateway Setup — ${CLUSTER} (GCP: ${GCP_PROJECT})"
echo "   KUBECONFIG : ${KUBECONFIG:-default}"
echo "   Infra Net  : ${EXT_NET_NAME}"
echo "   MCP VIP    : ${BNK_VIP}"
echo "   AIGW VIP   : ${AIGW_VIP}"
echo "================================================================="

# 1. Enable stdout logging in f5-toda-fluentd so BNKGOV iRule logs reach Loki
for ns in "${BNK_NS}" "${CORE_NS}"; do
  if kubectl get configmap f5-toda-fluentd-custom -n "${ns}" >/dev/null 2>&1; then
    kubectl patch configmap f5-toda-fluentd-custom -n "${ns}" --type merge \
      -p '{"data":{"stdout.conf":"<store>\n  @type stdout\n</store>\n"}}' >/dev/null 2>&1 || true
  fi
done

# 2. Deploy MCP Financial Tool, Observability (Loki + bnkgov-collector), and Vertex Token Shipper
echo "[Step 1] Deploying MCP Financial Tool, Loki Observability & Vertex AI OAuth2 Token Shipper..."
kubectl apply -k "${DEMO_DIR}/mcp-tool"
kubectl apply -f "${DEMO_DIR}/mcp-observability.yaml"

echo "[Step 2] Minting GCP OAuth2 bearer token & discovering active Vertex AI Gemini project..."
TOK="$(gcloud auth application-default print-access-token 2>/dev/null || gcloud auth print-access-token 2>/dev/null || true)"
VERTEX_PROJECT="${VERTEX_PROJECT:-${GCP_PROJECT}}"
if [ -n "${TOK}" ]; then
  probe_code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 8 \
    "https://australia-southeast1-aiplatform.googleapis.com/v1beta1/projects/${VERTEX_PROJECT}/locations/australia-southeast1/endpoints/openapi/chat/completions" \
    -H "Authorization: Bearer ${TOK}" \
    -H "Content-Type: application/json" \
    -d '{"model":"google/gemini-2.5-flash","messages":[{"role":"user","content":"OK"}],"max_tokens":4}' 2>/dev/null || echo 000)"
  if [ "${probe_code}" != "200" ]; then
    for cand in $(gcloud projects list --filter="projectId:*gemini*" --format="value(projectId)" 2>/dev/null || true); do
      c_code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 8 \
        "https://australia-southeast1-aiplatform.googleapis.com/v1beta1/projects/${cand}/locations/australia-southeast1/endpoints/openapi/chat/completions" \
        -H "Authorization: Bearer ${TOK}" \
        -H "Content-Type: application/json" \
        -d '{"model":"google/gemini-2.5-flash","messages":[{"role":"user","content":"OK"}],"max_tokens":4}' 2>/dev/null || echo 000)"
      if [ "${c_code}" = "200" ]; then
        VERTEX_PROJECT="${cand}"
        echo "  Discovered active Vertex AI Gemini project: ${VERTEX_PROJECT}"
        break
      fi
    done
  fi
fi

sed "s|YOUR_GCP_PROJECT_ID|${VERTEX_PROJECT}|g" "${DEMO_DIR}/mcp-vertex-token-shipper.yaml" | kubectl apply -f -
if [ -z "${TOK}" ]; then
  for _ in $(seq 1 15); do
    TOK="$(kubectl get secret vertex-gemini-primary-api-key -n "${BNK_NS}" -o jsonpath='{.data.api-key}' 2>/dev/null | base64 -d 2>/dev/null || true)"
    if [ -n "${TOK}" ] && [ "${TOK}" != "replace-with-vertex-gemini-primary-api-key" ]; then
      break
    fi
    sleep 2
  done
fi
TOK="${TOK:-replace-with-vertex-gemini-primary-api-key}"
for sec in vertex-gemini-primary-api-key vertex-gemini-fallback-api-key; do
  kubectl create secret generic "${sec}" -n "${BNK_NS}" \
    --from-literal=api-key="${TOK}" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null 2>&1 || true
done

# 3. Apply MCP Gateway, Persistence, Security Policy (iRule + F5BigFwPolicy), and BNK 2.4.1 AI Gateway
echo "[Step 3] Applying MCP Gateway (${BNK_VIP}), Security Policies, and BNK 2.4.1 AI Gateway (${AIGW_VIP}, Vertex project=${VERTEX_PROJECT})..."
sed \
  -e "s|10.10.1.10|${BNK_VIP}|g" \
  -e "s|name: external|name: ${EXT_NET_NAME}|g" \
  "${DEMO_DIR}/gateway-deployment.yaml" | kubectl apply -f -

kubectl apply -f "${DEMO_DIR}/mcp-persistence.yaml"
kubectl apply -f "${DEMO_DIR}/mcp-security-policy.yaml"

sed \
  -e "s|YOUR_GCP_PROJECT_ID|${VERTEX_PROJECT}|g" \
  -e "s|replace-with-vertex-gemini-primary-api-key|${TOK}|g" \
  -e "s|replace-with-vertex-gemini-fallback-api-key|${TOK}|g" \
  -e "s|10.10.1.11|${AIGW_VIP}|g" \
  -e "s|name: external|name: ${EXT_NET_NAME}|g" \
  "${DEMO_DIR}/aigateway-241.yaml" | kubectl apply -f -

# Ensure TMM pod has f5-traffic=true label for Toda fluentd log collection
TMM_POD="$(kubectl get pods -n "${BNK_NS}" -l app=f5-tmm -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
if [ -n "${TMM_POD}" ]; then
  kubectl label pod "${TMM_POD}" -n "${BNK_NS}" f5-traffic=true --overwrite >/dev/null 2>&1 || true
fi

echo "[Step 4] Waiting for Gateways and AIGatewayProfile/${ROUTE_NAME} to be Programmed/Accepted..."
for _ in $(seq 1 45); do
  GW1="$(kubectl get gateway bnk-vertex-demo-gateway -n default -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null || true)"
  GW2="$(kubectl get gateway llm-chat-route-gw -n "${BNK_NS}" -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null || true)"
  PROF="$(kubectl get aigatewayprofiles.inference.k8s.f5.com "${ROUTE_NAME}" -n "${BNK_NS}" -o jsonpath='{.status.conditions[?(@.type=="Accepted")].status}' 2>/dev/null || true)"
  if [ "${GW1}" = "True" ] && [ "${GW2}" = "True" ]; then
    echo "  Gateways Programmed=True (AIGatewayProfile Accepted=${PROF:-True})"
    break
  fi
  sleep 2
done

# 4. Provision DSSM Governance Hierarchy & Virtual Keys via CNE Controller :8443 REST API
echo "[Step 5] Provisioning DSSM Governance hierarchy & Virtual Keys (f5vk_...) via CNE Controller :8443..."
PF_PID=""
cleanup() {
  if [ -n "${PF_PID}" ]; then kill "${PF_PID}" 2>/dev/null || true; fi
}
trap cleanup EXIT

CNE_POD="$(kubectl get pods -n "${BNK_NS}" --no-headers 2>/dev/null | awk '/cne-controller/{print $1; exit}')"
ADMIN_PASS="$(bnk_get_aitenant_admin_password "${BNK_NS}" "${TENANT}")"
CH_PASS="$(bnk_get_clickhouse_password "${BNK_NS}")"

if [ -n "${CNE_POD}" ] && [ -n "${ADMIN_PASS}" ]; then
  kubectl port-forward -n "${BNK_NS}" "svc/f5-cne-controller-api" 18443:8443 >/dev/null 2>&1 &
  PF_PID=$!
  sleep 3

  PROFILES_JSON="$(python3 - "${ADMIN_PASS}" "${TENANT}" <<'PY'
import json, ssl, sys, time, urllib.error, urllib.request

admin_pass = sys.argv[1]
tenant = sys.argv[2]
ctx = ssl.create_default_context()
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE

tenant_base = f"https://127.0.0.1:18443/api/v1/tenants/{tenant}"
try:
    urllib.request.urlopen(f"http://127.0.0.1:18443/api/v1/tenants/{tenant}/governance/orgs", timeout=3)
    tenant_base = f"http://127.0.0.1:18443/api/v1/tenants/{tenant}"
except urllib.error.HTTPError:
    tenant_base = f"http://127.0.0.1:18443/api/v1/tenants/{tenant}"
except Exception:
    pass

gov_base = f"{tenant_base}/governance"

token = ""
for _ in range(15):
    try:
        login_req = urllib.request.Request(
            f"{tenant_base}/auth/login",
            data=json.dumps({"username": "admin", "password": admin_pass}).encode("utf-8"),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(login_req, context=ctx if tenant_base.startswith("https") else None, timeout=10) as r:
            d = json.loads(r.read().decode("utf-8", errors="replace"))
            u = d.get("data") if isinstance(d.get("data"), dict) else d
            token = u.get("token") or u.get("access_token") or ""
            if token:
                break
    except Exception:
        pass
    time.sleep(2)

def req_url(method, full_url, body=None):
    data = json.dumps(body).encode("utf-8") if body is not None else None
    headers = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}
    r = urllib.request.Request(full_url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(r, context=ctx if full_url.startswith("https") else None, timeout=10) as resp:
            raw = resp.read().decode("utf-8", errors="replace")
            return resp.status, (json.loads(raw) if raw.strip() else {})
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", errors="replace")
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, {"error": raw}
    except Exception as e:
        return 0, {"error": str(e)}

def req(method, path, body=None):
    return req_url(method, f"{gov_base}{path}", body)

def unwrap_data(obj):
    if isinstance(obj, dict) and isinstance(obj.get("data"), dict):
        return obj["data"]
    return obj if isinstance(obj, dict) else {}

def unwrap_list(obj, key=None):
    if isinstance(obj, list):
        return obj
    if isinstance(obj, dict):
        if key and isinstance(obj.get(key), list):
            return obj[key]
        for k in ("items", "orgs", "teams", "principals", "virtualKeys", "virtual_keys", "budgets", "data"):
            if isinstance(obj.get(k), list):
                return obj[k]
    return []

def ensure_org(name, budget_usd, models=None):
    scope = models or ["gemini-2-5-pro", "gpt-4o-mini"]
    _, created = req("POST", "/orgs", {"id": name, "name": name, "displayName": name, "modelScope": scope})
    org_id = unwrap_data(created).get("id") or unwrap_data(created).get("orgId") or ""
    if not org_id:
        _, existing = req("GET", "/orgs")
        for o in unwrap_list(existing):
            if o.get("name") == name or o.get("id") == name:
                org_id = o.get("id") or o.get("orgId") or name
                break
    org_id = org_id or name
    req("PUT", f"/orgs/{org_id}", {"displayName": name, "modelScope": scope})
    req("POST", "/budgets", {
        "name": f"{name}-monthly",
        "scope": "org",
        "targetId": org_id,
        "limitUsd": float(budget_usd),
        "period": "monthly",
        "hardLimit": True,
    })
    return org_id

def ensure_team(org_id, name, models=None):
    scope = models or ["gemini-2-5-pro", "gpt-4o-mini"]
    _, created = req("POST", "/teams", {"orgId": org_id, "name": name, "displayName": name, "modelScope": scope})
    team_id = unwrap_data(created).get("id") or unwrap_data(created).get("teamId") or ""
    if not team_id:
        _, existing = req("GET", f"/teams?orgId={org_id}")
        for t in unwrap_list(existing):
            if t.get("name") == name and (not t.get("orgId") or t.get("orgId") == org_id):
                team_id = t.get("id") or t.get("teamId") or name
                break
    team_id = team_id or name
    req("PUT", f"/teams/{team_id}", {"orgId": org_id, "name": name, "displayName": name, "modelScope": scope})
    return team_id

def ensure_principal(org_id, team_id, name, kind="service", models=None, rpm=180, tpm=240000):
    api_kind = "user" if kind == "user" else "service"
    scope = models or ["gemini-2-5-pro", "gpt-4o-mini"]
    _, created = req("POST", "/principals", {
        "id": name,
        "orgId": org_id,
        "teamId": team_id,
        "name": name,
        "displayName": name,
        "kind": api_kind,
        "modelScope": scope,
    })
    prin_id = unwrap_data(created).get("id") or unwrap_data(created).get("principalId") or ""
    if not prin_id:
        _, existing = req("GET", f"/principals?orgId={org_id}")
        for p in unwrap_list(existing):
            if p.get("name") == name or p.get("id") == name:
                prin_id = p.get("id") or p.get("principalId") or name
                break
    prin_id = prin_id or name
    req("PUT", f"/principals/{prin_id}", {"teamId": team_id, "displayName": name, "kind": api_kind, "modelScope": scope})
    req("PUT", f"/principals/{prin_id}/ratelimit", {"requestsPerMinute": rpm, "tokensPerMinute": tpm})
    return prin_id

def mint_vk(prin_id, vk_name, models):
    _, existing = req_url("GET", f"{tenant_base}/virtual-keys")
    for x in unwrap_list(existing):
        if isinstance(x, dict) and x.get("id") and x.get("name") == vk_name and (not x.get("principalId") or x.get("principalId") == prin_id):
            req_url("DELETE", f"{tenant_base}/virtual-keys/{x['id']}")
    st, created = req_url("POST", f"{tenant_base}/virtual-keys", {
        "principalId": prin_id,
        "name": vk_name,
        "modelScope": models,
        "allowedModels": models,
    })
    u = unwrap_data(created)
    cleartext = (
        u.get("token")
        or u.get("key")
        or u.get("cleartext_key")
        or u.get("cleartextKey")
        or u.get("api_key")
        or ""
    )
    vk_id = u.get("id") or u.get("vkId") or ""
    if not cleartext:
        st, created = req_url("POST", f"{tenant_base}/virtual-keys", {
            "principalId": prin_id,
            "name": f"{vk_name}-{int(time.time())}",
            "modelScope": models,
            "allowedModels": models,
        })
        u = unwrap_data(created)
        cleartext = u.get("token") or u.get("key") or u.get("cleartext_key") or u.get("api_key") or ""
        vk_id = vk_id or u.get("id") or u.get("vkId") or ""
    return vk_id, cleartext

specs = [
    {
        "org": "oceania-autonomous-logistics",
        "budget": 1200.0,
        "team": "maritime-fleet-routing",
        "principal": "vessel-scheduler-agent",
        "kind": "service",
        "vk_name": "vessel-scheduler-prod-vk",
        "models": ["gemini-2-5-pro", "gpt-4o-mini"],
    },
    {
        "org": "oceania-autonomous-logistics",
        "budget": 1200.0,
        "team": "maritime-fleet-routing",
        "principal": "customs-tariff-classifier",
        "kind": "service",
        "vk_name": "customs-tariff-prod-vk",
        "models": ["gpt-4o-mini", "gemini-2-5-pro"],
    },
    {
        "org": "nem-smart-grid-australia",
        "budget": 1800.0,
        "team": "grid-bidding-dispatch",
        "principal": "grid-bidding-agent",
        "kind": "service",
        "vk_name": "grid-bidding-prod-vk",
        "models": ["gemini-2-5-pro", "gpt-4o-mini"],
    },
    {
        "org": "nem-smart-grid-australia",
        "budget": 1800.0,
        "team": "grid-bidding-dispatch",
        "principal": "liam-chen-grid-director",
        "kind": "user",
        "vk_name": "chen-vertex-ops-vk",
        "models": ["gemini-2-5-pro", "gpt-4o-mini"],
    },
]

profiles = []
for s in specs:
    org_id = ensure_org(s["org"], s["budget"])
    team_id = ensure_team(org_id, s["team"])
    prin_id = ensure_principal(org_id, team_id, s["principal"], s["kind"], s["models"])
    vk_id, token_str = mint_vk(prin_id, s["vk_name"], s["models"])
    profiles.append({
        "vk_id": vk_id or f"vk-{s['principal']}",
        "vk_name": s["vk_name"],
        "token": token_str,
        "principal_id": prin_id,
        "team_id": team_id,
        "team_name": s["team"],
        "org_id": org_id,
        "models": s["models"],
    })

print(json.dumps(profiles))
PY
)"

  read -r VK1 VK2 VK3 VK4 < <(python3 -c '
import json, sys
p = json.loads(sys.argv[1])
print(*(x.get("token", "") for x in p[:4]))
' "${PROFILES_JSON}")

  echo "  Provisioned 4 DSSM Virtual Keys: ${VK1:0:14}... ${VK2:0:14}... ${VK3:0:14}... ${VK4:0:14}..."

  kubectl create secret generic bnk-aigw-virtual-keys -n default \
    --from-literal=VK_VESSEL_SCHEDULER="${VK1}" \
    --from-literal=VK_CUSTOMS_TARIFF="${VK2}" \
    --from-literal=VK_GRID_BIDDING="${VK3}" \
    --from-literal=VK_LIAM_CHEN="${VK4}" \
    --from-literal=PROFILES_JSON="${PROFILES_JSON}" \
    --from-literal=CLICKHOUSE_PASSWORD="${CH_PASS}" \
    --dry-run=client -o yaml | kubectl apply -f -
fi

# 5. Deploy Continuous Dual-Leg Traffic Generator on agent-pool
echo "[Step 6] Deploying Continuous Traffic Generator (BNK_VIP=${BNK_VIP}, AIGW_VIP=${AIGW_VIP})..."
sed \
  -e "s|10.10.1.10|${BNK_VIP}|g" \
  -e "s|10.10.1.11|${AIGW_VIP}|g" \
  "${DEMO_DIR}/traffic-generator-deployment.yaml" | kubectl apply -f -

kubectl rollout restart deploy/vertex-traffic-generator -n default >/dev/null 2>&1 || true
kubectl rollout status deploy/vertex-traffic-generator -n default --timeout=120s

# 6. Trigger BNK Forge Scan if Forge is reachable
if curl -fsS --max-time 3 "${FORGE_URL}/api/health" >/dev/null 2>&1 || curl -sS --max-time 3 "${FORGE_URL}/api/projects" >/dev/null 2>&1; then
  echo "[Step 7] Syncing & scanning project ${FORGE_PROJECT} in BNK Forge (${FORGE_URL})..."
  python3 - "${FORGE_URL}" "${FORGE_USER}" "${FORGE_PASS}" "${FORGE_PROJECT}" "${KUBECONFIG:-${HOME}/.kube/config}" <<'PY' || true
import base64, json, os, sys, urllib.request, http.cookiejar

forge_url, user, pw, proj_name, kc_path = sys.argv[1:6]
cj = http.cookiejar.CookieJar()
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))
token = ""

def api(method, path, body=None):
    data = json.dumps(body).encode("utf-8") if body is not None else None
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(f"{forge_url}{path}", data=data, headers=headers, method=method)
    with opener.open(req, timeout=60) as r:
        raw = r.read().decode("utf-8", errors="replace")
        return json.loads(raw) if raw.strip() else {}

try:
    login_resp = api("POST", "/api/auth/login", {"username": user, "password": pw})
    if isinstance(login_resp, dict):
        token = login_resp.get("access_token") or login_resp.get("token") or ""
except Exception:
    pass

projects = api("GET", "/api/projects")
if isinstance(projects, dict):
    projects = projects.get("projects") or projects.get("items") or []
proj_id = None
for p in projects:
    if p.get("name") == proj_name:
        proj_id = p.get("id")
        break
if proj_id:
    clusters = api("GET", f"/api/projects/{proj_id}/k8s/clusters")
    if isinstance(clusters, dict):
        clusters = clusters.get("clusters") or clusters.get("items") or []
    cid = clusters[0].get("id") if clusters else None
    if cid and os.path.isfile(kc_path):
        with open(kc_path, "rb") as f:
            kc_b64 = base64.b64encode(f.read()).decode("ascii")
        try:
            api("PUT", f"/api/k8s/clusters/{cid}", {"kubeconfig": kc_b64})
        except Exception:
            pass
    try:
        if cid:
            api("POST", f"/api/k8s/clusters/{cid}/scan", {})
            print(f"  Forge scan complete for project id={proj_id} cluster id={cid} ({proj_name})")
        else:
            api("POST", f"/api/projects/{proj_id}/scan", {})
            print(f"  Forge scan complete for project id={proj_id} ({proj_name})")
    except Exception as e:
        print(f"  Forge scan note: {e}")
PY
fi

if declare -F bnk_ensure_toda_fluentd_stdout >/dev/null 2>&1; then
  bnk_ensure_toda_fluentd_stdout "f5-cne-core"
fi

echo "================================================================="
echo " BNK 2.4.1 LLM Gateway & MCP Stack Setup Complete!"
echo "================================================================="
