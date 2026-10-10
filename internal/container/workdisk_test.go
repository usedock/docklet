package container

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dockexec "github.com/usedock/docklet/internal/exec"
	"github.com/usedock/docklet/internal/fc"
)

// fakeSys fakes the host commands behind the work disk: it tracks mounts and
// creates the image file on truncate, and passes everything else to inner.
type fakeSys struct {
	log     []string
	mounted map[string]bool
	inner   dockexec.Runner
	failOn  string
	// before runs ahead of each faked command, to observe state mid-build.
	before func(line string)
}

func (f *fakeSys) run(name string, args ...string) error {
	line := strings.Join(append([]string{name}, args...), " ")
	if f.before != nil {
		f.before(line)
	}
	if f.failOn != "" && strings.HasPrefix(line, f.failOn) {
		return errors.New("forced failure")
	}
	last := ""
	if len(args) > 0 {
		last = args[len(args)-1]
	}
	switch name {
	case "truncate":
		f.log = append(f.log, line)
		return os.WriteFile(last, nil, 0o600)
	case "mkfs.ext4", "chown", "cp":
		f.log = append(f.log, line)
		return nil
	case "mount":
		f.log = append(f.log, line)
		f.mounted[last] = true
		return nil
	case "umount":
		f.log = append(f.log, line)
		delete(f.mounted, last)
		return nil
	case "mountpoint":
		f.log = append(f.log, line)
		if !f.mounted[last] {
			return errors.New("not a mountpoint")
		}
		return nil
	}
	if f.inner != nil {
		return f.inner(name, args...)
	}
	f.log = append(f.log, line)
	return nil
}

func noLegacyVolume([]string) ([]byte, error) {
	return nil, errors.New("Error: No such volume: x")
}

// useDisk points d at a fake work-disk host, keeping the original runner for other commands.
func useDisk(d *Driver, state string) *fakeSys {
	f := &fakeSys{mounted: map[string]bool{}, inner: d.Runner}
	d.StateDir = state
	d.Runner = f.run
	d.dockerOut = noLegacyVolume
	return f
}

func newDiskDriver(t *testing.T) (*Driver, *fakeSys) {
	t.Helper()
	d := &Driver{Image: "img", InspectPID: func(string) (int, error) { return 4242, nil }}
	return d, useDisk(d, t.TempDir())
}

func TestEnsureWorkDiskCreatesFormatsAndMounts(t *testing.T) {
	d, f := newDiskDriver(t)
	dir, err := d.EnsureWorkDisk("d1", 7)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(d.StateDir, "docks", "d1")
	img, mnt := filepath.Join(root, "work.img"), filepath.Join(root, "work")
	if dir != mnt {
		t.Fatalf("dir = %q, want %q", dir, mnt)
	}
	want := []string{
		"mountpoint -q " + mnt,
		"truncate -s 7G " + img + ".tmp",
		"mkfs.ext4 -F -q " + img + ".tmp",
		"mountpoint -q " + mnt,
		"mount -o loop,nosuid,nodev " + img + ".tmp " + mnt,
		"chown 1001:1001 " + mnt,
		"mountpoint -q " + mnt,
	}
	if strings.Join(f.log, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(f.log, "\n"), strings.Join(want, "\n"))
	}
	if !fileExists(img) || fileExists(img+".tmp") {
		t.Fatal("the formatted image must replace its temp name")
	}
}

func TestEnsureWorkDiskIsIdempotent(t *testing.T) {
	d, f := newDiskDriver(t)
	if _, err := d.EnsureWorkDisk("d1", 7); err != nil {
		t.Fatal(err)
	}
	f.log = nil
	if _, err := d.EnsureWorkDisk("d1", 7); err != nil {
		t.Fatal(err)
	}
	// Already created and mounted: only the mount probe runs.
	if len(f.log) != 1 || !strings.HasPrefix(f.log[0], "mountpoint -q") {
		t.Fatalf("calls = %q, want only a mountpoint probe", f.log)
	}
	// After an unmount (stop) the existing image is remounted, not rebuilt.
	if err := d.unmountWork("d1"); err != nil {
		t.Fatal(err)
	}
	f.log = nil
	if _, err := d.EnsureWorkDisk("d1", 7); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.log {
		if strings.HasPrefix(c, "truncate") || strings.HasPrefix(c, "mkfs") {
			t.Fatalf("existing image was rebuilt: %q", f.log)
		}
	}
	if !strings.HasPrefix(f.log[len(f.log)-1], "mount -o loop") {
		t.Fatalf("calls = %q, want a remount", f.log)
	}
}

