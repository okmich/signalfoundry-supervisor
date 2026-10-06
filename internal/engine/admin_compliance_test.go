//go:build windows

package engine

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/okmich/signalfoundry-supervisor/internal/config"
	"github.com/okmich/signalfoundry-supervisor/internal/discovery"
	"github.com/okmich/signalfoundry-supervisor/internal/importsys"
	"github.com/okmich/signalfoundry-supervisor/internal/ipc"
	"github.com/okmich/signalfoundry-supervisor/internal/proc"
	"github.com/okmich/signalfoundry-supervisor/internal/state"
)

// TestAdminRunnerCompliance is ACCOUNT_ADMIN_SPEC §14.10 against the REAL Python Admin runner: the core host, tasks,
// clock, status file and heartbeat, over a fake account instead of an MT5 terminal (signalfoundry-lab
// systems/account_admin/compliance/fake_run.py). The Supervisor side is its own code: import, discovery, reconcile,
// liveness, the targeted Ctrl+C through the built supervisor binary, re-import and decommission.
//
// It needs a Python with okmich-quant-core installed, so it runs only when asked:
//
//	SF_COMPLIANCE_PYTHON=<python.exe> SF_COMPLIANCE_FAKE_ADMIN=<path to fake_run.py> go test ./internal/engine -run AdminRunner -v
func TestAdminRunnerCompliance(t *testing.T) {
	py, fake := os.Getenv("SF_COMPLIANCE_PYTHON"), os.Getenv("SF_COMPLIANCE_FAKE_ADMIN")
	if py == "" || fake == "" {
		t.Skip("set SF_COMPLIANCE_PYTHON and SF_COMPLIANCE_FAKE_ADMIN to run the Admin compliance test")
	}
	const account = "fxify.demo"
	root := t.TempDir()
	cfg := config.Config{LiveBase: filepath.Join(root, "live"), LogBase: filepath.Join(root, "log"),
		EnvDir: filepath.Join(root, "env"), StateDir: filepath.Join(root, "state"), Python: py,
		WedgeMultiple: 3, WedgeGrace: time.Minute}
	for _, d := range []string{cfg.LiveBase, cfg.LogBase, cfg.EnvDir, cfg.StateDir} {
		must(t, os.MkdirAll(d, 0o755))
	}
	must(t, os.WriteFile(filepath.Join(cfg.EnvDir, ".env."+account),
		[]byte("LOGIN_ID=42\nLOGIN_SERVER=Demo\nTERMINAL_PATH=C:\\mt5\\terminal64.exe\nBROKER_NAME=Fake\n"), 0o644))
	// The runner inherits this environment, as it inherits the engine's.
	t.Setenv("OKMICH_QUANT_LIVE_BASE", cfg.LiveBase)
	t.Setenv("OKMICH_QUANT_LOG_BASE", cfg.LogBase)
	t.Setenv("OKMICH_QUANT_ENV_DIR", cfg.EnvDir)

	// --- the artefact: run.py + config.json declaring the runner, imported like any system
	src := filepath.Join(root, "src")
	must(t, os.MkdirAll(src, 0o755))
	runPyBody, err := os.ReadFile(fake)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(src, "run.py"), runPyBody, 0o644))
	must(t, os.WriteFile(filepath.Join(src, "config.json"), []byte(adminConfig), 0o644))
	plan, err := importsys.BuildPlan(cfg, account, src)
	must(t, err)
	if _, err := plan.Apply(cfg); err != nil {
		t.Fatal(err)
	}

	// --- discovery: a runner named by its folder
	systems, problems, err := discovery.Scan(cfg.LiveBase)
	must(t, err)
	if len(problems) != 0 || len(systems) != 1 || systems[0].SystemID != account+"/_account_admin" || !systems[0].Runner {
		t.Fatalf("discovery: systems=%+v problems=%+v", systems, problems)
	}

	// --- start, exactly as the engine launches a system
	e := testEngine(t)
	e.cfg = cfg
	pid, console, err := e.launch(systems[0])
	must(t, err)
	t.Cleanup(func() { _ = proc.Kill(pid) })
	row := waitFor(t, cfg, 90*time.Second, func(s ipc.System) bool { return s.State == ipc.StateRunning && !s.LastBarTS.IsZero() }, console)

	if row.AccountMismatch != "" {
		t.Errorf("account check: %s", row.AccountMismatch)
	}
	if len(row.Legs) != 1 || row.Legs[0].Symbol != "account" || row.Legs[0].Timeframe != "1" {
		t.Errorf("legs = %+v, want one leg account/1", row.Legs)
	}
	if age := time.Since(row.LastBarTS); age > 3*time.Minute {
		t.Errorf("heartbeat age %s: the Supervisor would soon see a live Admin as wedged", age)
	}
	admin := filepath.Join(cfg.LiveBase, account, "_account_admin")
	for _, name := range []string{"directive.json", "state.json", "pending_order_cleanup.json"} {
		if _, err := os.Stat(filepath.Join(admin, name)); err != nil {
			t.Errorf("the Admin did not publish %s: %v", name, err)
		}
	}

	// --- liveness: fresh now; wedged once its heartbeat is older than 3 x 1 min + 60 s
	set := ipc.Settings{WedgeAlert: ipc.WedgeAlertAlways, WedgeMultiple: 3, WedgeGraceS: 60}
	rows := []ipc.System{row}
	e.checkLiveness(rows, time.Now(), set)
	if rows[0].Wedged {
		t.Errorf("a live Admin was judged wedged")
	}
	e.checkLiveness(rows, row.LastBarTS.Add(5*time.Minute), set)
	if !rows[0].Wedged {
		t.Errorf("an Admin whose heartbeat stopped 5 minutes ago was not judged wedged")
	}

	// --- stop: the targeted Ctrl+C through the real supervisor binary; the Admin finishes its cycle and proves it
	stopped := ctrlC(t, row.PID)
	if !stopped {
		logDir := filepath.Join(cfg.LogBase, account, "_account_admin")
		var dump strings.Builder
		entries, _ := os.ReadDir(logDir)
		for _, en := range entries {
			if strings.HasPrefix(en.Name(), "z_") || en.Name() == "status.json" {
				b, _ := os.ReadFile(filepath.Join(logDir, en.Name()))
				if len(b) > 3000 {
					b = b[len(b)-3000:]
				}
				dump.WriteString("== " + en.Name() + "\n" + string(b) + "\n")
			}
		}
		t.Fatalf("the Admin (status PID %d, launched PID %d) did not stop on Ctrl+C\n%s", row.PID, pid, dump.String())
	}
	row = waitFor(t, cfg, 30*time.Second, func(s ipc.System) bool { return s.State == ipc.StateStoppedByOp }, console)
	var status struct {
		State              string `json:"state"`
		Clean              bool   `json:"clean"`
		BrokerDisconnected bool   `json:"broker_disconnected"`
		Account            string `json:"account"`
	}
	b, err := os.ReadFile(filepath.Join(cfg.LogBase, account, "_account_admin", "status.json"))
	must(t, err)
	must(t, json.Unmarshal(b, &status))
	if status.State != "stopped" || !status.Clean || !status.BrokerDisconnected || status.Account != account {
		t.Errorf("stopped status = %+v", status)
	}

	// --- re-import keeps the governance files byte-identical; decommission is refused
	before := map[string][]byte{}
	for _, name := range []string{"directive.json", "state.json"} {
		before[name], err = os.ReadFile(filepath.Join(admin, name))
		must(t, err)
	}
	plan, err = importsys.BuildPlan(cfg, account, src)
	must(t, err)
	if _, err := plan.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	for name, want := range before {
		got, err := os.ReadFile(filepath.Join(admin, name))
		must(t, err)
		if !bytes.Equal(got, want) {
			t.Errorf("re-import changed %s", name)
		}
	}
	if _, err := importsys.Decommission(cfg, account+"/_account_admin"); err == nil || !strings.Contains(err.Error(), "governance") {
		t.Errorf("decommission of the Admin must be refused, got %v", err)
	}
}

