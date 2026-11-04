#!/bin/bash
set -euo pipefail

# Cleans up the AgentFoundry Kubernetes demo resources and Azure Private DNS resources.
#
# Usage:
#   ./scripts/teardown-foundry-network.sh [cluster-name] [resource-group]

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEMO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${DEMO_DIR}/../.." && pwd)"

CLUSTER_NAME="${1:-bnk-agentfoundry-demo}"
RG_NAME="${2:-azrbnkctl-${CLUSTER_NAME}-rg}"
BNK_DNS_ZONE="${BNK_DNS_ZONE:-bnk-demo.internal}"

if [[ -z "${KUBECONFIG:-}" ]] && [[ -f "${REPO_ROOT}/.azrbnkctl/state/${CLUSTER_NAME}/kubeconfig" ]]; then
    export KUBECONFIG="${REPO_ROOT}/.azrbnkctl/state/${CLUSTER_NAME}/kubeconfig"
fi

echo "=== Tearing Down Azure Network Seam & Demo Workloads for $CLUSTER_NAME ==="

if command -v kubectl >/dev/null 2>&1 && kubectl --request-timeout=5s cluster-info >/dev/null 2>&1; then
    echo "Removing AgentFoundry Kubernetes manifests..."
    for manifest in \
        "${DEMO_DIR}/k8s/bnk-llm-gateway.yaml" \
        "${DEMO_DIR}/traffic-generator-deployment.yaml" \
        "${DEMO_DIR}/mcp-foundry-token-shipper.yaml" \
        "${DEMO_DIR}/mcp-observability.yaml" \
        "${DEMO_DIR}/mcp-security-policy.yaml" \
        "${DEMO_DIR}/mcp-persistence.yaml" \
        "${DEMO_DIR}/gateway-deployment.yaml" \
        "${DEMO_DIR}/mcp-tool"; do
        if [[ -e "${manifest}" ]]; then
            kubectl delete -f "${manifest}" --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
        fi
    done
    kubectl delete secret bnk-aigw-virtual-keys -n f5-cne-system --ignore-not-found=true >/dev/null 2>&1 || true
fi

if command -v az >/dev/null 2>&1; then
    echo "Deleting Azure Private DNS VNet link ${CLUSTER_NAME}-vnet-link from $BNK_DNS_ZONE..."
    az network private-dns link vnet delete \
        --resource-group "$RG_NAME" \
        --zone-name "$BNK_DNS_ZONE" \
        --name "${CLUSTER_NAME}-vnet-link" \
        --yes -o none 2>/dev/null || true

    echo "Deleting Azure Private DNS Zone $BNK_DNS_ZONE from $RG_NAME..."
    az network private-dns zone delete \
        --resource-group "$RG_NAME" \
        --name "$BNK_DNS_ZONE" \
        --yes -o none 2>/dev/null || true
fi

rm -f "${DEMO_DIR}/.foundry-network-env.json" "${DEMO_DIR}/keys/virtual-keys.env"
echo "✓ Azure AI Foundry network seam teardown complete."
