//go:build e2e && linux

package local

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gvPullEngine is a real engine for the gVisor pull e2e tests; it fails or skips like
// TestE2ELocalGVisorDock when gVisor cannot run here.
func gvPullEngine(t *testing.T, ctx context.Context) *Engine {
	t.Helper()
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
	return e
}

func gvExec(t *testing.T, ctx context.Context, e *Engine, ref, script string) string {
	t.Helper()
	res, err := e.Exec(ctx, ref, script, 60*time.Second)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("%q: %+v %v", script, res, err)
	}
	return res.Stdout
}

// TestE2ELocalGVisorPull imports a cloud-style ext4 disk (work files owned by uid 1000) into a
// real gVisor dock and checks content, modes, ownership, links and names after the first resume.
func TestE2ELocalGVisorPull(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	tools := needE2fs(t)
	e := gvPullEngine(t, ctx)

	src := t.TempDir()
	files := map[string]string{"secret.txt": "s3cret\n", "quoted": "quoted\n", "dot": "dot\n", "inner": "inner\n"}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(src, n), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const w = gvWorkDest
	q := debugfsQuote
	spaced := w + `/sp ace"q.txt`
	lines := []string{
		"mkdir " + q(w+"/private"),
		"write " + q(filepath.Join(src, "secret.txt")) + " " + q(w+"/secret.txt"),
		"write " + q(filepath.Join(src, "quoted")) + " " + q(spaced),
		"write " + q(filepath.Join(src, "dot")) + " " + q(w+"/.hidden"),
		"write " + q(filepath.Join(src, "inner")) + " " + q(w+"/private/inner.txt"),
		"symlink " + q(w+"/link") + " secret.txt",
	}
	meta := func(path string, mode uint32) {
		lines = append(lines, "sif "+q(path)+" uid 1000", "sif "+q(path)+" gid 1000", "sif "+q(path)+fmt.Sprintf(" mode 0%o", mode))
	}
	meta(w, 0o40755)
	meta(w+"/private", 0o40700)
	meta(w+"/secret.txt", 0o100600)
	meta(spaced, 0o100644)
	meta(w+"/.hidden", 0o100644)
	meta(w+"/private/inner.txt", 0o100600)
	meta(w+"/link", 0o120777)
	lines = append(lines, "sif "+q(w+"/secret.txt")+" mtime @1600000000")
	img := tools.newBaseImage(t)
	_, stderr := runTool(t, tools.debugfs, "-w", "-f", writeScript(t, lines), img)
	if errs := debugfsErrors(stderr); len(errs) > 0 {
		// The base image already has /home/user/work, which the script does not recreate.
		t.Fatalf("building the disk: %v", errs)
	}

	d, err := e.ImportWork(ctx, ImportWork{Name: "e2epull", RawPath: img, CPUs: 1, MemoryMiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), d.ID) })
	if d.State != "archived" || d.Runtime != RuntimeGVisor {
		t.Fatalf("an imported dock is stopped: %+v", d)
	}
	if st, err := e.Resume(ctx, "e2epull"); err != nil || st.State != "ready" {
		t.Fatalf("resume: %+v %v", st, err)
	}
	got := gvExec(t, ctx, e, "e2epull", `cd /home/user/work && stat -c '%a %u:%g %Y %n' -- . secret.txt 'sp ace"q.txt' .hidden private private/inner.txt && readlink link && cat secret.txt 'sp ace"q.txt' .hidden private/inner.txt && stat -c '%a %u:%g %F' link && ls -A`)
	for _, want := range []string{
		"755 1001:1001 ", " .\n",
		"600 1001:1001 1600000000 secret.txt",
		`644 1001:1001 `, `sp ace"q.txt`,
		" .hidden", "700 1001:1001 ", " private\n", "600 1001:1001 ", " private/inner.txt",
		"secret.txt\ns3cret\nquoted\ndot\ninner\n",
		"777 1001:1001 symbolic link",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, ".stage-") {
		t.Errorf("the stage directory must be gone:\n%s", got)
	}
	// The volume root belongs to the dock user, and files made by the dock are its own.
	gvExec(t, ctx, e, "e2epull", `touch /home/user/work/new-file && test "$(stat -c %u /home/user/work/new-file)" = 1001`)
	// Stop and resume again: the work directory is the volume, not the import.
	if _, err := e.Stop(ctx, "e2epull", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Resume(ctx, "e2epull"); err != nil {
		t.Fatal(err)
	}
	gvExec(t, ctx, e, "e2epull", `test -f /home/user/work/new-file && test -f /home/user/work/secret.txt`)
}

// TestE2ELocalGVisorPullNoWorkdir imports a disk that has no /home/user/work.
func TestE2ELocalGVisorPullNoWorkdir(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	tools := needE2fs(t)
	e := gvPullEngine(t, ctx)
	img := filepath.Join(t.TempDir(), "disk.ext4")
	runTool(t, tools.mkfs, "-q", "-F", "-b", "4096", "-I", "256", img, "64M")

	d, err := e.ImportWork(ctx, ImportWork{Name: "e2eempty", RawPath: img, CPUs: 1, MemoryMiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), d.ID) })
	if _, err := e.Resume(ctx, "e2eempty"); err != nil {
		t.Fatal(err)
	}
	if out := gvExec(t, ctx, e, "e2eempty", `ls -A /home/user/work | wc -l; touch /home/user/work/f && stat -c %u /home/user/work/f`); strings.TrimSpace(out) != "0\n1001" {
		t.Fatalf("an empty, writable work directory expected: %q", out)
	}
}

// TestE2ELocalGVisorPullBadDisk fails cleanly on a file that is not an ext4 image.
func TestE2ELocalGVisorPullBadDisk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	e := gvPullEngine(t, ctx)
	bad := filepath.Join(t.TempDir(), "disk.raw")
	if err := os.WriteFile(bad, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ImportWork(ctx, ImportWork{Name: "e2ebad", RawPath: bad}); err == nil {
		t.Fatal("a disk that is not ext4 must fail")
	}
	if docks, _ := e.List(ctx); len(docks) != 0 {
		t.Fatalf("a failed import leaves no dock: %+v", docks)
	}
	if out, _, _ := e.gvisor().docker(ctx, nil, "volume", "ls", "-q", "--filter", "name=dock-local-"); strings.TrimSpace(out) != "" {
		t.Fatalf("a failed import leaves no volume: %q", out)
	}
}

func writeScript(t *testing.T, lines []string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
