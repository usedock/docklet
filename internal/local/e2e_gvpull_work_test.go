//go:build e2e && linux

package local

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestE2ELocalGVisorPullWorkTar unpacks a work tar made the way the fleet host makes one
// (GNU tar -C <workdir> --exclude=./lost+found -cf - .) into a real gVisor dock and checks
// content, modes, owner, mtimes, symlinks and hard links after the first resume.
func TestE2ELocalGVisorPullWorkTar(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	e := gvPullEngine(t, ctx)

	src := t.TempDir()
	mk := func(rel, content string, mode os.FileMode, mtime int64) {
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
		if err := os.Chtimes(p, time.Unix(mtime, 0), time.Unix(mtime, 0)); err != nil {
			t.Fatal(err)
		}
	}
	mk("secret.txt", "s3cret\n", 0o600, 1600000000)
	mk(`sp ace"q.txt`, "quoted\n", 0o644, 1600000001)
	mk(".hidden", "dot\n", 0o644, 1600000002)
	mk("private/inner.txt", "inner\n", 0o600, 1600000003)
	mk("run.sh", "#!/bin/sh\necho hi\n", 0o755, 1600000004)
	mk("a/b/c/deep.txt", "deep\n", 0o644, 1600000005)
	if err := os.Mkdir(filepath.Join(src, "empty"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, to := range map[string]string{"link": "secret.txt", "dangling": "nowhere", "abslink": "/etc/hostname"} {
		if err := os.Symlink(to, filepath.Join(src, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(src, "secret.txt"), filepath.Join(src, "hard.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(src, "lost+found"), 0o700); err != nil {
		t.Fatal(err)
	}
	tarPath := filepath.Join(t.TempDir(), "state.tar")
	// Owners 1000 as on the cloud hosts' work disks; the import maps everything to 1001.
	if out, err := exec.Command("tar", "-C", src, "--exclude=./lost+found", "--owner=1000", "--group=1000", "-cf", tarPath, ".").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v\n%s", err, out)
	}

	d, err := e.ImportWorkTar(ctx, ImportWorkTar{Name: "e2eworktar", TarPath: tarPath, CPUs: 1, MemoryMiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), d.ID) })
	if d.State != "archived" || d.Runtime != RuntimeGVisor {
		t.Fatalf("an imported dock is stopped: %+v", d)
	}
	if st, err := e.Resume(ctx, "e2eworktar"); err != nil || st.State != "ready" {
		t.Fatalf("resume: %+v %v", st, err)
	}
	got := gvExec(t, ctx, e, "e2eworktar", `cd /home/user/work && stat -c '%a %u:%g %Y %n' -- . secret.txt 'sp ace"q.txt' .hidden private private/inner.txt run.sh a/b/c/deep.txt empty hard.txt && readlink link dangling abslink && cat secret.txt 'sp ace"q.txt' .hidden private/inner.txt a/b/c/deep.txt && stat -c '%a %u:%g %F' link dangling && echo inodes $(stat -c %i secret.txt hard.txt | uniq | wc -l) && ls -A`)
	for _, want := range []string{
		"600 1001:1001 1600000000 secret.txt",
		"644 1001:1001 1600000001 sp ace\"q.txt",
		"644 1001:1001 1600000002 .hidden",
		"700 1001:1001 ", " private\n",
		"600 1001:1001 1600000003 private/inner.txt",
		"755 1001:1001 1600000004 run.sh",
		"644 1001:1001 1600000005 a/b/c/deep.txt",
		"750 1001:1001 ", " empty\n",
		"600 1001:1001 1600000000 hard.txt",
		"secret.txt\nnowhere\n/etc/hostname\n",
		"s3cret\nquoted\ndot\ninner\ndeep\n",
		"777 1001:1001 symbolic link",
		"inodes 1\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, bad := range []string{".stage-", "lost+found"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q must not be in the work directory:\n%s", bad, got)
		}
	}
	gvExec(t, ctx, e, "e2eworktar", `touch /home/user/work/new-file && test "$(stat -c %u /home/user/work/new-file)" = 1001`)
	if _, err := e.Stop(ctx, "e2eworktar", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Resume(ctx, "e2eworktar"); err != nil {
		t.Fatal(err)
	}
	gvExec(t, ctx, e, "e2eworktar", `test -f /home/user/work/new-file && test -f /home/user/work/secret.txt`)
}

// TestE2ELocalGVisorPullWorkTarCraftedArchives refuses archives that try to leave the work
// directory, write through a symlink, link to an outside file or carry device nodes, and
// leaves no dock and no volume behind.
func TestE2ELocalGVisorPullWorkTarCraftedArchives(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	e := gvPullEngine(t, ctx)
	cases := map[string][]tent{
		"dotdot":         {tdir("./"), tfile("../escape", "x")},
		"absolute":       {tdir("./"), tfile("/tmp/dock-e2e-escape", "x")},
		"symlink write":  {tdir("./"), tsym("./a", "/etc"), tfile("./a/passwd", "root::0:0")},
		"symlink dir":    {tdir("./"), tsym("./a", "/tmp"), tdir("./a/sub")},
		"hard link out":  {tdir("./"), thard("./h", "/etc/passwd")},
		"hard link dots": {tdir("./"), thard("./h", "../../etc/passwd")},
		"device":         {tdir("./"), tspecial("./null", 'c')},
		"fifo":           {tdir("./"), tspecial("./pipe", 'p')},
	}
	for name, ents := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := e.ImportWorkTar(ctx, ImportWorkTar{Name: "e2ecrafted", TarPath: writeTar(t, ents...)})
			if ErrCode(err) != "invalid_archive" {
				t.Fatalf("want invalid_archive, got %v", err)
			}
			if docks, _ := e.List(ctx); len(docks) != 0 {
				t.Fatalf("a refused import leaves no dock: %+v", docks)
			}
			if out, _, _ := e.gvisor().docker(ctx, nil, "volume", "ls", "-q", "--filter", "name=dock-local-"); strings.TrimSpace(out) != "" {
				t.Fatalf("a refused import leaves no volume: %q", out)
			}
		})
	}
	if _, err := os.Stat("/tmp/dock-e2e-escape"); err == nil {
		t.Fatal("an absolute path must never be written on the host")
	}
}

