// Package tui is the on-demand Bubble Tea face: it polls fleet_state.json and renders the fleet;
// keys issue start/stop/restart by dropping command files. Closing it never touches the engine.
package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/okmich/signalfoundry-supervisor/internal/config"
	"github.com/okmich/signalfoundry-supervisor/internal/ipc"
)

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
	confirm *confirmState         // non-nil while awaiting y/n for a bulk stop
	pending map[string]pendingCmd // submitted command id -> what it was, to surface its result
	status  string                // latest action / result line

	settingsOpen bool         // settings screen is showing
	settings     ipc.Settings // editable copy (loaded on open, written on each change)
	setCursor    int          // selected settings row (0=mode, 1=multiple, 2=grace)
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

type tickMsg time.Time
type errMsg struct{ err error }

func (m model) Init() tea.Cmd { return tea.Batch(poll(m.cfg), tick()) }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)
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
	case "c": // open the settings screen
		m.openSettings()
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
	var b strings.Builder
	b.WriteString("signalfoundry supervisor — fleet\n")
	if !m.fleet.UpdatedAt.IsZero() {
		fmt.Fprintf(&b, "engine pid %d · updated %s\n", m.fleet.Engine.PID, m.fleet.UpdatedAt.Format("15:04:05"))
	}
	b.WriteString("\n")
	if m.err != nil {
		fmt.Fprintf(&b, "engine offline? %v\n\n", m.err)
	}
	if len(m.fleet.Systems) == 0 {
		b.WriteString("(no systems)\n")
	}
	for i, s := range m.fleet.Systems {
		cursor := "  "
		if i == m.cursor {
			cursor = "> "
		}
		flag := ""
		if s.Wedged {
			flag = "  [WEDGED]"
		}
		fmt.Fprintf(&b, "%s%-44s %-18s pid=%-7d bar_age=%4.0fs%s\n", cursor, s.SystemID, s.State, s.PID, s.LastBarAgeS, flag)
	}
	b.WriteString("\n")
	if m.confirm != nil {
		fmt.Fprintf(&b, "** %s ALL %d running system(s)?  [y] confirm   [any other key] cancel **\n",
			strings.ToUpper(m.confirm.action), m.confirm.count)
	}
	if m.status != "" {
		fmt.Fprintf(&b, "%s\n", m.status)
	}
	b.WriteString("\n[↑/↓] select  [s]tart [x]stop [r]restart  [S]/[X]/[R]=all  [c]onfig  [q]uit\n")
	return b.String()
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
	var b strings.Builder
	b.WriteString("signalfoundry supervisor — settings\n\n")
	rows := []struct{ label, val, hint string }{
		{"wedge alert", string(m.settings.WedgeAlert), "always | weekend | surface_only"},
		{"wedge multiple", fmt.Sprintf("%d bars", m.settings.WedgeMultiple), "missed bar intervals before wedged"},
		{"wedge grace", fmt.Sprintf("%ds", m.settings.WedgeGraceS), "slack added to N×timeframe"},
	}
	for i, r := range rows {
		cursor := "  "
		if i == m.setCursor {
			cursor = "> "
		}
		fmt.Fprintf(&b, "%s%-16s %-16s %s\n", cursor, r.label+":", r.val, r.hint)
	}
	b.WriteString("\n")
	if m.status != "" {
		fmt.Fprintf(&b, "%s\n", m.status)
	}
	b.WriteString("\n[↑/↓] select   [←/→ or -/+] change (auto-saved)   [esc] back\n")
	return b.String()
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
