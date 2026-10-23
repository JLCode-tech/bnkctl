package aws

import (
	"context"
	"fmt"
	"strings"

	"github.com/JLCode-tech/bnkctl/internal/providers/aws/awsmw"
	"github.com/JLCode-tech/bnkctl/internal/providers/aws/tags"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func ensureRouteTable(
	ctx context.Context,
	c *Clients,
	st *state.State,
	cl *intent.Cluster,
	vpcID string,
	stateKey string,
	name string,
) (string, error) {
	if existing := st.Get(stateKey); existing != "" {
		out, err := c.PhaseEC2.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
			RouteTableIds: []string{existing},
		})
		if err == nil && len(out.RouteTables) > 0 {
			return existing, nil
		}
		if err != nil && !awsmw.IsNotFound(err) {
			return "", fmt.Errorf("DescribeRouteTables %s (%s): %w", existing, stateKey, err)
		}
		st.Delete(stateKey)
	}

	t := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, map[string]string{
		"Name": name,
	})
	out, err := c.PhaseEC2.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{
		VpcId:             aws.String(vpcID),
		TagSpecifications: tags.ToTagSpec(ec2types.ResourceTypeRouteTable, t),
	})
	if err != nil {
		return "", fmt.Errorf("CreateRouteTable %s: %w", name, err)
	}
	if out.RouteTable == nil || out.RouteTable.RouteTableId == nil {
		return "", fmt.Errorf("CreateRouteTable %s returned nil RouteTableId", name)
	}
	rtID := *out.RouteTable.RouteTableId
	st.Set(stateKey, rtID)
	if err := st.Save(); err != nil {
		return "", err
	}
	return rtID, nil
}

func isRouteAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	return awsmw.ErrorCode(err) == "RouteAlreadyExists" ||
		strings.Contains(err.Error(), "RouteAlreadyExists")
}

func ensureDefaultRoute(ctx context.Context, c *Clients, rtID, igwID, natID string) error {
	input := &ec2.CreateRouteInput{
		RouteTableId:         aws.String(rtID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
	}
	if igwID != "" {
		input.GatewayId = aws.String(igwID)
	} else if natID != "" {
		input.NatGatewayId = aws.String(natID)
	} else {
		return nil
	}
	_, err := c.PhaseEC2.CreateRoute(ctx, input)
	if err != nil && !isRouteAlreadyExists(err) {
		return fmt.Errorf("CreateRoute 0.0.0.0/0 on %s: %w", rtID, err)
	}
	return nil
}

func ensureSubnetAssociation(ctx context.Context, c *Clients, st *state.State, rtID, subnetID, assocStateKey string) error {
	if existing := st.Get(assocStateKey); existing != "" {
		return nil
	}
	out, err := c.PhaseEC2.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtID),
		SubnetId:     aws.String(subnetID),
	})
	if err != nil {
		return fmt.Errorf("AssociateRouteTable %s -> %s: %w", rtID, subnetID, err)
	}
	if out.AssociationId != nil {
		st.Set(assocStateKey, *out.AssociationId)
		return st.Save()
	}
	return nil
}

