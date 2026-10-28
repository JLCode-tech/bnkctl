#!/bin/bash
# Guided walk-through of the BNK + AgentCore governance demo.
#
# Drives both live paths and narrates what each step proves, so the demo can be
# run by someone who did not build it.
#
#   AWS_PROFILE=<profile> ./scripts/demo.sh            # full run
#   AWS_PROFILE=<profile> ./scripts/demo.sh --quick    # skip agent invokes (~2 min)
#   AWS_PROFILE=<profile> ./scripts/demo.sh --check    # preflight only
#
# The BNK VIP is private, so stranger-path calls are driven on the jumphost over
# SSM. Agent invokes run locally against the AgentCore control plane.
#
# Nothing here mutates cluster config — it is read-and-exercise only.
#
# Seven acts. Act 6 exercises the L4 firewall for real, provided
# scripts/setup-stranger.sh has been run: that builds a caller with one NIC
# inside 10.0.0.0/16 and one on a secondary VPC CIDR outside it, so the accept
# and reject branches can be compared from the same host with the same SG.
# Without it, act 6 degrades to asserting the policy is programmed and says so.

set -uo pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "$DEMO_DIR/../.." && pwd)"

CLUSTER="${CLUSTER:-bnk-agentcore-demo}"
GW="${GW:-bnk-agentcore-demo-gateway}"   # fixed in gateway-deployment.yaml, independent of the cluster name

# `up` writes the kubeconfig to .awsbnkctl/<cluster>/kubeconfig RELATIVE TO THE
# DIRECTORY IT RAN FROM — in practice the repo root, because that is where
# state.env lives. A stale copy can linger under the example directory pointing
# at a cluster that no longer exists, and every kubectl call then fails in
# confusing ways instead of saying so. Prefer the repo root, fall back to the
# example dir, and verify the thing actually answers (see preflight).
if [ -z "${KUBECONFIG:-}" ]; then
  for _kc in "$REPO_ROOT/.awsbnkctl/$CLUSTER/kubeconfig" \
             "$DEMO_DIR/.awsbnkctl/$CLUSTER/kubeconfig"; do
    if [ -f "$_kc" ]; then KUBECONFIG="$_kc"; break; fi
  done
fi
export KUBECONFIG="${KUBECONFIG:-}"
REGION="${REGION:-ap-southeast-2}"
VIP="${VIP:-10.0.10.150}"
INGRESS_HOST="${INGRESS_HOST:-bnk-ingress.bnk-demo.internal}"
RUNTIME="${RUNTIME:-FinanceAgentV2Agent}"
TARGET="${TARGET:-demo-v2}"
FORGE="${FORGE:-http://localhost:8000}"

# Demo tokens, matching mcp-tool/kustomization.yaml.
AGENT_TOKEN="${AGENT_TOKEN:-demo-agent-token-a7f3c1}"
EXTERNAL_TOKEN="${EXTERNAL_TOKEN:-demo-external-token-4b9e2d}"

QUICK=0; CHECK_ONLY=0
for arg in "$@"; do
  case "$arg" in
    --quick) QUICK=1 ;;
    --check) CHECK_ONLY=1 ;;
    -h|--help) sed -n '2,16p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown flag: $arg" >&2; exit 2 ;;
  esac
done

if [ -t 1 ]; then
  B=$'\033[1m'; DIM=$'\033[2m'; GRN=$'\033[32m'; RED=$'\033[31m'; YEL=$'\033[33m'; R=$'\033[0m'
else
  B=""; DIM=""; GRN=""; RED=""; YEL=""; R=""
fi

FAILED=0
say()  { printf '\n%s▏%s%s\n' "$B" "$1" "$R"; }
note() { printf '  %s%s%s\n' "$DIM" "$1" "$R"; }
ok()   { printf '  %s✓%s %s\n' "$GRN" "$R" "$1"; }
bad()  { printf '  %s✗%s %s\n' "$RED" "$R" "$1"; FAILED=$((FAILED+1)); }
warn() { printf '  %s!%s %s\n' "$YEL" "$R" "$1"; }

