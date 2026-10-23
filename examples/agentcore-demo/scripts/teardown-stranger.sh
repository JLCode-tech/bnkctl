#!/bin/bash
set -uo pipefail

# Removes everything setup-stranger.sh created.
#
# Usage:
#   AWS_PROFILE=<profile> ./scripts/teardown-stranger.sh [cluster-name]
#
# RUN THIS BEFORE `awsbnkctl down`.
#
# `awsbnkctl down` does not know about any of this. It is phase-symmetric: each
# phase deletes exactly what its Up created, from state.env, and it never sweeps
# the VPC for foreign resources (which is the right call — it must be safe to run
# in a shared VPC).
#
# What actually blocks a teardown, in order of severity:
#
#   - THE INSTANCE. Its eth0 is in subnet-public-2, which awsbnkctl manages and
#     deletes during phase 03. A running instance there aborts `down` with a
#     DependencyViolation on awsbnkctl's OWN subnet. This is the real hazard.
#   - the extra subnet in the secondary CIDR — awsbnkctl never created it, so it
#     is left behind, and a VPC will not delete while any subnet remains
#   - the SG rule on bnk-data referencing the stranger SG — a security group
#     cannot be deleted while another SG's rule references it
#
# The secondary CIDR association is NOT a blocker on its own: DeleteVpc removes
# CIDR associations with the VPC. It is disassociated here for tidiness and so a
# rebuild starts from a known state.
#
# Order below is therefore load-bearing: revoke the cross-reference, detach and
# delete the ENI, terminate the instance and WAIT for it, then the SG, then the
# subnet, then the CIDR association. Skipping the waits is how this fails.
#
# Safe to re-run: every step tolerates the resource already being gone.

CLUSTER_NAME="${1:-bnk-agentcore-demo}"
REGION="${AWS_REGION:-$(aws configure get region 2>/dev/null || echo "ap-southeast-2")}"
export AWS_DEFAULT_REGION="$REGION"

STRANGER_CIDR="${STRANGER_CIDR:-100.64.0.0/16}"
TAG="${CLUSTER_NAME}-stranger"

say() { printf '\n== %s\n' "$1"; }
note() { printf '   %s\n' "$1"; }

VPC_ID=$(aws ec2 describe-vpcs \
    --filters "Name=tag:awsbnkctl:cluster,Values=${CLUSTER_NAME}" \
              "Name=tag:awsbnkctl:component,Values=vpc" \
    --query "Vpcs[0].VpcId" --output text 2>/dev/null)
if [ "$VPC_ID" = "None" ] || [ -z "$VPC_ID" ]; then
    note "No VPC for $CLUSTER_NAME — nothing to do (already torn down?)"
    exit 0
fi
note "VPC: $VPC_ID"

SG_ID=$(aws ec2 describe-security-groups \
    --filters "Name=vpc-id,Values=${VPC_ID}" "Name=group-name,Values=${TAG}" \
    --query "SecurityGroups[0].GroupId" --output text 2>/dev/null)
BNK_DATA_SG=$(aws ec2 describe-security-groups \
    --filters "Name=vpc-id,Values=${VPC_ID}" "Name=group-name,Values=${CLUSTER_NAME}-bnk-data" \
    --query "SecurityGroups[0].GroupId" --output text 2>/dev/null)

# 1. Revoke the cross-reference first, or the SG delete below fails.
if [ "$SG_ID" != "None" ] && [ "$BNK_DATA_SG" != "None" ]; then
    say "Revoking tcp/443 from $SG_ID on bnk-data"
    if aws ec2 revoke-security-group-ingress --group-id "$BNK_DATA_SG" \
         --ip-permissions "IpProtocol=tcp,FromPort=443,ToPort=443,UserIdGroupPairs=[{GroupId=${SG_ID}}]" \
         >/dev/null 2>&1; then
        note "revoked"
    else
        note "not present"
    fi
fi

# 2. The ENI must be detached and deleted before its subnet will delete.
say "Removing the out-of-range ENI"
OUT_ENI=$(aws ec2 describe-network-interfaces \
    --filters "Name=tag:Name,Values=${TAG}-outside-eni" \
    --query "NetworkInterfaces[0].NetworkInterfaceId" --output text 2>/dev/null)
if [ "$OUT_ENI" != "None" ] && [ -n "$OUT_ENI" ]; then
    ATT=$(aws ec2 describe-network-interfaces --network-interface-ids "$OUT_ENI" \
        --query "NetworkInterfaces[0].Attachment.AttachmentId" --output text 2>/dev/null)
    if [ "$ATT" != "None" ] && [ -n "$ATT" ]; then
        aws ec2 detach-network-interface --attachment-id "$ATT" --force >/dev/null 2>&1
        note "detaching $OUT_ENI..."
        for _ in $(seq 1 30); do
            sleep 4
            st=$(aws ec2 describe-network-interfaces --network-interface-ids "$OUT_ENI" \
                --query "NetworkInterfaces[0].Status" --output text 2>/dev/null || echo gone)
            [ "$st" = "available" ] && break
            [ "$st" = "gone" ] && break
        done
    fi
    aws ec2 delete-network-interface --network-interface-id "$OUT_ENI" >/dev/null 2>&1 \
        && note "deleted $OUT_ENI" || note "already gone"
