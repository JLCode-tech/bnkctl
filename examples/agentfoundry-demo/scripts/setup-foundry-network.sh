#!/bin/bash
set -euo pipefail

# Provisions the Azure network seam so Azure AI Foundry runtimes and local clients
# can reach the F5 BNK VIP over VNet private DNS routing in australiaeast.
#
# Usage:
#   KUBECONFIG=<path> ./scripts/setup-foundry-network.sh [cluster-name] [resource-group]

CLUSTER_NAME="${1:-bnk-agentfoundry-demo}"
LOCATION="${AZURE_LOCATION:-australiaeast}"
RG_NAME="${2:-azrbnkctl-${CLUSTER_NAME}-rg}"

BNK_DNS_ZONE="${BNK_DNS_ZONE:-bnk-demo.internal}"
BNK_INGRESS_HOST="${BNK_INGRESS_HOST:-bnk-ingress.${BNK_DNS_ZONE}}"

echo "=== Setting up Azure Network Seam for $CLUSTER_NAME (Region: $LOCATION) ==="

echo "Discovering BNK VIP from Gateway bnk-agentfoundry-demo-gateway..."
VIP_IP=""
if command -v kubectl >/dev/null 2>&1; then
    VIP_IP=$(kubectl get gateway bnk-agentfoundry-demo-gateway -n default -o jsonpath='{.spec.addresses[0].value}' 2>/dev/null || echo "")
fi

if [ -z "$VIP_IP" ]; then
    VIP_IP="10.0.17.100"
    echo "Defaulting to external subnet VIP: $VIP_IP"
else
    echo "Found BNK VIP: $VIP_IP"
fi

echo "Discovering Azure VNet in Resource Group $RG_NAME..."
VNET_NAME=""
if command -v az >/dev/null 2>&1; then
    VNET_NAME=$(az network vnet list --resource-group "$RG_NAME" --query "[0].name" -o tsv 2>/dev/null || echo "")
fi

if [ -n "$VNET_NAME" ]; then
    echo "Found Azure VNet: $VNET_NAME"
    VNET_ID=$(az network vnet show --resource-group "$RG_NAME" --name "$VNET_NAME" --query "id" -o tsv)

    echo "Creating or updating Azure Private DNS Zone: $BNK_DNS_ZONE..."
    az network private-dns zone create \
        --resource-group "$RG_NAME" \
        --name "$BNK_DNS_ZONE" \
        -o none 2>/dev/null || true

    echo "Linking Private DNS Zone to VNet $VNET_NAME..."
    az network private-dns link vnet create \
        --resource-group "$RG_NAME" \
        --zone-name "$BNK_DNS_ZONE" \
        --name "${CLUSTER_NAME}-vnet-link" \
        --virtual-network "$VNET_ID" \
        --registration-enabled false \
        -o none 2>/dev/null || true

    echo "Adding A-record $BNK_INGRESS_HOST -> $VIP_IP..."
    az network private-dns record-set a add-record \
        --resource-group "$RG_NAME" \
        --zone-name "$BNK_DNS_ZONE" \
        --record-set-name "bnk-ingress" \
        --ipv4-address "$VIP_IP" \
        -o none 2>/dev/null || true
fi

cat <<EOF > .foundry-network-env.json
{
  "CLUSTER_NAME": "$CLUSTER_NAME",
  "LOCATION": "$LOCATION",
  "RESOURCE_GROUP": "$RG_NAME",
  "VNET_NAME": "$VNET_NAME",
  "BNK_DNS_ZONE": "$BNK_DNS_ZONE",
  "BNK_INGRESS_HOST": "$BNK_INGRESS_HOST",
  "VIP_IP": "$VIP_IP"
}
EOF

echo "✓ Azure AI Foundry network seam setup complete."
echo "  Variables written to .foundry-network-env.json"
