#!/bin/bash
set -uo pipefail

# Rebuilds the whole AgentCore + BNK demo from nothing, in the right order,
# stopping at the first thing that actually fails.
#
#   AWS_PROFILE=<profile> ./scripts/rebuild.sh
#   AWS_PROFILE=<profile> ./scripts/rebuild.sh --skip-agent   # no Bedrock spend
#
# Read this if you are picking the work up cold. It encodes an order that was
# arrived at by getting it wrong, and the ordering is the whole value:
#
#   * `up` MUST run from the repo root. State lives in
#     <repo>/.awsbnkctl/<cluster>/state.env, and `down` later reads it from
#     there. Run it from the example directory and you get a second, empty
#     state dir and a `down` that falls back to tag discovery.
#   * mcp-tool is applied as a DIRECTORY, not deployment.yaml. Kustomize
#     generates the ConfigMap *and* the bearer-token Secret; without the Secret
#     the pod exits 1 by design.
#   * the Gateway must exist before setup-agentcore-network.sh, which reads the
#     VIP off the live Gateway object.
#   * the Gateway must also exist before mcp-security-policy.yaml, whose
#     NetPolicies attach to named listeners.
#   * the HTTPS listener needs the mcp-tls Secret, which cert-manager issues
#     from the in-cluster CA that phase 12 of `up` creates. That is a wait, not
#     an instant, so this script waits for it.
#   * the token shipper needs the account ID substituted. The tracked file
#     deliberately keeps a <account-id> placeholder because this repo is
#     PUBLIC. We substitute into a temp copy and never write it back.
#
# Idempotent-ish: safe to re-run. Steps that already exist are left alone. It
# does NOT tear down first — run the teardown sequence for that.

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "$DEMO_DIR/../.." && pwd)"
# Cluster name and region come from the intent file unless overridden.
CFG="${CFG:-$DEMO_DIR/cluster.yaml}"
CLUSTER="${CLUSTER:-$(awk '/^  name:/{print $2; exit}' "$CFG")}"
REGION="${REGION:-$(awk '/^  region:/{print $2; exit}' "$CFG")}"
: "${CLUSTER:?could not read metadata.name from $CFG}"; : "${REGION:?could not read metadata.region from $CFG}"
CONFIG="examples/agentcore-demo/cluster.yaml"
KC="$REPO_ROOT/.awsbnkctl/$CLUSTER/kubeconfig"
# The Gateway name is fixed in gateway-deployment.yaml (the NetPolicies reference
# it), so it does not follow the cluster name.
GW="${GW:-bnk-agentcore-demo-gateway}"

SKIP_AGENT=0
for a in "$@"; do
  case "$a" in
    --skip-agent) SKIP_AGENT=1 ;;
    -h|--help) sed -n '3,32p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown flag: $a" >&2; exit 2 ;;
  esac
done

B=$'\033[1m'; GRN=$'\033[32m'; RED=$'\033[31m'; YEL=$'\033[33m'; DIM=$'\033[2m'; R=$'\033[0m'
STEP=0
step() { STEP=$((STEP+1)); printf '\n%s══ %d. %s%s\n' "$B" "$STEP" "$1" "$R"; }
ok()   { printf '   %s✓%s %s\n' "$GRN" "$R" "$1"; }
note() { printf '   %s%s%s\n' "$DIM" "$1" "$R"; }
warn() { printf '   %s!%s %s\n' "$YEL" "$R" "$1"; }
die()  { printf '\n   %s✗ %s%s\n\n' "$RED" "$1" "$R"; exit 1; }

# ── 0. preflight ─────────────────────────────────────────────────────────────
step "Preflight"
[ -n "${AWS_PROFILE:-}" ] || die "set AWS_PROFILE first"
for t in aws kubectl python3 node; do
  command -v "$t" >/dev/null 2>&1 || die "missing required tool: $t"
done
[ -x "$REPO_ROOT/awsbnkctl" ] || die "no awsbnkctl binary at $REPO_ROOT/awsbnkctl (go build ./cmd/awsbnkctl)"
ACCT=$(aws sts get-caller-identity --query Account --output text 2>/dev/null) \
  || die "no valid AWS credentials — run: aws sso login --profile $AWS_PROFILE"
ok "account $ACCT, profile $AWS_PROFILE"

