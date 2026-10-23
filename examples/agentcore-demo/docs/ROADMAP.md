# Roadmap and engineering notes

What the demo does not do yet, what it would take, and the lessons that cost
time. The running demo is described in [the README](../README.md); the reasoning
is in [`ARCHITECTURE.md`](ARCHITECTURE.md).

## Completing the double-checked path (Path 2)

No Amazon Bedrock AgentCore Gateway exists in this account, so the path through
one is not wired. The BNK side is ready: the port-443 listener exists because
AgentCore Gateway targets require an `https://` URL.

AWS's own lab for this topology runs:

```
AgentCore Gateway → VPC Lattice → resource gateway ENIs → internal NLB (TLS, ACM cert)
  → NGINX Ingress (HTTP) → EKS pods
```

BNK terminates TLS and rewrites the Host header itself, so the intended shape is
shorter:

```
AgentCore Gateway → resource gateway ENIs → BNK :443 (publicly trusted cert) → EKS pods
```

| Requirement | Today | Needed |
| --- | --- | --- |
| Publicly trusted TLS certificate on the target | in-cluster CA cert | cert-manager with Let's Encrypt via the Route 53 DNS-01 solver, or an exportable public ACM cert. **A domain we own is the prerequisite** |
| Inbound auth on the Gateway | none | `privateEndpoint` targets cannot use `NO_AUTH`; Cognito or another IdP with OAuth client credentials |
| DNS | private Route 53 zone to the VIP | the same name registered publicly for cert validation and resolving privately in the VPC (split horizon) |
| Gateway and target | none | managed VPC resource mode (`privateEndpoint.managedVpcResource`) via the raw API; the CLI has no VPC flags |

Two constraints bound the design: `McpServerTargetConfiguration.endpoint` must
match `https://.*`, and AWS validates the target cert against a public trust
store with no way to supply a custom CA. No BNK-side setting substitutes for a
publicly trusted certificate.

## Identity enforcement at BNK

BNK can validate tokens itself: `F5BigAccessJwtConfig` carries audience,
allowed signing algorithms, JWK references and token blacklisting, alongside
OAuth, SAML and access-policy CRDs. This matters for Path 3, where no AWS
component is present to check a token. For Paths 1 and 2 it is a placement
choice, not a gap.

## Guardrails via iRule integration

BNK supports iRule integration with external inspection services, the hook for
content filtering and prompt-attack detection. For traffic through an AgentCore
Gateway, Guardrails already feed those signals into Cedar decisions; the BNK
hook is for traffic that never passes a Gateway.

## What not to build

A JSON-RPC method allowlist or per-tool argument checks in the iRule would
duplicate what AgentCore Policy does natively with Cedar: principal from the
JWT, action from the tool, context from the arguments, default-deny, tool
filtering at list time, and temporal policies with rate limiting. Do not
reimplement it. BNK's payload visibility earns its place on Path 3, where the
question is "is my pod being abused", answered by per-source rate limits and
`F5BigDdosGlobal`, not by authorization.

