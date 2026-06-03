package tui

import (
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
			Systems: []ipc.System{
				{SystemID: "rsi2_mean_reversion/EURUSD/M5", State: ipc.StateRunning, PID: 48748, LastBarAgeS: 2},
				{SystemID: "hmm_neutral/GBPUSD/M15", State: ipc.StateStoppedByOp},
				{SystemID: "keras_direction/XAUUSD/H1", State: ipc.StateRunning, PID: 12044, LastBarAgeS: 312, Wedged: true},
				{SystemID: "rsi2_mean_reversion/USDJPY/M5", State: ipc.StateOrphanSuspected, PID: 5512},
			},
		},
		status: "rsi2_mean_reversion/EURUSD/M5 → stopping",
	}
	out := m.View()
	// includes the short labels for the wide states (full enum names would overflow colState)
	for _, want := range []string{"SYSTEM", "STATE", "BAR AGE", "rsi2_mean_reversion/EURUSD/M5", "WEDGED", "Stopped(op)", "Orphan?", "alerts"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered view is missing %q", want)
		}
	}
	t.Log("\n" + out)
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
