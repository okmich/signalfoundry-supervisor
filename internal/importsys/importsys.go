// Package importsys provisions a trading-system artefact directory into LIVE_BASE in the canonical,
// discovery-compatible layout (FLEET_SUPERVISOR_SPEC §16, LOGGING_CONTRACT §7.1/§10). It validates
// that the source conforms (run.py + config.json), classifies single vs multi from config.json
// exactly as discovery does, refuses to overwrite a running system, archives any existing copy, and
// installs via a staged atomic rename so a crash never leaves a half-written artefact in the live
// tree. It is the engine-independent core behind the TUI's import dialog: the engine consumes
// LIVE_BASE read-only and re-discovers the new system on its next tick — no command is needed.
package importsys

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/okmich/signalfoundry-supervisor/internal/accounts"
	"github.com/okmich/signalfoundry-supervisor/internal/config"
	"github.com/okmich/signalfoundry-supervisor/internal/discovery"
	"github.com/okmich/signalfoundry-supervisor/internal/proc"
	"github.com/okmich/signalfoundry-supervisor/internal/registry"
)

// ArchiveDir / StagingDir are the importer-owned subtrees under LIVE_BASE. They hold copies of run.py
// that are NOT live systems, so discovery.Scan skips every dot-directory to avoid mis-discovering them.
const (
	ArchiveDir = ".archive"
	StagingDir = ".staging"
)

// reservedPathChars mirrors okmich_quant_core logging.identity._RESERVED_PATH_CHARS: a token carrying
// any of these would raise IdentityTokenError and crash the runner at log-path construction, so we
// reject it here rather than deploy a system that cannot log.
var reservedPathChars = regexp.MustCompile(`[<>:"|?*\x00-\x1f]`)

// Plan is the validated outcome of inspecting a source dir — exactly what Apply will do. The TUI
// renders it for confirmation before anything is written.
type Plan struct {
	SourceDir   string
	Account     string // the account folder it is installed into (<broker>.<env>)
	Multi       bool
	Runner      bool     // installed directly under the account folder, named by config.json `runner`
	Strategy    string   // strategy code (the path label)
	Symbol      string   // single-trader only
	Symbols     []string // multi-trader only
	Timeframe   int      // single-trader only
	SystemID    string   // matches discovery's system_id exactly
	TargetDir   string   // absolute destination: LIVE_BASE/<account>/...
	WillArchive bool     // target already exists -> the current copy is archived first
}

type strategyEntry struct {
	Name      string `json:"name"`
	Symbol    string `json:"symbol"`
	Timeframe int    `json:"timeframe"`
}

type sysConfig struct {
	Name       string          `json:"name"`
	Runner     string          `json:"runner"` // a runner artefact, installed at <account>/<runner>
	Strategy   *strategyEntry  `json:"strategy"`
	Strategies []strategyEntry `json:"strategies"`
}

