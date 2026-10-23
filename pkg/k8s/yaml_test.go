package k8s

import "testing"

func TestSplitYAMLDocs_LeadingSeparator(t *testing.T) {
	in := []byte("---\nfoo: bar\n---\nbaz: qux\n")
	parts := SplitYAMLDocs(in)
	if len(parts) != 2 {
		t.Fatalf("SplitYAMLDocs: got %d parts, want 2", len(parts))
	}
	if string(parts[0]) != "foo: bar" {
		t.Errorf("part 0: got %q, want 'foo: bar'", string(parts[0]))
	}
	if string(parts[1]) != "baz: qux\n" {
		t.Errorf("part 1: got %q, want 'baz: qux\\n'", string(parts[1]))
	}
}
