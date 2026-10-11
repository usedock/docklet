package local

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestFlattenDiskArgv(t *testing.T) {
	var got []string
	q := &QEMU{GOOS: "linux", GOARCH: "amd64", Getenv: env(nil), system: "/q/qemu", img: "/q/qemu-img",
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			got = append([]string{name}, args...)
			return nil, nil
		}}
	if err := q.FlattenDisk(context.Background(), "disk.qcow2", "out.raw"); err != nil {
		t.Fatal(err)
	}
	want := []string{"/q/qemu-img", "convert", "-f", "qcow2", "-O", "raw", "disk.qcow2", "out.raw"}
	if !slices.Equal(got, want) {
		t.Fatalf("argv %v, want %v", got, want)
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
	if want, _ := FileSHA256(zst); want != sum {
		t.Fatalf("sha %s != %s", sum, want)
	}
	if err := DecompressFile(zst, back, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(back); string(got) != string(data) {
		t.Fatal("round trip differs")
	}
	if err := DecompressFile(zst, back, int64(len(data))-1); codeOf(err) != "decompress_failed" {
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
