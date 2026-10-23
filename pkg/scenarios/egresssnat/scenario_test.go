package egresssnat_test

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
	"github.com/JLCode-tech/bnkctl/pkg/scenarios/egresssnat"
	"github.com/JLCode-tech/bnkctl/pkg/state"

	// Side-effect import: registers egress-snat in init().
	_ "github.com/JLCode-tech/bnkctl/pkg/scenarios/egresssnat"
)

func readFileHelper(path string) ([]byte, error) {
	return os.ReadFile(path) // #nosec G304 — test helper, path from WriteManifest
}

// minimalCtx builds a scenarios.Context sufficient for render-only tests
// (no Cluster, no Clientset/Dynamic needed).
func minimalCtx(dir string) *scenarios.Context {
	return &scenarios.Context{
		Ctx:          context.Background(),
		Out:          io.Discard,
		WorkspaceDir: dir,
		Options:      map[string]string{},
	}
}

// renderAll renders the scenario's manifests and returns them keyed by file name.
func renderAll(t *testing.T, sctx *scenarios.Context) map[string]string {
	t.Helper()
	s := scenarios.Find("egress-snat")
	if s == nil {
		t.Fatal("egress-snat not registered")
	}
	paths, err := s.Manifests(sctx)
	if err != nil {
		t.Fatalf("Manifests: %v", err)
	}
	out := map[string]string{}
	for _, p := range paths {
		raw, readErr := readFileHelper(p)
		if readErr != nil {
			t.Fatalf("reading %s: %v", p, readErr)
		}
		if strings.Contains(string(raw), "{{") || strings.Contains(string(raw), "}}") {
			t.Errorf("manifest %s still contains template directives:\n%s", p, raw)
		}
		out[p[strings.LastIndex(p, "/")+1:]] = string(raw)
	}
	return out
}

func TestRegistered(t *testing.T) {
	s := scenarios.Find("egress-snat")
	if s == nil {
		t.Fatal("egress-snat not registered — init() not called?")
	}
	if s.Rating() != scenarios.Green {
		t.Errorf("rating = %q, want green", s.Rating())
	}
	if len(s.Dependencies()) != 0 {
		t.Errorf("dependencies = %v, want empty", s.Dependencies())
	}
}

// TestManifestsRendered pins the 2.4 egress shape: a GatewaySettings with one
// Automap egressConfigs entry on the tunnel network and an EgressGateway that
// references it by sectionName and selects the scenario namespace. Without a
// cluster the tunnel network is the external VLAN, which every pattern has.
func TestManifestsRendered(t *testing.T) {
	m := renderAll(t, minimalCtx(t.TempDir()))
	if len(m) != 4 {
		t.Errorf("expected 4 manifests, got %d: %v", len(m), m)
	}
	for _, ch := range []struct{ file, want, desc string }{
		{"01-namespace.yaml", "name: awsbnkctl-scn-egress", "scenario namespace"},
		{"03-gatewaysettings.yaml", "kind: GatewaySettings", "GatewaySettings kind"},
		{"03-gatewaysettings.yaml", "namespace: awsbnkctl-scn-egress", "GatewaySettings in the scenario namespace"},
		{"03-gatewaysettings.yaml", "- name: default-egress", "egressConfigs entry name"},
		{"03-gatewaysettings.yaml", "type: Automap", "Automap SNAT"},
		{"03-gatewaysettings.yaml", "name: ext-vlan-infra", "tunnel network defaults to the external Infra VLAN"},
		{"04-egressgateway.yaml", "kind: EgressGateway", "EgressGateway kind"},
		{"04-egressgateway.yaml", "gatewayClassName: f5-cne", "GatewayClass fallback"},
		{"04-egressgateway.yaml", "name: awsbnkctl-egress\n      sectionName: default-egress", "parametersRef to the GatewaySettings entry"},
		{"04-egressgateway.yaml", "selectionMode: NamespaceSelector", "namespace selection"},
		{"04-egressgateway.yaml", "- awsbnkctl-scn-egress", "the scenario namespace is selected"},
	} {
		if !strings.Contains(m[ch.file], ch.want) {
			t.Errorf("%s missing %s (%q):\n%s", ch.file, ch.desc, ch.want, m[ch.file])
		}
	}
	for _, old := range []string{"F5SPKEgress", "F5SPKStaticRoute", "pseudoCNIConfig"} {
		for f, c := range m {
			if strings.Contains(c, old) {
				t.Errorf("%s still carries the 2.3 %s", f, old)
			}
		}
	}
}

