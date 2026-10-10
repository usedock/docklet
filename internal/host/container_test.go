package host

import (
	"archive/tar"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/usedock/docklet/api/v1"
	"github.com/usedock/docklet/internal/disk"
	"github.com/usedock/docklet/internal/fc"
	"github.com/usedock/docklet/internal/net"
	"github.com/usedock/docklet/internal/snapmgr"
	"github.com/usedock/docklet/internal/stats"
	"github.com/usedock/docklet/internal/store"
)

// fakeVethDriver mimics the container driver: veth networking, disk-only
// generations, pid-tracked handles.
type fakeVethDriver struct {
	root       string
	boots      int
	terminated int
	paused     int
	resumed    int
	saved      int
	applied    int
	disks      map[string]int
	removed    []string
	mounts     int
	terminates []string
}

// writeTestImage writes size bytes of a deterministic pattern.
func writeTestImage(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	block := make([]byte, disk.BlockSize)
	for i := range block {
		block[i] = byte(i)
	}
	for off := int64(0); off < size; off += int64(len(block)) {
		n := int64(len(block))
		if rem := size - off; rem < n {
			n = rem
		}
		if _, err := f.Write(block[:n]); err != nil {
			t.Fatal(err)
		}
	}
}

func writeTestTar(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeVethDriver) Boot(_ context.Context, spec fc.BootSpec) (*fc.Handle, error) {
	f.boots++
	if spec.AttachNetwork == nil {
		return nil, errors.New("container boot without a network attacher")
	}
	if err := spec.AttachNetwork(4242); err != nil {
		return nil, err
	}
	return &fc.Handle{DockID: spec.DockID, JailRoot: spec.JailRoot, Pid: 4242}, nil
}
func (f *fakeVethDriver) Adopt(_ string, jailRoot string, _ apiv1.Resources) (*fc.Handle, error) {
	return &fc.Handle{JailRoot: jailRoot, Pid: 4242}, nil
}
func (f *fakeVethDriver) Pause(_ context.Context, _ *fc.Handle) error  { f.paused++; return nil }
func (f *fakeVethDriver) Resume(_ context.Context, _ *fc.Handle) error { f.resumed++; return nil }
func (f *fakeVethDriver) CreateSnapshot(_ context.Context, _ *fc.Handle, _ string) error {
	return nil
}
func (f *fakeVethDriver) SnapshotPaths(h *fc.Handle) (string, string) {
	return filepath.Join(h.JailRoot, "snap.mem"), filepath.Join(h.JailRoot, "snap.state")
}
func (f *fakeVethDriver) Terminate(h *fc.Handle) error {
	f.terminated++
	f.terminates = append(f.terminates, h.DockID)
	return nil
}
func (f *fakeVethDriver) UseVeth() bool  { return true }
func (f *fakeVethDriver) DiskOnly() bool { return true }

// WorkDir/EnsureWorkDisk/SaveState/RestoreState fake the workspace-state
// capture the container backend uses: the fake writes one workspace file into
// the state tar and records that a restore happened.
func (f *fakeVethDriver) WorkDir(dockID string) (string, error) {
	f.mounts++
	dir := filepath.Join(f.root, "volumes", "vol-"+dockID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// EnsureWorkDisk records the size each dock's disk was created with.
func (f *fakeVethDriver) EnsureWorkDisk(dockID string, diskGiB int) (string, error) {
	if f.disks == nil {
		f.disks = map[string]int{}
	}
	f.disks[dockID] = diskGiB
	return f.WorkDir(dockID)
}

func (f *fakeVethDriver) WorkDiskExists(dockID string) bool {
	_, ok := f.disks[dockID]
	return ok
}

func (f *fakeVethDriver) RemoveWorkDisk(dockID string) error {
	delete(f.disks, dockID)
	f.removed = append(f.removed, dockID)
	return nil
}

func (f *fakeVethDriver) SaveState(dir, dest string) error {
	f.saved++
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "exported.txt"), []byte("exported"), 0o644); err != nil {
		return err
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(out)
	if err := tw.WriteHeader(&tar.Header{Name: "exported.txt", Mode: 0o644, Size: 8}); err != nil {
		return err
	}
	if _, err := tw.Write([]byte("exported")); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return out.Close()
}

func (f *fakeVethDriver) RestoreState(dir, _ string) error {
	f.applied++
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "restored.txt"), []byte("restored"), 0o644)
}

