package ipc

import (
	"encoding/json"
	"os"
	"time"
)

// Publish atomically writes the fleet snapshot (tmp -> rename) so a reader never sees a torn file.
func Publish(path string, fs FleetState) error {
	fs.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(fs, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadFleetState loads a published snapshot (the TUI's view source).
func ReadFleetState(path string) (FleetState, error) {
	var fs FleetState
	b, err := os.ReadFile(path)
	if err != nil {
		return fs, err
	}
	return fs, json.Unmarshal(b, &fs)
}
