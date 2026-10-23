package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKubeconfigPath_FallbackWhenRelativePathMissing(t *testing.T) {
	wsDir := t.TempDir()
	st, err := Load(wsDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	wsKubeconfig := filepath.Join(wsDir, "kubeconfig")
	st.Set("KUBECONFIG_PATH", ".awsbnkctl/nonexistent-relative/kubeconfig")

	// Even before wsKubeconfig is created on disk, a missing relative KUBECONFIG_PATH
	// should fall back to filepath.Join(st.Dir(), "kubeconfig").
	if got := st.KubeconfigPath(); got != wsKubeconfig {
		t.Fatalf("KubeconfigPath() = %q, want %q", got, wsKubeconfig)
	}

	// When an existing custom path is present on disk, KubeconfigPath() returns it.
	existingCustom := filepath.Join(t.TempDir(), "custom-kubeconfig")
	if err := os.WriteFile(existingCustom, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatalf("write custom kubeconfig: %v", err)
	}
	st.Set("KUBECONFIG_PATH", existingCustom)
	if got := st.KubeconfigPath(); got != existingCustom {
		t.Fatalf("KubeconfigPath() with existing custom = %q, want %q", got, existingCustom)
	}

	// When KUBECONFIG_PATH is a stale relative path that does not exist on disk,
	// but KUBECONFIG points to an existing file on disk, KubeconfigPath() returns KUBECONFIG.
	st.Set("KUBECONFIG_PATH", ".awsbnkctl/nonexistent-relative/kubeconfig")
	st.Set("KUBECONFIG", existingCustom)
	if got := st.KubeconfigPath(); got != existingCustom {
		t.Fatalf("KubeconfigPath() with stale KUBECONFIG_PATH and valid KUBECONFIG = %q, want %q", got, existingCustom)
	}
}