pause() { [ -t 0 ] && { printf '\n  %s[enter to continue]%s' "$DIM" "$R"; read -r _; } || true; }

need() { command -v "$1" >/dev/null 2>&1 || { echo "required tool missing: $1" >&2; exit 2; }; }

# ── preflight ────────────────────────────────────────────────────────────────
say "Preflight"
if [ -z "$KUBECONFIG" ] || [ ! -f "$KUBECONFIG" ]; then
  bad "no kubeconfig found for cluster $CLUSTER"
  note "looked in $REPO_ROOT/.awsbnkctl/$CLUSTER/ and $DEMO_DIR/.awsbnkctl/$CLUSTER/"
  note "has 'awsbnkctl up' been run? it writes the kubeconfig beside state.env."
  echo; echo "  ${RED}preflight failed${R}"; exit 1
fi
note "kubeconfig: ${KUBECONFIG#$REPO_ROOT/}"
if ! kubectl get --raw=/readyz >/dev/null 2>&1; then
  bad "kubeconfig exists but the cluster does not answer"
  note "this is what a STALE kubeconfig looks like — it may point at a cluster"
  note "that was torn down. Delete it and re-run 'awsbnkctl up'."
  echo; echo "  ${RED}preflight failed${R}"; exit 1
fi
need aws; need kubectl; need python3
[ -n "${AWS_PROFILE:-}" ] || warn "AWS_PROFILE is unset; relying on ambient credentials"

if ! kubectl get gateway "$GW" -n default >/dev/null 2>&1; then
  bad "cannot reach the cluster, or the Gateway is missing. Is SSO current?"
  echo; echo "  try: aws sso login --profile \${AWS_PROFILE}"; exit 1
fi
PROGRAMMED=$(kubectl get gateway "$GW" -n default \
  -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null)
[ "$PROGRAMMED" = "True" ] && ok "BNK Gateway programmed on $VIP" || bad "Gateway not programmed"

IRULE_READY=$(kubectl get f5-big-cne-irules mcp-rate-limit-irule -n default \
  -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null)
[ "$IRULE_READY" = "True" ] && ok "governance iRule programmed on TMM" || bad "iRule not programmed"

POLICIES=$(kubectl get netpolicies.gateway.k8s.f5.com -n default --no-headers 2>/dev/null | wc -l | tr -d ' ')
[ "$POLICIES" -ge 2 ] && ok "$POLICIES listener-scoped NetPolicies attached" \
  || bad "expected 2 NetPolicies (one per listener), found $POLICIES"

AIGW_COUNT=$(kubectl get aigatewayprofiles.inference.k8s.f5.com,apppolicies.gateway.k8s.f5.com -n f5-cne-system --no-headers 2>/dev/null | wc -l | tr -d ' ')
[ "${AIGW_COUNT:-0}" -ge 2 ] && ok "BNK 2.4.1 AI Gateway (AITenant/AICredential/AIProvider/AIModel/AIGatewayProfile/AppPolicy in f5-cne-system) present" \
  || warn "BNK 2.4.1 AI Gateway not applied yet (./scripts/setup-llm-gateway.sh)"

TOOL_READY=$(kubectl get deploy mcp-financial-tool -n default \
  -o jsonpath='{.status.readyReplicas}' 2>/dev/null)
[ "${TOOL_READY:-0}" -ge 1 ] && ok "MCP tool pod ready" || bad "MCP tool pod not ready"

OBS=$(kubectl get pods -n llm-egress --no-headers 2>/dev/null | grep -c Running)
[ "${OBS:-0}" -ge 3 ] && ok "observability stack up ($OBS pods: loki, collectors, shipper)" \
  || warn "observability stack incomplete ($OBS pods running) — Forge panels may be empty"

