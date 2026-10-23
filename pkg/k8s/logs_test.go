// Unit tests for `awsbnkctl k logs` (`internal/k8s/logs.go`).
//
// LogsOptions.Run reaches a real REST config to build the typed client
// (corev1.Pods.GetLogs streams over a real connection). The pure helper
// `ParseSinceDuration` is unit-testable in isolation; option-mapping is
// guarded by validation tests.

package k8s

import (
	"context"
	"strings"
	"testing"
)

// TestLogsOptions_RequiresPodName: the raw-pod-name path needs a pod
// name; without it Run returns an error rather than reaching for
// "default".
func TestLogsOptions_RequiresPodName(t *testing.T) {
	o := &LogsOptions{}
	err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for empty PodName; got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "pod") {
		t.Errorf("expected 'pod' in err; got: %v", err)
	}
}

// TestParseSinceDuration_Empty returns -1 (sentinel for "no clamp").
func TestParseSinceDuration_Empty(t *testing.T) {
	got, err := ParseSinceDuration("")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != -1 {
		t.Errorf("expected -1 for empty; got %d", got)
	}
}

// TestParseSinceDuration_Standard converts kubectl-style strings
// correctly.
func TestParseSinceDuration_Standard(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"5m", 300},
		{"1h", 3600},
		{"30s", 30},
		{"1h30m", 5400},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParseSinceDuration(c.in)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != c.want {
				t.Errorf("--since=%s: got %d; want %d", c.in, got, c.want)
			}
		})
	}
}

// TestParseSinceDuration_Bad: malformed strings return a clear error.
func TestParseSinceDuration_Bad(t *testing.T) {
	cases := []string{"not-a-duration", "5x", "abc"}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			_, err := ParseSinceDuration(c)
			if err == nil {
				t.Errorf("expected error for %q; got nil", c)
			}
		})
	}
}
