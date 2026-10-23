# Governing the Agentic Action Path: Azure AI Foundry + F5 BNK

## Overview

As enterprises accelerate moving AI agents into production, the fundamental challenge shifts from *building* the agent to *securing and governing its reach*.

**Microsoft Azure AI Foundry** (and Azure OpenAI Service) provides the scalable, enterprise framework to run generative AI agents (handling reasoning, model orchestration, memory, and tool integration). However, when an AI agent decides to invoke an external API, a proprietary data repository, or an on-premise MCP server, it crosses a critical enterprise trust boundary.

This is where **F5 BIG-IP Next for Kubernetes (BNK)** operates. BNK does not replicate the AI agent's language reasoning; instead, it provides deterministic high-performance networking, L4/L7 security, fine-grained access control, rate limiting, and observability exactly where the agentic traffic crosses the cluster boundary.

**This demonstration stands up a dedicated Azure Kubernetes Service (AKS) environment in the Australia East (`australiaeast`) region, where an Azure AI Foundry agent invokes an MCP tool, with the entire data path governed by F5 BNK.**

---

## Deployment Topology & Architecture

- **Azure Region**: `australiaeast` (Sydney, Australia)
- **Cluster Name**: `bnk-agentfoundry-demo`
- **Network Pattern**: `dual-interface` (Azure Accelerated Networking on AKS)
- **VNet CIDR**: `10.0.0.0/16`
  - System Subnet: `10.0.0.0/20`
  - Public Subnet: `10.0.16.0/28`
  - External Subnet (BNK VIP): `10.0.17.0/24` (VIP: `10.0.17.100`)
  - Internal Subnet (Core Data Plane): `10.0.18.0/24`
- **Kubernetes Version**: `1.35`
- **F5 BNK Manifest**: `2.4.1` (`2.4.1-3.2850.1-0.0.9`, FLO `v2.30.1-0.0.8`)

```
                                      TRUST BOUNDARY
┌─────────────────────────────────┐         │         ┌─────────────────────────────────────────────────────────┐
│     Azure AI Foundry Agent      │         │         │               AKS Cluster (australiaeast)               │
│                                 │         │         │                                                         │
│  1. Prompt: "Forecast MSFT"     │         │         │  ┌───────────────────────────────────────────────────┐  │
│  2. Model: Azure OpenAI (gpt-4o)│         │         │  │              F5 BIG-IP Next for K8s (BNK 2.4.1)   │  │
│  3. Stop: needs tool 'forecast' │         │         │  │                                                   │  │
│                                 │         │         │  │  • Multi-Cluster AI Gateway (inference.k8s.f5.com)│  │
│  4. Outbound Tool Call ─────────┼─────────┼─────────┼─►│  • AIGatewayProfile & AppPolicy (Tokens/Failover) │  │
│     (Authorization: Bearer ...) │         │         │  │  • L4 Firewall (F5BigFwPolicy)                    │  │
│                                 │         │         │  │  • L7 TLS Termination (Gateway / Secret)          │  │
│                                 │         │         │  │  • Rate Limiting (F5BigCneIrule: 10 req/60s)      │  │
│                                 │         │         │  │  • RBAC Tool Enforcement (F5BigCneIrule)          │  │
│                                 │         │         │  │  • Telemetry Logging (BNKGOV -> Fluent Bit)       │  │
│                                 │         │         │  └─────────────────────────┬─────────────────────────┘  │
│                                 │         │         │                            │ HTTPRoute Rewrite          │
│                                 │         │         │                            ▼                            │
│                                 │         │         │  ┌───────────────────────────────────────────────────┐  │
│  6. Final Response Narrated     │         │         │  │                 FastMCP Tool Pod                  │  │
│     "MSFT projected +14%" ◄─────┼─────────┼─────────┼──┤  • forecast(symbol, days)                         │  │
│                                 │         │         │  │  • get_account_balance(account_id) [PRIVILEGED]   │  │
│                                 │         │         │  └───────────────────────────────────────────────────┘  │
└─────────────────────────────────┘         │         └─────────────────────────────────────────────────────────┘
```

---

## The Three Governed Paths

