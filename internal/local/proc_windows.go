//go:build windows

package local

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detachedFlags make the child survive this console and Ctrl+C in it.
const detachedFlags = windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP

// detach starts qemu without a console, in its own process group, with no window.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: detachedFlags}
}

const stillActive = 259

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// exited reports whether pid is verifiably gone: no such pid, or a process
// whose exit code is not STILL_ACTIVE. A pid we cannot even query is not
// reported as exited, so a failed kill of a live process is never hidden.
func exited(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return err == windows.ERROR_INVALID_PARAMETER
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code != stillActive
}

// killPid terminates pid. A process that already exited is a no-op: while an
// exited process is torn down, OpenProcess and TerminateProcess can fail with
// ERROR_ACCESS_DENIED, so a failure only counts as success if the process is
// verifiably gone. The caller has already checked the pid's start stamp.
func killPid(pid int) error {
	if pid <= 0 || exited(pid) {
		return nil
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if exited(pid) {
			return nil
		}
		return err
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err == nil && code != stillActive {
		return nil
	}
	if err := windows.TerminateProcess(h, 1); err != nil {
		if exited(pid) {
			return nil
		}
		return err
	}
	return nil
}

// procStart is the creation time of pid as a FILETIME.
func procStart(pid int) (uint64, bool) {
	if pid <= 0 {
		return 0, false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return 0, false
	}
	return uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime), true
}
