package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/usedock/docklet/api/v1"
	"github.com/usedock/docklet/internal/exec"
	"github.com/usedock/docklet/internal/fc"
	"github.com/usedock/docklet/internal/uid"
)

type specDriver struct {
	fakeDriver
	smu   sync.Mutex
	specs []fc.BootSpec
}

func (d *specDriver) Boot(ctx context.Context, spec fc.BootSpec) (*fc.Handle, error) {
	d.smu.Lock()
	d.specs = append(d.specs, spec)
	d.smu.Unlock()
	return d.fakeDriver.Boot(ctx, spec)
}

type chownCall struct {
	path     string
	uid, gid int
}

type isolated struct {
	h      *Host
	drv    *specDriver
	chowns []chownCall
	cmds   []string
	state  string
}

// isolatedHost is testHost with per-dock uids, recorded chowns and recorded network commands.
func isolatedHost(t *testing.T) *isolated {
	t.Helper()
	h, _, dir := testHost(t)
	iso := &isolated{h: h, drv: &specDriver{}, state: filepath.Join(dir, "state")}
	h.cfg.Driver = iso.drv
	h.cfg.Limits = fc.Limits{DiskMBps: fc.LimitAuto, DiskIOPS: fc.LimitAuto, NetMBps: fc.LimitAuto, PidsLimit: fc.LimitAuto}
	var err error
	if h.uids, err = uid.Open(iso.state, 2000000000, 65536); err != nil {
		t.Fatal(err)
	}
	h.cfg.Chown = func(p string, u, g int) error {
		iso.chowns = append(iso.chowns, chownCall{p, u, g})
		return nil
	}
	inner := h.cfg.NetRunner
	h.cfg.NetRunner = exec.Runner(func(name string, args ...string) error {
		iso.cmds = append(iso.cmds, name+" "+strings.Join(args, " "))
		return inner(name, args...)
	})
	return iso
}

func (i *isolated) rootfsOwner(t *testing.T, dockID string) chownCall {
	t.Helper()
	want := filepath.Join(i.h.jailRoot(dockID), fc.RootfsName)
	for _, c := range i.chowns {
		if c.path == want {
			return c
		}
	}
	t.Fatalf("rootfs of %s never chowned: %+v", dockID, i.chowns)
	return chownCall{}
}

