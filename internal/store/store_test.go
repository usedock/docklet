package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	m := Manifest{SnapshotID: "s1", DockID: "d1", Kind: "full", CPUFamily: "f1", Files: []FileEntry{{Path: "mem", Size: 3}}}
	if err := PutManifest(ctx, s, m); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "s1", "mem", bytes.NewReader([]byte("abc")), 3); err != nil {
		t.Fatal(err)
	}
	back, err := GetManifest(ctx, s, "s1")
	if err != nil || back.CPUFamily != "f1" || len(back.Files) != 1 {
		t.Fatalf("manifest = %+v,%v", back, err)
	}
	listed, err := s.List(ctx, "s1")
	if err != nil || len(listed) != 2 {
		t.Fatalf("list = %+v,%v", listed, err)
	}
	if err := s.Delete(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := GetManifest(ctx, s, "s1"); err == nil {
		t.Fatal("deleted generation must be gone")
	}
}

func TestCompressedRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	dir := t.TempDir()
	src := filepath.Join(dir, "rootfs.ext4")
	payload := bytes.Repeat([]byte("disk-block-0123456789"), 4096)
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	entry, err := PutFileCompressed(ctx, s, "s9", "rootfs.ext4", src, dir)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Encoding != EncodingZstd || entry.Path != "rootfs.ext4.zst" {
		t.Fatalf("entry = %+v", entry)
	}
	dst := filepath.Join(dir, "out", "rootfs.ext4")
	if err := GetFileAuto(ctx, s, "s9", entry, dst); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(dst)
	if !bytes.Equal(raw, payload) {
		t.Fatal("round trip mismatch")
	}
	legacy := FileEntry{Path: "rootfs.ext4", Size: int64(len(payload))}
	if err := s.Put(ctx, "s8", "rootfs.ext4", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	if err := GetFileAuto(ctx, s, "s8", legacy, filepath.Join(dir, "legacy.ext4")); err != nil {
		t.Fatal(err)
	}
}

func TestCompressedLeavesNoScratch(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	dir := t.TempDir()
	src := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(src, bytes.Repeat([]byte("x"), 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PutFileCompressed(ctx, s, "s7", "rootfs.ext4", src, dir); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "zst-*"))
	if len(matches) != 0 {
		t.Fatalf("streaming must leave no temp files, got %v", matches)
	}
}

