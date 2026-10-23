package forge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// LinkFileName is the on-disk file inside a workspace dir that records
// which forge project + cluster azrbnkctl registered the workspace as.
const LinkFileName = "forge_link.json"

// Link is the persisted association between an azrbnkctl workspace and
// a forge project + cluster.
type Link struct {
	ForgeURL     string    `json:"forge_url"`
	ForgeMCPURL  string    `json:"forge_mcp_url"`
	ProjectID    int       `json:"project_id"`
	ProjectName  string    `json:"project_name"`
	ClusterID    int       `json:"cluster_id"`
	ClusterName  string    `json:"cluster_name"`
	RegisteredAt time.Time `json:"registered_at"`
	Workspace    string    `json:"workspace"`
	Status       string    `json:"status,omitempty"`
}

// IsRegistered reports whether the link represents a completed registration.
func (l *Link) IsRegistered() bool {
	return l != nil && (l.Status == "" || l.Status == "registered") && l.ClusterID > 0 && l.ProjectID > 0
}

// LinkPath returns the absolute path to forge_link.json in the workspace directory.
func LinkPath(workspaceDir string) string {
	return filepath.Join(workspaceDir, LinkFileName)
}

// ReadLink loads forge_link.json. Returns (nil, os.ErrNotExist) when missing.
func ReadLink(workspaceDir string) (*Link, error) {
	p := LinkPath(workspaceDir)
	b, err := os.ReadFile(p) // #nosec G304 -- workspace managed path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	var l Link
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return &l, nil
}

// WriteLink atomically replaces forge_link.json in the workspace dir.
func WriteLink(workspaceDir string, l *Link) error {
	if err := os.MkdirAll(workspaceDir, 0o750); err != nil {
		return fmt.Errorf("ensure workspace dir: %w", err)
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	final := LinkPath(workspaceDir)
	tmp, err := os.CreateTemp(workspaceDir, "forge_link.*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, final, err)
	}
	return nil
}

// RemoveLink deletes forge_link.json. Returns nil if it was already absent.
func RemoveLink(workspaceDir string) error {
	err := os.Remove(LinkPath(workspaceDir))
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
