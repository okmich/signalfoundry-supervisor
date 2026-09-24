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
	want := Info{Name: "fxify.demo", EnvFile: filepath.Join(dir, ".env.fxify.demo"), EnvFound: true,
		Login: "123", Server: "FXIFY-Demo", Broker: "fxify", TerminalPath: `C:\MT5\terminal64.exe`}
	if got != want {
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
