// Package gcp bundles the GCP + Kubernetes clients used by the phase
// functions (internal/gcp/phases, added in Task B/C). See
// docs/SPEC-go-tracer.md §7 for why the SDK surface is wrapped in narrow
// interfaces: the concrete cloud.google.com/go clients aren't interfaces, so
// tests inject hand-rolled fakes against the interfaces defined here instead.
package gcp

import (
	"context"
	"fmt"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	container "cloud.google.com/go/container/apiv1"
	containerpb "cloud.google.com/go/container/apiv1/containerpb"
	gax "github.com/googleapis/gax-go/v2"
	"google.golang.org/api/iterator"
	serviceusage "google.golang.org/api/serviceusage/v1"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"

	"github.com/JLCode-tech/bnkctl/pkg/k8s"
)

// NetworksAPI is the subset of compute.NetworksClient used by Phase10.
type NetworksAPI interface {
	Insert(ctx context.Context, req *computepb.InsertNetworkRequest, opts ...gax.CallOption) (*compute.Operation, error)
	Get(ctx context.Context, req *computepb.GetNetworkRequest, opts ...gax.CallOption) (*computepb.Network, error)
	Patch(ctx context.Context, req *computepb.PatchNetworkRequest, opts ...gax.CallOption) (*compute.Operation, error)
	Delete(ctx context.Context, req *computepb.DeleteNetworkRequest, opts ...gax.CallOption) (*compute.Operation, error)
}

// SubnetworksAPI is the subset of compute.SubnetworksClient used by Phase10.
type SubnetworksAPI interface {
	Insert(ctx context.Context, req *computepb.InsertSubnetworkRequest, opts ...gax.CallOption) (*compute.Operation, error)
	Get(ctx context.Context, req *computepb.GetSubnetworkRequest, opts ...gax.CallOption) (*computepb.Subnetwork, error)
	Patch(ctx context.Context, req *computepb.PatchSubnetworkRequest, opts ...gax.CallOption) (*compute.Operation, error)
	Delete(ctx context.Context, req *computepb.DeleteSubnetworkRequest, opts ...gax.CallOption) (*compute.Operation, error)
	ListAll(ctx context.Context, project, region string) ([]*computepb.Subnetwork, error)
}

// FirewallsAPI is the subset of compute.FirewallsClient used by Phase10.
type FirewallsAPI interface {
	Insert(ctx context.Context, req *computepb.InsertFirewallRequest, opts ...gax.CallOption) (*compute.Operation, error)
	Get(ctx context.Context, req *computepb.GetFirewallRequest, opts ...gax.CallOption) (*computepb.Firewall, error)
	Patch(ctx context.Context, req *computepb.PatchFirewallRequest, opts ...gax.CallOption) (*compute.Operation, error)
	Delete(ctx context.Context, req *computepb.DeleteFirewallRequest, opts ...gax.CallOption) (*compute.Operation, error)
	ListAll(ctx context.Context, project string) ([]*computepb.Firewall, error)
}

// RoutersAPI is the subset of compute.RoutersClient used by Phase10 (Cloud
// Router + NAT). Insert tolerates a 403 compute.routers.create per spec §9.5.
// Patch is used to add a NAT to a router that already exists but has no NAT
// yet (the router-exists/NAT-missing case bash's net::create_nat also
// handles) — added in Task B since Insert alone cannot cover that case.
type RoutersAPI interface {
	Insert(ctx context.Context, req *computepb.InsertRouterRequest, opts ...gax.CallOption) (*compute.Operation, error)
	Get(ctx context.Context, req *computepb.GetRouterRequest, opts ...gax.CallOption) (*computepb.Router, error)
	Patch(ctx context.Context, req *computepb.PatchRouterRequest, opts ...gax.CallOption) (*compute.Operation, error)
	Delete(ctx context.Context, req *computepb.DeleteRouterRequest, opts ...gax.CallOption) (*compute.Operation, error)
	ListAll(ctx context.Context, project, region string) ([]*computepb.Router, error)
}