JUMPHOST=$(aws ec2 describe-instances --region "$REGION" \
  --filters "Name=tag:Name,Values=$CLUSTER-jumphost" "Name=instance-state-name,Values=running" \
  --query 'Reservations[].Instances[].InstanceId' --output text 2>/dev/null | awk '{print $1}')
[ -n "$JUMPHOST" ] && ok "jumphost $JUMPHOST (drives the stranger path)" \
  || bad "no running jumphost found — stranger-path steps cannot run"

# The purpose-built stranger: its own SG, subnet-public-2, plus a second NIC on
# a secondary VPC CIDR that is deliberately outside the firewall's accept list.
# Optional — provision with scripts/setup-stranger.sh. Act 6 degrades to a
# policy-only assertion when it is absent.
STRANGER=$(aws ec2 describe-instances --region "$REGION" \
  --filters "Name=tag:Name,Values=$CLUSTER-stranger" "Name=instance-state-name,Values=running" \
  --query 'Reservations[].Instances[].InstanceId' --output text 2>/dev/null | awk '{print $1}')
if [ -n "$STRANGER" ]; then
  STRANGER_IN=$(aws ec2 describe-instances --region "$REGION" --instance-ids "$STRANGER" \
    --query 'Reservations[].Instances[].NetworkInterfaces[?Attachment.DeviceIndex==`0`].PrivateIpAddress' \
    --output text 2>/dev/null | awk '{print $1}')
  STRANGER_OUT=$(aws ec2 describe-network-interfaces --region "$REGION" \
    --filters "Name=tag:Name,Values=$CLUSTER-stranger-outside-eni" \
    --query 'NetworkInterfaces[].PrivateIpAddress' --output text 2>/dev/null | awk '{print $1}')
  if [ -n "$STRANGER_IN" ] && [ -n "$STRANGER_OUT" ]; then
    ok "stranger $STRANGER ($STRANGER_IN in-range, $STRANGER_OUT out-of-range)"
  else
    warn "stranger $STRANGER found but its NICs did not resolve — act 6 will fall back"
    STRANGER=""
  fi
else
  warn "no stranger instance — act 6 cannot test the firewall reject (setup-stranger.sh)"
fi

[ "$FAILED" -gt 0 ] && { echo; echo "  ${RED}preflight failed${R} — fix the above before demoing."; exit 1; }
[ "$CHECK_ONLY" -eq 1 ] && { echo; ok "preflight clean"; exit 0; }

# ── helper: run an arbitrary shell command on the jumphost via SSM ───────────
# Echoes StandardOutputContent. The BNK VIP is private, so anything that has to
# reach it is driven from inside the VPC.
ssm_run() {
  local cmd="$1"
  local JUMPHOST="${2:-$JUMPHOST}"     # optional: run on a different instance
  local id
  id=$(aws ssm send-command --region "$REGION" --instance-ids "$JUMPHOST" \
        --document-name AWS-RunShellScript \
        --parameters "commands=[$(python3 -c 'import json,sys;print(json.dumps(sys.argv[1]))' "$cmd")]" \
        --query 'Command.CommandId' --output text 2>/dev/null)
  for _ in $(seq 1 40); do
    sleep 3
    local st
    st=$(aws ssm get-command-invocation --region "$REGION" --command-id "$id" \
          --instance-id "$JUMPHOST" --query 'Status' --output text 2>/dev/null || echo Pending)
    case "$st" in Success|Failed|Cancelled|TimedOut) break ;; esac
  done
  aws ssm get-command-invocation --region "$REGION" --command-id "$id" \
    --instance-id "$JUMPHOST" --query 'StandardOutputContent' --output text 2>/dev/null
}

