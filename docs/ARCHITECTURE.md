# `bnkctl` Architecture & Unified Multi-Cloud Engine

`bnkctl` consolidates three cloud-specific BNK lifecycle tools (`awsbnkctl`, `azrbnkctl`, and `gkebnkctl`) into a **single Go module (`github.com/JLCode-tech/bnkctl`)** with a shared BNK overlay engine, shared release catalog, shared validation scenario runner, and pluggable cloud underlay providers.

---

## 1. High-Level System Architecture

```mermaid
flowchart TD
    subgraph CLIs["4 Entrypoints (cmd/)"]
        U["cmd/bnkctl\n(Unified Multi-Cloud CLI)"]
        A["cmd/awsbnkctl\n(AWS-Pinned CLI)"]
        Z["cmd/azrbnkctl\n(Azure-Pinned CLI)"]
        G["cmd/gkebnkctl\n(GCP-Pinned CLI)"]
    end

    subgraph Core["Shared Core Packages (pkg/ & internal/cli)"]
        CLI["internal/cli\nCobra Commands & Flags"]
        INT["pkg/intent\nUnified cluster.yaml Parser & Validator"]
        REL["pkg/release\nBNK 2.3.x / 2.4.0 / 2.4.1 / 2.5.0 Catalog"]
        ENG["pkg/bnkengine\nShared BNK Overlay & Quirk Reconciler"]
        FRG["pkg/forge\nBNK Forge REST/MCP & AI Governance Client"]
        SCN["pkg/scenarios\n15 Built-in L4/L7 & GenAI Scenarios"]
    end

    subgraph Providers["Cloud Underlay Providers (internal/providers/)"]
        PAWS["internal/providers/aws\nEKS, VPC, Secondary ENIs, Route Server"]
        PAZR["internal/providers/azure\nAKS, VNet, Accelerated NICs, NSGs"]
        PGCP["internal/providers/gcp\nGKE, Multi-VPC Jumbo Frames, Cloud NAT"]
    end

    U & A & Z & G --> CLI
    CLI --> INT & REL & ENG & FRG & SCN
    CLI --> PAWS & PAZR & PGCP
    PAWS & PAZR & PGCP -->|provider.UnderlayContext| ENG
```

---

## 2. Repository Layout

```text
bnkctl/
├── cmd/
│   ├── bnkctl/          # Unified multi-cloud CLI entrypoint (--cloud aws|azure|gcp)
│   ├── awsbnkctl/       # AWS EKS-pinned CLI entrypoint
│   ├── azrbnkctl/       # Azure AKS-pinned CLI entrypoint
│   └── gkebnkctl/       # GCP GKE-pinned CLI entrypoint
├── internal/
│   ├── cli/             # Unified Cobra command tree (up, down, status, heal, migrate, forge, scenarios, ...)
│   └── providers/
│       ├── aws/         # AWS EKS underlay provisioning, secondary ENI attachment, S3/IAM/IRSA, Route Server
│       ├── azure/       # Azure AKS underlay provisioning, dual-interface Accelerated Networking, Workload Identity
│       └── gcp/         # GCP GKE multi-VPC underlay provisioning, multi-NIC node pools, IAP jumphost
├── pkg/
│   ├── bnk/             # Embedded BNK manifests, templates, and F5 BNK AI Service Plane definitions
│   ├── bnkconst/        # Canonical BNK namespaces, resource groups, and constants
│   ├── bnkengine/       # Cloud-agnostic BNK overlay installer, upgrader, migrator, and quirk healer
│   ├── forge/           # F5 BNK Forge (https://github.com/JLCode-tech/bnk-forge) REST + MCP client, cluster registration, and AI Governance bootstrapper
│   ├── genai/           # aiperf benchmark parser and token-governance telemetry formatter
│   ├── intent/          # Unified cluster.yaml schema, defaults, and strict per-cloud validation
│   ├── jumphost/        # SSH and Kubernetes pod-exec probe runners for connectivity & DNS tests
│   ├── jwt/             # Offline F5 subscription JWT parser and expiration checker
│   ├── k8s/             # Dynamic client-go apply, wait, finalizer stripping, and template variable rendering
│   ├── netcalc/         # CIDR / SelfIP / VIP deterministic network calculator
│   ├── provider/        # Cloud-agnostic Provider interface and UnderlayContext contract
│   ├── release/         # Version-aware BNK release matrix (2.3.x, 2.4.0, 2.4.1, 2.5.0) and quirk registry
│   ├── scenarios/       # 15 end-to-end validation scenarios (HTTP, gRPC, TCP/UDP L4, Egress SNAT, F5 BNK AI Service Plane)
│   └── state/           # Per-cluster state.env persistence (.bnkctl/, .awsbnkctl/, .azrbnkctl/, .gkebnkctl/)
├── examples/            # Ready-to-run multi-cloud blueprints (AWS AgentCore, Azure Foundry, GCP Vertex, Local Zones)
└── docs/                # Architecture, Forge Integration, and Multi-Cloud Quickstart guides
```

