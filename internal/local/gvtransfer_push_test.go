package local

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// pushFake answers the two push helpers of a fake docker: the work listing and the disk build.
type pushFake struct {
	mu         sync.Mutex
	listing    string
	debugfsErr string // stderr debugfs "wrote"
	calls      [][]string
	script     string // the debugfs script the build helper was given
}

func installPushFake(fd *fakeDocker, listing string) *pushFake {
	p := &pushFake{listing: listing}
	fd.pushHelper = func(args []string) (string, string, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.calls = append(p.calls, args)
		if slices.Contains(args, "find") {
			return p.listing, "", nil
		}
		for i, a := range args {
			if a == "-v" && strings.HasSuffix(args[i+1], ":/x") {
				dir := strings.TrimSuffix(args[i+1], ":/x")
				script, err := os.ReadFile(filepath.Join(dir, "script"))
				if err != nil {
					return "", err.Error(), err
				}
				p.script = string(script)
				if err := os.WriteFile(filepath.Join(dir, "debugfs.err"), []byte(p.debugfsErr), 0o600); err != nil {
					return "", err.Error(), err
				}
			}
		}
		return "", "", nil
	}
	return p
}

func (p *pushFake) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func workListing(extra ...string) string {
	return rec("d", "755", "1001", "1001", "1700000000.0", "4096", "", "64:1", "2", "") +
		rec("f", "600", "1001", "1001", "1700000001.0", "11", "", "64:2", "1", "secret.txt") +
		strings.Join(extra, "")
}

func newPushEngine(t *testing.T, listing string) (*Engine, *fakeDocker, *pushFake, *bytes.Buffer) {
	t.Helper()
	e, fd := newGVisorEngine(t)
	var log bytes.Buffer
	e.Progress = &log
	if _, err := e.New(context.Background(), Spec{Name: "gv", Runtime: RuntimeGVisor, DiskGiB: 1}); err != nil {
		t.Fatal(err)
	}
	return e, fd, installPushFake(fd, listing), &log
}

