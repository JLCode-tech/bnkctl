package gcp

import (
	"context"
	"fmt"
	"os"
	"strings"

	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"

	"github.com/JLCode-tech/bnkctl/internal/providers/gcp/labels"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

// Phase15Jumphost provisions a testing jumphost VM and associated firewall
// rules in the external network to simulate external traffic sources.
// Feature-gated by testing.jumphost.enabled in cluster.yaml.
func Phase15Jumphost(ctx context.Context, cl *intent.Cluster, st *state.State, clients *Clients, dryRun bool) error {
	fmt.Fprintf(os.Stderr, "[phase 15] jumphost: cluster=%s\n", cl.Metadata.Name)

	if cl.Testing.Jumphost == nil || !cl.Testing.Jumphost.Enabled {
		fmt.Fprintln(os.Stderr, "[phase 15] skipped: jumphost disabled")
		return nil
	}

	jh := cl.Testing.Jumphost
	instanceType := jh.MachineType
	if instanceType == "" {
		instanceType = "e2-micro"
	}
	name := cl.Metadata.Name + "-jumphost"
	fwName := cl.Metadata.Name + "-allow-iap-ssh"

	if dryRun {
		fmt.Fprintf(os.Stderr, "[phase 15] dry-run: would create IAP firewall rule %s on %s\n", fwName, cl.Network.External.VPC)
		fmt.Fprintf(os.Stderr, "[phase 15] dry-run: would create jumphost instance %s (%s) in %s\n", name, instanceType, cl.Network.External.Subnet)
		if st != nil {
			st.Set("JUMPHOST_NAME", name)
			st.Set("JUMPHOST_INSTANCE_NAME", name)
			st.Set("JUMPHOST_INSTANCE_ID", name)
			st.Set("JUMPHOST_FIREWALL_NAME", fwName)
		}
		return nil
	}

	if clients == nil || clients.Instances == nil {
		return nil
	}

	// 1. Create IAP Firewall rule
	if clients.Firewalls != nil {
		if err := ensureIAPFirewall(ctx, clients.Firewalls, cl.Metadata.Project, cl.Network.External.VPC, fwName, cl.Metadata.Name); err != nil {
			return fmt.Errorf("phase15: firewall: %w", err)
		}
		if st != nil {
			st.Set("JUMPHOST_FIREWALL_NAME", fwName)
		}
	}

	// 2. Create Instance
	if err := ensureJumphostInstance(ctx, clients.Instances, cl.Metadata.Project, cl.Metadata.Zone, cl.Network.External.VPC, cl.Network.External.Subnet, name, instanceType, cl); err != nil {
		return fmt.Errorf("phase15: instance: %w", err)
	}
	if st != nil {
		st.Set("JUMPHOST_NAME", name)
		st.Set("JUMPHOST_INSTANCE_NAME", name)
		st.Set("JUMPHOST_INSTANCE_ID", name)
		if inst, err := clients.Instances.Get(ctx, &computepb.GetInstanceRequest{
			Project:  cl.Metadata.Project,
			Zone:     cl.Metadata.Zone,
			Instance: name,
		}); err == nil && len(inst.GetNetworkInterfaces()) > 0 {
			if ip := inst.GetNetworkInterfaces()[0].GetNetworkIP(); ip != "" {
				st.Set("JUMPHOST_EXT_IP", ip)
				st.Set("JUMPHOST_BNK_EXT_ENI_IP", ip)
			}
		}
		return st.Save()
	}
	return nil
}

func ensureIAPFirewall(ctx context.Context, firewalls FirewallsAPI, project, vpc, fwName, clName string) error {
	_, err := firewalls.Get(ctx, &computepb.GetFirewallRequest{Project: project, Firewall: fwName})
	if err == nil {
		fmt.Fprintf(os.Stderr, "[phase 15] firewall %s already exists, skipping\n", fwName)
		return nil
	}
	if !isNotFound(err) {
		return fmt.Errorf("checking firewall %s: %w", fwName, err)
	}

	fmt.Fprintf(os.Stderr, "[phase 15] creating firewall %s (allow IAP SSH)\n", fwName)
	op, err := firewalls.Insert(ctx, &computepb.InsertFirewallRequest{
		Project: project,
		FirewallResource: &computepb.Firewall{
			Name:         proto.String(fwName),
			Description:  proto.String(labels.OwnerMarker(clName)),
			Network:      proto.String(networkSelfLink(project, vpc)),
			Direction:    proto.String("INGRESS"),
			SourceRanges: []string{"35.235.240.0/20"}, // IAP range
			Allowed: []*computepb.Allowed{
				{
					IPProtocol: proto.String("tcp"),
					Ports:      []string{"22"},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("creating firewall %s: %w", fwName, err)
	}
	return waitOp(ctx, op, "insert firewall "+fwName)
}

func ensureJumphostInstance(ctx context.Context, instances InstancesAPI, project, zone, vpc, subnet, name, machineType string, cl *intent.Cluster) error {
	_, err := instances.Get(ctx, &computepb.GetInstanceRequest{Project: project, Zone: zone, Instance: name})
	if err == nil {
		fmt.Fprintf(os.Stderr, "[phase 15] instance %s already exists, skipping\n", name)
		return nil
	}
	if !isNotFound(err) {
		return fmt.Errorf("checking instance %s: %w", name, err)
	}

	fmt.Fprintf(os.Stderr, "[phase 15] creating jumphost instance %s\n", name)
	op, err := instances.Insert(ctx, &computepb.InsertInstanceRequest{
		Project: project,
		Zone:    zone,
		InstanceResource: &computepb.Instance{
			Name:        proto.String(name),
			Description: proto.String(labels.OwnerMarker(cl.Metadata.Name)),
			MachineType: proto.String(fmt.Sprintf("zones/%s/machineTypes/%s", zone, machineType)),
			Labels:      labels.Merge(labels.Required(cl.Metadata.Name), cl.Metadata.Labels),
			NetworkInterfaces: []*computepb.NetworkInterface{
				{
					Network:    proto.String(networkSelfLink(project, vpc)),
					Subnetwork: proto.String(fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s", project, zoneToRegion(zone), subnet)),
				},
			},
			Disks: []*computepb.AttachedDisk{
				{
					Boot:       proto.Bool(true),
					AutoDelete: proto.Bool(true),
					InitializeParams: &computepb.AttachedDiskInitializeParams{
						SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12"),
						DiskSizeGb:  proto.Int64(20),
					},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("creating instance %s: %w", name, err)
	}
	return waitOp(ctx, op, "insert jumphost "+name)
}

func zoneToRegion(zone string) string {
	parts := len(zone) - 2
	if parts > 0 && zone[parts] == '-' {
		return zone[:parts]
	}
	return zone
}

// PhaseDownJumphost tears down the testing jumphost VM and its IAP firewall.
func PhaseDownJumphost(ctx context.Context, cl *intent.Cluster, st *state.State, clients *Clients, dryRun bool) error {
	fmt.Fprintf(os.Stderr, "[phase 15 down] jumphost: cluster=%s\n", cl.Metadata.Name)

	if cl.Testing.Jumphost == nil || !cl.Testing.Jumphost.Enabled {
		fmt.Fprintln(os.Stderr, "[phase 15 down] skipped: jumphost disabled")
		if st != nil {
			st.Delete("JUMPHOST_NAME")
			st.Delete("JUMPHOST_INSTANCE_NAME")
			st.Delete("JUMPHOST_INSTANCE_ID")
			st.Delete("JUMPHOST_EXT_IP")
			st.Delete("JUMPHOST_BNK_EXT_ENI_IP")
			st.Delete("JUMPHOST_FIREWALL_NAME")
			return st.Save()
		}
		return nil
	}

	if dryRun || clients == nil {
		return nil
	}

	name := ""
	fwName := ""
	if st != nil {
		name = st.Get("JUMPHOST_INSTANCE_NAME")
		fwName = st.Get("JUMPHOST_FIREWALL_NAME")
	}
	if name == "" || strings.HasPrefix(name, "gke-") {
		name = cl.Metadata.Name + "-jumphost"
	}
	if fwName == "" {
		fwName = cl.Metadata.Name + "-allow-iap-ssh"
	}

	if clients.Instances != nil {
		fmt.Fprintf(os.Stderr, "[phase 15 down] deleting instance %s\n", name)
		op, err := clients.Instances.Delete(ctx, &computepb.DeleteInstanceRequest{
			Project:  cl.Metadata.Project,
			Zone:     cl.Metadata.Zone,
			Instance: name,
		})
		if err != nil {
			if !isNotFound(err) {
				return fmt.Errorf("deleting instance %s: %w", name, err)
			}
		} else {
			if err := waitOp(ctx, op, "delete jumphost "+name); err != nil {
				return fmt.Errorf("waiting for instance %s deletion: %w", name, err)
			}
		}
	}

	if clients.Firewalls != nil {
		fmt.Fprintf(os.Stderr, "[phase 15 down] deleting firewall %s\n", fwName)
		op, err := clients.Firewalls.Delete(ctx, &computepb.DeleteFirewallRequest{
			Project:  cl.Metadata.Project,
			Firewall: fwName,
		})
		if err != nil {
			if !isNotFound(err) {
				return fmt.Errorf("deleting firewall %s: %w", fwName, err)
			}
		} else {
			if err := waitOp(ctx, op, "delete firewall "+fwName); err != nil {
				return fmt.Errorf("waiting for firewall %s deletion: %w", fwName, err)
			}
		}
	}

	if st != nil {
		st.Delete("JUMPHOST_NAME")
		st.Delete("JUMPHOST_INSTANCE_NAME")
		st.Delete("JUMPHOST_INSTANCE_ID")
		st.Delete("JUMPHOST_EXT_IP")
		st.Delete("JUMPHOST_BNK_EXT_ENI_IP")
		st.Delete("JUMPHOST_FIREWALL_NAME")
		return st.Save()
	}
	return nil
}