# SSO note, corrected. `expiresAt` in ~/.aws/sso/cache is the ACCESS token and
# lives only ~1 hour — but a refreshToken is cached alongside it and both the
# CLI and the Go SDK refresh silently, so the effective session is the IAM
# Identity Center session duration (~11 h here), not one hour. An earlier
# version of this script refused to start with under 60 minutes left on the
# access token, which meant it refused almost always. Removed.
#
# The REAL hazard is refresh-token rotation. Each refresh returns a new refresh
# token and invalidates the previous one, so two processes refreshing the same
# profile concurrently will burn each other: one wins, the other dies with
# InvalidGrantException, and it stays dead until the next `aws sso login`. That
# is the most likely cause of the mid-run failure we saw, which killed `down`
# thirteen phases in and left half-built billing infrastructure.
#
# So: DO NOT run other AWS commands against this profile while this script is
# running. Poll by reading its output, not by hitting the API in a loop.
if [ -n "${EXP:-}" ]; then
  note "SSO access token expires $EXP (auto-refreshes; session is much longer)"
fi
warn "Do not run other AWS CLI commands on this profile until this finishes —"
warn "concurrent SSO refresh rotates the token and kills whichever loses."

for f in cne_pull_64.json license.jwt; do
  [ -s "$DEMO_DIR/$f" ] || die "missing or empty $DEMO_DIR/$f (gitignored; must be present)"
done
ok "pull secret + license present"

# ── 1. cluster ───────────────────────────────────────────────────────────────
step "awsbnkctl up  (EKS + BNK — the long one, ~30-45 min)"
note "running from $REPO_ROOT so state.env lands where 'down' will look for it"
cd "$REPO_ROOT" || die "cannot cd to repo root"
if [ -f "$KC" ] && kubectl --kubeconfig "$KC" get --raw=/readyz >/dev/null 2>&1; then
  ok "cluster already up and answering — skipping"
else
  ./awsbnkctl up --config "$CONFIG" || die "up failed — read the phase output above"
  ok "up complete"
fi
[ -f "$KC" ] || die "up finished but no kubeconfig at $KC"
export KUBECONFIG="$KC"
kubectl get --raw=/readyz >/dev/null 2>&1 || die "cluster does not answer via $KC"
ok "kubeconfig: ${KC#$REPO_ROOT/}"

# ── 2. the MCP tool ──────────────────────────────────────────────────────────
step "MCP tool  (as a DIRECTORY — kustomize generates the token Secret)"
./awsbnkctl k apply -f examples/agentcore-demo/mcp-tool/ || die "mcp-tool apply failed"
kubectl rollout status deploy/mcp-financial-tool -n default --timeout=180s \
  || die "mcp-financial-tool never became ready (check: kubectl logs deploy/mcp-financial-tool)"
ok "tool pod ready"

# ── 3. gateway + routes + cert ───────────────────────────────────────────────
step "Gateway, HTTPRoutes and the TLS certificate"
./awsbnkctl k apply --config "$CONFIG" -f examples/agentcore-demo/gateway-deployment.yaml \
  || die "gateway apply failed"
note "waiting for cert-manager to issue mcp-tls from the in-cluster CA..."
# NB: do not name this R — R is the ANSI reset used by the helpers above.
CERT_READY=""
for i in $(seq 1 60); do
  CERT_READY=$(kubectl get certificate mcp-tls -n default \
      -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)
  [ "$CERT_READY" = "True" ] && break
  sleep 5
done
[ "$CERT_READY" = "True" ] || die "mcp-tls certificate never went Ready — the :443 listener will not bind"
ok "mcp-tls issued"
for i in $(seq 1 60); do
  P=$(kubectl get gateway "$GW" -n default \
      -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null)
  [ "$P" = "True" ] && break
  sleep 5
done
[ "$P" = "True" ] || die "Gateway never reported Programmed=True"
ok "Gateway programmed on $(kubectl get gateway "$GW" -n default -o jsonpath='{.spec.addresses[0].value}')"

# ── 4. network seam ──────────────────────────────────────────────────────────
step "AWS network seam  (SGs, SG-to-SG, private Route 53 zone)"
note "reads the VIP off the live Gateway, which is why it runs after step 3"
"$DEMO_DIR/scripts/setup-agentcore-network.sh" "$CLUSTER" || die "setup-agentcore-network.sh failed"
ok "network seam in place"

# ── 5. session persistence + governance policy ──────────────────────────────
step "MCP session persistence  (passphrase Secret, F5BigPersistenceProfile MODEL_CONTEXT_PROTOCOL)"
kubectl apply -f "$DEMO_DIR/mcp-persistence.yaml" || die "persistence apply failed"
for i in $(seq 1 40); do
  PP=$(kubectl get f5-big-persistence-profiles mcp-session -n default \
       -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null)
  [ "$PP" = "True" ] && break
  sleep 5
done
[ "$PP" = "True" ] && ok "persistence profile programmed" \
  || warn "persistence profile not Programmed yet — it may settle; forge scan will confirm"

