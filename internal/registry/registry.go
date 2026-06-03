// Package registry persists the engine's own record of the systems it spawned — the re-attach
// source of truth (FLEET_SUPERVISOR_SPEC §12), always reconciled against psutil, never trusted blindly.
package registry

import (
	"encoding/json"
	"os"
	"time"
)

// Entry records a Running system's confirmed identity — the re-attach source of truth (§12) and the
// persistent baseline for the PID-reuse guard (§9): a recycled PID has a different create-time, so
// CreateTime is what lets a restarted engine tell a genuine re-attach from a stranger on the same PID.
type Entry struct {
	SystemID   string    `json:"system_id"`
	PID        int       `json:"pid"`
	StartToken string    `json:"start_token"`
	CreateTime time.Time `json:"create_time"`
}

type Registry struct {
	Entries map[string]Entry `json:"entries"`
}

// Load reads process_registry.json (empty registry if absent).
func Load(path string) (Registry, error) {
	r := Registry{Entries: map[string]Entry{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return r, nil
		}
		return r, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, err
	}
	if r.Entries == nil {
		r.Entries = map[string]Entry{}
	}
	return r, nil
}

// Save atomically writes the registry.
func Save(path string, r Registry) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
