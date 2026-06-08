package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

// paneLines honors the vertical (voff) and horizontal (hoff) scroll offsets, clamping stale values.
func TestPaneLinesScroll(t *testing.T) {
	lines := []string{"line0", "line1", "line2", "line3", "line4"}

	// no offset -> live tail (last innerH lines)
	got := paneLines(logPane{lines: lines}, 80, 2)
	if len(got) != 2 || !strings.Contains(got[1], "line4") {
		t.Fatalf("live tail = %v", got)
	}
	// voff=2 -> window ends 2 from the bottom: line1,line2
	got = paneLines(logPane{lines: lines, voff: 2}, 80, 2)
	if len(got) != 2 || !strings.Contains(got[0], "line1") || !strings.Contains(got[1], "line2") {
		t.Fatalf("voff=2 = %v", got)
	}
	// voff past the top clamps to the oldest line
	got = paneLines(logPane{lines: lines, voff: 999}, 80, 2)
	if len(got) != 1 || !strings.Contains(got[0], "line0") {
		t.Fatalf("voff clamp = %v", got)
	}
	// hoff slices the left edge off (rune-safe)
	got = paneLines(logPane{lines: []string{"abcdef"}, hoff: 3}, 80, 1)
	if got[0] != "def" {
		t.Fatalf("hoff slice = %q", got[0])
	}
}

