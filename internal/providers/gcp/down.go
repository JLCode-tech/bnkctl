package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	computepb "cloud.google.com/go/compute/apiv1/computepb"
	containerpb "cloud.google.com/go/container/apiv1/containerpb"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

const (
	clusterPollInterval = 15 * time.Second
	clusterPollTimeout  = 25 * time.Minute
)

// PhaseDownCluster deletes the GKE cluster and polls the LRO to DONE. A 404 on
// DeleteCluster makes this a no-op so re-running `down` is idempotent.
func PhaseDownCluster(ctx context.Context, c *Clients, cl *intent.Cluster, st *state.State) error {
	if c == nil || c.GKE == nil {
		return nil
	}
	project := ""
	zone := ""
	name := ""
	if cl != nil {
		project = cl.Metadata.Project
		zone = cl.Metadata.Zone
		name = cl.Metadata.Name
	}
	if project == "" && st != nil {
		project = st.Get("GCP_PROJECT")
	}
	if zone == "" && st != nil {
		zone = st.Get("GCP_ZONE")
	}
	if name == "" && st != nil {
		name = st.Get("CLUSTER_NAME")
	}
	if project == "" || zone == "" || name == "" {
		return nil
	}

	op, err := c.GKE.DeleteCluster(ctx, &containerpb.DeleteClusterRequest{
		Name: fmt.Sprintf("projects/%s/locations/%s/clusters/%s", project, zone, name),
	})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("PhaseDownCluster: DeleteCluster(%s): %w", name, err)
	}
	if err := waitGKEOp(ctx, c, project, zone, op, "DeleteCluster("+name+")"); err != nil {
		return fmt.Errorf("PhaseDownCluster: %w", err)
	}
	return nil
}

func waitGKEOp(ctx context.Context, c *Clients, project, location string, op *containerpb.Operation, desc string) error {
	if op == nil || op.GetStatus() == containerpb.Operation_DONE {
		if op != nil && op.GetError() != nil && op.GetError().GetMessage() != "" {
			return fmt.Errorf("%s failed: %s", desc, op.GetError().GetMessage())
		}
		return nil
	}
	opName := fmt.Sprintf("projects/%s/locations/%s/operations/%s", project, location, op.GetName())
	return PollUntil(ctx, clusterPollInterval, clusterPollTimeout, func(ctx context.Context) (bool, error) {
		cur, err := c.GKE.GetOperation(ctx, &containerpb.GetOperationRequest{Name: opName})
		if err != nil {
			return false, fmt.Errorf("%s: GetOperation(%s): %w", desc, op.GetName(), err)
		}
		if cur.GetStatus() != containerpb.Operation_DONE {
			return false, nil
		}
		if cur.GetError() != nil && cur.GetError().GetMessage() != "" {
			return false, fmt.Errorf("%s failed: %s", desc, cur.GetError().GetMessage())
		}
		return true, nil
	})
}

