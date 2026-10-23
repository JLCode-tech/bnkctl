package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	containerpb "cloud.google.com/go/container/apiv1/containerpb"
	gax "github.com/googleapis/gax-go/v2"
	serviceusage "google.golang.org/api/serviceusage/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakek8s "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/JLCode-tech/bnkctl/pkg/forge/mcp"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

var (
	errNotFound         = errors.New("googleapi: got HTTP response code 404 with body: not found")
	errPermissionDenied = errors.New("googleapi: got HTTP response code 403 with body: permission denied")
)

type fakeNetworks struct {
	existing    map[string]string
	insertCalls []string
	insertErr   map[string]error
	patchCalls  []string
	deleteCalls []string
}

func (f *fakeNetworks) Get(_ context.Context, req *computepb.GetNetworkRequest, _ ...gax.CallOption) (*computepb.Network, error) {
	if desc, ok := f.existing[req.GetNetwork()]; ok {
		net := &computepb.Network{Name: strPtr(req.GetNetwork())}
		if desc != "" {
			net.Description = strPtr(desc)
		}
		return net, nil
	}
	return nil, errNotFound
}

func (f *fakeNetworks) Insert(_ context.Context, req *computepb.InsertNetworkRequest, _ ...gax.CallOption) (*compute.Operation, error) {
	name := req.GetNetworkResource().GetName()
	f.insertCalls = append(f.insertCalls, name)
	if f.insertErr != nil {
		if err, ok := f.insertErr[name]; ok {
			return nil, err
		}
	}
	return nil, nil
}

func (f *fakeNetworks) Patch(_ context.Context, req *computepb.PatchNetworkRequest, _ ...gax.CallOption) (*compute.Operation, error) {
	f.patchCalls = append(f.patchCalls, req.GetNetwork())
	return nil, nil
}

func (f *fakeNetworks) Delete(_ context.Context, req *computepb.DeleteNetworkRequest, _ ...gax.CallOption) (*compute.Operation, error) {
	f.deleteCalls = append(f.deleteCalls, req.GetNetwork())
	return nil, nil
}

type fakeSubnetworks struct {
	existing    map[string]string
	insertCalls []string
	patchCalls  []string
	deleteCalls []string
}

func (f *fakeSubnetworks) Get(_ context.Context, req *computepb.GetSubnetworkRequest, _ ...gax.CallOption) (*computepb.Subnetwork, error) {
	if desc, ok := f.existing[req.GetSubnetwork()]; ok {
		sub := &computepb.Subnetwork{Name: strPtr(req.GetSubnetwork())}
		if desc != "" {
			sub.Description = strPtr(desc)
		}
		return sub, nil
	}
	return nil, errNotFound
}

func (f *fakeSubnetworks) Insert(_ context.Context, req *computepb.InsertSubnetworkRequest, _ ...gax.CallOption) (*compute.Operation, error) {
	f.insertCalls = append(f.insertCalls, req.GetSubnetworkResource().GetName())
	return nil, nil
}

func (f *fakeSubnetworks) Patch(_ context.Context, req *computepb.PatchSubnetworkRequest, _ ...gax.CallOption) (*compute.Operation, error) {
	f.patchCalls = append(f.patchCalls, req.GetSubnetwork())
	return nil, nil
}

func (f *fakeSubnetworks) ListAll(_ context.Context, project, _ string) ([]*computepb.Subnetwork, error) {
	var res []*computepb.Subnetwork
	for name, desc := range f.existing {
		sub := &computepb.Subnetwork{Name: strPtr(name), Network: strPtr(networkSelfLink(project, "vpc1"))}
		if desc != "" {
			sub.Description = strPtr(desc)
		}
		res = append(res, sub)
	}
	return res, nil
}

func (f *fakeSubnetworks) Delete(_ context.Context, req *computepb.DeleteSubnetworkRequest, _ ...gax.CallOption) (*compute.Operation, error) {
	f.deleteCalls = append(f.deleteCalls, req.GetSubnetwork())
	return nil, nil
}

type fakeFirewalls struct {
	existing    map[string]string
	insertCalls []string
	patchCalls  []string
	deleteCalls []string
}

func (f *fakeFirewalls) Get(_ context.Context, req *computepb.GetFirewallRequest, _ ...gax.CallOption) (*computepb.Firewall, error) {
	if desc, ok := f.existing[req.GetFirewall()]; ok {
		fw := &computepb.Firewall{Name: strPtr(req.GetFirewall())}
		if desc != "" {
			fw.Description = strPtr(desc)
		}
		return fw, nil
	}
	return nil, errNotFound
}

func (f *fakeFirewalls) Insert(_ context.Context, req *computepb.InsertFirewallRequest, _ ...gax.CallOption) (*compute.Operation, error) {
	f.insertCalls = append(f.insertCalls, req.GetFirewallResource().GetName())
	return nil, nil
}

func (f *fakeFirewalls) Patch(_ context.Context, req *computepb.PatchFirewallRequest, _ ...gax.CallOption) (*compute.Operation, error) {
	f.patchCalls = append(f.patchCalls, req.GetFirewall())
	return nil, nil
}

func (f *fakeFirewalls) ListAll(_ context.Context, project string) ([]*computepb.Firewall, error) {
	var res []*computepb.Firewall
	for name, desc := range f.existing {
		fw := &computepb.Firewall{Name: strPtr(name), Network: strPtr(networkSelfLink(project, "vpc1"))}
		if desc != "" {
			fw.Description = strPtr(desc)
		}
		res = append(res, fw)
	}
	return res, nil
}