else
    note "none found"
fi

# 3. Terminate and WAIT. A terminating instance still holds its ENIs, which
#    holds the subnet and the SG.
say "Terminating the stranger instance"
INSTANCE_ID=$(aws ec2 describe-instances \
    --filters "Name=tag:Name,Values=${TAG}" "Name=vpc-id,Values=${VPC_ID}" \
              "Name=instance-state-name,Values=pending,running,stopping,stopped" \
    --query "Reservations[0].Instances[0].InstanceId" --output text 2>/dev/null)
if [ "$INSTANCE_ID" != "None" ] && [ -n "$INSTANCE_ID" ]; then
    aws ec2 terminate-instances --instance-ids "$INSTANCE_ID" >/dev/null
    note "terminating $INSTANCE_ID (waiting — its ENIs block the subnet)"
    aws ec2 wait instance-terminated --instance-ids "$INSTANCE_ID"
    note "terminated"
else
    note "none found"
fi

# 4. Now the SG has no members and no referrers.
say "Deleting security group $TAG"
if [ "$SG_ID" != "None" ] && [ -n "$SG_ID" ]; then
    for _ in $(seq 1 15); do
        if aws ec2 delete-security-group --group-id "$SG_ID" >/dev/null 2>&1; then
            note "deleted $SG_ID"; break
        fi
        note "still in use, retrying..."
        sleep 6
    done
else
    note "none found"
fi

# 5. Subnet, then the CIDR association it lived in.
say "Deleting subnet ${TAG}-outside"
OUT_SUBNET=$(aws ec2 describe-subnets \
    --filters "Name=vpc-id,Values=${VPC_ID}" "Name=tag:Name,Values=${TAG}-outside" \
    --query "Subnets[0].SubnetId" --output text 2>/dev/null)
if [ "$OUT_SUBNET" != "None" ] && [ -n "$OUT_SUBNET" ]; then
    for _ in $(seq 1 15); do
        if aws ec2 delete-subnet --subnet-id "$OUT_SUBNET" >/dev/null 2>&1; then
            note "deleted $OUT_SUBNET"; break
        fi
        note "still has dependencies, retrying..."
        sleep 6
    done
else
    note "none found"
fi

say "Disassociating secondary CIDR $STRANGER_CIDR"
# A disassociated association lingers in the set with State=disassociated for a
# while after the call succeeds. Filter on state, or teardown reports false
# residue and setup refuses to re-create the CIDR.
ASSOC=$(aws ec2 describe-vpcs --vpc-ids "$VPC_ID" \
    --query "Vpcs[0].CidrBlockAssociationSet[?CidrBlock=='${STRANGER_CIDR}' && CidrBlockState.State!='disassociated'].AssociationId" \
    --output text 2>/dev/null)
if [ -n "$ASSOC" ] && [ "$ASSOC" != "None" ]; then
    for _ in $(seq 1 15); do
        if aws ec2 disassociate-vpc-cidr-block --association-id "$ASSOC" >/dev/null 2>&1; then
            note "disassociated"; break
        fi
        note "still in use, retrying..."
        sleep 6
    done
else
    note "not associated"
fi

# ── residue check, so this script proves its own work ───────────────────────
say "Residue check (all should be empty)"
for desc in \
  "instance|$(aws ec2 describe-instances --filters "Name=tag:Name,Values=${TAG}" "Name=instance-state-name,Values=pending,running,stopping,stopped" --query 'Reservations[].Instances[].InstanceId' --output text 2>/dev/null)" \
  "eni|$(aws ec2 describe-network-interfaces --filters "Name=tag:Name,Values=${TAG}-outside-eni" --query 'NetworkInterfaces[].NetworkInterfaceId' --output text 2>/dev/null)" \
  "sg|$(aws ec2 describe-security-groups --filters "Name=vpc-id,Values=${VPC_ID}" "Name=group-name,Values=${TAG}" --query 'SecurityGroups[].GroupId' --output text 2>/dev/null)" \
  "subnet|$(aws ec2 describe-subnets --filters "Name=vpc-id,Values=${VPC_ID}" "Name=tag:Name,Values=${TAG}-outside" --query 'Subnets[].SubnetId' --output text 2>/dev/null)" \
  "cidr|$(aws ec2 describe-vpcs --vpc-ids "$VPC_ID" --query "Vpcs[0].CidrBlockAssociationSet[?CidrBlock=='${STRANGER_CIDR}' && CidrBlockState.State!='disassociated'].CidrBlock" --output text 2>/dev/null)" ; do
    k="${desc%%|*}"; v="${desc#*|}"
    if [ -z "$v" ]; then printf '   ok      %s\n' "$k"
    else printf '   LEFT    %-8s %s\n' "$k" "$v"; RESIDUE=1; fi
done

if [ "${RESIDUE:-0}" = "1" ]; then
    echo
    echo "Residue above is still billing and may block awsbnkctl down. Re-run this script."
    exit 1
fi
echo
echo "Clean. Safe to run awsbnkctl down."
