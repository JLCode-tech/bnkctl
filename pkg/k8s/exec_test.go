// Unit tests for `awsbnkctl k exec` (`internal/k8s/exec.go`).
//
// SPDY exec needs a real upgradeable connection — not fakeable. The
// validation surface is what's testable: missing pod, missing command,
// option zero-values. End-to-end exec is exercised by the live golden
// tests against a real cluster.

package k8s

import (
	"context"
	"strings"
	"testing"
)

// TestExecOptions_RequiresPod: missing PodName errors out.
func TestExecOptions_RequiresPod(t *testing.T) {
	o := &ExecOptions{Command: []string{"ls"}}
	err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for empty PodName; got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "pod") {
		t.Errorf("expected 'pod' in err; got: %v", err)
	}
}

// TestExecOptions_RequiresCommand: an empty Command slice errors out
// (matches kubectl: `kubectl exec foo` without `--` is a usage error).
func TestExecOptions_RequiresCommand(t *testing.T) {
	o := &ExecOptions{PodName: "p"}
	err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for empty Command; got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "command") {
		t.Errorf("expected 'command' in err; got: %v", err)
	}
}

// TestExecOptions_StderrTTYInteraction: when TTY=true the underlying
// PodExecOptions sets Stderr=false (TTY merges stderr into stdout).
// We can't intercept the request without a real apiserver, but we can
// at least sanity-check that ExecOptions stores the inputs we expect.
//
// Drift guard: the exec implementation merges stderr into stdout when
// TTY=true (-t flag); the TTY field on ExecOptions should round-trip.
func TestExecOptions_StderrTTYInteraction(t *testing.T) {
	o := &ExecOptions{
		PodName: "p",
		Command: []string{"sh"},
		TTY:     true,
		Stdin:   true,
	}
	if !o.TTY || !o.Stdin {
		t.Errorf("ExecOptions field round-trip failed: TTY=%v Stdin=%v", o.TTY, o.Stdin)
	}
}