func TestEnsureWorkDiskFailedMkfsLeavesNoImage(t *testing.T) {
	d, f := newDiskDriver(t)
	f.failOn = "mkfs.ext4"
	if _, err := d.EnsureWorkDisk("d1", 7); err == nil {
		t.Fatal("mkfs failure must fail the ensure")
	}
	if d.WorkDiskExists("d1") {
		t.Fatal("a half-built image must not pass for a disk")
	}
}

func TestEnsureWorkDiskRejectsBadInput(t *testing.T) {
	d, _ := newDiskDriver(t)
	if _, err := d.EnsureWorkDisk("d1", 0); err == nil {
		t.Fatal("a zero-size disk must be refused")
	}
	if _, err := (&Driver{}).EnsureWorkDisk("d1", 1); err == nil {
		t.Fatal("a driver without a state dir must refuse")
	}
}

func TestTerminateUnmountsTheWorkDisk(t *testing.T) {
	d, f := newDiskDriver(t)
	if _, err := d.EnsureWorkDisk("d1", 2); err != nil {
		t.Fatal(err)
	}
	if err := d.Terminate(&fc.Handle{DockID: "d1"}); err != nil {
		t.Fatal(err)
	}
	if f.mounted[d.workMount("d1")] {
		t.Fatal("stop must unmount the work disk")
	}
	if !fileExists(d.workImage("d1")) {
		t.Fatal("stop must keep the image for resume")
	}
	// A second terminate with nothing mounted is still fine.
	if err := d.Terminate(&fc.Handle{DockID: "d1"}); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveWorkDiskUnmountsBeforeDeleting(t *testing.T) {
	d, f := newDiskDriver(t)
	if _, err := d.EnsureWorkDisk("d1", 2); err != nil {
		t.Fatal(err)
	}
	f.log = nil
	if err := d.RemoveWorkDisk("d1"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range f.log {
		if c == "umount "+d.workMount("d1") {
			found = true
		}
	}
	if !found {
		t.Fatalf("calls = %q, want an umount", f.log)
	}
	if _, err := os.Stat(d.workRoot("d1")); !os.IsNotExist(err) {
		t.Fatal("delete must remove the disk directory of the dock")
	}
	if !strings.Contains(strings.Join(f.log, "\n"), "docker volume rm "+WorkVolume("d1")) {
		t.Fatalf("calls = %q, want the legacy volume dropped", f.log)
	}
}

func TestRemoveWorkDiskKeepsFilesWhenStillMounted(t *testing.T) {
	d, f := newDiskDriver(t)
	if _, err := d.EnsureWorkDisk("d1", 2); err != nil {
		t.Fatal(err)
	}
	f.failOn = "umount"
	if err := d.RemoveWorkDisk("d1"); err == nil {
		t.Fatal("an unmountable disk must fail the removal")
	}
	if !fileExists(d.workImage("d1")) {
		t.Fatal("nothing may be deleted while the disk is still mounted")
	}
}

func TestEnsureWorkDiskMigratesLegacyVolume(t *testing.T) {
	d, f := newDiskDriver(t)
	d.dockerOut = func([]string) ([]byte, error) {
		return []byte("/var/lib/docker/volumes/dock-d1-work/_data\n"), nil
	}
	if _, err := d.EnsureWorkDisk("d1", 3); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.log, "\n")
	for _, want := range []string{
		"cp -a /var/lib/docker/volumes/dock-d1-work/_data/. " + d.workMount("d1") + "/",
		"docker volume rm " + WorkVolume("d1"),
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("calls:\n%s\nwant %q", joined, want)
		}
	}
	// The migration runs once: the image exists afterwards.
	f.log = nil
	if _, err := d.EnsureWorkDisk("d1", 3); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(f.log, "\n"), "cp -a") {
		t.Fatal("migration must not repeat")
	}
}

