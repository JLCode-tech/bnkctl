# agentcore-demo — an AI agent's tool calls, governed by BNK

| | |
| --- | --- |
| Cluster name | `bnk-agentcore-demo` (state in `.awsbnkctl/bnk-agentcore-demo/`) |
| Region / pattern | `ap-southeast-2` / `dual-interface` |
| Footprint | 3 × m6i.4xlarge, EKS, NAT, c6i.4xlarge jumphost, plus an Amazon Bedrock AgentCore runtime |
| Cost | about **US$4/hour** plus Bedrock usage |
| BNK Gateway VIP | `10.0.10.150`, DNS `bnk-ingress.bnk-demo.internal` (private Route 53 zone) |

An AI agent cannot answer "forecast NFLX" on its own. The model asks for a tool
call, the runtime makes an HTTP request to an MCP server, and only then does the
model write its answer. That tool call is the hop this demo puts F5 BNK in front
of. An Amazon Bedrock AgentCore agent, and any other caller, reaches a small MCP
tool running on EKS only through a BNK Gateway, which authenticates the caller,
rate-limits it, blocks the privileged tool for callers who may not use it, and
logs every decision into Loki for BNK Forge to show.

```
  AgentCore agent (VPC ENI) ──┐
                              ├──► BNK Gateway 10.0.10.150 ──► mcp-server pod
  any other caller ───────────┘      │ bearer token       → 401 if missing
                                     │ privileged tool    → 403 for the wrong caller
                                     │ 10 requests / 60 s → 429 on the 11th
                                     │ L4 firewall        → reset outside 10.0.0.0/16
                                     └ one JSON record per decision → Loki → Forge
```

Why this shape, what AgentCore adds, and where its own Gateway would sit are in
[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md). What is not built yet is in
[`docs/ROADMAP.md`](docs/ROADMAP.md).

## Prerequisites

- F5 credentials next to `cluster.yaml`: `cne_pull_64.json` and `license.jwt`
  (gitignored; symlink them in if you keep them elsewhere).
- An AWS profile with an SSO session that has more than an hour left; the
  rebuild script refuses to start otherwise.
- Node.js for the AgentCore CLI (`agent/node_modules/.bin/agentcore`).
- Optional: a BNK Forge instance on `localhost:8000` for the observability
  panels.

## Run it

The scripted path builds everything in the right order and waits where waiting
is needed. Run it from the repository root:

```bash
AWS_PROFILE=<profile> examples/agentcore-demo/scripts/rebuild.sh              # ~35 min, includes awsbnkctl up
AWS_PROFILE=<profile> examples/agentcore-demo/scripts/rebuild.sh --skip-agent # cluster + tool only, no Bedrock spend
```

Then show it. `demo.sh` drives the agent and the external caller, asserts every
expected status code, and prints the Forge URLs; it exits non-zero if anything
disagrees, so it doubles as a smoke test.

```bash
AWS_PROFILE=<profile> examples/agentcore-demo/scripts/demo.sh           # guided walk-through
AWS_PROFILE=<profile> examples/agentcore-demo/scripts/demo.sh --quick   # skip agent invokes, ~2 min
AWS_PROFILE=<profile> examples/agentcore-demo/scripts/demo.sh --check   # preflight only
```

### What the scripts do, step by step

If you would rather run it by hand, this is the order. Each step depends on the
one before it.

| Step | Command | Why the order matters |
| --- | --- | --- |
| 1 | `awsbnkctl up -f examples/agentcore-demo/cluster.yaml` | from the repo root, so state lands in `.awsbnkctl/bnk-agentcore-demo/` |
| 2 | `awsbnkctl k apply -f mcp-tool/` | the directory, not a file: Kustomize generates the ConfigMap and the bearer-token Secret |
| 3 | `awsbnkctl k apply --config cluster.yaml -f gateway-deployment.yaml` | the `GatewaySettings`, the Gateway with listeners on 80 and 443 and both `HTTPRoute`s; `--config` fills in the GatewayClass and CA issuer names from the cluster state |
| 4 | `scripts/setup-agentcore-network.sh` | reads the VIP off the live Gateway; creates the agent security group, SG-to-SG ingress and the private Route 53 zone |
| 5 | `awsbnkctl k apply -f mcp-persistence.yaml` | the passphrase Secret and the `MODEL_CONTEXT_PROTOCOL` persistence profile the NetPolicies reference |
| 6 | `awsbnkctl k apply -f mcp-security-policy.yaml && ./scripts/setup-llm-gateway.sh` | the rate-limit iRule, persistence, firewall, and the BNK 2.4.1 7-step `inference.k8s.f5.com/v1alpha1` AI Gateway chain (`AITenant`, `AICredential`, `AIProvider`, `AIModel`, `AIGatewayProfile`) + `AppPolicy` (`gateway.k8s.f5.com/v1alpha1`) + `F5BigDnsCache` (`net-resolver`) + `F5SPKStaticRoute` |
| 7 | `awsbnkctl k apply -f mcp-observability.yaml`, then `scripts/rebuild.sh` step 7 | Loki and the collectors in `llm-egress`; the Bedrock token shipper (`llm-egress`) and the traffic generator (`default`). Apply those two through `rebuild.sh`: it substitutes the account id into the shipper and the live VIP into the generator |
| 8 | `cd agent && npx agentcore deploy --target demo-v2` | the AgentCore runtime, VPC mode, in the private subnets |
| 9 | `scripts/setup-stranger.sh` (optional) | an EC2 caller with one NIC inside `10.0.0.0/16` and one outside, so the firewall's reject branch can be shown |

