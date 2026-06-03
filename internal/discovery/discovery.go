// Package discovery scans LIVE_BASE for the catalog of configured trading systems — the rows of
// the fleet view, independent of whether any are currently running.
package discovery

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// System is a configured system found on disk (its artefact dir holds run.py + config.json). A
// single-trader is one logical system; a multi-trader is ONE process (one PID) running N symbols,
// modeled as a single runner row (FLEET_SUPERVISOR_SPEC §16 — stopped as a unit).
type System struct {
	SystemID       string   // canonical id: <strategy>/<symbol>/<timeframe> (single) or <strategy>-multi (multi)
	Strategy       string   // strategy code
	RunnerStrategy string   // log runner-root folder: <strategy>, or <strategy>-multi for a multi-trader
	Symbol         string   // single-trader only
	Timeframe      string   // single-trader only (the path label)
	Symbols        []string // multi-trader only: the logical-system symbols it carries
	Multi          bool
	Dir            string // the artefact directory
	RunPy          string // path to run.py (what the Supervisor spawns)
}

// Scan walks liveBase for run.py files and classifies each by its sibling config.json: a non-empty
// `strategies[]` is a multi-trader (LOGGING_CONTRACT §7.1, mapped to the statutory <strategy>-multi
// runner root); anything else is a single-trader identified by its <strategy>/<symbol>/<timeframe>
// path (live and log paths are symmetric, so the path components match the runner's log folders).
func Scan(liveBase string) ([]System, error) {
	var out []System
	err := filepath.WalkDir(liveBase, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "run.py" {
			return nil //nolint:nilerr // skip unreadable entries, keep scanning
		}
		dir := filepath.Dir(path)
		if code, symbols, ok := readMultiConfig(dir); ok {
			runner := runnerStrategyRoot(code)
			out = append(out, System{
				SystemID: runner, Strategy: code, RunnerStrategy: runner,
				Symbols: symbols, Multi: true, Dir: dir, RunPy: path,
			})
			return nil
		}
		tf := filepath.Base(dir)
		sym := filepath.Base(filepath.Dir(dir))
		strat := filepath.Base(filepath.Dir(filepath.Dir(dir)))
		out = append(out, System{
			SystemID: strat + "/" + sym + "/" + tf, Strategy: strat, RunnerStrategy: strat,
			Symbol: sym, Timeframe: tf, Dir: dir, RunPy: path,
		})
		return nil
	})
	return out, err
}

// readMultiConfig reports a multi-trader iff dir/config.json has a non-empty strategies[]; it returns
// the strategy code and the per-symbol list. A missing/unreadable config or an empty strategies[]
// (a single-trader's config carries a singular `strategy` and an empty `strategies`) yields ok=false.
func readMultiConfig(dir string) (code string, symbols []string, ok bool) {
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return "", nil, false
	}
	var c struct {
		Strategies []struct {
			Name   string `json:"name"`
			Symbol string `json:"symbol"`
		} `json:"strategies"`
	}
	if json.Unmarshal(b, &c) != nil || len(c.Strategies) == 0 {
		return "", nil, false
	}
	for _, s := range c.Strategies {
		symbols = append(symbols, s.Symbol)
	}
	return c.Strategies[0].Name, symbols, true
}

// runnerStrategyRoot appends the statutory -multi suffix (idempotently), mirroring the framework's
// logging.identity.runner_strategy_root so the supervisor reads the same runner-root folder.
func runnerStrategyRoot(code string) string {
	if strings.HasSuffix(code, "-multi") {
		return code
	}
	return code + "-multi"
}