# ── helper: POST to the MCP route on the jumphost, echo "<status>\n<body>" ────
ssm_curl() {
  local body="$1"; shift
  local hdrs=""
  for h in "$@"; do hdrs="$hdrs -H '$h'"; done
  ssm_run "curl -s -o /tmp/dbody -w '%{http_code}' --max-time 15 -X POST http://$VIP/v1/mcp/forecast \
-H 'Host: $INGRESS_HOST' -H 'Content-Type: application/json' -H 'Accept: application/json' $hdrs \
-d '$body'; echo; head -c 220 /tmp/dbody"
}

FORECAST_BODY='{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"forecast","arguments":{"symbol":"NVDA","days":30}}}'
BALANCE_BODY='{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_account_balance","arguments":{"account_id":"ACC-1001"}}}'

expect() { # expect <wanted-status> <label> <output>
  local want="$1" label="$2" out="$3"
  local got; got=$(printf '%s' "$out" | head -1 | tr -d '[:space:]')
  local snippet; snippet=$(printf '%s' "$out" | tail -n +2 | head -c 150)
  if [ "$got" = "$want" ]; then ok "$label → $got"; else bad "$label → got $got, wanted $want"; fi
  [ -n "$snippet" ] && note "$snippet"
}

# ── act 1: the agent's own path ──────────────────────────────────────────────
say "Act 1 — the trusted agent path"
note "The AgentCore runtime calls its MCP tool through the BNK VIP. Bedrock is"
note "hit twice: once to decide it needs the tool, once to narrate the answer."
if [ "$QUICK" -eq 1 ]; then
  warn "--quick: skipping agent invokes"
else
  ( cd "$DEMO_DIR/agent" && ./node_modules/.bin/agentcore invoke \
      --runtime "$RUNTIME" --target "$TARGET" --prompt "forecast AMZN" 2>&1 ) \
    | tr '\r' '\n' | grep -iE "expected growth" | head -2 | sed 's/^/  /' \
    && ok "agent got a forecast — the whole path works" \
    || bad "agent invoke produced no forecast"
  note "That percentage was generated by the pod. Seeing it proves the tool ran."
fi
pause

# ── act 2: the stranger, and what stops it ───────────────────────────────────
say "Act 2 — the stranger path: same tool, no AWS involved"
note "This caller never touched AWS. No JWT check, no Cedar policy, no guardrail,"
note "because none of those components are in this path. BNK is the only checkpoint."
expect 401 "no credential at all       " "$(ssm_curl "$FORECAST_BODY")"
expect 401 "wrong bearer token         " "$(ssm_curl "$FORECAST_BODY" "Authorization: Bearer not-a-real-token")"
expect 200 "valid token, benign tool   " "$(ssm_curl "$FORECAST_BODY" "Authorization: Bearer $EXTERNAL_TOKEN")"
pause

say "Act 3 — authorization: the sensitive tool"
note "get_account_balance is privileged. BNK decides, in the data path, before the"
note "request reaches the pod."
expect 403 "external caller → BALANCE  " "$(ssm_curl "$BALANCE_BODY" "Authorization: Bearer $EXTERNAL_TOKEN")"
expect 200 "agent's token  → BALANCE  " "$(ssm_curl "$BALANCE_BODY" "Authorization: Bearer $AGENT_TOKEN")"
note "Same route, same tool server. The network made the distinction."
pause

# ── act 4: the throttle ──────────────────────────────────────────────────────
say "Act 4 — rate limiting: stopping a runaway caller"
note "10 requests per 60s per caller identity. Watch where it flips to 429."
note "Acts 2-3 already spent part of this caller's budget — the bucket is shared"
note "across everything that identity does, which is the point of keying on it."
LOOP_CMD="for i in \$(seq 1 13); do printf '%s ' \"\$(curl -s -o /dev/null -w '%{http_code}' --max-time 12 \
-X POST http://$VIP/v1/mcp/forecast -H 'Host: $INGRESS_HOST' -H 'Content-Type: application/json' \
-H 'Accept: application/json' -H 'Authorization: Bearer $EXTERNAL_TOKEN' -d '$FORECAST_BODY')\"; done; echo"
LOOP_ID=$(aws ssm send-command --region "$REGION" --instance-ids "$JUMPHOST" \
  --document-name AWS-RunShellScript \
  --parameters "commands=[$(python3 -c 'import json,sys;print(json.dumps(sys.argv[1]))' "$LOOP_CMD")]" \
  --query 'Command.CommandId' --output text 2>/dev/null)
