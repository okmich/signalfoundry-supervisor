package importsys

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/okmich/signalfoundry-supervisor/internal/config"
	"github.com/okmich/signalfoundry-supervisor/internal/registry"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testCfg(t *testing.T) config.Config {
	t.Helper()
	dir := t.TempDir()
	return config.Config{LiveBase: filepath.Join(dir, "live"), StateDir: filepath.Join(dir, "state")}
}

// srcWith writes a conforming source dir with the given config.json and returns its path.
func srcWith(t *testing.T, configJSON string) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	writeFile(t, filepath.Join(src, "run.py"), "print('hi')")
	writeFile(t, filepath.Join(src, "config.json"), configJSON)
	return src
}

func TestBuildPlanSingle(t *testing.T) {
	cfg := testCfg(t)
	src := srcWith(t, `{"name":"X","strategy":{"name":"rsi2_mean_reversion","symbol":"Volatility 100 Index","timeframe":5},"strategies":[]}`)

	p, err := BuildPlan(cfg, src)
	if err != nil {
		t.Fatal(err)
	}
	if p.Multi {
		t.Fatal("want single-trader")
	}
	// system_id and target use the framework's path labels (spaces preserved, timeframe int).
	if p.SystemID != "rsi2_mean_reversion/Volatility 100 Index/5" {
		t.Fatalf("system_id = %q", p.SystemID)
	}
	want := filepath.Join(cfg.LiveBase, "rsi2_mean_reversion", "Volatility 100 Index", "5")
	if p.TargetDir != want {
		t.Fatalf("target = %q, want %q", p.TargetDir, want)
	}
}

func TestBuildPlanMulti(t *testing.T) {
	cfg := testCfg(t)
	src := srcWith(t, `{"name":"M","strategies":[{"name":"rsi2_mean_reversion","symbol":"BTCUSD","timeframe":5},{"name":"rsi2_mean_reversion","symbol":"Step Index","timeframe":5}]}`)

	p, err := BuildPlan(cfg, src)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Multi || p.SystemID != "rsi2_mean_reversion-multi" {
		t.Fatalf("plan = %+v, want rsi2_mean_reversion-multi", p)
	}
	if len(p.Symbols) != 2 {
		t.Fatalf("symbols = %v", p.Symbols)
	}
	want := filepath.Join(cfg.LiveBase, "rsi2_mean_reversion-multi")
	if p.TargetDir != want {
		t.Fatalf("target = %q, want %q", p.TargetDir, want)
	}
}

func TestBuildPlanValidationErrors(t *testing.T) {
	cfg := testCfg(t)

	t.Run("missing run.py", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "s")
		writeFile(t, filepath.Join(src, "config.json"), `{"strategy":{"name":"s","symbol":"EURUSD","timeframe":5}}`)
		if _, err := BuildPlan(cfg, src); err == nil || !strings.Contains(err.Error(), "run.py") {
			t.Fatalf("want run.py error, got %v", err)
		}
	})
	t.Run("missing config.json", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "s")
		writeFile(t, filepath.Join(src, "run.py"), "")
		if _, err := BuildPlan(cfg, src); err == nil || !strings.Contains(err.Error(), "config.json") {
			t.Fatalf("want config.json error, got %v", err)
		}
	})
	t.Run("bad json", func(t *testing.T) {
		src := srcWith(t, `{not json`)
		if _, err := BuildPlan(cfg, src); err == nil || !strings.Contains(err.Error(), "valid JSON") {
			t.Fatalf("want json error, got %v", err)
		}
	})
	t.Run("neither single nor multi", func(t *testing.T) {
		src := srcWith(t, `{"name":"x","strategies":[]}`)
		if _, err := BuildPlan(cfg, src); err == nil || !strings.Contains(err.Error(), "neither") {
			t.Fatalf("want classify error, got %v", err)
		}
	})
	t.Run("reserved char in symbol", func(t *testing.T) {
		src := srcWith(t, `{"strategy":{"name":"s","symbol":"EUR:USD","timeframe":5},"strategies":[]}`)
		if _, err := BuildPlan(cfg, src); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("want reserved-char error, got %v", err)
		}
	})
	t.Run("non-positive timeframe", func(t *testing.T) {
		src := srcWith(t, `{"strategy":{"name":"s","symbol":"EURUSD","timeframe":0},"strategies":[]}`)
		if _, err := BuildPlan(cfg, src); err == nil || !strings.Contains(err.Error(), "timeframe") {
			t.Fatalf("want timeframe error, got %v", err)
		}
	})
}

