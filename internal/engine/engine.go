// Package engine is the always-on headless daemon: discover, re-attach, watch (liveness), alert,
// and control. It is the SOLE actor on the trading systems; the TUI only reads state + drops commands.
package engine

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/okmich/signalfoundry-supervisor/internal/config"
	"github.com/okmich/signalfoundry-supervisor/internal/contract"
	"github.com/okmich/signalfoundry-supervisor/internal/discovery"
	"github.com/okmich/signalfoundry-supervisor/internal/ipc"
	"github.com/okmich/signalfoundry-supervisor/internal/notify"
	"github.com/okmich/signalfoundry-supervisor/internal/proc"
	"github.com/okmich/signalfoundry-supervisor/internal/registry"
	"github.com/okmich/signalfoundry-supervisor/internal/state"
)

// Version is the supervisor build version.
const Version = "0.0.1-dev"

// Command-file housekeeping: prune processed command/result files older than commandTTL, at most
// once per pruneEvery (the TUI reads a result within seconds, so the TTL is a generous margin).
const (
	commandTTL = time.Hour
	pruneEvery = time.Minute
)

// transition is an in-flight operator action the engine is awaiting. Reconcile() is stateless
// (it derives state purely from status.json), so the engine overlays these to show the in-flight
// state until status.json catches up, and to drive the stop/start deadlines.
type transition struct {
	action   string    // "stop" | "start" | "restart"
	phase    string    // restart only: "stopping" then "starting"
	pid      int       // the control PID the action targets / the spawned PID
	token    string    // status.json start token at fire time (PID-reuse context)
	runPy    string    // start/restart: the run.py to (re)launch
	deadline time.Time // current phase deadline
}

// identity is the (pid, create-time) the engine last confirmed for a Running system — the baseline
// for the PID-reuse guard (§9). `reused` edge-triggers the alert.
type identity struct {
	pid    int
	ctime  time.Time
	reused bool
}

type engine struct {
	cfg         config.Config
	transitions map[string]*transition // system_id -> in-flight action
	notifier    *notify.Telegram       // ops alerts (orphan/wedged/crash/hard-kill); no-op without creds
	wedged      map[string]bool        // system_id currently flagged wedged (edge-trigger the alert)
	identities  map[string]identity    // system_id -> confirmed (pid, create-time) for PID-reuse guard
	lastPrune   time.Time              // throttles command-file housekeeping
}

// alert logs an operator alert and best-effort pushes it to Telegram (no-op when creds absent).
func (e *engine) alert(msg string) {
	log.Printf("engine: ALERT %s", msg)
	if err := e.notifier.Send("supervisor ALERT: " + msg); err != nil {
		log.Printf("engine: notify failed: %v", err)
	}
}

// Run is the daemon loop: acquire the singleton, then each tick reconcile the fleet, process
// operator commands, overlay in-flight transitions, and publish the snapshot — until interrupted.
// It NEVER auto-starts a system (§13).
func Run(cfg config.Config) error {
	if err := os.MkdirAll(cfg.CommandsDir(), 0o755); err != nil {
		return err
	}
	sg, err := acquireSingleton(cfg.StateDir)
	if err != nil {
		return err
	}
	defer sg.release()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	e := &engine{
		cfg:         cfg,
		transitions: map[string]*transition{},
		wedged:      map[string]bool{},
		identities:  map[string]identity{},
		notifier:    notify.FromEnvFile(cfg.NotifierEnvPath()),
	}
	if _, err := os.Stat(cfg.SettingsPath()); os.IsNotExist(err) {
		if werr := ipc.WriteSettings(cfg.SettingsPath(), e.defaultSettings()); werr != nil {
			log.Println("engine: seed settings:", werr)
		}
	}
	startedAt := time.Now().UTC()
	log.Printf("engine %s up; live_base=%s state_dir=%s alerts=%v", Version, cfg.LiveBase, cfg.StateDir, e.notifier.Enabled())

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("engine: interrupted, shutting down")
			return nil
		case <-tick.C:
			now := time.Now().UTC()
			settings := e.loadSettings()
			systems := state.Reconcile(cfg)
			e.confirmIdentities(systems)            // PID-reuse guard: downgrade a Running row whose PID was recycled
			e.processCommands(systems)              // fire new actions, register transitions, write results
			e.applyTransitions(systems)             // overlay in-flight state, drive deadlines, clear on completion
			e.checkLiveness(systems, now, settings) // flag wedged (alive but stale JSONL), alert on the edge
			fs := ipc.FleetState{
				Engine:  ipc.EngineInfo{PID: os.Getpid(), StartedAt: startedAt, Version: Version},
				Systems: systems,
			}
			if err := ipc.Publish(cfg.FleetStatePath(), fs); err != nil {
				log.Println("engine: publish:", err)
			}
			if now.Sub(e.lastPrune) > pruneEvery {
				ipc.PruneCommands(cfg.CommandsDir(), commandTTL, now)
				e.lastPrune = now
			}
		}
	}
}

