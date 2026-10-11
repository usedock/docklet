//go:build !windows

package local

import (
	"os/exec"
	"testing"
	"time"
)

func TestDetachStartsANewSession(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	detach(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatal("qemu must run in its own session")
	}
}

func TestPidLivenessAndKillOnUnix(t *testing.T) {
	cmd := exec.Command("sleep", "30")
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
	if pidAlive(cmd.Process.Pid) || pidAlive(0) {
		t.Fatal("dead or invalid pids are not alive")
	}
	if err := killPid(cmd.Process.Pid); err != nil {
		t.Fatalf("killing a dead pid must be a no-op, got %v", err)
	}
}
