package disk

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeImage writes size bytes of a deterministic pattern so block
// comparison has real content to work with.
func writeImage(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, BlockSize)
	for i := range buf {
		buf[i] = byte(i)
	}
	for offset := int64(0); offset < size; offset += BlockSize {
		n := int64(BlockSize)
		if rem := size - offset; rem < n {
			n = rem
		}
		if _, err := f.WriteAt(buf[:n], offset); err != nil {
			t.Fatal(err)
		}
	}
}

// writeAt writes data at offset, creating the file if needed.
func writeAt(t *testing.T, path string, offset int64, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(data, offset); err != nil {
		t.Fatal(err)
	}
}

func readAll(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func compareFiles(t *testing.T, want, got string) {
	t.Helper()
	wantBytes := readAll(t, want)
	gotBytes := readAll(t, got)
	if !bytes.Equal(wantBytes, gotBytes) {
		t.Fatalf("%s and %s differ (%d vs %d bytes)", want, got, len(wantBytes), len(gotBytes))
	}
}

func TestFingerprintStableAcrossCopy(t *testing.T) {
	dir := t.TempDir()
	base := dir + "/base.ext4"
	writeImage(t, base, 2*BlockSize+123)
	fp, err := Fingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(fp) != 64 {
		t.Fatalf("fingerprint %q is not a sha256 hex digest", fp)
	}
	dense := dir + "/dense.ext4"
	writeImage(t, dense, 2*BlockSize+123)
	denseFP, err := Fingerprint(dense)
	if err != nil {
		t.Fatal(err)
	}
	if denseFP != fp {
		t.Fatalf("same contents fingerprint differently: %s vs %s", fp, denseFP)
	}
	// A reflinked sparse copy of the same image must fingerprint the same.
	copied := dir + "/copy.ext4"
	if err := Provision(nil, base, copied); err != nil {
		t.Fatal(err)
	}
	copyFP, err := Fingerprint(copied)
	if err != nil {
		t.Fatal(err)
	}
	if copyFP != fp {
		t.Fatalf("copied image fingerprints as %s, want %s", copyFP, fp)
	}
	writeAt(t, copied, 17, []byte("changed"))
	changedFP, err := Fingerprint(copied)
	if err != nil {
		t.Fatal(err)
	}
	if changedFP == fp {
		t.Fatal("fingerprint must change when a byte changes")
	}
}

func TestFingerprintIgnoresHoles(t *testing.T) {
	dir := t.TempDir()
	sparse := dir + "/sparse.ext4"
	writeAt(t, sparse, 0, make([]byte, 4096))
	writeAt(t, sparse, 4*BlockSize-4096, make([]byte, 4096))
	dense := dir + "/dense.ext4"
	writeAt(t, dense, 0, make([]byte, 4*BlockSize))
	sparseFP, err := Fingerprint(sparse)
	if err != nil {
		t.Fatal(err)
	}
	denseFP, err := Fingerprint(dense)
	if err != nil {
		t.Fatal(err)
	}
	if sparseFP != denseFP {
		t.Fatalf("holes changed the fingerprint: %s vs %s", sparseFP, denseFP)
	}
}

func TestWriteDeltaIdenticalImages(t *testing.T) {
	dir := t.TempDir()
	base, target := dir+"/base.ext4", dir+"/target.ext4"
	writeImage(t, base, 2*BlockSize+500)
	if err := Provision(nil, base, target); err != nil {
		t.Fatal(err)
	}
	out := dir + "/delta.bin"
	changed, err := WriteDelta(base, target, out)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Fatalf("changed bytes = %d, want 0", changed)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != deltaHeaderSize+recordHeaderSize {
		t.Fatalf("delta size = %d, want header and trailer only (%d)", info.Size(), deltaHeaderSize+recordHeaderSize)
	}
	restored := dir + "/restored.ext4"
	if err := ApplyDelta(base, out, restored); err != nil {
		t.Fatal(err)
	}
	compareFiles(t, base, restored)
}

func TestWriteDeltaMiddleBlock(t *testing.T) {
	dir := t.TempDir()
	base, target := dir+"/base.ext4", dir+"/target.ext4"
	const size = 8 * BlockSize
	writeImage(t, base, size)
	if err := Provision(nil, base, target); err != nil {
		t.Fatal(err)
	}
	writeAt(t, target, BlockSize+1234, []byte("dock delta middle block change"))
	out := dir + "/delta.bin"
	changed, err := WriteDelta(base, target, out)
	if err != nil {
		t.Fatal(err)
	}
	if changed != BlockSize {
		t.Fatalf("changed bytes = %d, want one block (%d)", changed, BlockSize)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= size/4 {
		t.Fatalf("delta is %d bytes, not much smaller than the %d byte image", info.Size(), size)
	}
	// Apply over a target the caller already provisioned.
	restored := dir + "/restored.ext4"
	if err := Provision(nil, base, restored); err != nil {
		t.Fatal(err)
	}
	if err := ApplyDelta(base, out, restored); err != nil {
		t.Fatal(err)
	}
	compareFiles(t, target, restored)
}

func TestWriteDeltaManyBlocks(t *testing.T) {
	dir := t.TempDir()
	base, target := dir+"/base.ext4", dir+"/target.ext4"
	const size = 8 * BlockSize
	writeImage(t, base, size)
	if err := Provision(nil, base, target); err != nil {
		t.Fatal(err)
	}
	for _, block := range []int64{0, 3, 4, 7} {
		writeAt(t, target, block*BlockSize+99, []byte("changed"))
	}
	out := dir + "/delta.bin"
	changed, err := WriteDelta(base, target, out)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(4 * BlockSize); changed != want {
		t.Fatalf("changed bytes = %d, want %d", changed, want)
	}
	restored := dir + "/restored.ext4"
	if err := ApplyDelta(base, out, restored); err != nil {
		t.Fatal(err)
	}
	compareFiles(t, target, restored)
}

func TestWriteDeltaPartialTailBlock(t *testing.T) {
	dir := t.TempDir()
	base, target := dir+"/base.ext4", dir+"/target.ext4"
	const tail = 777
	const size = 3*BlockSize + tail
	writeImage(t, base, size)
	if err := Provision(nil, base, target); err != nil {
		t.Fatal(err)
	}
	writeAt(t, target, 3*BlockSize, []byte("head of tail block"))
	writeAt(t, target, size-1, []byte("z"))
	out := dir + "/delta.bin"
	changed, err := WriteDelta(base, target, out)
	if err != nil {
		t.Fatal(err)
	}
	if changed != tail {
		t.Fatalf("changed bytes = %d, want the %d byte tail block", changed, tail)
	}
	restored := dir + "/restored.ext4"
	if err := ApplyDelta(base, out, restored); err != nil {
		t.Fatal(err)
	}
	compareFiles(t, target, restored)
}

func TestWriteDeltaSparseImage(t *testing.T) {
	dir := t.TempDir()
	base, target := dir+"/base.ext4", dir+"/target.ext4"
	const size = 16 * BlockSize
	// Data at both edges, a 14 block hole in between.
	edge := bytes.Repeat([]byte{0xAB}, 4096)
	writeAt(t, base, 0, edge)
	writeAt(t, base, size-4096, edge)
	if err := Provision(nil, base, target); err != nil {
		t.Fatal(err)
	}
	writeAt(t, target, 7, []byte("x"))
	writeAt(t, target, size-100, []byte("y"))
	out := dir + "/delta.bin"
	changed, err := WriteDelta(base, target, out)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(2 * BlockSize); changed != want {
		t.Fatalf("changed bytes = %d, want %d", changed, want)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= 3*BlockSize {
		t.Fatalf("delta is %d bytes: the hole was copied into it", info.Size())
	}
	restored := dir + "/restored.ext4"
	if err := ApplyDelta(base, out, restored); err != nil {
		t.Fatal(err)
	}
	compareFiles(t, target, restored)
}

func TestApplyDeltaBaseMismatch(t *testing.T) {
	dir := t.TempDir()
	base, other, target := dir+"/base.ext4", dir+"/other.ext4", dir+"/target.ext4"
	writeImage(t, base, 2*BlockSize)
	writeImage(t, other, 2*BlockSize)
	writeAt(t, other, 10, []byte("other base"))
	if err := Provision(nil, base, target); err != nil {
		t.Fatal(err)
	}
	writeAt(t, target, 500, []byte("real target"))
	out := dir + "/delta.bin"
	if _, err := WriteDelta(base, target, out); err != nil {
		t.Fatal(err)
	}
	restored := dir + "/restored.ext4"
	err := ApplyDelta(other, out, restored)
	if err == nil || !strings.Contains(err.Error(), "base mismatch") {
		t.Fatalf("err = %v, want base mismatch", err)
	}
	if _, err := os.Stat(restored); !os.IsNotExist(err) {
		t.Fatalf("mismatched delta must not create target, stat err = %v", err)
	}
}

func TestApplyDeltaCorruptMagic(t *testing.T) {
	dir := t.TempDir()
	base, target := dir+"/base.ext4", dir+"/target.ext4"
	writeImage(t, base, BlockSize)
	if err := Provision(nil, base, target); err != nil {
		t.Fatal(err)
	}
	writeAt(t, target, 42, []byte("changed"))
	out := dir + "/delta.bin"
	if _, err := WriteDelta(base, target, out); err != nil {
		t.Fatal(err)
	}
	writeAt(t, out, 0, []byte("X"))
	err := ApplyDelta(base, out, dir+"/restored.ext4")
	if err == nil || !strings.Contains(err.Error(), "magic") {
		t.Fatalf("err = %v, want bad magic", err)
	}
}

// reflinkWorks reports whether the filesystem holding dir can clone extents.
// The sharing tests below are only meaningful where it can.
func reflinkWorks(t *testing.T, dir string, size int64) bool {
	t.Helper()
	src := filepath.Join(dir, "reflink-src")
	if err := os.WriteFile(src, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "reflink-dst")
	out, err := exec.Command("cp", "--reflink=always", src, dst).CombinedOutput()
	if err != nil {
		t.Logf("reflink unavailable here, skipping sharing assertions: %s", strings.TrimSpace(string(out)))
		return false
	}
	return true
}

// TestWriteDeltaSkipsSharedBlocksWithReflink is the correctness guard for the
// speed win: a dock rootfs reflinked from the base shares extents for everything
// the guest never wrote, and those blocks must not be read or emitted. It also
// guards the subtle half — a block that was written must stop counting as shared,
// or the delta would silently omit it and restore would produce a corrupt image.
func TestWriteDeltaSkipsSharedBlocksWithReflink(t *testing.T) {
	dir := t.TempDir()
	if !reflinkWorks(t, dir, 2*BlockSize) {
		t.Skip("filesystem does not support reflink")
	}
	base := filepath.Join(dir, "base.ext4")
	writeImage(t, base, 4*BlockSize)
	target := filepath.Join(dir, "target.ext4")
	if out, err := exec.Command("cp", "--reflink=always", base, target).CombinedOutput(); err != nil {
		t.Skipf("cannot clone the base: %s", strings.TrimSpace(string(out)))
	}

	// Nothing changed: the delta is header-only, which is the whole point.
	out := filepath.Join(dir, "unchanged.delta")
	changed, err := WriteDelta(base, target, out)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Fatalf("changed = %d, want 0 for an untouched clone", changed)
	}
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != deltaHeaderSize+recordHeaderSize {
		t.Fatalf("delta is %d bytes, want only the header and trailer (%d)", fi.Size(), deltaHeaderSize+recordHeaderSize)
	}

	// One written block must be captured, not skipped as still-shared.
	writeAt(t, target, 2*BlockSize, []byte("guest wrote this block"))
	out2 := filepath.Join(dir, "changed.delta")
	changed, err = WriteDelta(base, target, out2)
	if err != nil {
		t.Fatal(err)
	}
	if changed != BlockSize {
		t.Fatalf("changed = %d, want exactly one block", changed)
	}
	if err := ApplyDelta(base, out2, filepath.Join(dir, "restored.ext4")); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "restored.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("restored %d bytes, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("restored differs at byte %d: a written block was treated as unchanged", i)
		}
	}
}

