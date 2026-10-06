package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/okmich/signalfoundry-supervisor/internal/config"
	"github.com/okmich/signalfoundry-supervisor/internal/registry"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runPy(env string) string {
	return fmt.Sprintf("p.add_argument(\"--env-file\", default=str(_ENV_DIR / \".env.%s\"))\n", env)
}

// flatBox is today's box in miniature: two flat multi-traders on two accounts, their logs, an archive,
// a log folder with only an archived system, a stray log folder, and a levels file at the log root.
func flatBox(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	cfg := config.Config{LiveBase: filepath.Join(root, "live"), LogBase: filepath.Join(root, "logs"),
		EnvDir: filepath.Join(root, "env"), StateDir: filepath.Join(root, "state")}
	write(t, filepath.Join(cfg.EnvDir, ".env.fxify.demo"), "LOGIN_ID=1\n")
	write(t, filepath.Join(cfg.EnvDir, ".env.icmarkets.demo"), "LOGIN_ID=2\n")

	write(t, filepath.Join(cfg.LiveBase, "ctlpb_raw-multi", "run.py"), runPy("fxify.demo"))
	write(t, filepath.Join(cfg.LiveBase, "ctlpb_raw-multi", "config.json"), `{"strategies":[{"name":"ctlpb_raw","symbol":"EURUSD.r","magic":20260827101}]}`)
	write(t, filepath.Join(cfg.LiveBase, "propfolio_trend-multi", "run.py"), runPy("icmarkets.demo"))
	write(t, filepath.Join(cfg.LiveBase, ".archive", "ctlpb_raw-multi", "20260902T101320Z", "run.py"), runPy("fxify.demo"))
	write(t, filepath.Join(cfg.LiveBase, ".archive", "rsi2_mean_reversion", "EURUSD", "5", "20260801T000000Z", "run.py"), runPy("deriv.demo"))

	write(t, filepath.Join(cfg.LogBase, "ctlpb_raw-multi", "status.json"), `{"state":"stopped","pid":1}`)
	write(t, filepath.Join(cfg.LogBase, "propfolio_trend-multi", "z_system_log_1.log"), "")
	write(t, filepath.Join(cfg.LogBase, "rsi2_mean_reversion", "status.json"), `{"state":"stopped"}`)
	write(t, filepath.Join(cfg.LogBase, "mystery", "x.log"), "")
	write(t, filepath.Join(cfg.LogBase, "ctlpb_levels_EURUSD.r_20260827101.json"), "{}")
	write(t, filepath.Join(cfg.LogBase, "ctlpb_levels_GBPUSD.r_999.json"), "{}")
	return cfg
}

func TestBuildPlanMapsFromRunPyAndArchives(t *testing.T) {
	cfg := flatBox(t)
	p, err := BuildPlan(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"ctlpb_raw-multi": "fxify.demo", "propfolio_trend-multi": "icmarkets.demo", "rsi2_mean_reversion": "deriv.demo"}
	for n, a := range want {
		if p.Mapping[n] != a {
			t.Errorf("mapping[%s] = %q, want %q", n, p.Mapping[n], a)
		}
	}
	moves := map[string]string{}
	for _, m := range p.Moves {
		rel, _ := filepath.Rel(filepath.Dir(cfg.LiveBase), m.To)
		moves[filepath.ToSlash(rel)] = m.Kind
	}
	for _, to := range []string{
		"live/fxify.demo/ctlpb_raw-multi", "live/icmarkets.demo/propfolio_trend-multi",
		"logs/fxify.demo/ctlpb_raw-multi", "logs/icmarkets.demo/propfolio_trend-multi", "logs/deriv.demo/rsi2_mean_reversion",
		"live/.archive/fxify.demo/ctlpb_raw-multi", "live/.archive/deriv.demo/rsi2_mean_reversion",
		"logs/fxify.demo/ctlpb_raw-multi/ctlpb_levels_EURUSD.r_20260827101.json",
	} {
		if _, ok := moves[to]; !ok {
			t.Errorf("missing move to %s; moves = %v", to, moves)
		}
	}
	if len(p.Moves) != 8 {
		t.Errorf("want 8 moves, got %d: %v", len(p.Moves), moves)
	}
	left := strings.Join(p.Unmapped, "\n")
	if !strings.Contains(left, "log mystery") || !strings.Contains(left, "GBPUSD.r_999") {
		t.Errorf("unmapped should report the stray log folder and the orphan levels file:\n%s", left)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "deriv.demo") {
		t.Errorf("want one warning for deriv.demo's missing env file, got %v", p.Warnings)
	}
	if len(p.Blockers) != 0 {
		t.Errorf("unexpected blockers %v", p.Blockers)
	}
}

