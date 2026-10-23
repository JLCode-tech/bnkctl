// Unit tests for `awsbnkctl k apply` (`internal/k8s/apply.go`).
//
// Apply.Run reaches a real REST config to build dynamic + discovery
// clients (server-side apply via dynamic.Interface.Patch needs a real
// API server). The pure parsing helpers — splitYAML, parseYAMLStream,
// loadKustomization, applyPatchOptions — are unit-testable in
// isolation; SSA round-trip is exercised end-to-end by the live golden
// tests.

package k8s

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const sampleConfigMapYAML = `apiVersion: v1
kind: ConfigMap
metadata:
  name: cm1
  namespace: default
data:
  key: value
`

const sampleMultiDocYAML = `apiVersion: v1
kind: ConfigMap
metadata:
  name: a
  namespace: default
data:
  k: v1
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: b
  namespace: default
data:
  k: v2
`

// TestApplyOptions_RequiresFilename: --filename / -f is mandatory.
func TestApplyOptions_RequiresFilename(t *testing.T) {
	o := &ApplyOptions{}
	err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for empty Filename; got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "filename") &&
		!strings.Contains(strings.ToLower(err.Error()), "-f") {
		t.Errorf("expected 'filename' / '-f' in err; got: %v", err)
	}
}

// TestParseYAMLStream_Single verifies a single-document YAML parses to
// one Unstructured.
func TestParseYAMLStream_Single(t *testing.T) {
	objs, err := parseYAMLStream(strings.NewReader(sampleConfigMapYAML))
	if err != nil {
		t.Fatalf("parseYAMLStream returned err: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("expected 1 object; got %d", len(objs))
	}
	if objs[0].GetKind() != "ConfigMap" {
		t.Errorf("kind: got %q, want ConfigMap", objs[0].GetKind())
	}
	if objs[0].GetName() != "cm1" {
		t.Errorf("name: got %q, want cm1", objs[0].GetName())
	}
}

// TestParseYAMLStream_Multi verifies multi-document YAML splits cleanly.
func TestParseYAMLStream_Multi(t *testing.T) {
	objs, err := parseYAMLStream(strings.NewReader(sampleMultiDocYAML))
	if err != nil {
		t.Fatalf("parseYAMLStream returned err: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("expected 2 objects; got %d", len(objs))
	}
	gotNames := []string{objs[0].GetName(), objs[1].GetName()}
	if !(gotNames[0] == "a" && gotNames[1] == "b") {
		t.Errorf("names: got %v, want [a b]", gotNames)
	}
}

// TestParseYAMLStream_EmptyDocsSkipped verifies leading/trailing/middle
// empty documents (blank or all whitespace) are dropped.
func TestParseYAMLStream_EmptyDocsSkipped(t *testing.T) {
	in := "---\n" + sampleConfigMapYAML + "\n---\n\n---\n"
	objs, err := parseYAMLStream(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parseYAMLStream returned err: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("expected 1 non-empty object; got %d", len(objs))
	}
}

// TestParseYAMLStream_BadYAML returns a descriptive error.
func TestParseYAMLStream_BadYAML(t *testing.T) {
	_, err := parseYAMLStream(strings.NewReader("not: yaml: : :"))
	if err == nil {
		t.Fatal("expected parse error; got nil")
	}
}

// TestSplitYAML_LeadingSeparator strips a leading "---\n" without
// producing an empty first part.
func TestSplitYAML_LeadingSeparator(t *testing.T) {
	in := []byte("---\nfoo: bar\n---\nbaz: qux\n")
	parts := splitYAML(in)
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts; got %d: %q", len(parts), parts)
	}
	if !bytes.Contains(parts[0], []byte("foo: bar")) {
		t.Errorf("first part lost content: %q", parts[0])
	}
	if !bytes.Contains(parts[1], []byte("baz: qux")) {
		t.Errorf("second part lost content: %q", parts[1])
	}
}

// TestApplyPatchOptions_DefaultFieldManager: server-side apply
// requires field-manager `awsbnkctl`.
func TestApplyPatchOptions_DefaultFieldManager(t *testing.T) {
	po := applyPatchOptions(false)
	if po.FieldManager != FieldManager {
		t.Errorf("FieldManager: got %q, want %q", po.FieldManager, FieldManager)
	}
	if FieldManager != "bnkctl" {
		t.Errorf("constant FieldManager value: got %q, want \"bnkctl\"", FieldManager)
	}
	if po.Force != nil {
		t.Errorf("Force should be nil when force=false; got %v", *po.Force)
	}
}

// TestApplyPatchOptions_Force flag toggles the SSA force-conflicts
// pointer.
func TestApplyPatchOptions_Force(t *testing.T) {
	po := applyPatchOptions(true)
	if po.Force == nil || !*po.Force {
		t.Errorf("Force: expected non-nil true; got %v", po.Force)
	}
}

// TestLoadObjects_MissingFile: a non-existent -f path produces an error.
func TestLoadObjects_MissingFile(t *testing.T) {
	o := &ApplyOptions{Filename: filepath.Join(t.TempDir(), "nope.yaml")}
	_, err := o.loadObjects()
	if err == nil {
		t.Fatal("expected error for missing file; got nil")
	}
}

// TestLoadObjects_DirectoryRecursesYAMLOnly: only .yaml/.yml files are
// picked up; README.md is skipped.
func TestLoadObjects_DirectoryRecursesYAMLOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(sampleConfigMapYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := &ApplyOptions{Filename: dir}
	objs, err := o.loadObjects()
	if err != nil {
		t.Fatalf("loadObjects err: %v", err)
	}
	if len(objs) != 1 {
		t.Errorf("expected 1 object (README.md skipped); got %d", len(objs))
	}
}

// TestLoadObjects_SingleFile: a plain file path goes through the
// single-file branch.
func TestLoadObjects_SingleFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.yaml")
	if err := os.WriteFile(p, []byte(sampleConfigMapYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	o := &ApplyOptions{Filename: p}
	objs, err := o.loadObjects()
	if err != nil {
		t.Fatalf("loadObjects err: %v", err)
	}
	if len(objs) != 1 {
		t.Errorf("single file: expected 1 object; got %d", len(objs))
	}
}

// quiet metav1 unused-import warning when only sub-tests touch it.
var _ = metav1.PatchOptions{}
var _ = errors.New
