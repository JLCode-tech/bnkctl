# Changelog

All notable changes to `bnkctl` are documented in this file.

## [2.5.0](https://github.com/JLCode-tech/bnkctl/compare/v2.4.1...v2.5.0) (2026-10-29)


### Features

* **aws:** dynamic AZ, IMDSv2 subnet/VPC CIDR, and service CIDR discovery ([723c800](https://github.com/JLCode-tech/bnkctl/commit/723c8006c38ce1bfbee578b3d3bb0564ad6350bc))


### Bug Fixes

* **agentcore-demo:** use OpenAI-compatible Bedrock model IDs for /openai/v1/chat/completions ([ae83017](https://github.com/JLCode-tech/bnkctl/commit/ae83017d9bb6039e269ef0028badc5f5d9d36586))
* **agentcore-demo:** zero-touch e2e lifecycle, TMM asymmetric conntrack, Forge governance sync, and clean teardown ([34b6ae8](https://github.com/JLCode-tech/bnkctl/commit/34b6ae86b4a889f29aa1a1516760880623f394cc))
* **aws,agentcore-demo:** ensure cross-AZ stranger subnet discovery via bnk.f5.com/datapath and awsbnkctl:component tags ([c250a57](https://github.com/JLCode-tech/bnkctl/commit/c250a571b30ab421d38a298e2f75abddb490204a))
* **aws,agentcore-demo:** sweep residual VPC instances before IGW/subnet teardown and scope MCP rate limits by session ([edc6d42](https://github.com/JLCode-tech/bnkctl/commit/edc6d42760683a741375abc5056b32910d6da199))
* **aws,bnkengine:** bake BNK 2.4.1 AWS ENA dataplane fixes and teardown hardening ([6e04894](https://github.com/JLCode-tech/bnkctl/commit/6e04894ab3c1db2b82a67372a7a3e3e9c77e0ed4))
* **aws,bnkengine:** bake BNK 2.4.1 AWS ENA dataplane fixes and teardown hardening ([3aca4b3](https://github.com/JLCode-tech/bnkctl/commit/3aca4b34ee16b6c5068f48256c1d50c00bd647c2))
* **aws:** complete dynamic AZ/NIC/CIDR discovery and sync agentcore-demo ([48689cf](https://github.com/JLCode-tech/bnkctl/commit/48689cf61621e1e75cbb22f163712559cddbcfcb))
* **aws:** fallback to AWS CLI credential cache on SSO token expiry and pin AWS_PROFILE in kubeconfig ([96ef937](https://github.com/JLCode-tech/bnkctl/commit/96ef937745c0c794e5201047326873e201ed9c7e))
* **ci:** upgrade indirect deps for govulncheck and add dockerconfig.json dry-run stub ([6d33c21](https://github.com/JLCode-tech/bnkctl/commit/6d33c216683d26abdef4325f38f02319bdde53ef))
* **forge,agentcore-demo:** use awsbnkctl/aws/f5-cne-system labels instead of azrbnkctl leftovers ([2417d2d](https://github.com/JLCode-tech/bnkctl/commit/2417d2d12ffee787f4d34977956f6c6ff00525ea))

## [2.4.1] - 2026-10-23

### Added
- Unified multi-cloud Go engine (`bnkctl`) supporting AWS EKS (`--cloud aws`), Azure AKS (`--cloud azure`), and Google Cloud GKE (`--cloud gcp`), plus cloud-pinned entrypoints (`awsbnkctl`, `azrbnkctl`, `gkebnkctl`).
- Version-aware BNK release catalog supporting BNK `2.3.x` (`legacy-spk`), `2.4.0`, and `2.4.1` (`gateway-infra`) with automatic release-quirk self-healing (`bnkctl heal`) and cross-train migration (`bnkctl migrate`).
- Turnkey **F5 BNK Forge** (`https://github.com/JLCode-tech/bnk-forge`) and **F5 BNK AI Service Plane** integration (`bnkctl forge register`, `bnkctl forge governance`, `bnkctl test aiperf`) across AWS Bedrock, Azure AI Foundry, and Google Vertex AI.
- 15 built-in L4/L7, egress SNAT, and GenAI validation scenarios (`bnkctl scenarios`).
- Deterministic zero-leak teardown (`bnkctl down`) across AWS, Azure, and GCP with Kubernetes finalizer stripping and orphaned volume/ENI/NIC cleanup.
