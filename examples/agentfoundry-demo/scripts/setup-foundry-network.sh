#!/bin/bash
set -euo pipefail

# Provisions the AgentFoundry MCP tool server, BNK Gateway/L7 security policy,
# Loki observability stack, traffic generator, and Azure Private DNS network seam
# in australiaeast.
#
# Usage:
#   KUBECONFIG=<path> ./scripts/setup-foundry-network.sh [cluster-name] [resource-group]

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEMO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${DEMO_DIR}/../.." && pwd)"

CLUSTER_NAME="${1:-bnk-agentfoundry-demo}"
LOCATION="${AZURE_LOCATION:-australiaeast}"
RG_NAME="${2:-azrbnkctl-${CLUSTER_NAME}-rg}"

BNK_DNS_ZONE="${BNK_DNS_ZONE:-bnk-demo.internal}"
BNK_INGRESS_HOST="${BNK_INGRESS_HOST:-bnk-ingress.${BNK_DNS_ZONE}}"
AIGW_VIP="${AIGW_VIP:-10.0.17.101}"

if [ -z "${KUBECONFIG:-}" ]; then
    for _kc in "${REPO_ROOT}/.azrbnkctl/${CLUSTER_NAME}/kubeconfig" \
               "${DEMO_DIR}/.azrbnkctl/${CLUSTER_NAME}/kubeconfig" \
               "${HOME}/.kube/config"; do
        if [ -f "${_kc}" ]; then KUBECONFIG="${_kc}"; break; fi
    done
fi
export KUBECONFIG="${KUBECONFIG:-}"

echo "=== Setting up Azure AgentFoundry Workloads & Network Seam for $CLUSTER_NAME (Region: $LOCATION) ==="

if command -v kubectl >/dev/null 2>&1 && [ -n "${KUBECONFIG}" ] && [ -f "${KUBECONFIG}" ]; then
    echo "[1/5] Deploying MCP Financial Tool server (mcp-tool)..."
    kubectl apply -k "${DEMO_DIR}/mcp-tool"

    echo "[2/5] Deploying BNK Gateway, TLS Certificate, Persistence & L7/L4 Security Policies..."
    kubectl apply -f "${DEMO_DIR}/gateway-deployment.yaml"
    kubectl apply -f "${DEMO_DIR}/mcp-persistence.yaml"
    kubectl apply -f "${DEMO_DIR}/mcp-security-policy.yaml"

    echo "[3/5] Deploying Loki Observability Stack & Azure Foundry Token Shipper (llm-egress)..."
    kubectl apply -f "${DEMO_DIR}/mcp-observability.yaml"
    kubectl apply -f "${DEMO_DIR}/mcp-foundry-token-shipper.yaml"

    # Enable stdout store on f5-toda-fluentd so iRule BNKGOV logs stream to bnkgov-collector -> Loki
    if kubectl get configmap f5-toda-fluentd-custom -n f5-cne-core >/dev/null 2>&1; then
        HAS_STDOUT="$(kubectl get configmap f5-toda-fluentd-custom -n f5-cne-core -o jsonpath='{.data.stdout\.conf}' 2>/dev/null || true)"
        if [ -z "${HAS_STDOUT}" ]; then
            kubectl patch configmap f5-toda-fluentd-custom -n f5-cne-core --type merge \
                -p '{"data":{"stdout.conf":"<store>\n  @type stdout\n</store>\n"}}' >/dev/null 2>&1 || true
            kubectl rollout restart deploy/f5-toda-fluentd -n f5-cne-core >/dev/null 2>&1 || true
        fi
    fi

    echo "[4/5] Deploying continuous dual-leg Traffic Generator (deploy/agentfoundry-traffic-generator)..."
    kubectl apply -f "${DEMO_DIR}/traffic-generator-deployment.yaml"

    echo "Waiting for MCP tool, observability, and traffic generator pods to become Ready..."
    kubectl rollout status deploy/mcp-financial-tool -n default --timeout=180s || true
    kubectl rollout status deploy/loki -n llm-egress --timeout=180s || true
    kubectl rollout status deploy/foundry-token-shipper -n llm-egress --timeout=180s || true
    kubectl rollout status deploy/agentfoundry-traffic-generator -n default --timeout=180s || true
fi

echo "[5/5] Discovering BNK VIP from Gateway bnk-agentfoundry-demo-gateway..."
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

    echo "Adding A-record $BNK_INGRESS_HOST -> $VIP_IP and aigw.$BNK_DNS_ZONE -> $AIGW_VIP..."
    az network private-dns record-set a add-record \
        --resource-group "$RG_NAME" \
        --zone-name "$BNK_DNS_ZONE" \
        --record-set-name "bnk-ingress" \
        --ipv4-address "$VIP_IP" \
        -o none 2>/dev/null || true
    az network private-dns record-set a add-record \
        --resource-group "$RG_NAME" \
        --zone-name "$BNK_DNS_ZONE" \
        --record-set-name "aigw" \
        --ipv4-address "$AIGW_VIP" \
        -o none 2>/dev/null || true
fi

cat <<EOF > "${DEMO_DIR}/.foundry-network-env.json"
{
  "CLUSTER_NAME": "$CLUSTER_NAME",
  "LOCATION": "$LOCATION",
  "RESOURCE_GROUP": "$RG_NAME",
  "VNET_NAME": "$VNET_NAME",
  "BNK_DNS_ZONE": "$BNK_DNS_ZONE",
  "BNK_INGRESS_HOST": "$BNK_INGRESS_HOST",
  "VIP_IP": "$VIP_IP",
  "AIGW_VIP": "$AIGW_VIP"
}
EOF

echo "✓ Azure AI Foundry workloads and network seam setup complete."
echo "  Variables written to ${DEMO_DIR}/.foundry-network-env.json"
