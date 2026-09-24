// Package migrate moves a box from the flat layout (<live_base>/<strategy>..., <log_base>/<strategy>...)
// to the account layout (<base>/<account>/<strategy>...) — the one-off cutover in
// ACCOUNT_LAYOUT_CHANGE_PLAN §5. It is a plan-then-apply tool: BuildPlan only reads, and prints what it
// would move; Apply performs the renames. It refuses while anything runs.
//
// The account of a flat system is taken from what the system itself uses today: the `.env.<account>`
// default hardcoded in its run.py. An operator mapping (--map name=account) overrides that, and is the
// only way to place a folder whose run.py names no single env file, or a log folder with no live system.
package migrate

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/okmich/signalfoundry-supervisor/internal/accounts"
	"github.com/okmich/signalfoundry-supervisor/internal/config"
	"github.com/okmich/signalfoundry-supervisor/internal/contract"
	"github.com/okmich/signalfoundry-supervisor/internal/proc"
	"github.com/okmich/signalfoundry-supervisor/internal/registry"
)

// archiveDir mirrors importsys.ArchiveDir (not imported, to keep this package free of the importer).
const archiveDir = ".archive"

// Move is one rename the plan performs.
type Move struct {
	Kind string // live | log | archive | file
	From string
	To   string
}

// Plan is everything BuildPlan found. Apply refuses unless Blockers is empty.
type Plan struct {
	Mapping  map[string]string // flat top-level name -> account
	Sources  map[string]string // flat name -> where its account came from ("run.py" | "--map" | "archive")
	Moves    []Move
	Unmapped []string // things left in place, with why
	Warnings []string
	Blockers []string
}

// envDefault finds the env file a run.py actually loads: its --env-file default or an _ENV_FILE constant.
// envMention is the fallback, any .env.<account> mention (help text can name a stale broker too).
var (
	envDefault = regexp.MustCompile(`(?:default=[^\n]*?|_ENV_FILE\s*=\s*)["']\.env\.([a-z0-9_]+\.[a-z0-9_]+)["']`)
	envMention = regexp.MustCompile(`\.env\.([a-z0-9_]+\.[a-z0-9_]+)`)
)

var magicSuffix = regexp.MustCompile(`_(\d+)\.json$`)