// Phase06RouteTables creates public, private, and BNK data-path route tables, installs default routes, and associates subnets.
func Phase06RouteTables(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 06 routetables: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 06 routetables: state is nil")
	}
	if dryRun {
		return nil
	}
	if c == nil || c.PhaseEC2 == nil {
		return fmt.Errorf("phase 06 routetables: EC2 client is nil")
	}
	vpcID := st.Get("VPC_ID")
	if vpcID == "" {
		return fmt.Errorf("phase 06 routetables: VPC_ID not found in state")
	}
	igwID := st.Get("IGW_ID")
	if igwID == "" {
		return fmt.Errorf("phase 06 routetables: IGW_ID not found in state")
	}

	pubRT, err := ensureRouteTable(ctx, c, st, cl, vpcID, "PUBLIC_RT_ID", cl.Metadata.Name+"-public-rt")
	if err != nil {
		return fmt.Errorf("phase 06 routetables: %w", err)
	}
	if err := ensureDefaultRoute(ctx, c, pubRT, igwID, ""); err != nil {
		return fmt.Errorf("phase 06 routetables: %w", err)
	}
	for i := range cl.Network.Subnets.PublicList {
		subID := st.Get(subnetStateKey("PUBLIC_SUBNET", i))
		if subID == "" {
			return fmt.Errorf("phase 06 routetables: PUBLIC_SUBNET_%d not found in state", i)
		}
		assocKey := fmt.Sprintf("PUBLIC_RT_ASSOC_%d", i)
		if err := ensureSubnetAssociation(ctx, c, st, pubRT, subID, assocKey); err != nil {
			return fmt.Errorf("phase 06 routetables: %w", err)
		}
	}

	if len(cl.Network.Subnets.PrivateList) > 0 {
		privRT, err := ensureRouteTable(ctx, c, st, cl, vpcID, "PRIVATE_RT_ID", cl.Metadata.Name+"-private-rt")
		if err != nil {
			return fmt.Errorf("phase 06 routetables: %w", err)
		}
		if cl.Network.NatGatewaysCount() >= 1 {
			natID := st.Get("NAT_GW_ID")
			if natID == "" {
				return fmt.Errorf("phase 06 routetables: NAT_GW_ID not found in state")
			}
			if err := ensureDefaultRoute(ctx, c, privRT, "", natID); err != nil {
				return fmt.Errorf("phase 06 routetables: %w", err)
			}
		}
		for i := range cl.Network.Subnets.PrivateList {
			subID := st.Get(subnetStateKey("PRIVATE_SUBNET", i))
			if subID == "" {
				return fmt.Errorf("phase 06 routetables: PRIVATE_SUBNET_%d not found in state", i)
			}
			assocKey := fmt.Sprintf("PRIVATE_RT_ASSOC_%d", i)
			if err := ensureSubnetAssociation(ctx, c, st, privRT, subID, assocKey); err != nil {
				return fmt.Errorf("phase 06 routetables: %w", err)
			}
		}
	}

	if cl.HasExternalDataPath() {
		extSub := st.Get("BNK_EXT_SUBNET")
		if extSub == "" {
			return fmt.Errorf("phase 06 routetables: BNK_EXT_SUBNET not found in state")
		}
		extRT, err := ensureRouteTable(ctx, c, st, cl, vpcID, "BNK_EXT_RT_ID", cl.Metadata.Name+"-bnk-ext-rt")
		if err != nil {
			return fmt.Errorf("phase 06 routetables: %w", err)
		}
		if err := ensureSubnetAssociation(ctx, c, st, extRT, extSub, "BNK_EXT_RT_ASSOC"); err != nil {
			return fmt.Errorf("phase 06 routetables: %w", err)
		}
	}

	if cl.HasInternalInterface() {
		intSub := st.Get("BNK_INT_SUBNET")
		if intSub == "" {
			return fmt.Errorf("phase 06 routetables: BNK_INT_SUBNET not found in state")
		}
		intRT, err := ensureRouteTable(ctx, c, st, cl, vpcID, "BNK_INT_RT_ID", cl.Metadata.Name+"-bnk-int-rt")
		if err != nil {
			return fmt.Errorf("phase 06 routetables: %w", err)
		}
		if err := ensureSubnetAssociation(ctx, c, st, intRT, intSub, "BNK_INT_RT_ASSOC"); err != nil {
			return fmt.Errorf("phase 06 routetables: %w", err)
		}
	}

	return nil
}