func TestProvisionGivesEachDockItsOwnUID(t *testing.T) {
	ctx := context.Background()
	iso := isolatedHost(t)
	for _, id := range []string{"dock-1", "dock-2"} {
		if _, err := iso.h.Provision(ctx, id, resDefault, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if len(iso.drv.specs) != 2 {
		t.Fatalf("boots = %d", len(iso.drv.specs))
	}
	a, b := iso.drv.specs[0], iso.drv.specs[1]
	if a.UID != a.GID || b.UID != b.GID {
		t.Fatalf("uid and gid must match: %d/%d %d/%d", a.UID, a.GID, b.UID, b.GID)
	}
	if a.UID == b.UID {
		t.Fatalf("docks share uid %d", a.UID)
	}
	for _, s := range iso.drv.specs {
		if s.UID < 2000000000 || s.UID >= 2000000000+65536 {
			t.Fatalf("uid %d outside the reserved range", s.UID)
		}
		if got := iso.rootfsOwner(t, s.DockID); got.uid != s.UID || got.gid != s.GID {
			t.Fatalf("%s rootfs owned %d:%d, boot spec %d:%d", s.DockID, got.uid, got.gid, s.UID, s.GID)
		}
		tap := fmt.Sprintf("ip tuntap add dev %s mode tap user %d", TapFor(s.DockID), s.UID)
		if !strings.Contains(strings.Join(iso.cmds, "\n"), tap) {
			t.Fatalf("no %q in %v", tap, iso.cmds)
		}
	}
}

func TestProvisionPassesLimitsToBoot(t *testing.T) {
	iso := isolatedHost(t)
	if _, err := iso.h.Provision(context.Background(), "dock-1", resDefault, "", ""); err != nil {
		t.Fatal(err)
	}
	got := iso.drv.specs[0].Limits
	want := fc.Effective{DiskMBps: 200, DiskIOPS: 4000, NetMBps: 50, PidsLimit: 4096}
	if got != want {
		t.Fatalf("limits = %+v, want %+v", got, want)
	}
}

func TestResumeKeepsUIDAcrossRestartAndRechownsDisk(t *testing.T) {
	ctx := context.Background()
	iso := isolatedHost(t)
	if _, err := iso.h.Provision(ctx, "dock-1", resDefault, "", ""); err != nil {
		t.Fatal(err)
	}
	first := iso.drv.specs[0].UID
	// Simulate a docklet restart: a fresh allocator over the same state dir.
	reopened, err := uid.Open(iso.state, 2000000000, 65536)
	if err != nil {
		t.Fatal(err)
	}
	iso.h.uids = reopened
	iso.h.mu.Lock()
	delete(iso.h.vms, "dock-1")
	iso.h.mu.Unlock()
	iso.chowns = nil
	if _, err := iso.h.Provision(ctx, "dock-1", resDefault, "", ""); err != nil {
		t.Fatal(err)
	}
	second := iso.drv.specs[1]
	if second.UID != first {
		t.Fatalf("resume uid %d != original %d", second.UID, first)
	}
	if got := iso.rootfsOwner(t, "dock-1"); got.uid != first {
		t.Fatalf("resumed disk owned by %d, want %d", got.uid, first)
	}
}

func TestDestroyFreesUID(t *testing.T) {
	ctx := context.Background()
	iso := isolatedHost(t)
	if _, err := iso.h.Provision(ctx, "dock-1", resDefault, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := iso.h.uids.Get("dock-1"); !ok {
		t.Fatal("provision must record the dock's uid")
	}
	if err := iso.h.Destroy(ctx, "dock-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := iso.h.uids.Get("dock-1"); ok {
		t.Fatal("destroy must free the uid")
	}
}

func TestForkDiskOwnedByNewDockUID(t *testing.T) {
	ctx := context.Background()
	iso := isolatedHost(t)
	if _, err := iso.h.Provision(ctx, "src", resDefault, "", ""); err != nil {
		t.Fatal(err)
	}
	snapID, _, _, err := iso.h.SnapshotCreate(ctx, "src", nil, apiv1.SnapshotDiff, false, false)
	if err != nil {
		t.Fatal(err)
	}
	iso.chowns = nil
	if _, err := iso.h.Provision(ctx, "fork", resDefault, "", snapID); err != nil {
		t.Fatal(err)
	}
	src, fork := iso.drv.specs[0], iso.drv.specs[len(iso.drv.specs)-1]
	if fork.DockID != "fork" || fork.UID == src.UID {
		t.Fatalf("fork spec = %+v, want its own uid distinct from %d", fork, src.UID)
	}
	if got := iso.rootfsOwner(t, "fork"); got.uid != fork.UID {
		t.Fatalf("forked disk owned by %d, want the new dock's %d", got.uid, fork.UID)
	}
}

func TestLegacySharedUIDWithoutRange(t *testing.T) {
	h, _, _ := testHost(t)
	u, g, err := h.dockOwner("dock-1")
	if err != nil || u != h.cfg.UID || g != h.cfg.GID {
		t.Fatalf("owner = %d:%d,%v; want shared %d:%d", u, g, err, h.cfg.UID, h.cfg.GID)
	}
}

// A failed boot preserves its jail as root.failed with a disk owned by the dock's
// uid; destroy must remove it before the uid goes to another dock.
func TestDestroyRemovesFailedJailBeforeFreeingUID(t *testing.T) {
	ctx := context.Background()
	iso := isolatedHost(t)
	if _, err := iso.h.Provision(ctx, "dock-1", resDefault, "", ""); err != nil {
		t.Fatal(err)
	}
	failed := iso.h.jailRoot("dock-1") + ".failed"
	if err := os.MkdirAll(failed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(failed, fc.RootfsName), []byte("old disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := iso.h.Destroy(ctx, "dock-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Fatalf("failed jail survived destroy: %v", err)
	}
	if _, ok := iso.h.uids.Get("dock-1"); ok {
		t.Fatal("destroy must free the uid")
	}
}

// cgroupFixture makes a jailer cgroup dir for a dock holding one process.
func cgroupFixture(t *testing.T, h *Host, dockID string, withKill bool) (dir string) {
	t.Helper()
	dir = filepath.Join(h.cfg.CgroupBase, "firecracker", dockID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte("4242\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if withKill {
		if err := os.WriteFile(filepath.Join(dir, "cgroup.kill"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDestroyKeepsUIDWhileCgroupHasProcesses(t *testing.T) {
	ctx := context.Background()
	iso := isolatedHost(t)
	iso.h.cfg.CgroupWait = 150 * time.Millisecond
	if _, err := iso.h.Provision(ctx, "dock-1", resDefault, "", ""); err != nil {
		t.Fatal(err)
	}
	dir := cgroupFixture(t, iso.h, "dock-1", true)
	if err := iso.h.Destroy(ctx, "dock-1"); err == nil {
		t.Fatal("destroy must report a cgroup that cannot be emptied")
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "cgroup.kill")); string(raw) != "1" {
		t.Fatalf("cgroup.kill = %q, want 1", raw)
	}
	if _, ok := iso.h.uids.Get("dock-1"); !ok {
		t.Fatal("uid freed while the cgroup still had processes")
	}
	// The processes die; a retry frees the uid and removes the cgroup.
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := iso.h.Destroy(ctx, "dock-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := iso.h.uids.Get("dock-1"); ok {
		t.Fatal("uid must be freed once the cgroup is empty")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("cgroup survived: %v", err)
	}
}

func TestDestroyFreesUIDAfterCgroupEmpties(t *testing.T) {
	ctx := context.Background()
	iso := isolatedHost(t)
	iso.h.cfg.CgroupWait = 5 * time.Second
	if _, err := iso.h.Provision(ctx, "dock-1", resDefault, "", ""); err != nil {
		t.Fatal(err)
	}
	dir := cgroupFixture(t, iso.h, "dock-1", true)
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(dir, "cgroup.procs"), nil, 0o644)
	}()
	if err := iso.h.Destroy(ctx, "dock-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := iso.h.uids.Get("dock-1"); ok {
		t.Fatal("uid must be freed after the cgroup empties")
	}
}

func TestDestroyKillsListedPidsWithoutCgroupKill(t *testing.T) {
	ctx := context.Background()
	iso := isolatedHost(t)
	iso.h.cfg.CgroupWait = 2 * time.Second
	if _, err := iso.h.Provision(ctx, "dock-1", resDefault, "", ""); err != nil {
		t.Fatal(err)
	}
	dir := cgroupFixture(t, iso.h, "dock-1", false)
	var killed []int
	iso.h.cfg.KillPID = func(pid int) error {
		killed = append(killed, pid)
		return os.WriteFile(filepath.Join(dir, "cgroup.procs"), nil, 0o644)
	}
	if err := iso.h.Destroy(ctx, "dock-1"); err != nil {
		t.Fatal(err)
	}
	if len(killed) != 1 || killed[0] != 4242 {
		t.Fatalf("killed = %v, want [4242]", killed)
	}
	if _, ok := iso.h.uids.Get("dock-1"); ok {
		t.Fatal("uid must be freed")
	}
}

// Lchown never follows a symlink; os.Chown on a dangling link would fail.
func TestChownDoesNotFollowSymlinks(t *testing.T) {
	h, _, dir := testHost(t)
	link := filepath.Join(dir, "rootfs-link")
	if err := os.Symlink(filepath.Join(dir, "missing"), link); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := h.chown(link, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("chown followed the symlink: %v", err)
	}
}