// settledClock makes every file look old enough that its timestamps are
// trustworthy, so cache behaviour can be tested without sleeping.
func settledClock(t *testing.T) {
	t.Helper()
	old := fingerprintNow
	fingerprintNow = func() time.Time { return time.Now().Add(time.Hour) }
	t.Cleanup(func() { fingerprintNow = old })
}

func cachedFingerprints() (n int) {
	fingerprintCache.Range(func(_, _ any) bool { n++; return true })
	return n
}

func TestFingerprintPersistsBesideTheImage(t *testing.T) {
	settledClock(t)
	dir := t.TempDir()
	img := filepath.Join(dir, "base.ext4")
	writeImage(t, img, 2*BlockSize)
	digest, err := Fingerprint(img)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(img + fingerprintSuffix)
	if err != nil {
		t.Fatalf("no cached fingerprint beside the image: %v", err)
	}
	info, err := os.Stat(img)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := parseFingerprintSidecar(string(raw), identityOf(info))
	if !ok {
		t.Fatalf("cached fingerprint did not validate: %q", raw)
	}
	if got != digest {
		t.Fatalf("cached digest %q != %q", got, digest)
	}
}

func TestFingerprintDoesNotCacheRacilyFreshFiles(t *testing.T) {
	// A file written moments ago may be rewritten within the filesystem's
	// timestamp granularity, so its digest must not be cached or persisted.
	dir := t.TempDir()
	img := filepath.Join(dir, "base.ext4")
	writeImage(t, img, 2*BlockSize)
	before := cachedFingerprints()
	if _, err := Fingerprint(img); err != nil {
		t.Fatal(err)
	}
	if cachedFingerprints() != before {
		t.Fatal("a freshly written image was cached")
	}
	if _, err := os.Stat(img + fingerprintSuffix); err == nil {
		t.Fatal("a freshly written image was persisted")
	}
}