// BuildPlan validates the source directory and resolves the canonical target under the account folder,
// LIVE_BASE/<account>/... The account must have a broker env file (.env.<account> in ENV_DIR): the
// folder decides which account the system trades, so an import into an account the box cannot log into
// is refused here rather than at first start. It returns a descriptive error (never panics) for any
// non-conforming input, and refuses if the resolved system is currently running — an artefact must not
// be swapped under a live PID.
func BuildPlan(cfg config.Config, account, sourceDir string) (Plan, error) {
	if !accounts.Valid(account) {
		return Plan{}, fmt.Errorf("account %q is not an env-file stem <broker>.<env> (e.g. fxify.demo)", account)
	}
	if info := accounts.Load(cfg.EnvDir, account); !info.EnvFound {
		return Plan{}, fmt.Errorf("no broker env file for account %s (%s)", account, info.EnvFile)
	}
	// Strip control/NUL runes first: a clipboard paste can interleave \x00 bytes (a mis-decoded UTF-16
	// path), which would otherwise reach filepath.Abs below and fail with an opaque "invalid argument".
	src := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, sourceDir)
	// Tolerate Windows "Copy as path" quoting (it wraps the path in ") and stray whitespace.
	src = strings.TrimSpace(strings.Trim(strings.TrimSpace(src), `"`))
	if src == "" {
		return Plan{}, fmt.Errorf("no source path given")
	}
	src, err := filepath.Abs(src)
	if err != nil {
		return Plan{}, fmt.Errorf("resolve source path: %w", err)
	}
	if info, err := os.Stat(src); err != nil || !info.IsDir() {
		return Plan{}, fmt.Errorf("source is not a directory: %s", src)
	}
	// Import is FROM an external location INTO LIVE_BASE. Importing from within it would make the
	// stage/archive churn operate on the source itself (and a source under .archive/.staging is junk).
	if liveAbs, aerr := filepath.Abs(cfg.LiveBase); aerr == nil {
		if rel, rerr := filepath.Rel(liveAbs, src); rerr == nil && !strings.HasPrefix(rel, "..") {
			return Plan{}, fmt.Errorf("source %s is inside LIVE_BASE — import from an external location", src)
		}
	}
	// Convention gate: both run.py and config.json must be present.
	if _, err := os.Stat(filepath.Join(src, "run.py")); err != nil {
		return Plan{}, fmt.Errorf("not a system folder: missing run.py in %s", src)
	}
	raw, err := os.ReadFile(filepath.Join(src, "config.json"))
	if err != nil {
		return Plan{}, fmt.Errorf("not a system folder: missing config.json in %s", src)
	}
	var sc sysConfig
	if err := json.Unmarshal(raw, &sc); err != nil {
		return Plan{}, fmt.Errorf("config.json is not valid JSON: %w", err)
	}

	p := Plan{SourceDir: src, Account: account}
	switch {
	case len(sc.Strategies) > 0: // multi-trader (mirrors discovery's classification)
		first := sc.Strategies[0]
		if err := validateToken("strategy", first.Name); err != nil {
			return Plan{}, err
		}
		for i, s := range sc.Strategies {
			if err := validateToken(fmt.Sprintf("strategies[%d].symbol", i), s.Symbol); err != nil {
				return Plan{}, err
			}
			if s.Timeframe <= 0 {
				return Plan{}, fmt.Errorf("strategies[%d].timeframe must be a positive int, got %d", i, s.Timeframe)
			}
			p.Symbols = append(p.Symbols, s.Symbol)
		}
		root := runnerStrategyRoot(first.Name)
		p.Multi, p.Strategy, p.SystemID = true, first.Name, account+"/"+root
		p.TargetDir = filepath.Join(cfg.LiveBase, account, root)
	case sc.Strategy != nil: // single-trader
		s := sc.Strategy
		if err := validateToken("strategy", s.Name); err != nil {
			return Plan{}, err
		}
		if err := validateToken("symbol", s.Symbol); err != nil {
			return Plan{}, err
		}
		if s.Timeframe <= 0 {
			return Plan{}, fmt.Errorf("strategy.timeframe must be a positive int, got %d", s.Timeframe)
		}
		strat, sym, tf := pathSafe(s.Name), pathSafe(s.Symbol), strconv.Itoa(s.Timeframe)
		p.Strategy, p.Symbol, p.Timeframe = s.Name, s.Symbol, s.Timeframe
		p.SystemID = account + "/" + strat + "/" + sym + "/" + tf
		p.TargetDir = filepath.Join(cfg.LiveBase, account, strat, sym, tf)
	case sc.Runner != "": // a runner (e.g. the Account Admin): its folder is its identity and its log root
		if err := validateToken("runner", sc.Runner); err != nil {
			return Plan{}, err
		}
		name := pathSafe(sc.Runner)
		p.Runner, p.Strategy, p.SystemID = true, name, account+"/"+name
		p.TargetDir = filepath.Join(cfg.LiveBase, account, name)
	default:
		return Plan{}, fmt.Errorf("config.json classifies as neither single (a `strategy` object), multi (a non-empty `strategies[]`) nor a runner (`runner`)")
	}

	if _, err := os.Stat(p.TargetDir); err == nil {
		p.WillArchive = true
	}
	if err := ensureNotRunning(cfg, p.SystemID); err != nil {
		return Plan{}, err
	}
	return p, nil
}

