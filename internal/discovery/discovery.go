// Package discovery scans LIVE_BASE for the catalog of configured trading systems — the rows of
// the fleet view, independent of whether any are currently running.
package discovery

import (
	"io/fs"
	"path/filepath"
)

// System is a configured system found on disk (its artefact dir holds run.py + config.json).
type System struct {
	Strategy  string
	Symbol    string
	Timeframe string
	Dir       string // the artefact directory
	RunPy     string // path to run.py (what the Supervisor spawns)
}

// Scan walks liveBase for <strategy>/<symbol>/<timeframe>/run.py.
func Scan(liveBase string) ([]System, error) {
	var out []System
	err := filepath.WalkDir(liveBase, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "run.py" {
			return nil //nolint:nilerr // skip unreadable entries, keep scanning
		}
		dir := filepath.Dir(path)
		tf := filepath.Base(dir)
		sym := filepath.Base(filepath.Dir(dir))
		strat := filepath.Base(filepath.Dir(filepath.Dir(dir)))
		out = append(out, System{Strategy: strat, Symbol: sym, Timeframe: tf, Dir: dir, RunPy: path})
		return nil
	})
	return out, err
}
