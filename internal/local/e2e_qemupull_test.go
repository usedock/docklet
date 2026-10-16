//go:build e2e && linux

package local

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/usedock/docklet/internal/dockyard"
)

// TestE2ELocalDockImportWorkTar unpacks a work tar made the way the fleet host makes one (GNU tar
// -C <workdir> --exclude=./lost+found --owner=1001 --group=1001 -cf) into a real QEMU dock and
// checks content, owner, modes, mtime, symlink, hard link and the missing FIFO after the import,
// and that the import drive is gone after a restart.
func TestE2ELocalDockImportWorkTar(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	t.Setenv("DOCK_DATA_DIR", t.TempDir())
	e, err := NewEngine(os.Getenv, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	skip := t.Skipf
	if os.Getenv("DOCK_E2E_REQUIRE") != "" && dockyard.DockyardIndexURL != "" {
		skip = t.Fatalf
	}
	if err := e.Driver.Preflight(ctx); err != nil {
		skip("local docks cannot run here: %v", err)
	}
	if _, err := e.ResolveImage(ctx, DefaultImage); err != nil {
		skip("no Dockyard image available: %v", err)
	}
	for _, tool := range []string{"tar", "mkfifo"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is needed to build the work tar: %v", tool, err)
		}
	}

	src := t.TempDir()
	write := func(rel, content string, mode os.FileMode) string {
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const mtime = 1700000000
	f := write("hello.txt", "hello\n", 0o640)
	if err := os.Chtimes(f, time.Unix(mtime, 0), time.Unix(mtime, 0)); err != nil {
		t.Fatal(err)
	}
	write("bin/run.sh", "#!/bin/sh\necho ran\n", 0o755)
	write("secret/key", "k\n", 0o600)
	if err := os.Chmod(filepath.Join(src, "secret"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hello.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(f, filepath.Join(src, "hard.txt")); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("mkfifo", filepath.Join(src, "pipe")).CombinedOutput(); err != nil {
		t.Fatalf("mkfifo: %v %s", err, out)
	}
	tarPath := filepath.Join(t.TempDir(), "work.tar")
	if out, err := exec.Command("tar", "-C", src, "--exclude=./lost+found", "--owner=1001", "--group=1001", "-cf", tarPath, ".").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v %s", err, out)
	}

	d, err := e.ImportWorkTarQEMU(ctx, WorkTarQEMU{Name: "e2eworktar", TarPath: tarPath, CPUs: 2, MemoryMiB: 1024, DiskGiB: 6})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), d.ID) })
	if d.State != "archived" {
		t.Fatalf("an imported dock is stopped: %+v", d)
	}
	rec := mustLookup(t, e, "e2eworktar")
	if _, err := os.Stat(e.file(rec.ID, gvImportingMarker)); err == nil {
		t.Fatal("the importing marker must be gone")
	}
	if _, err := e.Resume(ctx, "e2eworktar"); err != nil {
		t.Fatal(err)
	}
	run := func(script string) string {
		t.Helper()
		res, err := e.Exec(ctx, "e2eworktar", script, 60*time.Second)
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("%s: %+v %v", script, res, err)
		}
		return strings.TrimSpace(res.Stdout)
	}
	w := "/home/user/work"
	if got := run("cat " + w + "/hello.txt"); got != "hello" {
		t.Fatalf("content %q", got)
	}
	// Owner user (1000), mode, mtime.
	for file, want := range map[string]string{
		w + "/hello.txt":  "user 1000 640 " + "1700000000",
		w + "/bin/run.sh": "user 1000 755 ",
		w + "/secret":     "user 1000 700 ",
		w + "/secret/key": "user 1000 600 ",
		w:                 "user 1000 ",
	} {
		got := run("stat -c '%U %u %a %Y' " + file)
		if !strings.HasPrefix(got, want) {
			t.Fatalf("%s: stat %q, want prefix %q", file, got, want)
		}
	}
	if got := run("stat -c '%U:%G' " + w + "/hello.txt"); got != "user:user" {
		t.Fatalf("group %q", got)
	}
	if got := run("readlink " + w + "/link && stat -c '%U' " + w + "/link"); got != "hello.txt\nuser" {
		t.Fatalf("symlink %q", got)
	}
	if got := run("stat -c %i " + w + "/hello.txt " + w + "/hard.txt | sort -u | wc -l"); got != "1" {
		t.Fatalf("hard.txt must share its inode with hello.txt: %q", got)
	}
	if got := run("test ! -e " + w + "/pipe && echo gone"); got != "gone" {
		t.Fatalf("the FIFO must be skipped: %q", got)
	}
	if got := run("find " + w + " ! -user user | wc -l"); got != "0" {
		t.Fatalf("every file belongs to user, not %q extra", got)
	}
	// The import drive is on the first boot only. The base disk is the only virtio disk now.
	if got := run("ls /sys/block | grep -c '^vd'"); got != "1" {
		t.Fatalf("after a restart only the root disk may exist, got %q virtio disks", got)
	}
	if got := run("sudo -n sh -c 'cat /sys/block/vd*/serial 2>/dev/null | grep -c " + ImportDriveSerial + " || true'"); got != "0" {
		t.Fatalf("the import drive must be gone: %q", got)
	}
	if _, err := e.Stop(ctx, "e2eworktar", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Resume(ctx, "e2eworktar"); err != nil {
		t.Fatal(err)
	}
	if got := run("ls /sys/block | grep -c '^vd'; cat " + w + "/secret/key"); got != "1\nk" {
		t.Fatalf("after another restart: %q", got)
	}
}