---

## 3. Cloud Underlay vs. BNK Overlay Separation

A core design principle of `bnkctl` is the strict boundary between **Cloud Underlay Provisioning** (`internal/providers/{aws,azure,gcp}`) and **BNK Overlay Orchestration** (`pkg/bnkengine`).

### The `provider.Provider` Contract (`pkg/provider/provider.go`)

Each cloud provider implements five methods:

1. `Name() string` — Returns `"aws"`, `"azure"`, or `"gcp"`.
2. `ValidateIntent(cl *intent.Cluster) error` — Validates cloud-specific `cluster.yaml` constraints.
3. `ProvisionUnderlay(ctx, cl, st, dryRun) (*UnderlayContext, error)` — Provisions VPC/VNet, subnets, NAT gateways, security groups/firewalls, managed Kubernetes cluster, multi-NIC worker nodes, and optional jumphost.
4. `DiscoverUnderlay(ctx, cl, st) (*UnderlayContext, error)` — Hydrates an `UnderlayContext` from existing cloud resources and `state.env` without re-provisioning.
5. `DestroyUnderlay(ctx, cl, st, dryRun) error` — Tears down all cloud resources in reverse dependency order and cleans up `state.env`.

### The `UnderlayContext` Handoff

Once `ProvisionUnderlay` or `DiscoverUnderlay` finishes, it returns a normalized `provider.UnderlayContext` containing:
- Cluster name, region, Kubernetes `rest.Config`, and kubeconfig path
- Dataplane driver (`sock` / `host-device` / `virtio`) and CNI network attachment definitions (`external-nad`, `internal-nad`)
- External and Internal `SelfIPs`, prefixes, VLANs/MTUs, and default `VIP` addresses
- Cloud-specific lifecycle hooks (`PreOverlayHooks`, `PostOverlayHooks`, `DiscoverDataplaneResources`)

`pkg/bnkengine.Engine` consumes only `UnderlayContext`, `intent.Cluster`, and `release.Profile`. As a result, **every BNK release train (`2.3.x`, `2.4.0`, `2.4.1`, `2.5.0`), self-healing quirk, and validation scenario works identically across AWS, Azure, and GCP**.

---

## 4. Version-Aware Release Catalog & Self-Healing (`pkg/release`)

BNK releases span two major architectural trains:

| Release Train | Versions | Capability Overview |
| :--- | :--- | :--- |
| **`legacy-spk` (2.3.x)** | `2.3.2-3.2598.3-0.0.464` | Cloud-native L4/L7 ingress, egress, and carrier-grade network traffic management for Kubernetes clusters. |
| **`gateway-infra` (2.4.x / 2.5.0)** | `2.4.0`, `2.4.1`, `2.5.0` | Next-generation Kubernetes Gateway API architecture with native **F5 BNK AI Service Plane** traffic governance and lifecycle automation. |

For full release notes, architectural overviews, and feature specifications, see the [F5 BIG-IP Next for Kubernetes CloudDocs](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/overview.html).

`pkg/release` tracks the validated chart versions, image tags, and known release quirks for each version. Running `bnkctl heal` (or `bnkctl up`) automatically applies idempotent reconciliations:
- **Certificate & Webhook Readiness**: Ensures TLS certificates and admission endpoints are ready before custom resources are applied.
- **Data-Plane Resource & Hugepages Tuning**: Aligns data-plane resource requests and hugepages with cloud VM shapes.
- **F5 BNK AI Service Plane Readiness**: Reconciles BNK 2.4.1+ AI traffic routing, governance, and token-rate enforcement prerequisites.
- **Cross-Train Migration (`bnkctl migrate`)**: Safely transitions live clusters from `2.3.x` (`legacy-spk`) to `2.4.x/2.5.0` (`gateway-infra`) without recreating the cloud underlay.