// Phase06RouteTablesDown disassociates and deletes all route tables recorded in state.
func Phase06RouteTablesDown(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 06 routetables down: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 06 routetables down: state is nil")
	}
	if dryRun {
		return nil
	}
	if c == nil || c.PhaseEC2 == nil {
		return fmt.Errorf("phase 06 routetables down: EC2 client is nil")
	}

	var assocKeys []string
	assocKeys = append(assocKeys, "BNK_INT_RT_ASSOC", "BNK_EXT_RT_ASSOC")
	for i := len(cl.Network.Subnets.PrivateList) - 1; i >= 0; i-- {
		assocKeys = append(assocKeys, fmt.Sprintf("PRIVATE_RT_ASSOC_%d", i))
	}
	for i := len(cl.Network.Subnets.PublicList) - 1; i >= 0; i-- {
		assocKeys = append(assocKeys, fmt.Sprintf("PUBLIC_RT_ASSOC_%d", i))
	}

	for _, k := range assocKeys {
		assocID := st.Get(k)
		if assocID == "" {
			continue
		}
		_, err := c.PhaseEC2.DisassociateRouteTable(ctx, &ec2.DisassociateRouteTableInput{
			AssociationId: aws.String(assocID),
		})
		if err != nil && !awsmw.IsNotFound(err) {
			return fmt.Errorf("phase 06 routetables down: DisassociateRouteTable %s (%s): %w", assocID, k, err)
		}
		st.Delete(k)
	}

	deletedRTs := map[string]bool{}
	rtKeys := []string{"BNK_INT_RT_ID", "BNK_EXT_RT_ID", "PRIVATE_RT_ID", "PRIVATE_RTB", "PUBLIC_RT_ID", "PUBLIC_RTB"}
	for _, k := range rtKeys {
		rtID := st.Get(k)
		if rtID == "" {
			continue
		}
		if descOut, derr := c.PhaseEC2.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
			RouteTableIds: []string{rtID},
		}); derr == nil && descOut != nil {
			for _, rt := range descOut.RouteTables {
				for _, assoc := range rt.Associations {
					if assoc.Main != nil && *assoc.Main {
						continue
					}
					if assoc.RouteTableAssociationId != nil && *assoc.RouteTableAssociationId != "" {
						_, _ = c.PhaseEC2.DisassociateRouteTable(ctx, &ec2.DisassociateRouteTableInput{
							AssociationId: assoc.RouteTableAssociationId,
						})
					}
				}
			}
		}
		_, err := c.PhaseEC2.DeleteRouteTable(ctx, &ec2.DeleteRouteTableInput{
			RouteTableId: aws.String(rtID),
		})
		if err != nil && !awsmw.IsNotFound(err) {
			return fmt.Errorf("phase 06 routetables down: DeleteRouteTable %s (%s): %w", rtID, k, err)
		}
		deletedRTs[rtID] = true
		st.Delete(k)
	}

	if vpcID := st.Get("VPC_ID"); vpcID != "" {
		if descOut, derr := c.PhaseEC2.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
			Filters: []ec2types.Filter{
				{Name: aws.String("vpc-id"), Values: []string{vpcID}},
			},
		}); derr == nil && descOut != nil {
			for _, rt := range descOut.RouteTables {
				if rt.RouteTableId == nil || deletedRTs[*rt.RouteTableId] {
					continue
				}
				isMain := false
				for _, assoc := range rt.Associations {
					if assoc.Main != nil && *assoc.Main {
						isMain = true
						continue
					}
					if assoc.RouteTableAssociationId != nil && *assoc.RouteTableAssociationId != "" {
						_, _ = c.PhaseEC2.DisassociateRouteTable(ctx, &ec2.DisassociateRouteTableInput{
							AssociationId: assoc.RouteTableAssociationId,
						})
					}
				}
				if isMain {
					continue
				}
				_, err := c.PhaseEC2.DeleteRouteTable(ctx, &ec2.DeleteRouteTableInput{
					RouteTableId: rt.RouteTableId,
				})
				if err != nil && !awsmw.IsNotFound(err) {
					return fmt.Errorf("phase 06 routetables down: DeleteRouteTable %s (vpc-discovery): %w", *rt.RouteTableId, err)
				}
			}
		}
	}

	return st.Save()
}