step "Governance policy & BNK 2.4.1 AI Gateway  (iRule, NetPolicies, 6-CRD inference.k8s.f5.com/v1alpha1 chain, AppPolicy)"
kubectl apply -f "$DEMO_DIR/mcp-security-policy.yaml" || die "policy apply failed"
kubectl apply -f "$DEMO_DIR/aigateway-241.yaml" || die "aigateway-241.yaml apply failed"
for i in $(seq 1 40); do
  IR=$(kubectl get f5-big-cne-irules mcp-rate-limit-irule -n default \
       -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null)
  [ "$IR" = "True" ] && break
  sleep 5
done
[ "$IR" = "True" ] || warn "iRule not Programmed yet — it may settle; demo.sh will confirm"
POL=$(kubectl get netpolicies.gateway.k8s.f5.com -n default --no-headers 2>/dev/null | wc -l | tr -d ' ')
[ "${POL:-0}" -ge 2 ] && ok "iRule + $POL NetPolicies + BNK 2.4.1 AI Gateway CRDs attached" \
  || warn "expected 2 NetPolicies, found ${POL:-0}"

# ── 6. observability ─────────────────────────────────────────────────────────
step "Observability  (Loki + Fluent Bit, ns llm-egress)"
kubectl apply -f "$DEMO_DIR/mcp-observability.yaml" || die "observability apply failed"
kubectl rollout status deploy/loki -n llm-egress --timeout=180s >/dev/null 2>&1 \
  || warn "loki slow to start — check later"
ok "loki + collectors applied"

# ── 7. bedrock token shipper & continuous traffic generator ─────────────────
step "Bedrock token shipper & continuous traffic generator"
LG=$(aws bedrock get-model-invocation-logging-configuration \
      --query 'loggingConfig.cloudWatchConfig.logGroupName' --output text 2>/dev/null)
if [ -z "$LG" ] || [ "$LG" = "None" ]; then
  warn "Bedrock model-invocation logging is NOT configured — the shipper will find nothing."
  warn "See design doc section 9 step 7 to recreate it (needs \${ACCT} not \$ACCT in zsh)."
else
  ok "Bedrock invocation logging already configured → $LG"
fi
# The shipper's IAM role (docs/ROADMAP.md) trusts clusters by OIDC provider.
# Add this cluster's provider and keep the statements of the other clusters
# that still exist, so several demo clusters can share the role.
SHIPPER_ROLE=BNKDemoBedrockTokenShipper
if aws iam get-role --role-name "$SHIPPER_ROLE" >/dev/null 2>&1; then
  OIDC_HOST=$(aws eks describe-cluster --name "$CLUSTER" --region "$REGION" \
              --query 'cluster.identity.oidc.issuer' --output text | sed 's#^https://##')
  [ -n "$OIDC_HOST" ] || die "could not read the cluster OIDC issuer"
  TMPT=$(mktemp -t shipper-trust.XXXXXX.json) || die "mktemp failed"
  trap 'rm -f "$TMPT"' EXIT
  aws iam get-role --role-name "$SHIPPER_ROLE" --query 'Role.AssumeRolePolicyDocument' --output json \
    | ACCT="$ACCT" OIDC_HOST="$OIDC_HOST" \
      PROVIDERS="$(aws iam list-open-id-connect-providers --query 'OpenIDConnectProviderList[].Arn' --output text)" \
      python3 -c '
import json, os, sys
acct, host = os.environ["ACCT"], os.environ["OIDC_HOST"]
live = set(os.environ["PROVIDERS"].split())
arn = f"arn:aws:iam::{acct}:oidc-provider/{host}"
doc = json.load(sys.stdin)
keep = [s for s in doc.get("Statement", [])
        if s.get("Principal", {}).get("Federated") not in (arn, None)
        and s["Principal"]["Federated"] in live]
keep.append({"Effect": "Allow", "Principal": {"Federated": arn},
             "Action": "sts:AssumeRoleWithWebIdentity",
             "Condition": {"StringEquals": {f"{host}:aud": "sts.amazonaws.com",
                 f"{host}:sub": "system:serviceaccount:llm-egress:bedrock-token-shipper"}}})
json.dump({"Version": "2012-10-17", "Statement": keep}, sys.stdout)
' > "$TMPT" || die "could not build the trust policy for $SHIPPER_ROLE"
  aws iam update-assume-role-policy --role-name "$SHIPPER_ROLE" --policy-document "file://$TMPT" \
    || die "could not update the trust policy of $SHIPPER_ROLE"
  ok "$SHIPPER_ROLE trusts this cluster's OIDC provider ($(python3 -c 'import json,sys;print(len(json.load(open(sys.argv[1]))["Statement"]))' "$TMPT") cluster(s) in total)"
else
  warn "IAM role $SHIPPER_ROLE does not exist — create it as docs/ROADMAP.md describes; the CloudWatch leg stays off until then"