References: [Policy in AgentCore](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/policy.html),
[fine-grained access control](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/gateway-fine-grained-access-control.html),
[temporal policies](https://aws.amazon.com/blogs/machine-learning/control-agent-behaviors-and-cost-beyond-a-single-action-new-capabilities-in-amazon-bedrock-agentcore/).

## Bedrock token shipper

One-time AWS setup so `mcp-bedrock-token-shipper.yaml` has logs to read. Two
roles: one Bedrock assumes to write invocation logs, one the shipper pod assumes
via IRSA to read them.

```bash
export ACCT=<account-id> REGION=ap-southeast-2
aws logs create-log-group --log-group-name /aws/bedrock/modelinvocations
aws logs put-retention-policy --log-group-name /aws/bedrock/modelinvocations --retention-in-days 7

# Role Bedrock assumes to write
cat > trust.json <<EOF
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"bedrock.amazonaws.com"},
 "Action":"sts:AssumeRole","Condition":{"StringEquals":{"aws:SourceAccount":"${ACCT}"}}}]}
EOF
cat > perm.json <<EOF
{"Version":"2012-10-17","Statement":[{"Effect":"Allow",
 "Action":["logs:CreateLogStream","logs:PutLogEvents","logs:DescribeLogStreams"],
 "Resource":["arn:aws:logs:${REGION}:${ACCT}:log-group:/aws/bedrock/modelinvocations",
             "arn:aws:logs:${REGION}:${ACCT}:log-group:/aws/bedrock/modelinvocations:*"]}]}
EOF
aws iam create-role --role-name BedrockModelInvocationLogging --assume-role-policy-document file://trust.json
aws iam put-role-policy --role-name BedrockModelInvocationLogging --policy-name WriteInvocationLogs --policy-document file://perm.json
sleep 30   # IAM propagation
aws bedrock put-model-invocation-logging-configuration --logging-config "{\"cloudWatchConfig\":{\"logGroupName\":\"/aws/bedrock/modelinvocations\",\"roleArn\":\"arn:aws:iam::${ACCT}:role/BedrockModelInvocationLogging\"},\"textDataDeliveryEnabled\":true,\"imageDataDeliveryEnabled\":false,\"embeddingDataDeliveryEnabled\":false}"

# IRSA role for the shipper pod
OIDC=$(aws eks describe-cluster --name bnk-agentcore-demo --query 'cluster.identity.oidc.issuer' --output text); HOST=${OIDC#https://}
cat > shipper-trust.json <<EOF
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Federated":"arn:aws:iam::${ACCT}:oidc-provider/${HOST}"},
 "Action":"sts:AssumeRoleWithWebIdentity","Condition":{"StringEquals":{"${HOST}:aud":"sts.amazonaws.com",
 "${HOST}:sub":"system:serviceaccount:llm-egress:bedrock-token-shipper"}}}]}
EOF
cat > shipper-perm.json <<EOF
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["logs:FilterLogEvents","logs:DescribeLogStreams","logs:GetLogEvents"],
 "Resource":["arn:aws:logs:${REGION}:${ACCT}:log-group:/aws/bedrock/modelinvocations",
             "arn:aws:logs:${REGION}:${ACCT}:log-group:/aws/bedrock/modelinvocations:*"]}]}
EOF
aws iam create-role --role-name BNKDemoBedrockTokenShipper --assume-role-policy-document file://shipper-trust.json
aws iam put-role-policy --role-name BNKDemoBedrockTokenShipper --policy-name ReadInvocationLogs --policy-document file://shipper-perm.json
```

In zsh write `${ACCT}`, not `$ACCT`: `$ACCT:log-group` triggers the `:l`
modifier and mangles the ARN. `scripts/rebuild.sh` substitutes the account ID
and region into a temporary copy of the manifest and applies that; the tracked
file keeps its placeholders because the repository is public. Cost is computed
from `PRICE_IN_PER_1K` / `PRICE_OUT_PER_1K`; check Bedrock pricing and override
them, or set both to `0`. `BODY_LIMIT` (default 4000) caps how much prompt text
is copied into Loki.

## Lessons that cost time

| Symptom | Cause |
| --- | --- |
| Rate-limit window never rolls over | `table incr` + `table lifetime` refreshes the idle timer on every hit; use `table set KEY VALUE TIMEOUT LIFETIME` once, then `-notouch` on reads |
| `HSL::send` from an iRule delivers nothing | not wired; `log local0.` works and is what the collector tails |
| Pinned pip versions fail and the pod crash-loops | read versions off a working pod (`kubectl exec deploy/mcp-financial-tool -- pip list`) instead of guessing |
| Forge shows no iRule on a Gateway-scoped policy | Forge renders attachments only when `NetPolicy` names a listener via `sectionName` |
| `up` run from the example directory | state lands in a second, empty `.awsbnkctl/` and `down` falls back to tag discovery; always run from the repository root |
| `mcp-tool/deployment.yaml` applied as a file | the pod exits 1 by design without the Kustomize-generated token Secret; apply the directory |
| Migrating from the old single-file manifest | first apply fails with a server-side-apply field conflict; re-run once with `--force`, then delete the orphaned `mcp-server-code` ConfigMap |

Reused from the Forge module catalog: the Loki deployment and the log-tailing
Fluent Bit shape. Deliberately not reused: the LiteLLM proxy (this demo calls
Bedrock directly), F5BigAnalyzer, and the catalog's "token counting" iRule,
which only stamps headers.
