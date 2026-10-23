#!/bin/bash
set -euo pipefail

# Tears down the AWS network seam created by setup-agentcore-network.sh.
#
# Usage:
#   AWS_PROFILE=<profile> ./scripts/teardown-agentcore-network.sh [cluster-name]
#
# Reads .agentcore-network-env.json if present; otherwise discovers resources by
# name/tag. Idempotent: safe to re-run after teardown.

CLUSTER_NAME="${1:-bnk-agentcore-demo}"
REGION="${AWS_REGION:-$(aws configure get region 2>/dev/null || echo "ap-southeast-2")}"
export AWS_DEFAULT_REGION="$REGION"

BNK_DNS_ZONE="${BNK_DNS_ZONE:-bnk-demo.internal}"
BNK_INGRESS_HOST="${BNK_INGRESS_HOST:-bnk-ingress.${BNK_DNS_ZONE}}"
VIP_IP=""
AGENT_SG_ID=""

if [ -f .agentcore-network-env.json ]; then
    echo "Loading .agentcore-network-env.json..."
    BNK_DNS_ZONE=$(jq -r '.BNK_DNS_ZONE' .agentcore-network-env.json)
    BNK_INGRESS_HOST=$(jq -r '.BNK_INGRESS_HOST' .agentcore-network-env.json)
    VIP_IP=$(jq -r '.VIP_IP' .agentcore-network-env.json)
    AGENT_SG_ID=$(jq -r '.AGENT_SG_ID' .agentcore-network-env.json)
else
    echo "No .agentcore-network-env.json found. Discovering resources by name..."

    VPC_ID=$(aws ec2 describe-vpcs \
        --filters \
            "Name=tag:awsbnkctl:cluster,Values=${CLUSTER_NAME}" \
            "Name=tag:awsbnkctl:component,Values=vpc" \
        --query "Vpcs[0].VpcId" --output text 2>/dev/null || echo "None")

    if [ "$VPC_ID" != "None" ] && [ -n "$VPC_ID" ]; then
        VPC_CIDR=$(aws ec2 describe-vpcs \
            --vpc-ids "$VPC_ID" \
            --query "Vpcs[0].CidrBlock" --output text)
        AGENT_SG_NAME="${CLUSTER_NAME}-agent"
        AGENT_SG_ID=$(aws ec2 describe-security-groups \
            --filters \
                "Name=group-name,Values=$AGENT_SG_NAME" \
                "Name=vpc-id,Values=$VPC_ID" \
            --query "SecurityGroups[0].GroupId" --output text 2>/dev/null || echo "None")
    fi
fi

echo "Looking for Route 53 zone $BNK_DNS_ZONE..."
ZONE_ID=$(aws route53 list-hosted-zones-by-name \
    --dns-name "$BNK_DNS_ZONE" \
    --query "HostedZones[?Name=='${BNK_DNS_ZONE}.'].Id" --output text 2>/dev/null || true)

if [ -n "$ZONE_ID" ] && [ "$ZONE_ID" != "None" ]; then
    echo "Found zone $ZONE_ID. Checking for A record $BNK_INGRESS_HOST..."

    RECORD_SET=$(aws route53 list-resource-record-sets \
        --hosted-zone-id "$ZONE_ID" \
        --query "ResourceRecordSets[?Name=='${BNK_INGRESS_HOST}.' && Type=='A']" --output json 2>/dev/null || echo "[]")

    if [ "$RECORD_SET" != "[]" ] && [ -n "$RECORD_SET" ]; then
        # Use the live record's TTL and value if VIP_IP is unknown.
        TTL=$(echo "$RECORD_SET" | jq -r '.[0].TTL // 60')
        VALUE=$(echo "$RECORD_SET" | jq -r '.[0].ResourceRecords[0].Value // empty')
        if [ -z "$VIP_IP" ] || [ "$VIP_IP" == "null" ]; then
            VIP_IP="$VALUE"
        fi
        if [ -n "$VIP_IP" ] && [ "$VIP_IP" != "null" ]; then
            echo "Deleting A record for $BNK_INGRESS_HOST..."
            aws route53 change-resource-record-sets \
                --hosted-zone-id "$ZONE_ID" \
                --change-batch '{
                  "Comment": "Delete BNK Ingress VIP",
                  "Changes": [
                    {
                      "Action": "DELETE",
                      "ResourceRecordSet": {
                        "Name": "'"$BNK_INGRESS_HOST"'",
                        "Type": "A",
                        "TTL": '"$TTL"',
                        "ResourceRecords": [{"Value": "'"$VIP_IP"'"}]
                      }
                    }
                  ]
                }' || echo "Failed to delete A record; it may not match exactly."
        else
            echo "Cannot delete A record: VIP_IP unknown."
        fi
    fi

    echo "Deleting hosted zone $ZONE_ID..."
    aws route53 delete-hosted-zone --id "$ZONE_ID" || echo "Failed to delete hosted zone. Make sure it is empty."
else
    echo "Route 53 zone $BNK_DNS_ZONE not found. Skipping."
fi

if [ -n "$AGENT_SG_ID" ] && [ "$AGENT_SG_ID" != "None" ]; then
    # We might not have discovered VPC_CIDR if .agentcore-network-env.json was used, so fetch it if needed.
    if [ -z "${VPC_CIDR:-}" ]; then
        # Try to infer VPC ID from SG if we don't have it.
        VPC_ID=$(aws ec2 describe-security-groups --group-ids "$AGENT_SG_ID" --query "SecurityGroups[0].VpcId" --output text 2>/dev/null || echo "")
        if [ -n "$VPC_ID" ]; then
            VPC_CIDR=$(aws ec2 describe-vpcs --vpc-ids "$VPC_ID" --query "Vpcs[0].CidrBlock" --output text 2>/dev/null || echo "")
        fi
    fi
    if [ -n "${VPC_CIDR:-}" ]; then
        echo "Revoking ingress from VPC CIDR ($VPC_CIDR) to Agent SG ($AGENT_SG_ID) on port 8080..."
        aws ec2 revoke-security-group-ingress \
            --group-id "$AGENT_SG_ID" \
            --protocol tcp --port 8080 --cidr "$VPC_CIDR" 2>/dev/null || echo "Ingress rule already removed or does not exist."
    fi

    echo "Deleting Agent SG $AGENT_SG_ID..."
    aws ec2 delete-security-group --group-id "$AGENT_SG_ID" || echo "Warning: Failed to delete SG $AGENT_SG_ID. Ensure AgentCore is torn down first."
else
    echo "Agent SG not found. Skipping."
fi

if [ -f .agentcore-network-env.json ]; then
    rm .agentcore-network-env.json
    echo "Deleted .agentcore-network-env.json"
fi

# Undo what setup-agentcore-network.sh attached for demo.sh; awsbnkctl's own
# jumphost access (EICE) does not need it.
echo "Detaching AmazonSSMManagedInstanceCore from ${CLUSTER_NAME}-jumphost-role..."
aws iam detach-role-policy \
    --role-name "${CLUSTER_NAME}-jumphost-role" \
    --policy-arn "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore" 2>/dev/null \
    || echo "  (policy was not attached)"

echo "Teardown complete."
