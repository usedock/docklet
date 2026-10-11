package local

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// sleeper starts an unrelated long-running process and returns its pid.
func sleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if runtime.GOOS == "windows" {
		cmd = exec.Command("ping", "-n", "30", "127.0.0.1")
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a helper process: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})
	return cmd.Process.Pid
}

// A pid file left by a crash or a reboot can name a process that reused the pid; the engine
// must neither report the dock running nor kill that process on stop or rm.
func TestReusedPidIsNotTheVM(t *testing.T) {
	pid := sleeper(t)
	start, ok := procStart(pid)
	if !ok {
		t.Skipf("process start times are not available on %s", runtime.GOOS)
	}
	if again, _ := procStart(pid); again != start {
		t.Fatalf("start stamp of one process must be stable: %d then %d", start, again)
	}
	e := &Engine{Dir: t.TempDir(), Driver: NewQEMU(os.Getenv), StopTimeout: time.Second}
	id := "dk_reused"
	if err := os.MkdirAll(e.store().DockDir(id), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, v string) {
		if err := os.WriteFile(e.file(id, name), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("pid", strconv.Itoa(pid))

	for _, tc := range []struct{ name, stamp string }{
		{"no recorded start", ""},
		{"another process's start", strconv.FormatUint(start+1, 10)},
	} {
		_ = os.Remove(e.file(id, "pidstart"))
		if tc.stamp != "" {
			write("pidstart", tc.stamp)
		}
		if e.alive(id) {
			t.Fatalf("%s: a reused pid must not count as the running VM", tc.name)
		}
		write("pid", strconv.Itoa(pid))
		if err := e.stopVM(context.Background(), id, true); err != nil {
			t.Fatal(err)
		}
		if !pidAlive(pid) {
			t.Fatalf("%s: stop killed an unrelated process", tc.name)
		}
		write("pid", strconv.Itoa(pid))
	}

	write("pidstart", strconv.FormatUint(start, 10))
	if !e.alive(id) {
		t.Fatal("the process the engine started must count as running")
	}
	if err := e.stopVM(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	if pidAlive(pid) {
		t.Fatal("stop must kill the VM process it started")
	}
}
