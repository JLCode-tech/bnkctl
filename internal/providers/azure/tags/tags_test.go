package tags

import (
	"testing"
)

func TestForCluster(t *testing.T) {
	tg := ForCluster("test-cluster", "vnet", map[string]string{"env": "poc"})

	if tg[TagCluster] == nil || *tg[TagCluster] != "test-cluster" {
		t.Errorf("TagCluster = %v, want test-cluster", tg[TagCluster])
	}
	if tg[TagComponent] == nil || *tg[TagComponent] != "vnet" {
		t.Errorf("TagComponent = %v, want vnet", tg[TagComponent])
	}
	if tg[TagManaged] == nil || *tg[TagManaged] != "true" {
		t.Errorf("TagManaged = %v, want true", tg[TagManaged])
	}
	if tg["env"] == nil || *tg["env"] != "poc" {
		t.Errorf("env tag = %v, want poc", tg["env"])
	}

	if !IsManagedByCluster(tg, "test-cluster") {
		t.Errorf("expected IsManagedByCluster to return true")
	}
	if IsManagedByCluster(tg, "other-cluster") {
		t.Errorf("expected IsManagedByCluster to return false for other cluster")
	}
}
