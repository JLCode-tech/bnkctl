# F5 BNK Forge & F5 BNK AI Service Plane Integration

`bnkctl` includes native, first-class integration with [**F5 BNK Forge**](https://github.com/JLCode-tech/bnk-forge) (`pkg/forge`), transforming standalone Kubernetes clusters on AWS EKS, Azure AKS, and GCP GKE into a unified, visually managed multi-cloud CNF and **F5 BNK AI Service Plane**.

![BNK Forge Platform Overview](assets/forge-platform-overview.png)

---

## 1. Automatic Lifecycle Linking (`bnkctl up` & `bnkctl down`)

Adding a `forge:` block to your `cluster.yaml` enables automatic zero-touch registration with [BNK Forge](https://github.com/JLCode-tech/bnk-forge) during `bnkctl up`:

```yaml
forge:
  enabled: true
  url: http://localhost:8000
  mcpUrl: http://localhost:8081/mcp/
  projectName: bnk-multi-cloud-demo
  credentialTemplateId: 1
  username: admin
```

### What Happens During `bnkctl up` (Phase 29)
1. **Project & Credential Resolution**: `bnkctl` authenticates to [BNK Forge](https://github.com/JLCode-tech/bnk-forge) (via REST and MCP), resolves or creates the target Forge Project, and attaches the cloud credential template.
2. **Cluster Registration & Kubeconfig Sync**: Registers the newly provisioned EKS/AKS/GKE cluster, uploads its kubeconfig and network topology metadata, and writes `.bnkctl/<cluster>/forge_link.json` locally.
3. **F5 BNK AI Service Plane & Governance Seeding**: When `bnk.aiGateway.enabled: true`, `bnkctl` automatically bootstraps the cluster's AI Governance organization, team, virtual API key (`f5vk_...`), and cloud LLM router pools in [BNK Forge](https://github.com/JLCode-tech/bnk-forge).

### What Happens During `bnkctl down`
- Before destroying the cloud underlay, `bnkctl down` automatically unregisters the cluster from [BNK Forge](https://github.com/JLCode-tech/bnk-forge) and removes stale cluster links so your Forge dashboard never accumulates orphaned clusters.

---

## 2. Multi-Cloud Cluster Fleet & Visual Policy Authoring

Once registered, all clusters across AWS, Azure, and GCP are visible in the **Multi-Cloud Cluster Fleet** view, displaying data-plane health, active listeners, BGP peering status, and throughput telemetry:

![Multi-Cloud Cluster Fleet](assets/forge-multi-cloud-cluster-fleet.png)

Operators can deploy pre-validated **Multi-Cloud Blueprints** or author L4/L7 traffic routing, NAT, and network security policies visually in the **BNK Policy Builder**. For complete documentation on F5 BIG-IP Next for Kubernetes traffic and security capabilities, see the [F5 BIG-IP Next for Kubernetes CloudDocs](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/overview.html):

![Multi-Cloud Blueprints Catalog](assets/forge-blueprints-catalog.png)

![Visual BNK Policy Builder](assets/forge-bnk-policy-builder.png)

---

## 3. F5 BNK AI Service Plane: Token Governance & Guardrails

With BNK 2.4.1+, `bnkctl` enables the **F5 BNK AI Service Plane** to govern AI inference and agentic traffic across your clusters, while [BNK Forge](https://github.com/JLCode-tech/bnk-forge) aggregates token consumption, latency, cost, and guardrail enforcement across all three clouds in real time. See the [F5 BIG-IP Next for Kubernetes CloudDocs](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/overview.html) for full details on F5's cloud-native data plane capabilities:

![Multi-Cloud AI Token Governance Overview](assets/forge-ai-token-governance-overview.png)

### Per-Cloud AI Governance Views

Each cloud cluster reports granular prompt/completion token usage, virtual key budgets, and model-level telemetry:

#### AWS EKS (`agentcore-demo` — Amazon Bedrock Claude Sonnet / Haiku)
![AWS EKS AI Governance](assets/forge-aws-governance.png)

#### Azure AKS (`agentfoundry-demo` — Azure AI Foundry GPT-4o / GPT-4.1)
![Azure AKS AI Governance](assets/forge-azure-governance.png)

#### GCP GKE (`vertex-agent-demo` — Google Vertex AI Gemini 2.5 Pro / Flash)
![GCP GKE AI Governance](assets/forge-gcp-governance.png)

### AI Data Governance Guardrails (DLP, PII Redaction & Rate Limiting)

Enforce token-per-minute (TPM) rate limits, prompt-injection defense, and sensitive data redaction across every inference route:

![AI Data Governance Guardrails](assets/forge-ai-governance-guardrails.png)

---

## 4. Smart LLM Router Fleet & Multi-Cloud Pool Topologies

`bnkctl` and [BNK Forge](https://github.com/JLCode-tech/bnk-forge) configure weighted primary/fallback LLM routing pools so agentic workloads automatically fail over across regions or model tiers when rate limits or provider errors occur:

![Smart LLM Router Fleet](assets/forge-smart-llm-router-fleet.png)

![Smart LLM Router Topology](assets/forge-llm-router-topology.png)

### Cloud-Specific LLM Router Pools

- **AWS Bedrock Pools (`ap-southeast-2` -> `us-east-1` fallback)**:
  ![AWS LLM Router Pools](assets/forge-aws-llm-router-pools.png)
- **Azure AI Foundry Pools (`eastus2` -> `swedencentral` fallback)**:
  ![Azure LLM Router Pools](assets/forge-azure-llm-router-pools.png)
- **GCP Vertex AI Pools (`australia-southeast1` -> `us-central1` fallback)**:
  ![GCP LLM Router Pools](assets/forge-gcp-llm-router-pools.png)

---

## 5. Multi-Stack GenAI Benchmarking & MCP Operations

Use `bnkctl genai parse` and [BNK Forge](https://github.com/JLCode-tech/bnk-forge)'s **Benchmark Dashboard** to compare F5 BNK + F5 GAIE against Envoy, HAProxy, and `llm-d` Router across concurrency tiers:

![Benchmark Dashboard](assets/forge-benchmark-dashboard.png)

And connect any MCP-compatible AI coding or operations assistant to [BNK Forge](https://github.com/JLCode-tech/bnk-forge)'s **153 MCP Server Tools** for natural-language cluster inspection, policy verification, and live troubleshooting:

![Forge MCP Server Tools](assets/forge-mcp-server-tools.png)

---

## 6. `bnkctl forge` CLI Commands

```bash
# Check Forge link status and server reachability
bnkctl forge status -c cluster.yaml

# Manually register or re-register an active cluster with BNK Forge
bnkctl forge register -c cluster.yaml

# Inspect the Multi-Cluster F5 BNK AI Service Plane dependency graph & readiness checklist
bnkctl forge governance overview -c cluster.yaml

# Bootstrap F5 BNK AI Service Plane org, team, principal, and virtual API key (f5vk_...)
bnkctl forge governance bootstrap -c cluster.yaml

# Send a live chat-completion probe through the cluster's F5 BNK AI Service Plane VIP
bnkctl forge governance test-request -c cluster.yaml

# Remediate BNK 2.4.1 F5 BNK AI Service Plane configuration drift via Forge
bnkctl forge governance remediate -c cluster.yaml

# Unregister the cluster from BNK Forge
bnkctl forge unregister -c cluster.yaml
```