// processCommands executes pending operator commands against the freshly reconciled snapshot. The
// engine is the SOLE actor on systems. Each command is resolved by system_id to a live, verified
// PID (stop) or its run.py from the catalog (start/restart) before any action is taken.
func (e *engine) processCommands(systems []ipc.System) {
	cmds, err := ipc.PendingCommands(e.cfg.CommandsDir())
	if err != nil || len(cmds) == 0 {
		return
	}
	byID := indexByID(systems)
	var catalog map[string]discovery.System
	cat := func() map[string]discovery.System {
		if catalog == nil {
			catalog = scanCatalog(e.cfg.LiveBase)
		}
		return catalog
	}
	for _, c := range cmds {
		var res ipc.CommandResult
		switch c.Action {
		case "stop":
			res = e.doStop(c, byID[c.SystemID])
		case "start":
			res = e.doStart(c, byID[c.SystemID], cat()[c.SystemID])
		case "restart":
			res = e.doRestart(c, byID[c.SystemID], cat()[c.SystemID])
		default:
			res = ipc.CommandResult{ID: c.ID, Accepted: false, Error: "unknown action: " + c.Action}
		}
		_ = ipc.WriteResult(e.cfg.CommandsDir(), res)
	}
}

// doStop validates a stop against the snapshot, fires the targeted Ctrl+C (via the ctrlc helper),
// and registers the in-flight transition. The clean/dirty outcome is reflected in later snapshots
// (applyTransitions), not in this result — the result only records that the stop was accepted.
func (e *engine) doStop(c ipc.Command, s *ipc.System) ipc.CommandResult {
	res := ipc.CommandResult{ID: c.ID}
	switch {
	case s == nil:
		res.Accepted, res.Error = false, "unknown system: "+c.SystemID
		return res
	case e.transitions[c.SystemID] != nil:
		res.Accepted, res.Outcome = true, "stopping" // already in flight — idempotent
		return res
	case s.State != ipc.StateRunning || s.PID == 0 || !proc.Alive(s.PID):
		// PID-reuse guard (MVP): must be Running with a live PID. The create-time cross-check is
		// Phase 6 (proc.CreateTime); the control PID is status.json's pid (see memory note).
		res.Accepted, res.Error = false, fmt.Sprintf("not stoppable: state=%s pid=%d", s.State, s.PID)
		return res
	}
	if err := proc.Stop(s.PID); err != nil {
		res.Accepted, res.Error = false, "stop failed: "+err.Error()
		return res
	}
	e.transitions[c.SystemID] = &transition{action: "stop", pid: s.PID, token: s.StartToken, deadline: time.Now().UTC().Add(e.cfg.StopTimeout)}
	log.Printf("engine: stop fired for %s (pid=%d)", c.SystemID, s.PID)
	res.Accepted, res.Outcome = true, "stopping"
	return res
}

