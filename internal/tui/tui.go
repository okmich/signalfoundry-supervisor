// Package tui is the on-demand Bubble Tea face: it polls fleet_state.json and renders the fleet;
// keys issue start/stop/restart by dropping command files. Closing it never touches the engine.
package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/okmich/signalfoundry-supervisor/internal/config"
	"github.com/okmich/signalfoundry-supervisor/internal/importsys"
	"github.com/okmich/signalfoundry-supervisor/internal/ipc"
)

// Column widths for the fleet table (visible cells; lipgloss pads around color codes).
const (
	colSystem = 38
	colState  = 12
	colPID    = 8
	colAge    = 8
)

// terminalCap is the ≤10 logical-systems-per-broker-terminal blast-radius cap (§7).
const terminalCap = 10

var (
	titleStyle  = lipgloss.NewStyle().Bold(true)
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	headerStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Bold(true)
	cursorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	selStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Bold(true)
	wedgeStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Bold(true)
	alertStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
)

// stateLabel is the short display badge for a state — the full enum names (StoppedByOperator,
// OrphanSuspected, CrashLoopHalted) are wider than colState and would break column alignment.
func stateLabel(st ipc.State) string {
	switch st {
	case ipc.StateStoppedByOp:
		return "Stopped(op)"
	case ipc.StateOrphanSuspected:
		return "Orphan?"
	case ipc.StateCrashLoopHalted:
		return "CrashLoop"
	default:
		return string(st)
	}
}

// stateStyle colors a lifecycle state by severity: green running, amber in-flight, red fault, dim idle.
func stateStyle(st ipc.State) lipgloss.Style {
	switch st {
	case ipc.StateRunning:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	case ipc.StateStarting, ipc.StateStopping, ipc.StateRestarting:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	case ipc.StateCrashed, ipc.StateOrphanSuspected, ipc.StateCrashLoopHalted:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	case ipc.StateStoppedByOp:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	default: // Stopped / unknown
		return dimStyle
	}
}

// Run starts the TUI program.
func Run(cfg config.Config) error {
	m := model{cfg: cfg, pending: map[string]pendingCmd{}}
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

type model struct {
	cfg     config.Config
	fleet   ipc.FleetState
	err     error
	cursor  int                   // selected fleet row
	confirm   *confirmState         // non-nil while awaiting y/n for a bulk stop
	importing *importState          // non-nil while the import dialog is open
	pending   map[string]pendingCmd // submitted command id -> what it was, to surface its result
	status    string                // latest action / result line

	settingsOpen bool         // settings screen is showing
	settings     ipc.Settings // editable copy (loaded on open, written on each change)
	setCursor    int          // selected settings row (0=mode, 1=multiple, 2=grace)

	width int // terminal width (from WindowSizeMsg), for right-aligning the title bar
}

// confirmState gates a fleet-wide stop behind an explicit y/n (§11.1: a fleet stop is only an
// explicit operator command, never a single keystroke).
type confirmState struct {
	action string // "stop" | "restart"
	count  int
}

type pendingCmd struct {
	action   string
	systemID string
}

// importState drives the system-import dialog: the operator types a source folder, Enter validates it
// into a Plan (import mode is purely a TUI client of importsys), then a y/n confirm gates the
// archive+install. Closing the TUI mid-flow is safe — the install is a staged atomic rename.
type importState struct {
	input   string          // source path being typed (editing phase)
	plan    *importsys.Plan // non-nil once validated -> confirm phase
	errText string          // last validation error, shown inline
}

type tickMsg time.Time
type errMsg struct{ err error }

func (m model) Init() tea.Cmd { return tea.Batch(poll(m.cfg), tick()) }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case ipc.FleetState:
		m.fleet, m.err = msg, nil
		m.clampCursor()
		return m, nil
	case errMsg:
		m.err = msg.err
		return m, nil
	case tickMsg:
		m.resolvePending()
		return m, tea.Batch(poll(m.cfg), tick())
	}
	return m, nil
}