func TestFingerprintSameSizeRewriteWithRestoredMtime(t *testing.T) {
	// The worst case for a size+mtime key: rewrite in place, same size, and
	// put the old mtime back, all inside one timestamp tick (ctime is coarse
	// too, so it cannot be relied on alone). The racy-timestamp guard keeps
	// the fresh digest out of the cache, so this holds with no sleeping.
	dir := t.TempDir()
	img := filepath.Join(dir, "base.ext4")
	writeImage(t, img, 2*BlockSize)
	first, err := Fingerprint(img)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(img)
	if err != nil {
		t.Fatal(err)
	}
	writeAt(t, img, 17, []byte("changed"))
	if err := os.Chtimes(img, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	second, err := Fingerprint(img)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("stale fingerprint after a same-size rewrite")
	}
}

func TestParseFingerprintSidecarRejectsAMismatch(t *testing.T) {
	// A stale cache must never be trusted: a rebuilt image has a new size,
	// mtime, ctime or inode, and reusing the old digest would validate the
	// wrong base.
	id := fileIdentity{size: 4096, mtime: 1700000000000000000, ctime: 1700000000000000005, ino: 42}
	valid := "4096 1700000000000000000 1700000000000000005 42 abcdef"
	if _, ok := parseFingerprintSidecar(valid, id); !ok {
		t.Fatal("a matching sidecar must be accepted")
	}
	mut := func(f func(*fileIdentity)) fileIdentity { c := id; f(&c); return c }
	for name, tc := range map[string]struct {
		raw string
		id  fileIdentity
	}{
		"size changed":  {valid, mut(func(c *fileIdentity) { c.size = 8192 })},
		"mtime changed": {valid, mut(func(c *fileIdentity) { c.mtime++ })},
		"ctime changed": {valid, mut(func(c *fileIdentity) { c.ctime++ })},
		"inode changed": {valid, mut(func(c *fileIdentity) { c.ino++ })},
		"old format":    {"4096 1700000000000000000 abcdef", id},
		"truncated":     {"4096 1700000000000000000 1700000000000000005 42", id},
		"garbage":       {"not a cache line at all", id},
		"empty":         {"", id},
		"extra field":   {valid + " extra", id},
		"non-numeric":   {"big 1700000000000000000 1700000000000000005 42 abcdef", id},
	} {
		if _, ok := parseFingerprintSidecar(tc.raw, tc.id); ok {
			t.Fatalf("%s: stale or malformed cache was accepted", name)
		}
	}
}

