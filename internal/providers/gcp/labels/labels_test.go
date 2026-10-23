package labels

import "testing"

func TestRequired(t *testing.T) {
	got := Required("bnk-hostdev")
	if got[KeyCluster] != "bnk-hostdev" {
		t.Errorf("KeyCluster = %q, want bnk-hostdev", got[KeyCluster])
	}
	if got[KeyManaged] != "true" {
		t.Errorf("KeyManaged = %q, want true", got[KeyManaged])
	}
}

func TestMerge_LaterOverridesEarlier(t *testing.T) {
	base := map[string]string{"a": "1", "b": "2"}
	override := map[string]string{"b": "3", "c": "4"}
	got := Merge(base, override)
	want := map[string]string{"a": "1", "b": "3", "c": "4"}
	if len(got) != len(want) {
		t.Fatalf("Merge result has %d keys, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("Merge()[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestMerge_RequiredKeysSurviveUserOverride(t *testing.T) {
	// Required() first, then user-supplied maps — mandatory keys should still
	// be overridable (last map wins, matching awsbnkctl tags.Merge semantics)
	// but Required()'s presence guarantees they exist by default.
	got := Merge(Required("bnk-hostdev"), map[string]string{"env": "lab"})
	if got[KeyCluster] != "bnk-hostdev" {
		t.Errorf("KeyCluster = %q, want bnk-hostdev", got[KeyCluster])
	}
	if got["env"] != "lab" {
		t.Errorf("env = %q, want lab", got["env"])
	}
}

func TestMerge_NoArgsReturnsEmptyMap(t *testing.T) {
	got := Merge()
	if got == nil {
		t.Fatal("Merge() returned nil, want empty non-nil map")
	}
	if len(got) != 0 {
		t.Errorf("Merge() = %v, want empty map", got)
	}
}
