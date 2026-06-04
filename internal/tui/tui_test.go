package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/okmich/signalfoundry-supervisor/internal/config"
	"github.com/okmich/signalfoundry-supervisor/internal/ipc"
)

func TestViewRendersFleet(t *testing.T) {
	m := model{
		cfg:     config.Config{StateDir: t.TempDir()},
		pending: map[string]pendingCmd{},
		width:   92,
		fleet: ipc.FleetState{
			Engine:    ipc.EngineInfo{PID: 31576, Alerts: true},
			UpdatedAt: time.Date(2026, 6, 3, 18, 42, 39, 0, time.UTC),
			Terminals: []ipc.Terminal{
				{BrokerSessionID: "mt5-deriv-1", Account: "DEMO-123", LogicalSystems: 2,
					SystemIDs: []string{"rsi2_mean_reversion/EURUSD/M5", "keras_direction/XAUUSD/H1"}},
			},
			// systems arrive ordered by terminal (engine sorts); idle/non-running last
			Systems: []ipc.System{
				{SystemID: "rsi2_mean_reversion/EURUSD/M5", State: ipc.StateRunning, PID: 48748, LastBarAgeS: 2, SessionID: "mt5-deriv-1", Account: "DEMO-123"},
				{SystemID: "keras_direction/XAUUSD/H1", State: ipc.StateRunning, PID: 12044, LastBarAgeS: 312, Wedged: true, SessionID: "mt5-deriv-1", Account: "DEMO-123"},
				{SystemID: "hmm_neutral/GBPUSD/M15", State: ipc.StateStoppedByOp},
				{SystemID: "rsi2_mean_reversion/USDJPY/M5", State: ipc.StateOrphanSuspected, PID: 5512},
			},
		},
		status: "rsi2_mean_reversion/EURUSD/M5 → stopping",
	}
	out := m.View()
	for _, want := range []string{
		"SYSTEM", "STATE", "BAR AGE",
		"terminal mt5-deriv-1", "DEMO-123", "2/10 systems", "not running", // blast-radius grouping
		"rsi2_mean_reversion/EURUSD/M5", "WEDGED", "Stopped(op)", "Orphan?", "alerts",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered view is missing %q", want)
		}
	}
	t.Log("\n" + out)
}

func TestTailFileLastLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inference_x.jsonl")
	if err := os.WriteFile(path, []byte("a\nb\n\nc\nd\n"), 0o644); err != nil { // blank line ignored
		t.Fatal(err)
	}
	got, err := tailFile(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "c" || got[1] != "d" {
		t.Errorf("tailFile last-2 = %v, want [c d]", got)
	}
	if lines, err := tailFile(filepath.Join(dir, "nope.jsonl"), 5); err != nil || lines != nil {
		t.Errorf("missing file = (%v,%v), want (nil,nil) — a not-yet-written day is not an error", lines, err)
	}
}

