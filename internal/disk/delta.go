package disk

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BlockSize is the delta granularity: one record per changed 1MiB block.
// fingerprintSuffix names the cached base-identity file kept beside an image.
const fingerprintSuffix = ".fingerprint"

const BlockSize = 1 << 20

// deltaMagicV1 marks legacy deltas without a trailer; deltaMagic marks deltas that end in a count trailer.
const deltaMagicV1 = "DOCKDLT1"

const deltaMagic = "DOCKDLT2"

// deltaHeaderSize is magic, base fingerprint, imageSize, and blockSize.
const deltaHeaderSize = 8 + 64 + 8 + 8

// recordHeaderSize is offset plus length.
const recordHeaderSize = 8 + 4

// DeltaMeta is the header a delta file carries.
type DeltaMeta struct {
	BaseFingerprint string
	ImageSize       int64
	BlockSize       int64
	// Trailer is true for deltas that end in a record-count trailer.
	Trailer bool
}

// fingerprintCache maps an image's identity (path, device, inode, size,
// mtime, ctime) to a digest; images are immutable per host, so repeated
// captures must not re-hash 20GB.
var fingerprintCache sync.Map

// racyWindow is how recently a file must have changed for its timestamps to be
// untrustworthy. Filesystems stamp mtime and ctime at a coarse granularity
// (a jiffy on ext4, 2s on FAT), so a same-size rewrite shortly after a hash
// can carry identical timestamps. Like git's racily-clean index entries, a
// digest computed while the file is that fresh is never cached.
const racyWindow = 2 * time.Second

// fingerprintNow is the clock used for the racy-timestamp check; tests
// replace it.
var fingerprintNow = time.Now

// fileIdentity is everything cheap that changes when a file is rewritten.
type fileIdentity struct {
	size, mtime, ctime int64
	ino, dev           uint64
}

func identityOf(info os.FileInfo) fileIdentity {
	ctime, ino, dev := statIdentity(info)
	return fileIdentity{size: info.Size(), mtime: info.ModTime().UnixNano(), ctime: ctime, ino: ino, dev: dev}
}

// racy reports whether the file changed too recently, relative to when it was
// hashed, for its timestamps to prove it has not changed since.
func (id fileIdentity) racy(hashStart time.Time) bool {
	newest := id.mtime
	if id.ctime > newest {
		newest = id.ctime
	}
	return hashStart.UnixNano()-newest < int64(racyWindow)
}

// Fingerprint returns a stable content hash of an image's full logical
// contents, independent of where its holes are.
func Fingerprint(path string) (string, error) {
	hashStart := fingerprintNow()
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("fingerprint %s: %w", path, err)
	}
	id := identityOf(info)
	key := fmt.Sprintf("%s\x00%d\x00%d\x00%d\x00%d\x00%d", path, id.dev, id.ino, id.size, id.mtime, id.ctime)
	if cached, ok := fingerprintCache.Load(key); ok {
		return cached.(string), nil
	}
	// A golden image is immutable for a host's lifetime but hashing it costs
	// tens of seconds, so the result is also persisted beside it and survives a
	// docklet restart. The identity is the key, so a rebuilt or rewritten
	// image misses. (The device number is not persisted: it can be renumbered
	// across a reboot.)
	sidecar := path + fingerprintSuffix
	if raw, err := os.ReadFile(sidecar); err == nil && !id.racy(hashStart) {
		if digest, ok := parseFingerprintSidecar(string(raw), id); ok {
			fingerprintCache.Store(key, digest)
			return digest, nil
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("fingerprint %s: %w", path, err)
	}
	defer f.Close()
	// Reads through the hole map cost nothing: the kernel hands back zeros.
	sum := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			sum.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("fingerprint %s: %w", path, err)
		}
	}
	digest := hex.EncodeToString(sum.Sum(nil))
	if id.racy(hashStart) {
		return digest, nil
	}
	// Re-stat: a write during the hash changes the identity, and then the
	// digest may describe neither version, so it must not be cached.
	if after, err := os.Stat(path); err != nil || identityOf(after) != id {
		return digest, nil
	}
	fingerprintCache.Store(key, digest)
	// Best effort: an unwritable directory must not fail a capture.
	_ = os.WriteFile(sidecar, []byte(fmt.Sprintf("%d %d %d %d %s\n", id.size, id.mtime, id.ctime, id.ino, digest)), 0o644)
	return digest, nil
}

