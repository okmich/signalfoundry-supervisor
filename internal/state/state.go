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
		runnerStrategy := c.Strategy // TODO: + "-multi" for a multi-trader runner
		statusPath := contract.StatusPath(cfg.LogBase, runnerStrategy)
		infDir := contract.InferenceDir(cfg.LogBase, runnerStrategy, c.Symbol, c.Timeframe)

		s := ipc.System{
			SystemID:  c.Strategy + "/" + c.Symbol + "/" + c.Timeframe,
			Strategy:  c.Strategy,
			Symbol:    c.Symbol,
			Timeframe: c.Timeframe,
			State:     ipc.StateStopped,
			LogPaths: ipc.LogPaths{
				Status:    statusPath,
				Inference: infDir,
				Text:      filepath.Join(c.Dir, "z_system_log.log"),
			},
		}
		if rs, err := contract.ReadStatus(statusPath); err == nil && runnerCovers(rs, c.Symbol) {
			s.PID, s.StartToken = rs.PID, rs.RunnerStartToken
			s.Broker, s.Account, s.SessionID = rs.Broker, rs.AccountID, rs.BrokerSessionID
			switch {
			case rs.State == "running" && proc.Alive(rs.PID):
				s.State = ipc.StateRunning
				if ts, ok := contract.LastBarTS(infDir); ok {
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
