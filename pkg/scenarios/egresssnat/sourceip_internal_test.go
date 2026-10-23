package egresssnat

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

func selfIPCtx(t *testing.T) *scenarios.Context {
	t.Helper()
	st, err := state.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st.Set("TMM_EXT_SELFIP", "10.50.10.240")
	return &scenarios.Context{Ctx: context.Background(), Out: io.Discard, State: st, Options: map[string]string{}}
}

func fastSourceIPPoll(t *testing.T) {
	t.Helper()
	oldT, oldI := sourceIPPollTimeout, sourceIPPollInterval
	sourceIPPollTimeout, sourceIPPollInterval = 30*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { sourceIPPollTimeout, sourceIPPollInterval = oldT, oldI })
}

// tmctlStub answers the two tmctl reads with the given counters and records
// the curls it is asked to run.
type tmctlStub struct {
	virtual, snat int
	curls         int
	curlErr       error
	bump          bool   // advance counters on every curl, as a real TMM would
	vsName        string // the egress virtual server name TMM lists (default: 2.3 style)
}

func (s *tmctlStub) exec(_ *scenarios.Context, ns, pod, container string, cmd ...string) (string, error) {
	line := strings.Join(cmd, " ")
	vs := s.vsName
	if vs == "" {
		vs = "awsbnkctl-scn-egress-awsbnkctl-egress-egress-ipv4"
	}
	switch {
	case strings.Contains(line, "virtual_server_stat"):
		return "name  clientside.tot_conns\n----  ----\n" +
			vs + "  " + itoa(s.virtual) + "\nother-vs  99\n", nil
	case strings.Contains(line, "pool_member_stat"):
		return "pool_name addr serverside.tot_conns\n" +
			"snat_automap[0] 00:00:00:00:00:00:00:00:00:00:FF:FF:0A:32:01:66:00:00:00:00 340\n" +
			"snat_automap[0] 00:00:00:00:00:00:00:00:00:00:FF:FF:0A:32:0A:F0:00:00:00:00 " + itoa(s.snat) + "\n", nil
	case strings.HasPrefix(line, "curl"):
		if ns != "awsbnkctl-scn-egress" || pod != "egress-client" || container != "curl" {
			return "", errors.New("curl exec aimed at the wrong pod: " + ns + "/" + pod + "/" + container)
		}
		s.curls++
		if s.bump {
			s.virtual++
			s.snat++
		}
		return "", s.curlErr
	}
	return "", errors.New("unexpected exec: " + line)
}

func itoa(n int) string { return strconv.Itoa(n) }

func tmmDeps(stub *tmctlStub) *VerifyDeps {
	return &VerifyDeps{TMMPodFn: func(*scenarios.Context) (string, error) { return "f5-tmm-x", nil }, ExecInPodFn: stub.exec}
}

// TestVerifyTMMEgress_ProvesPath: each curl moves both TMM counters → OK.
func TestVerifyTMMEgress_ProvesPath(t *testing.T) {
	fastSourceIPPoll(t)
	stub := &tmctlStub{virtual: 1, snat: 2, bump: true}
	skipped, a := verifyTMMEgress(tmmDeps(stub), selfIPCtx(t), "awsbnkctl-scn-egress")
	if skipped || !a.OK {
		t.Fatalf("skipped=%v ok=%v got=%q", skipped, a.OK, a.Got)
	}
	if stub.curls != egressCurlCount {
		t.Errorf("curls = %d, want %d", stub.curls, egressCurlCount)
	}
	if !strings.Contains(a.Got, "awsbnkctl-scn-egress-awsbnkctl-egress-egress-ipv4 +3 conns; SNAT to 10.50.10.240 +3 conns") {
		t.Errorf("detail should carry the virtual server and both deltas: %q", a.Got)
	}
}

// TestVerifyTMMEgress_FindsRenamedVirtual: the 2.4 EgressGateway virtual
// server name is not pinned yet, so any IPv4 virtual carrying the
// EgressGateway name counts, and the detail names the one that matched.
func TestVerifyTMMEgress_FindsRenamedVirtual(t *testing.T) {
	fastSourceIPPoll(t)
	stub := &tmctlStub{virtual: 4, snat: 4, bump: true, vsName: "egw-awsbnkctl-scn-egress-awsbnkctl-egress-0.0.0.0-0-ipv4"}
	skipped, a := verifyTMMEgress(tmmDeps(stub), selfIPCtx(t), "awsbnkctl-scn-egress")
	if skipped || !a.OK {
		t.Fatalf("skipped=%v ok=%v got=%q", skipped, a.OK, a.Got)
	}
	if !strings.Contains(a.Got, "egw-awsbnkctl-scn-egress-awsbnkctl-egress-0.0.0.0-0-ipv4 +3 conns") {
		t.Errorf("detail should name the matched virtual server: %q", a.Got)
	}
}

