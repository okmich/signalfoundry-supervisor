package state

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/okmich/signalfoundry-supervisor/internal/config"
	"github.com/okmich/signalfoundry-supervisor/internal/ipc"
)

const acct = "fxify.demo"

// box lays out a scratch LIVE_BASE / LOG_BASE / ENV_DIR with one account, fxify.demo, whose env file
// carries LOGIN_ID 42.
func box(t *testing.T) (cfg config.Config, live, logb string) {
	t.Helper()
	root := t.TempDir()
	cfg = config.Config{LiveBase: filepath.Join(root, "live"), LogBase: filepath.Join(root, "log"), EnvDir: filepath.Join(root, "env")}
	if err := os.MkdirAll(cfg.EnvDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.EnvDir, ".env."+acct), []byte("LOGIN_ID=42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg, filepath.Join(cfg.LiveBase, acct), filepath.Join(cfg.LogBase, acct)
}

func writeStatus(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNewestTextLog(t *testing.T) {
	dir := t.TempDir()
	// none yet -> empty (details pane degrades to "(no log yet)")
	if got := newestTextLog(dir); got != "" {
		t.Errorf("no logs should yield \"\", got %q", got)
	}
	// timestamped names sort chronologically -> newest is the lexically-greatest
	for _, name := range []string{"z_system_log_250101120000.log", "z_system_log_250607093000.log", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := newestTextLog(dir); filepath.Base(got) != "z_system_log_250607093000.log" {
		t.Errorf("newestTextLog = %q, want the newest z_*.log", got)
	}
}

// A runner's status.json covers only the symbols in its logical_systems[]. A sibling row under the
// same strategy that the runner does NOT cover must stay Stopped — it must not inherit the runner's
// live PID/state (which would make it a wrong stop/kill target).
func TestReconcileGatesOnLogicalSystems(t *testing.T) {
	cfg, live, logb := box(t)
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
	status := fmt.Sprintf(`{"state":"running","pid":%d,"account":"fxify.demo","account_id":"42","logical_systems":[{"symbol":"EURUSD","timeframe":5}]}`, os.Getpid())
	if err := os.WriteFile(filepath.Join(statusDir, "status.json"), []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}

	got := map[string]ipc.State{}
	systems, problems := Reconcile(cfg)
	for _, s := range systems {
		got[s.SystemID] = s.State
		if s.AccountMismatch != "" {
			t.Errorf("%s: unexpected account mismatch %q", s.SystemID, s.AccountMismatch)
		}
	}
	if len(problems) != 0 {
		t.Errorf("unexpected problems %+v", problems)
	}
	if id := acct + "/" + strat + "/EURUSD/M5"; got[id] != ipc.StateRunning {
		t.Errorf("EURUSD (covered + alive pid) should be Running, got %s", got[id])
	}
	if id := acct + "/" + strat + "/GBPUSD/M5"; got[id] != ipc.StateStopped {
		t.Errorf("GBPUSD (not in logical_systems) should stay Stopped, got %s", got[id])
	}
}

// A multi-trader is one runner row carrying one liveness leg per logical system (read from each
// symbol's own inference dir at its own timeframe). The row's bar-age reflects the STALEST leg, so a
// dead leg surfaces in the fleet view even though the runner PID is alive (runner-level liveness, §15).
func TestReconcileMultiTraderLegs(t *testing.T) {
	cfg, live, logb := box(t)
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
	status := fmt.Sprintf(`{"state":"running","pid":%d,"account":"fxify.demo","logical_systems":[{"symbol":"EURUSD","timeframe":5},{"symbol":"GBPUSD","timeframe":5}]}`, os.Getpid())
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

	systems, _ := Reconcile(cfg)
	var multi *ipc.System
	for i := range systems {
		if systems[i].SystemID == acct+"/"+runner {
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

// A running system is checked against its account folder: the account its status.json was written
// under, and the terminal's login against the env file's LOGIN_ID.
func TestReconcileFlagsAccountMismatch(t *testing.T) {
	cases := []struct {
		name, account, login, want string
	}{
		{"agrees", `"account":"fxify.demo",`, `"42"`, ""},
		{"v1 runner", ``, `"42"`, "predates the account layout"},
		{"wrong folder", `"account":"icmarkets.demo",`, `"42"`, "logs under account icmarkets.demo"},
		{"wrong login", `"account":"fxify.demo",`, `"7"`, "logged into 7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, live, logb := box(t)
			if err := os.MkdirAll(filepath.Join(live, "s", "EURUSD", "5"), 0o755); err != nil {
				t.Fatal(err)
			}
			_ = os.WriteFile(filepath.Join(live, "s", "EURUSD", "5", "run.py"), nil, 0o644)
			writeStatus(t, filepath.Join(logb, "s"), fmt.Sprintf(`{"state":"running","pid":%d,%s"account_id":%s}`, os.Getpid(), tc.account, tc.login))
			systems, _ := Reconcile(cfg)
			if len(systems) != 1 {
				t.Fatalf("systems = %+v", systems)
			}
			got := systems[0].AccountMismatch
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Fatalf("mismatch = %q, want containing %q", got, tc.want)
			}
		})
	}
}

// An account folder without its env file, and a run.py outside any account folder, are problems.
func TestReconcileReportsProblems(t *testing.T) {
	cfg, _, _ := box(t)
	for _, p := range []string{filepath.Join("deriv.live", "s", "X", "5"), "flat-multi"} {
		dir := filepath.Join(cfg.LiveBase, p)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(dir, "run.py"), nil, 0o644)
	}
	systems, problems := Reconcile(cfg)
	if len(systems) != 1 || systems[0].Account != "deriv.live" {
		t.Fatalf("systems = %+v", systems)
	}
	if len(problems) != 2 || problems[0].Path != "deriv.live" || problems[1].Path != "flat-multi" {
		t.Fatalf("problems = %+v", problems)
	}
}