func TestApplyMovesAndIsIdempotentlyEmpty(t *testing.T) {
	cfg := flatBox(t)
	p, err := BuildPlan(cfg, map[string]string{"mystery": "fxify.demo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(cfg.LiveBase, "fxify.demo", "ctlpb_raw-multi", "run.py"),
		filepath.Join(cfg.LogBase, "fxify.demo", "ctlpb_raw-multi", "status.json"),
		filepath.Join(cfg.LogBase, "fxify.demo", "ctlpb_raw-multi", "ctlpb_levels_EURUSD.r_20260827101.json"),
		filepath.Join(cfg.LogBase, "fxify.demo", "mystery", "x.log"),
		filepath.Join(cfg.LiveBase, ".archive", "deriv.demo", "rsi2_mean_reversion", "EURUSD", "5", "20260801T000000Z", "run.py"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected %s after apply: %v", path, err)
		}
	}
	again, err := BuildPlan(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Moves) != 0 {
		t.Errorf("a migrated box should plan no moves, got %+v", again.Moves)
	}
}

func TestBlocksWhileRunningAndOnConflicts(t *testing.T) {
	cfg := flatBox(t)
	write(t, filepath.Join(cfg.LiveBase, "fxify.demo", "ctlpb_raw-multi", "run.py"), "") // conflict
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := registry.Registry{Entries: map[string]registry.Entry{"x": {SystemID: "x", PID: os.Getpid()}}}
	if err := registry.Save(cfg.RegistryPath(), reg); err != nil {
		t.Fatal(err)
	}
	p, err := BuildPlan(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	blockers := strings.Join(p.Blockers, "\n")
	if !strings.Contains(blockers, "is running") || !strings.Contains(blockers, "already exists") {
		t.Fatalf("want running + conflict blockers, got:\n%s", blockers)
	}
	if err := p.Apply(cfg); err == nil {
		t.Fatal("apply must refuse with blockers")
	}
	if _, err := os.Stat(filepath.Join(cfg.LiveBase, "propfolio_trend-multi")); err != nil {
		t.Error("nothing may move when blocked")
	}
}

func TestBadOverrideRejected(t *testing.T) {
	if _, err := BuildPlan(flatBox(t), map[string]string{"x": "fxify"}); err == nil {
		t.Fatal("a non-account override must be rejected")
	}
}

// A run.py's --env-file default wins over other .env mentions (help text naming a stale broker).
func TestEnvDefaultBeatsMentions(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "run.py"), `help="(default: $OKMICH_QUANT_ENV_DIR/.env.fxpig.demo)"`+"\n"+
		`parser.add_argument("--env-file", type=str, default=str(_ENV_DIR / ".env.fxify.demo"), help="x .env.fxpig.demo")`+"\n")
	if got := envDefaults(dir); len(got) != 1 || got[0] != "fxify.demo" {
		t.Fatalf("envDefaults = %v, want [fxify.demo]", got)
	}
	write(t, filepath.Join(dir, "b", "run.py"), `_ENV_FILE = ".env.icmarkets.demo"`+"\n")
	if got := envDefaults(filepath.Join(dir, "b")); len(got) != 1 || got[0] != "icmarkets.demo" {
		t.Fatalf("envDefaults(_ENV_FILE) = %v", got)
	}
}

// A re-run after an interrupted apply (live folders moved, logs not) still places the log folders and the
// root-level state files by where their systems already are.
func TestResumesAfterPartialApply(t *testing.T) {
	cfg := flatBox(t)
	for _, n := range []string{"ctlpb_raw-multi", "propfolio_trend-multi"} {
		acct := map[string]string{"ctlpb_raw-multi": "fxify.demo", "propfolio_trend-multi": "icmarkets.demo"}[n]
		if err := os.MkdirAll(filepath.Join(cfg.LiveBase, acct), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(cfg.LiveBase, n), filepath.Join(cfg.LiveBase, acct, n)); err != nil {
			t.Fatal(err)
		}
	}
	p, err := BuildPlan(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Mapping["ctlpb_raw-multi"] != "fxify.demo" || p.Sources["ctlpb_raw-multi"] != "already migrated" {
		t.Fatalf("mapping = %v / %v", p.Mapping, p.Sources)
	}
	want := map[string]bool{
		filepath.Join(cfg.LogBase, "fxify.demo", "ctlpb_raw-multi"):                                           false,
		filepath.Join(cfg.LogBase, "icmarkets.demo", "propfolio_trend-multi"):                                 false,
		filepath.Join(cfg.LogBase, "fxify.demo", "ctlpb_raw-multi", "ctlpb_levels_EURUSD.r_20260827101.json"): false,
	}
	for _, m := range p.Moves {
		if _, ok := want[m.To]; ok {
			want[m.To] = true
		}
	}
	for to, seen := range want {
		if !seen {
			t.Errorf("resume should plan a move to %s; moves = %+v", to, p.Moves)
		}
	}
}

// Apply proves every move first: a source that cannot be renamed (Windows: a file inside is open) aborts
// before anything moves.
func TestApplyMovesNothingWhenOneSourceIsLocked(t *testing.T) {
	cfg := flatBox(t)
	p, err := BuildPlan(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(cfg.LogBase, "propfolio_trend-multi", "z_system_log_1.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := probeRename(filepath.Join(cfg.LogBase, "propfolio_trend-multi")); err == nil {
		t.Skip("this platform renames a folder with an open file inside; nothing to prove")
	}
	if err := p.Apply(cfg); err == nil || !strings.Contains(err.Error(), "nothing was moved") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.LiveBase, "ctlpb_raw-multi", "run.py")); err != nil {
		t.Errorf("the first live move must not have happened: %v", err)
	}
}
