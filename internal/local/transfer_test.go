package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/usedock/docklet/internal/dockyard"
)

func TestFlattenDiskArgv(t *testing.T) {
	raw := filepath.Join(t.TempDir(), "out.raw")
	var got []string
	q := &QEMU{GOOS: "linux", GOARCH: "amd64", Getenv: env(nil), system: "/q/qemu", img: "/q/qemu-img",
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			got = append([]string{name}, args...)
			return nil, nil
		}}
	if err := q.FlattenDisk(context.Background(), "disk.qcow2", raw); err != nil {
		t.Fatal(err)
	}
	want := []string{"/q/qemu-img", "convert", "-f", "qcow2", "-O", "raw", "disk.qcow2", raw}
	if !slices.Equal(got, want) {
		t.Fatalf("argv %v, want %v", got, want)
	}
}

func TestFlattenDiskPreCreatesTarget(t *testing.T) {
	raw := filepath.Join(t.TempDir(), "out.raw")
	seen := false
	q := &QEMU{GOOS: "linux", GOARCH: "amd64", Getenv: env(nil), system: "/q/qemu", img: "/q/qemu-img",
		Run: func(context.Context, string, ...string) ([]byte, error) {
			_, err := os.Stat(raw)
			seen = err == nil
			return nil, nil
		}}
	if err := q.FlattenDisk(context.Background(), "disk.qcow2", raw); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("the raw target must exist (flagged sparse) before qemu-img runs")
	}
}

func TestConvertRawIsStandalone(t *testing.T) {
	var got []string
	q := &QEMU{GOOS: "linux", GOARCH: "amd64", Getenv: env(nil), system: "/q/qemu", img: "/q/qemu-img",
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			got = append([]string{name}, args...)
			return nil, nil
		}}
	if err := q.ConvertRaw(context.Background(), "in.raw", "disk.qcow2"); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got, "-b") || slices.Contains(got, "-F") || got[1] != "convert" {
		t.Fatalf("a pulled disk must have no backing file: %v", got)
	}
}

