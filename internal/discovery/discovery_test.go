package discovery

import (
	"os"
	"path/filepath"
	"testing"
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

func TestScanClassifiesSingleAndMulti(t *testing.T) {
	root := t.TempDir()
	// single trader: path-derived, no config.json at all
	write(t, filepath.Join(root, "rsi2_mean_reversion", "EURUSD", "5", "run.py"), "")
	// single trader with a singular-strategy config (empty strategies[]) -> still single
	write(t, filepath.Join(root, "hmm_neutral", "GBPUSD", "15", "run.py"), "")
	write(t, filepath.Join(root, "hmm_neutral", "GBPUSD", "15", "config.json"),
		`{"strategy":{"name":"hmm_neutral","symbol":"GBPUSD","timeframe":15},"strategies":[]}`)
	// multi trader: config.json with a non-empty strategies[]
	write(t, filepath.Join(root, "rsi2_mean_reversion_multi", "run.py"), "")
	write(t, filepath.Join(root, "rsi2_mean_reversion_multi", "config.json"),
		`{"name":"Rsi2Multi","strategies":[{"name":"rsi2_mean_reversion","symbol":"Volatility 75 Index","timeframe":5},{"name":"rsi2_mean_reversion","symbol":"BTCUSD","timeframe":5}]}`)

	cat, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]System{}
	for _, s := range cat {
		got[s.SystemID] = s
	}

	if s := got["rsi2_mean_reversion/EURUSD/5"]; s.Multi || s.RunnerStrategy != "rsi2_mean_reversion" {
		t.Errorf("single (no config) = %+v, want single rsi2_mean_reversion", s)
	}
	if got["hmm_neutral/GBPUSD/15"].Multi {
		t.Errorf("empty strategies[] must classify as single, got multi")
	}
	m, ok := got["rsi2_mean_reversion-multi"]
	if !ok || !m.Multi || m.RunnerStrategy != "rsi2_mean_reversion-multi" || len(m.Symbols) != 2 {
		t.Errorf("multi = %+v, want rsi2_mean_reversion-multi with 2 symbols", m)
	}
}

// TestScanSkipsDotDirs verifies the importer's archive/staging subtrees (and any dot-dir) are not
// mistaken for live systems even though they contain run.py copies.
func TestScanSkipsDotDirs(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "strat", "EURUSD", "5", "run.py"), "")
	// archived + staged copies hold run.py but must be invisible to discovery
	write(t, filepath.Join(root, ".archive", "strat", "EURUSD", "5", "20200101T000000Z", "run.py"), "")
	write(t, filepath.Join(root, ".staging", "strat_EURUSD_5", "run.py"), "")

	cat, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat) != 1 || cat[0].SystemID != "strat/EURUSD/5" {
		t.Fatalf("want exactly the one live system, got %+v", cat)
	}
}
