package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/okmich/signalfoundry-supervisor/internal/config"
	"github.com/okmich/signalfoundry-supervisor/internal/ipc"
)

// key builds a rune KeyMsg ("x", "y", …) for driving handleKey in tests.
func key(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

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

// The details screen resolves a single-trader's inference dir to today's JSONL, renders its tail in
// the inference pane, and shows the status panel + both pane titles.
func TestDetailViewTailsSelectedSystem(t *testing.T) {
	root := t.TempDir()
	day := time.Now().UTC().Format("20060102")
	infDir := filepath.Join(root, "rsi2", "EURUSD", "5", "inference")
	if err := os.MkdirAll(infDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bar := `{"event":"bar","close":1.2345}` // short so it isn't truncated in a half-width pane
	if err := os.WriteFile(filepath.Join(infDir, "inference_"+day+".jsonl"), []byte(bar+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := model{
		cfg: config.Config{StateDir: t.TempDir()}, pending: map[string]pendingCmd{},
		width: 160, height: 30,
		fleet: ipc.FleetState{Systems: []ipc.System{
			{SystemID: "rsi2/EURUSD/M5", Symbol: "EURUSD", State: ipc.StateRunning, PID: 4242,
				LogPaths: ipc.LogPaths{Inference: infDir, Text: filepath.Join(root, "z_system_log.log")}},
		}},
	}
	m.cursor = 0
	m.openDetail()
	if !m.detailOpen {
		t.Fatal("openDetail should open the details screen")
	}
	if m.infPane.key != infDir {
		t.Fatalf("infPane.key = %q, want the inference dir %q", m.infPane.key, infDir)
	}
	// Tail today's file the way a tick read does (the file is resolved per read, not at open).
	lines, err := tailFile(filepath.Join(infDir, "inference_"+day+".jsonl"), logTailMax)
	if err != nil {
		t.Fatal(err)
	}
	m.infPane.lines = lines
	out := m.detailView()
	for _, want := range []string{"rsi2/EURUSD/M5", "1.2345", "inference", "z_system_log", "PID", "4242", "EURUSD"} {
		if !strings.Contains(out, want) {
			t.Errorf("details view missing %q\n%s", want, out)
		}
	}
	t.Log("\n" + out)
}

// For a multi-trader the details screen defaults to the STALEST leg's tab; tab cycles to the next.
func TestOpenDetailPicksStalestTab(t *testing.T) {
	now := time.Now().UTC()
	m := model{
		pending: map[string]pendingCmd{},
		fleet: ipc.FleetState{Systems: []ipc.System{{
			SystemID: "basket-multi", State: ipc.StateRunning, PID: 7, Multi: true,
			LogPaths: ipc.LogPaths{Text: filepath.Join("log", "z_system_log.log")},
			Legs: []ipc.SystemLeg{
				{Symbol: "EURUSD", Inference: filepath.Join("log", "EURUSD"), LastBarTS: now.Add(-2 * time.Minute)},
				{Symbol: "GBPUSD", Inference: filepath.Join("log", "GBPUSD"), LastBarTS: now.Add(-1 * time.Hour)},
			},
		}}},
	}
	m.cursor = 0
	m.openDetail()
	if !m.detailOpen {
		t.Fatal("openDetail should open for a multi-trader")
	}
	tabs := detailTabs(m.fleet.Systems[0])
	if tabs[m.detailTab].symbol != "GBPUSD" {
		t.Errorf("default tab should be the stalest leg GBPUSD, got %q", tabs[m.detailTab].symbol)
	}
	if !strings.Contains(m.infPane.key, "GBPUSD") {
		t.Errorf("infPane should target GBPUSD's dir, got %q", m.infPane.key)
	}
	// tab cycles to the next symbol (wraps).
	next, _ := m.cycleTab(1)
	mm := next.(model)
	if tabs[mm.detailTab].symbol != "EURUSD" {
		t.Errorf("cycleTab should advance to EURUSD, got %q", tabs[mm.detailTab].symbol)
	}

	// eyeball the multi-trader details render (wedged leg, tabs, dual panes)
	m.width, m.height = 150, 28
	m.fleet.Systems[0].Wedged = true
	m.fleet.Systems[0].Legs[1].Wedged = true
	m.fleet.Systems[0].StartedAt = time.Date(2026, 6, 4, 9, 0, 0, 0, time.UTC)
	m.fleet.Systems[0].Broker, m.fleet.Systems[0].Account, m.fleet.Systems[0].SessionID = "deriv", "DEMO-123", "mt5-deriv-1"
	m.infPane.lines = []string{`{"event":"breaker","reason":"stale_feed"}`, `{"event":"bar","close":1.2701}`}
	m.sysPane.lines = []string{"12:00:01 INFO  bar GBPUSD 1.2701", "12:05:02 WARN  broker latency 800ms"}
	t.Log("\n" + m.detailView())
}

func TestHealthBadge(t *testing.T) {
	if healthBadge("unknown") != "" || healthBadge("") != "" {
		t.Errorf("unknown/unprobed health should render no badge")
	}
	if !strings.Contains(healthBadge("green"), "✓") {
		t.Errorf("green badge should show a check")
	}
	if !strings.Contains(healthBadge("red"), "DOWN") {
		t.Errorf("red badge should show DOWN")
	}
}

// A red broker session surfaces in the fleet view's terminal subheader (§13).
func TestFleetViewShowsSessionHealth(t *testing.T) {
	m := model{
		cfg: config.Config{StateDir: t.TempDir()}, pending: map[string]pendingCmd{}, width: 100,
		fleet: ipc.FleetState{
			Terminals: []ipc.Terminal{{BrokerSessionID: "mt5-deriv-1", Account: "DEMO-1", LogicalSystems: 1, Health: "red"}},
			Systems: []ipc.System{
				{SystemID: "rsi2/EURUSD/M5", State: ipc.StateRunning, PID: 1, SessionID: "mt5-deriv-1", Account: "DEMO-1"},
			},
		},
	}
	if out := m.fleetView(); !strings.Contains(out, "DOWN") {
		t.Errorf("fleet view should flag a red session as DOWN\n%s", out)
	}
}

// A single stop must be confirmed: pressing x arms a confirm (no command), an errant key cancels it,
// and only y actually submits.
func TestConfirmGuardsSingleStop(t *testing.T) {
	m := testModel(t)
	m.cursor = 0 // a/X/M5 (Running)

	armed, _ := m.handleKey(key("x"))
	mm := armed.(model)
	if mm.confirm == nil || mm.confirm.action != "stop" || mm.confirm.systemID != "a/X/M5" || mm.confirm.bulk {
		t.Fatalf("x should arm a single stop confirm for a/X/M5, got %+v", mm.confirm)
	}
	if cmds, _ := ipc.PendingCommands(mm.cfg.CommandsDir()); len(cmds) != 0 {
		t.Fatalf("no command may be submitted before confirmation, got %d", len(cmds))
	}

	// An errant key cancels — still nothing submitted.
	cancelled, _ := mm.handleKey(key("z"))
	cm := cancelled.(model)
	if cm.confirm != nil {
		t.Errorf("a non-y key should cancel the confirm")
	}
	if cmds, _ := ipc.PendingCommands(cm.cfg.CommandsDir()); len(cmds) != 0 {
		t.Fatalf("a cancelled confirm must not submit, got %d", len(cmds))
	}

	// Re-arm and confirm with y -> exactly one stop for the captured system.
	armed2, _ := cm.handleKey(key("x"))
	confirmed, _ := armed2.(model).handleKey(key("y"))
	cmds, _ := ipc.PendingCommands(confirmed.(model).cfg.CommandsDir())
	if len(cmds) != 1 || cmds[0].Action != "stop" || cmds[0].SystemID != "a/X/M5" {
		t.Fatalf("confirmed stop should submit one stop for a/X/M5, got %+v", cmds)
	}
}

// Force-kill (shift-K) is confirmed like the others and submits a "kill" command on y.
func TestConfirmGuardsKill(t *testing.T) {
	m := testModel(t)
	m.cursor = 0 // a/X/M5

	armed, _ := m.handleKey(key("K"))
	mm := armed.(model)
	if mm.confirm == nil || mm.confirm.action != "kill" || mm.confirm.systemID != "a/X/M5" {
		t.Fatalf("K should arm a kill confirm for a/X/M5, got %+v", mm.confirm)
	}
	if cmds, _ := ipc.PendingCommands(mm.cfg.CommandsDir()); len(cmds) != 0 {
		t.Fatalf("no kill may be submitted before confirmation")
	}
	confirmed, _ := mm.handleKey(key("y"))
	cmds, _ := ipc.PendingCommands(confirmed.(model).cfg.CommandsDir())
	if len(cmds) != 1 || cmds[0].Action != "kill" || cmds[0].SystemID != "a/X/M5" {
		t.Fatalf("confirmed kill should submit one kill for a/X/M5, got %+v", cmds)
	}
}

// Quitting is confirmed: q arms (no quit), only y returns the quit command.
func TestConfirmGuardsQuit(t *testing.T) {
	m := testModel(t)

	armed, cmd := m.handleKey(key("q"))
	mm := armed.(model)
	if mm.confirm == nil || mm.confirm.action != "quit" {
		t.Fatalf("q should arm a quit confirm, got %+v", mm.confirm)
	}
	if cmd != nil {
		t.Errorf("arming quit must not quit immediately")
	}

	// An errant key cancels (does not quit).
	if _, c := mm.handleKey(key("z")); c != nil {
		t.Errorf("a non-y key should cancel, not quit")
	}

	// y confirms -> tea.Quit.
	_, qcmd := mm.handleKey(key("y"))
	if qcmd == nil {
		t.Fatalf("confirming quit should return a command")
	}
	if _, ok := qcmd().(tea.QuitMsg); !ok {
		t.Errorf("confirmed quit should be tea.Quit")
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
