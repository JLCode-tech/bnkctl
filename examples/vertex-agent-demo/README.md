# Governing the Agentic Action Path: Google Cloud Vertex AI + F5 BNK on GKE

## Overview

As enterprises deploy autonomous AI agents into production on Google Cloud, securing the **action path**—where reasoning models transition into tool and API execution—becomes essential.

**Google Cloud Vertex AI Reasoning Engine** (powered by Gemini models such as `gemini-2.5-pro`) provides scalable reasoning, orchestration, and agent execution. When an agent invokes backend APIs, data services, or Model Context Protocol (MCP) servers, that traffic crosses critical enterprise networking and security boundaries.

**F5 BIG-IP Next for Kubernetes (BNK)** operates directly at this boundary on GKE:
- **High-Performance Multi-Networking**: Leverages GKE host-device secondary VPC interfaces and TMM data planes for deterministic L4-L7 traffic handling.
- **Action Path Governance**: Enforces per-agent rate limits, token authentication, and policy-driven routing for tool execution.
- **Enterprise Observability**: Streams comprehensive audit metrics, latency telemetry, and logs to BNK Forge and OpenTelemetry/Prometheus collectors.

---

## Architecture & Traffic Flow

```mermaid
flowchart TD
    subgraph Reasoning Layer [Google Cloud Vertex AI]
        Agent[Vertex AI Agent / Gemini 2.5]
        ExternalClient[External / Automated Client]
    end

    subgraph Governance Boundary [F5 BIG-IP Next for Kubernetes on GKE]
        TMM[F5 TMM Ingress Gateway 10.10.1.50]
        RateLimit[TMM Rate Limiter 20 req/s]
        AuthPolicy[Bearer Token & Security Policy]
    end

    subgraph Backend Workloads [GKE Cluster Pods]
        MCPTool[FastMCP Financial Service]
    end

    Agent -- "Trusted Agent Path (Bearer Token)" --> TMM
    ExternalClient -- "External Path (Strict Filter)" --> TMM
    TMM --> RateLimit --> AuthPolicy --> MCPTool
```

### The Three Governed Paths

1. **Trusted Agent Path**:
   The agent runs within Google Cloud or GKE using Workload Identity Federation. It resolves `bnk-ingress.bnk-demo.internal` to the BNK Ingress VIP (`10.10.1.50`) and executes MCP tool calls. F5 BNK validates caller credentials, enforces rate limits, and routes the traffic to the backend tool.
2. **Double-Checked Path**:
   The agent invocation passes through **Vertex AI Extensions and Model Armor** for semantic validation, followed by **F5 BNK** on GKE for network isolation, MTU alignment, and DDoS protection.
3. **External / Stranger Path**:
   External callers or untrusted workloads targeting cluster ingress are strictly filtered and governed by F5 BNK, protecting sensitive backend operations such as financial forecasting and account balances.

---

## Directory Structure

```text
examples/vertex-agent-demo/
├── README.md                 # GKE & Vertex AI governance guide (BNK 2.4.1, K8s 1.35)
├── cluster.yaml              # GKE cluster definition (K8s 1.35, BNK 2.4.1, FLO v2.30.1-0.0.8, aiGateway)
├── k8s/bnk-llm-gateway.yaml  # Authentic BNK 2.4.1 7-step inference.k8s.f5.com/v1alpha1 + AppPolicy + DNS/Route
├── gateway-deployment.yaml   # BNK Gateway & HTTPRoutes
├── mcp-persistence.yaml      # F5BigPersistenceProfile (MODEL_CONTEXT_PROTOCOL)
├── mcp-security-policy.yaml  # F5BigCneIrule, NetPolicy, F5BigFwPolicy, SecPolicy
├── mcp-observability.yaml    # Loki & Fluent Bit observability stack
├── mcp-vertex-token-shipper.yaml # Vertex AI Gemini 2.5 Pro token shipper
├── traffic-generator-deployment.yaml # Live dual-leg traffic generator (BNK 2.4.1 AI Gateway + MCP)
├── mcp-tool/
│   ├── mcp-server.py         # FastMCP Python service (forecast & get_account_balance)
│   ├── deployment.yaml       # K8s Deployment, Service, and Secret
│   └── kustomization.yaml    # Kustomize manifest generator
├── agent/
│   ├── vertex_agent.py       # Vertex AI Gemini agent runner
│   ├── requirements.txt      # Python dependencies
│   └── README.md             # Agent configuration guide
└── scripts/
    ├── setup-llm-gateway.sh  # 7-step BNK 2.4.1 AI Gateway + CNE restart + f5vk_ governance bootstrap
    ├── demo.sh               # Automated deployment and verification script
    └── traffic-gen.sh        # Live AI Gateway + MCP traffic generator
```

---

## Step-by-Step Guide

### 1. Validate and Provision the Cluster (K8s 1.35, BNK 2.4.1)
```bash
# Validate the GKE multi-network cluster manifest
gkebnkctl validate examples/vertex-agent-demo/cluster.yaml

# Provision GKE cluster with F5 BNK 2.4.1 (FLO v2.30.1-0.0.8)
gkebnkctl up -f examples/vertex-agent-demo/cluster.yaml
```

### 2. Deploy MCP Tool, BNK 2.4.1 AI Gateway, and Governance Policies
```bash
kubectl apply -k examples/vertex-agent-demo/mcp-tool
kubectl apply -f examples/vertex-agent-demo/gateway-deployment.yaml
kubectl apply -f examples/vertex-agent-demo/mcp-persistence.yaml
kubectl apply -f examples/vertex-agent-demo/mcp-security-policy.yaml
./examples/vertex-agent-demo/scripts/setup-llm-gateway.sh
```

### 3. Connect Cluster to BNK Forge & Inspect Multi-Cluster AI Gateway Fabric
```bash
# Scan BNK 2.4.1 AI Gateway dependency chain and MCP posture
gkebnkctl forge scan -f examples/vertex-agent-demo/cluster.yaml --register-targets

# Query Forge Multi-Cluster AI Gateway & Dependency Fabric
gkebnkctl forge governance overview -f examples/vertex-agent-demo/cluster.yaml --forge-url http://localhost:8000
gkebnkctl forge governance test-request -f examples/vertex-agent-demo/cluster.yaml --forge-url http://localhost:8000
```

### 4. Execute Governed Agent Reasoning
```bash
python3 examples/vertex-agent-demo/agent/vertex_agent.py --symbol GOOGL --days 30
```

### 5. Verify Rate Limiting and Live Traffic Governance
```bash
./examples/vertex-agent-demo/traffic-gen.sh
```
