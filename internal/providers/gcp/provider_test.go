package gcp

import (
	"context"
	"path/filepath"
	"testing"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"github.com/JLCode-tech/bnkctl/pkg/bnkconst"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/provider"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	gax "github.com/googleapis/gax-go/v2"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestGCPProviderUnderlayAndHooks(t *testing.T) {
	ctx := context.Background()
	p := NewProvider()
	if p.Name() != intent.ProviderGCP {
		t.Fatalf("expected name %s, got %s", intent.ProviderGCP, p.Name())
	}

	cl := &intent.Cluster{
		APIVersion: intent.APIVersion,
		Kind:       intent.Kind,
		Cloud:      &intent.CloudSpec{Provider: intent.ProviderGCP},
		Metadata:   intent.Metadata{Name: "gke-test", Region: "us-central1", Zone: "us-central1-a", Project: "p-test"},
		Pattern:    intent.PatternDualInterface,
		Network: intent.Network{
			Main:     intent.MainNetworkSpec{CIDR: "10.90.1.0/24", PodsCIDR: "10.92.0.0/14", SvcsCIDR: "10.96.0.0/20"},
			External: intent.DataplaneSpec{CIDR: "10.90.2.0/24"},
			Internal: intent.DataplaneSpec{CIDR: "10.90.3.0/24"},
		},
	}

	st, err := state.Load(filepath.Join(t.TempDir(), "state.env"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Set("GCP_GSA_EMAIL", "bnk-cne@p-test.iam.gserviceaccount.com")

	u, err := p.ProvisionUnderlay(ctx, cl, st, false)
	if err != nil {
		t.Fatalf("ProvisionUnderlay: %v", err)
	}
	if u.DataplaneDriver != provider.DriverVirtio {
		t.Fatalf("expected DriverVirtio, got %s", u.DataplaneDriver)
	}
	if ext := u.ExternalNetwork(); ext == nil || ext.TMMInterface != "1.1" {
		t.Fatalf("unexpected external network: %+v", ext)
	}
	if intl := u.InternalNetwork(); intl == nil || intl.TMMInterface != "1.2" {
		t.Fatalf("unexpected internal network: %+v", intl)
	}

	cs := k8sfake.NewClientset(
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "gke-tmm-0",
				Labels: map[string]string{"topology.kubernetes.io/zone": "us-central1-a"},
			},
			Spec: corev1.NodeSpec{ProviderID: "gce://p-test/us-central1-a/gke-tmm-0"},
		},
		&corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      bnkconst.CNEControllerServiceAccount,
				Namespace: bnkconst.InstanceNamespace,
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "f5-tmm-0",
				Namespace: bnkconst.InstanceNamespace,
				Labels:    map[string]string{"app": "f5-tmm"},
			},
		},
	)

	if err := u.DiscoverDataplaneResources(ctx, cs, nil); err != nil {
		t.Fatalf("DiscoverDataplaneResources: %v", err)
	}
	if len(u.DataplaneNodes) != 1 || len(u.DataplaneNodes[0].NICs) != 2 {
		t.Fatalf("unexpected DataplaneNodes: %+v", u.DataplaneNodes)
	}

	if err := u.ReconcileControllerIdentity(ctx, cs, nil); err != nil {
		t.Fatalf("ReconcileControllerIdentity: %v", err)
	}
	sa, err := cs.CoreV1().ServiceAccounts(bnkconst.InstanceNamespace).Get(ctx, bnkconst.CNEControllerServiceAccount, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get ServiceAccount: %v", err)
	}
	if sa.Annotations["iam.gke.io/gcp-service-account"] != "bnk-cne@p-test.iam.gserviceaccount.com" {
		t.Fatalf("unexpected SA annotations: %+v", sa.Annotations)
	}

	if err := u.PostActivationHook(ctx, cs, nil); err != nil {
		t.Fatalf("PostActivationHook: %v", err)
	}
	pod, err := cs.CoreV1().Pods(bnkconst.InstanceNamespace).Get(ctx, "f5-tmm-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get Pod: %v", err)
	}
	if pod.Labels["f5-traffic"] != "true" {
		t.Fatalf("expected f5-traffic=true label on TMM pod, got %+v", pod.Labels)
	}
}

type mockSubnetworks struct {
	SubnetworksAPI
	subs    []*computepb.Subnetwork
	deleted []string
}

func (m *mockSubnetworks) ListAll(ctx context.Context, project, region string) ([]*computepb.Subnetwork, error) {
	return m.subs, nil
}