func TestWriteDeltaSizeMismatch(t *testing.T) {
	dir := t.TempDir()
	base, target := dir+"/base.ext4", dir+"/target.ext4"
	writeImage(t, base, 3*BlockSize)
	writeImage(t, target, 2*BlockSize)
	out := dir + "/delta.bin"
	_, err := WriteDelta(base, target, out)
	if err == nil {
		t.Fatal("a target smaller than its base must be rejected")
	}
	if !strings.Contains(err.Error(), base) || !strings.Contains(err.Error(), target) {
		t.Fatalf("err = %v, want both file names", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("rejected delta must not leave output, stat err = %v", err)
	}
}

// twoRecordDelta builds a delta with two changed blocks and returns its path and the base and target.
func twoRecordDelta(t *testing.T) (dir, base, target, out string) {
	t.Helper()
	dir = t.TempDir()
	base, target, out = dir+"/base.ext4", dir+"/target.ext4", dir+"/delta.bin"
	writeImage(t, base, 3*BlockSize)
	if err := Provision(nil, base, target); err != nil {
		t.Fatal(err)
	}
	writeAt(t, target, 10, []byte("first change"))
	writeAt(t, target, 2*BlockSize+10, []byte("second change"))
	if _, err := WriteDelta(base, target, out); err != nil {
		t.Fatal(err)
	}
	return dir, base, target, out
}

func TestApplyDeltaRejectsTruncationAtRecordBoundary(t *testing.T) {
	dir, base, _, out := twoRecordDelta(t)
	raw := readAll(t, out)
	recordSize := recordHeaderSize + BlockSize
	cases := map[string][]byte{
		"trailer cut off":               raw[:len(raw)-recordHeaderSize],
		"last record and trailer":       raw[:len(raw)-recordHeaderSize-recordSize],
		"last record cut, trailer kept": append(append([]byte{}, raw[:len(raw)-recordHeaderSize-recordSize]...), raw[len(raw)-recordHeaderSize:]...),
	}
	for name, data := range cases {
		cut := filepath.Join(dir, "cut.bin")
		if err := os.WriteFile(cut, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ApplyDelta(base, cut, filepath.Join(dir, "restored-"+strings.ReplaceAll(name, " ", "-"))); err == nil {
			t.Fatalf("%s: truncated delta applied without error", name)
		}
	}
}

func TestApplyDeltaStillReadsLegacyDeltaWithoutTrailer(t *testing.T) {
	dir, base, target, out := twoRecordDelta(t)
	raw := readAll(t, out)
	legacy := append([]byte{}, raw[:len(raw)-recordHeaderSize]...)
	copy(legacy[:8], deltaMagicV1)
	legacyPath := filepath.Join(dir, "legacy.bin")
	if err := os.WriteFile(legacyPath, legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(dir, "restored.ext4")
	if err := ApplyDelta(base, legacyPath, restored); err != nil {
		t.Fatalf("legacy delta: %v", err)
	}
	compareFiles(t, target, restored)
}

func TestApplyDeltaNewFormatRoundTrips(t *testing.T) {
	dir, base, target, out := twoRecordDelta(t)
	restored := filepath.Join(dir, "restored.ext4")
	if err := ApplyDelta(base, out, restored); err != nil {
		t.Fatal(err)
	}
	compareFiles(t, target, restored)
}

// A dock disk grown past its base (diskGiB above the base size) still captures
// as a delta and restores byte for byte, including data past the base's end.
func TestDeltaRoundTripsAGrownTarget(t *testing.T) {
	dir := t.TempDir()
	base, target, out := dir+"/base.ext4", dir+"/target.ext4", dir+"/delta.bin"
	writeImage(t, base, 2*BlockSize+BlockSize/2)
	if err := Provision(nil, base, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(target, 5*BlockSize); err != nil {
		t.Fatal(err)
	}
	writeAt(t, target, 10, []byte("inside the base"))
	writeAt(t, target, 4*BlockSize+10, []byte("past the base"))
	changed, err := WriteDelta(base, target, out)
	if err != nil {
		t.Fatal(err)
	}
	// Only the two written blocks differ; the zero tail past the base does not.
	if changed != 2*BlockSize {
		t.Fatalf("changed = %d, want %d", changed, 2*BlockSize)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := readDeltaHeader(f, out)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if meta.ImageSize != 5*BlockSize {
		t.Fatalf("image size = %d, want the grown %d", meta.ImageSize, 5*BlockSize)
	}
	restored := dir + "/restored.ext4"
	if err := ApplyDelta(base, out, restored); err != nil {
		t.Fatal(err)
	}
	compareFiles(t, target, restored)
}