func (m model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.settingsOpen {
		return m.handleSettingsKey(k)
	}
	// While a bulk stop is pending confirmation, only y/Y proceeds; anything else cancels.
	if m.confirm != nil {
		if s := k.String(); s == "y" || s == "Y" {
			m.submitBulk(m.confirm.action)
		} else {
			m.status = "cancelled"
		}
		m.confirm = nil
		return m, nil
	}
	if m.importing != nil {
		return m.handleImportKey(k)
	}
	switch k.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.fleet.Systems)-1 {
			m.cursor++
		}
	case "s":
		m.submitOne("start")
	case "x":
		m.submitOne("stop")
	case "r":
		m.submitOne("restart")
	case "S": // start-all — no confirm (additive; §11.1)
		m.submitBulk("start")
	case "X": // stop-all — confirm first
		m.armBulkConfirm("stop")
	case "R": // restart-all — confirm first (it is a fleet-wide stop too)
		m.armBulkConfirm("restart")
	case "i": // open the import-system dialog
		m.importing = &importState{}
		m.status = ""
	case "c": // open the settings screen
		m.openSettings()
	}
	return m, nil
}

// handleImportKey drives the import dialog. Editing phase: type/paste the source folder, Enter
// validates it into a Plan, esc cancels. Confirm phase (plan built): y installs, any other key returns
// to editing. The install is delegated to importsys; the engine re-discovers the new system next tick.
func (m model) handleImportKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	im := m.importing
	if im.plan != nil { // confirm phase
		switch k.String() {
		case "y", "Y":
			archived, err := im.plan.Apply(m.cfg)
			switch {
			case err != nil:
				m.status = "import failed: " + err.Error()
			case archived != "":
				m.status = fmt.Sprintf("imported %s (previous copy archived)", im.plan.SystemID)
			default:
				m.status = "imported " + im.plan.SystemID
			}
			m.importing = nil
		case "ctrl+c":
			return m, tea.Quit
		default: // back to editing
			im.plan = nil
		}
		return m, nil
	}
	switch k.Type { // editing phase
	case tea.KeyEsc:
		m.importing = nil
		m.status = "import cancelled"
	case tea.KeyCtrlC:
		return m, tea.Quit
	case tea.KeyEnter:
		if plan, err := importsys.BuildPlan(m.cfg, im.input); err != nil {
			im.errText = err.Error()
		} else {
			im.errText, im.plan = "", &plan
		}
	case tea.KeyBackspace:
		if r := []rune(im.input); len(r) > 0 {
			im.input = string(r[:len(r)-1])
		}
	case tea.KeySpace:
		im.input += " "
	case tea.KeyRunes:
		im.input += string(k.Runes)
	}
	return m, nil
}

// handleSettingsKey drives the settings screen: navigate rows, change the selected value (which
// auto-writes settings.json so the engine applies it next tick), esc/c/q to close.
func (m model) handleSettingsKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc", "c", "q":
		m.settingsOpen = false
	case "up", "k":
		if m.setCursor > 0 {
			m.setCursor--
		}
	case "down", "j":
		if m.setCursor < 2 {
			m.setCursor++
		}
	case "left", "h", "-":
		m.settingsAdjust(-1)
	case "right", "l", "+", "enter", " ":
		m.settingsAdjust(1)
	}
	return m, nil
}