func TestApplyCopiesSkipsJunkAndArchives(t *testing.T) {
	cfg := testCfg(t)
	src := filepath.Join(t.TempDir(), "src")
	writeFile(t, filepath.Join(src, "run.py"), "print()")
	writeFile(t, filepath.Join(src, "config.json"), `{"strategy":{"name":"s","symbol":"EURUSD","timeframe":15},"strategies":[]}`)
	writeFile(t, filepath.Join(src, "model.json"), `{}`)             // a real artefact
	writeFile(t, filepath.Join(src, "__pycache__", "x.pyc"), "junk") // build noise
	writeFile(t, filepath.Join(src, "z_system_log_123.log"), "junk") // transient runner log

	p, err := BuildPlan(cfg, src)
	if err != nil {
		t.Fatal(err)
	}
	if p.WillArchive {
		t.Fatal("first import should not archive")
	}
	if _, err := p.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"run.py", "config.json", "model.json"} {
		if _, err := os.Stat(filepath.Join(p.TargetDir, f)); err != nil {
			t.Errorf("installed artefact %s missing: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(p.TargetDir, "__pycache__")); !os.IsNotExist(err) {
		t.Error("__pycache__ should have been skipped")
	}
	if _, err := os.Stat(filepath.Join(p.TargetDir, "z_system_log_123.log")); !os.IsNotExist(err) {
		t.Error("z_*.log should have been skipped")
	}
	// no leftover staging
	if _, err := os.Stat(filepath.Join(cfg.LiveBase, StagingDir)); err == nil {
		entries, _ := os.ReadDir(filepath.Join(cfg.LiveBase, StagingDir))
		if len(entries) != 0 {
			t.Errorf("staging not cleaned: %v", entries)
		}
	}

	// Re-import the same system -> the existing copy is archived under .archive.
	p2, err := BuildPlan(cfg, src)
	if err != nil {
		t.Fatal(err)
	}
	if !p2.WillArchive {
		t.Fatal("re-import should archive the existing copy")
	}
	archived, err := p2.Apply(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if archived == "" || !strings.Contains(archived, ArchiveDir) {
		t.Fatalf("want archive path under %s, got %q", ArchiveDir, archived)
	}
	if _, err := os.Stat(filepath.Join(archived, "run.py")); err != nil {
		t.Errorf("archived copy missing run.py: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p2.TargetDir, "run.py")); err != nil {
		t.Errorf("reinstalled copy missing run.py: %v", err)
	}
}

func TestDecommissionArchivesAndRemoves(t *testing.T) {
	cfg := testCfg(t)
	src := srcWith(t, `{"strategy":{"name":"s","symbol":"EURUSD","timeframe":15},"strategies":[]}`)
	p, err := BuildPlan(cfg, src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(cfg); err != nil {
		t.Fatal(err)
	}

	archived, err := Decommission(cfg, p.SystemID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.TargetDir); !os.IsNotExist(err) {
		t.Errorf("artefact should be gone from LIVE_BASE after decommission")
	}
	if !strings.Contains(archived, ArchiveDir) {
		t.Errorf("archive path should be under %s, got %q", ArchiveDir, archived)
	}
	if _, err := os.Stat(filepath.Join(archived, "run.py")); err != nil {
		t.Errorf("archived copy missing run.py: %v", err)
	}
}

func TestDecommissionRefusesRunning(t *testing.T) {
	cfg := testCfg(t)
	src := srcWith(t, `{"strategy":{"name":"s","symbol":"EURUSD","timeframe":15},"strategies":[]}`)
	p, _ := BuildPlan(cfg, src)
	if _, err := p.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := registry.Registry{Entries: map[string]registry.Entry{
		p.SystemID: {SystemID: p.SystemID, PID: os.Getpid()}, // this process == definitely alive
	}}
	if err := registry.Save(cfg.RegistryPath(), reg); err != nil {
		t.Fatal(err)
	}
	if _, err := Decommission(cfg, p.SystemID); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("want running refusal, got %v", err)
	}
	if _, err := os.Stat(p.TargetDir); err != nil {
		t.Errorf("artefact must remain when decommission is refused: %v", err)
	}
}

func TestDecommissionMissing(t *testing.T) {
	cfg := testCfg(t)
	if _, err := Decommission(cfg, "nope/X/5"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want not-found error, got %v", err)
	}
}

// A bogus id that resolves to LIVE_BASE itself or escapes it must be rejected, never archived — guards
// the exported os.Rename against ".", "..", and traversal ids.
func TestDecommissionRejectsTraversal(t *testing.T) {
	cfg := testCfg(t)
	if err := os.MkdirAll(cfg.LiveBase, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{".", "..", "../escape", "a/../.."} {
		if _, err := Decommission(cfg, id); err == nil {
			t.Errorf("id %q should be rejected", id)
		}
	}
	// LIVE_BASE itself must still exist (nothing was archived/moved).
	if _, err := os.Stat(cfg.LiveBase); err != nil {
		t.Errorf("LIVE_BASE must be untouched: %v", err)
	}
}

func TestBuildPlanRefusesRunning(t *testing.T) {
	cfg := testCfg(t)
	src := srcWith(t, `{"strategy":{"name":"s","symbol":"EURUSD","timeframe":15},"strategies":[]}`)

	// Seed the registry with THIS process's pid for the resolved system_id -> definitely alive.
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := registry.Registry{Entries: map[string]registry.Entry{
		"s/EURUSD/15": {SystemID: "s/EURUSD/15", PID: os.Getpid()},
	}}
	if err := registry.Save(cfg.RegistryPath(), reg); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildPlan(cfg, src); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("want running refusal, got %v", err)
	}
}
