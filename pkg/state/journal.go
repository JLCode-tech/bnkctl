package state

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"time"
)

// DefaultJournalPath is the standard location for workspace journal.
const DefaultJournalPath = "artifacts/journal.jsonl"

// JournalEntry represents a single audit or operational journal record.
type JournalEntry struct {
	Timestamp string            `json:"timestamp"`
	Actor     string            `json:"actor,omitempty"`
	Phase     string            `json:"phase,omitempty"`
	Action    string            `json:"action"`
	Cluster   string            `json:"cluster,omitempty"`
	Target    string            `json:"target,omitempty"`
	Status    string            `json:"status,omitempty"`
	Details   string            `json:"details,omitempty"`
	Message   string            `json:"message,omitempty"`
	Tags      []string          `json:"tags,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// AppendJournal appends an operational journal entry to artifacts/journal.jsonl.
func AppendJournal(artifactsDir, phase, action, target, status, details string) error {
	if err := os.MkdirAll(artifactsDir, 0o750); err != nil {
		return err
	}
	return AppendEntry(filepath.Join(artifactsDir, "journal.jsonl"), JournalEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Actor:     currentUsername(),
		Phase:     phase,
		Action:    action,
		Target:    target,
		Status:    status,
		Details:   details,
		Message:   details,
	})
}

// ReadJournal reads all journal entries from artifacts/journal.jsonl.
func ReadJournal(artifactsDir string) ([]JournalEntry, error) {
	return ListEntries(filepath.Join(artifactsDir, "journal.jsonl"))
}

// AppendEntry writes a new JournalEntry to the journal file at path.
func AppendEntry(path string, e JournalEntry) error {
	if path == "" {
		path = DefaultJournalPath
	}
	if e.Timestamp == "" {
		e.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	if e.Actor == "" {
		e.Actor = currentUsername()
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	// #nosec G304 -- operator workspace journal file path
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open journal %s: %w", path, err)
	}
	defer f.Close()

	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal journal entry: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write journal entry: %w", err)
	}
	return nil
}

// ListEntries reads all entries from the journal file at path.
func ListEntries(path string) ([]JournalEntry, error) {
	if path == "" {
		path = DefaultJournalPath
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}

	// #nosec G304 -- operator workspace journal file path
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open journal %s: %w", path, err)
	}
	defer f.Close()

	var entries []JournalEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e JournalEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan journal %s: %w", path, err)
	}
	return entries, nil
}

// RenderText writes formatted journal entries to w.
func RenderText(w io.Writer, entries []JournalEntry) {
	if len(entries) == 0 {
		fmt.Fprintln(w, "No journal entries found.")
		return
	}

	fmt.Fprintf(w, "%-20s  %-12s  %-10s  %s\n", "TIMESTAMP", "ACTOR", "ACTION", "MESSAGE")
	fmt.Fprintf(w, "%s\n", "--------------------------------------------------------------------------------")
	for _, e := range entries {
		ts := e.Timestamp
		if t, err := time.Parse(time.RFC3339, e.Timestamp); err == nil {
			ts = t.Format("2006-01-02 15:04:05")
		}
		action := e.Action
		if action == "" {
			action = "NOTE"
		}
		msg := e.Message
		if msg == "" {
			msg = e.Details
		}
		fmt.Fprintf(w, "%-20s  %-12s  %-10s  %s\n", ts, e.Actor, action, msg)
	}
}

func currentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if env := os.Getenv("USER"); env != "" {
		return env
	}
	return "unknown"
}
