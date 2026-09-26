// Package state reconciles config + status.json + PID liveness + inference freshness into the
// fleet model the engine publishes. The heart of startup is adoption (re-attach), never relaunch.
package state

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/okmich/signalfoundry-supervisor/internal/accounts"
	"github.com/okmich/signalfoundry-supervisor/internal/config"
	"github.com/okmich/signalfoundry-supervisor/internal/contract"
	"github.com/okmich/signalfoundry-supervisor/internal/discovery"
	"github.com/okmich/signalfoundry-supervisor/internal/ipc"
	"github.com/okmich/signalfoundry-supervisor/internal/proc"
)

// Reconcile builds the live fleet picture: discover the catalog, read each system's status.json,
// verify the PID, classify state. It NEVER relaunches (adoption only, FLEET_SUPERVISOR_SPEC §12).
//
// A single-trader row only adopts a runner's PID/state if its symbol is in the runner's
// logical_systems[] — one runner-root status.json covers several symbol/timeframe rows, so applying
// it blindly would let a row the runner does NOT cover inherit a live PID and become a wrong
// stop/kill target. A multi-trader is its own runner root (<strategy>-multi, resolved by discovery)
// and is adopted as a unit, with one liveness leg per logical system (runner-level liveness, §15).
//
// Every system lives in an account folder; its status and inference paths are under
// <log_base>/<account>/, and a running system is checked against that folder (accountMismatch). It also
// returns the problems the operator must see: discovery's (a run.py outside an account folder) and any
// account folder whose .env.<account> is missing (its systems cannot start).
//
// TODO: match coverage by timeframe too (status.json timeframe is an int, the row label a string) —
// today runnerCovers gates on symbol only.
func Reconcile(cfg config.Config) ([]ipc.System, []ipc.Problem) {
	cat, found, _ := discovery.Scan(cfg.LiveBase)
	envs := accounts.NewCache(cfg.EnvDir)
	var problems []ipc.Problem
	for _, p := range found {
		problems = append(problems, ipc.Problem{Path: p.Path, Reason: p.Reason})
	}
	missing := map[string]bool{}
	systems := make([]ipc.System, 0, len(cat))
	for _, c := range cat {
		env := envs.Get(c.Account)
		if !missing[c.Account] {
			if keys := env.MissingSessionKeys(); !env.EnvFound || len(keys) > 0 {
				missing[c.Account] = true
				reason := fmt.Sprintf("no broker env file %s — its systems cannot start", env.EnvFile)
				if env.EnvFound {
					reason = fmt.Sprintf("%s lacks %s — its systems cannot start", env.EnvFile, strings.Join(keys, ", "))
				}
				problems = append(problems, ipc.Problem{Path: c.Account, Reason: reason})
			}
		}
		statusPath := contract.StatusPath(cfg.LogBase, c.Account, c.RunnerStrategy)
		s := ipc.System{
			SystemID:  c.SystemID,
			Account:   c.Account,
			Strategy:  c.Strategy,
			Symbol:    c.Symbol,
			Timeframe: c.Timeframe,
			Multi:     c.Multi,
			Symbols:   c.Symbols,
			State:     ipc.StateStopped,
			// Text log: the runner writes z_*_log_<ts>.log into the runner-root log dir under LOG_BASE
			// (okmich_quant_core.setup_text_logger), beside status.json. Resolve the newest each tick;
			// "" -> the details left pane degrades to "(no log yet)".
			LogPaths: ipc.LogPaths{Status: statusPath, Text: newestTextLog(filepath.Dir(statusPath))},
		}
		runnerRoot := c.Multi || c.Runner // one status.json at its own root; legs from its logical_systems[]
		if !runnerRoot {
			s.LogPaths.Inference = contract.InferenceDir(cfg.LogBase, c.Account, c.RunnerStrategy, c.Symbol, c.Timeframe)
		}
		// A multi-trader's status.json IS its own runner root, so the coverage gate (which protects a
		// single-trader row from a sibling's runner file) does not apply.
		if rs, err := contract.ReadStatus(statusPath); err == nil && (runnerRoot || runnerCovers(rs, c.Symbol)) {
			s.PID, s.StartToken = rs.PID, rs.RunnerStartToken
			s.Broker, s.AccountID, s.SessionID = rs.Broker, rs.AccountID, rs.BrokerSessionID
			s.StartedAt = rs.StartedAt
			switch {
			case rs.State == "running" && proc.Alive(rs.PID):
				s.State = ipc.StateRunning
				s.AccountMismatch = accountMismatch(c.Account, env, rs)
				if runnerRoot {
					// Runner-level liveness: one leg per logical system (its own symbol + timeframe),
					// taken from status.json's logical_systems[] — the authoritative symbol/timeframe
					// map, since the config.json discovery reads carries no timeframe. The engine
					// judges the runner wedged if ANY leg is stale past its OWN cadence (§15); the
					// row's bar-age shows the STALEST leg so the fleet view flags the worst symbol.
					s.Legs = multiLegs(cfg.LogBase, c.Account, c.RunnerStrategy, rs.LogicalSystems)
					for _, leg := range s.Legs {
						if leg.LastBarTS.IsZero() {
							continue
						}
						if s.LastBarTS.IsZero() || leg.LastBarTS.Before(s.LastBarTS) {
							s.LastBarTS = leg.LastBarTS
						}
					}
					if !s.LastBarTS.IsZero() {
						s.LastBarAgeS = time.Since(s.LastBarTS).Seconds()
					}
				} else {
					// Single-trader = a multi-of-one: publish ONE leg so the details view (and every
					// other consumer) takes the SAME path as a multi-trader, not a single-only branch.
					// Behavioral single/multi differences key on s.Multi, never on len(Legs).
					leg := ipc.SystemLeg{Symbol: c.Symbol, Timeframe: c.Timeframe, Inference: s.LogPaths.Inference}
					if ts, ok := contract.LastBarTS(s.LogPaths.Inference); ok {
						leg.LastBarTS = ts
						s.LastBarTS = ts
						s.LastBarAgeS = time.Since(ts).Seconds()
					}
					s.Legs = []ipc.SystemLeg{leg}
				}
			case rs.State == "running" && !proc.Alive(rs.PID):
				s.State = ipc.StateOrphanSuspected
			case rs.State == "stopped":
				s.State = ipc.StateStoppedByOp
			}
		}
		systems = append(systems, s)
	}
	sort.SliceStable(problems, func(i, j int) bool { return problems[i].Path < problems[j].Path })
	return systems, problems
}