func (m model) View() string {
	if m.settingsOpen {
		return m.settingsView()
	}
	if m.importing != nil {
		return m.importView()
	}
	lines := []string{m.titleBar("fleet"), ""}
	if m.err != nil {
		lines = append(lines, alertStyle.Render("engine offline? ")+dimStyle.Render(m.err.Error()), "")
	}
	lines = append(lines, "  "+
		headerStyle.Width(colSystem).Render("SYSTEM")+
		headerStyle.Width(colState).Render("STATE")+
		headerStyle.Width(colPID).Render("PID")+
		headerStyle.Width(colAge).Render("BAR AGE"))
	if len(m.fleet.Systems) == 0 {
		lines = append(lines, dimStyle.Render("  (no systems)"))
	}
	terms := make(map[string]ipc.Terminal, len(m.fleet.Terminals))
	for _, t := range m.fleet.Terminals {
		terms[t.BrokerSessionID] = t
	}
	prevSession := "\x00" // sentinel before any real session
	for i, s := range m.fleet.Systems {
		if s.SessionID != prevSession { // start of a terminal group -> subheader
			prevSession = s.SessionID
			lines = append(lines, m.terminalHeader(s.SessionID, terms))
		}
		lines = append(lines, m.systemRow(s, i == m.cursor))
	}
	lines = append(lines, "")
	if m.confirm != nil {
		lines = append(lines, alertStyle.Render(fmt.Sprintf("%s ALL %d running system(s)?  [y] confirm   [any other key] cancel",
			strings.ToUpper(m.confirm.action), m.confirm.count)))
	}
	if m.status != "" {
		lines = append(lines, m.statusStyle().Render(m.status))
	}
	lines = append(lines, "", dimStyle.Render("↑/↓ select · s start · x stop · r restart · S/X/R all · i import · c config · q quit"))
	return strings.Join(lines, "\n") + "\n"
}

// importView renders the import dialog: the path editor, or — once a plan is built — the resolved
// classification/target for a y/n confirm.
func (m model) importView() string {
	im := m.importing
	lines := []string{m.titleBar("import system"), ""}
	if im.plan == nil { // editing phase
		lines = append(lines,
			dimStyle.Render("  Source folder to import (must contain run.py + config.json):"),
			"  "+selStyle.Render(im.input)+cursorStyle.Render("▌"))
		if im.errText != "" {
			lines = append(lines, "", alertStyle.Render("  ✗ "+im.errText))
		}
		lines = append(lines, "", dimStyle.Render("  enter validate · esc cancel"))
		return strings.Join(lines, "\n") + "\n"
	}
	p := im.plan
	kind, detail := "single-trader", fmt.Sprintf("%s / %s / %d", p.Strategy, p.Symbol, p.Timeframe)
	if p.Multi {
		kind = fmt.Sprintf("multi-trader · %d symbols", len(p.Symbols))
		detail = strings.Join(p.Symbols, ", ")
	}
	lines = append(lines,
		"  "+okStyle.Render("✓ valid ")+dimStyle.Render(kind),
		dimStyle.Render("  system  ")+selStyle.Render(p.SystemID),
		dimStyle.Render("  detail  ")+detail,
		dimStyle.Render("  source  ")+p.SourceDir,
		dimStyle.Render("  target  ")+p.TargetDir,
	)
	if p.WillArchive {
		lines = append(lines, "", wedgeStyle.Render("  ⚠ target exists — the current copy is archived first"))
	}
	lines = append(lines, "", alertStyle.Render("  install here?  [y] confirm   [any other key] back"))
	return strings.Join(lines, "\n") + "\n"
}

// systemRow renders one fleet line: cursor + system id + colored state badge + pid + bar age + wedged.
func (m model) systemRow(s ipc.System, selected bool) string {
	cursor := "  "
	idStyle := lipgloss.NewStyle()
	if selected {
		cursor = cursorStyle.Render("▸ ")
		idStyle = selStyle
	}
	pid := "–"
	if s.PID != 0 {
		pid = fmt.Sprintf("%d", s.PID)
	}
	age := "–"
	if s.State == ipc.StateRunning && !s.Multi { // a multi runner is one row; per-symbol bar-age isn't shown
		age = fmt.Sprintf("%.0fs", s.LastBarAgeS)
	}
	id := s.SystemID
	if s.Multi {
		id += fmt.Sprintf(" ·%dsym", len(s.Symbols))
	}
	row := cursor +
		idStyle.Width(colSystem).Render(truncate(id, colSystem)) +
		stateStyle(s.State).Width(colState).Render(stateLabel(s.State)) +
		dimStyle.Width(colPID).Render(pid) +
		dimStyle.Width(colAge).Render(age)
	if s.Wedged {
		row += wedgeStyle.Render("⚠ WEDGED")
	}
	return row
}