func (f *fakeFirewalls) Delete(_ context.Context, req *computepb.DeleteFirewallRequest, _ ...gax.CallOption) (*compute.Operation, error) {
	f.deleteCalls = append(f.deleteCalls, req.GetFirewall())
	return nil, nil
}

type fakeRouters struct {
	existing    map[string]*computepb.Router
	insertCalls []string
	insertErr   map[string]error
	patchCalls  []string
	patchErr    map[string]error
	deleteCalls []string
}

func (f *fakeRouters) Get(_ context.Context, req *computepb.GetRouterRequest, _ ...gax.CallOption) (*computepb.Router, error) {
	if r, ok := f.existing[req.GetRouter()]; ok {
		return r, nil
	}
	return nil, errNotFound
}

func (f *fakeRouters) Insert(_ context.Context, req *computepb.InsertRouterRequest, _ ...gax.CallOption) (*compute.Operation, error) {
	name := req.GetRouterResource().GetName()
	f.insertCalls = append(f.insertCalls, name)
	if f.insertErr != nil {
		if err, ok := f.insertErr[name]; ok {
			return nil, err
		}
	}
	return nil, nil
}

func (f *fakeRouters) Patch(_ context.Context, req *computepb.PatchRouterRequest, _ ...gax.CallOption) (*compute.Operation, error) {
	name := req.GetRouter()
	f.patchCalls = append(f.patchCalls, name)
	if f.patchErr != nil {
		if err, ok := f.patchErr[name]; ok {
			return nil, err
		}
	}
	return nil, nil
}

func (f *fakeRouters) ListAll(_ context.Context, _, _ string) ([]*computepb.Router, error) {
	var res []*computepb.Router
	for _, r := range f.existing {
		res = append(res, r)
	}
	return res, nil
}

func (f *fakeRouters) Delete(_ context.Context, req *computepb.DeleteRouterRequest, _ ...gax.CallOption) (*compute.Operation, error) {
	f.deleteCalls = append(f.deleteCalls, req.GetRouter())
	return nil, nil
}

type fakeInstances struct {
	byName      map[string]*computepb.Instance
	insertCalls []string
	deleteCalls []string
}

func (f *fakeInstances) Get(_ context.Context, req *computepb.GetInstanceRequest, _ ...gax.CallOption) (*computepb.Instance, error) {
	if inst, ok := f.byName[req.GetInstance()]; ok {
		return inst, nil
	}
	return nil, errNotFound
}

func (f *fakeInstances) Insert(_ context.Context, req *computepb.InsertInstanceRequest, _ ...gax.CallOption) (*compute.Operation, error) {
	inst := req.GetInstanceResource()
	f.insertCalls = append(f.insertCalls, inst.GetName())
	if f.byName == nil {
		f.byName = map[string]*computepb.Instance{}
	}
	f.byName[inst.GetName()] = inst
	return nil, nil
}

func (f *fakeInstances) Delete(_ context.Context, req *computepb.DeleteInstanceRequest, _ ...gax.CallOption) (*compute.Operation, error) {
	f.deleteCalls = append(f.deleteCalls, req.GetInstance())
	delete(f.byName, req.GetInstance())
	return nil, nil
}

type fakeRegions struct {
	region *computepb.Region
	err    error
}

func (f *fakeRegions) Get(_ context.Context, _ *computepb.GetRegionRequest, _ ...gax.CallOption) (*computepb.Region, error) {
	return f.region, f.err
}

type fakeServiceUsage struct {
	states map[string]string
	err    error
}

func (f *fakeServiceUsage) GetService(_ context.Context, name string) (*serviceusage.GoogleApiServiceusageV1Service, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &serviceusage.GoogleApiServiceusageV1Service{State: f.states[name]}, nil
}

type fakeGKE struct {
	clusters  map[string]*containerpb.Cluster
	nodePools map[string]*containerpb.NodePool

	createClusterCalls   []string
	createNodePoolCalls  []string
	deleteClusterCalls   []string
	createClusterErr     error
	createNodePoolErr    error
	deleteClusterErr     error
	getOperationStatuses map[string]containerpb.Operation_Status
}

func (f *fakeGKE) CreateCluster(_ context.Context, req *containerpb.CreateClusterRequest, _ ...gax.CallOption) (*containerpb.Operation, error) {
	f.createClusterCalls = append(f.createClusterCalls, req.GetCluster().GetName())
	if f.createClusterErr != nil {
		return nil, f.createClusterErr
	}
	if f.clusters == nil {
		f.clusters = map[string]*containerpb.Cluster{}
	}
	f.clusters[req.GetCluster().GetName()] = &containerpb.Cluster{
		Name:   req.GetCluster().GetName(),
		Status: containerpb.Cluster_RUNNING,
	}
	return &containerpb.Operation{Name: "op-create-cluster", Status: containerpb.Operation_DONE}, nil
}

func (f *fakeGKE) GetCluster(_ context.Context, req *containerpb.GetClusterRequest, _ ...gax.CallOption) (*containerpb.Cluster, error) {
	parts := strings.Split(req.GetName(), "/")
	shortName := parts[len(parts)-1]
	if c, ok := f.clusters[shortName]; ok {
		return c, nil
	}
	return nil, errNotFound
}

