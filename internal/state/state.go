// Package state reconciles config + status.json + PID liveness + inference freshness into the
// fleet model the engine publishes. The heart of startup is adoption (re-attach), never relaunch.
package state

import (
	"path/filepath"
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
// A row only adopts a runner's PID/state if its symbol is in the runner's logical_systems[] — one
// runner-root status.json covers several symbol/timeframe rows, so applying it blindly would let a
// row that the runner does NOT cover inherit a live PID and become a wrong stop/kill target.
//
// TODO: resolve the runner-root strategy (the `-multi` suffix) — a multi-trader reports under
// <strategy>-multi; this assumes the single-trader <strategy>. TODO: match coverage by timeframe
// too (status.json timeframe is an int, the row label is a string), blast-radius grouping (§7),
// wedged-vs-live grace, and registry reconciliation.
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
			LogPaths:  ipc.LogPaths{Status: statusPath, Text: filepath.Join(c.Dir, "z_system_log.log")},
		}
		if !c.Multi {
			s.LogPaths.Inference = contract.InferenceDir(cfg.LogBase, c.RunnerStrategy, c.Symbol, c.Timeframe)
		}
		// A multi-trader's status.json IS its own runner root, so the coverage gate (which protects a
		// single-trader row from a sibling's runner file) does not apply.
		if rs, err := contract.ReadStatus(statusPath); err == nil && (c.Multi || runnerCovers(rs, c.Symbol)) {
			s.PID, s.StartToken = rs.PID, rs.RunnerStartToken
			s.Broker, s.Account, s.SessionID = rs.Broker, rs.AccountID, rs.BrokerSessionID
			switch {
			case rs.State == "running" && proc.Alive(rs.PID):
				s.State = ipc.StateRunning
				// Per-symbol bar-age/wedged is shown for single-traders only; a multi-trader is a
				// single runner row, so runner-level liveness (stalest symbol) is deferred.
				if !c.Multi {
					if ts, ok := contract.LastBarTS(s.LogPaths.Inference); ok {
						s.LastBarTS = ts
						s.LastBarAgeS = time.Since(ts).Seconds()
					}
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
