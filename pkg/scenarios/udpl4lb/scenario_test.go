package udpl4lb

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
)

func TestUDPL4LB_Registration(t *testing.T) {
	s := scenarios.Find("udp-l4-loadbalance")
	if s == nil {
		t.Fatal("udp-l4-loadbalance scenario not registered")
	}
	if s.Name() != "udp-l4-loadbalance" {
		t.Errorf("Name = %q, want udp-l4-loadbalance", s.Name())
	}
	if s.Rating() != scenarios.Amber {
		t.Errorf("Rating = %q, want amber (control plane only — no data-path probe)", s.Rating())
	}
}

func TestUDPL4LB_Manifests(t *testing.T) {
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

func TestUDPL4LB_Verify(t *testing.T) {
	s := &scenario{
		vDeps: &VerifyDeps{
			WaitDeploymentAvailableFn: func(ctx context.Context, sctx *scenarios.Context, ns, name string, timeout time.Duration) error {
				return nil
			},
			WaitConditionFn: func(ctx context.Context, sctx *scenarios.Context, gvr schema.GroupVersionResource, ns, name, condType string, timeout time.Duration) error {
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