// InstancesAPI is the subset of compute.InstancesClient used by Phase25
// (iface/MAC/PCI discovery via NetworkInterfaces + AliasIpRanges).
type InstancesAPI interface {
	Get(ctx context.Context, req *computepb.GetInstanceRequest, opts ...gax.CallOption) (*computepb.Instance, error)
	Insert(ctx context.Context, req *computepb.InsertInstanceRequest, opts ...gax.CallOption) (*compute.Operation, error)
	Delete(ctx context.Context, req *computepb.DeleteInstanceRequest, opts ...gax.CallOption) (*compute.Operation, error)
}

// DisksAPI is the subset of compute.DisksClient used by PhaseDownNetworks to
// clean up unattached GCE persistent disks (pvc-*) left behind by BNK PVCs
// after GKE cluster deletion.
type DisksAPI interface {
	Delete(ctx context.Context, req *computepb.DeleteDiskRequest, opts ...gax.CallOption) (*compute.Operation, error)
	ListAll(ctx context.Context, project, zone string) ([]*computepb.Disk, error)
}

// RegionsAPI is the subset of compute.RegionsClient used by Phase00's
// best-effort quota check (CPUS / IN_USE_ADDRESSES headroom, warn-only).
type RegionsAPI interface {
	Get(ctx context.Context, req *computepb.GetRegionRequest, opts ...gax.CallOption) (*computepb.Region, error)
}

// ServiceUsageAPI is the subset of the serviceusage v1 REST client used by
// Phase00 to check compute.googleapis.com / container.googleapis.com are
// enabled on the target project. Narrow interface over
// *serviceusage.Service so Phase00 can be unit-tested without a live API
// call (see docs/SPEC-go-tracer.md §7).
type ServiceUsageAPI interface {
	GetService(ctx context.Context, name string) (*serviceusage.GoogleApiServiceusageV1Service, error)
}

// GKEAPI is the subset of container.ClusterManagerClient used by Phase20 +
// PhaseDownCluster. LRO mutations return *containerpb.Operation — callers
// poll GetOperation until DONE.
type GKEAPI interface {
	CreateCluster(ctx context.Context, req *containerpb.CreateClusterRequest, opts ...gax.CallOption) (*containerpb.Operation, error)
	GetCluster(ctx context.Context, req *containerpb.GetClusterRequest, opts ...gax.CallOption) (*containerpb.Cluster, error)
	DeleteCluster(ctx context.Context, req *containerpb.DeleteClusterRequest, opts ...gax.CallOption) (*containerpb.Operation, error)
	CreateNodePool(ctx context.Context, req *containerpb.CreateNodePoolRequest, opts ...gax.CallOption) (*containerpb.Operation, error)
	GetNodePool(ctx context.Context, req *containerpb.GetNodePoolRequest, opts ...gax.CallOption) (*containerpb.NodePool, error)
	GetOperation(ctx context.Context, req *containerpb.GetOperationRequest, opts ...gax.CallOption) (*containerpb.Operation, error)
}

// Clients bundles the GCP service clients + (once attached) the Kubernetes
// clients needed by phases. Later tasks add fields here without changing
// existing phase signatures.
type Clients struct {
	Networks     NetworksAPI
	Subnetworks  SubnetworksAPI
	Firewalls    FirewallsAPI
	Routers      RoutersAPI
	Instances    InstancesAPI
	Disks        DisksAPI
	GKE          GKEAPI
	Regions      RegionsAPI
	ServiceUsage ServiceUsageAPI
	Project      string

	// K8s is the typed Kubernetes client used by the BNK-install phases
	// (Task C). Nil until AttachK8s is called (after Phase20 — the GKE
	// cluster must be RUNNING before a rest.Config can be built).
	K8s kubernetes.Interface

	// Dynamic is the unstructured Kubernetes client used for F5 CRs and NADs,
	// which are not in the typed client-go scheme.
	Dynamic dynamic.Interface

	// RESTMapper resolves apiVersion+kind -> GroupVersionResource using a
	// live discovery client. Constructed alongside K8s by AttachK8s.
	RESTMapper meta.RESTMapper

	// RestConfig is the rest.Config AttachK8s built K8s/Dynamic/RESTMapper
	// from. Kept alongside the derived clients (Task D) so phases needing raw
	// transport access — Phase50's pod-exec verification, which must build a
	// remotecommand SPDY executor directly against the API server — don't
	// need to rebuild it from GetCluster a second time.
	RestConfig *rest.Config
}

