package local

import (
	"context"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync/atomic"
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

// stampedDriver is a fake hypervisor whose processes carry a start stamp, so the engine's
// pid-reuse protection is active; it counts kills.
type stampedDriver struct {
	*fakeDriver
	stamp atomic.Uint64
	kills atomic.Int32
}

func newStampedDriver() *stampedDriver {
	d := &stampedDriver{fakeDriver: newFakeDriver()}
	d.stamp.Store(7)
	return d
}

func (d *stampedDriver) StartStamp(int) (uint64, bool) { return d.stamp.Load(), true }

func (d *stampedDriver) Kill(pid int) error {
	d.kills.Add(1)
	return d.fakeDriver.Kill(pid)
}

// If the VM exits during the graceful-shutdown wait and its pid goes to another process
// (a different start stamp), the stop must not kill it.
func TestStopDoesNotKillAPidWhoseStampChangedDuringTheWait(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newTestEngine(t)
	drv := newStampedDriver()
	drv.ignorePoweroff = true
	e.Driver = drv
	e.StopTimeout = 10 * time.Second
	d, err := e.New(ctx, Spec{Name: "reused"})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		drv.stamp.Store(8) // the pid now belongs to another process, which is alive
	}()
	start := time.Now()
	if err := e.stopVM(ctx, d.ID, false); err != nil {
		t.Fatal(err)
	}
	if n := drv.kills.Load(); n != 0 {
		t.Fatalf("killed a process that is no longer the VM (%d kills)", n)
	}
	// The wait ends once the pid is no longer the VM, not at the stop timeout.
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("stop waited %s for a pid that is no longer the VM", took)
	}
	if e.readInt(d.ID, "pid") != 0 {
		t.Fatal("runtime files must be cleared")
	}
}

// With the same stamp and a VM that ignores poweroff, the stop kills it after the timeout.
func TestStopKillsTheVMWhenItsStampIsUnchanged(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newTestEngine(t)
	drv := newStampedDriver()
	drv.ignorePoweroff = true
	e.Driver = drv
	e.StopTimeout = 400 * time.Millisecond
	d, err := e.New(ctx, Spec{Name: "stuck"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.stopVM(ctx, d.ID, false); err != nil {
		t.Fatal(err)
	}
	if n := drv.kills.Load(); n != 1 {
		t.Fatalf("want exactly one kill, got %d", n)
	}
	if e.alive(d.ID) {
		t.Fatal("the VM must be stopped")
	}
}

// flipDriver is a stamped VM whose agent refuses every ping at once and whose pid goes to
// another process after the first two stamp reads (boot record, first readiness check).
type flipDriver struct {
	*stampedDriver
	reads atomic.Int32
}

func (d *flipDriver) StartStamp(int) (uint64, bool) {
	if d.reads.Add(1) > 2 {
		return 8, true
	}
	return 7, true
}

func (d *flipDriver) Start(ctx context.Context, spec LaunchSpec) (int, error) {
	pid, err := d.fakeDriver.Start(ctx, spec)
	if err != nil {
		return 0, err
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(spec.HostPort)))
	if err != nil {
		return 0, err
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return pid, nil
}

// If the pid stops being the VM right as the boot times out, the timeout must not kill it.
func TestBootTimeoutDoesNotKillAPidWhoseStampChanged(t *testing.T) {
	e, _, _ := newTestEngine(t)
	drv := &flipDriver{stampedDriver: newStampedDriver()}
	drv.neverAnswer = true
	e.Driver = drv
	e.BootTimeout = 100 * time.Millisecond
	_, err := e.New(context.Background(), Spec{Name: "flip"})
	if codeOf(err) != "boot_timeout" {
		t.Fatalf("got %v", err)
	}
	if n := drv.kills.Load(); n != 0 {
		t.Fatalf("killed a process that is no longer the VM (%d kills)", n)
	}
}
