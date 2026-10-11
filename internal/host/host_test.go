package host

import (
	"context"
	"errors"
	stdnet "net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/usedock/docklet/api/v1"
	"github.com/usedock/docklet/internal/fc"
	"github.com/usedock/docklet/internal/snapmgr"
	"github.com/usedock/docklet/internal/stats"
	"github.com/usedock/docklet/internal/store"
)

type fakeDriver struct {
	mu         sync.Mutex // refill boots concurrently
	boots      int
	paused     int
	resumed    int
	terminated int
	snaps      int
	lastRes    apiv1.Resources
	pauseErr   error
	snapErr    error
	adoptPid   int
}

func (f *fakeDriver) Boot(_ context.Context, spec fc.BootSpec) (*fc.Handle, error) {
	f.mu.Lock()
	f.boots++
	f.lastRes = spec.Resources
	f.mu.Unlock()
	return &fc.Handle{DockID: spec.DockID, Pid: 4242}, nil
}
func (f *fakeDriver) Pause(_ context.Context, _ *fc.Handle) error {
	f.paused++
	return f.pauseErr
}
func (f *fakeDriver) Resume(_ context.Context, _ *fc.Handle) error { f.resumed++; return nil }
func (f *fakeDriver) Adopt(_ string, _ string, _ apiv1.Resources) (*fc.Handle, error) {
	return &fc.Handle{Pid: f.adoptPid}, nil
}
func (f *fakeDriver) Terminate(_ *fc.Handle) error {
	f.mu.Lock()
	f.terminated++
	f.mu.Unlock()
	return nil
}
func (f *fakeDriver) CreateSnapshot(_ context.Context, h *fc.Handle, _ string) error {
	f.snaps++
	if f.snapErr != nil {
		return f.snapErr
	}
	memPath, snapPath := f.SnapshotPaths(h)
	if err := os.WriteFile(memPath, []byte("mem"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(snapPath, []byte("state"), 0o644)
}
func (f *fakeDriver) SnapshotPaths(h *fc.Handle) (string, string) {
	dir := filepath.Join(os.TempDir(), "snaptest", h.DockID)
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "mem"), filepath.Join(dir, "state")
}

type fakeMount struct{ root string }

func (f fakeMount) Mount(_ string, target string, _ bool) error {
	f.root = target
	return os.MkdirAll(filepath.Join(target, "etc"), 0o755)
}
func (f fakeMount) Unmount(_ string) error { return nil }

// Disk sizes in these tests are MiB, not GiB: the fake snapshot path reads and
// compresses the whole rootfs, which is minutes and tens of GiB at real sizes.
func init() {
	growDisk = func(rootfs string, gib int) error {
		if want := int64(gib) << 20; want > 0 {
			info, err := os.Stat(rootfs)
			if err != nil {
				return err
			}
			if want > info.Size() {
				return os.Truncate(rootfs, want)
			}
		}
		return nil
	}
}

func testHost(t *testing.T) (*Host, *fakeDriver, string) {
	t.Helper()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.ext4")
	if err := os.WriteFile(base, []byte("base-image"), 0o644); err != nil {
		t.Fatal(err)
	}
	driver := &fakeDriver{}
	runner := func(name string, args ...string) error {
		if name == "ip" || name == "iptables" || name == "sh" || name == "sysctl" {
			return nil
		}
		if name != "cp" {
			return errors.New("unexpected " + name)
		}
		raw, err := os.ReadFile(args[len(args)-2])
		if err != nil {
			return err
		}
		return os.WriteFile(args[len(args)-1], raw, 0o644)
	}
	proc := filepath.Join(dir, "proc")
	os.MkdirAll(filepath.Join(proc, "4242"), 0o755)
	os.WriteFile(filepath.Join(proc, "4242", "cgroup"), []byte("0::/machine-d1\n"), 0o644)
	cgroup := filepath.Join(dir, "sys/fs/cgroup/machine-d1")
	os.MkdirAll(cgroup, 0o755)
	os.WriteFile(filepath.Join(cgroup, "cpu.stat"), []byte("usage_usec 100000\n"), 0o644)
	h, err := New(Config{
		HostID: "h1", Family: "fam-1",
		ImageBase: base, KernelImage: filepath.Join(dir, "vmlinux"),
		VMsDir:         filepath.Join(dir, "vms"),
		FirecrackerBin: "fc", JailerBin: "jailer", JailBase: filepath.Join(dir, "jail"),
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
	return h, driver, dir
}

var (
	resSmall   = apiv1.Resources{CPUs: 1, MemoryMiB: 2048, DiskGiB: 10}
	resDefault = apiv1.Resources{CPUs: 2, MemoryMiB: 4096, DiskGiB: 20}
	resLarge   = apiv1.Resources{CPUs: 4, MemoryMiB: 8192, DiskGiB: 40}
	resXLarge  = apiv1.Resources{CPUs: 8, MemoryMiB: 16384, DiskGiB: 80}
)

func TestProvisionBootsAndTracks(t *testing.T) {
	ctx := context.Background()
	h, driver, _ := testHost(t)
	ip, err := h.Provision(ctx, "dock-1", resDefault, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "192.168.127.2" || driver.boots != 1 || driver.lastRes != resDefault {
		t.Fatalf("ip=%s boots=%d resources=%+v", ip, driver.boots, driver.lastRes)
	}
	if len(h.Running()) != 1 || h.Running()[0].DockID != "dock-1" {
		t.Fatalf("running = %+v", h.Running())
	}
}

func TestProvisionEnforcesResourceCgroupLimits(t *testing.T) {
	ctx := context.Background()
	h, _, dir := testHost(t)
	if _, err := h.Provision(ctx, "dock-1", resSmall, "", ""); err != nil {
		t.Fatal(err)
	}
	cgroup := filepath.Join(dir, "sys/fs/cgroup/machine-d1")
	raw, err := os.ReadFile(filepath.Join(cgroup, "cpu.max"))
	if err != nil || string(raw) != "100000 100000\n" {
		t.Fatalf("cpu.max = %q,%v, want 1 vCPU quota", raw, err)
	}
	raw, err = os.ReadFile(filepath.Join(cgroup, "memory.max"))
	if err != nil || string(raw) != "2147483648\n" {
		t.Fatalf("memory.max = %q,%v, want 2048MiB", raw, err)
	}
}

func TestProvisionInvalidResourcesFails(t *testing.T) {
	h, driver, _ := testHost(t)
	if _, err := h.Provision(context.Background(), "d", apiv1.Resources{}, "", ""); !errors.Is(err, fc.ErrInvalidResources) {
		t.Fatalf("err = %v, want ErrInvalidResources", err)
	}
	if driver.boots != 0 {
		t.Fatalf("boots = %d, invalid resources must not boot", driver.boots)
	}
}

func TestProvisionImageDefaultsAndRejectsOthers(t *testing.T) {
	h, _, _ := testHost(t)
	if _, err := h.Provision(context.Background(), "d1", resSmall, apiv1.DefaultImage, ""); err != nil {
		t.Fatalf("default image: %v", err)
	}
	if _, err := h.Provision(context.Background(), "d2", resSmall, "ubuntu-22.04", ""); !errors.Is(err, ErrImageUnavailable) {
		t.Fatalf("err = %v, want ErrImageUnavailable", err)
	}
}

func TestProvisionGrowsDiskToRequestedSize(t *testing.T) {
	h, _, dir := testHost(t)
	if _, err := h.Provision(context.Background(), "d1", resDefault, "", ""); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "jail", "firecracker", "d1", "root", fc.RootfsName))
	if err != nil || info.Size() != int64(resDefault.DiskGiB)<<20 {
		t.Fatalf("rootfs = %v, %v; want %d (scaled MiB)", info, err, resDefault.DiskGiB)
	}
}

func mustHost(t *testing.T) *Host {
	h, _, _ := testHost(t)
	return h
}

func TestSnapshotStopAndResumeCycle(t *testing.T) {
	ctx := context.Background()
	h, driver, _ := testHost(t)
	if _, err := h.Provision(ctx, "dock-1", resSmall, "", ""); err != nil {
		t.Fatal(err)
	}
	id, size, family, err := h.SnapshotCreate(ctx, "dock-1", nil, apiv1.SnapshotDiff, false, false)
	if err != nil || id == "" || size <= 0 || family != "fam-1" {
		t.Fatalf("capture = %s %d %s %v", id, size, family, err)
	}
	if driver.paused != 1 || driver.resumed != 0 {
		t.Fatalf("stop must leave paused (paused=%d resumed=%d)", driver.paused, driver.resumed)
	}
	ip, err := h.SnapshotLoad(ctx, "dock-1", resSmall, id)
	if err != nil || ip == "" {
		t.Fatalf("load = %s %v", ip, err)
	}
	if driver.lastRes != resSmall {
		t.Fatalf("resume must keep resources, got %+v", driver.lastRes)
	}
}

func TestLatestRefIsRefused(t *testing.T) {
	ctx := context.Background()
	h, _, _ := testHost(t)
	if _, err := h.Provision(ctx, "dock-1", resSmall, "", ""); err != nil {
		t.Fatal(err)
	}
	first, _, _, err := h.SnapshotCreate(ctx, "dock-1", nil, apiv1.SnapshotDiff, false, false)
	if err != nil {
		t.Fatal(err)
	}
	second, _, _, err := h.SnapshotCreate(ctx, "dock-1", nil, apiv1.SnapshotDiff, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("generations must differ")
	}
	ip, err := h.SnapshotLoad(ctx, "dock-1", resSmall, second)
	if err != nil || ip == "" {
		t.Fatalf("explicit load = %s %v", ip, err)
	}
	// The host-local pointer is gone: "latest" is refused even when generations exist.
	if _, err := h.SnapshotLoad(ctx, "dock-1", resSmall, "latest"); !errors.Is(err, ErrLatestRefused) {
		t.Fatalf("latest load err = %v, want ErrLatestRefused", err)
	}
	if _, err := h.SnapshotLoad(ctx, "dock-1", resSmall, ""); err == nil {
		t.Fatal("empty snapshot id must be refused")
	}
	if _, err := h.Provision(ctx, "dock-2", resSmall, "", "latest"); !errors.Is(err, ErrLatestRefused) {
		t.Fatalf("latest provision err = %v, want ErrLatestRefused", err)
	}
}

func TestSnapshotBranchResumesSource(t *testing.T) {
	ctx := context.Background()
	h, driver, _ := testHost(t)
	if _, err := h.Provision(ctx, "src", resDefault, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h.SnapshotCreate(ctx, "src", nil, apiv1.SnapshotFull, true, true); err != nil {
		t.Fatal(err)
	}
	if driver.snaps != 1 || driver.resumed != 1 {
		t.Fatalf("branch capture takes memory and resumes (snaps=%d resumed=%d)", driver.snaps, driver.resumed)
	}
}

func TestDestroyFreesSlot(t *testing.T) {
	ctx := context.Background()
	h, driver, _ := testHost(t)
	if _, err := h.Provision(ctx, "dock-1", resDefault, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.Destroy(ctx, "dock-1"); err != nil {
		t.Fatal(err)
	}
	if driver.terminated != 1 || len(h.Running()) != 0 {
		t.Fatal("destroy must terminate and untrack")
	}
	ip, err := h.Provision(ctx, "dock-2", resDefault, "", "")
	if err != nil || ip != "192.168.127.2" {
		t.Fatalf("IP must recycle after destroy: %s %v", ip, err)
	}
}

func TestStatsFromTrackedVM(t *testing.T) {
	ctx := context.Background()
	h, _, dir := testHost(t)
	if _, err := h.Provision(ctx, "dock-1", resDefault, "", ""); err != nil {
		t.Fatal(err)
	}
	// The first sample only marks the counter; the next reports usage since it.
	sample, err := h.Stats(ctx, "dock-1")
	if err != nil || sample.Source != "host" || sample.CPUPercent != 0 {
		t.Fatalf("first sample = %+v %v", sample, err)
	}
	cgroup := filepath.Join(dir, "sys/fs/cgroup/machine-d1")
	if err := os.WriteFile(filepath.Join(cgroup, "cpu.stat"), []byte("usage_usec 900000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sample, err = h.Stats(ctx, "dock-1")
	if err != nil || sample.CPUPercent <= 0 || sample.CPUPercent > 100 {
		t.Fatalf("second sample = %+v %v, want a rate in (0, 100]", sample, err)
	}
	if sample.DiskUsedBytes <= 0 {
		t.Fatalf("DiskUsedBytes = %d, want the rootfs image's allocated blocks", sample.DiskUsedBytes)
	}
	if _, err := h.Stats(ctx, "ghost"); err == nil || !strings.Contains(err.Error(), "no tracked microVM") {
		t.Fatalf("untracked dock must fail, got %v", err)
	}
}

func TestGateFilesWriteNetworkUnit(t *testing.T) {
	dir := t.TempDir()
	if err := writeGateFiles(dir, "192.168.127.2", "192.168.127.1", "24", "AA:FC:00:00:00:01"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "etc", "systemd", "network", "10-dock.network"))
	if err != nil {
		t.Fatal(err)
	}
	want := "[Match]\nMACAddress=AA:FC:00:00:00:01\n\n[Network]\nAddress=192.168.127.2/24\nGateway=192.168.127.1\nDNS=1.1.1.1\nDHCP=no\n"
	if string(raw) != want {
		t.Fatalf("unit = %q", raw)
	}
}

func TestGateFilesResetMachineID(t *testing.T) {
	dir := t.TempDir()
	for _, stale := range []string{"etc/machine-id", "var/lib/dbus/machine-id"} {
		full := filepath.Join(dir, stale)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeGateFiles(dir, "192.168.127.9", "192.168.127.1", "24", "AA:FC:00:00:00:02"); err != nil {
		t.Fatal(err)
	}
	for _, stale := range []string{"etc/machine-id", "var/lib/dbus/machine-id"} {
		if _, err := os.Stat(filepath.Join(dir, stale)); !os.IsNotExist(err) {
			t.Fatalf("%s must be removed", stale)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "etc", "systemd", "network", "10-dock.network"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "MACAddress=AA:FC:00:00:00:02") || !strings.Contains(string(raw), "Address=192.168.127.9/24") {
		t.Fatalf("unit = %q", raw)
	}
}

func TestRescanRetracksLiveSockets(t *testing.T) {
	dir := t.TempDir()
	jailBase := filepath.Join(dir, "jail")
	live := filepath.Join(jailBase, "firecracker", "dock-live", "root")
	dead := filepath.Join(jailBase, "firecracker", "dock-dead", "root")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dead, 0o755); err != nil {
		t.Fatal(err)
	}
	listener, err := stdnet.Listen("unix", filepath.Join(live, "api.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.WriteFile(filepath.Join(live, "spec.json"), []byte(`{"dockId":"dock-live","resources":{"cpus":1,"memoryMiB":2048,"diskGiB":10}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	driver := &fakeDriver{adoptPid: 4242}
	h, err := New(Config{
		HostID: "h1", Family: "fam-1",
		ImageBase: filepath.Join(dir, "base.ext4"),
		VMsDir:    filepath.Join(dir, "vms"), Subnet: "192.168.127.0/24",
		UID: os.Getuid(), GID: os.Getgid(),
		Driver: driver, Snaps: &snapmgr.Coordinator{},
		Stats: stats.Reader{}, NetRunner: func(name string, args ...string) error { return nil },
		Mount: fakeMount{}, JailBase: jailBase,
	}, filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	running := h.Running()
	if len(running) != 1 || running[0].DockID != "dock-live" {
		t.Fatalf("running = %+v", running)
	}
	if h.vms["dock-live"].pid != 4242 {
		t.Fatalf("adopted pid = %d, want 4242", h.vms["dock-live"].pid)
	}
	if again := h.Rescan(); len(again) != 0 {
		t.Fatalf("rescan must skip tracked VMs, adopted %v", again)
	}
}

func TestFreezeOnlyNeedsNoVM(t *testing.T) {
	ctx := context.Background()
	h, _, dir := testHost(t)
	jail := filepath.Join(dir, "jail", "firecracker", "dock-9", "root")
	if err := os.MkdirAll(jail, 0o755); err != nil {
		t.Fatal(err)
	}
	rootfs := filepath.Join(jail, "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("disk-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	// point host jail base at our tree for this call
	h.cfg.JailBase = filepath.Join(dir, "jail")
	fake, ok := h.cfg.Driver.(*fakeDriver)
	if !ok {
		t.Fatal("test host must use the fake driver")
	}
	fake.pauseErr = fc.ErrNoMicroVM
	id, size, _, err := h.SnapshotCreate(ctx, "dock-9", nil, apiv1.SnapshotFull, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if id == "" || size <= 0 {
		t.Fatalf("freeze-only capture = %s %d %v", id, size, err)
	}
	if fake.snaps != 0 {
		t.Fatal("fallback must skip memory capture for a dead VM")
	}
}

func TestTerminalCaptureStopsTracking(t *testing.T) {
	ctx := context.Background()
	h, driver, _ := testHost(t)
	if _, err := h.Provision(ctx, "dock-1", resSmall, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h.SnapshotCreate(ctx, "dock-1", nil, apiv1.SnapshotFull, false, false); err != nil {
		t.Fatal(err)
	}
	if len(h.Running()) != 0 {
		t.Fatal("terminal capture must untrack the stopped VM")
	}
	if driver.terminated != 1 {
		t.Fatalf("terminal capture must Terminate exactly once: %d", driver.terminated)
	}
}

func TestRunningMarksOnlyEphemeralRuntimesFast(t *testing.T) {
	ctx := context.Background()
	h, _, _ := testHost(t)
	stubAgent(t, h)
	if _, err := h.Provision(ctx, "dock-1", resDefault, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ProvisionWithAgent(ctx, "agent-dock", resDefault, "", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ProvisionFast(ctx, "fast-1", resDefault, "", "tok", "/home/user/work", nil); err != nil {
		t.Fatal(err)
	}
	byID := map[string]string{}
	for _, vm := range h.Running() {
		byID[vm.DockID] = vm.Kind
	}
	// Agent docks live in the docks table, so the reconciler must still see them as docks.
	if byID["dock-1"] != "dock" || byID["agent-dock"] != "dock" || byID["fast-1"] != "fast" {
		t.Fatalf("kinds = %v", byID)
	}
}

func TestStableTapAndMac(t *testing.T) {
	if TapFor("d1") != TapFor("d1") || len(TapFor("d1")) > 15 {
		t.Fatal("tap names must be stable and short")
	}
	if MacFor("d1") == MacFor("d2") || len(MacFor("d1")) != 17 {
		t.Fatal("macs must differ per dock")
	}
}

func TestBaseKernelArgsSkipsCloudInit(t *testing.T) {
	for _, want := range []string{"cloud-init=disabled", "ds=nocloud"} {
		if !strings.Contains(baseKernelArgs, want) {
			t.Fatalf("kernel args must contain %q, got %q", want, baseKernelArgs)
		}
	}
}

// A dock whose lock is held (Destroy, SnapshotLoad) is not adopted by Rescan; a later pass adopts it.
func TestRescanSkipsLockedDockAndAdoptsAfterRelease(t *testing.T) {
	dir := t.TempDir()
	jailBase := filepath.Join(dir, "jail")
	live := filepath.Join(jailBase, "firecracker", "dock-live", "root")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	listener, err := stdnet.Listen("unix", filepath.Join(live, "api.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.WriteFile(filepath.Join(live, "spec.json"), []byte(`{"dockId":"dock-live","resources":{"cpus":1,"memoryMiB":2048,"diskGiB":10}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{
		HostID: "h1", Family: "fam-1",
		ImageBase: filepath.Join(dir, "base.ext4"),
		VMsDir:    filepath.Join(dir, "vms"), Subnet: "192.168.127.0/24",
		UID: os.Getuid(), GID: os.Getgid(),
		Driver: &fakeDriver{adoptPid: 4242}, Snaps: &snapmgr.Coordinator{},
		Stats: stats.Reader{}, NetRunner: func(name string, args ...string) error { return nil },
		Mount: fakeMount{}, JailBase: jailBase,
	}, filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.vms = map[string]*vm{}
	h.mu.Unlock()
	unlock := h.lockDock("dock-live")
	if got := h.Rescan(); len(got) != 0 {
		t.Fatalf("rescan adopted %v while the dock lock was held", got)
	}
	unlock()
	if got := h.Rescan(); len(got) != 1 || got[0] != "dock-live" {
		t.Fatalf("adopted = %v, want [dock-live] after release", got)
	}
}