func (f *fakeGKE) DeleteCluster(_ context.Context, req *containerpb.DeleteClusterRequest, _ ...gax.CallOption) (*containerpb.Operation, error) {
	parts := strings.Split(req.GetName(), "/")
	shortName := parts[len(parts)-1]
	f.deleteClusterCalls = append(f.deleteClusterCalls, shortName)
	if f.deleteClusterErr != nil {
		return nil, f.deleteClusterErr
	}
	delete(f.clusters, shortName)
	return &containerpb.Operation{Name: "op-delete-cluster", Status: containerpb.Operation_DONE}, nil
}

func (f *fakeGKE) CreateNodePool(_ context.Context, req *containerpb.CreateNodePoolRequest, _ ...gax.CallOption) (*containerpb.Operation, error) {
	f.createNodePoolCalls = append(f.createNodePoolCalls, req.GetNodePool().GetName())
	if f.createNodePoolErr != nil {
		return nil, f.createNodePoolErr
	}
	if f.nodePools == nil {
		f.nodePools = map[string]*containerpb.NodePool{}
	}
	f.nodePools[req.GetNodePool().GetName()] = &containerpb.NodePool{
		Name:   req.GetNodePool().GetName(),
		Status: containerpb.NodePool_RUNNING,
	}
	return &containerpb.Operation{Name: "op-create-nodepool-" + req.GetNodePool().GetName(), Status: containerpb.Operation_DONE}, nil
}

func (f *fakeGKE) GetNodePool(_ context.Context, req *containerpb.GetNodePoolRequest, _ ...gax.CallOption) (*containerpb.NodePool, error) {
	parts := strings.Split(req.GetName(), "/")
	shortName := parts[len(parts)-1]
	if p, ok := f.nodePools[shortName]; ok {
		return p, nil
	}
	return nil, errNotFound
}

func (f *fakeGKE) GetOperation(_ context.Context, req *containerpb.GetOperationRequest, _ ...gax.CallOption) (*containerpb.Operation, error) {
	status := containerpb.Operation_DONE
	parts := strings.Split(req.GetName(), "/")
	shortName := parts[len(parts)-1]
	if f.getOperationStatuses != nil {
		if s, ok := f.getOperationStatuses[shortName]; ok {
			status = s
		}
	}
	return &containerpb.Operation{Name: req.GetName(), Status: status}, nil
}

func strPtr(s string) *string { return &s }

var _ NetworksAPI = (*fakeNetworks)(nil)
var _ SubnetworksAPI = (*fakeSubnetworks)(nil)
var _ FirewallsAPI = (*fakeFirewalls)(nil)
var _ RoutersAPI = (*fakeRouters)(nil)
var _ InstancesAPI = (*fakeInstances)(nil)
var _ RegionsAPI = (*fakeRegions)(nil)
var _ ServiceUsageAPI = (*fakeServiceUsage)(nil)
var _ GKEAPI = (*fakeGKE)(nil)

const testTMMPollInterval = time.Millisecond

func testCluster() *intent.Cluster {
	return &intent.Cluster{
		Metadata: intent.Metadata{Name: "bnk-hostdev", Project: "my-gcp-project", Zone: "us-central1-a"},
		Pattern:  intent.PatternHostDevice,
		Network: intent.Network{
			Main: intent.MainNetworkSpec{
				VPC: "bnkhd-vpc", Subnet: "bnkhd-subnet", CIDR: "10.60.0.0/24",
				PodsRangeName: "bnkhd-pods", PodsCIDR: "10.66.0.0/16",
				SvcsRangeName: "bnkhd-svcs", SvcsCIDR: "10.67.0.0/20",
			},
			External: intent.DataplaneSpec{
				VPC: "bnkhd-ext", Subnet: "bnkhd-ext", CIDR: "10.30.0.0/24", MTU: 8896,
				PodsRangeName: "tmm-ext-pods", PodsCIDR: "10.31.0.0/24", UnderlayCIDR: "10.30.0.0/16",
			},
			Internal: intent.DataplaneSpec{
				VPC: "bnkhd-int", Subnet: "bnkhd-int", CIDR: "10.50.0.0/24", MTU: 8896,
				PodsRangeName: "tmm-int-pods", PodsCIDR: "10.51.0.0/24",
			},
			AllowInternalCidr: "10.0.0.0/8",
		},
		ClusterSpec: &intent.ClusterSpec{
			KubernetesVersion: "1.35",
			DefaultPool:       intent.NodePoolSpec{MachineType: "e2-standard-4", NodeCount: 3},
			TmmPool:           intent.NodePoolSpec{Name: "tmm-pool", MachineType: "e2-standard-8", NodeCount: 1, MaxPodsPerNode: 8},
			AgentPool:         intent.NodePoolSpec{Name: "agent-pool", MachineType: "e2-standard-4", NodeCount: 1, MaxPodsPerNode: 8},
		},
		Bnk: &intent.BnkSpec{
			ManifestVersion: "2.4.0-3.2882.0",
			TmmMtu:          1460,
			FarSecret: intent.FarSecretSpec{
				Source:          "kubecontext",
				SourceContext:   "some-other-cluster",
				SourceNamespace: "f5-cne-core",
				Namespaces:      []string{"f5-cne-core", "f5-cne-system", "f5-bnk-instance", "f5-utils", "default"},
			},
		},
		Testing: intent.TestingSpec{
			Jumphost: &intent.JumphostSpec{Enabled: true},
		},
		Egress: intent.EgressSpec{
			TestNamespace: "egress-test",
			VxlanPort:     5789,
			VxlanKey:      100,
			VxlanMtu:      1410,
		},
		Region:               "us-central1",
		ExtGwIP:              "10.30.0.1",
		IntGwIP:              "10.50.0.1",
		ExtUnderlayDest:      "10.30.0.0",
		ExtUnderlayPrefixLen: 16,
		MainPodsDest:         "10.66.0.0",
		MainPodsPrefixLen:    16,
		MainSvcsDest:         "10.67.0.0",
		MainSvcsPrefixLen:    20,
	}
}