## What you should see

Two callers hold different bearer tokens. Both live in
`mcp-tool/kustomization.yaml` as clearly marked demo values.

| Caller | Token | `forecast` | `get_account_balance` |
| --- | --- | --- | --- |
| AgentCore runtime | `MCP_AGENT_TOKEN` | 200 | 200 |
| External caller | `MCP_EXTERNAL_TOKEN` | 200 | **403** from BNK, never reaches the pod |
| No token | — | **401** from the tool | 401 |
| Anyone, 11th request in 60 s | any | **429** with `Retry-After: 60` | 429 |
| Source outside `10.0.0.0/16` | any | TCP **reset** by the L4 firewall | reset |

The agent, from your machine:

```bash
cd examples/agentcore-demo/agent
AWS_PROFILE=<profile> ./node_modules/.bin/agentcore invoke \
  --runtime FinanceAgentV2Agent --target demo-v2 --prompt "forecast AMZN"
# → a forecast table whose growth figure was generated by the MCP pod
AWS_PROFILE=<profile> ./node_modules/.bin/agentcore invoke \
  --runtime FinanceAgentV2Agent --target demo-v2 --prompt "get the account balance for ACC-1001"
# → "The current balance for account ACC-1001 is $93,505.55"
```

The external caller, from the jumphost (the VIP is private; `demo.sh` drives
this over SSM for you; `setup-agentcore-network.sh` attaches the SSM policy to
the jumphost role for the demo and the teardown script removes it, since
awsbnkctl itself reaches the jumphost over EICE):

```bash
python3 external-agent.py --prompt "forecast NVDA"                                  # exit 0, allowed
python3 external-agent.py --tool get_account_balance --account ACC-1001             # exit 1, 403
python3 external-agent.py --tool get_account_balance --account ACC-1001 --token demo-agent-token-a7f3c1   # exit 0
python3 external-agent.py --prompt "forecast NVDA" --token ""                       # exit 1, 401
for i in $(seq 1 12); do curl -s -o /dev/null -w '%{http_code} ' -X POST http://10.0.10.150/v1/mcp/forecast \
  -H 'Host: bnk-ingress.bnk-demo.internal' -H 'Authorization: Bearer demo-external-token-4b9e2d' \
  -H 'Content-Type: application/json' -H 'Accept: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"forecast","arguments":{"symbol":"NVDA"}}}'; done
# → 200 ×10 then 429 429
```

The decisions, in Forge: **Gateway Topology** shows the iRule under each
listener and the firewall under Security Policies; **Multi-Cluster AI Gateway**
shows the BNK 2.4.1 `inference.k8s.f5.com/v1alpha1` dependency chain;
**LLM Observability** for cluster `bnk-agentcore-demo` shows request counts, the
429s and latency. Every record also carries `rpc_method`, `tool`, `session` and
`transport`.

From the CLI, `awsbnkctl forge scan -f cluster.yaml` lists the BNK 2.4.1 readiness
checks (`AITenant`, `AICredential`, `AIProvider`, `AIModel`, `AIGatewayProfile`,
`AppPolicy`) and the MCP endpoint with its persistence profile and
iRule; `awsbnkctl forge governance overview -f cluster.yaml` queries Forge's
Multi-Cluster AI Gateway & Dependency Fabric (`/api/k8s/clusters/{id}/governance/overview`),
`awsbnkctl forge governance remediate -f cluster.yaml --action heal_aigw_profile`
triggers healing, and `awsbnkctl forge governance test-request -f cluster.yaml`
runs a live verification probe. `awsbnkctl forge telemetry` (after
`awsbnkctl k port-forward -n llm-egress svc/loki 3100:3100`) checks every
record against the schema the panel reads. Without Forge, query Loki directly:

```bash
kubectl run lq --rm -i --restart=Never -n llm-egress --image=curlimages/curl:8.8.0 -- \
  -s --get 'http://loki:3100/loki/api/v1/query_range' --data-urlencode 'query={job="llm-gateway"}' --data-urlencode 'limit=5'
```

Two live model telemetry sources feed the Loki stream, each labelled with `source`:

- `source=bedrock-logs`: `mcp-bedrock-token-shipper.yaml` polls the Bedrock
  model-invocation log group and ships real token counts and costs. Needs
  Bedrock invocation logging turned on in the account.
- `source=aigateway-241`: `traffic-generator-deployment.yaml` probes the live
  BNK 2.4.1 AI Gateway (`http://10.0.10.150/v1/chat/completions`) and
  records real HTTP response statuses, latency, and usage token counts.

