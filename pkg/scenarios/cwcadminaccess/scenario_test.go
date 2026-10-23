package cwcadminaccess

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
)

func TestCWCAdminAccess_Registration(t *testing.T) {
	s := scenarios.Find("cwc-admin-access")
	if s == nil {
		t.Fatal("cwc-admin-access scenario not registered")
	}
	if s.Name() != "cwc-admin-access" {
		t.Errorf("Name = %q, want cwc-admin-access", s.Name())
	}
	if s.Rating() != scenarios.Green {
		t.Errorf("Rating = %q, want green", s.Rating())
	}
}

func TestCWCAdminAccess_Manifests(t *testing.T) {
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
	if len(paths) != 2 {
		t.Errorf("expected 2 manifest files, got %d", len(paths))
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("manifest file %s missing: %v", p, err)
		}
	}
}

func TestCWCAdminAccess_Verify(t *testing.T) {
	s := &scenario{
		vDeps: &VerifyDeps{
			WaitDeploymentAvailableFn: func(ctx context.Context, sctx *scenarios.Context, ns, name string, timeout time.Duration) error {
				return nil
			},
			CheckCWCAuthFn: func(ctx context.Context, sctx *scenarios.Context) (bool, bool, bool, string) {
				return true, true, true, "test pass"
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
	if len(res.Assertions) != 4 {
		t.Errorf("expected 4 assertions, got %d", len(res.Assertions))
	}
}
