package ipc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PendingCommands returns command files in dir that have no .result.json yet (dedup: a written
// result marks a command processed, so a restarted engine never re-runs it).
func PendingCommands(dir string) ([]Command, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cmds []Command
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".result.json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if _, err := os.Stat(filepath.Join(dir, id+".result.json")); err == nil {
			continue // already processed
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var c Command
		if json.Unmarshal(b, &c) == nil {
			// The FILENAME is the canonical id: the dedup marker is <id>.result.json, derived from
			// the filename, so the result must key off it too. A hand-broken file with an empty or
			// mismatched payload id would otherwise never get a matching marker and reprocess forever.
			c.ID = id
			cmds = append(cmds, c)
		}
	}
	return cmds, nil
}

// PruneCommands removes command and result files older than ttl, bounding the commands dir (a
// processed command keeps its result only long enough for the TUI to read it — seconds — so ttl is
// the safety margin). A command left unprocessed past ttl means the engine was down that long, so
// dropping it as stale is correct.
func PruneCommands(dir string, ttl time.Duration, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > ttl {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// WriteResult records a command outcome (and serves as the processed-marker for dedup).
func WriteResult(dir string, r CommandResult) error {
	r.Completed = time.Now().UTC()
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, r.ID+".result.json"), b, 0o644)
}