// doStart spawns a non-live system's run.py (its own console, §6) and awaits "running". A single
// start is a conscious operator action, so it accepts Stopped / StoppedByOperator / Crashed /
// CrashLoopHalted — but never something already live or in flight (§11.1).
func (e *engine) doStart(c ipc.Command, s *ipc.System, d discovery.System) ipc.CommandResult {
	res := ipc.CommandResult{ID: c.ID}
	switch {
	case s == nil || d.RunPy == "":
		res.Accepted, res.Error = false, "unknown system: "+c.SystemID
		return res
	case e.transitions[c.SystemID] != nil:
		res.Accepted, res.Outcome = true, "starting"
		return res
	case isLiveOrInFlight(s.State):
		res.Accepted, res.Error = false, "already live or in flight: "+string(s.State)
		return res
	}
	pid, err := e.launch(c.SystemID, d.RunPy)
	if err != nil {
		res.Accepted, res.Error = false, "spawn failed: "+err.Error()
		return res
	}
	e.transitions[c.SystemID] = &transition{action: "start", phase: "starting", pid: pid, runPy: d.RunPy, deadline: time.Now().UTC().Add(e.cfg.StartTimeout)}
	log.Printf("engine: start spawned %s (pid=%d, python=%s)", c.SystemID, pid, e.cfg.Python)
	res.Accepted, res.Outcome = true, "starting"
	return res
}

// doRestart is the clean restart primitive: graceful-stop-then-relaunch (§11). If the system is
// running it stops first (the relaunch happens in applyTransitions once the stop completes); if it
// is not running there is nothing to stop, so it launches directly.
func (e *engine) doRestart(c ipc.Command, s *ipc.System, d discovery.System) ipc.CommandResult {
	res := ipc.CommandResult{ID: c.ID}
	switch {
	case s == nil || d.RunPy == "":
		res.Accepted, res.Error = false, "unknown system: "+c.SystemID
		return res
	case e.transitions[c.SystemID] != nil:
		res.Accepted, res.Outcome = true, "restarting"
		return res
	}
	if s.State == ipc.StateRunning && s.PID != 0 && proc.Alive(s.PID) {
		if err := proc.Stop(s.PID); err != nil {
			res.Accepted, res.Error = false, "stop (for restart) failed: "+err.Error()
			return res
		}
		e.transitions[c.SystemID] = &transition{action: "restart", phase: "stopping", pid: s.PID, token: s.StartToken, runPy: d.RunPy, deadline: time.Now().UTC().Add(e.cfg.StopTimeout)}
		log.Printf("engine: restart (stopping) %s (pid=%d)", c.SystemID, s.PID)
		res.Accepted, res.Outcome = true, "restarting"
		return res
	}
	pid, err := e.launch(c.SystemID, d.RunPy)
	if err != nil {
		res.Accepted, res.Error = false, "spawn failed: "+err.Error()
		return res
	}
	e.transitions[c.SystemID] = &transition{action: "restart", phase: "starting", pid: pid, runPy: d.RunPy, deadline: time.Now().UTC().Add(e.cfg.StartTimeout)}
	log.Printf("engine: restart (spawn, was %s) %s (pid=%d)", s.State, c.SystemID, pid)
	res.Accepted, res.Outcome = true, "restarting"
	return res
}

// applyTransitions overlays in-flight actions on the reconciled snapshot and drives them to
// completion. Decisions read FRESH status.json: the reconciled snapshot was taken at the start of
// the tick, before this tick's actions, so its State can lag by one tick.
func (e *engine) applyTransitions(systems []ipc.System) {
	now := time.Now().UTC()
	byID := indexByID(systems)
	for id, tr := range e.transitions {
		s := byID[id]
		if s == nil {
			delete(e.transitions, id) // system left the catalog
			continue
		}
		switch tr.action {
		case "stop":
			e.resolveStop(id, s, tr, now)
		case "start":
			e.resolveStart(id, s, tr, now, ipc.StateStarting)
		case "restart":
			if tr.phase == "stopping" {
				e.resolveRestartStopping(id, s, tr, now)
			} else {
				e.resolveStart(id, s, tr, now, ipc.StateRestarting)
			}
		}
	}
}

