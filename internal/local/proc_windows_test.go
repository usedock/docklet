//go:build windows

package local

import (
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestDetachFlagsOnWindows(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "exit 0")
	detach(cmd)
	a := cmd.SysProcAttr
	if a == nil || !a.HideWindow {
		t.Fatal("qemu must run without a window")
	}
	if a.CreationFlags&windows.DETACHED_PROCESS == 0 || a.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Fatalf("creation flags %#x must detach and start a new process group", a.CreationFlags)
	}
}

func TestPidLivenessAndKillOnWindows(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "ping -n 30 127.0.0.1 >nul")
	detach(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	if !pidAlive(cmd.Process.Pid) {
		t.Fatal("a running process must be alive")
	}
	if err := killPid(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("process not killed")
	}
	if pidAlive(cmd.Process.Pid) {
		t.Fatal("a killed process must not be alive")
	}
	if pidAlive(0) || pidAlive(-1) {
		t.Fatal("invalid pids are not alive")
	}
	if err := killPid(cmd.Process.Pid); err != nil {
		t.Fatalf("killing a dead pid must be a no-op, got %v", err)
	}
}
