#!/bin/bash
set -euo pipefail

# Provisions an honest "stranger" for Path 3, and the one source address that
# can actually exercise BNK's L4 firewall reject rule.
#
# Usage:
#   AWS_PROFILE=<profile> ./scripts/setup-stranger.sh [cluster-name]
#
# WHY THIS EXISTS
#
# Path 3 is pitched as "a caller no managed front door sees". Until this script,
# it was driven from the jumphost — whose second NIC sits in subnet-bnk-ext and,
# worse, in BNK's own data-plane security group (bnk-data), which permits ALL
# protocols from itself. Tested: the same request out of the jumphost's
# public-1 NIC is dropped (000) and out of its bnk-ext NIC succeeds (200). So
# the gate was the security group, not the network, and the "stranger" was
# sitting inside BNK's own dataplane. That is the weakest possible version of
# the threat model the demo claims to show.
#
# This builds a caller that is actually a stranger:
#
#   - its own instance, its own security group (nothing else is in it)
#   - subnet-public-2: different subnet, different AZ from the VIP
#   - reaches the VIP only because bnk-data is given ONE narrow rule:
#     tcp/443 from the stranger SG. No self-referencing all-protocols shortcut.
#
# AND it fixes the other gap. The firewall accepts 10.0.0.0/16 and rejects
# everything else, but that reject branch was untestable: every address that
# can route to a VIP inside the VPC is, by definition, inside the VPC CIDR. So
# this also attaches a SECOND NIC on a secondary VPC CIDR (100.64.0.0/16 —
# CGNAT space, deliberately outside the accept list). Same host, same SG, two
# source subnets:
#
#   from 10.0.2.x    -> SG allows, firewall accepts  -> 200
#   from 100.64.2.x  -> SG allows, firewall rejects  -> refused
#
# Because the SG is identical on both NICs, anything that differs is BNK's
# firewall and nothing else. That is what makes it a real test rather than an
# assertion.
#
# Idempotent: re-running finds existing resources by tag and leaves them alone.
#
# Tear down with ./scripts/teardown-stranger.sh BEFORE `awsbnkctl down`. The
# reason is sharper than 'extra resources lying around': this instance's eth0
# sits in subnet-public-2, which awsbnkctl DOES manage and WILL try to delete
# (it is listed in PUBLIC_SUBNETS in state.env). Leave the instance running and
# `down` aborts at phase 03 with a DependencyViolation on its own subnet.

CLUSTER_NAME="${1:-bnk-agentcore-demo}"
REGION="${AWS_REGION:-$(aws configure get region 2>/dev/null || echo "ap-southeast-2")}"
export AWS_DEFAULT_REGION="$REGION"

STRANGER_CIDR="${STRANGER_CIDR:-100.64.0.0/16}"
STRANGER_SUBNET_CIDR="${STRANGER_SUBNET_CIDR:-100.64.2.0/24}"
INSTANCE_TYPE="${INSTANCE_TYPE:-t3.micro}"
TAG="${CLUSTER_NAME}-stranger"

say() { printf '\n== %s\n' "$1"; }
note() { printf '   %s\n' "$1"; }

# ── discover the cluster's network ───────────────────────────────────────────
say "Discovering VPC for cluster $CLUSTER_NAME"
VPC_ID=$(aws ec2 describe-vpcs \
    --filters "Name=tag:awsbnkctl:cluster,Values=${CLUSTER_NAME}" \
              "Name=tag:awsbnkctl:component,Values=vpc" \
    --query "Vpcs[0].VpcId" --output text)
[ "$VPC_ID" = "None" ] || [ -z "$VPC_ID" ] && { echo "No VPC for $CLUSTER_NAME"; exit 1; }
note "VPC: $VPC_ID"

# public-2 is where the stranger lives: different subnet AND different AZ
# from the VIP, which is what makes it a fair test.
PUB2_ID=$(aws ec2 describe-subnets \
    --filters "Name=vpc-id,Values=${VPC_ID}" \
              "Name=tag:Name,Values=${CLUSTER_NAME}-subnet-public-2" \
    --query "Subnets[0].SubnetId" --output text)
[ "$PUB2_ID" = "None" ] && { echo "No subnet-public-2 found"; exit 1; }
PUB2_AZ=$(aws ec2 describe-subnets --subnet-ids "$PUB2_ID" \
    --query "Subnets[0].AvailabilityZone" --output text)
note "stranger subnet: $PUB2_ID ($PUB2_AZ)"