// TestE2ELocalGVisorPullWorkTarHugeDeclaredSize refuses a tar whose header declares far more
// than the cap, without reading any data.
func TestE2ELocalGVisorPullWorkTarHugeDeclaredSize(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	e := gvPullEngine(t, ctx)
	var buf bytes.Buffer
	writeHugeHeader(t, &buf)
	p := filepath.Join(t.TempDir(), "huge.tar")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ImportWorkTar(ctx, ImportWorkTar{Name: "e2ehuge", TarPath: p, MaxBytes: 1 << 20}); ErrCode(err) != "import_too_big" {
		t.Fatalf("want import_too_big, got %v", err)
	}
	if docks, _ := e.List(ctx); len(docks) != 0 {
		t.Fatalf("a refused import leaves no dock: %+v", docks)
	}
}

// TestE2ELocalGVisorPullWorkTarHelperContainsAHostileTar runs the helper itself on a raw hostile
// tar that skipped the Go check (defense in depth): nothing may change outside the volume.
func TestE2ELocalGVisorPullWorkTarHelperContainsAHostileTar(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	e := gvPullEngine(t, ctx)
	image, _, err := e.gvPrepare(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rec := Record{ID: NewID()}
	g := e.gvisor()
	if _, stderr, err := g.docker(ctx, nil, "volume", "create", gvVolume(rec.ID)); err != nil {
		t.Fatalf("volume create: %v %s", err, stderr)
	}
	t.Cleanup(func() { _ = g.removeVolume(context.Background(), rec.ID) })
	before, _ := os.ReadFile("/etc/passwd")
	raw := writeTar(t, tdir("./"), tsym("./a", "/etc"), tfile("./a/passwd", "pwned::0:0\n"), tfile("../escape", "x"), tfile("/tmp/dock-e2e-helper-escape", "x"))
	_, stderr, err := g.docker(ctx, nil, gvImportTarArgs(rec.gvRuntime(), image, rec.ID, raw, NewID(), 1<<20)...)
	t.Logf("helper on a hostile tar: err=%v stderr=%q", err, stderr)
	if after, _ := os.ReadFile("/etc/passwd"); !bytes.Equal(before, after) {
		t.Fatal("the host's /etc/passwd changed")
	}
	for _, p := range []string{"/tmp/dock-e2e-helper-escape", "/escape"} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s was written on the host", p)
		}
	}
}
