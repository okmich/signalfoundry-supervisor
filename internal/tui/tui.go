// Package tui is the on-demand Bubble Tea face: it polls fleet_state.json and renders the fleet;
// keys issue start/stop/restart by dropping command files. Closing it never touches the engine.
package tui

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
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
	cfg       config.Config
	fleet     ipc.FleetState
	err       error
	cursor    int                   // selected fleet row
	confirm   *confirmState         // non-nil while awaiting y/n for a bulk stop
	importing *importState          // non-nil while the import dialog is open
	pending   map[string]pendingCmd // submitted command id -> what it was, to surface its result
	status    string                // latest action / result line

	settingsOpen bool         // settings screen is showing
	settings     ipc.Settings // editable copy (loaded on open, written on each change)
	setCursor    int          // selected settings row (0=mode, 1=multiple, 2=grace)

	detailOpen bool    // the per-system details screen (Glance, §15) is showing
	detailID   string  // SystemID being shown (re-looked-up each tick for live status)
	detailTab  int     // active inference symbol tab (right pane)
	sysPane    logPane // left pane: z_system_log.log (one per process)
	infPane    logPane // right pane: the active symbol's inference JSONL

	width  int // terminal width (from WindowSizeMsg), for right-aligning the title bar
	height int // terminal height, to bound the log panes
}

// logPane is one tailed file in the details view. key is the read's identity (the file path for the
// text log, the inference dir for the JSONL) so a stale async read for a pane we've since retargeted
// is discarded; path is the dated file last read (display); lines/err is the latest tail.
type logPane struct {
	key   string
	path  string
	lines []string
	err   error
}

// confirmState gates a destructive action behind an explicit y/n, so an errant keystroke never fires
// it: a single stop/restart, a fleet-wide stop/restart (§11.1), or quitting the TUI.
type confirmState struct {
	action   string // "stop" | "restart" | "quit"
	bulk     bool   // whole-box fan-out vs a single system
	systemID string // single target (empty for bulk / quit)
	count    int    // bulk target count (for the prompt)
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

// pane discriminates the two log panes of the details view.
const (
	paneSys = iota // left: z_system_log.log
	paneInf        // right: active symbol's inference JSONL
)

// logTailMsg carries a fresh tail for one details pane. pane says which; key is the read's identity
// (matched against the pane's current target so a stale async read is discarded); path is the file
// actually read (display, resolved per read so it follows the UTC-midnight rollover).
type logTailMsg struct {
	pane  int
	key   string
	path  string
	lines []string
	err   error
}

func (m model) Init() tea.Cmd { return tea.Batch(poll(m.cfg), tick()) }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case ipc.FleetState:
		m.fleet, m.err = msg, nil
		m.clampCursor()
		return m, nil
	case errMsg:
		m.err = msg.err
		return m, nil
	case logTailMsg:
		if m.detailOpen { // apply only if the pane still targets what this read was for
			switch msg.pane {
			case paneSys:
				if msg.key == m.sysPane.key {
					m.sysPane.path, m.sysPane.lines, m.sysPane.err = msg.path, msg.lines, msg.err
				}
			case paneInf:
				if msg.key == m.infPane.key {
					m.infPane.path, m.infPane.lines, m.infPane.err = msg.path, msg.lines, msg.err
				}
			}
		}
		return m, nil
	case tickMsg:
		m.resolvePending()
		cmds := []tea.Cmd{poll(m.cfg), tick()}
		if m.detailOpen { // keep both detail panes live alongside the fleet poll
			cmds = append(cmds, m.detailReads()...)
		}
		return m, tea.Batch(cmds...)
	}
	return m, nil
}

