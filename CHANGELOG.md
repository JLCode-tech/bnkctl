# Changelog

All notable changes to `bnkctl` are documented in this file.

## [2.4.2](https://github.com/JLCode-tech/bnkctl/compare/v2.4.1...v2.4.2) (2026-10-23)


### Bug Fixes

* **ci:** upgrade indirect deps for govulncheck and add dockerconfig.json dry-run stub ([6d33c21](https://github.com/JLCode-tech/bnkctl/commit/6d33c216683d26abdef4325f38f02319bdde53ef))

## [2.4.1] - 2026-10-23

### Added
- Unified multi-cloud Go engine (`bnkctl`) supporting AWS EKS (`--cloud aws`), Azure AKS (`--cloud azure`), and Google Cloud GKE (`--cloud gcp`), plus cloud-pinned entrypoints (`awsbnkctl`, `azrbnkctl`, `gkebnkctl`).
- Version-aware BNK release catalog supporting BNK `2.3.x` (`legacy-spk`), `2.4.0`, and `2.4.1` (`gateway-infra`) with automatic release-quirk self-healing (`bnkctl heal`) and cross-train migration (`bnkctl migrate`).
- Turnkey **F5 BNK Forge** (`https://github.com/JLCode-tech/bnk-forge`) and **F5 BNK AI Service Plane** integration (`bnkctl forge register`, `bnkctl forge governance`, `bnkctl test aiperf`) across AWS Bedrock, Azure AI Foundry, and Google Vertex AI.
- 15 built-in L4/L7, egress SNAT, and GenAI validation scenarios (`bnkctl scenarios`).
- Deterministic zero-leak teardown (`bnkctl down`) across AWS, Azure, and GCP with Kubernetes finalizer stripping and orphaned volume/ENI/NIC cleanup.
