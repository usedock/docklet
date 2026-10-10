package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/usedock/docklet/api/v1"
	"github.com/usedock/docklet/internal/fc"
	"github.com/usedock/docklet/internal/host"
)

var testRes = apiv1.Resources{CPUs: 1, MemoryMiB: 2048, DiskGiB: 10}

func TestImplementsBackend(t *testing.T) {
	var _ host.Backend = (*Driver)(nil)
	var _ fc.Driver = (*Driver)(nil)
}

func TestContainerNameSafe(t *testing.T) {
	name := ContainerName("33333333-3333-4333-8333-333333333333")
	if !strings.HasPrefix(name, "dock-") || len(name) > 64 {
		t.Fatalf("name = %q", name)
	}
	if ContainerName("a/b:c") == ContainerName("a-b-c") {
		t.Fatal("distinct ids must not share a container name")
	}
	long := strings.Repeat("a", 70)
	if ContainerName(long) == ContainerName(long+"b") {
		t.Fatal("ids differing past the old truncation point must not collide")
	}
}

func TestBootPinsRunscAndLocksProfile(t *testing.T) {
	var calls [][]string
	d := &Driver{
		Runner: func(name string, args ...string) error {
			calls = append(calls, append([]string{name}, args...))
			return nil
		},
		Image:      "dock-gvisor:test",
		InspectPID: func(string) (int, error) { return 4242, nil },
	}
	useDisk(d, t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fast-agent.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := d.Boot(context.Background(), fc.BootSpec{DockID: "dock-1", Resources: testRes, JailRoot: dir})
	if err != nil || h.Pid != 4242 {
		t.Fatalf("boot = %+v %v", h, err)
	}
	joined := strings.Join(calls[0], " ")
	// The rootfs stays writable (Docker's own layer is the container's
	// filesystem) and the container runs unprivileged with no capabilities.
	for _, want := range []string{
		"--runtime=runsc", "--network=none", "--cap-drop=ALL", "no-new-privileges",
		"--cpus 1", "--memory 2048m", "--user 1001:1001", "dock-gvisor:test",
		// Dock state lives on the bounded work disk so a capture is a host-side tar.
		"/docks/dock-1/work:/home/user/work",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("docker run = %q, want %q", joined, want)
		}
	}
	for _, never := range []string{"--privileged", "--read-only"} {
		if strings.Contains(joined, never) {
			t.Fatalf("docker run must never pass %s: %q", never, joined)
		}
	}
	if want := "fast-agent.json:/etc/dock-fast-agent.json:ro"; !strings.Contains(joined, want) {
		t.Fatalf("docker run = %q, want mount %q", joined, want)
	}
	got, err := fc.ReadSidecar(dir)
	if err != nil || got != testRes {
		t.Fatalf("sidecar = %+v %v, want %+v", got, err, testRes)
	}
}

func TestBootJoinsNetworkHeldBeforeStart(t *testing.T) {
	// gVisor snapshots interfaces at sandbox start, so the veth must already be
	// in the namespace the dock joins.
	var calls []string
	attachedPID := 0
	d := &Driver{
		Runner: func(name string, args ...string) error {
			calls = append(calls, strings.Join(append([]string{name}, args...), " "))
			return nil
		},
		Image: "dock-gvisor:test",
		InspectPID: func(name string) (int, error) {
			if name == NetHolderName("dock-1") {
				return 7, nil
			}
			return 4242, nil
		},
	}
	useDisk(d, t.TempDir())
	attach := func(pid int) error {
		attachedPID = pid
		calls = append(calls, "attach")
		return nil
	}
	h, err := d.Boot(context.Background(), fc.BootSpec{DockID: "dock-1", Resources: testRes, JailRoot: t.TempDir(), AttachNetwork: attach})
	if err != nil || h.Pid != 4242 {
		t.Fatalf("boot = %+v %v", h, err)
	}
	if attachedPID != 7 {
		t.Fatalf("attached pid = %d, want the holder's 7", attachedPID)
	}
	holder := NetHolderName("dock-1")
	if len(calls) != 4 || calls[0] != "docker rm -f "+holder || calls[2] != "attach" {
		t.Fatalf("calls = %q, want stale-holder clear, holder run, attach, dock run", calls)
	}
	for _, want := range []string{"--name " + holder, "--runtime=runc", "--network=none", "--dns 1.1.1.1", "--dns-search .", "--cap-drop=ALL", "--read-only", "--user 65534:65534", "sleep dock-gvisor:test infinity"} {
		if !strings.Contains(calls[1], want) {
			t.Fatalf("holder run = %q, want %q", calls[1], want)
		}
	}
	if !strings.Contains(calls[3], "--runtime=runsc") || !strings.Contains(calls[3], "--network=container:"+holder) {
		t.Fatalf("dock run = %q, want runsc joining %s", calls[3], holder)
	}
}

