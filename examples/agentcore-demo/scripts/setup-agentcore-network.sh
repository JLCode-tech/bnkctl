#!/bin/bash
set -euo pipefail

# Provisions the AWS network seam so an external AgentCore runtime can reach
# the BNK VIP over VPC/ENI routing.
#
# Usage:
#   AWS_PROFILE=<profile> KUBECONFIG=<path> ./scripts/setup-agentcore-network.sh [cluster-name]
#
# The cluster name defaults to "bnk-agentcore-demo". All AWS resource IDs are
# discovered at runtime from tags or from the live Gateway object. Nothing is
# hardcoded.

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER_NAME="${1:-bnk-agentcore-demo}"
CFG="${CFG:-$DEMO_DIR/cluster.yaml}"
# The demo Gateway in gateway-deployment.yaml is named <cluster>-gateway in the
# default namespace; override both if you renamed it.
# The Gateway name is fixed in gateway-deployment.yaml, whatever the cluster is called.
GATEWAY_NAME="${GATEWAY_NAME:-bnk-agentcore-demo-gateway}"
GATEWAY_NS="${GATEWAY_NS:-default}"
CFG_REGION=""
if [ -f "$CFG" ]; then
    CFG_REGION=$(awk '/^  region:/{print $2; exit}' "$CFG" 2>/dev/null || true)
fi
REGION="${AWS_REGION:-${CFG_REGION:-$(aws configure get region 2>/dev/null || echo "ap-southeast-2")}}"
export AWS_DEFAULT_REGION="$REGION"

BNK_DNS_ZONE="${BNK_DNS_ZONE:-bnk-demo.internal}"
BNK_INGRESS_HOST="${BNK_INGRESS_HOST:-bnk-ingress.${BNK_DNS_ZONE}}"

KUBECONFIG="${KUBECONFIG:-}"
if [ -n "$KUBECONFIG" ]; then
    export KUBECONFIG
fi

echo "Discovering VPC ID for cluster $CLUSTER_NAME..."
VPC_ID=$(aws ec2 describe-vpcs \
    --filters \
        "Name=tag:awsbnkctl:cluster,Values=${CLUSTER_NAME}" \
    --query "Vpcs[0].VpcId" --output text)
if [ "$VPC_ID" == "None" ] || [ -z "$VPC_ID" ]; then
    echo "Could not find VPC for cluster $CLUSTER_NAME"
    exit 1
fi
echo "Found VPC: $VPC_ID"

echo "Discovering VPC CIDR..."
VPC_CIDR=$(aws ec2 describe-vpcs \
    --vpc-ids "$VPC_ID" \
    --query "Vpcs[0].CidrBlock" --output text)
echo "Found VPC CIDR: $VPC_CIDR"

echo "Discovering BNK VIP from Gateway $GATEWAY_NS/$GATEWAY_NAME..."
if ! VIP_IP=$(kubectl get gateway "$GATEWAY_NAME" -n "$GATEWAY_NS" -o jsonpath='{.spec.addresses[0].value}' 2>/dev/null); then
    echo "Failed to read Gateway. Is KUBECONFIG set and pointing at the right cluster?"
    exit 1
fi
if [ -z "$VIP_IP" ]; then
    echo "Gateway $GATEWAY_NS/$GATEWAY_NAME has no spec.addresses[0].value"
    exit 1
fi
echo "Found BNK VIP: $VIP_IP"

echo "Discovering private subnets for the AgentCore runtime..."
AGENT_SUBNETS_JSON=$(aws ec2 describe-subnets \
    --filters \
        "Name=vpc-id,Values=$VPC_ID" \
        "Name=tag:awsbnkctl:cluster,Values=$CLUSTER_NAME" \
        "Name=tag:awsbnkctl:component,Values=subnet-private" \
    --query 'sort_by(Subnets, &AvailabilityZone)[].SubnetId' --output json | jq -c .)
if [ "$AGENT_SUBNETS_JSON" == "[]" ] || [ -z "$AGENT_SUBNETS_JSON" ]; then
    AGENT_SUBNETS_JSON=$(aws ec2 describe-subnets \
        --filters \
            "Name=vpc-id,Values=$VPC_ID" \
            "Name=tag:awsbnkctl:cluster,Values=$CLUSTER_NAME" \
            "Name=tag:kubernetes.io/role/internal-elb,Values=1" \
        --query 'sort_by(Subnets, &AvailabilityZone)[].SubnetId' --output json | jq -c .)
fi
if [ "$AGENT_SUBNETS_JSON" == "[]" ] || [ -z "$AGENT_SUBNETS_JSON" ]; then
    echo "Could not find private subnets for cluster $CLUSTER_NAME"
    exit 1
