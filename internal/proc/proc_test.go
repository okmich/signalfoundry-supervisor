package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLastLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "z_console.log")
	body := "Traceback (most recent call last):\r\n  File \"run.py\", line 29\r\nImportError: cannot import name 'x'\r\n\r\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LastLine(p, 200); got != "ImportError: cannot import name 'x'" {
		t.Fatalf("LastLine = %q", got)
	}
	if got := LastLine(p, 10); got != "ImportErr…" {
		t.Fatalf("truncated LastLine = %q", got)
	}
	if got := LastLine(filepath.Join(t.TempDir(), "missing"), 10); got != "" {
		t.Fatalf("missing file = %q", got)
	}
}

// A runner's console output — including a traceback it dies with before it can log — lands in the capture file.
func TestSpawnCapturesConsoleOutput(t *testing.T) {
	python, err := exec.LookPath("python")
	if err != nil {
		t.Skip("no python on PATH")
	}
	dir := t.TempDir()
	runPy := filepath.Join(dir, "run.py")
	if err := os.WriteFile(runPy, []byte("print('hello from run.py')\nraise SystemExit('boom: missing thing')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	console := filepath.Join(dir, "z_console_test.log")
	pid, err := Spawn(python, runPy, console)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for Alive(pid) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	b, _ := os.ReadFile(console)
	if out := string(b); !strings.Contains(out, "hello from run.py") || !strings.Contains(out, "boom: missing thing") {
		t.Fatalf("capture = %q", out)
	}
}
