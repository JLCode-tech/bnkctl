package manifests

import (
	"strings"
	"testing"
)

// Multus chroots into the node filesystem (chrootDir: /hostroot) before
// exec'ing a delegate CNI plugin, so daemon-config.json's binDir must be the
// path as it exists ON THE HOST — which is precisely what the DaemonSet's
// `cnibin` volume hostPaths. If these two drift apart, multus cannot find
// cilium-cni and EVERY new pod sandbox on the cluster fails to start, which
// takes TMM down with it. Live-hit 2026-07-21 on cluster bnk-scratch, where
// binDir was /opt/cni/bin — a container-side mount alias that does not exist
// on a GKE node.
func TestMultusBinDirMatchesDaemonSetHostPath(t *testing.T) {
	const wantBinDir = "/home/kubernetes/bin"

	cm, err := FS.ReadFile("multus/02-multus-configmap.yaml")
	if err != nil {
		t.Fatalf("reading multus configmap: %v", err)
	}
	if !strings.Contains(string(cm), `"binDir": "`+wantBinDir+`"`) {
		t.Errorf("multus daemon-config binDir is not %q — new pod sandboxes will fail with\n"+
			"  fork/exec <binDir>/cilium-cni: no such file or directory", wantBinDir)
	}

	ds, err := FS.ReadFile("multus/02-multus-ds.yaml")
	if err != nil {
		t.Fatalf("reading multus daemonset: %v", err)
	}
	// The cnibin volume's hostPath is the authoritative host-side location.
	if !strings.Contains(string(ds), "path: "+wantBinDir) {
		t.Errorf("multus DaemonSet no longer hostPaths %q — binDir and the cnibin volume must agree", wantBinDir)
	}
}
