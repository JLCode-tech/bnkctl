// Unit tests for `awsbnkctl k delete` (`internal/k8s/delete.go`).
//
// Delete.Run drives cli-runtime resource.Builder + resource.Helper,
// neither of which is fakeable without a real REST client. We unit-test
// the propagation-policy mapping (cascade flag round-trip into the
// metav1 enum) and the option-validation code; SSA + cascade end-to-end
// is exercised by the live golden tests.

package k8s

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestDeleteOptions_RequiresArgs: missing positional args errors out.
func TestDeleteOptions_RequiresArgs(t *testing.T) {
	o := &DeleteOptions{}
	err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for empty Args; got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "resource") {
		t.Errorf("expected 'resource' in err; got: %v", err)
	}
}

// TestPropagationFor_Background maps to PropagationBackground.
func TestPropagationFor_Background(t *testing.T) {
	got := propagationFor(CascadeBackground)
	if got == nil {
		t.Fatal("got nil; expected non-nil")
	}
	if *got != metav1.DeletePropagationBackground {
		t.Errorf("got %v; want Background", *got)
	}
}

// TestPropagationFor_Foreground maps to PropagationForeground.
func TestPropagationFor_Foreground(t *testing.T) {
	got := propagationFor(CascadeForeground)
	if got == nil || *got != metav1.DeletePropagationForeground {
		t.Errorf("got %v; want Foreground", got)
	}
}

// TestPropagationFor_Orphan maps to PropagationOrphan.
func TestPropagationFor_Orphan(t *testing.T) {
	got := propagationFor(CascadeOrphan)
	if got == nil || *got != metav1.DeletePropagationOrphan {
		t.Errorf("got %v; want Orphan", got)
	}
}

// TestPropagationFor_EmptyOrUnknown returns nil so the API uses the
// resource's default.
func TestPropagationFor_EmptyOrUnknown(t *testing.T) {
	cases := []DeleteCascade{"", DeleteCascade("garbage")}
	for _, c := range cases {
		t.Run(string(c), func(t *testing.T) {
			if got := propagationFor(c); got != nil {
				t.Errorf("expected nil for %q; got %v", c, *got)
			}
		})
	}
}

// TestDeleteCascadeConstants: drift guard. The cascade enum strings
// must match kubectl's --cascade values exactly so users with kubectl
// muscle memory don't get a surprise.
func TestDeleteCascadeConstants(t *testing.T) {
	cases := []struct {
		got, want DeleteCascade
	}{
		{CascadeBackground, "background"},
		{CascadeForeground, "foreground"},
		{CascadeOrphan, "orphan"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("constant value drift: %q != %q", c.got, c.want)
		}
	}
}
