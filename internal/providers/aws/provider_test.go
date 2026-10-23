package aws

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/bnkconst"
	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/provider"
	"github.com/JLCode-tech/bnkctl/pkg/state"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestAWSProviderUnderlayAndHooks(t *testing.T) {
	ctx := context.Background()
	p := NewProvider()
	if p.Name() != intent.ProviderAWS {
		t.Fatalf("expected name %s, got %s", intent.ProviderAWS, p.Name())
	}

	cl := &intent.Cluster{
		APIVersion: intent.APIVersion,
		Kind:       intent.Kind,
		Cloud:      &intent.CloudSpec{Provider: intent.ProviderAWS},
		Metadata:   intent.Metadata{Name: "eks-test", Region: "ap-southeast-2"},
		Pattern:    intent.PatternDualInterface,
		Network: intent.Network{
			VPCCidr: "10.80.0.0/16",
			DataPath: &intent.DataPathSpec{
				External: intent.SubnetSpec{CIDR: "10.80.100.0/24"},
				Internal: intent.SubnetSpec{CIDR: "10.80.101.0/24"},
			},
		},
	}

	st, err := state.Load(filepath.Join(t.TempDir(), "state.env"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Set("CNE_IRSA_ROLE_ARN", "arn:aws:iam::123456789012:role/bnk-cne-irsa")
	st.Set("VPC_ID", "vpc-0123456789")

	u, err := p.ProvisionUnderlay(ctx, cl, st, false)
	if err != nil {
		t.Fatalf("ProvisionUnderlay: %v", err)
	}
	if u.DataplaneDriver != provider.DriverDPDKPCI {
		t.Fatalf("expected DriverDPDKPCI, got %s", u.DataplaneDriver)
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
				Name:   "ip-10-80-1-10.ap-southeast-2.compute.internal",
				Labels: map[string]string{"topology.kubernetes.io/zone": "ap-southeast-2a"},
			},
			Spec: corev1.NodeSpec{ProviderID: "aws:///ap-southeast-2a/i-0123456789abcdef0"},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "f5-cne-controller",
				Namespace: bnkconst.InstanceNamespace,
			},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						ServiceAccountName: bnkconst.CNEControllerServiceAccount,
					},
				},
			},
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
	if sa.Annotations["eks.amazonaws.com/role-arn"] != "arn:aws:iam::123456789012:role/bnk-cne-irsa" {
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

	if err := p.DestroyUnderlay(ctx, cl, st, false); err != nil {
		t.Fatalf("DestroyUnderlay: %v", err)
	}
	if st.Get("VPC_ID") != "" || st.Get("UNDERLAY_DESTROYED") != "true" {
		t.Fatalf("expected VPC_ID cleared and UNDERLAY_DESTROYED=true after DestroyUnderlay")
	}
}

func TestMatchInterfaces(t *testing.T) {
	dump := `ens5 06:aa:bb:cc:dd:01 0000:00:05.0
ens6 06:aa:bb:cc:dd:02 0000:00:06.0
ens7 06:aa:bb:cc:dd:03 0000:00:07.0`

	disc, err := matchInterfaces(dump, "06:aa:bb:cc:dd:01", "06:aa:bb:cc:dd:02", "06:aa:bb:cc:dd:03", true)
	if err != nil {
		t.Fatalf("matchInterfaces: %v", err)
	}
	if disc.PrimaryIfName != "ens5" || disc.ExtIfName != "ens6" || disc.ExtPCIBDF != "0000:00:06.0" || disc.IntIfName != "ens7" || disc.IntPCIBDF != "0000:00:07.0" {
		t.Fatalf("unexpected DiscoveredInterfaces: %+v", disc)
	}
}
