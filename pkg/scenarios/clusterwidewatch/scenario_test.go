package clusterwidewatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

func TestClusterWideWatch_Registration(t *testing.T) {
	s := scenarios.Find("cluster-wide-watch")
	if s == nil {
		t.Fatal("cluster-wide-watch scenario not registered")
	}
	if s.Name() != "cluster-wide-watch" {
		t.Errorf("Name = %q, want cluster-wide-watch", s.Name())
	}
	if s.Rating() != scenarios.Green {
		t.Errorf("Rating = %q, want green", s.Rating())
	}
}

func TestClusterWideWatch_Manifests(t *testing.T) {
	tmpDir := t.TempDir()
	sctx := &scenarios.Context{
		WorkspaceDir: tmpDir,
		Options:      map[string]string{},
	}
	s := &scenario{}
	paths, err := s.Manifests(sctx)
	if err != nil {
		t.Fatalf("Manifests error: %v", err)
	}
	if len(paths) != 5 {
		t.Errorf("expected 5 manifest files, got %d", len(paths))
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("manifest file %s missing: %v", p, err)
		}
	}
}

func TestClusterWideWatch_Verify(t *testing.T) {
	s := &scenario{
		vDeps: &VerifyDeps{
			WaitDeploymentAvailableFn: func(ctx context.Context, sctx *scenarios.Context, ns, name string, timeout time.Duration) error {
				return nil
			},
			WaitConditionFn: func(ctx context.Context, sctx *scenarios.Context, gvr schema.GroupVersionResource, ns, name, condType string, timeout time.Duration) error {
				return nil
			},
			WaitHTTPRouteConditionFn: func(ctx context.Context, sctx *scenarios.Context, ns, name, condType string, timeout time.Duration) error {
				return nil
			},
		},
	}
	res := s.Verify(&scenarios.Context{
		Ctx:          context.Background(),
		WorkspaceDir: filepath.Join(os.TempDir(), "dummy"),
		Options:      map[string]string{},
	})
	if res.Status != "ok" {
		t.Fatalf("expected ok status, got %s (summary: %s)", res.Status, res.Summary)
	}
	if len(res.Assertions) != 3 {
		t.Errorf("expected 3 assertions, got %d", len(res.Assertions))
	}
}

func TestClusterWideWatch_Verify_ProbeVIP(t *testing.T) {
	var probedVIP string
	s := &scenario{
		vDeps: &VerifyDeps{
			WaitDeploymentAvailableFn: func(ctx context.Context, sctx *scenarios.Context, ns, name string, timeout time.Duration) error {
				return nil
			},
			WaitConditionFn: func(ctx context.Context, sctx *scenarios.Context, gvr schema.GroupVersionResource, ns, name, condType string, timeout time.Duration) error {
				return nil
			},
			WaitHTTPRouteConditionFn: func(ctx context.Context, sctx *scenarios.Context, ns, name, condType string, timeout time.Duration) error {
				return nil
			},
			RunCurlProbesFn: func(ctx context.Context, sctx *scenarios.Context, vip string, iterations int, timeout time.Duration) (bool, string) {
				probedVIP = vip
				return true, "5/5 HTTP 200"
			},
		},
	}
	dir := t.TempDir()
	st, _ := state.Load(dir)
	st.Set("JUMPHOST_INSTANCE_ID", "i-test")
	st.Set("JUMPHOST_BNK_EXT_ENI_IP", "10.50.10.200")
	sctx := &scenarios.Context{
		Ctx:          context.Background(),
		State:        st,
		WorkspaceDir: dir,
		Options: map[string]string{
			"vip": "10.50.10.100",
		},
	}
	res := s.Verify(sctx)
	if !res.DataPath {
		t.Errorf("expected res.DataPath to be true")
	}
	if probedVIP != "10.50.10.105" {
		t.Errorf("probed VIP = %q, want 10.50.10.105", probedVIP)
	}
}
