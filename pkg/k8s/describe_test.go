// Unit tests for `awsbnkctl k describe` (`internal/k8s/describe.go`).
//
// As with get_test.go, Describe drives cli-runtime's resource.Builder
// which can't be fakeable without a real REST client. Byte-equivalence
// vs kubectl is the responsibility of the live golden tests
// (golden_test.go). What we cover here:
//
//   - DescribeOptions zero-value validation: missing args

package k8s

import (
	"strings"
	"testing"
)

// TestDescribeOptions_RequiresArgs ensures Run errors when the caller
// passes no positional args. Cobra enforces this for the user-facing
// path; library callers get a clear message rather than a silent no-op.
func TestDescribeOptions_RequiresArgs(t *testing.T) {
	o := &DescribeOptions{}
	err := o.Run()
	if err == nil {
		t.Fatal("expected error for empty Args; got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "resource") {
		t.Errorf("expected 'resource' wording in err; got: %v", err)
	}
}