fi
# The tracked manifest keeps a <account-id> placeholder on purpose: this repo is
# public. Substitute into a temp file and apply that; never write it back.
TMP=$(mktemp -t shipper.XXXXXX.yaml) || die "mktemp failed"
trap 'rm -f "$TMP" "$TMPT"' EXIT
sed -e "s/<account-id>/$ACCT/g" -e "s/^\( *value: \)ap-southeast-2$/\1$REGION/" \
    "$DEMO_DIR/mcp-bedrock-token-shipper.yaml" > "$TMP"
grep -q "<account-id>" "$TMP" && die "account-id substitution failed"
kubectl apply -f "$TMP" || die "token shipper apply failed"
ok "shipper applied with account $ACCT (tracked file left with its placeholder)"

# Both legs drive the live VIP (Leg A hits BNK 2.4.1 AI Gateway :8443/v1/chat/completions;
# Leg B hits BNK MCP Gateway /v1/mcp/forecast), read off the live Gateway object.
VIP=$(kubectl get gateway "$GW" -n default -o jsonpath='{.spec.addresses[0].value}' 2>/dev/null)
[ -n "$VIP" ] || die "could not read the VIP off gateway $CLUSTER-gateway"
TMPG=$(mktemp -t traffic-gen.XXXXXX.yaml) || die "mktemp failed"
trap 'rm -f "$TMP" "$TMPG"' EXIT
sed -e "s/^\( *value: \)\"10\.0\.10\.150\"$/\1\"$VIP\"/" \
    "$DEMO_DIR/traffic-generator-deployment.yaml" > "$TMPG"
kubectl apply -f "$TMPG" || die "traffic generator apply failed"
ok "traffic generator deployed against live VIP $VIP (Leg A BNK 2.4.1 AI Gateway, Leg B MCP)"

# ── 8. the stranger ──────────────────────────────────────────────────────────
step "Path 3 stranger + the firewall's out-of-range source"
"$DEMO_DIR/scripts/setup-stranger.sh" "$CLUSTER" || die "setup-stranger.sh failed"
ok "stranger built"
note "waiting for it to register with SSM (act 6 drives it over SSM)..."
SID=$(aws ec2 describe-instances --region "$REGION" \
  --filters "Name=tag:Name,Values=$CLUSTER-stranger" "Name=instance-state-name,Values=running" \
  --query 'Reservations[].Instances[].InstanceId' --output text 2>/dev/null | awk '{print $1}')
for i in $(seq 1 40); do
  PS=$(aws ssm describe-instance-information --region "$REGION" \
       --filters "Key=InstanceIds,Values=$SID" \
       --query 'InstanceInformationList[0].PingStatus' --output text 2>/dev/null)
  [ "$PS" = "Online" ] && break
  sleep 15
done
[ "$PS" = "Online" ] && ok "stranger $SID online in SSM" \
  || warn "stranger not yet in SSM — act 6 will fall back to a policy-only assertion"

# ── 9. the agent ─────────────────────────────────────────────────────────────
step "AgentCore runtime"
if [ "$SKIP_AGENT" -eq 1 ]; then
  warn "--skip-agent: not deploying. Path 1 will be untested."
else
  note "the ECR repo was deleted with the teardown, so this rebuilds and pushes"
  note "the image — slower than a warm redeploy. Expect several minutes."
  ( cd "$DEMO_DIR/agent" && ./node_modules/.bin/agentcore deploy --target demo-v2 -y ) \
    || die "agentcore deploy failed"
  ok "runtime deployed"
fi

# ── 10. acceptance ───────────────────────────────────────────────────────────
step "Acceptance gate  (demo.sh — do not eyeball, it exits non-zero on mismatch)"
"$DEMO_DIR/scripts/demo.sh" --check || die "preflight failed — see above"
ok "preflight clean"
echo
if [ "$SKIP_AGENT" -eq 1 ]; then
  "$DEMO_DIR/scripts/demo.sh" --quick </dev/null || die "demo.sh reported failures"
else
  "$DEMO_DIR/scripts/demo.sh" </dev/null || die "demo.sh reported failures"
fi

echo
printf '%s══ rebuild complete%s\n' "$B" "$R"
note "TEARDOWN, when you are done, in THIS order (see design doc section 9):"
note "  0. aws cloudformation delete-stack --stack-name AgentCore-bnkagent-demo-v2"
note "     then WAIT for agentic_ai ENIs to reach zero — HOURS, not minutes:"
note "     aws ec2 describe-network-interfaces \\"
note "       --filters Name=interface-type,Values=agentic_ai --query 'length(NetworkInterfaces)'"
note "  1. scripts/teardown-stranger.sh"
note "  2. scripts/teardown-agentcore-network.sh    (deletes the agent SG)"
note "  3. ./awsbnkctl down --config $CONFIG --yes --auto   (from the repo root)"
