package gcp

import (
	"context"
	"fmt"
	"log/slog"

	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"

	"github.com/JLCode-tech/bnkctl/internal/providers/gcp/labels"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

// Phase10Networks creates the 3 VPC networks (main, external, internal), their
// primary + secondary subnetwork ranges, allow-internal firewall rules, and
// Cloud Router + Cloud NAT on the two dataplane VPCs.
//
// Every resource uses Get(name) -> skip-if-exists idempotency. Cloud Router /
// NAT creation is non-fatal when the caller lacks compute.routers.create
// (HTTP 403), logging a warning instead of aborting.
func Phase10Networks(ctx context.Context, cl *intent.Cluster, st *state.State, clients *Clients, dryRun bool) error {
	project := cl.Metadata.Project
	region := cl.Region
	cluster := cl.Metadata.Name
	main := cl.Network.Main
	ext := cl.Network.External
	intl := cl.Network.Internal
	allowCIDR := cl.Network.AllowInternalCidr

	slog.Info("phase 10: networks",
		"project", project,
		"region", region,
		"main_vpc", main.VPC,
		"ext_vpc", ext.VPC,
		"int_vpc", intl.VPC,
		"dry_run", dryRun,
	)

	if dryRun {
		slog.Info("phase 10 [dry-run]: would ensure VPC", "name", main.VPC, "mtu", 0)
		slog.Info("phase 10 [dry-run]: would ensure subnet", "name", main.Subnet, "vpc", main.VPC, "cidr", main.CIDR,
			"secondary_pods", main.PodsRangeName+"="+main.PodsCIDR,
			"secondary_svcs", main.SvcsRangeName+"="+main.SvcsCIDR,
		)
		slog.Info("phase 10 [dry-run]: would ensure VPC", "name", ext.VPC, "mtu", ext.MTU)
		slog.Info("phase 10 [dry-run]: would ensure subnet", "name", ext.Subnet, "vpc", ext.VPC, "cidr", ext.CIDR,
			"secondary_pods", ext.PodsRangeName+"="+ext.PodsCIDR,
		)
		if cl.IsDualInterface() {
			slog.Info("phase 10 [dry-run]: would ensure VPC", "name", intl.VPC, "mtu", intl.MTU)
			slog.Info("phase 10 [dry-run]: would ensure subnet", "name", intl.Subnet, "vpc", intl.VPC, "cidr", intl.CIDR,
				"secondary_pods", intl.PodsRangeName+"="+intl.PodsCIDR,
			)
		}
		vpcs := []string{main.VPC, ext.VPC}
		if cl.IsDualInterface() {
			vpcs = append(vpcs, intl.VPC)
		}
		for _, vpc := range vpcs {
			slog.Info("phase 10 [dry-run]: would ensure firewall", "name", vpc+"-allow-internal", "vpc", vpc, "source_range", allowCIDR)
		}
		if cl.Network.NatGatewaysEnabled() {
			natVPCs := []string{ext.VPC}
			if cl.IsDualInterface() {
				natVPCs = append(natVPCs, intl.VPC)
			}
			for _, vpc := range natVPCs {
				slog.Info("phase 10 [dry-run]: would ensure Cloud Router + NAT (403-tolerant)", "router", vpc+"-router", "nat", vpc+"-nat")
			}
		}
		return nil
	}

	if clients == nil || clients.Networks == nil {
		return nil
	}

	// --- Main VPC + subnet (primary + pods + services secondary ranges) ---
	if err := ensureVPC(ctx, clients.Networks, project, main.VPC, 0, cluster); err != nil {
		return fmt.Errorf("phase 10: main VPC %s: %w", main.VPC, err)
	}
	if err := ensureSubnet(ctx, clients.Subnetworks, project, region, main.Subnet, main.VPC, main.CIDR, []*computepb.SubnetworkSecondaryRange{
		{RangeName: proto.String(main.PodsRangeName), IpCidrRange: proto.String(main.PodsCIDR)},
		{RangeName: proto.String(main.SvcsRangeName), IpCidrRange: proto.String(main.SvcsCIDR)},
	}, cluster); err != nil {
		return fmt.Errorf("phase 10: main subnet %s: %w", main.Subnet, err)
	}

	// --- External dataplane VPC + subnet (MTU 8896, pods secondary range) ---
	// #nosec G115 -- MTU is validated in [1300, 8896] by intent validation
	if err := ensureVPC(ctx, clients.Networks, project, ext.VPC, int32(ext.MTU), cluster); err != nil {
		return fmt.Errorf("phase 10: external VPC %s: %w", ext.VPC, err)
	}
	if err := ensureSubnet(ctx, clients.Subnetworks, project, region, ext.Subnet, ext.VPC, ext.CIDR, []*computepb.SubnetworkSecondaryRange{
		{RangeName: proto.String(ext.PodsRangeName), IpCidrRange: proto.String(ext.PodsCIDR)},
	}, cluster); err != nil {
		return fmt.Errorf("phase 10: external subnet %s: %w", ext.Subnet, err)
	}

	// --- Internal dataplane VPC + subnet (MTU 8896, pods secondary range) ---
	if cl.IsDualInterface() {
		// #nosec G115 -- MTU is validated in [1300, 8896] by intent validation
		if err := ensureVPC(ctx, clients.Networks, project, intl.VPC, int32(intl.MTU), cluster); err != nil {
			return fmt.Errorf("phase 10: internal VPC %s: %w", intl.VPC, err)
		}
		if err := ensureSubnet(ctx, clients.Subnetworks, project, region, intl.Subnet, intl.VPC, intl.CIDR, []*computepb.SubnetworkSecondaryRange{
			{RangeName: proto.String(intl.PodsRangeName), IpCidrRange: proto.String(intl.PodsCIDR)},
		}, cluster); err != nil {
			return fmt.Errorf("phase 10: internal subnet %s: %w", intl.Subnet, err)
		}
	}

	// --- Allow-internal firewall rules on all VPCs ---
	vpcs := []string{main.VPC, ext.VPC}
	if cl.IsDualInterface() {
		vpcs = append(vpcs, intl.VPC)
	}
	for _, vpc := range vpcs {
		if err := ensureAllowInternalFirewall(ctx, clients.Firewalls, project, vpc, allowCIDR, cluster); err != nil {
			return fmt.Errorf("phase 10: firewall for %s: %w", vpc, err)
		}
	}

	// --- Cloud Router + Cloud NAT on dataplane VPCs (non-fatal on 403) ---
	if cl.Network.NatGatewaysEnabled() {
		natVPCs := []string{ext.VPC}
		if cl.IsDualInterface() {
			natVPCs = append(natVPCs, intl.VPC)
		}
		for _, vpc := range natVPCs {
			if err := ensureRouterNAT(ctx, clients.Routers, project, region, vpc, cluster); err != nil {
				return fmt.Errorf("phase 10: router/NAT for %s: %w", vpc, err)
			}
		}
	}

	_ = st // Phase 10 does not write state.env keys.
	return nil
}

// ensureVPC creates a custom-mode VPC network if it does not already exist.
// When mtu > 0 it is set on the Network resource (dataplane VPCs use 8896).
func ensureVPC(ctx context.Context, networks NetworksAPI, project, name string, mtu int32, cluster string) error {
	netRes, err := networks.Get(ctx, &computepb.GetNetworkRequest{
		Project: project,
		Network: name,
	})
	if err == nil {
		owner, managed := labels.ParseOwner(netRes.GetDescription())
		if !managed {
			slog.Info("phase 10: VPC already exists (external/unmanaged) — skipping", "vpc", name)
			return nil
		}
		if owner != cluster {
			return fmt.Errorf("VPC %s is already managed by another cluster (%s)", name, owner)
		}
		slog.Info("phase 10: VPC already exists (managed by this cluster) — skipping", "vpc", name)
		return nil
	}
	if !isNotFound(err) {
		return fmt.Errorf("get network %s: %w", name, err)
	}

	net := &computepb.Network{
		Name:                  proto.String(name),
		Description:           proto.String(labels.OwnerMarker(cluster)),
		AutoCreateSubnetworks: proto.Bool(false),
		RoutingConfig: &computepb.NetworkRoutingConfig{
			RoutingMode: proto.String("REGIONAL"),
		},
	}
	if mtu > 0 {
		net.Mtu = proto.Int32(mtu)
	}

	slog.Info("phase 10: creating VPC", "vpc", name, "mtu", mtu)
	op, err := networks.Insert(ctx, &computepb.InsertNetworkRequest{
		Project:         project,
		NetworkResource: net,
	})
	if err != nil {
		return fmt.Errorf("insert network %s: %w", name, err)
	}
	return waitOp(ctx, op, "insert network "+name)
}

// ensureSubnet creates a regional subnetwork with the given primary CIDR and
// secondary IP ranges if it does not already exist.
func ensureSubnet(ctx context.Context, subnetworks SubnetworksAPI, project, region, name, vpc, cidr string, secondaries []*computepb.SubnetworkSecondaryRange, cluster string) error {
	subRes, err := subnetworks.Get(ctx, &computepb.GetSubnetworkRequest{
		Project:    project,
		Region:     region,
		Subnetwork: name,
	})
	if err == nil {
		owner, managed := labels.ParseOwner(subRes.GetDescription())
		if !managed {
			slog.Info("phase 10: subnet already exists (external/unmanaged) — skipping", "subnet", name)
			return nil
		}
		if owner != cluster {
			return fmt.Errorf("subnet %s is already managed by another cluster (%s)", name, owner)
		}
		slog.Info("phase 10: subnet already exists (managed by this cluster) — skipping", "subnet", name)
		return nil
	}
	if !isNotFound(err) {
		return fmt.Errorf("get subnetwork %s: %w", name, err)
	}

	slog.Info("phase 10: creating subnet", "subnet", name, "vpc", vpc, "cidr", cidr)
	op, err := subnetworks.Insert(ctx, &computepb.InsertSubnetworkRequest{
		Project: project,
		Region:  region,
		SubnetworkResource: &computepb.Subnetwork{
			Name:                  proto.String(name),
			Description:           proto.String(labels.OwnerMarker(cluster)),
			Network:               proto.String(networkSelfLink(project, vpc)),
			Region:                proto.String(region),
			IpCidrRange:           proto.String(cidr),
			SecondaryIpRanges:     secondaries,
			PrivateIpGoogleAccess: proto.Bool(true),
		},
	})
	if err != nil {
		return fmt.Errorf("insert subnetwork %s: %w", name, err)
	}
	return waitOp(ctx, op, "insert subnetwork "+name)
}

// ensureAllowInternalFirewall creates <vpc>-allow-internal permitting tcp,
// udp, icmp, and ipip from allowCIDR if it does not already exist.
func ensureAllowInternalFirewall(ctx context.Context, firewalls FirewallsAPI, project, vpc, allowCIDR, cluster string) error {
	fwName := vpc + "-allow-internal"
	fwRes, err := firewalls.Get(ctx, &computepb.GetFirewallRequest{
		Project:  project,
		Firewall: fwName,
	})
	if err == nil {
		owner, managed := labels.ParseOwner(fwRes.GetDescription())
		if !managed {
			slog.Info("phase 10: firewall rule already exists (external/unmanaged) — skipping", "firewall", fwName)
			return nil
		}
		if owner != cluster {
			return fmt.Errorf("firewall %s is already managed by another cluster (%s)", fwName, owner)
		}
		slog.Info("phase 10: firewall rule already exists (managed by this cluster) — skipping", "firewall", fwName)
		return nil
	}
	if !isNotFound(err) {
		return fmt.Errorf("get firewall %s: %w", fwName, err)
	}

	slog.Info("phase 10: creating firewall rule", "firewall", fwName, "vpc", vpc, "source_range", allowCIDR)
	op, err := firewalls.Insert(ctx, &computepb.InsertFirewallRequest{
		Project: project,
		FirewallResource: &computepb.Firewall{
			Name:         proto.String(fwName),
			Description:  proto.String(labels.OwnerMarker(cluster)),
			Network:      proto.String(networkSelfLink(project, vpc)),
			Direction:    proto.String("INGRESS"),
			SourceRanges: []string{allowCIDR},
			Allowed: []*computepb.Allowed{
				{IPProtocol: proto.String("tcp")},
				{IPProtocol: proto.String("udp")},
				{IPProtocol: proto.String("icmp")},
				{IPProtocol: proto.String("ipip")},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("insert firewall %s: %w", fwName, err)
	}
	return waitOp(ctx, op, "insert firewall "+fwName)
}

// ensureRouterNAT creates <vpc>-router and <vpc>-nat in two distinct steps.
//
// Step 1 — Router: Get(<vpc>-router); if 404, Insert a bare Router (no embedded
// Nats). On HTTP 403 (compute.routers.create denied), log a warning and return
// nil without attempting Step 2. Any other error fails the phase.
//
// Step 2 — NAT: if <vpc>-nat is not already present on the router, Patch the
// router to attach it. Any error on the NAT Patch (including non-403) is
// logged as a warning and swallowed.
func ensureRouterNAT(ctx context.Context, routers RoutersAPI, project, region, vpc, cluster string) error {
	routerName := vpc + "-router"
	natName := vpc + "-nat"

	existing, err := routers.Get(ctx, &computepb.GetRouterRequest{
		Project: project,
		Region:  region,
		Router:  routerName,
	})
	if err != nil {
		if !isNotFound(err) {
			if isForbidden(err) {
				slog.Warn("phase 10: compute.routers.get denied; skipping Cloud Router + NAT",
					"router", routerName, "vpc", vpc)
				return nil
			}
			return fmt.Errorf("get router %s: %w", routerName, err)
		}

		slog.Info("phase 10: creating Cloud Router", "router", routerName, "vpc", vpc)
		op, ierr := routers.Insert(ctx, &computepb.InsertRouterRequest{
			Project: project,
			Region:  region,
			RouterResource: &computepb.Router{
				Name:        proto.String(routerName),
				Description: proto.String(labels.OwnerMarker(cluster)),
				Network:     proto.String(networkSelfLink(project, vpc)),
				Region:      proto.String(region),
			},
		})
		if ierr != nil {
			if isForbidden(ierr) {
				slog.Warn("phase 10: Cloud Router creation skipped (compute.routers.create denied) — egress via external VPC relies on existing routing",
					"router", routerName, "vpc", vpc)
				return nil
			}
			return fmt.Errorf("insert router %s: %w", routerName, ierr)
		}
		if werr := waitOp(ctx, op, "insert router "+routerName); werr != nil {
			return werr
		}
	} else {
		owner, managed := labels.ParseOwner(existing.GetDescription())
		if !managed {
			slog.Info("phase 10: Cloud Router already exists (external/unmanaged) — skipping", "router", routerName)
			return nil
		}
		if owner != cluster {
			return fmt.Errorf("router %s is already managed by another cluster (%s)", routerName, owner)
		}
		for _, n := range existing.GetNats() {
			if n.GetName() == natName {
				slog.Info("phase 10: Cloud Router + NAT already exist (managed by this cluster) — skipping", "router", routerName, "nat", natName)
				return nil
			}
		}
		slog.Info("phase 10: Cloud Router exists without NAT; attaching NAT", "router", routerName, "nat", natName)
	}

	slog.Info("phase 10: attaching Cloud NAT to router", "router", routerName, "nat", natName)
	patchOp, perr := routers.Patch(ctx, &computepb.PatchRouterRequest{
		Project: project,
		Region:  region,
		Router:  routerName,
		RouterResource: &computepb.Router{
			Name: proto.String(routerName),
			Nats: append(existing.GetNats(), buildRouterNat(natName)),
		},
	})
	if perr != nil {
		slog.Warn("phase 10: Cloud NAT creation failed (non-fatal)", "nat", natName, "router", routerName, "err", perr)
		return nil
	}
	if werr := waitOp(ctx, patchOp, "patch router "+routerName); werr != nil {
		slog.Warn("phase 10: Cloud NAT operation failed (non-fatal)", "nat", natName, "router", routerName, "err", werr)
	}
	return nil
}

func buildRouterNat(natName string) *computepb.RouterNat {
	return &computepb.RouterNat{
		Name:                          proto.String(natName),
		NatIpAllocateOption:           proto.String("AUTO_ONLY"),
		SourceSubnetworkIpRangesToNat: proto.String("ALL_SUBNETWORKS_ALL_IP_RANGES"),
	}
}

func networkSelfLink(project, vpc string) string {
	return fmt.Sprintf("projects/%s/global/networks/%s", project, vpc)
}
