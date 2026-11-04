#!/bin/bash
set -euo pipefail

# Tears down the AWS network seam created by setup-agentcore-network.sh.
#
# Usage:
#   AWS_PROFILE=<profile> ./scripts/teardown-agentcore-network.sh [cluster-name]
#
# Reads .agentcore-network-env.json if present; otherwise discovers resources by
# name/tag. Idempotent: safe to re-run after teardown.

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CFG="${CFG:-$DEMO_DIR/cluster.yaml}"
CLUSTER_NAME="${1:-bnk-agentcore-demo}"
CFG_REGION=""
if [ -f "$CFG" ]; then
    CFG_REGION=$(awk '/^  region:/{print $2; exit}' "$CFG" 2>/dev/null || true)
fi
REGION="${AWS_REGION:-${CFG_REGION:-$(aws configure get region 2>/dev/null || echo "ap-southeast-2")}}"
export AWS_DEFAULT_REGION="$REGION"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEARDOWN_FAILED=0
if [ -x "$SCRIPT_DIR/teardown-stranger.sh" ]; then
    "$SCRIPT_DIR/teardown-stranger.sh" "$CLUSTER_NAME" || TEARDOWN_FAILED=1
fi

BNK_DNS_ZONE="${BNK_DNS_ZONE:-bnk-demo.internal}"
BNK_INGRESS_HOST="${BNK_INGRESS_HOST:-bnk-ingress.${BNK_DNS_ZONE}}"
VIP_IP=""
AGENT_SG_ID=""
TMM_SG_ID=""
VPC_ID=""

ENV_FILE=""
for candidate in .agentcore-network-env.json "$DEMO_DIR/.agentcore-network-env.json"; do
    if [ -f "$candidate" ]; then
        ENV_FILE="$candidate"
        break
    fi
done

LIVE_VPC_ID=$(aws ec2 describe-vpcs \
    --filters \
        "Name=tag:awsbnkctl:cluster,Values=${CLUSTER_NAME}" \
    --query "Vpcs[0].VpcId" --output text 2>/dev/null || echo "None")

if [ -n "$ENV_FILE" ]; then
    echo "Loading $ENV_FILE..."
    BNK_DNS_ZONE=$(jq -r '.BNK_DNS_ZONE // "bnk-demo.internal"' "$ENV_FILE")
    BNK_INGRESS_HOST=$(jq -r '.BNK_INGRESS_HOST // empty' "$ENV_FILE")
    BNK_INGRESS_HOST="${BNK_INGRESS_HOST:-bnk-ingress.${BNK_DNS_ZONE}}"
    VIP_IP=$(jq -r '.VIP_IP // empty' "$ENV_FILE")
    AGENT_SG_ID=$(jq -r '.AGENT_SG_ID // empty' "$ENV_FILE")
    TMM_SG_ID=$(jq -r '.TMM_SG_ID // empty' "$ENV_FILE")
    VPC_ID=$(jq -r '.VPC_ID // empty' "$ENV_FILE")
fi

# If a live VPC for this cluster exists and differs from any cached VPC_ID,
# or if no env file was present, discover directly from the live VPC.
if [ "$LIVE_VPC_ID" != "None" ] && [ -n "$LIVE_VPC_ID" ] && { [ -z "$VPC_ID" ] || [ "$VPC_ID" != "$LIVE_VPC_ID" ]; }; then
    echo "Discovering resources from live VPC $LIVE_VPC_ID..."
    VPC_ID="$LIVE_VPC_ID"
    AGENT_SG_NAME="${CLUSTER_NAME}-agent"
    AGENT_SG_ID=$(aws ec2 describe-security-groups \
        --filters \
            "Name=group-name,Values=$AGENT_SG_NAME" \
            "Name=vpc-id,Values=$VPC_ID" \
        --query "SecurityGroups[0].GroupId" --output text 2>/dev/null || echo "None")
    TMM_SG_ID=$(aws ec2 describe-security-groups \
        --filters \
            "Name=group-name,Values=${CLUSTER_NAME}-bnk-data,${CLUSTER_NAME}-bnk-datapath-sg" \
            "Name=vpc-id,Values=$VPC_ID" \
        --query "SecurityGroups[0].GroupId" --output text 2>/dev/null || echo "None")
fi

if [ -n "$VPC_ID" ] && [ "$VPC_ID" != "None" ]; then
    VPC_CIDR=$(aws ec2 describe-vpcs \
        --vpc-ids "$VPC_ID" \
        --query "Vpcs[0].CidrBlock" --output text 2>/dev/null || echo "")
    [ "$VPC_CIDR" = "None" ] && VPC_CIDR=""
fi

echo "Looking for Route 53 zone $BNK_DNS_ZONE..."
ZONE_ID=$(aws route53 list-hosted-zones-by-name \
    --dns-name "$BNK_DNS_ZONE" \
    --query "HostedZones[?Name=='${BNK_DNS_ZONE}.'].Id" --output text 2>/dev/null || true)