// recordingWait stubs WaitCRConditionsFn and records the resources it was asked about.
type recordingWait struct {
	calls []string
	fail  map[string]error // resource → error to return
}

func (r *recordingWait) fn(_ context.Context, _ *scenarios.Context, gvr schema.GroupVersionResource, ns, name string, want []string, _ time.Duration) (string, error) {
	r.calls = append(r.calls, gvr.Resource+":"+ns+"/"+name+":"+strings.Join(want, "+"))
	if err := r.fail[gvr.Resource]; err != nil {
		return "Accepted=True Programmed=False/Programming: waiting", err
	}
	return "Accepted=True ResolvedRefs=True Programmed=True", nil
}

// TestVerifyCallOrder exercises Verify with injected stub deps: pod first,
// then the GatewaySettings, then the EgressGateway with Programmed; without
// an exec client the data-path step is recorded as skipped.
func TestVerifyCallOrder(t *testing.T) {
	var calls []string
	stubWaitPodReady := func(_ context.Context, _ *scenarios.Context, ns, name string, _ time.Duration) error {
		calls = append(calls, "WaitPodReady:"+ns+"/"+name)
		return nil
	}
	rw := &recordingWait{}
	s := egresssnat.NewScenarioForTest(egresssnat.VerifyDeps{WaitPodReadyFn: stubWaitPodReady, WaitCRConditionsFn: rw.fn})

	res := s.Verify(&scenarios.Context{Ctx: context.Background(), Out: io.Discard, Options: map[string]string{}})

	if !res.AllPassed() {
		t.Errorf("Verify failed: %s; assertions: %+v", res.Summary, res.Assertions)
	}
	if len(res.Assertions) != 4 {
		t.Fatalf("expected 4 assertions, got %d", len(res.Assertions))
	}
	if len(calls) != 1 || calls[0] != "WaitPodReady:awsbnkctl-scn-egress/egress-client" {
		t.Errorf("pod wait calls = %v", calls)
	}
	wantCalls := []string{
		"gatewaysettings:awsbnkctl-scn-egress/awsbnkctl-egress:Accepted+ResolvedRefs",
		"egressgateways:awsbnkctl-scn-egress/awsbnkctl-egress:Accepted+ResolvedRefs+Programmed",
	}
	if strings.Join(rw.calls, ";") != strings.Join(wantCalls, ";") {
		t.Errorf("CR waits = %v, want %v", rw.calls, wantCalls)
	}
	last := res.Assertions[3]
	if !last.OK || !strings.Contains(last.Got, "skipped") {
		t.Errorf("without an exec client the data-path assertion should be OK and skipped, got %+v", last)
	}
	if res.DataPath {
		t.Error("DataPath must be false when the proof was skipped")
	}
}

// TestVerifyPodFailure confirms that a failing WaitPodReady propagates as
// OK=false and the result is not AllPassed.
func TestVerifyPodFailure(t *testing.T) {
	stubFail := func(_ context.Context, _ *scenarios.Context, _, _ string, _ time.Duration) error {
		return errors.New("pod not Ready after 3m0s")
	}
	s := egresssnat.NewScenarioForTest(egresssnat.VerifyDeps{WaitPodReadyFn: stubFail, WaitCRConditionsFn: (&recordingWait{}).fn})

	res := s.Verify(&scenarios.Context{Ctx: context.Background(), Out: io.Discard, Options: map[string]string{}})
	if res.AllPassed() {
		t.Error("expected AllPassed=false when pod not Ready")
	}
	if res.Status != "failed" {
		t.Errorf("expected status=failed, got %q", res.Status)
	}
	if res.Assertions[0].OK {
		t.Error("first assertion (pod Ready) should be OK=false")
	}
}

