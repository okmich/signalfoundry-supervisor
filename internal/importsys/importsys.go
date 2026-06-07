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

	"github.com/okmich/signalfoundry-supervisor/internal/config"
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
	Multi       bool
	Strategy    string   // strategy code (the path label)
	Symbol      string   // single-trader only
	Symbols     []string // multi-trader only
	Timeframe   int      // single-trader only
	SystemID    string   // matches discovery's system_id exactly
	TargetDir   string   // absolute destination under LIVE_BASE
	WillArchive bool     // target already exists -> the current copy is archived first
}

type strategyEntry struct {
	Name      string `json:"name"`
	Symbol    string `json:"symbol"`
	Timeframe int    `json:"timeframe"`
}

type sysConfig struct {
	Name       string          `json:"name"`
	Strategy   *strategyEntry  `json:"strategy"`
	Strategies []strategyEntry `json:"strategies"`
}

// BuildPlan validates the source directory and resolves the canonical LIVE_BASE target. It returns a
// descriptive error (never panics) for any non-conforming input, and refuses if the resolved system
// is currently running — an artefact must not be swapped under a live PID.
func BuildPlan(cfg config.Config, sourceDir string) (Plan, error) {
	src := strings.TrimSpace(sourceDir)
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

	p := Plan{SourceDir: src}
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
		p.Multi, p.Strategy, p.SystemID = true, first.Name, root
		p.TargetDir = filepath.Join(cfg.LiveBase, root)
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
		p.SystemID = strat + "/" + sym + "/" + tf
		p.TargetDir = filepath.Join(cfg.LiveBase, strat, sym, tf)
	default:
		return Plan{}, fmt.Errorf("config.json classifies as neither single (a `strategy` object) nor multi (a non-empty `strategies[]`)")
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
	if p.WillArchive {
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