// PhaseDownNetworks deletes the jumphost GCE VM (if created) and tears down
// the 3 VPCs in dependency order: routers -> firewalls -> subnetworks -> networks.
// Shared dataplane VPCs (explicit vpcName in cluster.yaml) only lose this
// cluster's subnet unless this cluster was the last tenant.
func PhaseDownNetworks(ctx context.Context, c *Clients, cl *intent.Cluster, st *state.State) error {
	if c == nil || cl == nil {
		return nil
	}
	ensureGCPDefaults(cl)
	project := cl.Metadata.Project
	if project == "" && st != nil {
		project = st.Get("GCP_PROJECT")
	}
	region := cl.EffectiveRegion()
	if region == "" && st != nil {
		region = st.Get("GCP_REGION")
	}
	zone := cl.Metadata.Zone
	if zone == "" && st != nil {
		zone = st.Get("GCP_ZONE")
	}
	name := cl.Metadata.Name
	if name == "" && st != nil {
		name = st.Get("CLUSTER_NAME")
	}

	if c.Instances != nil && zone != "" && name != "" {
		jhName := name + "-jumphost"
		if st != nil {
			if v := st.Get("JUMPHOST_VM_NAME"); v != "" {
				jhName = v
			} else if v := st.Get("JUMPHOST_INSTANCE_NAME"); v != "" && !strings.HasPrefix(v, "gke-") {
				jhName = v
			} else if v := st.Get("JUMPHOST_NAME"); v != "" {
				jhName = v
			}
		}
		op, err := c.Instances.Delete(ctx, &computepb.DeleteInstanceRequest{
			Project:  project,
			Zone:     zone,
			Instance: jhName,
		})
		if err != nil && !isNotFound(err) {
			return fmt.Errorf("PhaseDownNetworks: delete jumphost %s: %w", jhName, err)
		}
		if err == nil {
			if werr := waitOp(ctx, op, "delete jumphost "+jhName); werr != nil && !isNotFound(werr) {
				return fmt.Errorf("PhaseDownNetworks: %w", werr)
			}
		}
	}

	mainVPC := cl.Network.Main.VPC
	if mainVPC == "" && st != nil {
		mainVPC = st.Get("MAIN_VPC")
	}
	if mainVPC == "" && name != "" {
		mainVPC = name + "-main-vpc"
	}
	mainSub := cl.Network.Main.Subnet
	if mainSub == "" && st != nil {
		mainSub = st.Get("MAIN_SUBNET")
	}
	if mainSub == "" && name != "" {
		mainSub = name + "-main-subnet"
	}
	extVPC := cl.ExternalVPCName()
	extSub := cl.ExternalSubnetName()
	intVPC := cl.InternalVPCName()
	intSub := cl.InternalSubnetName()

	// Dedicated main VPC: always tear down router, firewalls, subnet, and VPC.
	if err := deleteRouterIdempotent(ctx, c, project, region, mainVPC+"-router"); err != nil {
		return fmt.Errorf("PhaseDownNetworks: %w", err)
	}
	if err := deleteFirewallsForVPC(ctx, c, project, mainVPC); err != nil {
		return fmt.Errorf("PhaseDownNetworks: %w", err)
	}
	if err := deleteSubnetIdempotent(ctx, c, project, region, mainSub); err != nil {
		return fmt.Errorf("PhaseDownNetworks: %w", err)
	}
	if err := deleteNetworkIdempotent(ctx, c, project, mainVPC); err != nil {
		return fmt.Errorf("PhaseDownNetworks: %w", err)
	}

	// External dataplane VPC (may be shared across clusters).
	if extVPC != "" && extSub != "" {
		if err := teardownDataplaneResources(ctx, c, project, region, extVPC, extSub, cl.IsExternalVPCShared()); err != nil {
			return fmt.Errorf("PhaseDownNetworks (external): %w", err)
		}
	}

	// Internal dataplane VPC (dual-interface only; may be shared across clusters).
	if cl.IsDualInterface() && intVPC != "" && intSub != "" {
		if err := teardownDataplaneResources(ctx, c, project, region, intVPC, intSub, cl.IsInternalVPCShared()); err != nil {
			return fmt.Errorf("PhaseDownNetworks (internal): %w", err)
		}
	}

	if err := deleteOrphanedBNKDisks(ctx, c, project, zone, name); err != nil {
		return fmt.Errorf("PhaseDownNetworks (disks): %w", err)
	}

	return nil
}

// teardownDataplaneResources deletes this cluster's dataplane subnet and, if no
// other subnetworks remain in the VPC afterwards, also deletes the VPC's router,
// firewall rules, and the VPC itself.
func teardownDataplaneResources(ctx context.Context, c *Clients, project, region, vpc, subnet string, isShared bool) error {
	if err := deleteSubnetIdempotent(ctx, c, project, region, subnet); err != nil {
		return err
	}

	deletedDefaultRouter := false
	defaultRouter := vpc + "-router"
	if isShared {
		disc, err := DiscoverDataplaneResources(ctx, c, project, region, vpc)
		if err != nil {
			return err
		}
		for _, s := range disc.Subnetworks {
			if s.GetName() != subnet {
				// Another cluster's subnet still lives in this shared VPC; preserve
				// the shared router, firewall rules, and VPC network.
				return nil
			}
		}
		for _, r := range disc.Routers {
			if r.GetName() == defaultRouter {
				deletedDefaultRouter = true
			}
			if err := deleteRouterIdempotent(ctx, c, project, region, r.GetName()); err != nil {
				return err
			}
		}
	}
	if !deletedDefaultRouter {
		if err := deleteRouterIdempotent(ctx, c, project, region, defaultRouter); err != nil {
			return err
		}
	}

	if err := deleteFirewallsForVPC(ctx, c, project, vpc); err != nil {
		return err
	}
	return deleteNetworkIdempotent(ctx, c, project, vpc)
}

func deleteRouterIdempotent(ctx context.Context, c *Clients, project, region, routerName string) error {
	if c.Routers == nil {
		return nil
	}
	op, err := c.Routers.Delete(ctx, &computepb.DeleteRouterRequest{
		Project: project,
		Region:  region,
		Router:  routerName,
	})
	if err != nil {
		if isNotFound(err) || isForbidden(err) {
			return nil
		}
		return fmt.Errorf("delete router %s: %w", routerName, err)
	}
	if werr := waitOp(ctx, op, "delete router "+routerName); werr != nil && !isNotFound(werr) {
		return werr
	}
	return nil
}