fi
echo "Found private subnets: $AGENT_SUBNETS_JSON"

AGENT_SG_NAME="${CLUSTER_NAME}-agent"
echo "Checking for existing Agent SG ($AGENT_SG_NAME)..."
AGENT_SG_ID=$(aws ec2 describe-security-groups \
    --filters \
        "Name=group-name,Values=$AGENT_SG_NAME" \
        "Name=vpc-id,Values=$VPC_ID" \
    --query "SecurityGroups[0].GroupId" --output text 2>/dev/null || echo "None")

if [ "$AGENT_SG_ID" == "None" ] || [ -z "$AGENT_SG_ID" ]; then
    echo "Creating Agent SG..."
    AGENT_SG_ID=$(aws ec2 create-security-group \
        --group-name "$AGENT_SG_NAME" \
        --description "AgentCore Runtime SG for $CLUSTER_NAME" \
        --vpc-id "$VPC_ID" \
        --query "GroupId" --output text)
    aws ec2 create-tags --resources "$AGENT_SG_ID" \
        --tags Key=Name,Value="$AGENT_SG_NAME" Key=awsbnkctl:cluster,Value="$CLUSTER_NAME" Key=awsbnkctl:managed,Value=true
    echo "Created Agent SG: $AGENT_SG_ID"
else
    echo "Found existing Agent SG: $AGENT_SG_ID"
fi

# demo.sh drives the jumphost with SSM Run Command. awsbnkctl itself reaches
# the jumphost over EICE and does not attach this policy (phase 17b, AWS-17);
# the demo adds it here and teardown-agentcore-network.sh removes it again.
echo "Attaching AmazonSSMManagedInstanceCore to ${CLUSTER_NAME}-jumphost-role (demo.sh runs over SSM)..."
aws iam attach-role-policy \
    --role-name "${CLUSTER_NAME}-jumphost-role" \
    --policy-arn "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore" 2>/dev/null \
    || echo "Warning: ${CLUSTER_NAME}-jumphost-role not present (skipping SSM policy attach)"

echo "Authorizing ingress from VPC CIDR ($VPC_CIDR) to Agent SG ($AGENT_SG_ID) on port 8080..."
aws ec2 authorize-security-group-ingress \
    --group-id "$AGENT_SG_ID" \
    --protocol tcp --port 8080 --cidr "$VPC_CIDR" 2>/dev/null || echo "Ingress rules already exist."

TMM_SG_NAME="${CLUSTER_NAME}-bnk-data"
echo "Discovering TMM external ENI SG ($TMM_SG_NAME / ${CLUSTER_NAME}-bnk-datapath-sg)..."
TMM_SG_ID=$(aws ec2 describe-security-groups \
    --filters \
        "Name=group-name,Values=${CLUSTER_NAME}-bnk-data,${CLUSTER_NAME}-bnk-datapath-sg" \
        "Name=vpc-id,Values=$VPC_ID" \
    --query "SecurityGroups[0].GroupId" --output text || echo "None")

if [ "$TMM_SG_ID" == "None" ] || [ -z "$TMM_SG_ID" ]; then
    echo "Could not find TMM SG $TMM_SG_NAME"
    exit 1
fi
echo "Found TMM SG: $TMM_SG_ID"

echo "Authorizing ingress from Agent SG ($AGENT_SG_ID) and VPC CIDR ($VPC_CIDR) to TMM SG ($TMM_SG_ID)..."
aws ec2 authorize-security-group-ingress \
    --group-id "$TMM_SG_ID" \
    --ip-permissions \
    "IpProtocol=tcp,FromPort=80,ToPort=80,UserIdGroupPairs=[{GroupId=$AGENT_SG_ID,Description=agentcore-to-bnk-vip-http}]" \
    "IpProtocol=tcp,FromPort=443,ToPort=443,UserIdGroupPairs=[{GroupId=$AGENT_SG_ID,Description=agentcore-to-bnk-vip-https}]" \
    "IpProtocol=-1,IpRanges=[{CidrIp=$VPC_CIDR,Description=vpc-internal-to-bnk-tmm}]" 2>/dev/null || echo "Ingress rules already exist."

BNK_NS="${BNK_NS:-f5-cne-system}"
DISC_SVC_CIDR=$(aws eks describe-cluster --name "$CLUSTER_NAME" --query 'cluster.kubernetesNetworkConfig.serviceIpv4Cidr' --output text 2>/dev/null || true)
if [ -z "$DISC_SVC_CIDR" ] || [ "$DISC_SVC_CIDR" == "None" ]; then
    DISC_SVC_CIDR="172.20.0.0/16"
