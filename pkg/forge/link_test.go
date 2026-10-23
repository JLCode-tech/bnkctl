package forge_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JLCode-tech/bnkctl/pkg/forge"
)

func TestLink_Persistence(t *testing.T) {
	tmpDir := t.TempDir()
	wsDir := filepath.Join(tmpDir, ".azrbnkctl", "my-cluster")

	t.Run("read non-existent link", func(t *testing.T) {
		link, err := forge.ReadLink(wsDir)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected os.ErrNotExist, got %v (link: %+v)", err, link)
		}
	})

	testLink := &forge.Link{
		ForgeURL:     "http://localhost:8000",
		ForgeMCPURL:  "http://localhost:8081/mcp/",
		ProjectID:    12,
		ProjectName:  "azrbnkctl-my-cluster",
		ClusterID:    45,
		ClusterName:  "my-cluster",
		RegisteredAt: time.Now().UTC().Truncate(time.Second),
		Workspace:    "my-cluster",
		Status:       "registered",
	}

	t.Run("write and read link", func(t *testing.T) {
		if err := forge.WriteLink(wsDir, testLink); err != nil {
			t.Fatalf("WriteLink: %v", err)
		}

		got, err := forge.ReadLink(wsDir)
		if err != nil {
			t.Fatalf("ReadLink: %v", err)
		}
		if got.ProjectID != testLink.ProjectID || got.ClusterID != testLink.ClusterID {
			t.Errorf("got %+v, want %+v", got, testLink)
		}
		if !got.IsRegistered() {
			t.Errorf("expected IsRegistered() == true")
		}
	})

	t.Run("remove link", func(t *testing.T) {
		if err := forge.RemoveLink(wsDir); err != nil {
			t.Fatalf("RemoveLink: %v", err)
		}
		_, err := forge.ReadLink(wsDir)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected os.ErrNotExist after remove, got %v", err)
		}
	})
}
