//go:build !windows

// Non-Windows stubs so the engine / TUI / state logic builds and tests on dev machines. The real
// control mechanism is Windows-only (console Ctrl+C); off Windows it is a no-op that errors.
package proc

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func SendCtrlC(pid uint32) error {
	return errors.New("SendCtrlC: console Ctrl+C control is only implemented on Windows")
}

func Spawn(python, runPy string, args ...string) (int, error) {
	cmd := exec.Command(python, append([]string{runPy}, args...)...)
	cmd.Dir = filepath.Dir(runPy)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

func Alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func Kill(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// ctrlcSysProcAttr is a no-op off Windows (no console-event control there).
func ctrlcSysProcAttr() *syscall.SysProcAttr { return nil }

func CreateTime(pid int) (time.Time, bool) {
	_ = pid
	return time.Time{}, false
}
