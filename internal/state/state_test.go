package state

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// A multi-trader is one runner row carrying one liveness leg per logical system (read from each
// symbol's own inference dir at its own timeframe). The row's bar-age reflects the STALEST leg, so a
// dead leg surfaces in the fleet view even though the runner PID is alive (runner-level liveness, §15).
func TestReconcileMultiTraderLegs(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "live")
	logb := filepath.Join(root, "log")
	const runner = "basket-multi"

	artefact := filepath.Join(live, "basket")
	if err := os.MkdirAll(artefact, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artefact, "run.py"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// A non-empty strategies[] marks a multi-trader (discovery), mapped to the <strategy>-multi runner.
	if err := os.WriteFile(filepath.Join(artefact, "config.json"),
		[]byte(`{"strategies":[{"name":"basket","symbol":"EURUSD"},{"name":"basket","symbol":"GBPUSD"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	statusDir := filepath.Join(logb, runner)
	if err := os.MkdirAll(statusDir, 0o755); err != nil {
		t.Fatal(err)
	}
	status := fmt.Sprintf(`{"state":"running","pid":%d,"logical_systems":[{"symbol":"EURUSD","timeframe":5},{"symbol":"GBPUSD","timeframe":5}]}`, os.Getpid())
	if err := os.WriteFile(filepath.Join(statusDir, "status.json"), []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	day := now.Format("20060102")
	writeBar := func(symbol string, asof time.Time) {
		dir := filepath.Join(statusDir, symbol, "5", "inference")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		line := fmt.Sprintf(`{"event":"bar","asof_bar_ts":%q}`+"\n", asof.Format(time.RFC3339Nano))
		if err := os.WriteFile(filepath.Join(dir, "inference_"+day+".jsonl"), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fresh := now.Add(-2 * time.Minute)
	stale := now.Add(-90 * time.Minute)
	writeBar("EURUSD", fresh)
	writeBar("GBPUSD", stale) // the stalest leg

	systems := Reconcile(config.Config{LiveBase: live, LogBase: logb})
	var multi *ipc.System
	for i := range systems {
		if systems[i].SystemID == runner {
			multi = &systems[i]
		}
	}
	if multi == nil {
		t.Fatalf("multi runner row %q not found; got %+v", runner, systems)
	}
	if multi.State != ipc.StateRunning {
		t.Fatalf("multi runner should be Running, got %s", multi.State)
	}
	if len(multi.Legs) != 2 {
		t.Fatalf("expected 2 legs, got %d (%+v)", len(multi.Legs), multi.Legs)
	}
	if !multi.LastBarTS.Equal(stale) {
		t.Errorf("row bar-age should reflect the STALEST leg: got %s, want %s", multi.LastBarTS, stale)
	}
}