for _ in $(seq 1 40); do
  sleep 3
  ST=$(aws ssm get-command-invocation --region "$REGION" --command-id "$LOOP_ID" \
        --instance-id "$JUMPHOST" --query 'Status' --output text 2>/dev/null || echo Pending)
  case "$ST" in Success|Failed|Cancelled|TimedOut) break ;; esac
done
SEQ=$(aws ssm get-command-invocation --region "$REGION" --command-id "$LOOP_ID" \
  --instance-id "$JUMPHOST" --query 'StandardOutputContent' --output text 2>/dev/null | tr -d '\n')
note "$SEQ"
if printf '%s' "$SEQ" | grep -q 429; then
  ok "the limit bit — throttled requests never reached the pod"
else
  bad "expected 429s in the sequence"
fi
note "A throttled request is never buffered or forwarded: it costs nothing downstream."
pause

# ── act 5: TLS on the MCP hop ────────────────────────────────────────────────
say "Act 5 — TLS termination on the MCP hop"
note "BNK holds the certificate and terminates :443 itself — no load balancer in"
note "the path. The CA comes from the cluster, so --cacert is a real validation."
# The CA travels as one base64 line: SSM flattens the command, so a heredoc
# would lose its line structure and the here-document never terminates.
CA_B64=$(kubectl get secret mcp-tls -o jsonpath='{.data.ca\.crt}' 2>/dev/null)
if [ -z "$CA_B64" ]; then
  bad "could not read ca.crt from the mcp-tls Secret"
else
TLS_CMD="umask 077; echo '$CA_B64' | base64 -d > /tmp/bnkca.crt; \
curl -s -o /tmp/tbody -w '%{http_code}' --max-time 15 --cacert /tmp/bnkca.crt \
--resolve $INGRESS_HOST:443:$VIP -X POST https://$INGRESS_HOST/v1/mcp/forecast \
-H 'Content-Type: application/json' -H 'Accept: application/json' \
-H 'Authorization: Bearer $AGENT_TOKEN' -d '$FORECAST_BODY'; echo; \
echo | openssl s_client -connect $VIP:443 -servername $INGRESS_HOST 2>/dev/null \
| openssl x509 -noout -subject -issuer 2>/dev/null; rm -f /tmp/bnkca.crt"
  TLS_OUT=$(ssm_run "$TLS_CMD")
  TLS_CODE=$(printf '%s\n' "$TLS_OUT" | head -1 | tr -d '[:space:]')
  if [ "$TLS_CODE" = "200" ]; then
    ok "chain validated against the cluster CA → 200"
  else
    bad "TLS :443 → got '$TLS_CODE', wanted 200"
  fi
  while IFS= read -r l; do
    [ -n "$l" ] && note "$l"
  done <<EOT
$(printf '%s\n' "$TLS_OUT" | grep -E '^(subject|issuer)=')
EOT
  if printf '%s\n' "$TLS_OUT" | grep -qE "^subject=CN *= *$INGRESS_HOST"; then
    ok "certificate subject matches the ingress hostname"
  else
    bad "certificate subject does not match $INGRESS_HOST"
  fi
fi
pause

# ── act 6: what is deliberately open, and what the firewall says ─────────────
say "Act 6 — discovery stays open; the L4 firewall is programmed"
note "Agent-card discovery is unauthenticated by design — a client must be able to"
note "learn what the server offers before it holds a credential."
CARD=$(ssm_run "curl -s -o /tmp/cbody -w '%{http_code}' --max-time 15 \
http://$VIP/.well-known/agent-card.json -H 'Host: $INGRESS_HOST'; echo; head -c 120 /tmp/cbody")
expect 200 "agent-card, no credential  " "$CARD"

