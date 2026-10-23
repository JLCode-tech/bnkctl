package corefiles

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
)

func TestCorefiles_Registration(t *testing.T) {
	s := scenarios.Find("core-file-collection")
	if s == nil {
		t.Fatal("core-file-collection scenario not registered")
	}
	if s.Name() != "core-file-collection" {
		t.Errorf("Name = %q, want core-file-collection", s.Name())
	}
	if s.Rating() != scenarios.Green {
		t.Errorf("Rating = %q, want green", s.Rating())
	}
}

func TestCorefiles_Manifests(t *testing.T) {
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
	if len(paths) != 1 {
		t.Errorf("expected 1 manifest file, got %d", len(paths))
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("manifest file %s missing: %v", p, err)
		}
	}
}

func TestCorefiles_Verify(t *testing.T) {
	s := &scenario{
		vDeps: &VerifyDeps{
			CheckCoreMondFn: func(ctx context.Context, sctx *scenarios.Context) (bool, bool, bool, string) {
				return true, true, true, "test pass"
			},
			CheckTMMVolumesFn: func(ctx context.Context, sctx *scenarios.Context) (bool, string) {
				return true, "found volume kernel-cores"
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