// The Glance screen resolves a single-trader's inference dir to today's JSONL and renders its tail.
func TestLogViewTailsSelectedSystem(t *testing.T) {
	root := t.TempDir()
	day := time.Now().UTC().Format("20060102")
	infDir := filepath.Join(root, "rsi2", "EURUSD", "5", "inference")
	if err := os.MkdirAll(infDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bar := `{"event":"bar","asof_bar_ts":"2026-06-04T12:00:00Z","close":1.2345}`
	if err := os.WriteFile(filepath.Join(infDir, "inference_"+day+".jsonl"), []byte(bar+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := model{
		cfg: config.Config{StateDir: t.TempDir()}, pending: map[string]pendingCmd{},
		width: 120, height: 30,
		fleet: ipc.FleetState{Systems: []ipc.System{
			{SystemID: "rsi2/EURUSD/M5", State: ipc.StateRunning, PID: 1, LogPaths: ipc.LogPaths{Inference: infDir}},
		}},
	}
	m.cursor = 0
	m.openLog()
	if !m.logOpen {
		t.Fatal("openLog should open the Glance screen for a system with an inference dir")
	}
	if m.logDir != infDir {
		t.Fatalf("logDir = %q, want the inference dir %q", m.logDir, infDir)
	}
	// Resolve + tail today's file the way a tick read does (the file is resolved per read, not at open).
	path := filepath.Join(m.logDir, "inference_"+day+".jsonl")
	lines, err := tailFile(path, logTailMax)
	if err != nil {
		t.Fatal(err)
	}
	m.logPath, m.logLines = path, lines
	out := m.logView()
	for _, want := range []string{"rsi2/EURUSD/M5", "asof_bar_ts", "1.2345", "live tail"} {
		if !strings.Contains(out, want) {
			t.Errorf("log view missing %q\n%s", want, out)
		}
	}
}

// For a multi-trader the Glance screen watches the STALEST leg (usually the wedged symbol).
func TestOpenLogPicksStalestMultiLeg(t *testing.T) {
	now := time.Now().UTC()
	m := model{
		pending: map[string]pendingCmd{},
		fleet: ipc.FleetState{Systems: []ipc.System{{
			SystemID: "basket-multi", State: ipc.StateRunning, PID: 7, Multi: true,
			Legs: []ipc.SystemLeg{
				{Symbol: "EURUSD", Inference: filepath.Join("log", "EURUSD"), LastBarTS: now.Add(-2 * time.Minute)},
				{Symbol: "GBPUSD", Inference: filepath.Join("log", "GBPUSD"), LastBarTS: now.Add(-1 * time.Hour)},
			},
		}}},
	}
	m.cursor = 0
	m.openLog()
	if !m.logOpen {
		t.Fatal("openLog should open for a multi-trader")
	}
	if !strings.Contains(m.logTitle, "GBPUSD") {
		t.Errorf("multi Glance should target the stalest leg GBPUSD, title=%q", m.logTitle)
	}
	if !strings.Contains(m.logDir, "GBPUSD") {
		t.Errorf("logDir should point at the stalest leg dir, got %q", m.logDir)
	}
}

func testModel(t *testing.T) model {
	t.Helper()
	return model{
		cfg:     config.Config{StateDir: t.TempDir()},
		pending: map[string]pendingCmd{},
		fleet: ipc.FleetState{Systems: []ipc.System{
			{SystemID: "a/X/M5", State: ipc.StateRunning, PID: 1},
			{SystemID: "b/Y/M5", State: ipc.StateStopped},
			{SystemID: "c/Z/M5", State: ipc.StateRunning, PID: 2},
			{SystemID: "d/W/M5", State: ipc.StateStoppedByOp},
		}},
	}
}

func TestBulkTargets(t *testing.T) {
	m := testModel(t)
	if got := m.bulkTargets("stop"); len(got) != 2 {
		t.Errorf("stop targets = %v, want the 2 Running systems", got)
	}
	if got := m.bulkTargets("restart"); len(got) != 2 {
		t.Errorf("restart targets = %v, want the 2 Running systems", got)
	}
	got := m.bulkTargets("start")
	if len(got) != 1 || got[0] != "b/Y/M5" {
		t.Errorf("start targets = %v, want only the Stopped system [b/Y/M5] (not StoppedByOperator)", got)
	}
}

func TestSubmitBulkStopFansOut(t *testing.T) {
	m := testModel(t)
	m.submitBulk("stop")
	cmds, err := ipc.PendingCommands(m.cfg.CommandsDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 2 {
		t.Fatalf("wrote %d command files, want 2 (one per Running system)", len(cmds))
	}
	for _, c := range cmds {
		if c.Action != "stop" {
			t.Errorf("action = %q, want stop", c.Action)
		}
		if c.SystemID != "a/X/M5" && c.SystemID != "c/Z/M5" {
			t.Errorf("unexpected stop target %q", c.SystemID)
		}
	}
	if len(m.pending) != 2 {
		t.Errorf("pending tracked = %d, want 2", len(m.pending))
	}
}

func TestSubmitOneTargetsCursor(t *testing.T) {
	m := testModel(t)
	m.cursor = 1 // b/Y/M5
	m.submitOne("start")
	cmds, _ := ipc.PendingCommands(m.cfg.CommandsDir())
	if len(cmds) != 1 || cmds[0].Action != "start" || cmds[0].SystemID != "b/Y/M5" {
		t.Fatalf("got %+v, want a single start for b/Y/M5", cmds)
	}
}

func TestSettingsScreenEdits(t *testing.T) {
	m := testModel(t)
	m.openSettings()
	if !m.settingsOpen {
		t.Fatal("settings screen should be open")
	}
	if m.settings.WedgeAlert != ipc.WedgeAlertWeekend {
		t.Fatalf("seeded mode = %q, want weekend default", m.settings.WedgeAlert)
	}

	m.setCursor = 0 // cycle mode: weekend -> surface_only
	m.settingsAdjust(1)
	if m.settings.WedgeAlert != ipc.WedgeAlertSurface {
		t.Errorf("mode after +1 = %q, want surface_only", m.settings.WedgeAlert)
	}
	m.setCursor = 1 // multiple 3 -> 4
	m.settingsAdjust(1)
	m.setCursor = 2 // grace 60 -> 45 (step 15)
	m.settingsAdjust(-1)

	got, err := ipc.ReadSettings(m.cfg.SettingsPath())
	if err != nil {
		t.Fatalf("settings not persisted: %v", err)
	}
	if got.WedgeAlert != ipc.WedgeAlertSurface || got.WedgeMultiple != 4 || got.WedgeGraceS != 45 {
		t.Errorf("persisted = %+v, want {surface_only 4 45}", got)
	}
}

func TestResolvePendingSurfacesResult(t *testing.T) {
	m := testModel(t)
	m.cursor = 0
	m.submitOne("stop")
	var id string
	for k := range m.pending {
		id = k
	}
	if err := ipc.WriteResult(m.cfg.CommandsDir(), ipc.CommandResult{ID: id, Accepted: true, Outcome: "stopping"}); err != nil {
		t.Fatal(err)
	}
	m.resolvePending()
	if len(m.pending) != 0 {
		t.Errorf("pending not cleared after result: %v", m.pending)
	}
	if !strings.Contains(m.status, "stopping") {
		t.Errorf("status = %q, want it to surface the 'stopping' outcome", m.status)
	}
}