// NewClients constructs real GCP SDK clients over Application Default
// Credentials. No kubeconfig, no gcloud/kubectl shellout — see
// docs/SPEC-go-tracer.md §5.
func NewClients(ctx context.Context, project string) (*Clients, error) {
	if project == "" {
		return nil, fmt.Errorf("gcp: project is empty; set metadata.project in cluster.yaml")
	}

	networks, err := compute.NewNetworksRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: networks client: %w", err)
	}
	subnetworks, err := compute.NewSubnetworksRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: subnetworks client: %w", err)
	}
	firewalls, err := compute.NewFirewallsRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: firewalls client: %w", err)
	}
	routers, err := compute.NewRoutersRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: routers client: %w", err)
	}
	instances, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: instances client: %w", err)
	}
	disks, err := compute.NewDisksRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: disks client: %w", err)
	}
	regions, err := compute.NewRegionsRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: regions client: %w", err)
	}
	gke, err := container.NewClusterManagerRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: cluster manager client: %w", err)
	}
	su, err := serviceusage.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: serviceusage client: %w", err)
	}

	return &Clients{
		Networks:     networks,
		Subnetworks:  &realSubnetworks{client: subnetworks},
		Firewalls:    &realFirewalls{client: firewalls},
		Routers:      &realRouters{client: routers},
		Instances:    instances,
		Disks:        &realDisks{client: disks},
		Regions:      regions,
		GKE:          gke,
		ServiceUsage: &realServiceUsage{svc: su},
		Project:      project,
	}, nil
}

// realServiceUsage adapts *serviceusage.Service to ServiceUsageAPI.
type realServiceUsage struct {
	svc *serviceusage.Service
}

func (r *realServiceUsage) GetService(ctx context.Context, name string) (*serviceusage.GoogleApiServiceusageV1Service, error) {
	return r.svc.Services.Get(name).Context(ctx).Do()
}

// AttachK8s builds a typed kubernetes.Interface, a dynamic.Interface, and a
// live RESTMapper from a rest.Config built by BuildRESTConfig (auth.go) for
// clusterName/zone. Called by the up orchestrator AFTER the GKE cluster is
// RUNNING (i.e. after Phase20).
func (c *Clients) AttachK8s(ctx context.Context, clusterName, zone string) error {
	if c.GKE == nil {
		return fmt.Errorf("gcp: AttachK8s(%s, %s): Clients.GKE is nil", clusterName, zone)
	}
	cluster, err := c.GKE.GetCluster(ctx, &containerpb.GetClusterRequest{
		Name: fmt.Sprintf("projects/%s/locations/%s/clusters/%s", c.Project, zone, clusterName),
	})
	if err != nil {
		return fmt.Errorf("gcp: AttachK8s(%s, %s): GetCluster: %w", clusterName, zone, err)
	}
	cfg, err := BuildRESTConfig(ctx, cluster)
	if err != nil {
		return fmt.Errorf("gcp: AttachK8s(%s, %s): %w", clusterName, zone, err)
	}
	return attachK8sFromConfig(c, cfg)
}

// attachK8sFromConfig builds the typed/dynamic/RESTMapper trio from an
// already-constructed rest.Config and stores them on c. Task B's AttachK8s
// calls this after BuildRESTConfig(ctx, cluster) succeeds — kept as a
// standalone function (rather than inlined) so it can be unit-tested against
// a fake rest.Config without a live GKE cluster.
func attachK8sFromConfig(c *Clients, cfg *rest.Config) error {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("gcp: AttachK8s: build typed clientset: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("gcp: AttachK8s: build dynamic client: %w", err)
	}
	if dyn == nil {
		return fmt.Errorf("gcp: AttachK8s: dynamic client is nil")
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return fmt.Errorf("gcp: AttachK8s: build discovery client: %w", err)
	}
	c.K8s = cs
	c.Dynamic = dyn
	c.RESTMapper = restmapper.NewDeferredDiscoveryRESTMapper(k8s.NewNoopCachedDiscovery(disc))
	c.RestConfig = cfg
	return nil
}

type realSubnetworks struct {
	client *compute.SubnetworksClient
}

