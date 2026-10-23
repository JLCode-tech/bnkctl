package scenarios

import (
	"context"
	"testing"

	k8sapply "github.com/JLCode-tech/bnkctl/pkg/k8s"
)

func TestApplyManifests_ForceAndKubeconfig(t *testing.T) {
	var captured *k8sapply.ApplyOptions

	// Swap in a stub runner that records the options and returns nil.
	orig := applyRunner
	defer func() { applyRunner = orig }()
	applyRunner = func(_ context.Context, ao *k8sapply.ApplyOptions) error {
		captured = ao
		return nil
	}

	sctx := &Context{
		Ctx:            context.Background(),
		WorkspaceDir:   t.TempDir(),
		KubeconfigPath: "/tmp/kubeconfig",
	}

	if err := ApplyManifests(sctx, "test-scenario"); err != nil {
		t.Fatalf("ApplyManifests returned unexpected error: %v", err)
	}

	if captured == nil {
		t.Fatal("applyRunner was not called")
	}
	if !captured.Force {
		t.Errorf("expected Force=true, got false")
	}
	if captured.KubeconfigPath != "/tmp/kubeconfig" {
		t.Errorf("expected KubeconfigPath=%q, got %q", "/tmp/kubeconfig", captured.KubeconfigPath)
	}
	if captured.Filename == "" {
		t.Errorf("expected non-empty Filename, got empty string")
	}
}