The generator also drives the live MCP VIP every cycle: a forecast with the agent
token (200), a privileged tool with the external token (403), discovery (200)
and an unauthenticated call (401). It uses four of the external caller's ten
requests per minute, so `demo.sh` Act 4 reaches its 429 sooner than on a quiet
cluster.

## Scenarios and BGP

All 15 `awsbnkctl scenarios` run here with no VIP override: the demo Gateway sits
on `.150`, outside the scenario range.
`bnk.bgp: true` opens the BGP/BFD ports; to peer, follow
[`docs/BGP-ROUTE-SERVER.md`](../../docs/BGP-ROUTE-SERVER.md) and apply
[`bgp-route-server.yaml`](bgp-route-server.yaml).

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `400 {"error":"model_missing"}` on every MCP call | token counting was enabled on the Gateway. It is Gateway-scoped and demands an LLM `model` field in every body; leave it off here |
| iRule rejected with `braces are required around the expression` | the admission webhook rejects `#` comment lines inside an `F5BigCneIrule`; keep the TCL comment-free |
| Rate limit trips at half the count | the same iRule attached at both Gateway and listener scope runs twice; attach per listener only |
| VIP goes dark after applying a policy file | a manifest re-declared the `Gateway` with only `metadata`, so apply pruned its `listeners`; re-apply `gateway-deployment.yaml` |
| HTTPS listener healthy but nothing listens on 443 | a listener's protocol was changed in place; delete the Gateway and re-apply |
| `Gateway` is `Programmed=True` but TMM has no virtual server | the Gateway has no `infrastructure.parametersRef` to a `GatewaySettings`, or its static address is outside the Infra listener pool (`.100`–`.199`); use the shape in `gateway-deployment.yaml` |
| `httpx.ReadError` right after redeploying the MCP pod | a warm agent held a pooled connection to the old pod; invoke again |
| `agentcore invoke` times out with "Runtime initialization time exceeded" | the ECR repository was deleted out of band; recreate `bnkagent/financeagentv2agent`, change `main.py` trivially, `npx agentcore deploy --target demo-v2 -y` |
| The agent politely declines a tool instead of calling it | a model guardrail read the tool description; keep sensitivity wording out of the docstring |

## Teardown

Order matters. The stranger's secondary CIDR and security-group cross references
block VPC deletion, and any Route Server endpoint blocks subnet deletion.

```bash
examples/agentcore-demo/scripts/teardown-stranger.sh
examples/agentcore-demo/scripts/teardown-agentcore-network.sh
awsbnkctl down -f examples/agentcore-demo/cluster.yaml --yes
```

## Files

| File | Contains |
| --- | --- |
| `cluster.yaml` | the awsbnkctl intent (K8s `1.35`, BNK `2.4.1`, FLO `v2.30.1-0.0.8`, `aiGateway` enabled) |
| `k8s/bnk-llm-gateway.yaml` (`aigateway-241.yaml`) | BNK 2.4.1 7-step `inference.k8s.f5.com/v1alpha1` chain (`AITenant`, `AICredential`, `AIProvider`, `AIModel`, `AIGatewayProfile`) + `AppPolicy` + `F5BigDnsCache` (`net-resolver`) + `F5SPKStaticRoute` |
| `mcp-tool/` | the MCP finance tool: `mcp-server.py` and the Kustomize base that generates its ConfigMap and token Secret |
| `gateway-deployment.yaml` | the `GatewaySettings`, the BNK `Gateway` (80 and 443) and both `HTTPRoute`s |
| `mcp-persistence.yaml` | passphrase `Secret` and the `F5BigPersistenceProfile` (`MODEL_CONTEXT_PROTOCOL`) that pins each MCP session to one pod |
| `mcp-security-policy.yaml` | rate-limit iRule, per-listener `NetPolicy` (iRule + persistence profile), `F5BigFwPolicy`, `SecPolicy` |
| `mcp-observability.yaml` | `llm-egress` namespace, Loki, the `bnkgov-collector` DaemonSet |
| `mcp-bedrock-token-shipper.yaml` | IRSA ServiceAccount and the shipper that copies Bedrock token counts into Loki |
| `bgp-route-server.yaml` | optional BGP stanza (a ZebOS ConfigMap) for Route Server peering |
| `external-agent.py` | the unmanaged caller, run from inside the VPC |
| `agent/` | the AgentCore runtime (`FinanceAgentV2Agent`) and its deployment config |
| `scripts/rebuild.sh` | builds everything in order |
| `scripts/demo.sh` | walks the demo and asserts every outcome |
| `scripts/setup-agentcore-network.sh` / `teardown-agentcore-network.sh` | security groups and the private DNS zone |
| `scripts/setup-stranger.sh` / `teardown-stranger.sh` | the out-of-range caller for the firewall test |
| `scripts/build-diagram.py` | regenerates the SVGs in `images/` used by `docs/ARCHITECTURE.md` |