func testState(t *testing.T) *state.State {
	t.Helper()
	st, err := state.Load(t.TempDir())
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	return st
}

func testClients10() (*Clients, *fakeNetworks, *fakeSubnetworks, *fakeFirewalls, *fakeRouters) {
	networks := &fakeNetworks{existing: map[string]string{}}
	subnetworks := &fakeSubnetworks{existing: map[string]string{}}
	firewalls := &fakeFirewalls{existing: map[string]string{}}
	routers := &fakeRouters{existing: map[string]*computepb.Router{}}
	return &Clients{
		Networks:    networks,
		Subnetworks: subnetworks,
		Firewalls:   firewalls,
		Routers:     routers,
	}, networks, subnetworks, firewalls, routers
}

func TestPhase00Preflight_NilClients_SkipsChecks(t *testing.T) {
	cl := testCluster()
	st := testState(t)
	if err := Phase00Preflight(context.Background(), cl, st, nil, false); err != nil {
		t.Fatalf("Phase00Preflight(clients=nil): unexpected error: %v", err)
	}
}

func TestPhase00Preflight_WithJWT(t *testing.T) {
	tmpDir := t.TempDir()
	jwtFile := filepath.Join(tmpDir, "test.jwt")
	testToken := "eyJhbGciOiJSUzUxMiIsInR5cCI6IkpXVCIsImtpZCI6InYxIiwiamt1IjoiaHR0cHM6Ly9wcm9kdWN0LXRzdC5hcGlzLmY1bmV0d29ya3MubmV0L2VlL3YxL2tleXMvandrcyJ9.eyJzdWIiOiJUU1QtMTIzNDUiLCJpc3MiOiJGNSBJbmMuIiwiZjVfc2F0IjoxODA3ODMzNjAwfQ.sig"
	if err := os.WriteFile(jwtFile, []byte(testToken), 0o600); err != nil {
		t.Fatalf("failed writing test jwt: %v", err)
	}

	cl := testCluster()
	cl.Bnk.JwtPath = jwtFile
	st := testState(t)
	if err := Phase00Preflight(context.Background(), cl, st, nil, false); err != nil {
		t.Fatalf("Phase00Preflight with JWT failed: %v", err)
	}
}

func TestCheckAPIEnabled_DisabledAndEnabled(t *testing.T) {
	su := &fakeServiceUsage{states: map[string]string{
		"projects/my-gcp-project/services/compute.googleapis.com":   "DISABLED",
		"projects/my-gcp-project/services/container.googleapis.com": "ENABLED",
	}}
	enabled, err := checkAPIEnabled(context.Background(), su, "my-gcp-project", "compute.googleapis.com")
	if err != nil {
		t.Fatalf("checkAPIEnabled: unexpected error: %v", err)
	}
	if enabled {
		t.Error("expected compute.googleapis.com to be reported as not enabled")
	}

	enabled, err = checkAPIEnabled(context.Background(), su, "my-gcp-project", "container.googleapis.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !enabled {
		t.Error("expected enabled=true")
	}
}

func TestWarnRegionQuota(t *testing.T) {
	warnRegionQuota(context.Background(), nil, "p", "us-central1")
	limit := 16.0
	usage := 15.0
	metric := "CPUS"
	regions := &fakeRegions{region: &computepb.Region{
		Quotas: []*computepb.Quota{{Metric: &metric, Limit: &limit, Usage: &usage}},
	}}
	warnRegionQuota(context.Background(), regions, "p", "us-central1")
	warnRegionQuota(context.Background(), &fakeRegions{err: errNotFound}, "p", "us-central1")
}

func TestHasHTTPStatus_StringFallback(t *testing.T) {
	if !isNotFound(errNotFound) {
		t.Error("expected errNotFound to match isNotFound via string fallback")
	}
	if !isPermissionDenied(errPermissionDenied) {
		t.Error("expected errPermissionDenied to match isPermissionDenied via string fallback")
	}
	if isNotFound(nil) || isPermissionDenied(nil) {
		t.Error("nil error must never match a status")
	}
}

func TestPhase10Networks_CreatesWhenAbsent(t *testing.T) {
	cl := testCluster()
	st := testState(t)
	clients, networks, subnetworks, firewalls, routers := testClients10()

	if err := Phase10Networks(context.Background(), cl, st, clients, false); err != nil {
		t.Fatalf("Phase10Networks: %v", err)
	}

	for _, want := range []string{"bnkhd-vpc", "bnkhd-ext", "bnkhd-int"} {
		found := false
		for _, got := range networks.insertCalls {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("expected VPC %s to be created; insertCalls=%v", want, networks.insertCalls)
		}
	}
	for _, want := range []string{"bnkhd-subnet", "bnkhd-ext", "bnkhd-int"} {
		found := false
		for _, got := range subnetworks.insertCalls {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("expected subnet %s to be created; insertCalls=%v", want, subnetworks.insertCalls)
		}
	}
	for _, want := range []string{"bnkhd-vpc-allow-internal", "bnkhd-ext-allow-internal", "bnkhd-int-allow-internal"} {
		found := false
		for _, got := range firewalls.insertCalls {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("expected firewall %s to be created; insertCalls=%v", want, firewalls.insertCalls)
		}
	}
	for _, want := range []string{"bnkhd-ext-router", "bnkhd-int-router"} {
		found := false
		for _, got := range routers.insertCalls {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("expected router %s to be created; insertCalls=%v", want, routers.insertCalls)
		}
	}
}

