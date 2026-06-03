// Package proc owns OS process control: spawning systems (each in its own console), checking
// PID liveness, and stopping a system with a targeted console Ctrl+C (FLEET_SUPERVISOR_SPEC §9).
// Platform-specific bits live in control_windows.go / stub_other.go; this file is OS-neutral.
package proc

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
)

// CtrlCMain is the `supervisor ctrlc --pid N` helper: it borrows the target's console and fires
// CTRL_C_EVENT, then exits. Run as a throwaway process so the engine's own console is untouched.
func CtrlCMain(args []string) int {
	fs := flag.NewFlagSet("ctrlc", flag.ContinueOnError)
	pid := fs.Int("pid", 0, "target PID")
	if err := fs.Parse(args); err != nil || *pid <= 0 {
		fmt.Fprintln(os.Stderr, "ctrlc: --pid <n> is required")
		return 2
	}
	if err := SendCtrlC(uint32(*pid)); err != nil {
		fmt.Fprintln(os.Stderr, "ctrlc:", err)
		return 1
	}
	return 0
}

// Stop delivers a graceful stop to pid by exec-ing the ctrlc helper (keeps the caller's console
// intact). The caller MUST first verify pid is still the intended incarnation (PID-reuse guard).
//
// The helper is spawned with its own console (ctrlcSysProcAttr -> CREATE_NEW_CONSOLE on Windows):
// a headless engine (Task-Scheduler-at-logon, §13) has no console of its own, and a helper that
// inherits "no console" reports success but never delivers the event. Its own fresh console makes
// the FreeConsole -> AttachConsole(target) dance deterministic.
func Stop(pid int) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "ctrlc", "--pid", fmt.Sprint(pid))
	cmd.SysProcAttr = ctrlcSysProcAttr()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ctrlc helper failed: %v: %s", err, out)
	}
	return nil
}