// accountMismatch checks a running system against its account folder (ACCOUNT_LAYOUT_CHANGE_PLAN D6): the
// account its status.json was written under, and the login its terminal is actually on (MT5BrokerSession
// reads it from the terminal) against the env file's LOGIN_ID. "" when they agree or the env file has no
// LOGIN_ID to compare (e.g. an IB account).
func accountMismatch(folder string, env accounts.Info, rs contract.RunnerStatus) string {
	switch {
	case rs.Account == "":
		return "runner predates the account layout (no account in status.json)"
	case rs.Account != folder:
		return fmt.Sprintf("runner logs under account %s, but lives in %s", rs.Account, folder)
	case env.Login != "" && rs.AccountID != "" && rs.AccountID != env.Login:
		return fmt.Sprintf("terminal is logged into %s, but .env.%s has LOGIN_ID %s", rs.AccountID, folder, env.Login)
	}
	return ""
}

// newestTextLog returns the most recently written z_*.log in the runner-root log dir, or "" if none exists
// yet: the runner's own text log (z_system_log_<ts>.log, okmich_quant_core.setup_text_logger) or the console
// capture the engine made of its launch (z_console_<ts>.log). By modification time, so a launch that died
// before writing its own log shows its console capture, not an older run's log.
func newestTextLog(runnerRootDir string) string {
	matches, _ := filepath.Glob(filepath.Join(runnerRootDir, "z_*.log"))
	newest, newestT := "", time.Time{}
	for _, p := range matches {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if t := info.ModTime(); newest == "" || t.After(newestT) || (t.Equal(newestT) && filepath.Base(p) > filepath.Base(newest)) {
			newest, newestT = p, t
		}
	}
	return newest
}

// multiLegs builds one liveness leg per logical system of a multi-trader, reading each symbol's own
// inference dir at its own timeframe. status.json carries the timeframe as integer minutes, which is
// both the path label (e.g. "5") and what the engine's parseTimeframe expects. A leg with no bar yet
// has a zero LastBarTS and is simply not judged for wedging (§15).
func multiLegs(logBase, account, runnerStrategy string, ls []contract.LogicalSystem) []ipc.SystemLeg {
	if len(ls) == 0 {
		return nil
	}
	legs := make([]ipc.SystemLeg, 0, len(ls))
	for _, l := range ls {
		tf := strconv.Itoa(l.Timeframe)
		dir := contract.InferenceDir(logBase, account, runnerStrategy, l.Symbol, tf)
		leg := ipc.SystemLeg{Symbol: l.Symbol, Timeframe: tf, Inference: dir}
		if ts, ok := contract.LastBarTS(dir); ok {
			leg.LastBarTS = ts
		}
		legs = append(legs, leg)
	}
	return legs
}

// runnerCovers reports whether the runner's status.json claims this symbol. An empty logical_systems
// (a minimal/older status.json) falls back to "covered" so single-trader behavior is unchanged.
func runnerCovers(rs contract.RunnerStatus, symbol string) bool {
	if len(rs.LogicalSystems) == 0 {
		return true
	}
	for _, ls := range rs.LogicalSystems {
		if ls.Symbol == symbol {
			return true
		}
	}
	return false
}
