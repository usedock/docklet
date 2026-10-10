package disk

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// checkExtents asserts the invariants every Extents result must satisfy: each
// extent non-empty and inside [0, size), the list sorted, non-overlapping, and
// with touching extents merged unless their Shared tag differs.
func checkExtents(t *testing.T, extents []Extent, size int64) {
	t.Helper()
	prev := int64(-1)
	var prevShared bool
	for i, e := range extents {
		if e.End <= e.Start {
			t.Fatalf("extent %d = [%d,%d): End must exceed Start", i, e.Start, e.End)
		}
		if e.Start < 0 || e.End > size {
			t.Fatalf("extent %d = [%d,%d): outside file [0,%d)", i, e.Start, e.End, size)
		}
		if e.Start < prev {
			t.Fatalf("extent %d = [%d,%d): overlaps previous end %d", i, e.Start, e.End, prev)
		}
		if i > 0 && e.Start == prev && e.Shared == prevShared {
			t.Fatalf("extent %d = [%d,%d): abuts previous extent with the same Shared=%v, must be merged", i, e.Start, e.End, e.Shared)
		}
		prev, prevShared = e.End, e.Shared
	}
}

// reflinkCopy clones src to dst with a reflink and reports whether the
// filesystem supports it; a false return means sharing cannot be observed
// here, not that the test failed.
func reflinkCopy(src, dst string) (ok bool, detail string) {
	out, err := exec.Command("cp", "--reflink=always", src, dst).CombinedOutput()
	if err != nil {
		return false, err.Error() + ": " + string(out)
	}
	return true, ""
}

func TestExtentsMissingFile(t *testing.T) {
	if _, err := Extents(filepath.Join(t.TempDir(), "absent.ext4")); err == nil {
		t.Fatal("missing file must return an error")
	}
}

func TestExtentsRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image.ext4")
	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	extents, err := Extents(path)
	if err != nil {
		t.Fatalf("Extents(%s) = %v", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	checkExtents(t, extents, info.Size())
}

func TestExtentsReflinkCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "base.ext4")
	payload := make([]byte, 4<<20)
	for i := range payload {
		payload[i] = byte(i)
	}
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "copy.ext4")
	if ok, detail := reflinkCopy(src, dst); !ok {
		t.Skipf("filesystem refuses reflink (%s); sharing cannot be observed here", detail)
	}

	extents, err := Extents(dst)
	if err != nil {
		t.Fatalf("Extents(%s) = %v", dst, err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	checkExtents(t, extents, info.Size())
	if !anyShared(extents) {
		// Reflink works but this filesystem does not report FIEMAP_EXTENT_SHARED;
		// the caller falls back to comparing bytes, so this is not a failure.
		t.Log("reflink copy reports no shared extents; byte comparison still applies")
	}
}

// TestExtentsReflinkWrite proves the property the delta optimisation rests on:
// a region the guest wrote must stop being reported as shared.
func TestExtentsReflinkWrite(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "base.ext4")
	payload := make([]byte, 8<<20)
	for i := range payload {
		payload[i] = byte(i)
	}
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "copy.ext4")
	if ok, detail := reflinkCopy(src, dst); !ok {
		t.Skipf("filesystem refuses reflink (%s); sharing cannot be observed here", detail)
	}
	size := int64(len(payload))

	before, err := Extents(dst)
	if err != nil {
		t.Fatalf("Extents(%s) = %v", dst, err)
	}
	checkExtents(t, before, size)
	if !anyShared(before) {
		t.Skip("filesystem does not report FIEMAP_EXTENT_SHARED; sharing cannot be observed here")
	}

	const off, n = int64(4 << 20), int64(64 << 10)
	if e, ok := extentCovering(before, off); !ok {
		t.Fatalf("no extent covers the region about to be written: %+v", before)
	} else if !e.Shared {
		t.Fatalf("extent [%d,%d) was not shared before the write", e.Start, e.End)
	}

	write := make([]byte, n)
	for i := range write {
		write[i] = 0xAB
	}
	f, err := os.OpenFile(dst, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(write, off); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := Extents(dst)
	if err != nil {
		t.Fatalf("Extents(%s) after write = %v", dst, err)
	}
	checkExtents(t, after, size)
	covered := false
	for _, e := range after {
		if e.Start < off+n && off < e.End {
			covered = true
			if e.Shared {
				t.Fatalf("extent [%d,%d) still reports shared after writing [%d,%d)", e.Start, e.End, off, off+n)
			}
		}
	}
	if !covered {
		t.Fatalf("no extent covers the written region [%d,%d): %+v", off, off+n, after)
	}
}

func anyShared(extents []Extent) bool {
	for _, e := range extents {
		if e.Shared {
			return true
		}
	}
	return false
}

func extentCovering(extents []Extent, off int64) (Extent, bool) {
	for _, e := range extents {
		if e.Start <= off && off < e.End {
			return e, true
		}
	}
	return Extent{}, false
}

// TestMergeExtentsResolvesOverlapConservatively pins what happens if a
// filesystem ever returns overlapping extents: FIEMAP does not do that for one
// inode, so a byte covered by extents that disagree must come back as neither
// shared nor unwritten. Guessing either way could hide a change (a missed range
// restores wrong bytes) or skip a needed read.
func TestMergeExtentsResolvesOverlapConservatively(t *testing.T) {
	merged := mergeExtents([]Extent{
		{Start: 0, End: 100, Physical: 0, Shared: true},
		{Start: 50, End: 150, Physical: 500, Unwritten: true},
	})

	if len(merged) != 3 {
		t.Fatalf("merged into %d extents, want 3: %+v", len(merged), merged)
	}
	first, middle, last := merged[0], merged[1], merged[2]
	if first.Start != 0 || first.End != 50 || !first.Shared {
		t.Fatalf("first = %+v, want the uncontested shared prefix [0,50)", first)
	}
	if middle.Start != 50 || middle.End != 100 || middle.Shared || middle.Unwritten {
		t.Fatalf("middle = %+v, want the contested range [50,100) with neither tag", middle)
	}
	if last.Start != 100 || last.End != 150 || !last.Unwritten {
		t.Fatalf("last = %+v, want the uncontested unwritten tail [100,150)", last)
	}
}