// parseFingerprintSidecar accepts a cached digest only when it was recorded
// for exactly this file identity (size, mtime, ctime, and inode).
func parseFingerprintSidecar(raw string, id fileIdentity) (string, bool) {
	fields := strings.Fields(raw)
	if len(fields) != 5 {
		return "", false
	}
	want := [4]int64{id.size, id.mtime, id.ctime, int64(id.ino)}
	for i, w := range want {
		got, err := strconv.ParseInt(fields[i], 10, 64)
		if err != nil || got != w {
			return "", false
		}
	}
	return fields[4], true
}

// WriteDelta writes every block of target that differs from base into out.
// target may be larger than base (a grown disk); bytes past the base read as zeros.
func WriteDelta(base, target, out string) (changedBytes int64, err error) {
	baseFile, err := os.Open(base)
	if err != nil {
		return 0, fmt.Errorf("write delta: open base %s: %w", base, err)
	}
	defer baseFile.Close()
	targetFile, err := os.Open(target)
	if err != nil {
		return 0, fmt.Errorf("write delta: open target %s: %w", target, err)
	}
	defer targetFile.Close()
	baseInfo, err := baseFile.Stat()
	if err != nil {
		return 0, fmt.Errorf("write delta: stat base %s: %w", base, err)
	}
	targetInfo, err := targetFile.Stat()
	if err != nil {
		return 0, fmt.Errorf("write delta: stat target %s: %w", target, err)
	}
	if targetInfo.Size() < baseInfo.Size() {
		return 0, fmt.Errorf("write delta: base %s is %d bytes but target %s is only %d bytes", base, baseInfo.Size(), target, targetInfo.Size())
	}
	fp, err := Fingerprint(base)
	if err != nil {
		return 0, fmt.Errorf("write delta: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(out), filepath.Base(out)+".tmp*")
	if err != nil {
		return 0, fmt.Errorf("write delta: create temp next to %s: %w", out, err)
	}
	changedBytes, err = writeDelta(tmp, base, target, baseFile, targetFile, targetInfo.Size(), fp, out)
	if closeErr := tmp.Close(); err == nil && closeErr != nil {
		err = fmt.Errorf("write delta %s: %w", out, closeErr)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return 0, err
	}
	if err := os.Rename(tmp.Name(), out); err != nil {
		return 0, fmt.Errorf("write delta: rename temp into %s: %w", out, err)
	}
	return changedBytes, nil
}

// writeDelta emits the header and one record per changed block.
//
// Blocks still sharing physical extents with the base hold the base's bytes by
// construction, so they are skipped without being read: the dock rootfs was
// reflinked from the base, and everything the guest never wrote is still
// shared. Restricting the comparison to the remaining ranges is what keeps a
// capture proportional to the dock's work rather than the image size; the
// ranges are found from extent maps, never by scanning the image.
//
// When extents cannot be read the comparison falls back to reading every
// block, which is correct but proportional to the image size.
func writeDelta(w io.Writer, basePath, targetPath string, base, target *os.File, size int64, fp, out string) (int64, error) {
	if err := writeDeltaHeader(w, fp, size, BlockSize); err != nil {
		return 0, fmt.Errorf("write delta %s: %w", out, err)
	}
	examine, ok := examineRanges(basePath, targetPath, size)
	if !ok {
		return compareBlocks(w, base, target, basePath, targetPath, size, out)
	}
	var records int64
	baseBuf := make([]byte, BlockSize)
	targetBuf := make([]byte, BlockSize)
	var changed int64
	// Blocks are compared whole, so a range boundary that falls inside a block
	// re-covers that block; nextBlock keeps each block visited once.
	var nextBlock int64
	for _, r := range examine {
		start := r[0] / BlockSize * BlockSize
		for offset := start; offset < r[1]; offset += BlockSize {
			if offset < nextBlock {
				continue
			}
			n := int64(BlockSize)
			if rem := size - offset; rem < n {
				n = rem
			}
			nextBlock = offset + n
			if err := readBlockAt(base, basePath, offset, baseBuf[:n]); err != nil {
				return 0, err
			}
			if err := readBlockAt(target, targetPath, offset, targetBuf[:n]); err != nil {
				return 0, err
			}
			if bytes.Equal(baseBuf[:n], targetBuf[:n]) {
				continue
			}
			if err := writeRecord(w, out, offset, targetBuf[:n]); err != nil {
				return 0, err
			}
			changed += n
			records++
		}
	}
	if err := writeTrailer(w, out, records); err != nil {
		return 0, err
	}
	return changed, nil
}

// compareBlocks emits a record for every block of the image whose bytes differ.
//
// This is the no-extent fallback: it proves each block by its bytes, at the
// cost of reading the whole image.
func compareBlocks(w io.Writer, base, target *os.File, basePath, targetPath string, size int64, out string) (int64, error) {
	baseBuf := make([]byte, BlockSize)
	targetBuf := make([]byte, BlockSize)
	var changed, records int64
	for offset := int64(0); offset < size; offset += BlockSize {
		n := int64(BlockSize)
		if rem := size - offset; rem < n {
			n = rem
		}
		if err := readBlockAt(base, basePath, offset, baseBuf[:n]); err != nil {
			return 0, err
		}
		if err := readBlockAt(target, targetPath, offset, targetBuf[:n]); err != nil {
			return 0, err
		}
		if !bytes.Equal(baseBuf[:n], targetBuf[:n]) {
			if err := writeRecord(w, out, offset, targetBuf[:n]); err != nil {
				return 0, err
			}
			changed += n
			records++
		}
	}
	if err := writeTrailer(w, out, records); err != nil {
		return 0, err
	}
	return changed, nil
}

// examineRanges returns the byte ranges of target that can differ from base, in
// ascending order, reporting false when extent maps are unavailable.
//
// A block whose bytes cannot differ needs no record. Sharing removes most of the
// image: a block still shared with the base holds the base's bytes by
// construction. Sharing alone does not prove that, though, because the flag says
// the blocks are shared with some inode, not with this base, so a rootfs cloned
// from another dock reports shared blocks whose bytes differ from the base.
// Skipping those would emit a delta that restores the wrong bytes, so sharing
// only counts when the extent's physical blocks are the base's own.
//
// Of the rest, only extents that actually hold data can differ. A region the
// base has data in but the target does not reads as zeros, so the delta has to
// clear it; a region that is unwritten or a hole on both sides reads as zeros in
// both and needs nothing. This matters on real images: a freshly allocated image
// is mostly unwritten extents, which XFS does not share with a reflink clone, so
// counting them as changed would read gigabytes of zeros on every capture.
func examineRanges(basePath, targetPath string, size int64) ([][2]int64, bool) {
	targetExtents, err := Extents(targetPath)
	// A nil slice means the filesystem cannot report extents. An empty non-nil
	// slice means a genuinely extent-free file, which is usable information.
	if err != nil || targetExtents == nil {
		return nil, false
	}
	baseExtents, err := Extents(basePath)
	if err != nil || baseExtents == nil {
		return nil, false
	}
	basePhysical := physicalRanges(baseExtents)
	var shared, candidates [][2]int64
	for _, e := range targetExtents {
		end := min(e.End, size)
		if e.Start >= end {
			continue
		}
		if e.Shared && overlapsPhysical(basePhysical, e.Physical, e.Physical+end-e.Start) {
			shared = append(shared, [2]int64{e.Start, end})
			continue
		}
		if e.Unwritten {
			// Allocated but zero; the base decides whether this differs.
			continue
		}
		candidates = append(candidates, [2]int64{e.Start, end})
	}
	for _, e := range baseExtents {
		start, end := e.Start, min(e.End, size)
		if start < end && !e.Unwritten {
			candidates = append(candidates, [2]int64{start, end})
		}
	}
	slices.SortFunc(candidates, func(a, b [2]int64) int { return cmp.Compare(a[0], b[0]) })
	slices.SortFunc(shared, func(a, b [2]int64) int { return cmp.Compare(a[0], b[0]) })
	return subtractRanges(mergeRanges(candidates), mergeRanges(shared)), true
}

// physicalRanges returns the base's allocated blocks in physical-offset order.
func physicalRanges(extents []Extent) [][2]int64 {
	out := make([][2]int64, 0, len(extents))
	for _, e := range extents {
		out = append(out, [2]int64{e.Physical, e.Physical + e.End - e.Start})
	}
	slices.SortFunc(out, func(a, b [2]int64) int { return cmp.Compare(a[0], b[0]) })
	return mergeRanges(out)
}

// overlapsPhysical reports whether [start,end) meets any of the sorted ranges.
func overlapsPhysical(ranges [][2]int64, start, end int64) bool {
	if start >= end {
		return false
	}
	i := sort.Search(len(ranges), func(i int) bool { return ranges[i][1] > start })
	return i < len(ranges) && ranges[i][0] < end
}

// mergeRanges sorts and coalesces overlapping or touching ranges.
func mergeRanges(in [][2]int64) [][2]int64 {
	if len(in) == 0 {
		return nil
	}
	slices.SortFunc(in, func(a, b [2]int64) int { return cmp.Compare(a[0], b[0]) })
	out := in[:1]
	for _, r := range in[1:] {
		last := &out[len(out)-1]
		if r[0] <= last[1] {
			last[1] = max(last[1], r[1])
			continue
		}
		out = append(out, r)
	}
	return out
}

// subtractRanges removes every part of cut from in. Both are sorted and merged.
func subtractRanges(in, cut [][2]int64) [][2]int64 {
	if len(cut) == 0 {
		return in
	}
	var out [][2]int64
	next := 0
	for _, r := range in {
		start := r[0]
		for next < len(cut) && cut[next][1] <= start {
			next++
		}
		for i := next; i < len(cut) && cut[i][0] < r[1]; i++ {
			if cut[i][0] > start {
				out = append(out, [2]int64{start, cut[i][0]})
			}
			start = max(start, cut[i][1])
			if start >= r[1] {
				break
			}
		}
		if start < r[1] {
			out = append(out, [2]int64{start, r[1]})
		}
	}
	return out
}

func writeDeltaHeader(w io.Writer, fp string, imageSize, blockSize int64) error {
	var hdr [deltaHeaderSize]byte
	copy(hdr[:8], deltaMagic)
	copy(hdr[8:72], fp)
	binary.LittleEndian.PutUint64(hdr[72:80], uint64(imageSize))
	binary.LittleEndian.PutUint64(hdr[80:88], uint64(blockSize))
	_, err := w.Write(hdr[:])
	return err
}

func writeRecord(w io.Writer, out string, offset int64, payload []byte) error {
	var hdr [recordHeaderSize]byte
	binary.LittleEndian.PutUint64(hdr[:8], uint64(offset))
	binary.LittleEndian.PutUint32(hdr[8:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("write record header at offset %d to %s: %w", offset, out, err)
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("write record payload at offset %d to %s: %w", offset, out, err)
	}
	return nil
}

// writeTrailer ends a delta with a zero-length record whose offset field is the record count.
func writeTrailer(w io.Writer, out string, records int64) error {
	var hdr [recordHeaderSize]byte
	binary.LittleEndian.PutUint64(hdr[:8], uint64(records))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("write trailer to %s: %w", out, err)
	}
	return nil
}

// readBlockAt reads at an explicit offset. Positional reads matter here: shared
// blocks are skipped without reading, so a sequential read would compare the
// wrong data and silently drop real changes from the delta. Bytes past the end
// of the file read as zeros, so a base shorter than a grown target compares right.
func readBlockAt(f *os.File, path string, offset int64, buf []byte) error {
	n, err := f.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return fmt.Errorf("read %d bytes at %d from %s: %w", len(buf), offset, path, err)
	}
	clear(buf[n:])
	return nil
}

// ApplyDelta reconstructs target from base plus delta.
func ApplyDelta(base, delta, target string) error {
	deltaFile, err := os.Open(delta)
	if err != nil {
		return fmt.Errorf("apply delta: open delta %s: %w", delta, err)
	}
	defer deltaFile.Close()
	meta, err := readDeltaHeader(deltaFile, delta)
	if err != nil {
		return fmt.Errorf("apply delta: %w", err)
	}
	// Wrong base means the delta would write foreign bytes silently: refuse
	// before creating or touching target.
	fp, err := Fingerprint(base)
	if err != nil {
		return fmt.Errorf("apply delta: %w", err)
	}
	if fp != meta.BaseFingerprint {
		return fmt.Errorf("apply delta %s: base mismatch: base %s fingerprints as %s, delta expects %s", delta, base, fp, meta.BaseFingerprint)
	}
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		if err := Provision(nil, base, target); err != nil {
			return fmt.Errorf("apply delta: copy base %s to target %s: %w", base, target, err)
		}
	} else if err != nil {
		return fmt.Errorf("apply delta: stat target %s: %w", target, err)
	}
	baseInfo, err := os.Stat(base)
	if err != nil {
		return fmt.Errorf("apply delta: stat base %s: %w", base, err)
	}
	if baseInfo.Size() > meta.ImageSize {
		return fmt.Errorf("apply delta %s: base %s is %d bytes, delta expects at most %d", delta, base, baseInfo.Size(), meta.ImageSize)
	}
	targetFile, err := os.OpenFile(target, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("apply delta: open target %s: %w", target, err)
	}
	info, err := targetFile.Stat()
	if err != nil {
		targetFile.Close()
		return fmt.Errorf("apply delta: stat target %s: %w", target, err)
	}
	// A copy of the base extends to the grown size the delta was captured at.
	if info.Size() == baseInfo.Size() && info.Size() < meta.ImageSize {
		if err := targetFile.Truncate(meta.ImageSize); err != nil {
			targetFile.Close()
			return fmt.Errorf("apply delta: grow target %s: %w", target, err)
		}
		info, err = targetFile.Stat()
		if err != nil {
			targetFile.Close()
			return fmt.Errorf("apply delta: stat target %s: %w", target, err)
		}
	}
	if info.Size() != meta.ImageSize {
		targetFile.Close()
		return fmt.Errorf("apply delta %s: target %s is %d bytes, delta expects %d", delta, target, info.Size(), meta.ImageSize)
	}
	if err := applyRecords(targetFile, deltaFile, meta, delta, target); err != nil {
		targetFile.Close()
		return err
	}
	if err := targetFile.Close(); err != nil {
		return fmt.Errorf("apply delta: close target %s: %w", target, err)
	}
	return nil
}

