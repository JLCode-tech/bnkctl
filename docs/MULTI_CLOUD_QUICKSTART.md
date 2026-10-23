# Multi-Cloud Quickstart Guide (AWS EKS, Azure AKS & GCP GKE)

This guide walks through deploying, validating, and tearing down **F5 BIG-IP Next for Kubernetes (BNK)** across **Amazon EKS**, **Azure AKS**, and **Google Cloud GKE** using `bnkctl`.

---

## Step 1: Build the Binaries

```bash
make build
```

This compiles four binaries in `./bin/`:
- `./bin/bnkctl` — Unified CLI (auto-detects cloud from `cluster.yaml` or accepts `--cloud aws|azure|gcp`)
- `./bin/awsbnkctl` — Pinned to AWS EKS
- `./bin/azrbnkctl` — Pinned to Azure AKS
- `./bin/gkebnkctl` — Pinned to GCP GKE

---

## Step 2: Configure F5 BIG-IP Next for Kubernetes Prerequisites

Every BNK deployment requires operator-supplied licensing and F5 container registry access (strictly gitignored across the repository so credentials can never be committed):

1. **F5 Subscription JWT (`license.jwt`)** — Grants BNK instance licensing.
2. **F5 Artifact Registry Access** — Grants container image and Helm chart pull access from `repo.f5.com`.

Follow the official [F5 BIG-IP Next for Kubernetes CloudDocs](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/overview.html) to obtain your F5 subscription JWT and configure registry pull credentials. Place or symlink them into the example directory you plan to run (or at the repository root as referenced by `cluster.yaml`):

```bash
# Verify your local prerequisites and JWT expiration before provisioning
./bin/bnkctl doctor -c examples/agentcore-demo/cluster.yaml
```

---

## Option A: AWS EKS (`examples/agentcore-demo/`)

### 1. Authenticate to AWS
Ensure your AWS CLI session or environment variables are active:
```bash
aws sts get-caller-identity
```

### 2. Validate & Dry-Run
```bash
./bin/bnkctl validate -c examples/agentcore-demo/cluster.yaml
./bin/bnkctl up -c examples/agentcore-demo/cluster.yaml --dry-run
```

