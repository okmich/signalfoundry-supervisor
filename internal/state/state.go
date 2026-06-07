// Package state reconciles config + status.json + PID liveness + inference freshness into the
// fleet model the engine publishes. The heart of startup is adoption (re-attach), never relaunch.
package state

import (
	"path/filepath"
	"strconv"
	"time"

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
// TODO: match coverage by timeframe too (status.json timeframe is an int, the row label a string) —
// today runnerCovers gates on symbol only.
func Reconcile(cfg config.Config) []ipc.System {
	cat, _ := discovery.Scan(cfg.LiveBase)
	systems := make([]ipc.System, 0, len(cat))
	for _, c := range cat {
		statusPath := contract.StatusPath(cfg.LogBase, c.RunnerStrategy)
		s := ipc.System{
			SystemID:  c.SystemID,
			Strategy:  c.Strategy,
			Symbol:    c.Symbol,
			Timeframe: c.Timeframe,
			Multi:     c.Multi,
			Symbols:   c.Symbols,
			State:     ipc.StateStopped,
			// TODO(unverified): the framework defines no standard text-log file — `setup_logging` is
			// never called and `z_system_log.log` does not exist in it. Only the inference JSONL is
			// contract-backed. This Text path is provisional (the details left pane degrades to "(no
			// log yet)" when absent); confirm the real layout against live run artifacts before relying
			// on it — it may instead be the §15 raw-stdout ops capture.
			LogPaths: ipc.LogPaths{Status: statusPath, Text: filepath.Join(c.Dir, "z_system_log.log")},
		}
		if !c.Multi {
			s.LogPaths.Inference = contract.InferenceDir(cfg.LogBase, c.RunnerStrategy, c.Symbol, c.Timeframe)
		}
		// A multi-trader's status.json IS its own runner root, so the coverage gate (which protects a
		// single-trader row from a sibling's runner file) does not apply.
		if rs, err := contract.ReadStatus(statusPath); err == nil && (c.Multi || runnerCovers(rs, c.Symbol)) {
			s.PID, s.StartToken = rs.PID, rs.RunnerStartToken
			s.Broker, s.Account, s.SessionID = rs.Broker, rs.AccountID, rs.BrokerSessionID
			s.StartedAt = rs.StartedAt
			switch {
			case rs.State == "running" && proc.Alive(rs.PID):
				s.State = ipc.StateRunning
				if c.Multi {
					// Runner-level liveness: one leg per logical system (its own symbol + timeframe),
					// taken from status.json's logical_systems[] — the authoritative symbol/timeframe
					// map, since the config.json discovery reads carries no timeframe. The engine
					// judges the runner wedged if ANY leg is stale past its OWN cadence (§15); the
					// row's bar-age shows the STALEST leg so the fleet view flags the worst symbol.
					s.Legs = multiLegs(cfg.LogBase, c.RunnerStrategy, rs.LogicalSystems)
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
				} else if ts, ok := contract.LastBarTS(s.LogPaths.Inference); ok {
					s.LastBarTS = ts
					s.LastBarAgeS = time.Since(ts).Seconds()
				}
			case rs.State == "running" && !proc.Alive(rs.PID):
				s.State = ipc.StateOrphanSuspected
			case rs.State == "stopped":
				s.State = ipc.StateStoppedByOp
			}
		}
		systems = append(systems, s)
	}
	return systems
}

// multiLegs builds one liveness leg per logical system of a multi-trader, reading each symbol's own
// inference dir at its own timeframe. status.json carries the timeframe as integer minutes, which is
// both the path label (e.g. "5") and what the engine's parseTimeframe expects. A leg with no bar yet
// has a zero LastBarTS and is simply not judged for wedging (§15).
func multiLegs(logBase, runnerStrategy string, ls []contract.LogicalSystem) []ipc.SystemLeg {
	if len(ls) == 0 {
		return nil
	}
	legs := make([]ipc.SystemLeg, 0, len(ls))
	for _, l := range ls {
		tf := strconv.Itoa(l.Timeframe)
		dir := contract.InferenceDir(logBase, runnerStrategy, l.Symbol, tf)
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