// terminalHeader is the blast-radius subheader for a broker session (§7): account + the N/10 cap,
// reddened when over cap. Non-running systems group under an "— not running —" header.
func (m model) terminalHeader(session string, terms map[string]ipc.Terminal) string {
	if session == "" {
		return dimStyle.Render("— not running —")
	}
	t := terms[session]
	acct := t.Account
	if acct == "" {
		acct = "?"
	}
	capStr, capStyle := fmt.Sprintf("%d/%d systems", t.LogicalSystems, terminalCap), dimStyle
	if t.LogicalSystems > terminalCap {
		capStr += "  OVER CAP"
		capStyle = alertStyle
	}
	return headerStyle.Render(fmt.Sprintf("terminal %s · %s · ", session, acct)) + capStyle.Render(capStr)
}

// titleBar renders "signalfoundry supervisor — <view>" with engine pid/clock/alert state, right-aligned
// to the terminal width when known.
func (m model) titleBar(view string) string {
	left := titleStyle.Render("signalfoundry supervisor") + dimStyle.Render(" — "+view)
	if m.fleet.UpdatedAt.IsZero() {
		return left + dimStyle.Render("   (waiting for engine…)")
	}
	alerts := dimStyle.Render("alerts off")
	if m.fleet.Engine.Alerts {
		alerts = okStyle.Render("alerts ✓")
	}
	right := dimStyle.Render(fmt.Sprintf("engine %d · %s · ", m.fleet.Engine.PID, m.fleet.UpdatedAt.Format("15:04:05"))) + alerts
	gap := 3
	if m.width > 0 {
		if g := m.width - lipgloss.Width(left) - lipgloss.Width(right); g > gap {
			gap = g
		}
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m model) statusStyle() lipgloss.Style {
	if strings.Contains(m.status, "REJECTED") || strings.Contains(m.status, "failed") {
		return alertStyle
	}
	return okStyle
}

// truncate shortens s to w runes with a trailing ellipsis (rune-safe so a non-ASCII id is never
// split mid-rune). Note: assumes single-width runes — fine for system ids.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	return string(r[:w-1]) + "…"
}

// --- actions -----------------------------------------------------------------

// submitOne drops a command for the selected row. The engine validates eligibility and the PID;
// the TUI just relays the result. Per-system stop is a single keystroke (no confirm, §11.1).
func (m *model) submitOne(action string) {
	if m.cursor < 0 || m.cursor >= len(m.fleet.Systems) {
		return
	}
	sysID := m.fleet.Systems[m.cursor].SystemID
	if id, err := ipc.SubmitCommand(m.cfg.CommandsDir(), action, sysID); err == nil {
		m.pending[id] = pendingCmd{action: action, systemID: sysID}
		m.status = fmt.Sprintf("submitted %s for %s", action, sysID)
	} else {
		m.status = "submit failed: " + err.Error()
	}
}

// submitBulk fans out one single-system command per eligible target (§11.1: bulk is a fan-out, not
// a new command shape). Eligibility: start-all -> Stopped; stop-all / restart-all -> Running.
func (m *model) submitBulk(action string) {
	targets := m.bulkTargets(action)
	if len(targets) == 0 {
		m.status = "nothing eligible for " + action + "-all"
		return
	}
	n := 0
	for _, sysID := range targets {
		if id, err := ipc.SubmitCommand(m.cfg.CommandsDir(), action, sysID); err == nil {
			m.pending[id] = pendingCmd{action: action, systemID: sysID}
			n++
		}
	}
	m.status = fmt.Sprintf("submitted %s-all to %d system(s)", action, n)
}

func (m *model) armBulkConfirm(action string) {
	if n := len(m.bulkTargets(action)); n > 0 {
		m.confirm = &confirmState{action: action, count: n}
	} else {
		m.status = "nothing running for " + action + "-all"
	}
}

// bulkTargets returns the system_ids eligible for a whole-box action (§11.1).
func (m *model) bulkTargets(action string) []string {
	var ids []string
	for _, s := range m.fleet.Systems {
		switch action {
		case "start":
			if s.State == ipc.StateStopped {
				ids = append(ids, s.SystemID)
			}
		case "stop", "restart":
			if s.State == ipc.StateRunning {
				ids = append(ids, s.SystemID)
			}
		}
	}
	return ids
}