// BuildPlan inspects LIVE_BASE and LOG_BASE (read-only) and returns the migration plan. overrides maps a
// flat top-level folder name to its account and wins over what run.py says.
func BuildPlan(cfg config.Config, overrides map[string]string) (Plan, error) {
	p := Plan{Mapping: map[string]string{}, Sources: map[string]string{}}
	for name, acct := range overrides {
		if !accounts.Valid(acct) {
			return p, fmt.Errorf("--map %s=%s: %q is not an account name <broker>.<env>", name, acct, acct)
		}
	}
	p.checkNothingRuns(cfg)

	liveNames, err := flatDirs(cfg.LiveBase)
	if err != nil {
		return p, fmt.Errorf("read LIVE_BASE: %w", err)
	}
	logNames, err := flatDirs(cfg.LogBase)
	if err != nil {
		return p, fmt.Errorf("read LOG_BASE: %w", err)
	}
	archNames, _ := flatDirs(filepath.Join(cfg.LiveBase, archiveDir))

	// 1. The account of each flat live system.
	for _, name := range liveNames {
		if acct, ok := overrides[name]; ok {
			p.Mapping[name], p.Sources[name] = acct, "--map"
			continue
		}
		if found := envDefaults(filepath.Join(cfg.LiveBase, name)); len(found) == 1 {
			p.Mapping[name], p.Sources[name] = found[0], "run.py"
		} else if len(found) > 1 {
			p.Sources[name] = fmt.Sprintf("its run.py files name several accounts %v", found)
		}
	}
	// Everything not placed yet (log folders and archives of systems no longer live, or a live folder with
	// no run.py): an override, else the archived run.py. A live folder whose run.py was ambiguous stays
	// unplaced rather than trusting an older archive.
	for _, name := range append(append(append([]string{}, liveNames...), logNames...), archNames...) {
		if _, done := p.Mapping[name]; done || p.Sources[name] != "" {
			continue
		}
		if acct, ok := overrides[name]; ok {
			p.Mapping[name], p.Sources[name] = acct, "--map"
			continue
		}
		if found := envDefaults(filepath.Join(cfg.LiveBase, archiveDir, name)); len(found) == 1 {
			p.Mapping[name], p.Sources[name] = found[0], "archive"
		}
	}

	// 2. The moves.
	for _, name := range liveNames {
		acct, ok := p.Mapping[name]
		switch {
		case ok:
			p.add("live", filepath.Join(cfg.LiveBase, name), filepath.Join(cfg.LiveBase, acct, name))
		case p.Sources[name] != "":
			p.Unmapped = append(p.Unmapped, fmt.Sprintf("live %s: %s; use --map %s=<account>", name, p.Sources[name], name))
		default:
			p.Unmapped = append(p.Unmapped, fmt.Sprintf("live %s: no run.py names its .env.<account>; use --map %s=<account>", name, name))
		}
	}
	for _, name := range logNames {
		if acct, ok := p.Mapping[name]; ok {
			p.add("log", filepath.Join(cfg.LogBase, name), filepath.Join(cfg.LogBase, acct, name))
		} else {
			p.Unmapped = append(p.Unmapped, fmt.Sprintf("log %s: no live or archived system names its account; use --map %s=<account>", name, name))
		}
	}
	for _, name := range archNames {
		if acct, ok := p.Mapping[name]; ok {
			p.add("archive", filepath.Join(cfg.LiveBase, archiveDir, name), filepath.Join(cfg.LiveBase, archiveDir, acct, name))
		} else {
			p.Unmapped = append(p.Unmapped, fmt.Sprintf("archive %s: no account known; left in place", name))
		}
	}
	p.planRootFiles(cfg)

	for _, acct := range uniq(p.Mapping) {
		if !accounts.Load(cfg.EnvDir, acct).EnvFound {
			p.Warnings = append(p.Warnings, fmt.Sprintf("account %s has no env file %s: its systems will not start", acct, accounts.EnvPath(cfg.EnvDir, acct)))
		}
	}
	for _, m := range p.Moves {
		if _, err := os.Stat(m.To); err == nil {
			p.Blockers = append(p.Blockers, fmt.Sprintf("target already exists: %s", m.To))
		}
	}
	sort.Strings(p.Unmapped)
	return p, nil
}

// planRootFiles places the files a strategy kept at the log root (e.g. ctlpb_levels_<symbol>_<magic>.json)
// into its runner folder, by the magic number in the name.
func (p *Plan) planRootFiles(cfg config.Config) {
	entries, err := os.ReadDir(cfg.LogBase)
	if err != nil {
		return
	}
	owners := magicOwners(cfg.LiveBase, p.Mapping)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := magicSuffix.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if dest, ok := owners[m[1]]; ok {
			p.add("file", filepath.Join(cfg.LogBase, e.Name()), filepath.Join(cfg.LogBase, dest, e.Name()))
		} else {
			p.Unmapped = append(p.Unmapped, fmt.Sprintf("file %s: magic %s belongs to no live system; left in place", e.Name(), m[1]))
		}
	}
}

// Apply performs the plan's renames in order. It refuses if the plan has blockers, and re-checks that
// nothing runs, since the plan may have been built a while ago.
func (p Plan) Apply(cfg config.Config) error {
	if len(p.Blockers) > 0 {
		return fmt.Errorf("refusing: %d blocker(s)", len(p.Blockers))
	}
	var again Plan
	if again.checkNothingRuns(cfg); len(again.Blockers) > 0 {
		return fmt.Errorf("refusing: %s", again.Blockers[0])
	}
	for _, m := range p.Moves {
		if err := os.MkdirAll(filepath.Dir(m.To), 0o755); err != nil {
			return err
		}
		if err := os.Rename(m.From, m.To); err != nil {
			return fmt.Errorf("move %s -> %s: %w", m.From, m.To, err)
		}
	}
	return nil
}

