package ipc

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestPublishReadRace hammers Publish against ReadFleetState the way the engine and TUI race on
// Windows. The reader polls every millisecond (2000x the TUI's real cadence); without the retry in
// place this fails within milliseconds with "Access is denied".
func TestPublishReadRace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet_state.json")
	if err := Publish(path, FleetState{}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var readErr atomic.Value
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := ReadFleetState(path); err != nil {
				readErr.Store(err)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := Publish(path, FleetState{}); err != nil {
			close(stop)
			t.Fatalf("publish: %v", err)
		}
	}
	close(stop)
	if err := readErr.Load(); err != nil {
		t.Fatalf("read: %v", err)
	}
}
