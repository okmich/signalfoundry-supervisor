package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeInference(t *testing.T, dir string, lines []string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "inference_" + time.Now().UTC().Format("20060102") + ".jsonl"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func TestLastBarTS(t *testing.T) {
	dir := t.TempDir()
	want := "2026-06-03T12:34:56Z"
	writeInference(t, dir, []string{
		`{"event":"bar","asof_bar_ts":"2026-06-03T12:00:00Z"}`,
		`{"event":"bar","asof_bar_ts":"2026-06-03T12:30:00Z"}`,
		`{"event":"bar","asof_bar_ts":"` + want + `"}`,
	})
	ts, ok := LastBarTS(dir)
	if !ok || !ts.Equal(mustTime(t, want)) {
		t.Errorf("LastBarTS = (%s, %v), want %s", ts, ok, want)
	}
}

// The tail read must find the final bar even when the day's file is far larger than the window.
func TestLastBarTSTailLargeFile(t *testing.T) {
	dir := t.TempDir()
	pad := strings.Repeat("x", 80)
	var lines []string
	for range 3000 { // ~3000 * ~120 bytes ~= 360KB >> 64KB window
		lines = append(lines, `{"event":"bar","asof_bar_ts":"2026-06-03T00:00:00Z","pad":"`+pad+`"}`)
	}
	want := "2026-06-03T23:59:59Z"
	lines = append(lines, `{"event":"bar","asof_bar_ts":"`+want+`"}`)
	writeInference(t, dir, lines)
	ts, ok := LastBarTS(dir)
	if !ok || !ts.Equal(mustTime(t, want)) {
		t.Errorf("tail read on large file = (%s, %v), want %s", ts, ok, want)
	}
}

func TestLastBarTSMissingFile(t *testing.T) {
	if _, ok := LastBarTS(t.TempDir()); ok {
		t.Error("a missing inference file should return ok=false")
	}
}

// The newest lines may be non-bar events (breaker/trade) or a partial mid-write; LastBarTS must
// still find the most recent parseable bar rather than going blind.
func TestLastBarTSSkipsNonBarTail(t *testing.T) {
	dir := t.TempDir()
	want := "2026-06-03T12:00:00Z"
	writeInference(t, dir, []string{
		`{"event":"bar","asof_bar_ts":"` + want + `"}`,
		`{"event":"breaker","reason":"halt"}`, // newer, not a bar
		`{"event":"trade","side":"buy"}`,      // newest, not a bar
		`{"event":"bar","asof_bar_ts":`,       // a partial mid-write line
	})
	ts, ok := LastBarTS(dir)
	if !ok || !ts.Equal(mustTime(t, want)) {
		t.Errorf("LastBarTS = (%s, %v), want the last complete bar %s despite newer non-bar/partial lines", ts, ok, want)
	}
}
