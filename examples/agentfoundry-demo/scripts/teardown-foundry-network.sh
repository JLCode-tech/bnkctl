#!/bin/bash
set -euo pipefail

# Cleans up the Azure Private DNS resources created for the AgentFoundry demo.
#
# Usage:
#   ./scripts/teardown-foundry-network.sh [cluster-name] [resource-group]

CLUSTER_NAME="${1:-bnk-agentfoundry-demo}"
RG_NAME="${2:-azrbnkctl-${CLUSTER_NAME}-rg}"
BNK_DNS_ZONE="${BNK_DNS_ZONE:-bnk-demo.internal}"

echo "=== Tearing Down Azure Network Seam for $CLUSTER_NAME ==="

if command -v az >/dev/null 2>&1; then
    echo "Deleting Azure Private DNS Zone $BNK_DNS_ZONE from $RG_NAME..."
    az network private-dns zone delete \
        --resource-group "$RG_NAME" \
        --name "$BNK_DNS_ZONE" \
        --yes -o none 2>/dev/null || true
fi

rm -f .foundry-network-env.json
echo "✓ Azure AI Foundry network seam teardown complete."