func TestFileHelpers(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	dir := t.TempDir()
	src := filepath.Join(dir, "state")
	if err := os.WriteFile(src, []byte("state-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PutFile(ctx, s, "s2", "state", src); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "nested", "state")
	if err := GetFile(ctx, s, "s2", "state", dst); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(dst)
	if string(raw) != "state-bytes" {
		t.Fatalf("round trip = %q", raw)
	}
}

// shortStore serves only the first limit bytes of every object and counts Gets.
type shortStore struct {
	Store
	limit int
	gets  atomic.Int32
	gate  chan struct{}
}

func (s *shortStore) Get(ctx context.Context, snapshotID, name string) (io.ReadCloser, error) {
	s.gets.Add(1)
	if s.gate != nil {
		<-s.gate
	}
	rc, err := s.Store.Get(ctx, snapshotID, name)
	if err != nil || s.limit < 0 {
		return rc, err
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, int64(s.limit)))
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func putCompressed(t *testing.T, payload []byte) (*MemStore, FileEntry, string) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewMemStore()
	entry, err := PutFileCompressed(context.Background(), s, "g1", "rootfs.ext4", src, dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, entry, dir
}

func noTempFiles(t *testing.T, dir string) {
	t.Helper()
	left, _ := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	if len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

func TestCaptureRecordsDigestAndRawSize(t *testing.T) {
	payload := bytes.Repeat([]byte("abc"), 5000)
	_, entry, _ := putCompressed(t, payload)
	if entry.SHA256 == "" || entry.RawSize != int64(len(payload)) {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestTruncatedDownloadIsRejectedAndNotCached(t *testing.T) {
	payload := make([]byte, 1<<16)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	mem, entry, _ := putCompressed(t, payload)
	short := &shortStore{Store: mem, limit: int(entry.Size) / 2}
	dest := filepath.Join(t.TempDir(), "gen", "rootfs.ext4")
	if err := EnsureFile(context.Background(), short, "g1", entry, dest); err == nil {
		t.Fatal("truncated download must fail")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("truncated download must not be cached, stat err = %v", err)
	}
	noTempFiles(t, filepath.Dir(dest))
	// A later load from a healthy store succeeds.
	if err := EnsureFile(context.Background(), mem, "g1", entry, dest); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(dest); !bytes.Equal(raw, payload) {
		t.Fatal("healthy retry did not produce the full file")
	}
	noTempFiles(t, filepath.Dir(dest))
}

func TestRawTruncationAndDigestMismatchAreRejected(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	payload := []byte("0123456789abcdef")
	if err := mem.Put(ctx, "g2", "state", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	entry := FileEntry{Path: "state", Size: int64(len(payload))}
	dir := t.TempDir()
	if err := GetFileAuto(ctx, &shortStore{Store: mem, limit: 5}, "g2", entry, filepath.Join(dir, "state")); err == nil {
		t.Fatal("short raw download must fail")
	}
	entry.SHA256 = "00"
	if err := GetFileAuto(ctx, mem, "g2", entry, filepath.Join(dir, "state")); err == nil {
		t.Fatal("digest mismatch must fail")
	}
	if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
		t.Fatalf("rejected download must not appear, stat err = %v", err)
	}
	noTempFiles(t, dir)
}

func TestLegacyEntryWithoutDigestStillLoads(t *testing.T) {
	payload := bytes.Repeat([]byte("legacy"), 1000)
	mem, entry, _ := putCompressed(t, payload)
	entry.SHA256, entry.RawSize = "", 0
	dest := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := EnsureFile(context.Background(), mem, "g1", entry, dest); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(dest); !bytes.Equal(raw, payload) {
		t.Fatal("legacy entry mismatch")
	}
}

func TestPreexistingPartialFileIsReplaced(t *testing.T) {
	payload := bytes.Repeat([]byte("full-image"), 3000)
	mem, entry, _ := putCompressed(t, payload)
	dest := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(dest, payload[:100], 0o644); err != nil {
		t.Fatal(err)
	}
	counted := &shortStore{Store: mem, limit: -1}
	if err := EnsureFile(context.Background(), counted, "g1", entry, dest); err != nil {
		t.Fatal(err)
	}
	if counted.gets.Load() != 1 {
		t.Fatalf("partial file must trigger a download, gets = %d", counted.gets.Load())
	}
	if raw, _ := os.ReadFile(dest); !bytes.Equal(raw, payload) {
		t.Fatal("partial file was not replaced")
	}
	// A complete file is trusted without another download.
	if err := EnsureFile(context.Background(), counted, "g1", entry, dest); err != nil {
		t.Fatal(err)
	}
	if counted.gets.Load() != 1 {
		t.Fatalf("complete file must be reused, gets = %d", counted.gets.Load())
	}
}

func TestConcurrentLoadsShareOneVerifiedDownload(t *testing.T) {
	payload := bytes.Repeat([]byte("concurrent"), 5000)
	mem, entry, _ := putCompressed(t, payload)
	gated := &shortStore{Store: mem, limit: -1, gate: make(chan struct{})}
	dest := filepath.Join(t.TempDir(), "gen", "rootfs.ext4")
	const callers = 6
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() { errs <- EnsureFile(context.Background(), gated, "g1", entry, dest) }()
	}
	// Let every caller reach the flight before the leader's download proceeds.
	time.Sleep(100 * time.Millisecond)
	close(gated.gate)
	for i := 0; i < callers; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := gated.gets.Load(); n != 1 {
		t.Fatalf("store gets = %d, want 1 shared download", n)
	}
	if raw, _ := os.ReadFile(dest); !bytes.Equal(raw, payload) {
		t.Fatal("concurrent load produced a bad file")
	}
	noTempFiles(t, filepath.Dir(dest))
}

func TestLegacyEntryIsVerifiedOnceThenTrusted(t *testing.T) {
	payload := bytes.Repeat([]byte("legacy-cache"), 3000)
	mem, entry, _ := putCompressed(t, payload)
	entry.SHA256, entry.RawSize = "", 0
	counted := &shortStore{Store: mem, limit: -1}
	dest := filepath.Join(t.TempDir(), "rootfs.ext4")
	// A file left by old code has no sidecar, so it is replaced once.
	if err := os.WriteFile(dest, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := EnsureFile(context.Background(), counted, "g1", entry, dest); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(dest)
		if err != nil {
			t.Fatalf("path must stay openable: %v", err)
		}
		f.Close()
	}
	if n := counted.gets.Load(); n != 1 {
		t.Fatalf("store gets = %d, want 1", n)
	}
	// A cached file whose size no longer matches the sidecar is not trusted.
	if err := os.WriteFile(dest, payload[:10], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureFile(context.Background(), counted, "g1", entry, dest); err != nil {
		t.Fatal(err)
	}
	if n := counted.gets.Load(); n != 2 {
		t.Fatalf("store gets = %d, want 2 after size drift", n)
	}
	if raw, _ := os.ReadFile(dest); !bytes.Equal(raw, payload) {
		t.Fatal("drifted file was not replaced")
	}
}

func TestRedownloadNeverMakesThePathDisappear(t *testing.T) {
	payload := bytes.Repeat([]byte("never-enoent"), 20000)
	mem, entry, _ := putCompressed(t, payload)
	entry.SHA256, entry.RawSize = "", 0
	dest := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(dest, payload[:50], 0o644); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	missing := make(chan error, 1)
	go func() {
		defer close(missing)
		for {
			select {
			case <-stop:
				return
			default:
			}
			f, err := os.Open(dest)
			if err != nil {
				missing <- err
				return
			}
			f.Close()
		}
	}()
	err := EnsureFile(context.Background(), mem, "g1", entry, dest)
	close(stop)
	if err != nil {
		t.Fatal(err)
	}
	if openErr, ok := <-missing; ok && openErr != nil {
		t.Fatalf("reader lost the path during re-download: %v", openErr)
	}
}

func TestWaiterHonoursItsOwnContext(t *testing.T) {
	payload := bytes.Repeat([]byte("ctx"), 5000)
	mem, entry, _ := putCompressed(t, payload)
	gated := &shortStore{Store: mem, limit: -1, gate: make(chan struct{})}
	dest := filepath.Join(t.TempDir(), "rootfs.ext4")
	leader := make(chan error, 1)
	go func() { leader <- EnsureFile(context.Background(), gated, "g1", entry, dest) }()
	for gated.gets.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := EnsureFile(ctx, gated, "g1", entry, dest); err == nil {
		t.Fatal("waiter must give up when its context ends")
	}
	close(gated.gate)
	if err := <-leader; err != nil {
		t.Fatal(err)
	}
}

func TestPanicInDownloadDoesNotWedgeLaterCallers(t *testing.T) {
	g := &flightGroup{calls: map[string]*flightCall{}}
	func() {
		defer func() { _ = recover() }()
		_ = g.do(context.Background(), "k", func() error { panic("boom") })
	}()
	if err := g.do(context.Background(), "k", func() error { return nil }); err != nil {
		t.Fatalf("later caller = %v, want success", err)
	}
}

// endlessStore serves every object as an endless stream of zeros and counts what was read.
type endlessStore struct {
	Store
	read atomic.Int64
}

type zeros struct{ n *atomic.Int64 }

func (z zeros) Read(p []byte) (int, error) {
	clear(p)
	z.n.Add(int64(len(p)))
	return len(p), nil
}

func (s *endlessStore) Get(context.Context, string, string) (io.ReadCloser, error) {
	return io.NopCloser(zeros{&s.read}), nil
}

func TestDownloadStopsJustPastTheDeclaredSize(t *testing.T) {
	s := &endlessStore{Store: NewMemStore()}
	dest := filepath.Join(t.TempDir(), "rootfs.ext4")
	err := GetFileAuto(context.Background(), s, "s1", FileEntry{Path: "rootfs.ext4", Size: 1 << 20}, dest)
	if err == nil {
		t.Fatal("an object longer than its declared size was accepted")
	}
	// io.Copy reads in 32 KiB chunks, so the overrun is at most one chunk.
	if got := s.read.Load(); got > 1<<20+64<<10 {
		t.Fatalf("read %d bytes of a 1 MiB object", got)
	}
	noTempFiles(t, filepath.Dir(dest))
}

func TestDownloadRefusesAnObjectThatUnpacksPastTheLimit(t *testing.T) {
	s, entry, _ := putCompressed(t, make([]byte, 2<<20))
	dir := t.TempDir()
	dest := filepath.Join(dir, "rootfs.ext4")
	if err := GetFileAutoLimit(context.Background(), s, "g1", entry, dest, 1<<20); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("a refused download left a file: %v", err)
	}
	noTempFiles(t, dir)
	if err := GetFileAutoLimit(context.Background(), s, "g1", entry, dest, 2<<20); err != nil {
		t.Fatal(err)
	}
}