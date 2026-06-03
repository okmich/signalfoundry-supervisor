package ipc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// cmdSeq guarantees unique command ids even when several are submitted within one clock tick (a
// bulk fan-out loops faster than the wall clock's resolution, so a timestamp alone can collide).
var cmdSeq atomic.Uint64

// SubmitCommand (TUI side) drops a command file for the engine to pick up; returns its id.
func SubmitCommand(dir, action, systemID string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	c := Command{
		ID:       fmt.Sprintf("%s-%d", time.Now().UTC().Format("20060102T150405.000000000Z"), cmdSeq.Add(1)),
		Action:   action,
		SystemID: systemID,
		IssuedAt: time.Now().UTC(),
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, c.ID+".json.tmp")
	final := filepath.Join(dir, c.ID+".json")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return "", err
	}
	return c.ID, os.Rename(tmp, final)
}

// ReadResult (TUI side) fetches a command's result if the engine has written one yet.
func ReadResult(dir, id string) (CommandResult, bool) {
	var r CommandResult
	b, err := os.ReadFile(filepath.Join(dir, id+".result.json"))
	if err != nil {
		return r, false
	}
	return r, json.Unmarshal(b, &r) == nil
}