func TestPhase10Networks_SkipsWhenPresent(t *testing.T) {
	cl := testCluster()
	st := testState(t)
	clients, networks, subnetworks, firewalls, routers := testClients10()

	networks.existing["bnkhd-vpc"] = ""
	networks.existing["bnkhd-ext"] = ""
	networks.existing["bnkhd-int"] = ""
	subnetworks.existing["bnkhd-subnet"] = ""
	subnetworks.existing["bnkhd-ext"] = ""
	subnetworks.existing["bnkhd-int"] = ""
	firewalls.existing["bnkhd-vpc-allow-internal"] = ""
	firewalls.existing["bnkhd-ext-allow-internal"] = ""
	firewalls.existing["bnkhd-int-allow-internal"] = ""
	natName := "bnkhd-ext-nat"
	routers.existing["bnkhd-ext-router"] = &computepb.Router{Description: strPtr("gkebnkctl-managed=true gkebnkctl-cluster=bnk-hostdev"), Nats: []*computepb.RouterNat{{Name: &natName}}}
	natNameInt := "bnkhd-int-nat"
	routers.existing["bnkhd-int-router"] = &computepb.Router{Description: strPtr("gkebnkctl-managed=true gkebnkctl-cluster=bnk-hostdev"), Nats: []*computepb.RouterNat{{Name: &natNameInt}}}

	if err := Phase10Networks(context.Background(), cl, st, clients, false); err != nil {
		t.Fatalf("Phase10Networks: %v", err)
	}

	if len(networks.insertCalls) != 0 || len(subnetworks.insertCalls) != 0 ||
		len(firewalls.insertCalls) != 0 || len(routers.insertCalls) != 0 || len(routers.patchCalls) != 0 {
		t.Errorf("expected 0 Insert/Patch calls when resources exist")
	}
}

func TestPhase10Networks_DryRun_MakesZeroMutations(t *testing.T) {
	cl := testCluster()
	st := testState(t)
	clients, networks, subnetworks, firewalls, routers := testClients10()

	if err := Phase10Networks(context.Background(), cl, st, clients, true); err != nil {
		t.Fatalf("Phase10Networks(dryRun=true): %v", err)
	}
	if len(networks.insertCalls) != 0 || len(subnetworks.insertCalls) != 0 ||
		len(firewalls.insertCalls) != 0 || len(routers.insertCalls) != 0 {
		t.Error("dry-run must make zero GCP mutations")
	}
}

func TestEnsureRouterNAT_403Tolerated(t *testing.T) {
	routers := &fakeRouters{
		existing:  map[string]*computepb.Router{},
		insertErr: map[string]error{"bnkhd-ext-router": errPermissionDenied},
	}
	err := ensureRouterNAT(context.Background(), routers, "my-gcp-project", "us-central1", "bnkhd-ext", "test-cluster")
	if err != nil {
		t.Fatalf("ensureRouterNAT: expected 403 to be tolerated (nil error), got: %v", err)
	}
	if len(routers.insertCalls) != 1 || len(routers.patchCalls) != 0 {
		t.Errorf("unexpected router calls: insert=%d patch=%d", len(routers.insertCalls), len(routers.patchCalls))
	}
}

func TestPhase15Jumphost_CreateAndDown(t *testing.T) {
	cl := testCluster()
	st := testState(t)
	instances := &fakeInstances{byName: map[string]*computepb.Instance{}}
	firewalls := &fakeFirewalls{existing: map[string]string{}}
	clients := &Clients{Instances: instances, Firewalls: firewalls}

	if err := Phase15Jumphost(context.Background(), cl, st, clients, false); err != nil {
		t.Fatalf("Phase15Jumphost: %v", err)
	}
	if len(instances.insertCalls) != 1 {
		t.Fatalf("expected 1 jumphost insert call, got %v", instances.insertCalls)
	}
	if st.Get("JUMPHOST_NAME") != "bnk-hostdev-jumphost" {
		t.Errorf("JUMPHOST_NAME = %q, want bnk-hostdev-jumphost", st.Get("JUMPHOST_NAME"))
	}

	if err := PhaseDownJumphost(context.Background(), cl, st, clients, false); err != nil {
		t.Fatalf("PhaseDownJumphost: %v", err)
	}
	if len(instances.deleteCalls) != 1 {
		t.Errorf("expected 1 jumphost delete call, got %v", instances.deleteCalls)
	}
}

func TestPhase20Cluster_DryRun_MakesZeroMutations(t *testing.T) {
	cl := testCluster()
	st := testState(t)
	gke := &fakeGKE{}
	clients := &Clients{GKE: gke}

	if err := Phase20Cluster(context.Background(), cl, st, clients, true); err != nil {
		t.Fatalf("Phase20Cluster(dryRun=true): %v", err)
	}
	if len(gke.createClusterCalls) != 0 || len(gke.createNodePoolCalls) != 0 {
		t.Error("dry-run must make zero GKE mutations")
	}
}