// resolvePending surfaces the engine's result for any command we submitted (accepted/outcome, or
// the rejection reason), then forgets it.
func (m *model) resolvePending() {
	for id, pc := range m.pending {
		res, ok := ipc.ReadResult(m.cfg.CommandsDir(), id)
		if !ok {
			continue
		}
		if res.Accepted {
			m.status = fmt.Sprintf("%s %s → %s", pc.action, pc.systemID, res.Outcome)
		} else {
			m.status = fmt.Sprintf("%s %s → REJECTED: %s", pc.action, pc.systemID, res.Error)
		}
		delete(m.pending, id)
	}
}

func (m *model) clampCursor() {
	if m.cursor >= len(m.fleet.Systems) {
		m.cursor = len(m.fleet.Systems) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// --- settings ----------------------------------------------------------------

// openSettings loads the live settings.json (engine-seeded) into the editable copy.
func (m *model) openSettings() {
	s, err := ipc.ReadSettings(m.cfg.SettingsPath())
	if err != nil {
		s = ipc.DefaultSettings()
	}
	m.settings = s.Normalized(ipc.DefaultSettings())
	m.settingsOpen, m.setCursor = true, 0
}

// settingsAdjust changes the selected setting by delta and persists immediately — the engine
// live-reads settings.json each tick, so the change applies without a restart.
func (m *model) settingsAdjust(delta int) {
	switch m.setCursor {
	case 0: // wedge alert mode — cycle through the three modes
		modes := []ipc.WedgeAlertMode{ipc.WedgeAlertAlways, ipc.WedgeAlertWeekend, ipc.WedgeAlertSurface}
		idx := 0
		for i, mode := range modes {
			if mode == m.settings.WedgeAlert {
				idx = i
			}
		}
		m.settings.WedgeAlert = modes[(idx+delta+len(modes))%len(modes)]
	case 1: // wedge multiple (>= 1)
		m.settings.WedgeMultiple += delta
		if m.settings.WedgeMultiple < 1 {
			m.settings.WedgeMultiple = 1
		}
	case 2: // wedge grace seconds (>= 0, step 15s)
		m.settings.WedgeGraceS += delta * 15
		if m.settings.WedgeGraceS < 0 {
			m.settings.WedgeGraceS = 0
		}
	}
	if err := ipc.WriteSettings(m.cfg.SettingsPath(), m.settings); err != nil {
		m.status = "settings save failed: " + err.Error()
	} else {
		m.status = "settings saved"
	}
}

func (m model) settingsView() string {
	rows := []struct{ label, val, hint string }{
		{"wedge alert", string(m.settings.WedgeAlert), "always | weekend | surface_only"},
		{"wedge multiple", fmt.Sprintf("%d bars", m.settings.WedgeMultiple), "missed bar intervals before wedged"},
		{"wedge grace", fmt.Sprintf("%ds", m.settings.WedgeGraceS), "slack added to N×timeframe"},
	}
	lines := []string{m.titleBar("settings"), ""}
	for i, r := range rows {
		cursor, labelStyle := "  ", dimStyle
		if i == m.setCursor {
			cursor, labelStyle = cursorStyle.Render("▸ "), selStyle
		}
		lines = append(lines, cursor+
			labelStyle.Width(18).Render(r.label)+
			okStyle.Width(16).Render(r.val)+
			dimStyle.Render(r.hint))
	}
	lines = append(lines, "")
	if m.status != "" {
		lines = append(lines, m.statusStyle().Render(m.status))
	}
	lines = append(lines, "", dimStyle.Render("↑/↓ select · ←/→ or -/+ change (auto-saved) · esc back"))
	return strings.Join(lines, "\n") + "\n"
}

// --- polling -----------------------------------------------------------------

func poll(cfg config.Config) tea.Cmd {
	return func() tea.Msg {
		fs, err := ipc.ReadFleetState(cfg.FleetStatePath())
		if err != nil {
			return errMsg{err}
		}
		return fs
	}
}

func tick() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}