| Path | Caller Identity | Security & Governance Mechanisms | Status |
| :--- | :--- | :--- | :--- |
| **Trusted Agent Path** | Azure AI Foundry Agent (`agent`) | F5 BNK 2.4.1 AI Gateway (`AITenant`, `AICredential`, `AIProvider`, `AIModel`, `AIGatewayProfile`, `AppPolicy`) + L7 MCP Policy, Bearer Token Validation, Rate Limiting | Supported |
| **Double-Checked Path** | Azure AI Gateway + Agent | Azure API Management / Gateway semantic check **plus** F5 BNK 2.4.1 L7 Policy | Supported |
| **Stranger Path** | Unmanaged Scripts / Other Clouds (`external` or `anonymous`) | F5 BNK L4 Firewall (`F5BigFwPolicy`) + L7 RBAC blocking sensitive tools | Supported |

---

## Demo Walkthrough & Lifecycle

### Step 1: Provision Cluster & BNK 2.4.1 Control Plane
```bash
# Validate configuration
azrbnkctl validate examples/agentfoundry-demo/cluster.yaml

# Provision infrastructure and F5 BNK 2.4.1 in australiaeast
azrbnkctl up -f examples/agentfoundry-demo/cluster.yaml
```

### Step 2: Deploy FastMCP Tool, Ingress Gateway, and BNK 2.4.1 AI Gateway
```bash
KUBECONFIG=.azrbnkctl/bnk-agentfoundry-demo/kubeconfig \
  kubectl apply -k examples/agentfoundry-demo/mcp-tool/

KUBECONFIG=.azrbnkctl/bnk-agentfoundry-demo/kubeconfig \
  kubectl apply -f examples/agentfoundry-demo/gateway-deployment.yaml

KUBECONFIG=.azrbnkctl/bnk-agentfoundry-demo/kubeconfig \
  ./examples/agentfoundry-demo/scripts/setup-llm-gateway.sh
```

### Step 3: Apply BNK Security Policies & Rate Limiting
```bash
KUBECONFIG=.azrbnkctl/bnk-agentfoundry-demo/kubeconfig \
  kubectl apply -f examples/agentfoundry-demo/mcp-security-policy.yaml
```

### Step 4: Deploy Live Observability Pipeline
```bash
KUBECONFIG=.azrbnkctl/bnk-agentfoundry-demo/kubeconfig \
  kubectl apply -f examples/agentfoundry-demo/mcp-observability.yaml

KUBECONFIG=.azrbnkctl/bnk-agentfoundry-demo/kubeconfig \
  kubectl apply -f examples/agentfoundry-demo/mcp-foundry-token-shipper.yaml

KUBECONFIG=.azrbnkctl/bnk-agentfoundry-demo/kubeconfig \
  kubectl apply -f examples/agentfoundry-demo/traffic-generator-deployment.yaml
```

### Step 5: Execute Governed Invocations
```bash
# 1. Run trusted Azure AI Foundry agent invocation
python3 examples/agentfoundry-demo/foundry-agent.py --prompt "Forecast MSFT for 30 days"

# 2. Run stranger agent attempting to access privileged tool
python3 examples/agentfoundry-demo/external-agent.py --tool get_account_balance --account ACC-1001

# 3. Query Forge Multi-Cluster AI Gateway & Dependency Fabric
azrbnkctl forge governance overview -f examples/agentfoundry-demo/cluster.yaml
azrbnkctl forge governance test-request -f examples/agentfoundry-demo/cluster.yaml

# 4. Or execute the full 7-act guided walkthrough
./examples/agentfoundry-demo/scripts/demo.sh
```

---

## Verification in BNK Forge

When registered with BNK Forge (`http://localhost:8000`):
1. **Multi-Cluster AI Gateway**: Displays the `inference.k8s.f5.com/v1alpha1` chain (`AITenant`, `AICredential`, `AIProvider`, `AIModel`, `AIGatewayProfile`), `AppPolicy` (`gateway.k8s.f5.com/v1alpha1`), `F5BigDnsCache` (`net-resolver`), and `F5SPKStaticRoute`.
2. **Gateway Topology**: Renders Gateway `bnk-agentfoundry-demo-gateway`, listeners on ports 80 & 443, and attached `F5BigCneIrule` and `F5BigFwPolicy`.
3. **LLM Observability**: Ingests real-time `BNKGOV` and `aigateway-241` streams from Loki (`http://loki.llm-egress:3100`), visualizing caller identities (`agent`, `external`), models (`mcp:finance-tool`, `gpt-4o`), latencies, status codes (`200`, `403`, `429`), and cost breakdowns.