if [ -n "$ZONE_ID" ] && [ "$ZONE_ID" != "None" ]; then
    for zid in $ZONE_ID; do
        echo "Found zone $zid. Checking for A record $BNK_INGRESS_HOST..."

        RECORD_SET=$(aws route53 list-resource-record-sets \
            --hosted-zone-id "$zid" \
            --query "ResourceRecordSets[?Name=='${BNK_INGRESS_HOST}.' && Type=='A']" --output json 2>/dev/null || echo "[]")

        if [ "$RECORD_SET" != "[]" ] && [ -n "$RECORD_SET" ]; then
            # Prefer the live record's TTL and value so Route 53 exact-match DELETE succeeds.
            TTL=$(echo "$RECORD_SET" | jq -r '.[0].TTL // 60')
            VALUE=$(echo "$RECORD_SET" | jq -r '.[0].ResourceRecords[0].Value // empty')
            RECORD_IP="${VALUE:-$VIP_IP}"
            if [ -n "$RECORD_IP" ] && [ "$RECORD_IP" != "null" ]; then
                echo "Deleting A record for $BNK_INGRESS_HOST ($RECORD_IP)..."
                aws route53 change-resource-record-sets \
                    --hosted-zone-id "$zid" \
                    --change-batch '{
                      "Comment": "Delete BNK Ingress VIP",
                      "Changes": [
                        {
                          "Action": "DELETE",
                          "ResourceRecordSet": {
                            "Name": "'"$BNK_INGRESS_HOST"'",
                            "Type": "A",
                            "TTL": '"$TTL"',
                            "ResourceRecords": [{"Value": "'"$RECORD_IP"'"}]
                          }
                        }
                      ]
                    }' || echo "Failed to delete A record; it may not match exactly."
            else
                echo "Cannot delete A record: VIP_IP unknown."
            fi
        fi

        echo "Deleting hosted zone $zid..."
        if ! aws route53 delete-hosted-zone --id "$zid" 2>/dev/null; then
            if aws route53 get-hosted-zone --id "$zid" >/dev/null 2>&1; then
                echo "Failed to delete hosted zone $zid. Make sure it is empty."
                TEARDOWN_FAILED=1
            else
                echo "Hosted zone $zid already deleted."
            fi
        fi
    done
else
    echo "Route 53 zone $BNK_DNS_ZONE not found. Skipping."
fi

if [ -n "$AGENT_SG_ID" ] && [ "$AGENT_SG_ID" != "None" ] && [ "$AGENT_SG_ID" != "null" ]; then
    if [ -n "${TMM_SG_ID:-}" ] && [ "$TMM_SG_ID" != "None" ] && [ "$TMM_SG_ID" != "null" ]; then
        echo "Revoking Agent SG ($AGENT_SG_ID) references from TMM SG ($TMM_SG_ID)..."
        aws ec2 revoke-security-group-ingress \
            --group-id "$TMM_SG_ID" \
            --ip-permissions \
            "IpProtocol=tcp,FromPort=80,ToPort=80,UserIdGroupPairs=[{GroupId=$AGENT_SG_ID}]" \
            "IpProtocol=tcp,FromPort=443,ToPort=443,UserIdGroupPairs=[{GroupId=$AGENT_SG_ID}]" 2>/dev/null || true
    fi

    if aws ec2 describe-security-groups --group-ids "$AGENT_SG_ID" >/dev/null 2>&1; then
        if [ -z "${VPC_CIDR:-}" ]; then
            SG_VPC_ID=$(aws ec2 describe-security-groups --group-ids "$AGENT_SG_ID" --query "SecurityGroups[0].VpcId" --output text 2>/dev/null || echo "")
            if [ -n "$SG_VPC_ID" ] && [ "$SG_VPC_ID" != "None" ]; then
                VPC_CIDR=$(aws ec2 describe-vpcs --vpc-ids "$SG_VPC_ID" --query "Vpcs[0].CidrBlock" --output text 2>/dev/null || echo "")
            fi
        fi
        if [ -n "${VPC_CIDR:-}" ] && [ "$VPC_CIDR" != "None" ]; then
            echo "Revoking ingress from VPC CIDR ($VPC_CIDR) to Agent SG ($AGENT_SG_ID) on port 8080..."
            aws ec2 revoke-security-group-ingress \
                --group-id "$AGENT_SG_ID" \
                --protocol tcp --port 8080 --cidr "$VPC_CIDR" 2>/dev/null || echo "Ingress rule already removed or does not exist."
        fi

        echo "Deleting Agent SG $AGENT_SG_ID..."
        if ! aws ec2 delete-security-group --group-id "$AGENT_SG_ID" 2>/dev/null; then
            if aws ec2 describe-security-groups --group-ids "$AGENT_SG_ID" >/dev/null 2>&1; then
                echo "Warning: Failed to delete SG $AGENT_SG_ID. Ensure AgentCore is torn down first."
                TEARDOWN_FAILED=1
            else
                echo "Agent SG $AGENT_SG_ID already deleted."
            fi
        fi
    else
        echo "Agent SG $AGENT_SG_ID already deleted. Skipping."
    fi
else
    echo "Agent SG not found. Skipping."
fi

if [ "$TEARDOWN_FAILED" -eq 0 ]; then
    for env_candidate in .agentcore-network-env.json "$DEMO_DIR/.agentcore-network-env.json"; do
        if [ -f "$env_candidate" ]; then
            rm -f "$env_candidate"
            echo "Deleted $env_candidate"
        fi
    done
fi

# Undo what setup-agentcore-network.sh attached for demo.sh; awsbnkctl's own
# jumphost access (EICE) does not need it.
echo "Detaching AmazonSSMManagedInstanceCore from ${CLUSTER_NAME}-jumphost-role..."
aws iam detach-role-policy \
    --role-name "${CLUSTER_NAME}-jumphost-role" \
    --policy-arn "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore" 2>/dev/null \
    || echo "  (policy was not attached)"

if [ "$TEARDOWN_FAILED" -ne 0 ]; then
    echo "Teardown finished with errors; keeping .agentcore-network-env.json for retry."
    exit 1
fi

echo "Teardown complete."
