package ipc

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A bulk fan-out submits many commands faster than the wall clock advances; ids must stay unique
// so none overwrite another's file.
func TestSubmitCommandUniqueIDs(t *testing.T) {
	dir := t.TempDir()
	seen := map[string]bool{}
	for range 50 {
		id, err := SubmitCommand(dir, "stop", "a/b/c")
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("duplicate command id: %s", id)
		}
		seen[id] = true
	}
	cmds, err := PendingCommands(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 50 {
		t.Errorf("got %d command files, want 50 (a collision would drop one)", len(cmds))
	}
}

// A command file whose payload id disagrees with its filename must be keyed off the FILENAME, so
// its result marker matches and it is deduped — otherwise it reprocesses every tick forever.
func TestPendingCommandsCanonicalizesID(t *testing.T) {
	dir := t.TempDir()
	name := "20260603T000000.000000000Z-1.json"
	body := `{"id":"WRONG","action":"stop","system_id":"a/b/c"}`
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cmds, err := PendingCommands(dir)
	if err != nil {
		t.Fatal(err)
	}
	wantID := "20260603T000000.000000000Z-1"
	if len(cmds) != 1 || cmds[0].ID != wantID {
		t.Fatalf("got %+v, want one command with filename-derived id %q", cmds, wantID)
	}
	if err := WriteResult(dir, CommandResult{ID: cmds[0].ID}); err != nil {
		t.Fatal(err)
	}
	if again, _ := PendingCommands(dir); len(again) != 0 {
		t.Errorf("command should be deduped after its result is written, still pending: %d", len(again))
	}
}

func TestPruneCommands(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.json")
	fresh := filepath.Join(dir, "fresh.json")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	PruneCommands(dir, time.Hour, time.Now())
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("file older than ttl should be pruned")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh file should survive the prune")
	}
}
