package state

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/okmich/signalfoundry-supervisor/internal/config"
	"github.com/okmich/signalfoundry-supervisor/internal/ipc"
)

// A runner's status.json covers only the symbols in its logical_systems[]. A sibling row under the
// same strategy that the runner does NOT cover must stay Stopped — it must not inherit the runner's
// live PID/state (which would make it a wrong stop/kill target).
func TestReconcileGatesOnLogicalSystems(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "live")
	logb := filepath.Join(root, "log")
	const strat = "rsi2_mean_reversion"

	for _, sym := range []string{"EURUSD", "GBPUSD"} {
		dir := filepath.Join(live, strat, sym, "M5")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "run.py"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	statusDir := filepath.Join(logb, strat)
	if err := os.MkdirAll(statusDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// One runner, running as THIS (alive) process, claiming only EURUSD.
	status := fmt.Sprintf(`{"state":"running","pid":%d,"logical_systems":[{"symbol":"EURUSD","timeframe":5}]}`, os.Getpid())
	if err := os.WriteFile(filepath.Join(statusDir, "status.json"), []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}

	got := map[string]ipc.State{}
	for _, s := range Reconcile(config.Config{LiveBase: live, LogBase: logb}) {
		got[s.SystemID] = s.State
	}
	if got[strat+"/EURUSD/M5"] != ipc.StateRunning {
		t.Errorf("EURUSD (covered + alive pid) should be Running, got %s", got[strat+"/EURUSD/M5"])
	}
	if got[strat+"/GBPUSD/M5"] != ipc.StateStopped {
		t.Errorf("GBPUSD (not in logical_systems) should stay Stopped, got %s", got[strat+"/GBPUSD/M5"])
	}
}