const adminConfig = `{
  "kind": "account_admin", "runner": "_account_admin",
  "clock": {"cycle_s": 5, "jitter_s": 1, "valid_for_s": 60, "blackout": [{"every_s": 60, "offset_s": 0, "length_s": 1}]},
  "requests": {"request_ttl_s": 900},
  "tasks": [
    {"kind": "prop_guard", "checks": {"close_grace_s": 120, "obey_grace_s": 30}, "max_override_s": 3600,
     "policy": {"initial_capital": 100000, "account_start_utc": "2026-09-01T00:00:00Z", "day_tz": "America/New_York",
                "day_start_hour": 17, "daily_base": "day_start_balance", "daily_limit_reference": "initial_capital",
                "daily_loss_pct": 5, "daily_warn_fraction": 0.6, "max_loss_pct": 10, "max_loss_mode": "static",
                "max_loss_trail_locks_at_initial": false, "max_warn_fraction": 0.8, "profit_target_pct": null,
                "target_measure": "equity",
                "conditions": {"daily_loss": {"directive": "NO_OPS", "latch": "trading_day"},
                               "daily_warn": {"directive": "NO_ENTRY_OPS", "latch": "none"},
                               "max_loss": {"directive": "NO_OPS", "latch": "manual"},
                               "max_warn": {"directive": "NO_ENTRY_OPS", "latch": "none"}},
                "calendar": []}},
    {"kind": "pending_order_cleanup", "max_age_s": 3600}
  ]
}`

// waitFor polls the Supervisor's own reconcile until the Admin's row satisfies ok.
func waitFor(t *testing.T, cfg config.Config, within time.Duration, ok func(ipc.System) bool, console string) ipc.System {
	t.Helper()
	deadline := time.Now().Add(within)
	var last ipc.System
	for time.Now().Before(deadline) {
		systems, _ := state.Reconcile(cfg)
		for _, s := range systems {
			if s.SystemID == "fxify.demo/_account_admin" {
				last = s
				if ok(s) {
					return s
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	out, _ := os.ReadFile(console)
	t.Fatalf("timed out; last row %+v\nconsole:\n%s", last, out)
	return last
}

// ctrlC stops pid the way the engine does: the supervisor binary's `ctrlc` helper in its own console.
func ctrlC(t *testing.T, pid int) bool {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	module := filepath.Clean(filepath.Join(filepath.Dir(here), "..", ".."))
	exe := filepath.Join(t.TempDir(), "supervisor.exe")
	build := exec.Command("go", "build", "-o", exe, "./cmd/supervisor")
	build.Dir = module
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the supervisor: %v\n%s", err, out)
	}
	cmd := exec.Command(exe, "ctrlc", "--pid", strconv.Itoa(pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ctrlc helper: %v\n%s", err, out)
	}
	for i := 0; i < 60; i++ {
		if !proc.Alive(pid) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