func (r *realSubnetworks) Insert(ctx context.Context, req *computepb.InsertSubnetworkRequest, opts ...gax.CallOption) (*compute.Operation, error) {
	return r.client.Insert(ctx, req, opts...)
}

func (r *realSubnetworks) Get(ctx context.Context, req *computepb.GetSubnetworkRequest, opts ...gax.CallOption) (*computepb.Subnetwork, error) {
	return r.client.Get(ctx, req, opts...)
}

func (r *realSubnetworks) Patch(ctx context.Context, req *computepb.PatchSubnetworkRequest, opts ...gax.CallOption) (*compute.Operation, error) {
	return r.client.Patch(ctx, req, opts...)
}

func (r *realSubnetworks) Delete(ctx context.Context, req *computepb.DeleteSubnetworkRequest, opts ...gax.CallOption) (*compute.Operation, error) {
	return r.client.Delete(ctx, req, opts...)
}

func (r *realSubnetworks) ListAll(ctx context.Context, project, region string) ([]*computepb.Subnetwork, error) {
	it := r.client.List(ctx, &computepb.ListSubnetworksRequest{Project: project, Region: region})
	var res []*computepb.Subnetwork
	for {
		s, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		res = append(res, s)
	}
	return res, nil
}

type realFirewalls struct {
	client *compute.FirewallsClient
}

func (r *realFirewalls) Insert(ctx context.Context, req *computepb.InsertFirewallRequest, opts ...gax.CallOption) (*compute.Operation, error) {
	return r.client.Insert(ctx, req, opts...)
}

func (r *realFirewalls) Get(ctx context.Context, req *computepb.GetFirewallRequest, opts ...gax.CallOption) (*computepb.Firewall, error) {
	return r.client.Get(ctx, req, opts...)
}

func (r *realFirewalls) Patch(ctx context.Context, req *computepb.PatchFirewallRequest, opts ...gax.CallOption) (*compute.Operation, error) {
	return r.client.Patch(ctx, req, opts...)
}

func (r *realFirewalls) Delete(ctx context.Context, req *computepb.DeleteFirewallRequest, opts ...gax.CallOption) (*compute.Operation, error) {
	return r.client.Delete(ctx, req, opts...)
}

func (r *realFirewalls) ListAll(ctx context.Context, project string) ([]*computepb.Firewall, error) {
	it := r.client.List(ctx, &computepb.ListFirewallsRequest{Project: project})
	var res []*computepb.Firewall
	for {
		f, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		res = append(res, f)
	}
	return res, nil
}

type realRouters struct {
	client *compute.RoutersClient
}

func (r *realRouters) Insert(ctx context.Context, req *computepb.InsertRouterRequest, opts ...gax.CallOption) (*compute.Operation, error) {
	return r.client.Insert(ctx, req, opts...)
}

func (r *realRouters) Get(ctx context.Context, req *computepb.GetRouterRequest, opts ...gax.CallOption) (*computepb.Router, error) {
	return r.client.Get(ctx, req, opts...)
}

func (r *realRouters) Patch(ctx context.Context, req *computepb.PatchRouterRequest, opts ...gax.CallOption) (*compute.Operation, error) {
	return r.client.Patch(ctx, req, opts...)
}

func (r *realRouters) Delete(ctx context.Context, req *computepb.DeleteRouterRequest, opts ...gax.CallOption) (*compute.Operation, error) {
	return r.client.Delete(ctx, req, opts...)
}

func (r *realRouters) ListAll(ctx context.Context, project, region string) ([]*computepb.Router, error) {
	it := r.client.List(ctx, &computepb.ListRoutersRequest{Project: project, Region: region})
	var res []*computepb.Router
	for {
		router, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		res = append(res, router)
	}
	return res, nil
}

type realDisks struct {
	client *compute.DisksClient
}

func (r *realDisks) Delete(ctx context.Context, req *computepb.DeleteDiskRequest, opts ...gax.CallOption) (*compute.Operation, error) {
	return r.client.Delete(ctx, req, opts...)
}

func (r *realDisks) ListAll(ctx context.Context, project, zone string) ([]*computepb.Disk, error) {
	it := r.client.List(ctx, &computepb.ListDisksRequest{Project: project, Zone: zone})
	var res []*computepb.Disk
	for {
		d, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		res = append(res, d)
	}
	return res, nil
}