func deleteFirewallsForVPC(ctx context.Context, c *Clients, project, vpc string) error {
	if c.Firewalls == nil {
		return nil
	}
	vpcSelfLink := fmt.Sprintf("projects/%s/global/networks/%s", project, vpc)
	fws, err := c.Firewalls.ListAll(ctx, project)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("list firewalls for %s: %w", vpc, err)
	}
	defaultFW := vpc + "-allow-internal"
	deletedDefaultFW := false
	for _, fw := range fws {
		if !matchesVPC(fw.GetNetwork(), vpcSelfLink) && !strings.HasPrefix(fw.GetName(), vpc+"-") {
			continue
		}
		if fw.GetName() == defaultFW {
			deletedDefaultFW = true
		}
		op, derr := c.Firewalls.Delete(ctx, &computepb.DeleteFirewallRequest{
			Project:  project,
			Firewall: fw.GetName(),
		})
		if derr != nil {
			if isNotFound(derr) {
				continue
			}
			return fmt.Errorf("delete firewall %s: %w", fw.GetName(), derr)
		}
		if werr := waitOp(ctx, op, "delete firewall "+fw.GetName()); werr != nil && !isNotFound(werr) {
			return werr
		}
	}
	if !deletedDefaultFW {
		op, derr := c.Firewalls.Delete(ctx, &computepb.DeleteFirewallRequest{
			Project:  project,
			Firewall: defaultFW,
		})
		if derr != nil && !isNotFound(derr) {
			return fmt.Errorf("delete firewall %s: %w", defaultFW, derr)
		}
		if derr == nil {
			if werr := waitOp(ctx, op, "delete firewall "+defaultFW); werr != nil && !isNotFound(werr) {
				return werr
			}
		}
	}
	return nil
}

func deleteSubnetIdempotent(ctx context.Context, c *Clients, project, region, subName string) error {
	if c.Subnetworks == nil {
		return nil
	}
	op, err := c.Subnetworks.Delete(ctx, &computepb.DeleteSubnetworkRequest{
		Project:    project,
		Region:     region,
		Subnetwork: subName,
	})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete subnetwork %s: %w", subName, err)
	}
	if werr := waitOp(ctx, op, "delete subnetwork "+subName); werr != nil && !isNotFound(werr) {
		return werr
	}
	return nil
}

func deleteNetworkIdempotent(ctx context.Context, c *Clients, project, vpc string) error {
	if c.Networks == nil {
		return nil
	}
	op, err := c.Networks.Delete(ctx, &computepb.DeleteNetworkRequest{
		Project: project,
		Network: vpc,
	})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete network %s: %w", vpc, err)
	}
	if werr := waitOp(ctx, op, "delete network "+vpc); werr != nil && !isNotFound(werr) {
		return werr
	}
	return nil
}

func deleteOrphanedBNKDisks(ctx context.Context, c *Clients, project, zone, clusterName string) error {
	if c == nil || c.Disks == nil || project == "" || zone == "" {
		return nil
	}
	disks, err := c.Disks.ListAll(ctx, project, zone)
	if err != nil {
		if isNotFound(err) || isForbidden(err) {
			return nil
		}
		return fmt.Errorf("list disks in %s/%s: %w", project, zone, err)
	}
	for _, d := range disks {
		if d == nil || len(d.GetUsers()) > 0 {
			continue
		}
		diskName := d.GetName()
		if !strings.HasPrefix(diskName, "pvc-") {
			continue
		}
		if !isBNKPVCDisk(d, clusterName) {
			continue
		}
		op, derr := c.Disks.Delete(ctx, &computepb.DeleteDiskRequest{
			Project: project,
			Zone:    zone,
			Disk:    diskName,
		})
		if derr != nil {
			if isNotFound(derr) || isForbidden(derr) {
				continue
			}
			return fmt.Errorf("delete disk %s: %w", diskName, derr)
		}
		if werr := waitOp(ctx, op, "delete disk "+diskName); werr != nil && !isNotFound(werr) {
			return werr
		}
	}
	return nil
}

func isBNKPVCDisk(d *computepb.Disk, clusterName string) bool {
	if d == nil {
		return false
	}
	if labels := d.GetLabels(); clusterName != "" && labels != nil {
		if labels["goog-gke-cluster"] == clusterName {
			return true
		}
	}
	desc := d.GetDescription()
	if desc == "" {
		return false
	}
	var meta map[string]string
	if err := json.Unmarshal([]byte(desc), &meta); err == nil {
		switch meta["kubernetes.io/created-for/pvc/namespace"] {
		case "f5-bnk", "f5-cne-system", "f5-cne-core", "f5-utils", "aigateway":
			return true
		}
		pvcName := meta["kubernetes.io/created-for/pvc/name"]
		if strings.HasPrefix(pvcName, "data-f5-dssm-db-") ||
			pvcName == "cluster-wide-controller" ||
			strings.HasPrefix(pvcName, "data-f5-aigateway-") {
			return true
		}
	}
	return false
}