// TestVerifyEgressGatewayNotProgrammed confirms that an EgressGateway that
// never reaches Programmed=True fails its assertion and carries the
// conditions it last reported.
func TestVerifyEgressGatewayNotProgrammed(t *testing.T) {
	stubPodOK := func(_ context.Context, _ *scenarios.Context, _, _ string, _ time.Duration) error { return nil }
	rw := &recordingWait{fail: map[string]error{"egressgateways": errors.New("Programmed not True after 3m0s")}}
	s := egresssnat.NewScenarioForTest(egresssnat.VerifyDeps{WaitPodReadyFn: stubPodOK, WaitCRConditionsFn: rw.fn})

	res := s.Verify(&scenarios.Context{Ctx: context.Background(), Out: io.Discard, Options: map[string]string{}})
	if res.AllPassed() {
		t.Error("expected AllPassed=false when the EgressGateway is not Programmed")
	}
	if res.Assertions[1].OK != true {
		t.Error("GatewaySettings assertion should pass")
	}
	if res.Assertions[2].OK || !strings.Contains(res.Assertions[2].Got, "Programming") || !strings.Contains(res.Assertions[2].Got, "after 3m0s") {
		t.Errorf("EgressGateway assertion should fail with the conditions and the wait error, got %+v", res.Assertions[2])
	}
}

// TestOptionsOverrides confirms namespace and tunnel-network overrides are
// reflected in the rendered manifests.
func TestOptionsOverrides(t *testing.T) {
	sctx := minimalCtx(t.TempDir())
	sctx.Options = map[string]string{"namespace": "custom-ns", "tunnel-network": "custom-vlan"}
	m := renderAll(t, sctx)
	if !strings.Contains(m["03-gatewaysettings.yaml"], "name: custom-vlan") {
		t.Errorf("03-gatewaysettings.yaml should use custom-vlan, got:\n%s", m["03-gatewaysettings.yaml"])
	}
	for _, f := range []string{"01-namespace.yaml", "02-curlpod.yaml", "03-gatewaysettings.yaml", "04-egressgateway.yaml"} {
		if !strings.Contains(m[f], "custom-ns") {
			t.Errorf("%s should use custom-ns, got:\n%s", f, m[f])
		}
	}
	if strings.Contains(m["04-egressgateway.yaml"], "awsbnkctl-scn-egress") {
		t.Error("04-egressgateway.yaml must select the overridden namespace only")
	}
}

// TestManifestsRendered_WithClusterAndState is the live shape: state.env
// carries the GatewayClass phase 23b registered and cluster.yaml says
// dual-interface, so the EgressGateway binds to that class and the tunnel
// network is the internal Infra VLAN (matching the Infra egressDefaults).
func TestManifestsRendered_WithClusterAndState(t *testing.T) {
	dir := t.TempDir()
	sctx := minimalCtx(dir)
	st, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.Set("GATEWAYCLASS_NAME", "bnk-live-gatewayclass")
	sctx.State = st
	sctx.Cluster = &intent.Cluster{
		Metadata: intent.Metadata{Name: "bnk-live"},
		Pattern:  intent.PatternDualInterface,
		Network: intent.Network{
			VPCCidr: "10.50.0.0/16",
			DataPath: &intent.DataPathSpec{
				External: intent.SubnetSpec{CIDR: "10.50.10.0/24"},
				Internal: intent.SubnetSpec{CIDR: "10.50.20.0/24"},
			},
		},
	}
	m := renderAll(t, sctx)
	if !strings.Contains(m["04-egressgateway.yaml"], "gatewayClassName: bnk-live-gatewayclass") {
		t.Errorf("EgressGateway should bind to the GatewayClass in state:\n%s", m["04-egressgateway.yaml"])
	}
	if !strings.Contains(m["03-gatewaysettings.yaml"], "name: int-vlan-infra") {
		t.Errorf("dual-interface tunnel network should be int-vlan-infra:\n%s", m["03-gatewaysettings.yaml"])
	}

	// Without the state key the class follows the cluster-name convention.
	sctx.State = nil
	m = renderAll(t, sctx)
	if !strings.Contains(m["04-egressgateway.yaml"], "gatewayClassName: bnk-live-gatewayclass") {
		t.Errorf("EgressGateway should derive the GatewayClass from the cluster name:\n%s", m["04-egressgateway.yaml"])
	}
}