func TestBootRemovesHolderWhenAttachFails(t *testing.T) {
	var calls []string
	d := &Driver{
		Runner: func(name string, args ...string) error {
			calls = append(calls, strings.Join(append([]string{name}, args...), " "))
			return nil
		},
		Image:      "dock-gvisor:test",
		InspectPID: func(string) (int, error) { return 7, nil },
	}
	useDisk(d, t.TempDir())
	attach := func(int) error { return errors.New("nsenter failed") }
	if _, err := d.Boot(context.Background(), fc.BootSpec{DockID: "dock-1", Resources: testRes, JailRoot: t.TempDir(), AttachNetwork: attach}); err == nil {
		t.Fatal("a failed attach must fail the boot")
	}
	last := calls[len(calls)-1]
	if last != "docker rm -f "+NetHolderName("dock-1") {
		t.Fatalf("last holder removal = %q, want the holder removed", last)
	}
	for _, c := range calls {
		if strings.Contains(c, "--runtime=runsc") {
			t.Fatalf("dock must not start without its network: %q", calls)
		}
	}
}

func TestTerminateRemovesNetHolder(t *testing.T) {
	var calls []string
	d := &Driver{Runner: func(name string, args ...string) error {
		calls = append(calls, strings.Join(append([]string{name}, args...), " "))
		return nil
	}}
	if err := d.Terminate(&fc.Handle{DockID: "x"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"docker rm -f " + ContainerName("x"), "docker rm -f " + NetHolderName("x")}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
}

func TestBootRejectsInvalidResources(t *testing.T) {
	d := &Driver{Image: "img", InspectPID: func(string) (int, error) { return 1, nil }}
	if _, err := d.Boot(context.Background(), fc.BootSpec{DockID: "d"}); !errors.Is(err, fc.ErrInvalidResources) {
		t.Fatalf("err = %v, want ErrInvalidResources", err)
	}
}

func TestBootRequiresAnImage(t *testing.T) {
	d := &Driver{InspectPID: func(string) (int, error) { return 1, nil }}
	if _, err := d.Boot(context.Background(), fc.BootSpec{DockID: "d", Resources: testRes}); err == nil {
		t.Fatal("a container backend without an image must fail closed")
	}
}

func TestPauseMissingDegradesToNoMicroVM(t *testing.T) {
	d := &Driver{Runner: func(string, ...string) error { return errors.New("Error: No such container: dock-x") }}
	if err := d.Pause(context.Background(), &fc.Handle{DockID: "x"}); !errors.Is(err, fc.ErrNoMicroVM) {
		t.Fatalf("err = %v, want ErrNoMicroVM", err)
	}
}

func TestPauseIsIdempotent(t *testing.T) {
	// A failed capture can leave the container frozen; the retry must proceed.
	d := &Driver{Runner: func(string, ...string) error {
		return errors.New("Error response from daemon: container abc is already paused")
	}}
	if err := d.Pause(context.Background(), &fc.Handle{DockID: "x"}); err != nil {
		t.Fatalf("err = %v, want nil for an already-paused container", err)
	}
}

func TestResumeIsIdempotent(t *testing.T) {
	d := &Driver{Runner: func(string, ...string) error {
		return errors.New("Error response from daemon: container abc is not paused")
	}}
	if err := d.Resume(context.Background(), &fc.Handle{DockID: "x"}); err != nil {
		t.Fatalf("err = %v, want nil for a running container", err)
	}
}

func TestPauseStillReportsRealFailures(t *testing.T) {
	d := &Driver{Runner: func(string, ...string) error { return errors.New("device or resource busy") }}
	if err := d.Pause(context.Background(), &fc.Handle{DockID: "x"}); err == nil {
		t.Fatal("a real pause failure must surface")
	}
}

func TestTerminateMissingIsGone(t *testing.T) {
	d := &Driver{Runner: func(string, ...string) error { return errors.New("No such container") }}
	if err := d.Terminate(&fc.Handle{DockID: "x"}); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

func TestTerminateRetriesUntilContainerIsGone(t *testing.T) {
	// A paused runsc container can be killed without being deleted in the same
	// call; the retry keeps it from pinning the workspace volume.
	attempts := 0
	d := &Driver{Runner: func(_ string, args ...string) error {
		if args[len(args)-1] != ContainerName("x") {
			return nil
		}
		attempts++
		if attempts < 3 {
			return errors.New("Error response from daemon: cannot remove a paused container")
		}
		return nil
	}}
	if err := d.Terminate(&fc.Handle{DockID: "x"}); err != nil {
		t.Fatalf("err = %v, want nil after retry", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestTerminateReportsPersistentFailure(t *testing.T) {
	d := &Driver{Runner: func(string, ...string) error { return errors.New("device or resource busy") }}
	if err := d.Terminate(&fc.Handle{DockID: "x"}); err == nil {
		t.Fatal("a container that never goes away must surface an error")
	}
}

func TestCreateSnapshotNoopForDiskOnly(t *testing.T) {
	d := &Driver{}
	if !d.DiskOnly() || !d.UseVeth() {
		t.Fatal("container driver must be veth + disk-only")
	}
	if err := d.CreateSnapshot(context.Background(), &fc.Handle{DockID: "x"}, "Full"); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

func TestSaveStateTarIsRelativeToTheVolume(t *testing.T) {
	var args []string
	d := &Driver{hostTar: func(a []string) error {
		args = a
		return nil
	}}
	if err := d.SaveState("/var/lib/docker/volumes/v/_data", "/tmp/state.tar"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	// Paths are relative to the volume root, so a restore reproduces the same
	// layout regardless of where the volume lives on the target host.
	for _, want := range []string{"-C /var/lib/docker/volumes/v/_data", "-cf /tmp/state.tar", "."} {
		if !strings.Contains(joined, want) {
			t.Fatalf("tar args = %q, want %q", joined, want)
		}
	}
}

func TestRestoreStateClearsStaleFilesFirst(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "stale.txt")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	var args []string
	d := &Driver{
		hostTar: func(a []string) error {
			args = a
			return nil
		},
		Runner: func(string, ...string) error { return nil },
	}
	if err := d.RestoreState(dir, "/tmp/state.tar"); err != nil {
		t.Fatal(err)
	}
	// A restore must not leave files the generation did not contain.
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale file survived the restore")
	}
	if !strings.Contains(strings.Join(args, " "), "-xf /tmp/state.tar") {
		t.Fatalf("tar args = %v", args)
	}
}

func TestRestoreStateOwnsTheWorkspaceAsTheDockUser(t *testing.T) {
	var chown string
	d := &Driver{
		hostTar: func([]string) error { return nil },
		Runner: func(name string, args ...string) error {
			if name == "chown" {
				chown = strings.Join(args, " ")
			}
			return nil
		},
	}
	dir := filepath.Join(t.TempDir(), "work")
	if err := d.RestoreState(dir, "/tmp/state.tar"); err != nil {
		t.Fatal(err)
	}
	// The dock runs unprivileged, so restored files must be writable by it.
	if !strings.Contains(chown, "1001:1001") || !strings.Contains(chown, dir) {
		t.Fatalf("chown = %q, want the workspace owned by the dock user", chown)
	}
}