var stuckFinalizerGVRs = []schema.GroupVersionResource{
	{Group: "k8s.f5.com", Version: "v1", Resource: "cneinstances"},
	{Group: "k8s.f5.com", Version: "v1", Resource: "f5spkvlan"},
	{Group: "k8s.f5.com", Version: "v1", Resource: "f5spkvlans"},
	{Group: "k8s.f5.com", Version: "v1", Resource: "f5spkstaticroutes"},
	{Group: "k8s.f5.com", Version: "v1", Resource: "f5spksnatpools"},
	{Group: "k8s.f5.com", Version: "v1", Resource: "f5spkegresses"},
	{Group: "fic.f5.com", Version: "v1alpha1", Resource: "infras"},
	{Group: "fic.f5.com", Version: "v1alpha1", Resource: "gatewaysettings"},
	{Group: "fic.f5.com", Version: "v1alpha1", Resource: "egressgateways"},
	{Group: "fic.f5.com", Version: "v1alpha1", Resource: "l4routes"},
	{Group: "fic.f5.com", Version: "v1", Resource: "infras"},
	{Group: "fic.f5.com", Version: "v1", Resource: "gatewaysettings"},
	{Group: "fic.f5.com", Version: "v1", Resource: "egressgateways"},
	{Group: "fic.f5.com", Version: "v1", Resource: "l4routes"},
	{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "infras"},
	{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "gatewaysettings"},
	{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "egressgateways"},
	{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "l4routes"},
	{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "l4globalpolicies"},
	{Group: "gateway.k8s.f5.com", Version: "v1alpha1", Resource: "apppolicies"},
	{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "infras"},
	{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "gatewaysettings"},
	{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "egressgateways"},
	{Group: "gateway.k8s.f5.com", Version: "v1", Resource: "l4routes"},
	{Group: "gateway.k8s.f5net.com", Version: "v1", Resource: "l4routes"},
	{Group: "gateway.k8s.f5net.com", Version: "v1", Resource: "gatewaysettings"},
	{Group: "k8s.f5net.com", Version: "v1", Resource: "infras"},
	{Group: "k8s.f5net.com", Version: "v1", Resource: "egressgateways"},
	{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-big-dns-caches"},
	{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-big-persistence-profiles"},
	{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-spk-egress"},
	{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-spk-egresses"},
	{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-spk-snatpools"},
	{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-spk-staticroutes"},
	{Group: "k8s.f5net.com", Version: "v1", Resource: "f5-spk-vlans"},
	{Group: "k8s.f5net.com", Version: "v1", Resource: "licenses"},
	{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "airoutepolicies"},
	{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "airoutes"},
	{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aigateways"},
	{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aigatewayprofiles"},
	{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aimodels"},
	{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aiproviders"},
	{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aicredentials"},
	{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aitenants"},
	{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"},
	{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"},
	{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "grpcroutes"},
}

// stripStuckFinalizersWithClient clears metadata.finalizers on any F5 / BNK
// custom resources in ns that remain after CNEInstance and Helm releases have
// been removed.
func stripStuckFinalizersWithClient(ctx context.Context, dyn dynamic.Interface, ns string) error {
	if dyn == nil {
		return nil
	}
	patchBytes, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"finalizers": nil,
		},
	})
	for _, gvr := range stuckFinalizerGVRs {
		list, err := dyn.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			continue
		}
		for _, item := range list.Items {
			if len(item.GetFinalizers()) == 0 {
				continue
			}
			if _, err := dyn.Resource(gvr).Namespace(ns).Patch(
				ctx, item.GetName(), types.MergePatchType, patchBytes, metav1.PatchOptions{},
			); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("strip finalizers on %s/%s: %w", gvr.Resource, item.GetName(), err)
			}
		}
	}
	tenantGVR := schema.GroupVersionResource{Group: "inference.k8s.f5.com", Version: "v1alpha1", Resource: "aitenants"}
	if list, err := dyn.Resource(tenantGVR).List(ctx, metav1.ListOptions{}); err == nil && list != nil {
		for _, item := range list.Items {
			if len(item.GetFinalizers()) == 0 {
				continue
			}
			_, _ = dyn.Resource(tenantGVR).Patch(
				ctx, item.GetName(), types.MergePatchType, patchBytes, metav1.PatchOptions{},
			)
		}
	}
	return nil
}
