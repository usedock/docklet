//go:build linux

package disk

import (
	"cmp"
	"encoding/binary"
	"errors"
	"os"
	"slices"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Extent is one mapped extent of a file, tagged with whether its blocks are
// still shared with another file (a reflink copy) and whether their contents
// are unwritten, meaning they read as zeros without holding data.
//
// Shared means shared with *some* inode, not necessarily the one the caller
// cares about; Physical is what distinguishes the two, since reflink sharing
// means the same physical blocks.
type Extent struct {
	Start     int64 // logical offset in bytes
	End       int64 // exclusive
	Physical  int64 // physical offset of the first block
	Shared    bool
	Unwritten bool
}

// FIEMAP ABI from linux/fiemap.h. The pinned x/sys/unix has no bindings for
// it, so the ioctl number and record layout are spelled out here.
const (
	fsIOCFIEMAP        = 0xc020660b // _IOWR('f', 11, struct fiemap)
	fiemapFlagSync     = 0x00000001
	fiemapExtentLast   = 0x00000001
	fiemapExtentShared = 0x00002000
	// Unwritten extents are allocated but hold no data: they read as zeros, so
	// a capture can treat them as holes rather than reading them. Images are
	// created with fallocate, which leaves most of a fresh image unwritten.
	fiemapExtentUnwritten = 0x00000800
	// struct fiemap: fm_start, fm_length (u64), fm_flags, fm_mapped_extents,
	// fm_extent_count, fm_reserved (u32); struct fiemap_extent: fe_logical,
	// fe_physical, fe_length (u64), fe_reserved64[2] (u64), fe_flags (u32),
	// fe_reserved[3] (u32).
	fiemapHeaderSize = 32
	fiemapExtentSize = 56
	fiemapBatch      = 512
)

// Extents returns every mapped extent of path in ascending order, each tagged
// Shared or not. A nil slice with nil error means "cannot tell" (unsupported
// filesystem or kernel), which callers treat as a fallback condition.
func Extents(path string) ([]Extent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	if size <= 0 {
		return []Extent{}, nil
	}

	buf := make([]byte, fiemapHeaderSize+fiemapBatch*fiemapExtentSize)
	var extents []Extent
	for start := int64(0); start < size; {
		mapped, err := fiemapQuery(f, buf, start, size-start)
		if err != nil {
			if fiemapUnsupported(err) {
				return nil, nil
			}
			return nil, err
		}
		if mapped == 0 {
			break
		}
		// One batch covers at most fiemapBatch extents; walk on from the end
		// of the last one until the file is exhausted or LAST is reported.
		end, last := fiemapCollect(buf, mapped, size, &extents)
		if last || end <= start {
			break
		}
		start = end
	}
	if extents == nil {
		// No mapped extents at all: distinct from "cannot tell".
		return []Extent{}, nil
	}
	return mergeExtents(extents), nil
}

// fiemapQuery asks for up to fiemapBatch extents of length bytes from start
// and returns how many the kernel mapped.
func fiemapQuery(f *os.File, buf []byte, start, length int64) (int, error) {
	if length <= 0 {
		return 0, nil
	}
	h := buf[:fiemapHeaderSize]
	clear(h)
	binary.NativeEndian.PutUint64(h[0:], uint64(start))
	binary.NativeEndian.PutUint64(h[8:], uint64(length))
	// Without SYNC a block written since the last writeback still carries the
	// stale shared extent and would be wrongly reported as skippable.
	binary.NativeEndian.PutUint32(h[16:], fiemapFlagSync)
	binary.NativeEndian.PutUint32(h[24:], fiemapBatch)
	if err := ioctlFiemap(f, buf); err != nil {
		return 0, err
	}
	n := int(binary.NativeEndian.Uint32(h[20:]))
	if n > fiemapBatch {
		n = fiemapBatch
	}
	return n, nil
}

// fiemapCollect appends the extents in buf's extent array and reports the end
// of the last mapped extent plus whether FIEMAP_EXTENT_LAST was set.
func fiemapCollect(buf []byte, mapped int, size int64, out *[]Extent) (end int64, last bool) {
	for i := range mapped {
		off := fiemapHeaderSize + i*fiemapExtentSize
		logical := int64(binary.NativeEndian.Uint64(buf[off:]))
		physical := int64(binary.NativeEndian.Uint64(buf[off+8:]))
		length := int64(binary.NativeEndian.Uint64(buf[off+16:]))
		flags := binary.NativeEndian.Uint32(buf[off+40:])
		if flags&fiemapExtentLast != 0 {
			last = true
		}
		if e := logical + length; e > end {
			end = e
		}
		// FIEMAP reports within i_size, so the clamp is only reachable if the
		// file shrank between Stat and the ioctl.
		stop := logical + length
		if stop > size {
			stop = size
		}
		if logical >= stop {
			continue
		}
		*out = append(*out, Extent{
			Start:     logical,
			End:       stop,
			Physical:  physical,
			Shared:    flags&fiemapExtentShared != 0,
			Unwritten: flags&fiemapExtentUnwritten != 0,
		})
	}
	return end, last
}

// ioctlFiemap issues FS_IOC_FIEMAP on buf, whose first word is struct fiemap
// followed by room for fiemapBatch struct fiemap_extent records.
func ioctlFiemap(f *os.File, buf []byte) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uintptr(fsIOCFIEMAP), uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return errno
	}
	return nil
}

// fiemapUnsupported reports the errnos that mean "this filesystem cannot tell
// us"; the caller degrades to byte comparison rather than failing.
func fiemapUnsupported(err error) bool {
	return errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.ENOSYS)
}

// mergeExtents returns the extents sorted, disjoint, and coalesced.
//
// FIEMAP does not return overlapping extents for one inode, so overlaps only
// appear when a filesystem misbehaves. They are resolved in the conservative
// direction: a byte covered by extents that disagree is reported as neither
// shared nor unwritten, which costs a read but can never hide a change.
func mergeExtents(es []Extent) []Extent {
	if len(es) < 2 {
		return es
	}
	slices.SortStableFunc(es, func(a, b Extent) int { return cmp.Compare(a.Start, b.Start) })
	// Every cut point is an extent boundary, so each interval between two cuts
	// has one settled tag.
	cuts := make([]int64, 0, 2*len(es))
	for _, e := range es {
		cuts = append(cuts, e.Start, e.End)
	}
	slices.Sort(cuts)
	cuts = slices.Compact(cuts)

	out := make([]Extent, 0, len(es))
	for i := 0; i+1 < len(cuts); i++ {
		start, end := cuts[i], cuts[i+1]
		if start >= end {
			continue
		}
		shared, unwritten, physical, covered := true, true, int64(0), false
		for _, e := range es {
			if e.Start > start || e.End < end {
				continue
			}
			if !covered {
				covered, physical = true, e.Physical+start-e.Start
			}
			shared = shared && e.Shared
			unwritten = unwritten && e.Unwritten
		}
		if !covered {
			continue
		}
		if last := len(out) - 1; last >= 0 && out[last].End == start &&
			out[last].Shared == shared && out[last].Unwritten == unwritten &&
			out[last].Physical+start-out[last].Start == physical {
			out[last].End = end
			continue
		}
		out = append(out, Extent{Start: start, End: end, Physical: physical, Shared: shared, Unwritten: unwritten})
	}
	return out
}