// resolveStop: clean stop once status.json is stopped+disconnected (§10); orphan on a gone PID
// without that proof; hard-kill + orphan at the deadline; otherwise show Stopping.
func (e *engine) resolveStop(id string, s *ipc.System, tr *transition, now time.Time) {
	switch {
	case e.cleanStopped(s):
		s.State = ipc.StateStoppedByOp
		log.Printf("engine: %s stopped cleanly (operator stop)", id)
		delete(e.transitions, id)
	case !proc.Alive(tr.pid):
		s.State = ipc.StateOrphanSuspected
		e.alert(fmt.Sprintf("%s: pid %d gone without a clean disconnect — orphan suspected", id, tr.pid))
		delete(e.transitions, id)
	case now.After(tr.deadline):
		log.Printf("engine: %s did not stop within %s — hard-killing pid %d", id, e.cfg.StopTimeout, tr.pid)
		if err := proc.Kill(tr.pid); err != nil {
			log.Printf("engine: hard-kill %s failed: %v", id, err)
		}
		s.State = ipc.StateOrphanSuspected
		e.alert(fmt.Sprintf("%s: hard-killed after %s stop timeout — orphan suspected (pid %d)", id, e.cfg.StopTimeout, tr.pid))
		delete(e.transitions, id)
	default:
		s.State = ipc.StateStopping
	}
}

// resolveStart: confirmed running -> Running; past deadline without running -> Crashed (start
// failed); otherwise show the overlay (Starting for start, Restarting for a restart's start phase).
func (e *engine) resolveStart(id string, s *ipc.System, tr *transition, now time.Time, overlay ipc.State) {
	if rs, err := contract.ReadStatus(s.LogPaths.Status); err == nil && rs.State == "running" && proc.Alive(rs.PID) {
		s.State, s.PID, s.StartToken = ipc.StateRunning, rs.PID, rs.RunnerStartToken
		log.Printf("engine: %s started (running, pid=%d)", id, rs.PID)
		delete(e.transitions, id)
		return
	}
	if now.After(tr.deadline) {
		s.State = ipc.StateCrashed
		e.alert(fmt.Sprintf("%s: did not reach running within %s — start failed (crashed)", id, e.cfg.StartTimeout))
		delete(e.transitions, id)
		return
	}
	s.State = overlay
}

// resolveRestartStopping: the stop phase of a restart. Once stopped (cleanly, or the PID is gone,
// or hard-killed at the deadline) it relaunches and advances the transition to its start phase.
func (e *engine) resolveRestartStopping(id string, s *ipc.System, tr *transition, now time.Time) {
	stopped := e.cleanStopped(s) || !proc.Alive(tr.pid)
	if !stopped && now.After(tr.deadline) {
		if err := proc.Kill(tr.pid); err != nil {
			log.Printf("engine: hard-kill %s failed: %v", id, err)
		}
		e.alert(fmt.Sprintf("%s: hard-killed during restart after %s stop timeout — orphan risk (pid %d)", id, e.cfg.StopTimeout, tr.pid))
		stopped = true
	}
	if !stopped {
		s.State = ipc.StateRestarting
		return
	}
	pid, err := e.launch(id, tr.runPy)
	if err != nil {
		s.State = ipc.StateCrashed
		log.Printf("engine: restart %s relaunch failed: %v", id, err)
		delete(e.transitions, id)
		return
	}
	tr.pid, tr.phase, tr.deadline = pid, "starting", now.Add(e.cfg.StartTimeout)
	s.State = ipc.StateRestarting
	log.Printf("engine: restart %s relaunched (pid=%d)", id, pid)
}

// cleanStopped reports whether status.json shows the §10 clean-stop proof (stopped + disconnected).
func (e *engine) cleanStopped(s *ipc.System) bool {
	rs, err := contract.ReadStatus(s.LogPaths.Status)
	return err == nil && rs.State == "stopped" && rs.BrokerDisconnected != nil && *rs.BrokerDisconnected
}