note ""
note "The L4 firewall accepts 10.0.0.0/16 and rejects everything else."
FW_RULES=$(kubectl get fwpol mcp-firewall \
  -o jsonpath='{range .spec.rule[*]}{.name}: {.action} from {.source.addresses}{"\n"}{end}' 2>/dev/null)
if printf '%s' "$FW_RULES" | grep -q 'accept from .*10\.0\.0\.0/16' \
   && printf '%s' "$FW_RULES" | grep -qE 'reject from (\[\]|$)'; then
  printf '%s\n' "$FW_RULES" | while read -r l; do note "$l"; done
  ok "firewall policy programmed on TMM (accept VPC, reject all else)"
else
  bad "firewall policy is not the expected accept-VPC / reject-all shape"
fi

if [ -n "$STRANGER" ]; then
  note ""
  note "Now the data path, from a caller that is genuinely foreign: its own SG,"
  note "its own subnet in another AZ, admitted by ONE rule (tcp/443). It has two"
  note "NICs — one inside 10.0.0.0/16, one outside it on a secondary VPC CIDR."
  note "Same host, same SG, same port. Only the source range differs, so"
  note "anything that differs in the result is the firewall and nothing else."
  # Written to a file via base64: SSM flattens a multi-line command, so an
  # inline script loses its line structure.
  FW_SCRIPT=$(cat <<EOSCRIPT
#!/bin/bash
OUT=$STRANGER_OUT; VIP=$VIP; HOST=$INGRESS_HOST; TOK=$EXTERNAL_TOKEN
DEV=\$(ip -o -4 addr show | awk -v ip="\$OUT/" '\$4 ~ ip {print \$2}')
OUTGW=\$(echo "\$OUT" | awk -F. '{print \$1"."\$2"."\$3".1"}')
# source-based routing, so packets from the out-of-range NIC leave via it
ip route replace default via "\$OUTGW" dev "\$DEV" table 200 >/dev/null 2>&1
ip rule add from "\$OUT" table 200 >/dev/null 2>&1
ip route flush cache >/dev/null 2>&1
for SRC in $STRANGER_IN \$OUT; do
  C=\$(curl -sk -o /dev/null -w '%{http_code}' --max-time 12 --interface "\$SRC" \
    "https://\$VIP/v1/mcp/forecast" -H "Host: \$HOST" -H 'Content-Type: application/json' \
    -H 'Accept: application/json' -H "Authorization: Bearer \$TOK" \
    -d '$FORECAST_BODY' 2>/dev/null); R=\$?
  echo "\$SRC http=\$C rc=\$R"
done
EOSCRIPT
)
  FW_B64=$(printf '%s' "$FW_SCRIPT" | base64 | tr -d '\n')
  FW_OUT=$(ssm_run "echo '$FW_B64' | base64 -d > /tmp/fw.sh; sudo bash /tmp/fw.sh; rm -f /tmp/fw.sh" "$STRANGER")
  IN_LINE=$(printf '%s\n' "$FW_OUT" | grep "^$STRANGER_IN " | head -1)
  OUT_LINE=$(printf '%s\n' "$FW_OUT" | grep "^$STRANGER_OUT " | head -1)
  note "$IN_LINE"
  note "$OUT_LINE"
  if printf '%s' "$IN_LINE" | grep -q 'http=200'; then
    ok "in-range source $STRANGER_IN accepted → 200"
  else
    bad "in-range source $STRANGER_IN should have been accepted"
  fi
  # rc=7 is a TCP reset: the firewall actively refused. rc=28 would be a
  # timeout, which cannot be distinguished from a missing route, so only 7
  # counts as proof.
  if printf '%s' "$OUT_LINE" | grep -q 'rc=7'; then
    ok "out-of-range source $STRANGER_OUT REJECTED before TCP completed (RST)"
    note "rc=7 is a reset, not a timeout — the firewall refused it, rather than"
    note "the packet being lost. That distinction is the whole proof."
  elif printf '%s' "$OUT_LINE" | grep -q 'http=200'; then
    bad "out-of-range source $STRANGER_OUT was ALLOWED — the firewall is not enforcing"
  else
    warn "out-of-range source did not connect, but with a timeout rather than a"
    warn "reset. That is consistent with the firewall, but also with a routing"
    warn "problem, so it does not prove enforcement on its own."
  fi
