// PSA / SCC compliance pin for the iperf3 server Pod the
// awsbnkctl test throughput verb deploys into the cluster.
//
// EKS 1.25+ enforces Pod Security Admission's `restricted` profile by
// default on every new namespace. Without the right SecurityContext
// fields, kube-apiserver rejects the Pod at admission with
//
//	Error from server (Forbidden): pods "awsbnkctl-iperf3" is forbidden:
//	violates PodSecurity "restricted:latest": ...
//
// The same rules apply on OpenShift's restricted-v2 SCC. This test pins
// the four load-bearing fields per PSA `restricted` baseline so a
// future refactor doesn't silently drop them:
//
//  1. SecurityContext.RunAsNonRoot=true (pod and container level)
//  2. SecurityContext.SeccompProfile.Type=RuntimeDefault
//  3. Container.AllowPrivilegeEscalation=false
//  4. Container.Capabilities.Drop=[ALL]
//
// Plus a Service-shape pin so the LoadBalancer path used for north-south
// throughput on EKS continues to dispatch as expected.

package k8s

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestBuildIperf3Pod_PSARestricted asserts the iperf3 server Pod is
// admissible under the EKS 1.25+ `restricted` Pod Security profile.
func TestBuildIperf3Pod_PSARestricted(t *testing.T) {
	pod := BuildIperf3Pod(Iperf3Options{})

	if pod.Namespace != Iperf3Namespace {
		t.Errorf("Namespace: got %q, want %q", pod.Namespace, Iperf3Namespace)
	}
	if pod.Spec.SecurityContext == nil {
		t.Fatal("PodSecurityContext is nil — PSA restricted requires explicit fields")
	}
	psc := pod.Spec.SecurityContext
	if psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot {
		t.Error("PodSecurityContext.RunAsNonRoot: want true")
	}
	if psc.SeccompProfile == nil {
		t.Fatal("PodSecurityContext.SeccompProfile: nil; PSA restricted rejects Unconfined")
	}
	if psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("PodSecurityContext.SeccompProfile.Type: got %q, want %q",
			psc.SeccompProfile.Type, corev1.SeccompProfileTypeRuntimeDefault)
	}

	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("Containers: got %d, want 1", len(pod.Spec.Containers))
	}
	c := pod.Spec.Containers[0]
	if c.SecurityContext == nil {
		t.Fatal("Container.SecurityContext is nil — PSA restricted requires explicit fields")
	}
	sc := c.SecurityContext
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("Container.AllowPrivilegeEscalation: want false")
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("Container.RunAsNonRoot: want true")
	}
	if sc.Capabilities == nil {
		t.Fatal("Container.Capabilities is nil — PSA restricted requires drop ALL")
	}
	if !containsCapability(sc.Capabilities.Drop, "ALL") {
		t.Errorf("Container.Capabilities.Drop: got %v, want to contain %q",
			sc.Capabilities.Drop, "ALL")
	}
}

// TestBuildIperf3Pod_DefaultsImageAndNamespace pins the zero-value
// behaviour so a caller passing an empty Iperf3Options gets the
// documented defaults (no nil-deref, no empty fields).
func TestBuildIperf3Pod_DefaultsImageAndNamespace(t *testing.T) {
	pod := BuildIperf3Pod(Iperf3Options{})
	if pod.Spec.Containers[0].Image != Iperf3DefaultImage {
		t.Errorf("default Image: got %q, want %q",
			pod.Spec.Containers[0].Image, Iperf3DefaultImage)
	}
	if pod.Namespace != Iperf3Namespace {
		t.Errorf("default Namespace: got %q, want %q",
			pod.Namespace, Iperf3Namespace)
	}
}

// TestBuildIperf3Service_LoadBalancerShape pins the LoadBalancer
// dispatch path used for north-south throughput on EKS — the AWS Load
// Balancer Controller picks up Services of this shape and provisions
// an NLB. Verifies the Port + Selector + Type wire together as
// expected.
func TestBuildIperf3Service_LoadBalancerShape(t *testing.T) {
	svc := BuildIperf3Service(Iperf3Options{ServiceType: corev1.ServiceTypeLoadBalancer})
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("Spec.Type: got %q, want %q",
			svc.Spec.Type, corev1.ServiceTypeLoadBalancer)
	}
	if got, want := svc.Spec.Selector["app"], Iperf3PodName; got != want {
		t.Errorf("Selector[app]: got %q, want %q", got, want)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != Iperf3Port {
		t.Errorf("Ports: got %+v, want [{Port:%d}]", svc.Spec.Ports, Iperf3Port)
	}
}

// containsCapability is a small helper so the assertion above reads
// well; the existing internal/exec helper of the same name lives in a
// different package.
func containsCapability(caps []corev1.Capability, want corev1.Capability) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}