func (m model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	// A pending confirmation intercepts ALL keys on every screen: only y proceeds, any other key
	// cancels — so an errant keystroke can never fire a stop/restart or quit the TUI.
	if m.confirm != nil {
		return m.handleConfirmKey(k)
	}
	if m.settingsOpen {
		return m.handleSettingsKey(k)
	}
	if m.detailOpen {
		return m.handleDetailKey(k)
	}
	switch k.String() {
	case "q", "ctrl+c": // quitting is confirmed too (§ erroneous-keypress guard)
		m.armQuit()
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.fleet.Systems)-1 {
			m.cursor++
		}
	case "s": // start — confirm first (every market-moving key is gated; operator preference)
		m.armSingleConfirm("start", m.currentSystemID())
	case "x": // stop — confirm first
		m.armSingleConfirm("stop", m.currentSystemID())
	case "r": // restart — confirm first
		m.armSingleConfirm("restart", m.currentSystemID())
	case "K": // force-kill — confirm first (bypasses graceful shutdown); shift-K to avoid the vim-k nav + add friction
		m.armSingleConfirm("kill", m.currentSystemID())
	case "S": // start-all — confirm first
		m.armBulkConfirm("start")
	case "X": // stop-all — confirm first
		m.armBulkConfirm("stop")
	case "R": // restart-all — confirm first (it is a fleet-wide stop too)
		m.armBulkConfirm("restart")
	case "i": // open the import-system dialog
		m.importing = &importState{}
		m.status = ""
	case "c": // open the settings screen
		m.openSettings()
	case "l", "enter": // open the per-system details screen (Glance, §15) for the selected row
		m.openDetail()
		if m.detailOpen {
			return m, tea.Batch(m.detailReads()...)
		}
	}
	return m, nil
}

// handleConfirmKey resolves a pending confirmation: only y/Y proceeds, anything else cancels.
func (m model) handleConfirmKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	c := m.confirm
	m.confirm = nil
	if s := k.String(); s != "y" && s != "Y" {
		m.status = "cancelled"
		return m, nil
	}
	switch c.action {
	case "quit":
		return m, tea.Quit
	case "start", "stop", "restart", "kill":
		if c.bulk {
			m.submitBulk(c.action)
		} else {
			m.submitByID(c.action, c.systemID)
		}
	}
	return m, nil
}

// handleDetailKey drives the details screen: tab/←→ cycle the inference symbol tabs, s/x/r/K control
// this system in place (all confirmed), esc/q back to the fleet (no confirm — non-destructive),
// ctrl+c quits (confirmed).
func (m model) handleDetailKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "ctrl+c":
		m.armQuit()
	case "esc", "q":
		m.closeDetail()
	case "tab", "right":
		return m.cycleTab(1)
	case "shift+tab", "left":
		return m.cycleTab(-1)
	case "s":
		m.armSingleConfirm("start", m.detailID)
	case "x":
		m.armSingleConfirm("stop", m.detailID)
	case "r":
		m.armSingleConfirm("restart", m.detailID)
	case "K": // force-kill (shift-K, consistent with the fleet view)
		m.armSingleConfirm("kill", m.detailID)
	}
	return m, nil
}

// handleSettingsKey drives the settings screen: navigate rows, change the selected value (which
// auto-writes settings.json so the engine applies it next tick), esc/c/q to close.
func (m model) handleSettingsKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "ctrl+c":
		m.armQuit()
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
	switch {
	case m.settingsOpen:
		return m.settingsView()
	case m.detailOpen:
		return m.detailView()
	default:
		return m.fleetView()
	}
}

// fleetView is the main screen: the fleet table inside a titled panel, with a bottom key-hint bar.
func (m model) fleetView() string {
	var body []string
	if m.err != nil {
		body = append(body, alertStyle.Render("engine offline? ")+dimStyle.Render(m.err.Error()), "")
	}
	body = append(body, "  "+
		headerStyle.Width(colSystem).Render("SYSTEM")+
		headerStyle.Width(colState).Render("STATE")+
		headerStyle.Width(colPID).Render("PID")+
		headerStyle.Width(colAge).Render("BAR AGE"))
	if len(m.fleet.Systems) == 0 {
		body = append(body, dimStyle.Render("  (no systems)"))
	}
	terms := make(map[string]ipc.Terminal, len(m.fleet.Terminals))
	for _, t := range m.fleet.Terminals {
		terms[t.BrokerSessionID] = t
	}
	prevSession := "\x00" // sentinel before any real session
	for i, s := range m.fleet.Systems {
		if s.SessionID != prevSession { // start of a terminal group -> subheader
			prevSession = s.SessionID
			body = append(body, m.terminalHeader(s.SessionID, terms))
		}
		body = append(body, m.systemRow(s, i == m.cursor))
	}

	right := dimStyle.Render(plural(len(m.fleet.Systems), "system") + " · " + plural(len(m.fleet.Terminals), "terminal"))
	box := titledBox(boxTitleStyle.Render("fleet"), right, m.cols()-2, len(body), body)

	lines := []string{m.titleBar("fleet"), "", box, ""}
	if c := m.confirmBar(); c != "" {
		lines = append(lines, c)
	}
	if m.status != "" {
		lines = append(lines, m.statusStyle().Render(m.status))
	}
	lines = append(lines, "", hintBar(
		hint{"↑↓", "select"}, hint{"s", "start"}, hint{"x", "stop"}, hint{"r", "restart"}, hint{"K", "kill"},
		hint{"S/X/R", "all"}, hint{"l", "details"}, hint{"c", "config"}, hint{"q", "quit"},
	))
	return strings.Join(lines, "\n") + "\n"
}

