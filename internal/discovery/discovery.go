// Package discovery scans LIVE_BASE for the catalog of configured trading systems — the rows of
// the fleet view, independent of whether any are currently running. LIVE_BASE is laid out by account:
// <live_base>/<account>/<strategy>/... (ACCOUNT_LAYOUT_CHANGE_PLAN).
package discovery

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/okmich/signalfoundry-supervisor/internal/accounts"
)

// System is a configured system found on disk (its artefact dir holds run.py + config.json). A
// single-trader is one logical system; a multi-trader is ONE process (one PID) running N symbols,
// modeled as a single runner row (FLEET_SUPERVISOR_SPEC §16 — stopped as a unit). Every system lives in
// an account folder, <live_base>/<account>/..., and its id carries that account as a prefix.
type System struct {
	SystemID       string   // canonical id: <account>/<strategy>/<symbol>/<timeframe> (single) or <account>/<strategy>-multi (multi)
	Account        string   // the account folder: the broker env-file stem, e.g. fxify.demo
	Strategy       string   // strategy code
	RunnerStrategy string   // log runner-root folder: <strategy>, or <strategy>-multi for a multi-trader
	Symbol         string   // single-trader only
	Timeframe      string   // single-trader only (the path label)
	Symbols        []string // multi-trader only: the logical-system symbols it carries
	Multi          bool
	Dir            string // the artefact directory
	RunPy          string // path to run.py (what the Supervisor spawns)
}

// Problem is something under LIVE_BASE that looks like a system but cannot be run: a run.py outside an
// account folder (the pre-account flat layout) or at a path discovery cannot classify. Problems are
// reported, never started.
type Problem struct {
	Path   string // relative to LIVE_BASE
	Reason string
}

// AdminStateDir is the Account Admin's live state folder inside an account (ACCOUNT_ADMIN_SPEC §3.1). It
// is never an artefact, so discovery does not look inside it.
const AdminStateDir = "account-admin"

// Scan reads liveBase's account folders (<broker>.<env>) and walks each for run.py files, classifying
// each by its sibling config.json: a non-empty `strategies[]` is a multi-trader (LOGGING_CONTRACT §7.1,
// mapped to the statutory <strategy>-multi runner root); anything else is a single-trader identified by
// its <strategy>/<symbol>/<timeframe> path below the account folder (live and log paths are symmetric,
// so the path components match the runner's log folders). Dot-dirs (the importer's .archive/.staging)
// are skipped. A top-level folder that is not an account but holds a run.py is reported as a Problem.
func Scan(liveBase string) ([]System, []Problem, error) {
	entries, err := os.ReadDir(liveBase)
	if err != nil {
		return nil, nil, err
	}
	var out []System
	var problems []Problem
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		dir := filepath.Join(liveBase, name)
		if !accounts.Valid(name) {
			if hasRunPy(dir) {
				problems = append(problems, Problem{Path: name,
					Reason: "not in an account folder (<broker>.<env>) — pre-account layout; run `supervisor migrate-layout`"})
			}
			continue
		}
		sys, probs := scanAccount(liveBase, name)
		out = append(out, sys...)
		problems = append(problems, probs...)
	}
	return out, problems, nil
}

func scanAccount(liveBase, account string) ([]System, []Problem) {
	accDir := filepath.Join(liveBase, account)
	var out []System
	var problems []Problem
	_ = filepath.WalkDir(accDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable entries, keep scanning
		}
		if d.IsDir() {
			if path != accDir && (strings.HasPrefix(d.Name(), ".") || d.Name() == AdminStateDir) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "run.py" {
			return nil
		}
		dir := filepath.Dir(path)
		rel, _ := filepath.Rel(accDir, dir)
		if code, symbols, ok := readMultiConfig(dir); ok {
			runner := runnerStrategyRoot(code)
			out = append(out, System{
				SystemID: account + "/" + runner, Account: account, Strategy: code, RunnerStrategy: runner,
				Symbols: symbols, Multi: true, Dir: dir, RunPy: path,
			})
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 3 {
			problems = append(problems, Problem{Path: filepath.ToSlash(filepath.Join(account, rel)),
				Reason: "run.py is neither at <strategy>/<symbol>/<timeframe> nor beside a multi-trader config.json"})
			return nil
		}
		strat, sym, tf := parts[0], parts[1], parts[2]
		out = append(out, System{
			SystemID: account + "/" + strat + "/" + sym + "/" + tf, Account: account, Strategy: strat,
			RunnerStrategy: strat, Symbol: sym, Timeframe: tf, Dir: dir, RunPy: path,
		})
		return nil
	})
	return out, problems
}

// hasRunPy reports whether any run.py exists under dir (dot-dirs skipped).
func hasRunPy(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found {
			return filepath.SkipAll
		}
		if d.IsDir() && path != dir && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == "run.py" {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
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
