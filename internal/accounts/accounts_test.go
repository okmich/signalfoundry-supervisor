package accounts

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestValid(t *testing.T) {
	for _, ok := range []string{"fxify.demo", "deriv.live", "ib.paper", "ic_markets.demo"} {
		if !Valid(ok) {
			t.Errorf("Valid(%q) = false", ok)
		}
	}
	for _, bad := range []string{"fxify", "Fxify.demo", "a.b.c", ".demo", "fxify.", "ctlpb_raw-multi", "account-admin", ".archive"} {
		if Valid(bad) {
			t.Errorf("Valid(%q) = true", bad)
		}
	}
}

func TestLoadReadsOnlyIdentityKeys(t *testing.T) {
	dir := t.TempDir()
	body := "# comment\nexport LOGIN_ID=123\nLOGIN_PASSWORD=secret\nLOGIN_SERVER=\"FXIFY-Demo\"\nBROKER_NAME=fxify # label\nTERMINAL_PATH='C:\\MT5\\terminal64.exe'\n"
	if err := os.WriteFile(filepath.Join(dir, ".env.fxify.demo"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Load(dir, "fxify.demo")
	if !got.defined["LOGIN_PASSWORD"] {
		t.Error("a defined key should be known by name")
	}
	got.defined = nil
	want := Info{Name: "fxify.demo", EnvFile: filepath.Join(dir, ".env.fxify.demo"), EnvFound: true,
		Login: "123", Server: "FXIFY-Demo", Broker: "fxify", TerminalPath: `C:\MT5\terminal64.exe`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load = %+v, want %+v", got, want)
	}
}

func TestLoadMissing(t *testing.T) {
	if got := Load(t.TempDir(), "deriv.live"); got.EnvFound || got.Login != "" {
		t.Fatalf("missing env file should load empty, got %+v", got)
	}
}

func TestList(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{".env.icmarkets.demo", ".env.fxify.demo", ".env.bad", "notes.txt", ".env.ib.paper"} {
		_ = os.WriteFile(filepath.Join(dir, n), nil, 0o644)
	}
	if got, want := List(dir), []string{"fxify.demo", "ib.paper", "icmarkets.demo"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
}

func TestMissingSessionKeys(t *testing.T) {
	if got := (Info{EnvFound: true, Login: "1"}).MissingSessionKeys(nil); !reflect.DeepEqual(got, []string{"TERMINAL_PATH", "LOGIN_SERVER"}) {
		t.Errorf("MT5 missing = %v", got)
	}
	if got := (Info{EnvFound: true, Login: "1", Server: "S", TerminalPath: "T"}).MissingSessionKeys(nil); got != nil {
		t.Errorf("complete MT5 = %v", got)
	}
	if got := (Info{EnvFound: true, IBHost: "127.0.0.1"}).MissingSessionKeys(nil); got != nil {
		t.Errorf("IB needs no MT5 keys, got %v", got)
	}
	if got := (Info{}).MissingSessionKeys(nil); got != nil {
		t.Errorf("a missing file is reported as missing, not as missing keys: %v", got)
	}
}

// An account whose configs name API credentials is an API account: it needs exactly those keys (with a value),
// and none of MT5's.
func TestAPISession(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env.bybit.demo"), []byte("API_KEY=k\nAPI_SECRET=\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info := Load(dir, "bybit.demo")
	keys := []string{"API_KEY", "API_SECRET"}
	if got := info.Session(keys); got != SessionAPI {
		t.Errorf("Session = %s, want api", got)
	}
	if got := info.MissingSessionKeys(keys); !reflect.DeepEqual(got, []string{"API_SECRET"}) {
		t.Errorf("missing = %v, want the empty API_SECRET only", got)
	}
	if got := info.Session(nil); got != SessionMT5 {
		t.Errorf("without config-named credentials the account is MT5, got %s", got)
	}
}