// Print renders the plan for the operator.
func (p Plan) Print(w func(format string, a ...any)) {
	names := make([]string, 0, len(p.Mapping))
	for n := range p.Mapping {
		names = append(names, n)
	}
	sort.Strings(names)
	w("Account of each flat folder:\n")
	for _, n := range names {
		w("  %-32s -> %-16s (%s)\n", n, p.Mapping[n], p.Sources[n])
	}
	w("\nMoves (%d):\n", len(p.Moves))
	for _, m := range p.Moves {
		w("  [%-7s] %s\n            -> %s\n", m.Kind, m.From, m.To)
	}
	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		w("\n%s (%d):\n", title, len(items))
		for _, s := range items {
			w("  %s\n", s)
		}
	}
	section("Left in place", p.Unmapped)
	section("Warnings", p.Warnings)
	section("BLOCKERS — nothing will be moved until these are resolved", p.Blockers)
}

func (p *Plan) add(kind, from, to string) {
	p.Moves = append(p.Moves, Move{Kind: kind, From: from, To: to})
}

// checkNothingRuns blocks while any registered or status-reported runner is alive: moving a live
// system's folders from under it would strand its logs and its restart.
func (p *Plan) checkNothingRuns(cfg config.Config) {
	if reg, err := registry.Load(cfg.RegistryPath()); err == nil {
		for id, e := range reg.Entries {
			if e.PID != 0 && proc.Alive(e.PID) {
				p.Blockers = append(p.Blockers, fmt.Sprintf("system %s is running (pid %d) — stop the fleet first", id, e.PID))
			}
		}
	}
	matches, _ := filepath.Glob(filepath.Join(cfg.LogBase, "*", "status.json"))
	for _, path := range matches {
		if rs, err := contract.ReadStatus(path); err == nil && rs.State == "running" && rs.PID != 0 && proc.Alive(rs.PID) {
			p.Blockers = append(p.Blockers, fmt.Sprintf("%s reports running (pid %d) — stop the fleet first", path, rs.PID))
		}
	}
}

// flatDirs lists base's top-level folders that are not already account folders, dot-dirs or the
// reserved Account Admin state folder.
func flatDirs(base string) ([]string, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() && !strings.HasPrefix(n, ".") && !accounts.Valid(n) && n != "account-admin" {
			out = append(out, n)
		}
	}
	return out, nil
}

// envDefaults returns the distinct .env.<account> names referenced by the run.py files under dir.
func envDefaults(dir string) []string {
	set := map[string]bool{}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // unreadable entries are skipped
		}
		if d.IsDir() || d.Name() != "run.py" {
			return nil
		}
		if b, err := os.ReadFile(path); err == nil {
			matches := envDefault.FindAllStringSubmatch(string(b), -1)
			if len(matches) == 0 {
				matches = envMention.FindAllStringSubmatch(string(b), -1)
			}
			for _, m := range matches {
				set[m[1]] = true
			}
		}
		return nil
	})
	out := make([]string, 0, len(set))
	for a := range set {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// magicOwners maps each magic number in a mapped live system's config.json to <account>/<runner root>.
func magicOwners(liveBase string, mapping map[string]string) map[string]string {
	out := map[string]string{}
	for name, acct := range mapping {
		_ = filepath.WalkDir(filepath.Join(liveBase, name), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Name() != "config.json" {
				return nil //nolint:nilerr
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return nil //nolint:nilerr
			}
			var c struct {
				Strategy   *struct{ Magic json.Number }  `json:"strategy"`
				Strategies []struct{ Magic json.Number } `json:"strategies"`
			}
			if json.Unmarshal(b, &c) != nil {
				return nil
			}
			if c.Strategy != nil && c.Strategy.Magic != "" {
				out[c.Strategy.Magic.String()] = filepath.Join(acct, name)
			}
			for _, s := range c.Strategies {
				if s.Magic != "" {
					out[s.Magic.String()] = filepath.Join(acct, name)
				}
			}
			return nil
		})
	}
	return out
}

func uniq(m map[string]string) []string {
	set := map[string]bool{}
	for _, v := range m {
		set[v] = true
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