BNK_DATA_SG=$(aws ec2 describe-security-groups \
    --filters "Name=vpc-id,Values=${VPC_ID}" \
              "Name=group-name,Values=${CLUSTER_NAME}-bnk-data" \
    --query "SecurityGroups[0].GroupId" --output text)
[ "$BNK_DATA_SG" = "None" ] && { echo "No bnk-data SG found"; exit 1; }
note "bnk-data SG: $BNK_DATA_SG"

PROFILE_ARN=$(aws ec2 describe-instances \
    --filters "Name=tag:Name,Values=${CLUSTER_NAME}-jumphost" \
              "Name=instance-state-name,Values=running,stopped" \
    --query "Reservations[0].Instances[0].IamInstanceProfile.Arn" --output text)
[ "$PROFILE_ARN" = "None" ] && { echo "No jumphost instance profile to reuse"; exit 1; }
PROFILE_NAME="${PROFILE_ARN##*/}"
note "reusing instance profile: $PROFILE_NAME  (nothing new to clean up in IAM)"

# ── secondary VPC CIDR, outside the firewall's accept list ───────────────────
say "Associating secondary CIDR $STRANGER_CIDR"
# Filter on state: a previously torn-down CIDR lingers as State=disassociated,
# and treating that as "already associated" would skip the association and then
# fail at create-subnet with an opaque InvalidSubnet.Range.
if aws ec2 describe-vpcs --vpc-ids "$VPC_ID" \
     --query "Vpcs[0].CidrBlockAssociationSet[?CidrBlock=='${STRANGER_CIDR}' && CidrBlockState.State!='disassociated'].CidrBlock" \
     --output text | grep -q .; then
    note "already associated"
else
    aws ec2 associate-vpc-cidr-block --vpc-id "$VPC_ID" \
        --cidr-block "$STRANGER_CIDR" >/dev/null
    note "associated $STRANGER_CIDR"
    note "AWS adds a local route for it automatically, so it is routable to the VIP"
fi

say "Creating subnet $STRANGER_SUBNET_CIDR in $PUB2_AZ"
OUT_SUBNET=$(aws ec2 describe-subnets \
    --filters "Name=vpc-id,Values=${VPC_ID}" "Name=tag:Name,Values=${TAG}-outside" \
    --query "Subnets[0].SubnetId" --output text)
if [ "$OUT_SUBNET" != "None" ] && [ -n "$OUT_SUBNET" ]; then
    note "exists: $OUT_SUBNET"
else
    OUT_SUBNET=$(aws ec2 create-subnet --vpc-id "$VPC_ID" \
        --cidr-block "$STRANGER_SUBNET_CIDR" --availability-zone "$PUB2_AZ" \
        --tag-specifications "ResourceType=subnet,Tags=[{Key=Name,Value=${TAG}-outside},{Key=awsbnkctl:cluster,Value=${CLUSTER_NAME}},{Key=bnkdemo:stranger,Value=true}]" \
        --query "Subnet.SubnetId" --output text)
    note "created: $OUT_SUBNET"
fi

# ── the stranger's own security group ────────────────────────────────────────
say "Creating security group ${TAG}"
SG_ID=$(aws ec2 describe-security-groups \
    --filters "Name=vpc-id,Values=${VPC_ID}" "Name=group-name,Values=${TAG}" \
    --query "SecurityGroups[0].GroupId" --output text)
if [ "$SG_ID" != "None" ] && [ -n "$SG_ID" ]; then
    note "exists: $SG_ID"
else
    SG_ID=$(aws ec2 create-security-group --vpc-id "$VPC_ID" --group-name "$TAG" \
        --description "Path 3 stranger: its own SG, deliberately NOT bnk-data" \
        --tag-specifications "ResourceType=security-group,Tags=[{Key=Name,Value=${TAG}},{Key=awsbnkctl:cluster,Value=${CLUSTER_NAME}},{Key=bnkdemo:stranger,Value=true}]" \
        --query "GroupId" --output text)
    note "created: $SG_ID"
fi

# One narrow rule on bnk-data: tcp/443 from the stranger SG. This is the whole
# authorisation story for Path 3 — no subnet adjacency, no shared SG.
say "Allowing tcp/443 from the stranger SG into bnk-data"
if aws ec2 authorize-security-group-ingress --group-id "$BNK_DATA_SG" \
     --ip-permissions "IpProtocol=tcp,FromPort=443,ToPort=443,UserIdGroupPairs=[{GroupId=${SG_ID},Description=path-3 stranger}]" \
     >/dev/null 2>&1; then
    note "rule added"
else
    note "rule already present"
fi

