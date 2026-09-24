package proc

import (
	"reflect"
	"testing"
)

// The per-system override replaces an inherited value of the same name (case-insensitively, as on
// Windows) instead of duplicating it, so a stray OKMICH_QUANT_ACCOUNT in the engine's own environment
// can never reach a child.
func TestMergeEnvOverrides(t *testing.T) {
	base := []string{"PATH=C:\\bin", "=C:=C:\\work", "okmich_quant_account=stale.demo", "OKMICH_QUANT_LOG_BASE=D:\\logs"}
	got := MergeEnv(base, []string{"OKMICH_QUANT_ACCOUNT=fxify.demo"})
	want := []string{"PATH=C:\\bin", "=C:=C:\\work", "OKMICH_QUANT_LOG_BASE=D:\\logs", "OKMICH_QUANT_ACCOUNT=fxify.demo"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MergeEnv = %q, want %q", got, want)
	}
	if got := MergeEnv(base, nil); !reflect.DeepEqual(got, base) {
		t.Fatalf("no overrides should return base unchanged, got %q", got)
	}
}
