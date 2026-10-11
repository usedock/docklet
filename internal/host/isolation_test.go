package host

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
