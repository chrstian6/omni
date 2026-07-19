//go:build windows

package main

import (
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

// processAlive asks Windows directly rather than trusting the registry file,
// which — same as on macOS — can outlive a process that crashed.
func processAlive(pid int32) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)

	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	// STILL_ACTIVE (STATUS_PENDING, 259): the process hasn't exited yet.
	const stillActive = 259
	return code == stillActive
}

// endProcess: Windows has no SIGTERM for an unrelated process. `taskkill`
// without /F asks the process to close itself (a real WM_CLOSE for GUI apps;
// console apps get a close signal too), which gives Claude the same chance
// to flush its transcript that SIGTERM gives it on macOS/Linux. We deliberately
// don't fall back to a force-kill — that would strand the transcript mid-write,
// exactly what the graceful path exists to avoid.
func endProcess(pid int32) error {
	cmd := exec.Command("taskkill", "/PID", strconv.Itoa(int(pid)))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run()
}
