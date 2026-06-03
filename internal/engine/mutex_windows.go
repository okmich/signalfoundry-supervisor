//go:build windows

package engine

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procCreateMutexW = windows.NewLazySystemDLL("kernel32.dll").NewProc("CreateMutexW")

// acquireMutex creates a named mutex; if it already exists, another engine for this deployment holds
// it, so we refuse. The OS releases the mutex automatically when this process exits (even on a
// crash), so unlike a pidfile it never goes stale. CreateMutexW returns a valid handle even when the
// name already exists — GetLastError (the third Call return) is what distinguishes the two.
func acquireMutex(name string) (uintptr, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	h, _, lastErr := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(p)))
	if h == 0 {
		return 0, fmt.Errorf("CreateMutex: %v", lastErr)
	}
	if lastErr == syscall.ERROR_ALREADY_EXISTS {
		windows.CloseHandle(windows.Handle(h))
		return 0, fmt.Errorf("an engine is already running for this deployment — refusing to start a second")
	}
	return h, nil
}

func releaseMutex(h uintptr) {
	if h != 0 {
		windows.CloseHandle(windows.Handle(h))
	}
}