else
  warn "No stranger instance, so the reject branch is NOT exercised: every source"
  warn "that can route to this private VIP is inside the accepted 10.0.0.0/16."
  warn "Run scripts/setup-stranger.sh to make this a real test."
fi
pause

# ── act 7: the evidence ──────────────────────────────────────────────────────
say "Act 7 — the evidence, in Forge"
CLUSTER_ID=""
TOKEN=$(curl -s -X POST "$FORGE/api/auth/login" -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin123"}' --max-time 8 2>/dev/null \
  | python3 -c 'import sys,json;print(json.load(sys.stdin).get("token",""))' 2>/dev/null)
if [ -n "$TOKEN" ]; then
  CLUSTER_ID=$(curl -s -H "Authorization: Bearer $TOKEN" "$FORGE/api/k8s/clusters" --max-time 8 2>/dev/null \
    | python3 -c "import sys,json;print(next((c['id'] for c in json.load(sys.stdin) if c['name']=='$CLUSTER'),''))" 2>/dev/null)
fi
if [ -n "$CLUSTER_ID" ]; then
  curl -s -H "Authorization: Bearer $TOKEN" \
    "$FORGE/api/k8s/clusters/$CLUSTER_ID/llm-observability/rankings" --max-time 15 2>/dev/null \
    | python3 -c '
import sys,json
d=json.load(sys.stdin)
print(f"  available: {d.get(\"available\")}")
for r in d.get("rows",[]):
    print(f"  {r[\"model\"]:<38} req={r[\"requests\"]:<4} tokens={r[\"tokens\"]:<7} cost={round(r.get(\"cost\") or 0,4)}")
' 2>/dev/null || warn "could not read Forge rankings"
  ok "two legs, one view"
  note "mcp:finance-tool  = BNK's view: who was allowed, who was refused, 0 tokens"
  note "claude-sonnet-4-6 = Bedrock's view: what the reasoning actually cost"
  echo
  note "Open these:"
  note "  $FORGE  →  LLM Observability  →  $CLUSTER"
  note "  $FORGE  →  Gateway Topology   →  $GW  (iRule under each listener)"
else
  warn "Forge not reachable at $FORGE — skipping. Start the local stack to see the panels."
fi

# ── wrap ─────────────────────────────────────────────────────────────────────
say "Summary"
if [ "$FAILED" -eq 0 ]; then
  ok "every expected outcome matched"
  cat <<'EOS'

  Three layers, four distinct refusals, one route:
    401  no / wrong credential            (the TOOL — not BNK; BNK logs it)
    403  privileged tool, wrong caller    (BNK, before the pod)
    429  too many requests                (BNK, before the pod)
EOS
  if [ -n "$STRANGER" ]; then
    cat <<'EOS'
    RST  source outside 10.0.0.0/16       (BNK firewall, before TCP completed)
EOS
  else
    cat <<'EOS'
    ---  source outside 10.0.0.0/16       (BNK firewall — policy asserted only,
                                           no stranger host to prove it)
EOS
  fi
  cat <<'EOS'

  Plus: TLS terminated by BNK itself on :443, chain validated against the
  cluster CA, and discovery left deliberately open.

  And the point of it: an agent is useless without its tool call, so whoever
  controls that hop controls the agent's reach. That hop is where BNK sits.
EOS
else
  bad "$FAILED step(s) did not match — see above"
fi
exit $((FAILED > 0 ? 1 : 0))