// defaultSettings is the engine's seed for settings.json: the operator-chosen default gate
// (weekend) plus the env-configured thresholds.
func (e *engine) defaultSettings() ipc.Settings {
	return ipc.Settings{
		WedgeAlert:    ipc.WedgeAlertWeekend,
		WedgeMultiple: e.cfg.WedgeMultiple,
		WedgeGraceS:   int(e.cfg.WedgeGrace / time.Second),
	}
}

// loadSettings reads the live settings.json, falling back to (and normalizing against) the engine
// defaults so a missing/partial/hand-broken file never wedges the engine itself.
func (e *engine) loadSettings() ipc.Settings {
	def := e.defaultSettings()
	s, err := ipc.ReadSettings(e.cfg.SettingsPath())
	if err != nil {
		return def
	}
	return s.Normalized(def)
}

// confirmIdentities is the PID-reuse guard (§9): for each Running system it compares the live
// process's creation time against the one first confirmed for that PID. A matching PID with a
// CHANGED creation time means the OS recycled the PID for an unrelated process — so the row is
// downgraded to OrphanSuspected and control is refused (the existing state==Running guards then
// reject any stop/kill), preventing the supervisor from Ctrl+C/TerminateProcess-ing a stranger.
//
// Scope: this catches the steady-state case (a system the engine saw Running later dies and its PID
// is reused). Adopting a system that was ALREADY stale+reused at engine startup needs the registry
// baseline of §12 re-attach (a recorded create-time/start-token), which is still deferred. When
// CreateTime is unavailable (non-Windows / no rights) the guard is a no-op (falls back to Alive).
func (e *engine) confirmIdentities(systems []ipc.System) {
	for i := range systems {
		s := &systems[i]
		if s.State != ipc.StateRunning || s.PID == 0 {
			continue
		}
		ct, ok := proc.CreateTime(s.PID)
		if !ok {
			continue
		}
		prev, seen := e.identities[s.SystemID]
		switch {
		case !seen || prev.pid != s.PID:
			e.identities[s.SystemID] = identity{pid: s.PID, ctime: ct} // first sight / new incarnation
		case prev.ctime.Equal(ct):
			// same PID, same process — consistent, nothing to do
		default:
			s.State = ipc.StateOrphanSuspected
			if !prev.reused {
				e.alert(fmt.Sprintf("%s: pid %d reused by another process (create-time changed) — refusing control, orphan suspected", s.SystemID, s.PID))
				prev.reused = true
				e.identities[s.SystemID] = prev
			}
		}
	}
}

// checkLiveness flags a Running system as wedged when its JSONL has gone stale past the threshold
// (FLEET_SUPERVISOR_SPEC §15): alive but not advancing. It edge-triggers the alert (once per wedge,
// once on recovery). The flag is always surfaced; the runtime-tunable WedgeAlert mode gates only
// the page — `surface_only` never pages, `weekend` skips the FX weekend (when an FX system
// legitimately emits no bars), `always` pages regardless (correct for 24/7 instruments). NOTE: a
// real per-instrument market calendar is §14 (broker adapter) territory; the weekend mode is the
// MVP stand-in and is wrong for 24/7 instruments — hence it is operator-selectable.
func (e *engine) checkLiveness(systems []ipc.System, now time.Time, settings ipc.Settings) {
	for i := range systems {
		s := &systems[i]
		// Only a Running system with at least one observed bar is assessable. (No bar yet -> a
		// just-started system; not wedged. A transition overlay -> not Running; skip.)
		if s.State != ipc.StateRunning || s.LastBarTS.IsZero() {
			delete(e.wedged, s.SystemID)
			continue
		}
		tf, ok := parseTimeframe(s.Timeframe)
		if !ok {
			continue // unknown cadence — cannot judge staleness
		}
		threshold := time.Duration(settings.WedgeMultiple)*tf + time.Duration(settings.WedgeGraceS)*time.Second
		age := now.Sub(s.LastBarTS)
		s.Wedged = age > threshold
		switch {
		case s.Wedged && !e.wedged[s.SystemID]:
			e.wedged[s.SystemID] = true
			msg := fmt.Sprintf("%s: WEDGED — no new bar for %s while PID %d alive (threshold %s)",
				s.SystemID, age.Round(time.Second), s.PID, threshold)
			switch {
			case settings.WedgeAlert == ipc.WedgeAlertSurface:
				log.Printf("engine: %s (alert suppressed — surface_only)", msg)
			case settings.WedgeAlert == ipc.WedgeAlertWeekend && isFXWeekend(now):
				log.Printf("engine: %s (alert suppressed — FX weekend)", msg)
			default:
				e.alert(msg)
			}
		case !s.Wedged && e.wedged[s.SystemID]:
			delete(e.wedged, s.SystemID)
			log.Printf("engine: %s recovered — bar fresh again", s.SystemID)
		}
	}
}