func TestEnsureWorkDiskNamesTheImageOnlyAfterInit(t *testing.T) {
	d, f := newDiskDriver(t)
	d.dockerOut = func([]string) ([]byte, error) { return []byte("/legacy\n"), nil }
	f.before = func(line string) {
		if (strings.HasPrefix(line, "chown") || strings.HasPrefix(line, "cp")) && d.WorkDiskExists("d1") {
			t.Errorf("image named before init finished (%q): a crash here would skip chown or migration for good", line)
		}
	}
	if _, err := d.EnsureWorkDisk("d1", 3); err != nil {
		t.Fatal(err)
	}
	if !d.WorkDiskExists("d1") {
		t.Fatal("a finished build must name the image")
	}
}

func TestEnsureWorkDiskRecoversFromCrashMidMigration(t *testing.T) {
	d, f := newDiskDriver(t)
	d.dockerOut = func([]string) ([]byte, error) { return []byte("/legacy\n"), nil }
	// A docklet killed during cp leaves the temp image mounted and no named image.
	img, mnt := d.workImage("d1"), d.workMount("d1")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(img+".tmp", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.mounted[mnt] = true
	if _, err := d.EnsureWorkDisk("d1", 3); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.log, "\n")
	for _, want := range []string{"umount " + mnt, "mkfs.ext4", "cp -a /legacy/. " + mnt + "/", "docker volume rm " + WorkVolume("d1")} {
		if !strings.Contains(joined, want) {
			t.Fatalf("calls:\n%s\nwant %q", joined, want)
		}
	}
	if strings.Index(joined, "umount "+mnt) > strings.Index(joined, "mount -o loop") {
		t.Fatalf("the stale temp mount must go before the rebuilt disk mounts:\n%s", joined)
	}
	if !d.WorkDiskExists("d1") || fileExists(img+".tmp") {
		t.Fatal("the rebuilt image must replace its temp name")
	}
}

func TestEnsureWorkDiskFailedMigrationLeavesNoImage(t *testing.T) {
	d, f := newDiskDriver(t)
	d.dockerOut = func([]string) ([]byte, error) { return []byte("/legacy\n"), nil }
	f.failOn = "cp"
	if _, err := d.EnsureWorkDisk("d1", 3); err == nil {
		t.Fatal("a failed migration must fail the ensure")
	}
	if d.WorkDiskExists("d1") || fileExists(d.workImage("d1")+".tmp") {
		t.Fatal("a failed migration must not leave an empty image that hides the old data")
	}
	if f.mounted[d.workMount("d1")] {
		t.Fatal("a failed migration must unmount the temp image")
	}
}

func TestWorkDirMountsTheImageForCapture(t *testing.T) {
	d, f := newDiskDriver(t)
	if _, err := d.EnsureWorkDisk("d1", 2); err != nil {
		t.Fatal(err)
	}
	if err := d.unmountWork("d1"); err != nil {
		t.Fatal(err)
	}
	dir, err := d.WorkDir("d1")
	if err != nil || dir != d.workMount("d1") || !f.mounted[dir] {
		t.Fatalf("dir = %q err = %v mounted = %v", dir, err, f.mounted[dir])
	}
}

func TestWorkDirFallsBackToLegacyVolume(t *testing.T) {
	d, _ := newDiskDriver(t)
	d.dockerOut = func([]string) ([]byte, error) { return []byte("/legacy/_data\n"), nil }
	dir, err := d.WorkDir("nodisk")
	if err != nil || dir != "/legacy/_data" {
		t.Fatalf("dir = %q err = %v", dir, err)
	}
}

func TestWorkVolumeName(t *testing.T) {
	if got := WorkVolume("dock-1"); got != "dock-dock-1-work" {
		t.Fatalf("volume = %q, want the id-derived name", got)
	}
}

func TestSaveStateSkipsLostAndFound(t *testing.T) {
	var args []string
	d := &Driver{hostTar: func(a []string) error { args = a; return nil }}
	if err := d.SaveState("/w", "/t.tar"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, " "), "--exclude=./lost+found") {
		t.Fatalf("tar args = %v", args)
	}
}

func TestRestoreStateKeepsTheMountpointDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(filepath.Join(dir, "lost+found"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	d := &Driver{hostTar: func([]string) error { return nil }, Runner: func(string, ...string) error { return nil }}
	if err := d.RestoreState(dir, "/t.tar"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "lost+found")); err != nil {
		t.Fatal("restore must keep the filesystem's lost+found")
	}
	if _, err := os.Stat(filepath.Join(dir, "old")); !os.IsNotExist(err) {
		t.Fatal("restore must clear stale files")
	}
}