func applyRecords(target, deltaFile *os.File, meta DeltaMeta, deltaPath, targetPath string) error {
	payload := make([]byte, meta.BlockSize)
	var hdr [recordHeaderSize]byte
	for index := 0; ; index++ {
		if _, err := io.ReadFull(deltaFile, hdr[:]); err != nil {
			if err == io.EOF && !meta.Trailer {
				return nil
			}
			if err == io.EOF {
				return fmt.Errorf("delta %s is truncated: no trailer after %d records", deltaPath, index)
			}
			return fmt.Errorf("read record %d of delta %s: %w", index, deltaPath, err)
		}
		offset := binary.LittleEndian.Uint64(hdr[:8])
		length := uint64(binary.LittleEndian.Uint32(hdr[8:]))
		if length == 0 && meta.Trailer {
			if offset != uint64(index) {
				return fmt.Errorf("delta %s trailer counts %d records but %d were read", deltaPath, offset, index)
			}
			var extra [1]byte
			if n, _ := deltaFile.Read(extra[:]); n != 0 {
				return fmt.Errorf("delta %s has data after its trailer", deltaPath)
			}
			return nil
		}
		if length == 0 || length > uint64(meta.BlockSize) {
			return fmt.Errorf("record %d of delta %s has invalid length %d", index, deltaPath, length)
		}
		if offset > uint64(meta.ImageSize) || length > uint64(meta.ImageSize)-offset {
			return fmt.Errorf("record %d of delta %s covers [%d,%d) past image size %d", index, deltaPath, offset, offset+length, meta.ImageSize)
		}
		if _, err := io.ReadFull(deltaFile, payload[:length]); err != nil {
			return fmt.Errorf("read payload of record %d of delta %s: %w", index, deltaPath, err)
		}
		if _, err := target.WriteAt(payload[:length], int64(offset)); err != nil {
			return fmt.Errorf("write record %d at offset %d to target %s: %w", index, offset, targetPath, err)
		}
	}
}

func readDeltaHeader(r io.Reader, deltaPath string) (DeltaMeta, error) {
	var hdr [deltaHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return DeltaMeta{}, fmt.Errorf("read header of %s: %w", deltaPath, err)
	}
	magic := string(hdr[:8])
	if magic != deltaMagic && magic != deltaMagicV1 {
		return DeltaMeta{}, fmt.Errorf("%s is not a dock delta: magic is %q, want %q", deltaPath, magic, deltaMagic)
	}
	imageSize := binary.LittleEndian.Uint64(hdr[72:80])
	blockSize := binary.LittleEndian.Uint64(hdr[80:88])
	if imageSize > math.MaxInt64 || blockSize == 0 || blockSize > math.MaxInt64 {
		return DeltaMeta{}, fmt.Errorf("%s has invalid image size %d and block size %d", deltaPath, imageSize, blockSize)
	}
	return DeltaMeta{
		BaseFingerprint: string(hdr[8:72]),
		ImageSize:       int64(imageSize),
		BlockSize:       int64(blockSize),
		Trailer:         magic == deltaMagic,
	}, nil
}
