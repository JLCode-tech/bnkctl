package aws

import (
	"context"
	"fmt"
	"os"

	"github.com/JLCode-tech/bnkctl/internal/providers/aws/awsmw"
	"github.com/JLCode-tech/bnkctl/internal/providers/aws/tags"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Phase04IGW creates (or re-uses) an Internet Gateway and attaches it to the VPC.
func Phase04IGW(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 04 igw: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 04 igw: state is nil")
	}
	if dryRun {
		return nil
	}
	if c == nil || c.PhaseEC2 == nil {
		return fmt.Errorf("phase 04 igw: EC2 client is nil")
	}
	vpcID := st.Get("VPC_ID")
	if vpcID == "" {
		return fmt.Errorf("phase 04 igw: VPC_ID not found in state")
	}

	if existing := st.Get("IGW_ID"); existing != "" {
		out, err := c.PhaseEC2.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{
			InternetGatewayIds: []string{existing},
		})
		if err == nil && len(out.InternetGateways) > 0 {
			igw := out.InternetGateways[0]
			attached := false
			for _, a := range igw.Attachments {
				if a.VpcId != nil && *a.VpcId == vpcID {
					attached = true
					break
				}
			}
			if !attached {
				if _, err := c.PhaseEC2.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{
					InternetGatewayId: aws.String(existing),
					VpcId:             aws.String(vpcID),
				}); err != nil {
					return fmt.Errorf("phase 04 igw: AttachInternetGateway %s to %s: %w", existing, vpcID, err)
				}
			}
			return nil
		}
		if err != nil && !awsmw.IsNotFound(err) {
			return fmt.Errorf("phase 04 igw: DescribeInternetGateways %s: %w", existing, err)
		}
		st.Delete("IGW_ID")
	}

	igwName := cl.Metadata.Name + "-igw"
	t := tags.Build(cl.Metadata.Name, cl.Metadata.Tags, map[string]string{
		"Name": igwName,
	})

	createOut, err := c.PhaseEC2.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{
		TagSpecifications: tags.ToTagSpec(ec2types.ResourceTypeInternetGateway, t),
	})
	if err != nil {
		return fmt.Errorf("phase 04 igw: CreateInternetGateway: %w", err)
	}
	if createOut.InternetGateway == nil || createOut.InternetGateway.InternetGatewayId == nil {
		return fmt.Errorf("phase 04 igw: CreateInternetGateway returned nil InternetGatewayId")
	}
	igwID := *createOut.InternetGateway.InternetGatewayId

	if _, err := c.PhaseEC2.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String(vpcID),
	}); err != nil {
		return fmt.Errorf("phase 04 igw: AttachInternetGateway %s to %s: %w", igwID, vpcID, err)
	}

	st.Set("IGW_ID", igwID)
	return st.Save()
}

// Phase04IGWDown detaches and deletes the Internet Gateway recorded in state.
func Phase04IGWDown(ctx context.Context, cl *intent.Cluster, st *state.State, c *Clients, dryRun bool) error {
	if st == nil {
		return fmt.Errorf("phase 04 igw down: state is nil")
	}
	igwID := st.Get("IGW_ID")
	if igwID == "" || dryRun {
		return nil
	}
	if c == nil || c.PhaseEC2 == nil {
		return fmt.Errorf("phase 04 igw down: EC2 client is nil")
	}

	vpcID := st.Get("VPC_ID")
	if vpcID != "" {
		_, err := c.PhaseEC2.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{
			InternetGatewayId: aws.String(igwID),
			VpcId:             aws.String(vpcID),
		})
		if err != nil && awsmw.IsDependencyViolation(err) {
			sweepVPCInstancesAndENIs(ctx, c, vpcID)
			_, err = c.PhaseEC2.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{
				InternetGatewayId: aws.String(igwID),
				VpcId:             aws.String(vpcID),
			})
			if err != nil && awsmw.IsDependencyViolation(err) {
				fmt.Fprintf(os.Stderr, "phase 04 igw down: warning: IGW %s on %s still has mapped public dependencies (%v); deferring to VPC blocker sweep\n", igwID, vpcID, err)
				return nil
			}
		}
		if err != nil && !awsmw.IsNotFound(err) {
			return fmt.Errorf("phase 04 igw down: DetachInternetGateway %s from %s: %w", igwID, vpcID, err)
		}
	}

	_, err := c.PhaseEC2.DeleteInternetGateway(ctx, &ec2.DeleteInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
	})
	if err != nil && awsmw.IsDependencyViolation(err) {
		fmt.Fprintf(os.Stderr, "phase 04 igw down: warning: IGW %s still has dependencies (%v); deferring to VPC blocker sweep\n", igwID, err)
		return nil
	}
	if err != nil && !awsmw.IsNotFound(err) {
		return fmt.Errorf("phase 04 igw down: DeleteInternetGateway %s: %w", igwID, err)
	}
	st.Delete("IGW_ID")
	return st.Save()
}
