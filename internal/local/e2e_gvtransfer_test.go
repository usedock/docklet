//go:build e2e && linux

package local

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var (
	debugfsOwnerRE = regexp.MustCompile(`User:\s+(\d+)\s+Group:\s+(\d+)`)
	debugfsModeRE  = regexp.MustCompile(`Mode:\s+(\d+)`)
	debugfsInodeRE = regexp.MustCompile(`Inode:\s+(\d+)`)
	debugfsLinksRE = regexp.MustCompile(`Links:\s+(\d+)`)
)

// TestE2ELocalGVisorFlatten pushes a real gVisor dock's work directory onto the pinned Dockyard
// base: Flatten stops the dock, builds the raw disk with the e2fsprogs helper, and the result is
// checked with debugfs and e2fsck on the runner. It needs Docker, runsc, e2fsprogs and the
// Dockyard release; it skips when Docker or runsc is missing unless DOCK_E2E_REQUIRE_GVISOR is set.
func TestE2ELocalGVisorFlatten(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	t.Setenv("DOCK_DATA_DIR", t.TempDir())
	e, err := NewEngine(os.Getenv, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.gvisor().Preflight(ctx); err != nil {
		if os.Getenv("DOCK_E2E_REQUIRE_GVISOR") != "" {
			t.Fatalf("gvisor docks cannot run here: %v", err)
		}
		t.Skipf("gvisor docks cannot run here: %v", err)
	}
	tools := needE2fs(t)

	d, err := e.New(ctx, Spec{Name: "e2egvpush", Runtime: RuntimeGVisor, CPUs: 1, MemoryMiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), d.ID) })
	res, err := e.Exec(ctx, "e2egvpush", `set -e
cd /home/user/work
mkdir -p sub private
chmod 700 private
printf secret > private/key.txt
chmod 600 private/key.txt
printf hello > sub/hello.txt
ln -s sub/hello.txt link
printf hard > h1
ln h1 h2
printf space > "a b.txt"
printf quote > "it's \"q\".txt"
chmod 755 sub
ln -s /x/disk.raw esc-disk
ln -s /x/script esc-script
ln -s /proc/self/environ esc-proc
ln -s ../../../../x/disk.raw esc-up
ln -s /x esc-dir
printf dots > ..dots
printf hash > "#hash"
printf tq > 'q" "x'`, 60*time.Second)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("make the tree: %+v %v", res, err)
	}

	dir, err := e.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	flat, err := e.Flatten(ctx, "e2egvpush", dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if flat.Image != DefaultImage || flat.DiskGiB != DefaultDiskGiB || filepath.Dir(flat.RawPath) != dir {
		t.Fatalf("flat %+v", flat)
	}
	if rec, _ := e.Lookup("e2egvpush"); e.aliveRec(ctx, rec) {
		t.Fatal("--stop must stop the dock")
	}

	debugfs := func(cmd string) string {
		t.Helper()
		out, _ := runTool(t, tools.debugfs, "-R", cmd, flat.RawPath)
		return out
	}
	stat := func(path string) (uid, gid, mode, inode, links string) {
		t.Helper()
		out := debugfs("stat " + debugfsQuote(path))
		o, m, i, l := debugfsOwnerRE.FindStringSubmatch(out), debugfsModeRE.FindStringSubmatch(out), debugfsInodeRE.FindStringSubmatch(out), debugfsLinksRE.FindStringSubmatch(out)
		if o == nil || m == nil || i == nil || l == nil {
			t.Fatalf("stat %s: %q", path, out)
		}
		return o[1], o[2], m[1], i[1], l[1]
	}
	w := gvWorkDest + "/"
	for _, c := range []struct{ path, mode, content string }{
		{gvWorkDest, "0755", ""},
		{w + "private", "0700", ""},
		{w + "sub", "0755", ""},
		{w + "private/key.txt", "0600", "secret"},
		{w + "sub/hello.txt", "0644", "hello"},
		{w + "a b.txt", "0644", "space"},
		{w + `it's "q".txt`, "0644", "quote"},
		{w + "..dots", "0644", "dots"},
		{w + "#hash", "0644", "hash"},
		{w + `q" "x`, "0644", "tq"},
	} {
		uid, gid, mode, _, _ := stat(c.path)
		if uid != "1000" || gid != "1000" || !strings.HasSuffix(mode, c.mode) {
			t.Errorf("%s: owner %s:%s mode %s, want 1000:1000 %s", c.path, uid, gid, mode, c.mode)
		}
		if c.content != "" {
			if got := debugfs("cat " + debugfsQuote(c.path)); got != c.content {
				t.Errorf("%s: content %q, want %q", c.path, got, c.content)
			}
		}
	}
	if out := debugfs("stat " + debugfsQuote(w+"link")); !strings.Contains(out, "sub/hello.txt") || !strings.Contains(out, "symlink") {
		t.Errorf("symlink: %q", out)
	}
	// The helper's debugfs write opens /work/<path> itself; a symlink must stay a symlink and
	// never pull in the transfer directory, the disk being built or the helper's /proc.
	for name, target := range map[string]string{"esc-disk": "/x/disk.raw", "esc-script": "/x/script",
		"esc-proc": "/proc/self/environ", "esc-up": "../../../../x/disk.raw", "esc-dir": "/x"} {
		out := debugfs("stat " + debugfsQuote(w+name))
		if !strings.Contains(out, "Type: symlink") || !strings.Contains(out, `"`+target+`"`) {
			t.Errorf("%s must stay a symlink to %s: %q", name, target, out)
		}
	}
	if out := debugfs("ls " + debugfsQuote(w+"esc-dir")); strings.Contains(out, "disk.raw") || strings.Contains(out, "script") {
		t.Errorf("a symlinked directory must not be followed: %q", out)
	}
	if ents, err := os.ReadDir(dir); err != nil || len(ents) != 3 {
		t.Errorf("the transfer directory must hold only disk.raw, script and debugfs.err: %v %v", ents, err)
	}
	_, _, _, i1, l1 := stat(w + "h1")
	_, _, _, i2, l2 := stat(w + "h2")
	if i1 != i2 || l1 != "2" || l2 != "2" {
		t.Errorf("hard link: inodes %s/%s links %s/%s", i1, i2, l1, l2)
	}
	if got := debugfs("cat " + debugfsQuote(w+"h2")); got != "hard" {
		t.Errorf("hard link content %q", got)
	}
	// The Dockyard base is intact.
	if out := debugfs("stat /etc/os-release"); !strings.Contains(out, "regular") && !strings.Contains(out, "symlink") {
		t.Errorf("the base image has no /etc/os-release: %q", out)
	}
	// e2fsck -fn exits 0 only on a clean filesystem.
	runTool(t, tools.e2fsck, "-fn", flat.RawPath)

	// A work volume that is gone (container and volume pruned) fails the push instead of
	// pushing an empty work directory, and is not recreated.
	g := e.gvisor()
	if _, stderr, err := g.docker(ctx, nil, "rm", "-f", gvContainer(d.ID)); err != nil {
		t.Fatalf("docker rm: %v %s", err, stderr)
	}
	if _, stderr, err := g.docker(ctx, nil, "volume", "rm", gvVolume(d.ID)); err != nil {
		t.Fatalf("docker volume rm: %v %s", err, stderr)
	}
	dir2, err := e.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir2) })
	if _, err := e.Flatten(ctx, "e2egvpush", dir2, true); ErrCode(err) != "push_failed" || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("a missing work volume must fail the push: %v", err)
	}
	if _, _, err := g.docker(ctx, nil, "volume", "inspect", gvVolume(d.ID)); err == nil {
		t.Fatal("a failed push must not create the work volume")
	}
}
