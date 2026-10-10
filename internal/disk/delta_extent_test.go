package disk

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMergeAndSubtractRanges(t *testing.T) {
	cases := []struct {
		name       string
		candidates [][2]int64
		cut        [][2]int64
		want       [][2]int64
	}{
		{
			name:       "overlapping candidates collapse",
			candidates: [][2]int64{{0, 10}, {5, 20}, {30, 40}},
			want:       [][2]int64{{0, 20}, {30, 40}},
		},
		{
			name:       "touching candidates collapse",
			candidates: [][2]int64{{0, 10}, {10, 20}},
			want:       [][2]int64{{0, 20}},
		},
		{
			name:       "unsorted input is sorted",
			candidates: [][2]int64{{30, 40}, {0, 10}},
			want:       [][2]int64{{0, 10}, {30, 40}},
		},
		{
			name:       "cut removes the middle",
			candidates: [][2]int64{{0, 100}},
			cut:        [][2]int64{{40, 60}},
			want:       [][2]int64{{0, 40}, {60, 100}},
		},
		{
			name:       "cut at the start leaves the tail",
			candidates: [][2]int64{{0, 100}},
			cut:        [][2]int64{{0, 30}},
			want:       [][2]int64{{30, 100}},
		},
		{
			name:       "cut at the end leaves the head",
			candidates: [][2]int64{{0, 100}},
			cut:        [][2]int64{{70, 100}},
			want:       [][2]int64{{0, 70}},
		},
		{
			name:       "cut covering everything leaves nothing",
			candidates: [][2]int64{{0, 100}},
			cut:        [][2]int64{{0, 100}},
			want:       nil,
		},
		{
			name:       "one cut spans several candidates",
			candidates: [][2]int64{{0, 10}, {20, 30}, {40, 50}},
			cut:        [][2]int64{{5, 45}},
			want:       [][2]int64{{0, 5}, {45, 50}},
		},
		{
			name:       "cut outside the candidates changes nothing",
			candidates: [][2]int64{{10, 20}},
			cut:        [][2]int64{{100, 200}},
			want:       [][2]int64{{10, 20}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := subtractRanges(mergeRanges(tc.candidates), mergeRanges(tc.cut))
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestWriteDeltaPunchedHole covers the case the extent walk must not lose: the
// base has data where the target now has a hole. Those bytes are not shared
// (there is nothing to share) and do not appear as a target extent, so only the
// base's extents reveal that the delta has to clear them. Skipping the region
// would restore the base's stale bytes into the dock.
func TestWriteDeltaPunchedHole(t *testing.T) {
	dir := t.TempDir()
	if !reflinkWorks(t, dir, 4*BlockSize) {
		t.Skip("filesystem does not support reflink")
	}
	base := filepath.Join(dir, "base.ext4")
	writeImage(t, base, 4*BlockSize)
	target := filepath.Join(dir, "target.ext4")
	if out, err := exec.Command("cp", "--reflink=always", base, target).CombinedOutput(); err != nil {
		t.Skipf("cannot clone the base: %s", strings.TrimSpace(string(out)))
	}

	// Free the middle blocks the way a discard-capable guest would.
	f, err := os.OpenFile(target, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, BlockSize, BlockSize); err != nil {
		f.Close()
		t.Skipf("filesystem cannot punch holes (%v); restoring a discard needs this to be exercised", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	delta := filepath.Join(dir, "punched.delta")
	if _, err := WriteDelta(base, target, delta); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(dir, "restored.ext4")
	if err := ApplyDelta(base, delta, restored); err != nil {
		t.Fatal(err)
	}
	compareFiles(t, target, restored)
}

// TestExamineRangesSkipsUnwrittenAndHoles pins the cost of a capture against a
// real image layout: freshly allocated images are mostly unwritten extents,
// which read as zeros and are not shared with a reflink clone. Counting them as
// candidates would read gigabytes of zeros on every capture, so the ranges must
// stay confined to bytes that actually hold data.
func TestExamineRangesSkipsUnwrittenAndHoles(t *testing.T) {
	dir := t.TempDir()
	if !reflinkWorks(t, dir, 4*BlockSize) {
		t.Skip("filesystem does not support reflink")
	}
	base := filepath.Join(dir, "base.ext4")
	f, err := os.Create(base)
	if err != nil {
		t.Fatal(err)
	}
	size := int64(4 * BlockSize)
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_KEEP_SIZE, 0, size); err != nil {
		f.Close()
		t.Skipf("filesystem cannot allocate unwritten extents (%v)", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	extents, err := Extents(base)
	if err != nil {
		t.Fatal(err)
	}
	unwritten := 0
	for _, e := range extents {
		if e.Unwritten {
			unwritten++
		}
	}
	if unwritten == 0 {
		t.Skip("filesystem does not report unwritten extents; the filter cannot be exercised here")
	}

	target := filepath.Join(dir, "target.ext4")
	if out, err := exec.Command("cp", "--reflink=always", base, target).CombinedOutput(); err != nil {
		t.Skipf("cannot clone the base: %s", strings.TrimSpace(string(out)))
	}
	// A hole in the clone over a base extent that holds no data: both read as
	// zeros, so no record is needed.
	tf, err := os.OpenFile(target, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Fallocate(int(tf.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, BlockSize, BlockSize); err != nil {
		tf.Close()
		t.Skipf("filesystem cannot punch holes (%v)", err)
	}
	if err := tf.Close(); err != nil {
		t.Fatal(err)
	}

	ranges, ok := examineRanges(base, target, size)
	if !ok {
		t.Fatal("examineRanges reported that extents are unavailable")
	}
	var total int64
	for _, r := range ranges {
		total += r[1] - r[0]
	}
	if total != 0 {
		t.Fatalf("examineRanges covers %d bytes of zero-filled extents, want none: %v", total, ranges)
	}

	delta := filepath.Join(dir, "zero.delta")
	changed, err := WriteDelta(base, target, delta)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Fatalf("changed = %d, want 0 when only zero-filled extents differ", changed)
	}
	if fi, err := os.Stat(delta); err != nil {
		t.Fatal(err)
	} else if fi.Size() != deltaHeaderSize+recordHeaderSize {
		t.Fatalf("delta is %d bytes, want only the header and trailer (%d)", fi.Size(), deltaHeaderSize+recordHeaderSize)
	}
}

// TestWriteDeltaSiblingCloneNotSharedWithBase reproduces a silent-corruption
// case: FIEMAP reports an extent as shared when it is shared with *any* inode,
// not necessarily with the base. A dock cloned from another dock inherits that
// dock's blocks, so an extent can be shared (with the sibling) while holding
// bytes that differ from the base. Treating it as shared emits a delta that
// restores the base's bytes over the dock's data.
func TestWriteDeltaSiblingCloneNotSharedWithBase(t *testing.T) {
	dir := t.TempDir()
	if !reflinkWorks(t, dir, 4*BlockSize) {
		t.Skip("filesystem does not support reflink")
	}
	// The base and the sibling start as the same zero-filled image.
	base := filepath.Join(dir, "base.ext4")
	writeImage(t, base, 4*BlockSize)
	sibling := filepath.Join(dir, "sibling.ext4")
	if out, err := exec.Command("cp", "--reflink=always", base, sibling).CombinedOutput(); err != nil {
		t.Skipf("cannot clone the base: %s", strings.TrimSpace(string(out)))
	}
	// The sibling writes a distinctive block; its copy-on-write blocks now
	// differ from the base while remaining shareable.
	marked := bytes.Repeat([]byte{0xC7}, BlockSize)
	writeAt(t, sibling, BlockSize, marked)
	// The dock under test is cloned from the sibling, so it shares the
	// sibling's written blocks, not the base's.
	target := filepath.Join(dir, "target.ext4")
	if out, err := exec.Command("cp", "--reflink=always", sibling, target).CombinedOutput(); err != nil {
		t.Skipf("cannot clone the sibling: %s", strings.TrimSpace(string(out)))
	}

	delta := filepath.Join(dir, "sibling.delta")
	changed, err := WriteDelta(base, target, delta)
	if err != nil {
		t.Fatal(err)
	}
	if changed < BlockSize {
		t.Fatalf("changed = %d, want at least one block: the block differs from the base", changed)
	}
	restored := filepath.Join(dir, "restored.ext4")
	if err := ApplyDelta(base, delta, restored); err != nil {
		t.Fatal(err)
	}
	compareFiles(t, target, restored)
}