func TestPhase20Cluster_CreatesWhenAbsent(t *testing.T) {
	cl := testCluster()
	st := testState(t)
	gke := &fakeGKE{}
	k8sClient := fakek8s.NewClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "tmm-node-1", Labels: map[string]string{gkeNodepoolLabel: "tmm-pool"}}},
	)
	clients := &Clients{GKE: gke, K8s: k8sClient}

	if err := Phase20Cluster(context.Background(), cl, st, clients, false); err != nil {
		t.Fatalf("Phase20Cluster: %v", err)
	}
	if len(gke.createClusterCalls) != 1 || gke.createClusterCalls[0] != "bnk-hostdev" {
		t.Errorf("expected 1 CreateCluster call for bnk-hostdev, got %v", gke.createClusterCalls)
	}
	wantPools := map[string]bool{"tmm-pool": false, "agent-pool": false}
	for _, p := range gke.createNodePoolCalls {
		if _, ok := wantPools[p]; ok {
			wantPools[p] = true
		}
	}
	for pool, called := range wantPools {
		if !called {
			t.Errorf("expected CreateNodePool call for %s, got calls=%v", pool, gke.createNodePoolCalls)
		}
	}

	node, err := k8sClient.CoreV1().Nodes().Get(context.Background(), "tmm-node-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting labeled node: %v", err)
	}
	if node.Labels[tmmNodeLabelKey] != tmmNodeLabelValue {
		t.Errorf("expected tmm-node-1 to be labeled %s=%s, got labels=%v", tmmNodeLabelKey, tmmNodeLabelValue, node.Labels)
	}
}

func TestPhase20Cluster_SkipsWhenClusterAndPoolsRunning(t *testing.T) {
	cl := testCluster()
	st := testState(t)
	gke := &fakeGKE{
		clusters: map[string]*containerpb.Cluster{"bnk-hostdev": {Name: "bnk-hostdev", Status: containerpb.Cluster_RUNNING}},
		nodePools: map[string]*containerpb.NodePool{
			"tmm-pool":   {Name: "tmm-pool", Status: containerpb.NodePool_RUNNING},
			"agent-pool": {Name: "agent-pool", Status: containerpb.NodePool_RUNNING},
		},
	}
	k8sClient := fakek8s.NewClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "tmm-node-1", Labels: map[string]string{gkeNodepoolLabel: "tmm-pool"}}},
	)
	clients := &Clients{GKE: gke, K8s: k8sClient}

	if err := Phase20Cluster(context.Background(), cl, st, clients, false); err != nil {
		t.Fatalf("Phase20Cluster: %v", err)
	}
	if len(gke.createClusterCalls) != 0 || len(gke.createNodePoolCalls) != 0 {
		t.Errorf("expected 0 CreateCluster/CreateNodePool calls when RUNNING")
	}
}

func TestLabelTMMNodes_PollsUntilNodesAppear(t *testing.T) {
	k8sClient := fakek8s.NewClientset()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "tmm-1", Labels: map[string]string{gkeNodepoolLabel: "tmm-pool"}}}

	calls := 0
	k8sClient.PrependReactor("list", "nodes", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls == 2 {
			if err := k8sClient.Tracker().Add(node); err != nil {
				t.Fatalf("seeding node on attempt 2: %v", err)
			}
		}
		return false, nil, nil
	})

	if err := labelTMMNodes(context.Background(), k8sClient, "tmm-pool", testTMMPollInterval); err != nil {
		t.Fatalf("labelTMMNodes: %v", err)
	}
	if calls < 2 {
		t.Errorf("expected at least 2 List attempts, got %d", calls)
	}
}

func TestPhase25Discover_DryRun_SetsPlaceholderStateAndNoMutations(t *testing.T) {
	cl := testCluster()
	st := testState(t)
	instances := &fakeInstances{}
	clients := &Clients{Instances: instances}

	if err := Phase25Discover(context.Background(), cl, st, clients, true); err != nil {
		t.Fatalf("Phase25Discover(dryRun=true): %v", err)
	}
	for _, k := range DiscoverStateKeys {
		want := "dry-run-" + strings.ToLower(k)
		if got := st.Get(k); got != want {
			t.Errorf("state[%s] = %q, want %q", k, got, want)
		}
	}
}

func TestPhase25Discover_Helpers(t *testing.T) {
	k8sClient := fakek8s.NewClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "tmm-1", Labels: map[string]string{gkeNodepoolLabel: "tmm-pool"}}},
	)
	name, err := firstNodeInPool(context.Background(), k8sClient, "tmm-pool")
	if err != nil || name != "tmm-1" {
		t.Fatalf("firstNodeInPool = (%q, %v), want (tmm-1, nil)", name, err)
	}

	nic := &computepb.NetworkInterface{
		AliasIpRanges: []*computepb.AliasIpRange{
			{SubnetworkRangeName: strPtr("tmm-ext-pods"), IpCidrRange: strPtr("10.31.0.48/28")},
		},
	}
	cidr, err := aliasCIDRForRange(nic, "tmm-ext-pods")
	if err != nil || cidr != "10.31.0.48/28" {
		t.Fatalf("aliasCIDRForRange = (%q, %v), want (10.31.0.48/28, nil)", cidr, err)
	}

	pod := buildDiscoveryPod("tmm-node-1")
	if pod.Spec.NodeName != "tmm-node-1" || !pod.Spec.HostPID {
		t.Errorf("unexpected discovery pod spec: %+v", pod.Spec)
	}
}