func (m *mockSubnetworks) Delete(ctx context.Context, req *computepb.DeleteSubnetworkRequest, opts ...gax.CallOption) (*compute.Operation, error) {
	m.deleted = append(m.deleted, req.GetSubnetwork())
	return nil, nil
}

type mockNetworks struct {
	NetworksAPI
	deleted []string
}

func (m *mockNetworks) Delete(ctx context.Context, req *computepb.DeleteNetworkRequest, opts ...gax.CallOption) (*compute.Operation, error) {
	m.deleted = append(m.deleted, req.GetNetwork())
	return nil, nil
}

func TestDiscoverDataplaneResourcesAndSharedVPCTeardown(t *testing.T) {
	ctx := context.Background()
	fullURL := "https://www.googleapis.com/compute/v1/projects/p-test/global/networks/shared-ext-vpc"

	subs := &mockSubnetworks{
		subs: []*computepb.Subnetwork{
			{Name: proto.String("cluster-a-ext"), Network: proto.String(fullURL)},
			{Name: proto.String("cluster-b-ext"), Network: proto.String(fullURL)},
		},
	}
	nets := &mockNetworks{}
	c := &Clients{
		Subnetworks: subs,
		Networks:    nets,
	}

	disc, err := DiscoverDataplaneResources(ctx, c, "p-test", "us-central1", "shared-ext-vpc")
	if err != nil {
		t.Fatalf("DiscoverDataplaneResources: %v", err)
	}
	if len(disc.Subnetworks) != 2 {
		t.Fatalf("expected 2 subnetworks matched via full Compute API URL, got %d", len(disc.Subnetworks))
	}

	// Teardown cluster-a-ext when cluster-b-ext still exists: shared VPC must NOT be deleted.
	if err := teardownDataplaneResources(ctx, c, "p-test", "us-central1", "shared-ext-vpc", "cluster-a-ext", true); err != nil {
		t.Fatalf("teardownDataplaneResources (multi-tenant): %v", err)
	}
	if len(nets.deleted) != 0 {
		t.Fatalf("expected shared VPC to be preserved while another tenant subnet remains, got deleted=%v", nets.deleted)
	}

	// Now only cluster-b-ext remains: tearing down cluster-b-ext must also delete the shared VPC.
	subs.subs = []*computepb.Subnetwork{
		{Name: proto.String("cluster-b-ext"), Network: proto.String(fullURL)},
	}
	if err := teardownDataplaneResources(ctx, c, "p-test", "us-central1", "shared-ext-vpc", "cluster-b-ext", true); err != nil {
		t.Fatalf("teardownDataplaneResources (last-tenant): %v", err)
	}
	if len(nets.deleted) != 1 || nets.deleted[0] != "shared-ext-vpc" {
		t.Fatalf("expected shared VPC to be deleted when last tenant subnet is removed, got deleted=%v", nets.deleted)
	}
}

func TestGCPDiscoverHelpers(t *testing.T) {
	if len(ifaceDumpScript) == 0 {
		t.Fatal("expected non-empty ifaceDumpScript")
	}
	inst := &computepb.Instance{
		NetworkInterfaces: []*computepb.NetworkInterface{
			{
				Network:   proto.String("https://www.googleapis.com/compute/v1/projects/p-test/global/networks/shared-ext-vpc"),
				NetworkIP: proto.String("10.90.2.5"),
			},
		},
	}
	nic, err := nicForVPC(inst, "shared-ext-vpc")
	if err != nil || nic == nil || nic.GetNetworkIP() != "10.90.2.5" {
		t.Fatalf("unexpected nicForVPC result: %+v, err=%v", nic, err)
	}
	dump := "eth1 aa:bb:cc:dd:ee:ff 0000:00:05.0\n"
	if bdf := fieldForMAC(dump, "AA:BB:CC:DD:EE:FF", 2); bdf != "0000:00:05.0" {
		t.Fatalf("expected BDF 0000:00:05.0, got %q", bdf)
	}
	if ip := stripCIDR("10.90.2.5/32"); ip != "10.90.2.5" {
		t.Fatalf("unexpected stripCIDR: %q", ip)
	}
	gw, err := lastOctetPlusOne("10.90.2.0/24")
	if err != nil || gw != "10.90.2.1" {
		t.Fatalf("unexpected lastOctetPlusOne: %q, err=%v", gw, err)
	}
}
