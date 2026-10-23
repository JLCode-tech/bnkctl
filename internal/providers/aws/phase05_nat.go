package aws

import (
	"context"
	"fmt"
	"time"

	"github.com/JLCode-tech/bnkctl/internal/providers/aws/awsmw"
	"github.com/JLCode-tech/bnkctl/internal/providers/aws/tags"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// NATPollInterval is the polling interval when waiting for NAT Gateway state transitions.
var NATPollInterval = 5 * time.Second

// NATPollTimeout is the maximum time to wait for a NAT Gateway state transition.
var NATPollTimeout = 10 * time.Minute

func waitForNATState(ctx context.Context, c *Clients, natID string, target ec2types.NatGatewayState) error {
	deadline := time.Now().Add(NATPollTimeout)
	for {
		out, err := c.PhaseEC2.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{
			NatGatewayIds: []string{natID},
		})
		if err != nil {
			if target == ec2types.NatGatewayStateDeleted && awsmw.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("DescribeNatGateways %s: %w", natID, err)
		}
		if len(out.NatGateways) == 0 {
			if target == ec2types.NatGatewayStateDeleted {
				return nil
			}
			return fmt.Errorf("NAT gateway %s disappeared while waiting for %s", natID, target)
		}
		ng := out.NatGateways[0]
		if ng.State == target {
			return nil
		}
		if ng.State == ec2types.NatGatewayStateFailed {
			msg := ""
			if ng.FailureMessage != nil {
				msg = *ng.FailureMessage
			}
			return fmt.Errorf("NAT gateway %s entered failed state: %s", natID, msg)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for NAT gateway %s to reach %s (current: %s)", natID, target, ng.State)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(NATPollInterval):
		}
	}
}

// Phase05NAT allocates an EIP and creates a NAT Gateway in PUBLIC_SUBNET_0 when natGateways >= 1.
func Phase05NAT(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 05 nat: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 05 nat: state is nil")
	}
	if cl.Network.NatGatewaysCount() < 1 || dryRun {
		return nil
	}
	if c == nil || c.PhaseEC2 == nil {
		return fmt.Errorf("phase 05 nat: EC2 client is nil")
	}

	pubSubnet := st.Get("PUBLIC_SUBNET_0")
	if pubSubnet == "" {
		return fmt.Errorf("phase 05 nat: PUBLIC_SUBNET_0 not found in state")
	}

	if existing := st.Get("NAT_GW_ID"); existing != "" {
		out, err := c.PhaseEC2.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{
			NatGatewayIds: []string{existing},
		})
		if err == nil && len(out.NatGateways) > 0 {
			stateVal := out.NatGateways[0].State
			if stateVal == ec2types.NatGatewayStateAvailable || stateVal == ec2types.NatGatewayStatePending {
				return waitForNATState(ctx, c, existing, ec2types.NatGatewayStateAvailable)
			}
		}
		if err != nil && !awsmw.IsNotFound(err) {
			return fmt.Errorf("phase 05 nat: DescribeNatGateways %s: %w", existing, err)
		}
		st.Delete("NAT_GW_ID")
	}

	allocID := st.Get("NAT_EIP_ALLOC_ID")
	if allocID != "" {
		out, err := c.PhaseEC2.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
			AllocationIds: []string{allocID},
		})
		if err != nil || len(out.Addresses) == 0 {
			if err != nil && !awsmw.IsNotFound(err) {
				return fmt.Errorf("phase 05 nat: DescribeAddresses %s: %w", allocID, err)
			}
			allocID = ""
			st.Delete("NAT_EIP_ALLOC_ID")
		}
	}
	if allocID == "" {
		eipTags := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, map[string]string{
			"Name": cl.Metadata.Name + "-nat-eip",
		})
		eipOut, err := c.PhaseEC2.AllocateAddress(ctx, &ec2.AllocateAddressInput{
			Domain:            ec2types.DomainTypeVpc,
			TagSpecifications: tags.ToTagSpec(ec2types.ResourceTypeElasticIp, eipTags),
		})
		if err != nil {
			return fmt.Errorf("phase 05 nat: AllocateAddress: %w", err)
		}
		if eipOut.AllocationId == nil {
			return fmt.Errorf("phase 05 nat: AllocateAddress returned nil AllocationId")
		}
		allocID = *eipOut.AllocationId
		st.Set("NAT_EIP_ALLOC_ID", allocID)
		if err := st.Save(); err != nil {
			return err
		}
	}

	natTags := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, map[string]string{
		"Name": cl.Metadata.Name + "-nat",
	})
	natOut, err := c.PhaseEC2.CreateNatGateway(ctx, &ec2.CreateNatGatewayInput{
		SubnetId:          aws.String(pubSubnet),
		AllocationId:      aws.String(allocID),
		TagSpecifications: tags.ToTagSpec(ec2types.ResourceTypeNatgateway, natTags),
	})
	if err != nil {
		return fmt.Errorf("phase 05 nat: CreateNatGateway: %w", err)
	}
	if natOut.NatGateway == nil || natOut.NatGateway.NatGatewayId == nil {
		return fmt.Errorf("phase 05 nat: CreateNatGateway returned nil NatGatewayId")
	}
	natID := *natOut.NatGateway.NatGatewayId
	st.Set("NAT_GW_ID", natID)
	if err := st.Save(); err != nil {
		return err
	}

	return waitForNATState(ctx, c, natID, ec2types.NatGatewayStateAvailable)
}

// Phase05NATDown deletes the NAT Gateway, waits for it to reach the deleted state, and releases the EIP.
func Phase05NATDown(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if st == nil {
		return fmt.Errorf("phase 05 nat down: state is nil")
	}
	if dryRun {
		return nil
	}
	if c == nil || c.PhaseEC2 == nil {
		return fmt.Errorf("phase 05 nat down: EC2 client is nil")
	}

	if natID := st.Get("NAT_GW_ID"); natID != "" {
		_, err := c.PhaseEC2.DeleteNatGateway(ctx, &ec2.DeleteNatGatewayInput{
			NatGatewayId: aws.String(natID),
		})
		if err != nil && !awsmw.IsNotFound(err) {
			return fmt.Errorf("phase 05 nat down: DeleteNatGateway %s: %w", natID, err)
		}
		if err := waitForNATState(ctx, c, natID, ec2types.NatGatewayStateDeleted); err != nil {
			return fmt.Errorf("phase 05 nat down: waiting for NAT gateway %s deletion: %w", natID, err)
		}
		st.Delete("NAT_GW_ID")
		if err := st.Save(); err != nil {
			return err
		}
	}

	allocID := st.Get("NAT_EIP_ALLOC_ID")
	if allocID == "" {
		allocID = st.Get("NAT_EIP_ALLOC")
	}
	if allocID != "" {
		_, err := c.PhaseEC2.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{
			AllocationId: aws.String(allocID),
		})
		if err != nil && !awsmw.IsNotFound(err) {
			return fmt.Errorf("phase 05 nat down: ReleaseAddress %s: %w", allocID, err)
		}
		st.Delete("NAT_EIP_ALLOC_ID")
		st.Delete("NAT_EIP_ALLOC")
		if err := st.Save(); err != nil {
			return err
		}
	}

	return nil
}