fi
SERVICE_CIDR="${SERVICE_CIDR:-$DISC_SVC_CIDR}"

echo "Discovering BNK worker node and data-path ENIs in VPC $VPC_ID..."
EXT_ENI_ID=$(aws ec2 describe-network-interfaces \
    --filters \
        "Name=vpc-id,Values=$VPC_ID" \
        "Name=tag:awsbnkctl:cluster,Values=$CLUSTER_NAME" \
        "Name=tag:bnk.f5.com/datapath,Values=external" \
    --query "NetworkInterfaces[0].NetworkInterfaceId" --output text 2>/dev/null || echo "None")
INT_ENI_ID=$(aws ec2 describe-network-interfaces \
    --filters \
        "Name=vpc-id,Values=$VPC_ID" \
        "Name=tag:awsbnkctl:cluster,Values=$CLUSTER_NAME" \
        "Name=tag:bnk.f5.com/datapath,Values=internal" \
    --query "NetworkInterfaces[0].NetworkInterfaceId" --output text 2>/dev/null || echo "None")

BNK_INSTANCE_ID="None"
if [ -n "$EXT_ENI_ID" ] && [ "$EXT_ENI_ID" != "None" ]; then
    BNK_INSTANCE_ID=$(aws ec2 describe-network-interfaces \
        --network-interface-ids "$EXT_ENI_ID" \
        --query "NetworkInterfaces[0].Attachment.InstanceId" --output text 2>/dev/null || echo "None")
fi
if [ -z "$BNK_INSTANCE_ID" ] || [ "$BNK_INSTANCE_ID" == "None" ]; then
    K8S_PROVIDER_ID=$(kubectl get nodes -l role=bnk -o jsonpath='{.items[0].spec.providerID}' 2>/dev/null || true)
    if [ -n "$K8S_PROVIDER_ID" ]; then
        BNK_INSTANCE_ID="${K8S_PROVIDER_ID##*/}"
    fi
fi
if [ -z "$BNK_INSTANCE_ID" ] || [ "$BNK_INSTANCE_ID" == "None" ]; then
    BNK_INSTANCE_ID=$(aws ec2 describe-instances \
        --filters \
            "Name=vpc-id,Values=$VPC_ID" \
            "Name=instance-state-name,Values=running" \
        --query "Reservations[].Instances[?length(NetworkInterfaces) >= \`3\` && !contains(Tags[?Key=='Name'].Value | [0] || '', 'jumphost') && !contains(Tags[?Key=='Name'].Value | [0] || '', 'stranger')][].InstanceId | [0]" --output text 2>/dev/null || echo "None")
fi