func TestPhase29ForgeRegister(t *testing.T) {
	ctx := context.Background()

	origBackoffs := forgeRegisterBackoffs
	forgeRegisterBackoffs = []time.Duration{1 * time.Millisecond, 1 * time.Millisecond}
	defer func() { forgeRegisterBackoffs = origBackoffs }()

	t.Run("skipped when Forge disabled", func(t *testing.T) {
		cl := &intent.Cluster{
			Metadata: intent.Metadata{Name: "test-gke", Project: "my-proj", Zone: "us-central1-a"},
			Forge:    &intent.ForgeSpec{Enabled: false},
		}
		st := testState(t)
		if err := Phase29ForgeRegister(ctx, cl, st, nil, false); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("successful MCP registration", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			method, _ := req["method"].(string)
			id := req["id"]

			if method == "initialize" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result": map[string]any{
						"protocolVersion": mcp.ProtocolVersion,
						"serverInfo":      map[string]string{"name": "test-forge", "version": "1.0"},
					},
				})
				return
			}
			if method == "notifications/initialized" {
				w.WriteHeader(http.StatusOK)
				return
			}

			params, _ := req["params"].(map[string]any)
			toolName, _ := params["name"].(string)

			var resText string
			switch toolName {
			case "create_project":
				resText = `{"project_id":50,"name":"gkebnkctl-test-gke","success":true}`
			case "create_cluster":
				resText = `{"cluster_id":200,"name":"test-gke","project_id":50,"success":true}`
			case "scan_cluster":
				resText = `{"status":"scanned"}`
			case "bnk_health":
				resText = `{"status":"healthy"}`
			}

			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"content": []map[string]string{
						{"type": "text", "text": resText},
					},
				},
			})
		}))
		defer srv.Close()

		wsDir := t.TempDir()
		kubePath := filepath.Join(wsDir, "kubeconfig")
		_ = os.WriteFile(kubePath, []byte("apiVersion: v1\nclusters: []"), 0o600)

		cl := &intent.Cluster{
			Metadata: intent.Metadata{Name: "test-gke", Project: "my-proj", Zone: "us-central1-a"},
			Region:   "us-central1",
			Forge: &intent.ForgeSpec{
				Enabled: true,
				MCPURL:  srv.URL,
			},
		}

		st, err := state.Load(wsDir)
		if err != nil {
			t.Fatalf("state.Load: %v", err)
		}
		st.Set("KUBECONFIG_PATH", kubePath)

		if err := Phase29ForgeRegister(ctx, cl, st, nil, false); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if st.Get("FORGE_REGISTERED") != "true" {
			t.Errorf("expected FORGE_REGISTERED=true in state")
		}
	})
}

