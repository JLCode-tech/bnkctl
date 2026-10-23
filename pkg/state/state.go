// Package state provides persistent JSONL resource state and operational journal
// tracking across cloud underlay and BNK overlay operations.
package state

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ResourceEntry records a provisioned cloud or cluster resource in state.jsonl.
type ResourceEntry struct {
	Type      string            `json:"type"`
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Timestamp string            `json:"timestamp"`
}

// SaveResource appends a single resource entry to artifacts/state.jsonl.
func SaveResource(artifactsDir, resType, id, name string, metadata map[string]string) error {
	if err := os.MkdirAll(artifactsDir, 0o750); err != nil {
		return err
	}
	// #nosec G304 -- state.jsonl inside operator workspace artifacts directory
	f, err := os.OpenFile(filepath.Join(artifactsDir, "state.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	entry := ResourceEntry{
		Type:      resType,
		ID:        id,
		Name:      name,
		Metadata:  metadata,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(f, "%s\n", string(data))
	return err
}

// LoadResources reads all resource entries from artifacts/state.jsonl.
func LoadResources(artifactsDir string) ([]ResourceEntry, error) {
	path := filepath.Join(artifactsDir, "state.jsonl")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}
	// #nosec G304 -- state.jsonl inside operator workspace artifacts directory
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []ResourceEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var entry ResourceEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err == nil {
			entries = append(entries, entry)
		}
	}
	return entries, scanner.Err()
}

// Clear removes artifacts/state.jsonl if present.
func Clear(artifactsDir string) error {
	path := filepath.Join(artifactsDir, "state.jsonl")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	return os.Remove(path)
}