if [ -n "$BNK_INSTANCE_ID" ] && [ "$BNK_INSTANCE_ID" != "None" ]; then
    echo "Found BNK worker instance: $BNK_INSTANCE_ID"
    PRIMARY_ENI_ID=$(aws ec2 describe-instances --instance-ids "$BNK_INSTANCE_ID" \
        --query "Reservations[0].Instances[0].NetworkInterfaces[?Attachment.DeviceIndex==\`0\`].NetworkInterfaceId | [0]" --output text)

    EXT_SUBNET_ID=$(aws ec2 describe-subnets \
        --filters "Name=vpc-id,Values=$VPC_ID" "Name=tag:awsbnkctl:cluster,Values=$CLUSTER_NAME" "Name=tag:awsbnkctl:component,Values=subnet-bnk-ext" \
        --query "Subnets[0].SubnetId" --output text 2>/dev/null || echo "None")
    INT_SUBNET_ID=$(aws ec2 describe-subnets \
        --filters "Name=vpc-id,Values=$VPC_ID" "Name=tag:awsbnkctl:cluster,Values=$CLUSTER_NAME" "Name=tag:awsbnkctl:component,Values=subnet-bnk-int" \
        --query "Subnets[0].SubnetId" --output text 2>/dev/null || echo "None")

    if { [ -z "$EXT_ENI_ID" ] || [ "$EXT_ENI_ID" == "None" ]; } && [ -n "$EXT_SUBNET_ID" ] && [ "$EXT_SUBNET_ID" != "None" ]; then
        EXT_ENI_ID=$(aws ec2 describe-instances --instance-ids "$BNK_INSTANCE_ID" \
            --query "Reservations[0].Instances[0].NetworkInterfaces[?SubnetId=='$EXT_SUBNET_ID'].NetworkInterfaceId | [0]" --output text)
    fi
    if { [ -z "$INT_ENI_ID" ] || [ "$INT_ENI_ID" == "None" ]; } && [ -n "$INT_SUBNET_ID" ] && [ "$INT_SUBNET_ID" != "None" ]; then
        INT_ENI_ID=$(aws ec2 describe-instances --instance-ids "$BNK_INSTANCE_ID" \
            --query "Reservations[0].Instances[0].NetworkInterfaces[?SubnetId=='$INT_SUBNET_ID'].NetworkInterfaceId | [0]" --output text)
    fi

    EXT_SUBNET_CIDR=""
    if [ -n "$EXT_ENI_ID" ] && [ "$EXT_ENI_ID" != "None" ]; then
        EXT_ENI_SUBNET=$(aws ec2 describe-network-interfaces --network-interface-ids "$EXT_ENI_ID" --query "NetworkInterfaces[0].SubnetId" --output text 2>/dev/null || echo "")
        if [ -n "$EXT_ENI_SUBNET" ] && [ "$EXT_ENI_SUBNET" != "None" ]; then
            EXT_SUBNET_CIDR=$(aws ec2 describe-subnets --subnet-ids "$EXT_ENI_SUBNET" --query "Subnets[0].CidrBlock" --output text 2>/dev/null || echo "")
        fi
    fi
    EXT_SUBNET_CIDR="${EXT_SUBNET_CIDR:-10.0.10.0/24}"
    EXT_PREFIX="${EXT_SUBNET_CIDR%.*}"

    INT_SUBNET_CIDR=""
    if [ -n "$INT_ENI_ID" ] && [ "$INT_ENI_ID" != "None" ]; then
        INT_ENI_SUBNET=$(aws ec2 describe-network-interfaces --network-interface-ids "$INT_ENI_ID" --query "NetworkInterfaces[0].SubnetId" --output text 2>/dev/null || echo "")
        if [ -n "$INT_ENI_SUBNET" ] && [ "$INT_ENI_SUBNET" != "None" ]; then
            INT_SUBNET_CIDR=$(aws ec2 describe-subnets --subnet-ids "$INT_ENI_SUBNET" --query "Subnets[0].CidrBlock" --output text 2>/dev/null || echo "")
        fi
    fi
    INT_SUBNET_CIDR="${INT_SUBNET_CIDR:-10.0.20.0/24}"
    INT_PREFIX="${INT_SUBNET_CIDR%.*}"

    DISC_AIGW_VIP=$(kubectl get gateway aigw-gateway -n "$BNK_NS" -o jsonpath='{.spec.addresses[0].value}' 2>/dev/null || true)
    AIGW_VIP="${AIGW_VIP:-${DISC_AIGW_VIP:-${EXT_PREFIX}.151}}"

    for ENI_ID in "$PRIMARY_ENI_ID" "$EXT_ENI_ID" "$INT_ENI_ID"; do
        if [ -n "$ENI_ID" ] && [ "$ENI_ID" != "None" ]; then
            echo "Disabling SourceDestCheck on ENI $ENI_ID..."
            aws ec2 modify-network-interface-attribute --network-interface-id "$ENI_ID" --no-source-dest-check || true
        fi
    done

    if [ -n "$EXT_ENI_ID" ] && [ "$EXT_ENI_ID" != "None" ]; then
        EXT_VLAN_IPS=$(kubectl get f5-big-net-vlan external-vlan -n "$BNK_NS" -o jsonpath='{.spec.interfaces[0].selfIps[*]}' 2>/dev/null | tr ' ' '\n' | sed 's|/.*||' | tr '\n' ' ' || true)
        echo "Assigning secondary private VIPs ($VIP_IP, $AIGW_VIP) and SelfIPs onto external TMM ENI $EXT_ENI_ID..."
        for IP in "$VIP_IP" "$AIGW_VIP" $EXT_VLAN_IPS "${EXT_PREFIX}.10" "${EXT_PREFIX}.11" "${EXT_PREFIX}.12"; do
            [ -n "$IP" ] || continue
            aws ec2 assign-private-ip-addresses \
                --network-interface-id "$EXT_ENI_ID" \
                --private-ip-addresses "$IP" \
                --allow-reassignment 2>/dev/null || true
        done
    fi

    if [ -n "$INT_ENI_ID" ] && [ "$INT_ENI_ID" != "None" ]; then
        INT_VLAN_IPS=$(kubectl get f5-big-net-vlan internal-vlan -n "$BNK_NS" -o jsonpath='{.spec.interfaces[0].selfIps[*]}' 2>/dev/null | tr ' ' '\n' | sed 's|/.*||' | tr '\n' ' ' || true)
        echo "Ensuring internal SelfIPs are assigned onto internal TMM ENI $INT_ENI_ID..."
        for IP in $INT_VLAN_IPS "${INT_PREFIX}.10" "${INT_PREFIX}.11" "${INT_PREFIX}.12"; do
            [ -n "$IP" ] || continue
            aws ec2 assign-private-ip-addresses \
                --network-interface-id "$INT_ENI_ID" \
                --private-ip-addresses "$IP" \
                --allow-reassignment 2>/dev/null || true
        done
    fi

    # Program VPC route tables:
    # 1. Private worker subnet RTBs & int-subnet RTB: /32 VIP routes -> EXT_ENI_ID
    # 2. ext-subnet RTB: 0.0.0.0/0 -> NAT Gateway
    # 3. ext-subnet RTB & int-subnet RTB: $SERVICE_CIDR -> PRIMARY_ENI_ID
    NAT_GW_ID=$(aws ec2 describe-nat-gateways \
        --filter "Name=vpc-id,Values=$VPC_ID" "Name=state,Values=available" \
        --query "NatGateways[0].NatGatewayId" --output text 2>/dev/null || echo "None")
    EXT_RT_ID=$(aws ec2 describe-route-tables \
        --filters "Name=vpc-id,Values=$VPC_ID" "Name=tag:awsbnkctl:cluster,Values=$CLUSTER_NAME" "Name=tag:awsbnkctl:component,Values=rt-bnk-ext" \
        --query "RouteTables[0].RouteTableId" --output text 2>/dev/null || echo "None")
    if [ -z "$EXT_RT_ID" ] || [ "$EXT_RT_ID" == "None" ]; then
        EXT_RT_ID=$(aws ec2 describe-route-tables \
            --filters "Name=vpc-id,Values=$VPC_ID" "Name=tag:Name,Values=${CLUSTER_NAME}-bnk-ext-rt" \
            --query "RouteTables[0].RouteTableId" --output text 2>/dev/null || echo "None")
    fi
    INT_RT_ID=$(aws ec2 describe-route-tables \
        --filters "Name=vpc-id,Values=$VPC_ID" "Name=tag:awsbnkctl:cluster,Values=$CLUSTER_NAME" "Name=tag:awsbnkctl:component,Values=rt-bnk-int" \
        --query "RouteTables[0].RouteTableId" --output text 2>/dev/null || echo "None")
    if [ -z "$INT_RT_ID" ] || [ "$INT_RT_ID" == "None" ]; then
        INT_RT_ID=$(aws ec2 describe-route-tables \
            --filters "Name=vpc-id,Values=$VPC_ID" "Name=tag:Name,Values=${CLUSTER_NAME}-bnk-int-rt" \
            --query "RouteTables[0].RouteTableId" --output text 2>/dev/null || echo "None")
    fi
    PRIV_RT_IDS=$(aws ec2 describe-route-tables \
        --filters "Name=vpc-id,Values=$VPC_ID" "Name=tag:awsbnkctl:cluster,Values=$CLUSTER_NAME" "Name=tag:awsbnkctl:component,Values=rt-private" \
        --query "RouteTables[].RouteTableId" --output text 2>/dev/null || echo "")
    if [ -z "$PRIV_RT_IDS" ]; then
        PRIV_RT_IDS=$(aws ec2 describe-route-tables \
            --filters "Name=vpc-id,Values=$VPC_ID" "Name=tag:Name,Values=${CLUSTER_NAME}-private-rt" \
            --query "RouteTables[].RouteTableId" --output text 2>/dev/null || echo "")
    fi

    if [ -n "$EXT_RT_ID" ] && [ "$EXT_RT_ID" != "None" ]; then
        if [ -n "$NAT_GW_ID" ] && [ "$NAT_GW_ID" != "None" ]; then
            aws ec2 replace-route --route-table-id "$EXT_RT_ID" --destination-cidr-block "0.0.0.0/0" --nat-gateway-id "$NAT_GW_ID" 2>/dev/null || \
            aws ec2 create-route --route-table-id "$EXT_RT_ID" --destination-cidr-block "0.0.0.0/0" --nat-gateway-id "$NAT_GW_ID" 2>/dev/null || true
        fi
        if [ -n "$PRIMARY_ENI_ID" ] && [ "$PRIMARY_ENI_ID" != "None" ]; then
            aws ec2 replace-route --route-table-id "$EXT_RT_ID" --destination-cidr-block "$SERVICE_CIDR" --network-interface-id "$PRIMARY_ENI_ID" 2>/dev/null || \
            aws ec2 create-route --route-table-id "$EXT_RT_ID" --destination-cidr-block "$SERVICE_CIDR" --network-interface-id "$PRIMARY_ENI_ID" 2>/dev/null || true
        fi
    fi

    if [ -n "$INT_RT_ID" ] && [ "$INT_RT_ID" != "None" ]; then
        if [ -n "$PRIMARY_ENI_ID" ] && [ "$PRIMARY_ENI_ID" != "None" ]; then
            aws ec2 replace-route --route-table-id "$INT_RT_ID" --destination-cidr-block "$SERVICE_CIDR" --network-interface-id "$PRIMARY_ENI_ID" 2>/dev/null || \
            aws ec2 create-route --route-table-id "$INT_RT_ID" --destination-cidr-block "$SERVICE_CIDR" --network-interface-id "$PRIMARY_ENI_ID" 2>/dev/null || true
        fi
        if [ -n "$EXT_ENI_ID" ] && [ "$EXT_ENI_ID" != "None" ]; then
            for VIP_CIDR in "${VIP_IP}/32" "${AIGW_VIP}/32"; do
                aws ec2 replace-route --route-table-id "$INT_RT_ID" --destination-cidr-block "$VIP_CIDR" --network-interface-id "$EXT_ENI_ID" 2>/dev/null || \
                aws ec2 create-route --route-table-id "$INT_RT_ID" --destination-cidr-block "$VIP_CIDR" --network-interface-id "$EXT_ENI_ID" 2>/dev/null || true
            done
        fi
    fi

    if [ -n "$EXT_ENI_ID" ] && [ "$EXT_ENI_ID" != "None" ]; then
        for RT_ID in $PRIV_RT_IDS; do
            for VIP_CIDR in "${VIP_IP}/32" "${AIGW_VIP}/32"; do
                aws ec2 replace-route --route-table-id "$RT_ID" --destination-cidr-block "$VIP_CIDR" --network-interface-id "$EXT_ENI_ID" 2>/dev/null || \
                aws ec2 create-route --route-table-id "$RT_ID" --destination-cidr-block "$VIP_CIDR" --network-interface-id "$EXT_ENI_ID" 2>/dev/null || true
            done
        done
    fi

    # Dynamically bring up all PCI NICs, enable liberal conntrack, and program iptables Service CIDR NAT
    aws ssm send-command \
        --instance-ids "$BNK_INSTANCE_ID" \
        --document-name "AWS-RunShellScript" \
        --parameters "commands=[
          \"for d in /sys/class/net/*; do [ -e \\\"\\\$d/device\\\" ] || continue; dev=\\\$(basename \\\"\\\$d\\\"); ip link set \\\"\\\$dev\\\" up 2>/dev/null || true; ethtool -K \\\"\\\$dev\\\" tso off gso off gro off tx-nocache-copy off 2>/dev/null || true; done\",
          \"sysctl -w net.ipv4.ip_forward=1\",
          \"sysctl -w net.netfilter.nf_conntrack_tcp_be_liberal=1 2>/dev/null || true\",
          \"iptables -C FORWARD -m conntrack --ctstate INVALID -j ACCEPT 2>/dev/null || iptables -I FORWARD 1 -m conntrack --ctstate INVALID -j ACCEPT\",
          \"iptables -t nat -C PREROUTING -s ${EXT_SUBNET_CIDR} -d ${SERVICE_CIDR} -j KUBE-SERVICES 2>/dev/null || iptables -t nat -I PREROUTING 1 -s ${EXT_SUBNET_CIDR} -d ${SERVICE_CIDR} -j KUBE-SERVICES\",
          \"iptables -t nat -C PREROUTING -s ${INT_SUBNET_CIDR} -d ${SERVICE_CIDR} -j KUBE-SERVICES 2>/dev/null || iptables -t nat -I PREROUTING 1 -s ${INT_SUBNET_CIDR} -d ${SERVICE_CIDR} -j KUBE-SERVICES\",
          \"iptables -t nat -C POSTROUTING -s ${EXT_SUBNET_CIDR} -j MASQUERADE 2>/dev/null || iptables -t nat -A POSTROUTING -s ${EXT_SUBNET_CIDR} -j MASQUERADE\",
          \"iptables -t nat -C POSTROUTING -s ${INT_SUBNET_CIDR} -j MASQUERADE 2>/dev/null || iptables -t nat -A POSTROUTING -s ${INT_SUBNET_CIDR} -j MASQUERADE\"
        ]" >/dev/null 2>&1 || true

    for HP_POD in $(kubectl get pods -n kube-system -l app=hugepages-setup -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
        kubectl exec -n kube-system "$HP_POD" -- nsenter -t 1 -m -u -i -n -p -- sh -c "
            for d in /sys/class/net/*; do
                [ -e \"\$d/device\" ] || continue
                dev=\$(basename \"\$d\")
                ip link set \"\$dev\" up 2>/dev/null || true
                ethtool -K \"\$dev\" tso off gso off gro off tx-nocache-copy off 2>/dev/null || true
            done
            sysctl -qw net.ipv4.ip_forward=1
            sysctl -qw net.netfilter.nf_conntrack_tcp_be_liberal=1 2>/dev/null || true
            iptables -C FORWARD -m conntrack --ctstate INVALID -j ACCEPT 2>/dev/null || iptables -I FORWARD 1 -m conntrack --ctstate INVALID -j ACCEPT || true
            iptables -t nat -C PREROUTING -s ${EXT_SUBNET_CIDR} -d ${SERVICE_CIDR} -j KUBE-SERVICES 2>/dev/null || iptables -t nat -I PREROUTING 1 -s ${EXT_SUBNET_CIDR} -d ${SERVICE_CIDR} -j KUBE-SERVICES || true
            iptables -t nat -C PREROUTING -s ${INT_SUBNET_CIDR} -d ${SERVICE_CIDR} -j KUBE-SERVICES 2>/dev/null || iptables -t nat -I PREROUTING 1 -s ${INT_SUBNET_CIDR} -d ${SERVICE_CIDR} -j KUBE-SERVICES || true
            iptables -t nat -C POSTROUTING -s ${EXT_SUBNET_CIDR} -d ${VPC_CIDR} -j MASQUERADE 2>/dev/null || iptables -t nat -A POSTROUTING -s ${EXT_SUBNET_CIDR} -d ${VPC_CIDR} -j MASQUERADE || true
            iptables -t nat -C POSTROUTING -s ${INT_SUBNET_CIDR} -d ${VPC_CIDR} -j MASQUERADE 2>/dev/null || iptables -t nat -A POSTROUTING -s ${INT_SUBNET_CIDR} -d ${VPC_CIDR} -j MASQUERADE || true
        " >/dev/null 2>&1 || true
    done

    # Re-trigger F5BigFwPolicy/mcp-firewall reconciliation so f5-afm (pccd) pushes ACL blobs over 9222
    if kubectl get f5-big-fw-policy mcp-firewall -n default >/dev/null 2>&1; then
        kubectl annotate f5-big-fw-policy mcp-firewall -n default "bnk.f5.com/reconcile-ts=$(date +%s)" --overwrite >/dev/null 2>&1 || true
        kubectl rollout restart deploy/f5-afm -n "${BNK_NS}" >/dev/null 2>&1 || true
    fi
fi

# Ensure TMM ENA TSO/LRO disable in ConfigMap/tmm-init in f5-cne-system
kubectl apply -f - <<EOF 2>/dev/null || true
apiVersion: v1
kind: ConfigMap
metadata:
  name: tmm-init
  namespace: ${BNK_NS}
data:
  tmm_init.tcl: |
    set_from_env  POD_IP  POD_IP  [info hostname]

        profile tcp nagledisable {
          default_name tcp
          nagle disable
        }

        profile tcp dia-tcp {
          default_name tcp
        }

        profile sctp dia-sctp {
          default_name sctp
        }

        profile http http_socket {
          default_name http
        }

        pool pool1 {

        }

        snatpool overlay_pool {

        }

        file_reload {
          file_name /opt/lib/tmm/user_conf.tcl
          interval 1
        }

        profile genericmsg gm_proto_for_gtp {
          app-service none
          disable-parser yes
          message-terminator none
          no-response yes
          description none
          max-egress-buffer 32768
          max-message-size 32768
        }

        profile messagerouter gm_router_for_gtp {
          name "gm_router_for_gtp"
          app-service none
          irule-scope-message no
        }

    	bigdb tm.tcpsegmentationoffload disable

    	bigdb tm.tcpwoodsidefasterrecovery disable

    	bigdb tm.tcplargereceiveoffload disable

        pool blobd_pool {
          member 169.254.0.253 9222
        }

        profile tcp blobd_tcp {
          default_name tcp
        }

        # Virtual for blobd
        virtual \$POD_IP 9222 {
          name blobd_virtual
          profile blobd_tcp
          vlan enable all
          snat_automap
          pool blobd_pool
          auto_lasthop off
        }
  user_conf.tcl: |
    bigdb tm.tcpsegmentationoffload disable
    bigdb tm.tcplargereceiveoffload disable
    pool pool1 {
    }
EOF

echo "Checking for existing Route 53 private hosted zone $BNK_DNS_ZONE..."
ZONE_ID=$(aws route53 list-hosted-zones-by-name \
    --dns-name "$BNK_DNS_ZONE" \
    --query "HostedZones[?Name=='${BNK_DNS_ZONE}.'].Id" --output text)

if [ -z "$ZONE_ID" ] || [ "$ZONE_ID" == "None" ]; then
    echo "Creating private hosted zone $BNK_DNS_ZONE..."
    CALLER_REF="setup-$(date +%s)-$$"
    ZONE_ID=$(aws route53 create-hosted-zone \
        --name "$BNK_DNS_ZONE" \
        --caller-reference "$CALLER_REF" \
        --hosted-zone-config Comment="Private zone for $CLUSTER_NAME BNK VIP",PrivateZone=true \
        --vpc VPCRegion="$REGION",VPCId="$VPC_ID" \
        --query "HostedZone.Id" --output text)
    echo "Created hosted zone: $ZONE_ID"
else
    echo "Found existing hosted zone: $ZONE_ID"
    VPC_ASSOC=$(aws route53 get-hosted-zone \
        --id "$ZONE_ID" \
        --query "VPCs[?VPCId=='$VPC_ID'].VPCId" --output text 2>/dev/null || echo "")
    if [ -z "$VPC_ASSOC" ] || [ "$VPC_ASSOC" == "None" ]; then
        echo "Associating VPC with hosted zone..."
        aws route53 associate-vpc-with-hosted-zone \
            --hosted-zone-id "$ZONE_ID" \
            --vpc VPCRegion="$REGION",VPCId="$VPC_ID" || echo "Already associated or error"
    fi
fi

echo "Creating/Updating DNS record $BNK_INGRESS_HOST -> $VIP_IP..."
aws route53 change-resource-record-sets \
    --hosted-zone-id "$ZONE_ID" \
    --change-batch '{
      "Comment": "Upsert BNK Ingress VIP",
      "Changes": [
        {
          "Action": "UPSERT",
          "ResourceRecordSet": {
            "Name": "'"$BNK_INGRESS_HOST"'",
            "Type": "A",
            "TTL": 60,
            "ResourceRecords": [{"Value": "'"$VIP_IP"'"}]
          }
        }
      ]
    }'

cat <<EOF > .agentcore-network-env.json
{
  "CLUSTER_NAME": "$CLUSTER_NAME",
  "VPC_ID": "$VPC_ID",
  "AGENT_SG_ID": "$AGENT_SG_ID",
  "TMM_SG_ID": "$TMM_SG_ID",
  "BNK_DNS_ZONE": "$BNK_DNS_ZONE",
  "BNK_INGRESS_HOST": "$BNK_INGRESS_HOST",
  "VIP_IP": "$VIP_IP",
  "AGENT_SUBNETS_JSON": $(echo "$AGENT_SUBNETS_JSON" | python3 -c 'import sys,json; print(json.dumps(sys.stdin.read().strip()))')
}
EOF

AGENT_SGS_JSON="[\"$AGENT_SG_ID\"]"

echo "Rendering agentcore.json from template..."
sed -e "s|{{ .VpcId }}|$VPC_ID|g" \
    -e "s|{{ .AgentSubnetIdsJson }}|$AGENT_SUBNETS_JSON|g" \
    -e "s|{{ .AgentSecurityGroupIdsJson }}|$AGENT_SGS_JSON|g" \
    "$DEMO_DIR/agent/agentcore/agentcore.json.tmpl" > "$DEMO_DIR/agent/agentcore/agentcore.json"

echo "Rendering harness.json from template..."
sed -e "s|{{ .BnkIngressHost }}|$BNK_INGRESS_HOST|g" \
    "$DEMO_DIR/agent/app/FinanceAgentV2/harness.json.tmpl" > "$DEMO_DIR/agent/app/FinanceAgentV2/harness.json"

# The AgentCore Gateway tool ARN is dynamic and cannot be templated without
# knowing the deployed gateway ARN. The supported CLI path is to add it after
# rendering the base harness.
if [ -d "$DEMO_DIR/agent/node_modules" ]; then
    echo "Adding BnkGatewayTool to harness via agentcore CLI..."
    (cd "$DEMO_DIR/agent" && npx agentcore add tool --harness FinanceAgentV2 --type agentcore_gateway --name BnkGatewayTool --gateway BnkGateway --outbound-auth awsIam 2>/dev/null || true)
fi

echo "AgentCore network seam setup complete."
echo "Variables written to .agentcore-network-env.json"
