// Package importsys provisions a trading-system artefact directory into LIVE_BASE in the canonical,
// discovery-compatible layout (FLEET_SUPERVISOR_SPEC §16, LOGGING_CONTRACT §7.1/§10). It validates
// that the source conforms (run.py + config.json), classifies single vs multi from config.json
// exactly as discovery does (MT5, IB and crypto configs adapted by sysconfig to what each runner
// logs), refuses to overwrite a running system, archives any existing copy, and
// installs via a staged atomic rename so a crash never leaves a half-written artefact in the live
// tree. It is the engine-independent core behind the TUI's import dialog: the engine consumes
// LIVE_BASE read-only and re-discovers the new system on its next tick — no command is needed.
package importsys

import (
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

	"github.com/okmich/signalfoundry-supervisor/internal/config"
	"github.com/okmich/signalfoundry-supervisor/internal/contract"
	"github.com/okmich/signalfoundry-supervisor/internal/proc"
	"github.com/okmich/signalfoundry-supervisor/internal/registry"
	"github.com/okmich/signalfoundry-supervisor/internal/sysconfig"
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
	Kind        sysconfig.Kind // mt5 / ib / crypto, detected from config.json
	Multi       bool
	Strategy    string   // strategy code (the path label)
	Symbol      string   // single-trader only: the symbol as the runner logs it
	Symbols     []string // multi-trader only: the logged symbols
	Timeframe   int      // single-trader only: minutes, as the runner logs it (the path label)
	SystemID    string   // matches discovery's system_id exactly
	TargetDir   string   // absolute destination under LIVE_BASE
	WillArchive bool     // target already exists -> the current copy is archived first
}

// BuildPlan validates the source directory and resolves the canonical LIVE_BASE target. It returns a
// descriptive error (never panics) for any non-conforming input, and refuses if the resolved system
// is currently running — an artefact must not be swapped under a live PID.
func BuildPlan(cfg config.Config, sourceDir string) (Plan, error) {
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
	// MT5, IB and crypto configs differ in how they write the symbol and the timeframe; sysconfig adapts each to
	// what its runner logs (classification identical to discovery's), and refuses a timeframe the runner could
	// not label.
	sc, err := sysconfig.Parse(raw)
	if err != nil {
		return Plan{}, err
	}
	symbolField := "symbol"
	if sc.Kind == sysconfig.KindCrypto {
		symbolField = "market_symbol"
	}

	p := Plan{SourceDir: src, Kind: sc.Kind}
	if sc.Multi {
		first := sc.Sleeves[0]
		if err := validateToken("strategy", first.Name); err != nil {
			return Plan{}, err
		}
		for i, s := range sc.Sleeves {
			if err := validateToken(fmt.Sprintf("strategies[%d].%s", i, symbolField), s.Symbol); err != nil {
				return Plan{}, err
			}
			p.Symbols = append(p.Symbols, s.Symbol)
		}
		root := runnerStrategyRoot(first.Name)
		p.Multi, p.Strategy, p.SystemID = true, first.Name, root
		p.TargetDir = filepath.Join(cfg.LiveBase, root)
	} else {
		s := sc.Sleeves[0]
		if err := validateToken("strategy", s.Name); err != nil {
			return Plan{}, err
		}
		if err := validateToken(symbolField, s.Symbol); err != nil {
			return Plan{}, err
		}
		// The <timeframe> label is MINUTES, as the runner's log folder: an MT5 H1 config (constant 16385) lands
		// in .../60, beside its logs, so the Supervisor finds its bars.
		strat, sym, tf := contract.PathSafe(s.Name), contract.PathSafe(s.Symbol), strconv.Itoa(s.TimeframeMinutes)
		p.Strategy, p.Symbol, p.Timeframe = s.Name, s.Symbol, s.TimeframeMinutes
		p.SystemID = strat + "/" + sym + "/" + tf
		p.TargetDir = filepath.Join(cfg.LiveBase, strat, sym, tf)
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
// refuses if the system is running. Returns the archive location. The system_id is the relative path
// under LIVE_BASE for both a single-trader (<strategy>/<symbol>/<timeframe>) and a multi-trader
// (<strategy>-multi), so it maps straight to the artefact dir.
func Decommission(cfg config.Config, systemID string) (archivedTo string, err error) {
	if strings.TrimSpace(systemID) == "" {
		return "", fmt.Errorf("no system id given")
	}
	if err := ensureNotRunning(cfg, systemID); err != nil {
		return "", err
	}
	rel := filepath.FromSlash(systemID)
	target := filepath.Join(cfg.LiveBase, rel)
	// Containment: an exported rename must never escape LIVE_BASE or hit LIVE_BASE itself, even if a
	// bogus id (".", "..", "../x") slips in — a system_id="." would otherwise archive the whole tree.
	if liveAbs, aerr := filepath.Abs(cfg.LiveBase); aerr == nil {
		if tAbs, terr := filepath.Abs(target); terr != nil {
			return "", terr
		} else if r, rerr := filepath.Rel(liveAbs, tAbs); rerr != nil || r == "." || strings.HasPrefix(r, "..") {
			return "", fmt.Errorf("invalid system id %q (resolves outside LIVE_BASE)", systemID)
		}
	}
	if info, statErr := os.Stat(target); statErr != nil || !info.IsDir() {
		return "", fmt.Errorf("system %q not found in LIVE_BASE (%s)", systemID, target)
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
