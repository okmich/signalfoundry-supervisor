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

const acct = "fxify.demo"

func TestScanClassifiesSingleAndMulti(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, acct)
	// single trader: path-derived, no config.json at all
	write(t, filepath.Join(a, "rsi2_mean_reversion", "EURUSD", "5", "run.py"), "")
	// single trader with a singular-strategy config (empty strategies[]) -> still single
	write(t, filepath.Join(a, "hmm_neutral", "GBPUSD", "15", "run.py"), "")
	write(t, filepath.Join(a, "hmm_neutral", "GBPUSD", "15", "config.json"),
		`{"strategy":{"name":"hmm_neutral","symbol":"GBPUSD","timeframe":15},"strategies":[]}`)
	// multi trader: config.json with a non-empty strategies[]
	write(t, filepath.Join(a, "rsi2_mean_reversion_multi", "run.py"), "")
	write(t, filepath.Join(a, "rsi2_mean_reversion_multi", "config.json"),
		`{"name":"Rsi2Multi","strategies":[{"name":"rsi2_mean_reversion","symbol":"Volatility 75 Index","timeframe":5},{"name":"rsi2_mean_reversion","symbol":"BTCUSD","timeframe":5}]}`)

	cat, probs, err := Scan(root)
	if err != nil || len(probs) != 0 {
		t.Fatal(err, probs)
	}
	got := map[string]System{}
	for _, s := range cat {
		got[s.SystemID] = s
	}

	if s := got["fxify.demo/rsi2_mean_reversion/EURUSD/5"]; s.Multi || s.RunnerStrategy != "rsi2_mean_reversion" || s.Account != acct {
		t.Errorf("single (no config) = %+v, want single rsi2_mean_reversion in %s", s, acct)
	}
	if got["fxify.demo/hmm_neutral/GBPUSD/15"].Multi {
		t.Errorf("empty strategies[] must classify as single, got multi")
	}
	m, ok := got["fxify.demo/rsi2_mean_reversion-multi"]
	if !ok || !m.Multi || m.RunnerStrategy != "rsi2_mean_reversion-multi" || len(m.Symbols) != 2 || m.Account != acct {
		t.Errorf("multi = %+v, want rsi2_mean_reversion-multi with 2 symbols", m)
	}
}

// TestScanSkipsDotDirs verifies the importer's archive/staging subtrees (and any dot-dir) are not
// mistaken for live systems even though they contain run.py copies, and that the Account Admin's live
// state folder is never walked.
func TestScanSkipsDotDirs(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, acct, "strat", "EURUSD", "5", "run.py"), "")
	// archived + staged copies hold run.py but must be invisible to discovery
	write(t, filepath.Join(root, ".archive", acct, "strat", "EURUSD", "5", "20200101T000000Z", "run.py"), "")
	write(t, filepath.Join(root, ".staging", "fxify.demo_strat_EURUSD_5", "run.py"), "")
	write(t, filepath.Join(root, acct, ".tmp", "x", "y", "run.py"), "")
	write(t, filepath.Join(root, acct, "account-admin", "a", "b", "run.py"), "")

	cat, probs, err := Scan(root)
	if err != nil || len(probs) != 0 {
		t.Fatal(err, probs)
	}
	if len(cat) != 1 || cat[0].SystemID != "fxify.demo/strat/EURUSD/5" {
		t.Fatalf("want exactly the one live system, got %+v", cat)
	}
}

// TestScanReportsSystemsOutsideAccounts: the pre-account flat layout and unclassifiable paths become
// problems, never systems, so the engine cannot start a runner that does not know its account.
func TestScanReportsSystemsOutsideAccounts(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "ctlpb_raw-multi", "run.py"), "")       // flat layout
	write(t, filepath.Join(root, "notes", "readme.txt"), "")             // not a system: ignored
	write(t, filepath.Join(root, acct, "strat", "EURUSD", "run.py"), "") // too shallow for a single-trader
	write(t, filepath.Join(root, acct, "admin_account", "icmarkets_1", "M1", "run.py"), "")

	cat, probs, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat) != 1 || cat[0].SystemID != "fxify.demo/admin_account/icmarkets_1/M1" {
		t.Fatalf("want only the admin system, got %+v", cat)
	}
	paths := map[string]bool{}
	for _, p := range probs {
		paths[p.Path] = true
	}
	if len(probs) != 2 || !paths["ctlpb_raw-multi"] || !paths["fxify.demo/strat/EURUSD"] {
		t.Fatalf("problems = %+v", probs)
	}
}