// Apply installs the planned system: stage a clean copy, archive any existing target, then atomically
// rename the staged copy into place. Returns the archive location (empty if nothing was archived). The
// staged-then-rename order means an interrupted import never leaves a partial artefact at TargetDir.
func (p Plan) Apply(cfg config.Config) (archivedTo string, err error) {
	// Re-check liveness at apply time — the plan may have been confirmed seconds after it was built.
	if err := ensureNotRunning(cfg, p.SystemID); err != nil {
		return "", err
	}
	relUnderLive, err := filepath.Rel(cfg.LiveBase, p.TargetDir)
	if err != nil {
		return "", err
	}
	staging := filepath.Join(cfg.LiveBase, StagingDir, strings.ReplaceAll(relUnderLive, string(os.PathSeparator), "_"))
	if err := os.RemoveAll(staging); err != nil {
		return "", fmt.Errorf("clear staging: %w", err)
	}
	if err := copyTree(p.SourceDir, staging); err != nil {
		_ = os.RemoveAll(staging)
		return "", fmt.Errorf("stage copy: %w", err)
	}
	if p.isAdmin() {
		if err := carryAdminRuntime(p.TargetDir, staging); err != nil {
			_ = os.RemoveAll(staging)
			return "", fmt.Errorf("carry the Account Admin's governance files: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(p.TargetDir), 0o755); err != nil {
		_ = os.RemoveAll(staging)
		return "", err
	}
	// Re-stat rather than trusting the plan's WillArchive — the target may have appeared/vanished
	// between BuildPlan and confirmation.
	if _, statErr := os.Stat(p.TargetDir); statErr == nil {
		ts := time.Now().UTC().Format("20060102T150405Z")
		archivedTo = filepath.Join(cfg.LiveBase, ArchiveDir, relUnderLive, ts)
		if err := os.MkdirAll(filepath.Dir(archivedTo), 0o755); err != nil {
			_ = os.RemoveAll(staging)
			return "", err
		}
		if err := os.Rename(p.TargetDir, archivedTo); err != nil {
			_ = os.RemoveAll(staging)
			return "", fmt.Errorf("archive existing: %w", err)
		}
	}
	if err := os.Rename(staging, p.TargetDir); err != nil {
		return archivedTo, fmt.Errorf("install (rename staging->target): %w", err)
	}
	return archivedTo, nil
}

// Decommission retires an installed system: it archives the system's LIVE_BASE artefact dir to
// .archive (the inverse of an import) so discovery drops it from the fleet, and is reversible. It
// refuses if the system is running. Returns the archive location.
//
// The id must be one discovery currently reports, and the folder archived is that system's own artefact
// dir. An id is never turned into a path by itself, so a crafted or stale id ("fxify.demo/../deriv.live",
// "fxify.demo/.", a strategy folder holding several systems) can never archive an account, another
// account's systems, or anything outside one system.
func Decommission(cfg config.Config, systemID string) (archivedTo string, err error) {
	if strings.TrimSpace(systemID) == "" {
		return "", fmt.Errorf("no system id given")
	}
	cat, _, err := discovery.Scan(cfg.LiveBase)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("scan LIVE_BASE: %w", err)
	}
	var target string
	for _, s := range cat {
		if s.SystemID == systemID {
			target = s.Dir
			break
		}
	}
	if target == "" {
		return "", fmt.Errorf("system %q not found in LIVE_BASE", systemID)
	}
	if filepath.Base(target) == accounts.AdminFolder {
		return "", fmt.Errorf("%s is the Account Admin: decommissioning it would take the account out of governance — "+
			"follow the out-of-governance runbook (ACCOUNT_ADMIN_SPEC §8.3) instead", systemID)
	}
	if err := ensureNotRunning(cfg, systemID); err != nil {
		return "", err
	}
	// Defense in depth: the artefact dir sits inside an account folder, below LIVE_BASE.
	rel, err := filepath.Rel(cfg.LiveBase, target)
	if acct, rest, _ := strings.Cut(filepath.ToSlash(rel), "/"); err != nil || !accounts.Valid(acct) || rest == "" {
		return "", fmt.Errorf("system %q resolves outside an account folder (%s)", systemID, target)
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	archivedTo = filepath.Join(cfg.LiveBase, ArchiveDir, rel, ts)
	if err := os.MkdirAll(filepath.Dir(archivedTo), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(target, archivedTo); err != nil {
		return "", fmt.Errorf("archive (rename target->archive): %w", err)
	}
	return archivedTo, nil
}

// isAdmin reports whether the plan installs an account's Account Admin.
func (p Plan) isAdmin() bool { return p.Runner && p.Strategy == accounts.AdminFolder }

// carryAdminRuntime makes the staged Admin copy hold exactly the governance files of the installed one: it
// drops any the source brought (a directive is written only by the Admin that governs the account, never
// shipped) and copies in the current directive, state and request inbox. writer.lock is not carried: the
// import refuses while the Admin runs, so no lock is held. With no installed copy (first deployment) the
// staged copy simply starts without them.
func carryAdminRuntime(installed, staging string) error {
	for _, name := range accounts.AdminRuntime {
		if err := os.RemoveAll(filepath.Join(staging, name)); err != nil {
			return err
		}
	}
	for _, name := range accounts.AdminRuntime {
		if name == "writer.lock" {
			continue
		}
		src := filepath.Join(installed, name)
		info, err := os.Stat(src)
		if os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if info.IsDir() {
			err = copyTree(src, filepath.Join(staging, name))
		} else {
			err = copyFile(src, filepath.Join(staging, name))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ensureNotRunning refuses if the registry shows the system_id bound to a live PID.
func ensureNotRunning(cfg config.Config, systemID string) error {
	reg, err := registry.Load(cfg.RegistryPath())
	if err != nil {
		return fmt.Errorf("read registry: %w", err)
	}
	if e, ok := reg.Entries[systemID]; ok && e.PID != 0 && proc.Alive(e.PID) {
		return fmt.Errorf("system %q is running (pid %d); stop it before re-importing", systemID, e.PID)
	}
	return nil
}

// validateToken rejects a strategy/symbol that cannot be a safe single path component (mirrors
// identity._validate_identity_token): empty, '.'/'..' traversal, or a reserved filesystem character.
func validateToken(kind, value string) error {
	s := strings.TrimSpace(value)
	if s == "" || s == "." || s == ".." {
		return fmt.Errorf("%s %q is empty or a path-traversal component ('.'/'..')", kind, value)
	}
	if reservedPathChars.MatchString(s) {
		return fmt.Errorf("%s %q contains a reserved filesystem character", kind, value)
	}
	return nil
}

// pathSafe mirrors identity._path_safe: replace path separators only and trim — internal spaces are
// intentionally preserved so the live folder label matches the framework's log folder byte-for-byte.
func pathSafe(s string) string {
	return strings.TrimSpace(strings.NewReplacer("/", "_", "\\", "_").Replace(s))
}

// runnerStrategyRoot mirrors discovery.runnerStrategyRoot / identity.runner_strategy_root: append the
// statutory -multi suffix idempotently.
func runnerStrategyRoot(code string) string {
	if strings.HasSuffix(code, "-multi") {
		return code
	}
	return code + "-multi"
}

// copyTree copies src into dst, omitting build/VCS noise and the framework's transient z_*.log files.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel != "." && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if skipFile(d.Name()) {
			return nil
		}
		return copyFile(path, filepath.Join(dst, rel))
	})
}

func skipDir(name string) bool {
	return name == "__pycache__" || name == ".git" || name == ArchiveDir || name == StagingDir
}

func skipFile(name string) bool {
	if strings.HasSuffix(name, ".pyc") {
		return true
	}
	// z_system_log_*.log / z_ib_system_log_*.log — the runner's transient per-process logs (run.py).
	return strings.HasPrefix(name, "z_") && strings.HasSuffix(name, ".log")
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, cerr := io.Copy(out, in)
	if cerr2 := out.Close(); cerr2 != nil && cerr == nil {
		cerr = cerr2
	}
	return cerr
}