### 3. Provision Underlay & Deploy BNK 2.4.1 + F5 BNK AI Service Plane
```bash
./bin/bnkctl up -c examples/agentcore-demo/cluster.yaml
```
What `bnkctl up` provisions on AWS:
- VPC (`10.0.0.0/16`) with public, private, and dedicated `external` (`10.0.10.0/24`) and `internal` (`10.0.20.0/24`) data-plane subnets
- Managed EKS 1.35 cluster and worker nodegroup (`m6i.4xlarge`)
- Secondary ENIs attached to worker nodes (`host-device` mode) with source/dest check disabled
- Full F5 BIG-IP Next for Kubernetes 2.4.1 data plane and **F5 BNK AI Service Plane** (see [F5 CloudDocs](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/overview.html))
- Optional EC2 jumphost in the management subnet and automatic registration with [BNK Forge](https://github.com/JLCode-tech/bnk-forge)

### 4. Verify & Tear Down
```bash
./bin/bnkctl status -c examples/agentcore-demo/cluster.yaml
./bin/bnkctl scenarios run http-routing-e2e -c examples/agentcore-demo/cluster.yaml
./bin/bnkctl down -c examples/agentcore-demo/cluster.yaml
```

---

## Option B: Azure AKS (`examples/agentfoundry-demo/`)

### 1. Authenticate to Azure
Log in with Azure CLI (or export `AZURE_SUBSCRIPTION_ID`):
```bash
az login
az account show
```
*(Note: When `metadata.subscriptionId` in `examples/agentfoundry-demo/cluster.yaml` is left at the placeholder `00000000-0000-0000-0000-000000000000`, `bnkctl` automatically resolves your active subscription from `AZURE_SUBSCRIPTION_ID` or `az account show`.)*

### 2. Validate & Dry-Run
```bash
./bin/bnkctl validate -c examples/agentfoundry-demo/cluster.yaml
./bin/bnkctl up -c examples/agentfoundry-demo/cluster.yaml --dry-run
```

### 3. Provision Underlay & Deploy BNK 2.4.1 + F5 BNK AI Service Plane
```bash
./bin/bnkctl up -c examples/agentfoundry-demo/cluster.yaml
```
What `bnkctl up` provisions on Azure:
- Resource Group and VNet (`10.0.0.0/16`) with `system`, `public`, `external`, and `internal` subnets
- Managed AKS cluster with separate `system` and Accelerated Networking `tmm` node pools (`dual-interface` pattern)
- Dynamic secondary NIC attachment on worker VMs and IP forwarding enablement
- Full F5 BIG-IP Next for Kubernetes 2.4.1 data plane, VXLAN egress, **F5 BNK AI Service Plane** (see [F5 CloudDocs](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/overview.html)), and [BNK Forge](https://github.com/JLCode-tech/bnk-forge) registration

### 4. Verify & Tear Down
```bash
./bin/bnkctl status -c examples/agentfoundry-demo/cluster.yaml
./bin/bnkctl down -c examples/agentfoundry-demo/cluster.yaml
```

---

## Option C: Google Cloud GKE (`examples/vertex-agent-demo/`)

### 1. Authenticate to GCP & Set Your Project ID
Authenticate with `gcloud` and export your GCP project ID (or update `metadata.project` in `examples/vertex-agent-demo/cluster.yaml`):
```bash
gcloud auth application-default login
export GOOGLE_CLOUD_PROJECT="your-gcp-project-id"
```
*(Note: When `metadata.project` in `examples/vertex-agent-demo/cluster.yaml` is set to `YOUR_GCP_PROJECT_ID`, `bnkctl` automatically uses `$GOOGLE_CLOUD_PROJECT`, `$GCLOUD_PROJECT`, or `$CLOUDSDK_CORE_PROJECT`.)*

### 2. Validate & Dry-Run
```bash
./bin/bnkctl validate -c examples/vertex-agent-demo/cluster.yaml
./bin/bnkctl up -c examples/vertex-agent-demo/cluster.yaml --dry-run
```

### 3. Provision Underlay & Deploy BNK 2.4.1 + F5 BNK AI Service Plane
```bash
./bin/bnkctl up -c examples/vertex-agent-demo/cluster.yaml
```
What `bnkctl up` provisions on GCP:
- Three VPCs (`main`, `external`, `internal`) with secondary pod/service IP ranges, Cloud Router + Cloud NAT, and Jumbo Frame MTU (`8896`) on the data-plane VPCs
- GKE cluster (`1.35`) with multi-network support enabled, `tmm-pool` attached to the `external` and `internal` VPCs (`virtio` driver), and `agent-pool`
- Full F5 BIG-IP Next for Kubernetes 2.4.1 data plane, VXLAN egress, **F5 BNK AI Service Plane** routing to Google Vertex AI (Gemini 2.5 Pro/Flash) (see [F5 CloudDocs](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/overview.html)), and [BNK Forge](https://github.com/JLCode-tech/bnk-forge) registration

### 4. Verify & Tear Down
```bash
./bin/bnkctl status -c examples/vertex-agent-demo/cluster.yaml
./bin/bnkctl down -c examples/vertex-agent-demo/cluster.yaml
```

---

## Upgrading & Migrating BNK Releases

`bnkctl` supports both in-train upgrades and cross-train architectural migrations on any running cluster:

```bash
# Upgrade within the 2.4.x / 2.5.0 gateway-infra train
./bin/bnkctl upgrade --to-version 2.5.0 -c cluster.yaml

# Migrate a 2.3.x legacy-spk cluster to 2.4.1 gateway-infra
./bin/bnkctl migrate --to-version 2.4.1 -c cluster.yaml
```