// TestVerifyTMMEgress_NoVirtualListsWhatTMMHas: when TMM has no virtual for
// the EgressGateway the assertion fails and lists the virtual servers and
// SNAT members TMM does have, so the name can be pinned from the report.
func TestVerifyTMMEgress_NoVirtualListsWhatTMMHas(t *testing.T) {
	fastSourceIPPoll(t)
	stub := &tmctlStub{virtual: 4, snat: 4, bump: true, vsName: "unrelated-gateway-http"}
	skipped, a := verifyTMMEgress(tmmDeps(stub), selfIPCtx(t), "awsbnkctl-scn-egress")
	if skipped || a.OK {
		t.Fatalf("expected a failing, non-skipped assertion; got skipped=%v ok=%v (%s)", skipped, a.OK, a.Got)
	}
	for _, want := range []string{"<none matching awsbnkctl-egress>", "virtual servers on TMM: other-vs, unrelated-gateway-http", "snat_automap members: snat_automap[0]"} {
		if !strings.Contains(a.Got, want) {
			t.Errorf("detail should contain %q: %q", want, a.Got)
		}
	}
}

// TestVerifyTMMEgress_BypassedTMMFails: curls succeed but TMM never sees them
// (traffic left via the node) → fail with the zero deltas in the detail.
func TestVerifyTMMEgress_BypassedTMMFails(t *testing.T) {
	fastSourceIPPoll(t)
	stub := &tmctlStub{virtual: 5, snat: 5}
	skipped, a := verifyTMMEgress(tmmDeps(stub), selfIPCtx(t), "awsbnkctl-scn-egress")
	if skipped || a.OK {
		t.Fatalf("expected a failing, non-skipped assertion; got skipped=%v ok=%v (%s)", skipped, a.OK, a.Got)
	}
	if !strings.Contains(a.Got, "+0 conns") {
		t.Errorf("detail should show the counters did not move: %q", a.Got)
	}
}

// TestVerifyTMMEgress_NoTMMPodIsAFailure: no TMM, no proof.
func TestVerifyTMMEgress_NoTMMPodIsAFailure(t *testing.T) {
	d := &VerifyDeps{
		TMMPodFn:    func(*scenarios.Context) (string, error) { return "", errors.New("no Running pod with app=f5-tmm") },
		ExecInPodFn: (&tmctlStub{}).exec,
	}
	skipped, a := verifyTMMEgress(d, selfIPCtx(t), "ns")
	if skipped || a.OK || !strings.Contains(a.Got, "app=f5-tmm") {
		t.Fatalf("skipped=%v ok=%v got=%q", skipped, a.OK, a.Got)
	}
}

// TestVerifyTMMEgress_SkipsWithoutExec: no exec client → informational, not red.
func TestVerifyTMMEgress_SkipsWithoutExec(t *testing.T) {
	d := &VerifyDeps{TMMPodFn: func(*scenarios.Context) (string, error) { t.Fatal("must not look for TMM"); return "", nil }}
	skipped, a := verifyTMMEgress(d, selfIPCtx(t), "ns")
	if !skipped || !a.OK || !strings.Contains(a.Got, "skipped") {
		t.Fatalf("skipped=%v ok=%v got=%q", skipped, a.OK, a.Got)
	}
}

func TestTmctlParsing(t *testing.T) {
	if got := ipv4Hex("10.50.10.240"); got != "FF:FF:0A:32:0A:F0:00:00:00:00" {
		t.Errorf("ipv4Hex = %q", got)
	}
	if got := ipv4Hex("nope"); got != "" {
		t.Errorf("ipv4Hex(invalid) = %q, want empty", got)
	}
	out := "name x\nfoo-vs   12\nbar-vs 7\n"
	if got := lastIntOnLine(out, "bar-vs"); got != 7 {
		t.Errorf("lastIntOnLine = %d, want 7", got)
	}
	if got := lastIntOnLine(out, "missing"); got != 0 {
		t.Errorf("lastIntOnLine(missing) = %d, want 0", got)
	}
	if got := egressVirtualName("awsbnkctl-scn-egress", "awsbnkctl-egress"); got != "awsbnkctl-scn-egress-awsbnkctl-egress-egress-ipv4" {
		t.Errorf("egressVirtualName = %q", got)
	}
	table := "name  clientside.tot_conns\n----  ----\n" +
		"awsbnkctl-scn-egress-awsbnkctl-egress-egress-ipv6  1\n" +
		"awsbnkctl-scn-egress-awsbnkctl-egress-egress-ipv4  2\n" +
		"other  3\n"
	if got := egressVirtualLine(table, "awsbnkctl-scn-egress", "awsbnkctl-egress"); !strings.HasPrefix(got, "awsbnkctl-scn-egress-awsbnkctl-egress-egress-ipv4") {
		t.Errorf("egressVirtualLine must prefer the exact IPv4 name, got %q", got)
	}
	if got := egressVirtualLine(table, "elsewhere", "nothing"); got != "" {
		t.Errorf("egressVirtualLine(no match) = %q, want empty", got)
	}
	if got := strings.Join(tableFirstColumn(table), ","); got != "awsbnkctl-scn-egress-awsbnkctl-egress-egress-ipv4,awsbnkctl-scn-egress-awsbnkctl-egress-egress-ipv6,other" {
		t.Errorf("tableFirstColumn = %q", got)
	}
}
