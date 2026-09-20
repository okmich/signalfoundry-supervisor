package ipc

import (
	"encoding/json"
	"errors"
	"os"
	"time"
)

// Windows refuses to replace-rename a file that another process has open without delete sharing,
// and a plain Go read holds the target that way for its (sub-millisecond) duration. The engine
// publishes and the TUI polls on independent 2s tickers, so the two overlap once every few hours
// and the loser gets ERROR_ACCESS_DENIED ("Access is denied"). The window is tiny, so a handful
// of short retries on each side resolves it without changing the one-writer / plain-file design.
const (
	swapRetries    = 10
	swapBackoffMin = 5 * time.Millisecond
	swapBackoffMax = 50 * time.Millisecond // worst case ~350ms total, well inside one 2s tick
)

// retrySwap runs fn until it succeeds, or until the retry budget is spent, doubling the pause between
// attempts. A not-exist error is not retried (the reader's case: nothing published yet) and is
// returned immediately.
func retrySwap(fn func() error) error {
	var err error
	backoff := swapBackoffMin
	for i := 0; i < swapRetries; i++ {
		if err = fn(); err == nil || errors.Is(err, os.ErrNotExist) {
			return err
		}
		time.Sleep(backoff)
		if backoff *= 2; backoff > swapBackoffMax {
			backoff = swapBackoffMax
		}
	}
	return err
}

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
	return retrySwap(func() error { return os.Rename(tmp, path) })
}

// ReadFleetState loads a published snapshot (the TUI's view source).
func ReadFleetState(path string) (FleetState, error) {
	var fs FleetState
	var b []byte
	err := retrySwap(func() error {
		var rerr error
		b, rerr = os.ReadFile(path)
		return rerr
	})
	if err != nil {
		return fs, err
	}
	return fs, json.Unmarshal(b, &fs)
}
