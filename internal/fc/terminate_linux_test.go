//go:build linux

package fc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// spawnFake starts a process whose argv looks like "<argv0> --id <id>".
func spawnFake(t *testing.T, argv0, id string) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "sleep 60 & wait", "--id", id)
	cmd.Args[0] = argv0
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd, done
}

func TestTerminateStopsCustomNamedBinary(t *testing.T) {
	custom := "/opt/vmm/custom-vmm"
	cmd, done := spawnFake(t, custom, "dock-1")
	jail := t.TempDir()
	if err := os.WriteFile(filepath.Join(jail, "firecracker.pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}

	unconfigured := NewSDKDriver()
	unconfigured.Terminate(&Handle{DockID: "dock-1", JailRoot: jail})
	select {
	case <-done:
		t.Fatal("a driver without the configured binary must not kill a differently named process")
	case <-time.After(300 * time.Millisecond):
	}

	d := NewSDKDriver()
	d.FirecrackerBin = custom
	if err := d.Terminate(&Handle{DockID: "dock-1", JailRoot: jail}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the configured custom binary must be terminated")
	}
}

func TestTerminateLeavesOtherDocksAlone(t *testing.T) {
	cmd, done := spawnFake(t, "firecracker", "dock-12")
	jail := t.TempDir()
	if err := os.WriteFile(filepath.Join(jail, "firecracker.pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := NewSDKDriver().Terminate(&Handle{DockID: "dock-1", JailRoot: jail}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Fatal("a prefix id must not terminate another dock's VM")
	case <-time.After(300 * time.Millisecond):
	}
}