func TestFlattenNeedsAStoppedDock(t *testing.T) {
	ctx := context.Background()
	e, drv, _ := newTestEngine(t)
	d, err := e.New(ctx, Spec{Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := e.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if _, err := e.Flatten(ctx, "web", dir, false); codeOf(err) != "dock_running" {
		t.Fatalf("a running dock must be refused, got %v", err)
	}
	if len(drv.flattens) != 0 {
		t.Fatal("nothing may be flattened from a running dock")
	}
	before, _ := os.ReadFile(e.file(d.ID, "token"))
	flat, err := e.Flatten(ctx, "web", dir, true)
	if err != nil {
		t.Fatal(err)
	}
	// The flattened disk holds the guest's copy of the token, so the local token rotates.
	after, _ := os.ReadFile(e.file(d.ID, "token"))
	if len(after) == 0 || string(after) == string(before) {
		t.Fatalf("flatten must rotate the agent token (before %q after %q)", before, after)
	}
	if _, err := os.Stat(e.file(d.ID, "token.tmp")); err == nil {
		t.Fatal("token.tmp left behind")
	}
	if e.alive(d.ID) {
		t.Fatal("--stop must stop the dock")
	}
	raw, _ := os.ReadFile(flat.RawPath)
	if !strings.HasPrefix(string(raw), "overlay-of:") {
		t.Fatalf("raw %q", raw)
	}
	// Only the disk is read: no runtime file lands in the work dir.
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 || ents[0].Name() != "disk.raw" {
		t.Fatalf("work dir %v", ents)
	}
}

func TestCompressRoundTripAndCap(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src, zst, back := filepath.Join(dir, "a.raw"), filepath.Join(dir, "a.zst"), filepath.Join(dir, "b.raw")
	data := []byte(strings.Repeat("dock", 4096))
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sum, size, err := CompressFile(ctx, src, zst)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(zst); st.Size() != size {
		t.Fatalf("size %d != file %d", size, st.Size())
	}
	if want, _ := FileSHA256(context.Background(), zst); want != sum {
		t.Fatalf("sha %s != %s", sum, want)
	}
	if err := DecompressFile(context.Background(), zst, back, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(back); string(got) != string(data) {
		t.Fatal("round trip differs")
	}
	if err := DecompressFile(context.Background(), zst, back, int64(len(data))-1); codeOf(err) != "decompress_failed" {
		t.Fatalf("an oversized stream must be refused, got %v", err)
	}
}

func TestImportRegistersAStoppedStandaloneDock(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newTestEngine(t)
	cached, err := e.EnsureImage(ctx, DefaultImage)
	if err != nil {
		t.Fatal(err)
	}
	raw := filepath.Join(t.TempDir(), "d.raw")
	_ = os.WriteFile(raw, []byte("disk"), 0o600)
	in := Import{Name: "pulled", Image: DefaultImage, Cached: cached, RawPath: raw, CPUs: 3, MemoryMiB: 512, DiskGiB: 4}
	d, err := e.Import(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if d.State != "archived" || d.Name != "pulled" || d.CPUs != 3 || d.DiskGiB != 4 || d.Image != DefaultImage {
		t.Fatalf("dock %+v", d)
	}
	rec, _ := e.Lookup("pulled")
	if rec.ImageDir != cached.Dir {
		t.Fatalf("image dir %q", rec.ImageDir)
	}
	disk, _ := os.ReadFile(e.file(rec.ID, "disk.qcow2"))
	if string(disk) != "qcow2:disk" {
		t.Fatalf("disk %q", disk)
	}
	if _, err := os.Stat(e.file(rec.ID, "token")); err != nil {
		t.Fatalf("a pulled dock needs a fresh local token: %v", err)
	}
	if _, err := e.Import(ctx, in); codeOf(err) != "dock_name_taken" {
		t.Fatalf("name collision, got %v", err)
	}
	if recs, _ := e.store().List(); len(recs) != 1 {
		t.Fatalf("records %d", len(recs))
	}
	ents, _ := os.ReadDir(filepath.Join(e.Dir, "docks"))
	if len(ents) != 1 {
		t.Fatalf("a refused import must clean its dock dir: %v", ents)
	}
}

func TestFileSHA256AndDecompressStopOnCancel(t *testing.T) {
	dir := t.TempDir()
	src, zst, out := filepath.Join(dir, "a.raw"), filepath.Join(dir, "a.zst"), filepath.Join(dir, "b.raw")
	if err := os.WriteFile(src, []byte(strings.Repeat("dock", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CompressFile(context.Background(), src, zst); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FileSHA256(ctx, src); !errors.Is(err, context.Canceled) {
		t.Fatalf("the sha256 pass must stop on cancel, got %v", err)
	}
	if err := DecompressFile(ctx, zst, out, 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("decompression must stop on cancel, got %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a cancelled decompression must not leave its output")
	}
}

func TestCheckFreeSpace(t *testing.T) {
	e, _, _ := newTestEngine(t)
	if err := e.CheckFreeSpace(1, "x"); err != nil {
		t.Fatalf("one byte must fit: %v", err)
	}
	err := e.CheckFreeSpace(1<<62, "the pull")
	if codeOf(err) != "insufficient_space" || !strings.Contains(err.Error(), "the pull needs about") {
		t.Fatalf("an impossible size must fail clearly, got %v", err)
	}
}

func TestCheckNameValidatesTheName(t *testing.T) {
	e, _, _ := newTestEngine(t)
	if codeOf(e.CheckName("bad name!")) != "invalid_argument" {
		t.Fatal("an invalid name must be rejected before any download")
	}
	if err := e.CheckName("good-name.1"); err != nil {
		t.Fatal(err)
	}
}

func TestCleanStaleTemp(t *testing.T) {
	e, _, _ := newTestEngine(t)
	tmp := filepath.Join(e.Dir, "tmp")
	mk := func(name string, age time.Duration) string {
		d := filepath.Join(tmp, name)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(d, "export.zst")
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-age)
		for _, p := range []string{f, d} {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
		return d
	}
	dead := "2147483646" // no such process
	live := strconv.Itoa(os.Getpid())
	staleDead := mk("xfer-"+dead+"-1", 48*time.Hour)
	staleLegacy := mk("xfer-123456", 48*time.Hour)
	staleLive := mk("xfer-"+live+"-1", 48*time.Hour)
	freshDead := mk("xfer-"+dead+"-2", time.Hour)
	other := mk("keep-me", 48*time.Hour)

	if n := e.CleanStaleTemp(); n != 2 {
		t.Fatalf("removed %d, want 2", n)
	}
	for _, d := range []string{staleDead, staleLegacy} {
		if _, err := os.Stat(d); err == nil {
			t.Fatalf("%s is stale and unowned and must go", d)
		}
	}
	for _, d := range []string{staleLive, freshDead, other} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("%s must stay: %v", d, err)
		}
	}
	// TempDir names carry the owner, so a live transfer is never swept.
	d, err := e.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	if pid, ok := xferOwner(filepath.Base(d)); !ok || pid != os.Getpid() {
		t.Fatalf("TempDir name %q must carry this pid", d)
	}
}

func TestPushSpaceNeedCountsData(t *testing.T) {
	const gib = int64(1) << 30
	// A 20 GiB disk holding 2 GiB: the data plus zstd headroom, not 20 GiB.
	if got := pushSpaceNeed(20*gib, 2*gib); got != 3*gib {
		t.Fatalf("need %d, want %d", got, 3*gib)
	}
	// Overlay plus base can count more than the disk holds; the disk size bounds it.
	if got := pushSpaceNeed(4*gib, 6*gib); got != 6*gib {
		t.Fatalf("capped need %d, want %d", got, 6*gib)
	}
}

func TestPullSpaceNeed(t *testing.T) {
	const gib = int64(1) << 30
	// No recorded data size (an older export): 1 GiB export of a 20 GiB disk guesses ~3 GiB of
	// data; peak is data plus the larger of export and data.
	if got := pullSpaceNeed(gib, 0, 20*gib, 0, 0); got != 6*gib {
		t.Fatalf("need %d, want %d", got, 6*gib)
	}
	// The guessed data cannot exceed the disk.
	if got := pullSpaceNeed(10*gib, 0, 4*gib, 0, 0); got != 14*gib {
		t.Fatalf("capped need %d, want %d", got, 14*gib)
	}
	// A base image still to fetch is held through the pull, and its own peak can dominate.
	if got := pullSpaceNeed(gib, 0, 20*gib, 0, 2*gib); got != 8*gib {
		t.Fatalf("with kept image %d, want %d", got, 8*gib)
	}
	if got := pullSpaceNeed(gib, 0, 20*gib, 9*gib, 2*gib); got != 9*gib {
		t.Fatalf("image peak %d, want %d", got, 9*gib)
	}
}

func TestPullSpaceNeedUsesRecordedDataSize(t *testing.T) {
	const gib = int64(1) << 30
	// Incompressible: 6 GiB of data on a 10 GiB disk exports to 6 GiB. The 3x guess wanted
	// 10+10 GiB; the real peak is the data plus the export, 12 GiB.
	if got := pullSpaceNeed(6*gib, 6*gib, 10*gib, 0, 0); got != 12*gib {
		t.Fatalf("incompressible need %d, want %d", got, 12*gib)
	}
	// Highly compressible: 8 GiB of data exports to 1 GiB. The guess wanted 6 GiB and the pull
	// would run out of space; the real peak is 16 GiB (the raw disk and the qcow2 made from it).
	if got := pullSpaceNeed(gib, 8*gib, 20*gib, 0, 0); got != 16*gib {
		t.Fatalf("compressible need %d, want %d", got, 16*gib)
	}
	// A recorded size above the disk (it cannot be) is still bounded by the disk.
	if got := pullSpaceNeed(gib, 30*gib, 4*gib, 0, 0); got != 8*gib {
		t.Fatalf("capped need %d, want %d", got, 8*gib)
	}
	// An absurd recorded size keeps the arithmetic far from overflow.
	if got := pullSpaceNeed(gib, 1<<62, 0, 0, 0); got <= 0 {
		t.Fatalf("overflowed: %d", got)
	}
}

func TestPullSpaceNeedCountsBaseImageFilesAndTwiceItsData(t *testing.T) {
	e, _, _ := newTestEngine(t)
	im, err := e.ResolveImage(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var files int64
	for _, f := range im.Files {
		files += f.Size
	}
	rootfs := im.Files[dockyard.FileRootfs].Size
	data := rootfs * rawDataRatio
	// A tiny export so the base-image peak dominates. rootfs.zst, the sparse raw file and
	// base.qcow2.part coexist: the image files plus twice the data.
	if got, want := e.PullSpaceNeed(context.Background(), 1, 1, 1, ""), files+2*data; got != want {
		t.Fatalf("need %d, want %d", got, want)
	}
}

func TestCheckPushSpaceUsesAllocatedData(t *testing.T) {
	e, _, _ := newTestEngine(t)
	if _, err := e.New(context.Background(), Spec{Name: "web", CPUs: 1, MemoryMiB: 512, DiskGiB: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	rec, err := e.Lookup("web")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.CheckPushSpace(rec); err != nil {
		t.Fatalf("a 1 PiB disk holding a few bytes must fit when the raw is sparse: %v", err)
	}
}