func TestGenerateKubeconfig(t *testing.T) {
	b, err := GenerateKubeconfig("test-cluster", "my-proj", "us-central1-a", "34.1.2.3", "LS0tLS1CRUdJTi...")
	if err != nil {
		t.Fatalf("GenerateKubeconfig: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, "https://34.1.2.3") || !strings.Contains(s, "gke_my-proj_us-central1-a_test-cluster") {
		t.Errorf("unexpected kubeconfig YAML:\n%s", s)
	}
}

func TestPhaseDownNetworks_UsesCustomVPCAndEffectiveRegion(t *testing.T) {
	cl := testCluster()
	cl.Metadata.Region = "" // Region omitted in intent; derived from Zone (us-central1-a -> us-central1)
	st := testState(t)
	clients, networks, subnetworks, firewalls, routers := testClients10()

	if err := Phase10Networks(context.Background(), cl, st, clients, false); err != nil {
		t.Fatalf("Phase10Networks: %v", err)
	}
	if err := PhaseDownNetworks(context.Background(), clients, cl, st); err != nil {
		t.Fatalf("PhaseDownNetworks: %v", err)
	}

	wantSubnets := map[string]bool{"bnkhd-subnet": false, "bnkhd-ext": false, "bnkhd-int": false}
	for _, s := range subnetworks.deleteCalls {
		if _, ok := wantSubnets[s]; ok {
			wantSubnets[s] = true
		}
	}
	for s, deleted := range wantSubnets {
		if !deleted {
			t.Errorf("expected subnet %s to be deleted; deleteCalls=%v", s, subnetworks.deleteCalls)
		}
	}

	wantVPCs := map[string]bool{"bnkhd-vpc": false, "bnkhd-ext": false, "bnkhd-int": false}
	for _, v := range networks.deleteCalls {
		if _, ok := wantVPCs[v]; ok {
			wantVPCs[v] = true
		}
	}
	for v, deleted := range wantVPCs {
		if !deleted {
			t.Errorf("expected VPC %s to be deleted; deleteCalls=%v", v, networks.deleteCalls)
		}
	}
	wantRouters := map[string]bool{"bnkhd-vpc-router": false, "bnkhd-ext-router": false, "bnkhd-int-router": false}
	for _, r := range routers.deleteCalls {
		if _, ok := wantRouters[r]; ok {
			wantRouters[r] = true
		}
	}
	for r, deleted := range wantRouters {
		if !deleted {
			t.Errorf("expected router %s to be deleted; deleteCalls=%v", r, routers.deleteCalls)
		}
	}
	if len(firewalls.deleteCalls) < 3 {
		t.Errorf("expected at least 3 firewalls deleted, got %v", firewalls.deleteCalls)
	}
}

func TestSingleInterfaceExternalOnly_NetworksAndCluster(t *testing.T) {
	cl := testCluster()
	cl.Pattern = intent.PatternExternalOnly
	cl.Network.Internal = intent.DataplaneSpec{}
	natDisabled := false
	cl.Network.NatGatewaysRaw.Enabled = &natDisabled

	st := testState(t)
	clients, networks, subnetworks, _, routers := testClients10()

	if err := Phase10Networks(context.Background(), cl, st, clients, false); err != nil {
		t.Fatalf("Phase10Networks(external-only): %v", err)
	}
	for _, v := range networks.insertCalls {
		if v == "bnkhd-int" {
			t.Errorf("unexpected internal VPC created in external-only mode: %v", networks.insertCalls)
		}
	}
	for _, s := range subnetworks.insertCalls {
		if s == "bnkhd-int" {
			t.Errorf("unexpected internal subnet created in external-only mode: %v", subnetworks.insertCalls)
		}
	}
	if len(routers.insertCalls) != 0 {
		t.Errorf("expected 0 routers when natGateways=false, got %v", routers.insertCalls)
	}

	poolCfg := buildTMMPoolConfig(cl)
	if len(poolCfg.GetNetworkConfig().GetAdditionalNodeNetworkConfigs()) != 1 {
		t.Errorf("expected 1 additional node network in external-only mode, got %d", len(poolCfg.GetNetworkConfig().GetAdditionalNodeNetworkConfigs()))
	}
	if len(poolCfg.GetNetworkConfig().GetAdditionalPodNetworkConfigs()) != 1 {
		t.Errorf("expected 1 additional pod network in external-only mode, got %d", len(poolCfg.GetNetworkConfig().GetAdditionalPodNetworkConfigs()))
	}
}

func TestDiscoverUnderlay_FetchesAndPersistsKubeconfig(t *testing.T) {
	cl := testCluster()
	st := testState(t)
	gke := &fakeGKE{
		clusters: map[string]*containerpb.Cluster{
			"bnk-hostdev": {
				Name:       "bnk-hostdev",
				Endpoint:   "34.10.20.30",
				Status:     containerpb.Cluster_RUNNING,
				MasterAuth: &containerpb.MasterAuth{ClusterCaCertificate: "LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCg=="},
			},
		},
	}
	p := &Provider{Clients: &Clients{GKE: gke}}
	u, err := p.DiscoverUnderlay(context.Background(), cl, st)
	if err != nil {
		t.Fatalf("DiscoverUnderlay: %v", err)
	}
	if st.Get("KUBECONFIG") == "" {
		t.Fatalf("expected KUBECONFIG to be set in state after DiscoverUnderlay")
	}
	if u.KubeconfigPath == "" || len(u.Kubeconfig) == 0 {
		t.Fatalf("expected UnderlayContext.KubeconfigPath and Kubeconfig to be populated, got path=%q len=%d", u.KubeconfigPath, len(u.Kubeconfig))
	}
}

type fakeDisks struct {
	disks       []*computepb.Disk
	deleteCalls []string
}

func (f *fakeDisks) ListAll(_ context.Context, _, _ string) ([]*computepb.Disk, error) {
	return f.disks, nil
}

func (f *fakeDisks) Delete(_ context.Context, req *computepb.DeleteDiskRequest, _ ...gax.CallOption) (*compute.Operation, error) {
	f.deleteCalls = append(f.deleteCalls, req.GetDisk())
	return nil, nil
}

func TestPhaseDownNetworks_DeletesOrphanedBNKPVCDisks(t *testing.T) {
	cl := testCluster()
	st := testState(t)
	clients, _, _, _, _ := testClients10()
	disks := &fakeDisks{
		disks: []*computepb.Disk{
			{
				Name:        strPtr("pvc-bnk-dssm-0"),
				Description: strPtr(`{"kubernetes.io/created-for/pvc/name":"data-f5-dssm-db-0","kubernetes.io/created-for/pvc/namespace":"f5-bnk"}`),
			},
			{
				Name:        strPtr("pvc-bnk-cwc"),
				Description: strPtr(`{"kubernetes.io/created-for/pvc/name":"cluster-wide-controller","kubernetes.io/created-for/pvc/namespace":"f5-utils"}`),
			},
			{
				Name:        strPtr("pvc-attached-disk"),
				Description: strPtr(`{"kubernetes.io/created-for/pvc/name":"data-f5-dssm-db-1","kubernetes.io/created-for/pvc/namespace":"f5-bnk"}`),
				Users:       []string{"projects/my-gcp-project/zones/us-central1-a/instances/gke-node-1"},
			},
			{
				Name:        strPtr("pvc-other-app"),
				Description: strPtr(`{"kubernetes.io/created-for/pvc/name":"postgres-data","kubernetes.io/created-for/pvc/namespace":"other-team"}`),
			},
		},
	}
	clients.Disks = disks

	if err := PhaseDownNetworks(context.Background(), clients, cl, st); err != nil {
		t.Fatalf("PhaseDownNetworks: %v", err)
	}
	if len(disks.deleteCalls) != 2 {
		t.Fatalf("expected 2 orphaned BNK PVC disks deleted, got %v", disks.deleteCalls)
	}
	if disks.deleteCalls[0] != "pvc-bnk-dssm-0" || disks.deleteCalls[1] != "pvc-bnk-cwc" {
		t.Errorf("unexpected deleted disks: %v", disks.deleteCalls)
	}
}