// Scroll keys drive the focused pane only; f toggles focus; G snaps back to the live tail. Routed via
// press() (handleKey -> handleDetailKey since detailOpen), so it exercises the real dispatch.
func TestDetailScrollKeys(t *testing.T) {
	m := testModel(t)
	m.detailOpen, m.detailFocus = true, 0 // focus the left (z_system_log) pane
	m.sysPane = logPane{lines: []string{"a", "b", "c", "d", "e"}}
	m.infPane = logPane{lines: []string{strings.Repeat("z", 40), "y"}} // long enough to scroll right

	press := func(s string) { tm, _ := m.handleKey(key(s)); m = tm.(model) }

	press("up") // scroll focused (sys) up
	if m.sysPane.voff != 1 || m.infPane.voff != 0 {
		t.Fatalf("up should scroll only the focused pane: sys=%d inf=%d", m.sysPane.voff, m.infPane.voff)
	}
	press("f") // toggle focus to inference
	if m.detailFocus != 1 {
		t.Fatalf("f should toggle focus, got %d", m.detailFocus)
	}
	press("right") // horizontal scroll the inference pane
	if m.infPane.hoff != 8 || m.sysPane.hoff != 0 {
		t.Fatalf("right should scroll only inf horizontally: inf=%d sys=%d", m.infPane.hoff, m.sysPane.hoff)
	}
	m.infPane.voff = 3
	press("G") // snap back to live
	if m.infPane.voff != 0 || m.infPane.hoff != 0 {
		t.Fatalf("G should reset to live: voff=%d hoff=%d", m.infPane.voff, m.infPane.hoff)
	}
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

// Start is gated too (operator preference overrides "start is additive"): s arms a confirm and only
// y submits a start for the captured system.
func TestConfirmGuardsSingleStart(t *testing.T) {
	m := testModel(t)
	m.cursor = 1 // b/Y/M5 (Stopped)

	armed, _ := m.handleKey(key("s"))
	mm := armed.(model)
	if mm.confirm == nil || mm.confirm.action != "start" || mm.confirm.systemID != "b/Y/M5" || mm.confirm.bulk {
		t.Fatalf("s should arm a single start confirm for b/Y/M5, got %+v", mm.confirm)
	}
	if cmds, _ := ipc.PendingCommands(mm.cfg.CommandsDir()); len(cmds) != 0 {
		t.Fatalf("no start may be submitted before confirmation, got %d", len(cmds))
	}
	confirmed, _ := mm.handleKey(key("y"))
	cmds, _ := ipc.PendingCommands(confirmed.(model).cfg.CommandsDir())
	if len(cmds) != 1 || cmds[0].Action != "start" || cmds[0].SystemID != "b/Y/M5" {
		t.Fatalf("confirmed start should submit one start for b/Y/M5, got %+v", cmds)
	}
}

// Start-all is gated and, on confirm, fans out one start per eligible (Stopped) system.
func TestConfirmGuardsStartAll(t *testing.T) {
	m := testModel(t)
	armed, _ := m.handleKey(key("S"))
	mm := armed.(model)
	if mm.confirm == nil || mm.confirm.action != "start" || !mm.confirm.bulk || mm.confirm.count != 1 {
		t.Fatalf("S should arm a bulk start confirm (1 Stopped target), got %+v", mm.confirm)
	}
	confirmed, _ := mm.handleKey(key("y"))
	cmds, _ := ipc.PendingCommands(confirmed.(model).cfg.CommandsDir())
	if len(cmds) != 1 || cmds[0].Action != "start" || cmds[0].SystemID != "b/Y/M5" {
		t.Fatalf("confirmed start-all should submit one start for b/Y/M5, got %+v", cmds)
	}
}

func testModel(t *testing.T) model {
	t.Helper()
	return model{
		cfg:      config.Config{StateDir: t.TempDir()},
		pending:  map[string]pendingCmd{},
		awaiting: map[string]awaitStart{},
		fleet: ipc.FleetState{Systems: []ipc.System{
			{SystemID: "a/X/M5", State: ipc.StateRunning, PID: 1},
			{SystemID: "b/Y/M5", State: ipc.StateStopped},
			{SystemID: "c/Z/M5", State: ipc.StateRunning, PID: 2},
			{SystemID: "d/W/M5", State: ipc.StateStoppedByOp},
		}},
	}
}

// D decommissions a stopped system: confirm, then its LIVE_BASE artefact is archived & removed.
func TestDecommissionGatedAndArchives(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{LiveBase: filepath.Join(dir, "live"), StateDir: filepath.Join(dir, "state")}
	sysDir := filepath.Join(cfg.LiveBase, "s", "EURUSD", "15")
	if err := os.MkdirAll(sysDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sysDir, "run.py"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	m := model{cfg: cfg, pending: map[string]pendingCmd{}, awaiting: map[string]awaitStart{},
		fleet: ipc.FleetState{Systems: []ipc.System{{SystemID: "s/EURUSD/15", State: ipc.StateStopped}}}}
	m.cursor = 0

	armed, _ := m.handleKey(key("D"))
	mm := armed.(model)
	if mm.confirm == nil || mm.confirm.action != "decommission" || mm.confirm.systemID != "s/EURUSD/15" {
		t.Fatalf("D should arm a decommission confirm, got %+v", mm.confirm)
	}
	confirmed, _ := mm.handleKey(key("y"))
	cm := confirmed.(model)
	if _, err := os.Stat(sysDir); !os.IsNotExist(err) {
		t.Errorf("artefact dir should be archived/removed after decommission")
	}
	if !strings.Contains(cm.status, "decommissioned") {
		t.Errorf("status = %q, want a decommissioned result", cm.status)
	}
}

// The import dialog ('i') must render and capture input. Regression: the dialog was unwired — 'i' set
// importState with no View case and no key handler, so it silently did nothing on screen.
func TestImportDialogWiring(t *testing.T) {
	m := model{cfg: config.Config{StateDir: t.TempDir(), LiveBase: t.TempDir()}, pending: map[string]pendingCmd{}, width: 92}
	press := func(k tea.KeyMsg) { tm, _ := m.handleKey(k); m = tm.(model) }

	press(key("i"))
	if m.importing == nil {
		t.Fatal("pressing i did not open the import dialog")
	}
	if out := m.View(); !strings.Contains(out, "import system") || !strings.Contains(out, "LIVE_BASE") {
		t.Fatalf("View() did not render the import dialog:\n%s", out)
	}

	// Keys route to the dialog, not the fleet shortcuts: 'q' types a char, it does not quit.
	press(key("q"))
	press(key("x"))
	press(key("/no/such/dir"))
	if m.importing == nil {
		t.Fatal("typing leaked to the fleet shortcuts — the dialog closed")
	}
	if got := m.importing.input; got != "qx/no/such/dir" {
		t.Fatalf("input not captured verbatim: %q", got)
	}

	// Enter validates; a bad path surfaces an inline error and keeps the dialog open.
	press(tea.KeyMsg{Type: tea.KeyEnter})
	if m.importing == nil || m.importing.plan != nil || m.importing.errText == "" {
		t.Fatalf("a bad path should set errText and stay in editing, got %+v", m.importing)
	}
	if !strings.Contains(m.View(), "✗") {
		t.Error("the validation error is not shown in the import view")
	}

	// esc cancels back to the fleet.
	press(tea.KeyMsg{Type: tea.KeyEsc})
	if m.importing != nil {
		t.Error("esc did not close the import dialog")
	}
}

// Full import flow: type a valid source dir → Enter validates it into a Plan (confirm phase renders the
// system id) → y installs it into LIVE_BASE via importsys.
func TestImportInstallsValidatedSystem(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{LiveBase: filepath.Join(dir, "live"), StateDir: filepath.Join(dir, "state")}
	src := filepath.Join(dir, "incoming", "rsi2")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "run.py"), []byte("# run"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "config.json"),
		[]byte(`{"strategy":{"name":"rsi2","symbol":"EURUSD","timeframe":5}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m := model{cfg: cfg, pending: map[string]pendingCmd{}, width: 92}
	press := func(k tea.KeyMsg) { tm, _ := m.handleKey(k); m = tm.(model) }

	press(key("i"))
	press(key(src))                       // type the source path
	press(tea.KeyMsg{Type: tea.KeyEnter}) // validate
	if m.importing == nil || m.importing.plan == nil {
		t.Fatalf("a valid source should validate into a plan, got %+v", m.importing)
	}
	if m.importing.plan.SystemID != "rsi2/EURUSD/5" {
		t.Fatalf("plan system id = %q, want rsi2/EURUSD/5", m.importing.plan.SystemID)
	}
	if out := m.View(); !strings.Contains(out, "rsi2/EURUSD/5") || !strings.Contains(out, "install") {
		t.Fatalf("confirm view should show the system id and an install hint:\n%s", out)
	}

	press(key("y")) // install
	if m.importing != nil {
		t.Error("dialog should close after a successful install")
	}
	if !strings.Contains(m.status, "imported rsi2/EURUSD/5") {
		t.Errorf("status = %q, want an imported result", m.status)
	}
	if _, err := os.Stat(filepath.Join(cfg.LiveBase, "rsi2", "EURUSD", "5", "run.py")); err != nil {
		t.Errorf("run.py should be installed under LIVE_BASE: %v", err)
	}
}

// A clipboard paste that interleaves NUL bytes (a mis-decoded UTF-16 path) must not poison the source
// path: the dialog strips control runes so filepath.Abs resolves instead of failing "invalid argument".
func TestImportStripsControlRunesFromPaste(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{LiveBase: filepath.Join(dir, "live"), StateDir: filepath.Join(dir, "state")}
	src := filepath.Join(dir, "incoming", "rsi2")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "run.py"), []byte("# run"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "config.json"),
		[]byte(`{"strategy":{"name":"rsi2","symbol":"EURUSD","timeframe":5}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// A NUL after every rune (and trailing), as a bad UTF-16 clipboard decode would deliver it.
	var pasted []rune
	for _, r := range src {
		pasted = append(pasted, r, '\x00')
	}
	m := model{cfg: cfg, importing: &importState{}, pending: map[string]pendingCmd{}, width: 92}
	press := func(k tea.KeyMsg) { tm, _ := m.handleKey(k); m = tm.(model) }

	press(tea.KeyMsg{Type: tea.KeyRunes, Runes: pasted})
	if strings.ContainsRune(m.importing.input, '\x00') {
		t.Fatalf("input still holds a NUL after paste: %q", m.importing.input)
	}
	if m.importing.input != src {
		t.Fatalf("stripped input = %q, want %q", m.importing.input, src)
	}
	press(tea.KeyMsg{Type: tea.KeyEnter}) // validate
	if m.importing == nil || m.importing.plan == nil {
		t.Fatalf("a NUL-laden paste should still validate after stripping, got %+v (status=%q)", m.importing, m.status)
	}
}

// D on a running system refuses (no confirm) and tells the operator to stop it first.
func TestDecommissionRefusedWhileRunning(t *testing.T) {
	m := testModel(t)
	m.cursor = 0 // a/X/M5 Running, PID 1
	armed, _ := m.handleKey(key("D"))
	mm := armed.(model)
	if mm.confirm != nil {
		t.Errorf("D on a running system must not arm a confirm")
	}
	if !strings.Contains(mm.status, "stop") {
		t.Errorf("status = %q, want a 'stop first' hint", mm.status)
	}
}

// A Stopped(op) system that still carries its last PID (Reconcile keeps it for display) is NOT
// active — D must arm the decommission confirm, not refuse with "stop first".
func TestDecommissionAllowedWhenStoppedWithStalePID(t *testing.T) {
	m := testModel(t)
	m.cursor = 3 // d/W/M5 StoppedByOp — give it a retained PID, as a real Stopped(op) row has
	m.fleet.Systems[3].PID = 9376
	armed, _ := m.handleKey(key("D"))
	mm := armed.(model)
	if mm.confirm == nil || mm.confirm.action != "decommission" || mm.confirm.systemID != "d/W/M5" {
		t.Fatalf("D on a Stopped(op) row with a stale PID should arm decommission, got %+v / status=%q", mm.confirm, mm.status)
	}
}

// A started system that ends up Crashed self-corrects the status away from the optimistic "starting".
func TestStartStatusSelfCorrectsToFailure(t *testing.T) {
	m := testModel(t)
	m.awaiting = map[string]awaitStart{
		"a/X/M5": {action: "start", deadline: time.Now().Add(time.Minute), sawActive: true},
	}
	m.fleet.Systems[0].State = ipc.StateCrashed
	m.reconcileAwaiting()
	if _, still := m.awaiting["a/X/M5"]; still {
		t.Error("awaiting should clear once the system settles")
	}
	if !strings.Contains(m.status, "FAILED") {
		t.Errorf("status = %q, want a FAILED outcome", m.status)
	}
}

// A started system that reaches Running self-corrects to a success line.
func TestStartStatusSelfCorrectsToRunning(t *testing.T) {
	m := testModel(t)
	m.awaiting = map[string]awaitStart{"b/Y/M5": {action: "start", deadline: time.Now().Add(time.Minute)}}
	m.fleet.Systems[1].State = ipc.StateRunning // b/Y/M5
	m.reconcileAwaiting()
	if !strings.Contains(m.status, "running") {
		t.Errorf("status = %q, want running ✓", m.status)
	}
}

// Pre-start grace: a system still Stopped (engine hasn't picked up the command), not yet seen active
// and before the deadline, must NOT be reported as a failure.
func TestStartStatusGraceBeforeActive(t *testing.T) {
	m := testModel(t)
	m.awaiting = map[string]awaitStart{"b/Y/M5": {action: "start", deadline: time.Now().Add(time.Minute)}}
	m.reconcileAwaiting() // b/Y/M5 is Stopped in testModel
	if _, still := m.awaiting["b/Y/M5"]; !still {
		t.Error("should keep waiting during the pre-start grace, not report failure")
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

// The details frame must never exceed the terminal height — even with a status line AND a pending
// confirm present (the over-height frame scrolled the chrome off: the single-details bug).
func TestDetailViewFitsTerminalHeight(t *testing.T) {
	for _, rows := range []int{18, 24, 40, 60} {
		m := testModel(t)
		m.detailOpen, m.detailID = true, "a/X/M5"
		m.width, m.height = 120, rows
		lines := make([]string, 400)
		for i := range lines {
			lines[i] = "a log line"
		}
		m.sysPane.lines, m.infPane.lines = lines, lines
		m.status = "start a/X/M5 did not start (now Stopped) some longer status text"
		m.confirm = &confirmState{action: "stop", systemID: "a/X/M5"}
		if got := len(strings.Split(m.detailView(), "\n")); got > rows {
			t.Errorf("rows=%d: detail frame is %d lines, exceeds terminal height", rows, got)
		}
	}
}

// A runner's text log can contain non-UTF-8 bytes (a cp1252 em-dash, 0x97). tailFile must sanitize
// them so raw bytes never reach the terminal and inflate the width (the single-details wide-screen bug).
func TestTailFileSanitizesInvalidUTF8(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.log")
	if err := os.WriteFile(path, []byte("Signal \x97 long=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := tailFile(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !utf8.ValidString(got[0]) {
		t.Fatalf("tailFile must return valid UTF-8, got %q", got)
	}
}