func testContainerHost(t *testing.T) (*Host, *fakeVethDriver, string, *[]string) {
	t.Helper()
	dir := t.TempDir()
	driver := &fakeVethDriver{root: dir}
	var calls []string
	runner := func(name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name == "cp" {
			raw, err := os.ReadFile(args[len(args)-2])
			if err != nil {
				return err
			}
			return os.WriteFile(args[len(args)-1], raw, 0o644)
		}
		return nil
	}
	proc := filepath.Join(dir, "proc")
	_ = os.MkdirAll(filepath.Join(proc, "4242"), 0o755)
	_ = os.WriteFile(filepath.Join(proc, "4242", "cgroup"), []byte("0::/machine-d1\n"), 0o644)
	cgroup := filepath.Join(dir, "sys/fs/cgroup/machine-d1")
	_ = os.MkdirAll(cgroup, 0o755)
	_ = os.WriteFile(filepath.Join(cgroup, "cpu.stat"), []byte("usage_usec 100000\n"), 0o644)
	h, err := New(Config{
		HostID: "h-container", Family: "fam-1",
		ImageBase: filepath.Join(dir, "base.ext4"), KernelImage: "",
		VMsDir:         filepath.Join(dir, "vms"),
		FirecrackerBin: "", JailerBin: "", JailBase: filepath.Join(dir, "jail"),
		CgroupBase: filepath.Join(dir, "sys/fs/cgroup"), Subnet: "192.168.127.0/24",
		UID: os.Getuid(), GID: os.Getgid(),
		Driver: driver,
		Snaps: &snapmgr.Coordinator{
			Driver: driver, Store: store.NewMemStore(),
			VMsDir: filepath.Join(dir, "vms"), GensDir: filepath.Join(dir, "gens"),
			HostFamily: "fam-1", Runner: runner,
		},
		Stats:     stats.Reader{Root: dir, Proc: proc, CgroupMount: filepath.Join(dir, "sys/fs/cgroup")},
		NetRunner: runner,
		Mount:     fakeMount{},
	}, filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	return h, driver, dir, &calls
}

func TestContainerProvisionUsesVeth(t *testing.T) {
	ctx := context.Background()
	h, driver, dir, calls := testContainerHost(t)
	ip, err := h.Provision(ctx, "dock-1", resSmall, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "192.168.127.2" || driver.boots != 1 {
		t.Fatalf("ip=%s boots=%d", ip, driver.boots)
	}
	joined := strings.Join(*calls, "\n")
	for _, want := range []string{"link add dv-", "netns 4242", "name eth0"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("calls missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "tuntap") {
		t.Fatalf("container provision must not touch TAP:\n%s", joined)
	}
	// Tracked under the veth name with the dock limits applied.
	h.mu.Lock()
	v := h.vms["dock-1"]
	h.mu.Unlock()
	if v == nil || v.tap != net.VethFor("dock-1") {
		t.Fatalf("tracked = %+v, want veth %s", v, net.VethFor("dock-1"))
	}
	raw, err := os.ReadFile(filepath.Join(dir, "sys/fs/cgroup/machine-d1", "cpu.max"))
	if err != nil || string(raw) != "100000 100000\n" {
		t.Fatalf("cpu.max = %q,%v, want 1 vCPU quota", raw, err)
	}
}

func TestContainerStatsAreHostTruth(t *testing.T) {
	ctx := context.Background()
	h, _, _, _ := testContainerHost(t)
	if _, err := h.Provision(ctx, "dock-1", resSmall, "", ""); err != nil {
		t.Fatal(err)
	}
	sample, err := h.Stats(ctx, "dock-1")
	if err != nil {
		t.Fatal(err)
	}
	if sample.Source != "host" || sample.DockID != "dock-1" {
		t.Fatalf("sample = %+v, want host truth", sample)
	}
}

func TestContainerDiskUsedIsWorkspaceVolumeAndCached(t *testing.T) {
	ctx := context.Background()
	h, driver, _, _ := testContainerHost(t)
	if _, err := h.Provision(ctx, "dock-1", resSmall, "", ""); err != nil {
		t.Fatal(err)
	}
	vol, _ := driver.WorkDir("dock-1")
	if err := os.WriteFile(filepath.Join(vol, "data"), make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := h.Stats(ctx, "dock-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.DiskUsedBytes < 1<<20 {
		t.Fatalf("DiskUsedBytes = %d, want at least the 1 MiB workspace file", first.DiskUsedBytes)
	}
	if err := os.WriteFile(filepath.Join(vol, "more"), make([]byte, 4<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := h.Stats(ctx, "dock-1")
	if err != nil {
		t.Fatal(err)
	}
	if second.DiskUsedBytes != first.DiskUsedBytes {
		t.Fatalf("DiskUsedBytes = %d within the TTL, want cached %d", second.DiskUsedBytes, first.DiskUsedBytes)
	}
}

func TestContainerDestroyDeletesVeth(t *testing.T) {
	ctx := context.Background()
	h, driver, _, calls := testContainerHost(t)
	if _, err := h.Provision(ctx, "dock-1", resSmall, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.Destroy(ctx, "dock-1"); err != nil {
		t.Fatal(err)
	}
	if driver.terminated != 1 {
		t.Fatalf("terminated = %d", driver.terminated)
	}
	if joined := strings.Join(*calls, "\n"); !strings.Contains(joined, "link del dv-") {
		t.Fatalf("calls missing veth delete:\n%s", joined)
	}
}

func TestContainerSkipsMemoryCapture(t *testing.T) {
	h, _, _, _ := testContainerHost(t)
	if !h.diskOnly() || !h.usesVeth() {
		t.Fatal("veth driver must report veth + disk-only")
	}
	microvm, _, _ := testHost(t)
	if microvm.diskOnly() || microvm.usesVeth() {
		t.Fatal("SDK driver must stay TAP + memory-capable")
	}
}

func TestContainerStopResumeRoundTripKeepsFiles(t *testing.T) {
	ctx := context.Background()
	h, driver, dir, _ := testContainerHost(t)
	if _, err := h.Provision(ctx, "dock-1", resSmall, "", ""); err != nil {
		t.Fatal(err)
	}
	snap, size, family, err := h.SnapshotCreate(ctx, "dock-1", nil, "full", false, false)
	if err != nil || snap == "" || size <= 0 || family != "fam-1" {
		t.Fatalf("capture = %s %d %s %v", snap, size, family, err)
	}
	// Capture reads the live container: freezing it would make the changed
	// paths unreachable, and reboot semantics only need the files.
	if driver.saved != 1 || driver.paused != 0 || driver.terminated != 1 {
		t.Fatalf("saved=%d paused=%d terminated=%d", driver.saved, driver.paused, driver.terminated)
	}
	h.mu.Lock()
	_, tracked := h.vms["dock-1"]
	h.mu.Unlock()
	if tracked {
		t.Fatal("terminal full capture must untrack the dock")
	}
	ip, err := h.SnapshotLoad(ctx, "dock-1", resSmall, snap)
	if err != nil || ip == "" {
		t.Fatalf("load = %s %v", ip, err)
	}
	// Resume replays the captured workspace state onto the dock's volume.
	if driver.applied != 1 {
		t.Fatalf("applied = %d, want 1", driver.applied)
	}
	restored, err := os.ReadFile(filepath.Join(dir, "volumes", "vol-dock-1", "restored.txt"))
	if err != nil || string(restored) != "restored" {
		t.Fatalf("workspace restore = %q, %v", restored, err)
	}
}

func TestContainerBranchResumesSource(t *testing.T) {
	ctx := context.Background()
	h, driver, _, _ := testContainerHost(t)
	if _, err := h.Provision(ctx, "src", resSmall, "", ""); err != nil {
		t.Fatal(err)
	}
	snap, _, _, err := h.SnapshotCreate(ctx, "src", nil, "full", true, true)
	if err != nil {
		t.Fatal(err)
	}
	// A resume-after capture leaves the source running and tracked; only a
	// terminal full capture frees the host.
	if driver.terminated != 0 {
		t.Fatalf("resume-after capture terminated the source: %d", driver.terminated)
	}
	h.mu.Lock()
	_, tracked := h.vms["src"]
	h.mu.Unlock()
	if !tracked {
		t.Fatal("resume-after capture must keep the source tracked")
	}
	if snap == "" {
		t.Fatal("empty snapshot id")
	}
}

// TestMicroVMDeltaGenerationReadsWithoutMachine covers the archived-dock read path
// for a delta generation: the rootfs is rebuilt from the base, so a file
// recorded in the delta is servable without booting the machine that captured it.
func TestMicroVMDeltaGenerationReadsWithoutMachine(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.ext4")
	rootfs := filepath.Join(dir, "rootfs.ext4")
	writeTestImage(t, base, 2*disk.BlockSize)
	raw, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfs, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// A change inside the guest's filesystem, at a known offset.
	f, err := os.OpenFile(rootfs, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("guest change marker"), disk.BlockSize); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	h, _, _, _ := testContainerHost(t)
	h.cfg.Snaps.BaseImage = base
	if _, err := h.cfg.Snaps.FreezeRootfs(ctx, "dock-9", rootfs, "s9", "full", "fam-1", nil); err != nil {
		t.Fatal(err)
	}
	// The stored artifact is a delta, not a mountable image: reads must rebuild
	// the rootfs from the base rather than trusting the artifact as a filesystem.
	manifest, err := store.GetManifest(ctx, h.cfg.Snaps.Store, "s9")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 1 || manifest.Files[0].Path != "rootfs.delta.zst" {
		t.Fatalf("stored artifacts = %+v, want a single delta", manifest.Files)
	}
	if manifest.BaseFingerprint == "" {
		t.Fatal("a delta generation must record the base it was built against")
	}
	// The materialized image must carry the guest's change.
	genPath := h.cfg.Snaps.GenerationPath("s9", "rootfs.ext4")
	if err := h.cfg.Snaps.MaterializeDisk(ctx, "s9", genPath); err != nil {
		t.Fatal(err)
	}
	materialized, err := os.ReadFile(genPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(materialized[disk.BlockSize:disk.BlockSize+len("guest change marker")]) != "guest change marker" {
		t.Fatal("materialized generation lost the guest's change")
	}
}

func TestContainerGenerationReadsWithoutMachine(t *testing.T) {
	ctx := context.Background()
	h, _, _, _ := testContainerHost(t)
	tarPath := filepath.Join(t.TempDir(), "state.tar")
	writeTestTar(t, tarPath, map[string]string{"home/user/work/hello.txt": "hello"})
	manifest, err := h.cfg.Snaps.FreezeContainerState(ctx, "dock-9", tarPath, "s9", "full", "fam-1", nil)
	if err != nil || len(manifest.Files) == 0 {
		t.Fatalf("freeze = %+v %v", manifest, err)
	}
	// Archive the dock: no tracked VM remains, reads serve the store.
	entries, err := h.GenerationTree(ctx, "s9")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		found = found || e.Path == "home/user/work/hello.txt"
	}
	if !found {
		t.Fatalf("tree = %+v", entries)
	}
	raw, err := h.GenerationFile(ctx, "s9", "home/user/work/hello.txt")
	if err != nil || string(raw) != "hello" {
		t.Fatalf("file = %q,%v", raw, err)
	}
}

func TestContainerGenerationTreeListsWorkspaceRoot(t *testing.T) {
	ctx := context.Background()
	h, _, _, _ := testContainerHost(t)
	tarPath := filepath.Join(t.TempDir(), "state.tar")
	// SaveState tars the work volume itself, so paths sit at the tarball root.
	writeTestTar(t, tarPath, map[string]string{"./notes.txt": "hi", "./src/app.go": "package app"})
	if _, err := h.cfg.Snaps.FreezeContainerState(ctx, "dock-9", tarPath, "s10", "full", "fam-1", nil); err != nil {
		t.Fatal(err)
	}
	entries, err := h.GenerationTree(ctx, "s10")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Path != "notes.txt" || entries[1].Path != "src/app.go" {
		t.Fatalf("tree = %+v", entries)
	}
	raw, err := h.GenerationFile(ctx, "s10", entries[0].Path)
	if err != nil || string(raw) != "hi" {
		t.Fatalf("file = %q,%v", raw, err)
	}
}

const gib = int64(1) << 30

func TestContainerDiskAdmissionRefusesOversizedDisk(t *testing.T) {
	h, driver, _, _ := testContainerHost(t)
	h.cfg.DiskFreeBytes = func() int64 { return 5 * gib }
	_, err := h.Provision(context.Background(), "big", resSmall, "", "")
	if !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("err = %v, want capacity_exhausted for a 10 GiB disk on 5 GiB free", err)
	}
	if driver.boots != 0 || len(driver.disks) != 0 {
		t.Fatalf("boots=%d disks=%v, refused provision must not allocate", driver.boots, driver.disks)
	}
}

func TestContainerDiskAdmissionAdmitsWhatFits(t *testing.T) {
	h, _, _, _ := testContainerHost(t)
	h.cfg.DiskFreeBytes = func() int64 { return 10 * gib }
	if _, err := h.Provision(context.Background(), "ok", resSmall, "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestContainerDiskAdmissionCountsBootsInFlight(t *testing.T) {
	h, _, _, _ := testContainerHost(t)
	h.cfg.DiskFreeBytes = func() int64 { return 15 * gib }
	release, err := h.reserve("a", resSmall)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.reserve("b", resSmall); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("err = %v, want the second 10 GiB boot refused on 15 GiB free", err)
	}
	release()
	if _, err := h.reserve("b", resSmall); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

func TestContainerDiskAdmissionSkipsDocksThatOwnTheirDisk(t *testing.T) {
	h, driver, _, _ := testContainerHost(t)
	h.cfg.DiskFreeBytes = func() int64 { return 0 }
	driver.disks = map[string]int{"back": 10}
	if _, err := h.reserve("back", resSmall); err != nil {
		t.Fatalf("a resume of an existing disk allocates nothing: %v", err)
	}
}

func TestContainerDiskAdmissionOffWithoutProbe(t *testing.T) {
	h, _, _, _ := testContainerHost(t)
	if _, err := h.Provision(context.Background(), "any", resXLarge, "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestContainerDiskAdmissionDoesNotApplyToMicroVMs(t *testing.T) {
	h, _, _ := testHost(t)
	h.cfg.DiskFreeBytes = func() int64 { return 0 }
	if _, err := h.Provision(context.Background(), "mvm", resSmall, "", ""); err != nil {
		t.Fatalf("microvm rootfs is not a work disk: %v", err)
	}
}

func TestContainerBootsCreateTheDiskAtTheDockSize(t *testing.T) {
	ctx := context.Background()
	h, driver, _, _ := testContainerHost(t)
	if _, err := h.Provision(ctx, "src", resSmall, "", ""); err != nil {
		t.Fatal(err)
	}
	snap, _, _, err := h.SnapshotCreate(ctx, "src", nil, "full", true, true)
	if err != nil {
		t.Fatal(err)
	}
	// A fork restores onto its own disk sized by the fork's resources.
	if _, err := h.Provision(ctx, "fork", resLarge, "", snap); err != nil {
		t.Fatal(err)
	}
	if driver.disks["fork"] != resLarge.DiskGiB {
		t.Fatalf("fork disk = %d GiB, want %d", driver.disks["fork"], resLarge.DiskGiB)
	}
}

func TestContainerDestroyRemovesTheWorkDisk(t *testing.T) {
	ctx := context.Background()
	h, driver, _, _ := testContainerHost(t)
	if _, err := h.Provision(ctx, "dock-1", resSmall, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.Destroy(ctx, "dock-1"); err != nil {
		t.Fatal(err)
	}
	if len(driver.removed) != 1 || driver.removed[0] != "dock-1" {
		t.Fatalf("removed = %v, want the dock's work disk", driver.removed)
	}
}

// WorkDiskPath is the read-only lookup stats use; it never counts as a mount.
func (f *fakeVethDriver) WorkDiskPath(dockID string) (string, error) {
	dir := filepath.Join(f.root, "volumes", "vol-"+dockID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func TestContainerStatsNeverMountTheWorkDisk(t *testing.T) {
	ctx := context.Background()
	h, driver, _, _ := testContainerHost(t)
	if _, err := h.Provision(ctx, "dock-1", resSmall, "", ""); err != nil {
		t.Fatal(err)
	}
	driver.mounts = 0
	if _, err := h.Stats(ctx, "dock-1"); err != nil {
		t.Fatal(err)
	}
	if driver.mounts != 0 {
		t.Fatalf("stats mounted the work disk %d times, want 0", driver.mounts)
	}
}

func TestContainerDestroyOfClaimedPoolDockRemovesSlotResources(t *testing.T) {
	ctx := context.Background()
	h, driver, _, _ := testContainerHost(t)
	h.pl = newPool(2)
	stubAgent(t, h)
	slot := seedSlot(t, h, resSmall)
	h.pl.target = 0
	if _, err := h.ProvisionFast(ctx, "run-1", resSmall, "", "tok", "/home/user/work", nil); err != nil {
		t.Fatal(err)
	}
	if err := h.Destroy(ctx, "run-1"); err != nil {
		t.Fatal(err)
	}
	if len(driver.terminates) != 1 || driver.terminates[0] != slot.id {
		t.Fatalf("terminated = %v, want the slot container %s", driver.terminates, slot.id)
	}
	if len(driver.removed) != 1 || driver.removed[0] != slot.id {
		t.Fatalf("removed work disks = %v, want the slot disk %s", driver.removed, slot.id)
	}
}

func TestContainerAdmissionCountsEveryDocksAllocatedDisk(t *testing.T) {
	ctx := context.Background()
	h, driver, _, _ := testContainerHost(t)
	h.cfg.DiskTotalBytes = func() int64 { return 25 * gib }
	for _, id := range []string{"a", "b"} {
		if _, err := h.Provision(ctx, id, resSmall, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	boots := driver.boots
	if _, err := h.Provision(ctx, "c", resSmall, "", ""); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("err = %v, want capacity_exhausted: 30 GiB allocated on 25 GiB", err)
	}
	if driver.boots != boots {
		t.Fatal("refused provision must not boot")
	}
	h.cfg.DiskOvercommit = 1.5
	if _, err := h.Provision(ctx, "c", resSmall, "", ""); err != nil {
		t.Fatalf("1.5x overcommit admits 30 GiB on 25 GiB: %v", err)
	}
}

func TestMicroVMAdmissionCountsAllocatedDiskAndInFlightBoots(t *testing.T) {
	ctx := context.Background()
	h, _, _ := testHost(t)
	h.cfg.DiskTotalBytes = func() int64 { return 25 * gib }
	if _, err := h.Provision(ctx, "a", resSmall, "", ""); err != nil {
		t.Fatal(err)
	}
	release, err := h.reserve("inflight", resSmall)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Provision(ctx, "b", resSmall, "", ""); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("err = %v, want capacity_exhausted: 30 GiB allocated on 25 GiB", err)
	}
	release()
	if _, err := h.Provision(ctx, "b", resSmall, "", ""); err != nil {
		t.Fatalf("after the in-flight boot ended: %v", err)
	}
}
