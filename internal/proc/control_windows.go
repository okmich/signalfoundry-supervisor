//go:build windows

package proc

import (
	"fmt"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32     = windows.NewLazySystemDLL("kernel32.dll")
	pAttach      = kernel32.NewProc("AttachConsole")
	pFree        = kernel32.NewProc("FreeConsole")
	pGenCtrl     = kernel32.NewProc("GenerateConsoleCtrlEvent")
	pSetCtrlHdlr = kernel32.NewProc("SetConsoleCtrlHandler")
)

const ctrlCEvent = 0

// SendCtrlC borrows the child's console and fires CTRL_C_EVENT into it, so the child runs its
// existing KeyboardInterrupt/graceful_shutdown path. Run inside the `ctrlc` helper process — it
// FreeConsole's itself, so it must not be the engine. Serialize calls (one attach per process).
func SendCtrlC(pid uint32) error {
	pFree.Call() // detach our own console (no-op if none)
	if r, _, err := pAttach.Call(uintptr(pid)); r == 0 {
		return fmt.Errorf("AttachConsole(%d): %v", pid, err)
	}
	defer func() {
		pFree.Call()
		pAttach.Call(uintptr(0xFFFFFFFF)) // ATTACH_PARENT_PROCESS, best-effort restore
	}()
	// Disable our own Ctrl+C handling so the event we send does not kill us.
	if r, _, err := pSetCtrlHdlr.Call(0, 1); r == 0 {
		return fmt.Errorf("SetConsoleCtrlHandler(NULL,TRUE): %v", err)
	}
	defer pSetCtrlHdlr.Call(0, 0)
	if r, _, err := pGenCtrl.Call(uintptr(ctrlCEvent), 0); r == 0 {
		return fmt.Errorf("GenerateConsoleCtrlEvent: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let the event deliver before we detach
	return nil
}

// ctrlcSysProcAttr launches the ctrlc helper in its OWN new console so its console state is
// deterministic regardless of whether the engine has one (it does not when run headless via
// Task-Scheduler-at-logon). The helper then FreeConsole's this console and AttachConsole's the
// target — see SendCtrlC. Without this, a helper spawned by a console-less engine reports success
// but the CTRL_C_EVENT is never delivered.
func ctrlcSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE}
}

// Spawn launches a system's run.py in its OWN console (CREATE_NEW_CONSOLE), so it can be targeted
// individually by SendCtrlC without disturbing siblings. The console opens MINIMIZED and unfocused
// (SW_SHOWMINNOACTIVE) so a fleet start does not blanket the desktop with foreground windows — it
// stays a taskbar entry the operator can restore. We go through CreateProcess directly because
// os/exec only exposes HideWindow (SW_HIDE, fully hidden); a hidden console is harder to inspect and
// loses the taskbar entry. Returns the child PID; the process handle is intentionally not retained —
// control is by PID and the child must outlive the supervisor (FLEET_SUPERVISOR_SPEC §6).
func Spawn(python, runPy string, args ...string) (int, error) {
	cmdline := windows.ComposeCommandLine(append([]string{python, runPy}, args...))
	clPtr, err := windows.UTF16PtrFromString(cmdline)
	if err != nil {
		return 0, err
	}
	dirPtr, err := windows.UTF16PtrFromString(filepath.Dir(runPy)) // OPS convention: cd <dir>; python run.py
	if err != nil {
		return 0, err
	}
	// STARTF_USESHOWWINDOW makes CreateProcess honor ShowWindow; SW_SHOWMINNOACTIVE opens the console
	// minimized without stealing focus from the foreground window.
	si := &windows.StartupInfo{Flags: windows.STARTF_USESHOWWINDOW, ShowWindow: windows.SW_SHOWMINNOACTIVE}
	si.Cb = uint32(unsafe.Sizeof(*si))
	var pi windows.ProcessInformation
	// lpEnvironment=nil -> inherit the engine's environment (so the child sees OKMICH_QUANT_* roots).
	if err := windows.CreateProcess(nil, clPtr, nil, nil, false,
		windows.CREATE_NEW_CONSOLE, nil, dirPtr, si, &pi); err != nil {
		return 0, err
	}
	_ = windows.CloseHandle(pi.Thread)
	_ = windows.CloseHandle(pi.Process) // not retained — closing the handle does not terminate the child
	return int(pi.ProcessId), nil
}

// Alive reports whether pid is a currently-running process.
func Alive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259 // STILL_ACTIVE
	return code == stillActive
}

// Kill hard-terminates pid (TerminateProcess) — the timeout fallback when a graceful stop does
// not complete within T (§11). An orphan-connection risk: the broker socket may be left half-open,
// which is exactly why the caller marks the system orphan_suspected and alerts.
func Kill(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 1)
}

// CreateTime returns pid's process creation time — the PID-reuse guard cross-check (§9/§12). A
// recycled PID belongs to a different process with a different creation time, so comparing this
// against a recorded baseline detects reuse before the supervisor fires a control event at it.
func CreateTime(pid int) (time.Time, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return time.Time{}, false
	}
	defer windows.CloseHandle(h)
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return time.Time{}, false
	}
	return time.Unix(0, creation.Nanoseconds()).UTC(), true
}