// parseTimeframe turns a timeframe label into its bar interval. Accepts MT5-style labels (M5, M15,
// H1, H4, D1, W1) and a bare integer meaning minutes (some layouts use "5"). Returns ok=false for
// anything it can't size (e.g. MN1 monthly), so liveness simply isn't judged for it.
func parseTimeframe(tf string) (time.Duration, bool) {
	tf = strings.TrimSpace(tf)
	if tf == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(tf); err == nil && n > 0 { // bare integer => minutes
		return time.Duration(n) * time.Minute, true
	}
	if len(tf) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(tf[1:])
	if err != nil || n <= 0 {
		return 0, false
	}
	switch tf[0] {
	case 'M', 'm':
		return time.Duration(n) * time.Minute, true
	case 'H', 'h':
		return time.Duration(n) * time.Hour, true
	case 'D', 'd':
		return time.Duration(n) * 24 * time.Hour, true
	case 'W', 'w':
		return time.Duration(n) * 7 * 24 * time.Hour, true
	}
	return 0, false
}

// isFXWeekend reports whether t falls in the FX market weekend (Fri 21:00 → Sun 21:00 UTC), the
// window in which an FX system legitimately produces no bars.
func isFXWeekend(t time.Time) bool {
	t = t.UTC()
	switch t.Weekday() {
	case time.Saturday:
		return true
	case time.Friday:
		return t.Hour() >= 21
	case time.Sunday:
		return t.Hour() < 21
	default:
		return false
	}
}

// launch spawns python run.py (own console) and records the spawn in the registry. The returned
// PID is the spawned process; the authoritative control PID is status.json's pid once written.
func (e *engine) launch(systemID, runPy string) (int, error) {
	cmd, err := proc.Spawn(e.cfg.Python, runPy)
	if err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	e.recordSpawn(systemID, pid)
	return pid, nil
}

// recordSpawn upserts the spawned system into process_registry.json (the re-attach source of
// truth, §12 — reconciled against ground truth, never trusted blindly).
func (e *engine) recordSpawn(systemID string, pid int) {
	reg, err := registry.Load(e.cfg.RegistryPath())
	if err != nil {
		log.Printf("engine: registry load: %v", err)
		return
	}
	strat, sym := "", ""
	if parts := strings.SplitN(systemID, "/", 3); len(parts) >= 2 {
		strat, sym = parts[0], parts[1]
	}
	reg.Entries[systemID] = registry.Entry{SystemID: systemID, PID: pid, Strategy: strat, Symbol: sym}
	if err := registry.Save(e.cfg.RegistryPath(), reg); err != nil {
		log.Printf("engine: registry save: %v", err)
	}
}

func isLiveOrInFlight(st ipc.State) bool {
	switch st {
	case ipc.StateRunning, ipc.StateStarting, ipc.StateStopping, ipc.StateRestarting:
		return true
	}
	return false
}

func scanCatalog(liveBase string) map[string]discovery.System {
	cat, _ := discovery.Scan(liveBase)
	m := make(map[string]discovery.System, len(cat))
	for _, c := range cat {
		m[c.Strategy+"/"+c.Symbol+"/"+c.Timeframe] = c
	}
	return m
}

func indexByID(systems []ipc.System) map[string]*ipc.System {
	m := make(map[string]*ipc.System, len(systems))
	for i := range systems {
		m[systems[i].SystemID] = &systems[i]
	}
	return m
}