func TestGVisorFlattenBuildsTheDiskOnADockyardBase(t *testing.T) {
	ctx := context.Background()
	e, fd, p, log := newPushEngine(t, workListing(rec("p", "644", "1001", "1001", "1700000002.0", "0", "", "64:3", "1", "fifo")))
	p.debugfsErr = "debugfs 1.47.0 (5-Feb-2023)\next2fs_mkdir: Ext2 directory already exists while creating directory \"work\"\nmkdir: Ext2 directory already exists\n"
	dir := t.TempDir()
	flat, err := e.Flatten(ctx, "gv", dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if flat.Image != DefaultImage || strings.Contains(flat.Image, "dock-local-gvisor") || flat.DiskGiB != 1 {
		t.Fatalf("flat %+v", flat)
	}
	if st, err := os.Stat(flat.RawPath); err != nil || st.Size() != 1<<30 {
		t.Fatalf("the disk must be truncated to DiskGiB: %v %v", st, err)
	}
	if len(fd.callsWith("stop")) != 1 {
		t.Fatalf("--stop must stop the container: %v", fd.calls)
	}
	if !strings.Contains(p.script, `write "/work/secret.txt" "/home/user/work/secret.txt"`) {
		t.Fatalf("script %q", p.script)
	}
	// Listed while running (the checks), again once stopped (the copy), then the build.
	if p.count() != 3 {
		t.Fatalf("want two listings and a build helper, got %v", p.calls)
	}
	list, build := strings.Join(p.calls[1], " "), strings.Join(p.calls[2], " ")
	if !slices.Contains(p.calls[0], "find") || !slices.Contains(p.calls[1], "find") {
		t.Fatalf("want two listings first: %v", p.calls)
	}
	for _, c := range []string{list, build} {
		if !strings.Contains(c, "--cap-drop ALL --cap-add DAC_READ_SEARCH") || !strings.Contains(c, "--runtime=runsc ") ||
			!strings.Contains(c, "-v dock-local-"+flat.Record.ID+"-work:/work:ro ") || !strings.Contains(c, "--network none") {
			t.Fatalf("helper must run with DAC_READ_SEARCH and the work volume read-only: %s", c)
		}
	}
	if strings.Contains(list, "/x") || !strings.Contains(build, ":/x ") || strings.Contains(build, ":/x:ro") {
		t.Fatalf("only the build helper gets the writable directory: %s | %s", list, build)
	}
	for _, step := range []string{"e2fsck -fy /x/disk.raw", "resize2fs /x/disk.raw", "debugfs -w -f /x/script /x/disk.raw", "e2fsck -fn /x/disk.raw"} {
		if !strings.Contains(build, step) {
			t.Fatalf("build helper lacks %q: %s", step, build)
		}
	}
	if !strings.Contains(log.String(), "warning: skipped /work/fifo") {
		t.Fatalf("skipped special files must be surfaced: %q", log)
	}
	tag, err := GVisorImageTag()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(build, " "+tag+" ") || strings.Contains(build, " "+gvImageRepo+" ") {
		t.Fatalf("the helper must run the dock image with e2fsprogs: %s", build)
	}
	if !fd.images[tag] {
		t.Fatal("the helper image must exist")
	}
}

func TestGVisorFlattenRefusesARunningDockWithoutStop(t *testing.T) {
	e, fd, p, _ := newPushEngine(t, workListing())
	if _, err := e.Flatten(context.Background(), "gv", t.TempDir(), false); codeOf(err) != "dock_running" {
		t.Fatalf("a running dock must be refused: %v", err)
	}
	if len(fd.callsWith("stop")) != 0 || p.count() != 0 {
		t.Fatalf("nothing may be stopped or run: %v", fd.calls)
	}
}

func TestGVisorFlattenWorksOnAStoppedDockAndHonoursOptions(t *testing.T) {
	ctx := context.Background()
	e, fd, _, _ := newPushEngine(t, workListing())
	if _, err := e.Stop(ctx, "gv", false); err != nil {
		t.Fatal(err)
	}
	stops := len(fd.callsWith("stop"))
	flat, err := e.FlattenWith(ctx, "gv", t.TempDir(), false, PushOpts{Image: DefaultImage, DiskGiB: 2})
	if err != nil {
		t.Fatal(err)
	}
	if flat.DiskGiB != 2 || len(fd.callsWith("stop")) != stops {
		t.Fatalf("flat %+v stops %d->%d", flat, stops, len(fd.callsWith("stop")))
	}
	if _, err := e.FlattenWith(ctx, "gv", t.TempDir(), false, PushOpts{Image: "no-such-image"}); err == nil {
		t.Fatal("an unknown Dockyard image must fail")
	}
}

func TestGVisorFlattenFailsWhenTheDataExceedsTheDisk(t *testing.T) {
	big := rec("f", "644", "1001", "1001", "1700000002.0", "2147483648", "", "64:9", "1", "big.bin")
	e, fd, p, _ := newPushEngine(t, workListing(big))
	dir := t.TempDir()
	_, err := e.Flatten(context.Background(), "gv", dir, true)
	if codeOf(err) != "push_too_big" || !strings.Contains(err.Error(), "--disk") {
		t.Fatalf("data over DiskGiB must fail clearly: %v", err)
	}
	if p.count() != 1 {
		t.Fatalf("no disk may be built: %v", p.calls)
	}
	if len(fd.callsWith("stop")) != 0 {
		t.Fatalf("a push that cannot fit must not stop the dock: %v", fd.calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "disk.raw")); err == nil {
		t.Fatal("no disk may be left behind")
	}
	// A larger disk takes it.
	if _, err := e.FlattenWith(context.Background(), "gv", t.TempDir(), true, PushOpts{DiskGiB: 4}); err != nil {
		t.Fatal(err)
	}
}

func TestGVisorFlattenFailsOnDebugfsErrors(t *testing.T) {
	ctx := context.Background()
	e, _, p, _ := newPushEngine(t, workListing())
	p.debugfsErr = "debugfs 1.47.0 (5-Feb-2023)\next2fs_mkdir: Ext2 directory already exists while creating directory \"work\"\nmkdir: Ext2 directory already exists\n" +
		"write: No space left on device while writing /home/user/work/big\n"
	dir := t.TempDir()
	_, err := e.Flatten(ctx, "gv", dir, true)
	if codeOf(err) != "push_failed" || !strings.Contains(err.Error(), "No space left on device") {
		t.Fatalf("a debugfs error must fail the push: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "disk.raw")); err == nil {
		t.Fatal("a failed build must not leave its disk")
	}
	// A second "already exists" (not the work dir's) is an error too.
	p.debugfsErr = "mkdir: Ext2 directory already exists\nmkdir: Ext2 directory already exists\n"
	if _, err := e.Flatten(ctx, "gv", t.TempDir(), true); codeOf(err) != "push_failed" {
		t.Fatalf("only the first already-exists pair is tolerated: %v", err)
	}
}

func TestGVisorFlattenFailsOnAnUnsupportedName(t *testing.T) {
	e, _, p, _ := newPushEngine(t, workListing(rec("f", "644", "1001", "1001", "1700000002.0", "1", "", "64:9", "1", "a\nb")))
	if _, err := e.Flatten(context.Background(), "gv", t.TempDir(), true); codeOf(err) != "push_unsupported_name" {
		t.Fatalf("%v", err)
	}
	if p.count() != 1 {
		t.Fatalf("no disk may be built: %v", p.calls)
	}
}

func TestGVisorCheckPushSpaceDefersToFlatten(t *testing.T) {
	e, _, _, _ := newPushEngine(t, workListing())
	rec, _ := e.Lookup("gv")
	if err := e.CheckPushSpace(rec); err != nil {
		t.Fatal(err)
	}
}

func TestGVWorkBytes(t *testing.T) {
	l := rec("d", "755", "1", "1", "1.0", "4096", "", "1:1", "2", "") +
		rec("f", "644", "1", "1", "1.0", "1", "", "1:2", "2", "a") +
		rec("f", "644", "1", "1", "1.0", "1", "", "1:2", "2", "b") +
		rec("f", "644", "1", "1", "1.0", "4097", "", "1:3", "1", "c") +
		rec("l", "777", "1", "1", "1.0", "1", "a", "1:4", "1", "l")
	if got, want := gvWorkBytes([]byte(l)), int64(4096+4096+8192); got != want {
		t.Fatalf("got %d want %d", got, want)
	}
}

func TestGVisorFlattenChecksFreeSpaceBeforeStopping(t *testing.T) {
	huge := rec("f", "644", "1001", "1001", "1700000002.0", strconv.FormatInt(1<<50, 10), "", "64:9", "1", "huge.bin")
	e, fd, p, _ := newPushEngine(t, workListing(huge))
	_, err := e.FlattenWith(context.Background(), "gv", t.TempDir(), true, PushOpts{DiskGiB: 1 << 21})
	if codeOf(err) != "insufficient_space" {
		t.Fatalf("want insufficient_space: %v", err)
	}
	if len(fd.callsWith("stop")) != 0 || p.count() != 1 {
		t.Fatalf("the space check must run before the stop and the build: %v | %v", fd.calls, p.calls)
	}
}

func TestGVisorFlattenRefusesAMissingWorkVolume(t *testing.T) {
	e, fd, p, _ := newPushEngine(t, workListing())
	r, _ := e.Lookup("gv")
	// docker volume prune after the container went: docker run -v would recreate it empty.
	delete(fd.volumes, gvVolume(r.ID))
	dir := t.TempDir()
	_, err := e.Flatten(context.Background(), "gv", dir, true)
	if codeOf(err) != "push_failed" || !strings.Contains(err.Error(), gvVolume(r.ID)) || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("a missing work volume must fail clearly: %v", err)
	}
	if p.count() != 0 || len(fd.callsWith("stop")) != 0 || fd.volumes[gvVolume(r.ID)] {
		t.Fatalf("no helper may run (it would create the volume) and nothing may stop: %v | %v", fd.calls, p.calls)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("nothing may be written: %v", ents)
	}
}

func TestGVisorFlattenPushesAnEmptyVolumeThatExists(t *testing.T) {
	root := rec("d", "755", "1001", "1001", "1700000000.0", "4096", "", "64:1", "2", "")
	e, _, p, _ := newPushEngine(t, root)
	if _, err := e.Flatten(context.Background(), "gv", t.TempDir(), true); err != nil {
		t.Fatalf("an existing empty work volume pushes: %v", err)
	}
	if strings.Contains(p.script, "write ") {
		t.Fatalf("script %q", p.script)
	}
}

func TestGVisorFlattenRelistsWhenTheRunningListingFails(t *testing.T) {
	e, fd, p, _ := newPushEngine(t, workListing())
	inner, failed := fd.pushHelper, false
	fd.pushHelper = func(args []string) (string, string, error) {
		// find exits 1 when the running dock deletes a file during the walk (the first listing).
		if slices.Contains(args, "find") && !failed {
			failed = true
			return "", "find: '/work/tmp.123': No such file or directory", errors.New("exit status 1")
		}
		return inner(args)
	}
	if _, err := e.Flatten(context.Background(), "gv", t.TempDir(), true); err != nil {
		t.Fatalf("the listing of the stopped dock decides: %v", err)
	}
	if !failed || !strings.Contains(p.script, "secret.txt") || len(fd.callsWith("stop")) != 1 {
		t.Fatalf("script %q", p.script)
	}
}