# ── the instance, with a NIC in each CIDR ───────────────────────────────────
say "Launching the stranger ($INSTANCE_TYPE)"
INSTANCE_ID=$(aws ec2 describe-instances \
    --filters "Name=tag:Name,Values=${TAG}" "Name=vpc-id,Values=${VPC_ID}" \
              "Name=instance-state-name,Values=pending,running,stopping,stopped" \
    --query "Reservations[0].Instances[0].InstanceId" --output text)
if [ "$INSTANCE_ID" != "None" ] && [ -n "$INSTANCE_ID" ]; then
    note "exists: $INSTANCE_ID"
else
    AMI=$(aws ssm get-parameter \
        --name /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64 \
        --query "Parameter.Value" --output text)
    note "AMI: $AMI"
    INSTANCE_ID=$(aws ec2 run-instances \
        --image-id "$AMI" --instance-type "$INSTANCE_TYPE" \
        --iam-instance-profile "Name=${PROFILE_NAME}" \
        --network-interfaces "DeviceIndex=0,SubnetId=${PUB2_ID},Groups=${SG_ID},AssociatePublicIpAddress=true,DeleteOnTermination=true" \
        --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=${TAG}},{Key=awsbnkctl:cluster,Value=${CLUSTER_NAME}},{Key=bnkdemo:stranger,Value=true}]" \
        --metadata-options "HttpTokens=required" \
        --query "Instances[0].InstanceId" --output text)
    note "launched: $INSTANCE_ID"
fi

note "waiting for the instance to run..."
aws ec2 wait instance-running --instance-ids "$INSTANCE_ID"

# Second NIC, in the out-of-range CIDR. Same SG as eth0 on purpose: if the SG
# is identical, any behavioural difference between the two sources is BNK's
# firewall and nothing else.
say "Attaching the out-of-range NIC ($STRANGER_SUBNET_CIDR)"
OUT_ENI=$(aws ec2 describe-network-interfaces \
    --filters "Name=tag:Name,Values=${TAG}-outside-eni" \
    --query "NetworkInterfaces[0].NetworkInterfaceId" --output text)
if [ "$OUT_ENI" != "None" ] && [ -n "$OUT_ENI" ]; then
    note "exists: $OUT_ENI"
else
    OUT_ENI=$(aws ec2 create-network-interface --subnet-id "$OUT_SUBNET" \
        --groups "$SG_ID" \
        --description "${TAG} out-of-firewall-range source" \
        --tag-specifications "ResourceType=network-interface,Tags=[{Key=Name,Value=${TAG}-outside-eni},{Key=awsbnkctl:cluster,Value=${CLUSTER_NAME}},{Key=bnkdemo:stranger,Value=true}]" \
        --query "NetworkInterface.NetworkInterfaceId" --output text)
    note "created: $OUT_ENI"
fi

ATTACHED=$(aws ec2 describe-network-interfaces --network-interface-ids "$OUT_ENI" \
    --query "NetworkInterfaces[0].Attachment.InstanceId" --output text)
if [ "$ATTACHED" = "$INSTANCE_ID" ]; then
    note "already attached to $INSTANCE_ID"
else
    aws ec2 attach-network-interface --network-interface-id "$OUT_ENI" \
        --instance-id "$INSTANCE_ID" --device-index 1 >/dev/null
    note "attached at device index 1"
fi

IN_IP=$(aws ec2 describe-instances --instance-ids "$INSTANCE_ID" \
    --query "Reservations[0].Instances[0].NetworkInterfaces[?Attachment.DeviceIndex==\`0\`].PrivateIpAddress" \
    --output text)
OUT_IP=$(aws ec2 describe-network-interfaces --network-interface-ids "$OUT_ENI" \
    --query "NetworkInterfaces[0].PrivateIpAddress" --output text)

cat <<EOS

== Done

  instance          $INSTANCE_ID   ($INSTANCE_TYPE, $PUB2_AZ)
  security group    $SG_ID   ($TAG)  — allowed into bnk-data on tcp/443 only
  in-range source   $IN_IP   (subnet-public-2)      -> firewall should ACCEPT
  out-of-range src  $OUT_IP  ($STRANGER_SUBNET_CIDR) -> firewall should REJECT

  The second NIC needs a source-routing rule before it will emit packets with
  its own address; ./scripts/demo.sh act 6 configures that at test time so this
  script leaves no host state behind.

  Verify with:  ./scripts/demo.sh
  Tear down:    ./scripts/teardown-stranger.sh     <-- BEFORE awsbnkctl down
EOS