// confirmBar renders the pending-confirmation prompt (empty when none). Shown on every screen so a
// confirm armed anywhere is visible and answered in place.
func (m model) confirmBar() string {
	if m.confirm == nil {
		return ""
	}
	c := m.confirm
	var what string
	switch {
	case c.action == "quit":
		what = "Quit the supervisor"
	case c.action == "kill":
		what = fmt.Sprintf("KILL %s (force — skips graceful shutdown)", c.systemID)
	case c.bulk:
		what = fmt.Sprintf("%s ALL %d system(s)", strings.ToUpper(c.action), c.count)
	default:
		what = fmt.Sprintf("%s %s", strings.ToUpper(c.action), c.systemID)
	}
	return alertStyle.Render(what+"?") + dimStyle.Render("   [y] confirm   ·   any other key cancels")
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
	if s.State == ipc.StateRunning && s.LastBarAgeS > 0 { // for a multi runner this is the STALEST leg (§15)
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
	hdr := headerStyle.Render(fmt.Sprintf("terminal %s · %s · ", session, acct)) + capStyle.Render(capStr)
	if b := healthBadge(t.Health); b != "" { // broker-session precondition (§13)
		hdr += dimStyle.Render(" · ") + b
	}
	return hdr
}

// healthBadge renders a broker-session health badge; unknown/unprobed (the default until an adapter
// or operator override exists) shows nothing, to keep the common case uncluttered.
func healthBadge(health string) string {
	switch health {
	case "green":
		return okStyle.Render("session ✓")
	case "red":
		return alertStyle.Render("session ✗ DOWN")
	default:
		return ""
	}
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

// currentSystemID is the selected fleet row's id ("" if the fleet is empty).
func (m model) currentSystemID() string {
	if m.cursor < 0 || m.cursor >= len(m.fleet.Systems) {
		return ""
	}
	return m.fleet.Systems[m.cursor].SystemID
}

// submitOne drops a command for the selected row via submitByID. Interactive start/stop/restart now
// all route through a confirm; this remains for direct/programmatic use.
func (m *model) submitOne(action string) { m.submitByID(action, m.currentSystemID()) }

// submitByID drops a single-system command for an explicit system id (captured at confirm-arm time,
// so a confirmed stop/restart targets exactly what was shown in the prompt).
func (m *model) submitByID(action, systemID string) {
	if systemID == "" {
		return
	}
	if id, err := ipc.SubmitCommand(m.cfg.CommandsDir(), action, systemID); err == nil {
		m.pending[id] = pendingCmd{action: action, systemID: systemID}
		m.status = fmt.Sprintf("submitted %s for %s", action, systemID)
	} else {
		m.status = "submit failed: " + err.Error()
	}
}

// armSingleConfirm gates a single-system start/stop/restart/kill behind y/n.
func (m *model) armSingleConfirm(action, systemID string) {
	if systemID == "" {
		return
	}
	m.confirm = &confirmState{action: action, systemID: systemID}
}

// armQuit gates closing the TUI behind y/n (an errant q / ctrl+c must not exit).
func (m *model) armQuit() { m.confirm = &confirmState{action: "quit"} }

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
		m.confirm = &confirmState{action: action, bulk: true, count: n}
	} else {
		m.status = "nothing eligible for " + action + "-all"
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
	var body []string
	for i, r := range rows {
		cursor, labelStyle := "  ", dimStyle
		if i == m.setCursor {
			cursor, labelStyle = cursorStyle.Render("▸ "), selStyle
		}
		body = append(body, cursor+
			labelStyle.Width(18).Render(r.label)+
			okStyle.Width(16).Render(r.val)+
			dimStyle.Render(r.hint))
	}
	box := titledBox(boxTitleStyle.Render("settings"), dimStyle.Render("wedge alerting · live"), m.cols()-2, len(body), body)

	lines := []string{m.titleBar("settings"), "", box, ""}
	if c := m.confirmBar(); c != "" {
		lines = append(lines, c)
	}
	if m.status != "" {
		lines = append(lines, m.statusStyle().Render(m.status))
	}
	lines = append(lines, "", hintBar(
		hint{"↑↓", "select"}, hint{"←→ / -+", "change (auto-saved)"}, hint{"esc", "back"}, hint{"q", "quit"},
	))
	return strings.Join(lines, "\n") + "\n"
}

// --- system details (Glance, §15) --------------------------------------------

const (
	logTailMax    = 500        // most lines kept/scanned from the tail
	logTailWindow = 256 * 1024 // bytes read from the file's end (the live view depends on the JSONL, §15)
)

// infTab is one inference symbol tab in the details view (one per logical system).
type infTab struct {
	symbol    string
	inference string // the symbol's inference dir
	wedged    bool
	lastBar   time.Time
}

// detailTabs lists the inference tabs for a system: one per leg for a multi-trader, or the single
// symbol for a single-trader.
func detailTabs(s ipc.System) []infTab {
	if len(s.Legs) > 0 {
		tabs := make([]infTab, len(s.Legs))
		for i, l := range s.Legs {
			tabs[i] = infTab{symbol: l.Symbol, inference: l.Inference, wedged: l.Wedged, lastBar: l.LastBarTS}
		}
		return tabs
	}
	return []infTab{{symbol: s.Symbol, inference: s.LogPaths.Inference, wedged: s.Wedged, lastBar: s.LastBarTS}}
}

// detailSystem re-finds the system being shown by id (the published fleet is rebuilt each tick, so
// we look it up fresh rather than snapshotting it — the details view stays live).
func (m model) detailSystem() (ipc.System, bool) {
	for _, s := range m.fleet.Systems {
		if s.SystemID == m.detailID {
			return s, true
		}
	}
	return ipc.System{}, false
}

// sessionHealth finds the published broker-session health for a session id ("" if not grouped).
func (m model) sessionHealth(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	for _, t := range m.fleet.Terminals {
		if t.BrokerSessionID == sessionID {
			return t.Health
		}
	}
	return ""
}

// openDetail opens the details screen for the selected row, defaulting the inference tab to the
// stalest leg (most likely the one worth inspecting).
func (m *model) openDetail() {
	if m.cursor < 0 || m.cursor >= len(m.fleet.Systems) {
		return
	}
	s := m.fleet.Systems[m.cursor]
	tabs := detailTabs(s)
	m.detailID, m.detailTab, m.detailOpen = s.SystemID, stalestTab(tabs), true
	m.sysPane = logPane{key: s.LogPaths.Text}
	m.infPane = logPane{}
	if t := m.detailTab; t < len(tabs) {
		m.infPane.key = tabs[t].inference
	}
}

func (m *model) closeDetail() {
	m.detailOpen, m.detailID, m.detailTab = false, "", 0
	m.sysPane, m.infPane = logPane{}, logPane{}
}

// cycleTab moves the active inference tab by d (wrapping) and retargets the right pane.
func (m model) cycleTab(d int) (tea.Model, tea.Cmd) {
	s, ok := m.detailSystem()
	if !ok {
		return m, nil
	}
	tabs := detailTabs(s)
	if len(tabs) <= 1 {
		return m, nil
	}
	m.detailTab = (m.detailTab + d + len(tabs)) % len(tabs)
	m.infPane = logPane{key: tabs[m.detailTab].inference}
	if m.infPane.key == "" {
		return m, nil
	}
	return m, readInferenceTail(m.infPane.key)
}

// detailReads are the tea.Cmds that refresh both panes (skipping a pane with no target).
func (m model) detailReads() []tea.Cmd {
	var cmds []tea.Cmd
	if m.sysPane.key != "" {
		cmds = append(cmds, readTextTail(m.sysPane.key))
	}
	if m.infPane.key != "" {
		cmds = append(cmds, readInferenceTail(m.infPane.key))
	}
	return cmds
}

// stalestTab picks the tab to open by default: the leg with the oldest bar (preferring one that has
// data), i.e. the one most likely wedged.
func stalestTab(tabs []infTab) int {
	best := 0
	for i := 1; i < len(tabs); i++ {
		switch {
		case tabs[best].lastBar.IsZero() && !tabs[i].lastBar.IsZero():
			best = i
		case !tabs[i].lastBar.IsZero() && tabs[i].lastBar.Before(tabs[best].lastBar):
			best = i
		}
	}
	return best
}

func (m model) detailView() string {
	s, ok := m.detailSystem()
	if !ok { // the system left the catalog while we were looking at it
		return strings.Join([]string{m.titleBar("system"), "",
			alertStyle.Render("  " + m.detailID + " is no longer in the fleet"), "",
			hintBar(hint{"esc", "back"}, hint{"q", "quit"})}, "\n") + "\n"
	}
	tabs := detailTabs(s)
	if m.detailTab >= len(tabs) { // legs shrank under us
		m.detailTab = 0
	}

	// status panel
	title := boxTitleStyle.Render("status") + "  " + dimStyle.Render(s.SystemID)
	if s.Multi {
		title += dimStyle.Render(fmt.Sprintf(" ·%dsym", len(tabs)))
	}
	statusBody := m.detailStatusBody(s)
	badge := stateStyle(s.State).Render(stateLabel(s.State))
	if b := healthBadge(m.sessionHealth(s.SessionID)); b != "" { // broker-session precondition (§13)
		badge += "  " + b
	}
	statusBox := titledBox(title, badge, m.cols()-2, len(statusBody), statusBody)

	// two side-by-side log panes filling the rest of the height
	innerH := m.detailPaneHeight()
	leftW := (m.cols() - 4) / 2
	rightW := m.cols() - 4 - leftW
	leftBox := titledBox(boxTitleStyle.Render("z_system_log"), "", leftW, innerH, paneLines(m.sysPane, leftW, innerH))
	rightTitle := boxTitleStyle.Render("inference") + "  " + m.tabStrip(tabs)
	rightBox := titledBox(rightTitle, "", rightW, innerH, paneLines(m.infPane, rightW, innerH))
	panes := lipgloss.JoinHorizontal(lipgloss.Top, leftBox, rightBox)

	lines := []string{m.titleBar("system"), "", statusBox, panes, ""}
	if c := m.confirmBar(); c != "" {
		lines = append(lines, c, "")
	}
	if m.status != "" {
		lines = append(lines, m.statusStyle().Render(m.status), "")
	}
	lines = append(lines, hintBar(hint{"tab/←→", "switch symbol"},
		hint{"s", "start"}, hint{"x", "stop"}, hint{"r", "restart"}, hint{"K", "kill"}, hint{"esc", "back"}, hint{"q", "quit"}))
	return strings.Join(lines, "\n") + "\n"
}

// detailStatusBody is the two-row key/value grid of the status panel (state lives in the box badge).
func (m model) detailStatusBody(s ipc.System) []string {
	f := func(label, value string) string {
		return lipgloss.NewStyle().Width(22).Render(dimStyle.Render(label+" ") + value)
	}
	val := func(v string) string {
		if v == "" {
			return dimStyle.Render("–")
		}
		return v
	}
	pid := "–"
	if s.PID != 0 {
		pid = strconv.Itoa(s.PID)
	}
	started := "–"
	if !s.StartedAt.IsZero() {
		started = s.StartedAt.UTC().Format("01-02 15:04Z")
	}
	age := "–"
	if s.State == ipc.StateRunning && s.LastBarAgeS > 0 {
		age = humanizeAge(s.LastBarTS)
		if s.Multi {
			age += " (stalest)"
		}
	}
	wedged := okStyle.Render("no")
	if s.Wedged {
		w := "⚠ yes"
		if sym := stalestWedgedSymbol(s); sym != "" {
			w += " (" + sym + ")"
		}
		wedged = wedgeStyle.Render(w)
	}
	return []string{
		"  " + f("PID", val(pid)) + f("Broker", val(s.Broker)) + f("Account", val(s.Account)) + f("Session", val(s.SessionID)),
		"  " + f("Token", val(shortToken(s.StartToken))) + f("Started", val(started)) + f("Bar age", val(age)) + f("Wedged", wedged),
	}
}

// tabStrip renders the inference symbol tabs into the right pane's border — active tab reverse-video,
// each showing its own bar-age and a ⚠ if that leg is wedged.
func (m model) tabStrip(tabs []infTab) string {
	parts := make([]string, len(tabs))
	for i, t := range tabs {
		label := t.symbol
		if age := humanizeAge(t.lastBar); age != "" {
			label += " " + age
		}
		if t.wedged {
			label += " ⚠"
		}
		if i == m.detailTab {
			parts[i] = tabActiveStyle.Render(" " + label + " ")
		} else {
			parts[i] = tabStyle.Render(label)
		}
	}
	return strings.Join(parts, dimStyle.Render(" · "))
}

// paneLines renders a pane's tail into innerH rows, each clipped to w (log lines are plain text, so
// rune-truncation is safe).
func paneLines(p logPane, w, innerH int) []string {
	switch {
	case p.err != nil:
		return []string{alertStyle.Render(truncate("read error: "+p.err.Error(), w))}
	case len(p.lines) == 0:
		return []string{dimStyle.Render("(no log yet)")}
	default:
		body := p.lines
		if len(body) > innerH {
			body = body[len(body)-innerH:]
		}
		out := make([]string, len(body))
		for i, l := range body {
			out[i] = truncate(l, w)
		}
		return out
	}
}

// detailPaneHeight is the inner height of the log panes — the screen minus the title/status/hint
// chrome, so the panes fill the lower ~⅔.
func (m model) detailPaneHeight() int {
	return max(m.rows()-12, 3)
}

// shortToken trims a start token to a glanceable prefix.
func shortToken(t string) string {
	if len(t) > 10 {
		return t[:8] + "…"
	}
	return t
}

// stalestWedgedSymbol names a wedged leg (for the status panel's Wedged annotation), "" if none.
func stalestWedgedSymbol(s ipc.System) string {
	for _, l := range s.Legs {
		if l.Wedged {
			return l.Symbol
		}
	}
	return ""
}

// humanizeAge renders a compact age since t ("2s", "7m", "1h04m"), "" if t is zero.
func humanizeAge(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := max(time.Since(t), 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func readTextTail(path string) tea.Cmd {
	return func() tea.Msg {
		lines, err := tailFile(path, logTailMax)
		return logTailMsg{pane: paneSys, key: path, path: path, lines: lines, err: err}
	}
}

// readInferenceTail resolves today's inference file under dir (re-resolved every read so the tail
// follows the UTC-midnight rollover) and tails it.
func readInferenceTail(dir string) tea.Cmd {
	return func() tea.Msg {
		path := filepath.Join(dir, "inference_"+time.Now().UTC().Format("20060102")+".jsonl")
		lines, err := tailFile(path, logTailMax)
		return logTailMsg{pane: paneInf, key: dir, path: path, lines: lines, err: err}
	}
}

// tailFile returns up to the last `limit` non-empty lines of a file, reading only its tail window so
// the cost stays constant as the day's JSONL grows. A missing file is not an error (the day's file
// may not exist yet) — it yields no lines.
func tailFile(path string, limit int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := int64(0)
	if fi.Size() > logTailWindow {
		start = fi.Size() - logTailWindow
	}
	buf := make([]byte, fi.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return nil, err
	}
	raw := strings.Split(string(buf), "\n")
	if start > 0 && len(raw) > 0 {
		raw = raw[1:] // drop a partial first line from mid-window
	}
	lines := make([]string, 0, len(raw))
	for _, l := range raw {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines, nil
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
