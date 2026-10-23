// Unit tests for `awsbnkctl k get` (`internal/k8s/get.go`).
//
// Staff's GetOptions.Run drives cli-runtime's resource.Builder, which
// requires a real RESTClientGetter and reaches a real cluster — not
// fakeable with `kubernetes/fake`. The byte-equivalence check is
// delivered by the live golden tests in golden_test.go (build-tag
// `live`); these unit tests cover what's testable without a cluster:
//
//   - GetOptions zero-value validation: missing args
//   - IsNotFound helper round-trips a real API "not found" error
//   - DefaultKubeconfigPath behaviour (which Get falls back to)
//
// Note: the original brief assumed a fake-clientset-friendly
// surface; the chosen design (cli-runtime resource.Builder) is the
// kubectl-byte-equivalent path, but trades unit testability for output
// fidelity. The golden tests provide end-to-end coverage.

package k8s

import (
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestGetOptions_RequiresArgs ensures Run errors out if the caller
// forgets to populate Args. The real cobra layer enforces this with
// MinimumNArgs(1); we still want the library entry point to refuse
// gracefully so library callers don't silently no-op.
func TestGetOptions_RequiresArgs(t *testing.T) {
	o := &GetOptions{}
	err := o.Run()
	if err == nil {
		t.Fatal("expected error for empty Args; got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "resource") {
		t.Errorf("expected 'resource' wording in err; got: %v", err)
	}
}

// TestIsNotFound_TrueForAPINotFound rounds-trips a real API NotFound
// error through IsNotFound. Cheap regression guard against future code
// motion that swaps apierrors.IsNotFound for something stricter.
func TestIsNotFound_TrueForAPINotFound(t *testing.T) {
	gr := schema.GroupResource{Group: "", Resource: "pods"}
	apiErr := apierrors.NewNotFound(gr, "missing")
	if !IsNotFound(apiErr) {
		t.Errorf("IsNotFound(apierrors.NewNotFound) returned false")
	}
}

// TestIsNotFound_FalseForOtherErrors verifies non-NotFound errors don't
// confuse IsNotFound.
func TestIsNotFound_FalseForOtherErrors(t *testing.T) {
	cases := []error{
		errors.New("network unreachable"),
		apierrors.NewBadRequest("invalid"),
		apierrors.NewUnauthorized("nope"),
		nil,
	}
	for i, e := range cases {
		if IsNotFound(e) {
			t.Errorf("case %d: IsNotFound returned true for non-not-found %v", i, e)
		}
	}
}

// TestDefaultKubeconfigPath_NoEnvNoFile verifies the helper returns
// empty cleanly when neither $KUBECONFIG nor ~/.kube/config exists.
// We achieve "no file" by pointing $KUBECONFIG at a path under a fresh
// temp dir.
func TestDefaultKubeconfigPath_NoEnvNoFile(t *testing.T) {
	tdir := t.TempDir()
	t.Setenv("KUBECONFIG", tdir+"/none")
	t.Setenv("HOME", tdir) // ~/.kube/config under the fresh tdir
	got := DefaultKubeconfigPath()
	if got != "" {
		t.Errorf("expected empty path with no kubeconfig present; got %q", got)
	}
}

// quiet unused-symbol warnings for imports kept for parallel test
// files in the package.
var _ = metav1.ObjectMeta{}
