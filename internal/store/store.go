// Package store persists snapshot artifacts (memory files, state files,
// frozen rootfs generations, manifests) in an S3-compatible bucket.
// Single-host deployments read generations from local disk; the store
// carries them across hosts and survives host loss.
package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// FileEntry describes one stored artifact of a generation. Encoding is
// empty for legacy raw objects, "zstd" for compressed ones.
type FileEntry struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Encoding string `json:"encoding,omitempty"`
	// SHA256 is the hex digest of the stored bytes (compressed for zstd).
	// Empty for generations captured before digests were recorded.
	SHA256 string `json:"sha256,omitempty"`
	// RawSize is the decompressed size of a zstd entry; known when SHA256 is set.
	RawSize int64 `json:"rawSize,omitempty"`
	// DataBytes is what the decompressed entry takes when written sparse in SparseBlock
	// blocks, as a pull writes it. Set by PutFileCompressed; not part of the manifest.
	DataBytes int64 `json:"-"`
}

// EncodingZstd marks zstd-compressed objects.
const EncodingZstd = "zstd"

// Manifest is the portable description of one snapshot generation.
type Manifest struct {
	SnapshotID string      `json:"snapshotId"`
	DockID     string      `json:"dockId"`
	ParentID   *string     `json:"parentId"`
	Kind       string      `json:"kind"`
	CPUFamily  string      `json:"cpuFamily"`
	Files      []FileEntry `json:"files"`
	// BaseFingerprint identifies the base image a delta generation was built
	// against. Empty for whole-image generations. Restore refuses to apply a
	// delta to a different base rather than producing a corrupt filesystem.
	BaseFingerprint string `json:"baseFingerprint,omitempty"`
	// ImageSize is the logical size of the rootfs a delta reconstructs.
	ImageSize int64 `json:"imageSize,omitempty"`
	// SHA256 and SizeBytes describe the whole-image object (rootfs.ext4.zst):
	// the hex digest and byte length of the stored (compressed) bytes. Set by
	// full snapshots and control-plane imports; restore verifies them when
	// present and refuses a mismatch.
	SHA256    string `json:"sha256,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	// DataBytes is the space the flattened disk's data takes once decompressed and written sparse
	// (its non-zero 64 KiB blocks, not the virtual size). Set by full snapshots so a pull can size
	// its free-space check; absent on older generations and imports.
	DataBytes int64 `json:"dataBytes,omitempty"`
	// Image names the Dockyard image the disk derives from; empty when the
	// base was built on the host.
	Image string `json:"image,omitempty"`
	// Source is SourceImport when the control plane recorded a user-uploaded disk; empty otherwise.
	Source string `json:"source,omitempty"`
}

// SourceImport marks a generation whose disk a user uploaded (dock push).
const SourceImport = "import"

// Key prefixes inside the bucket.
func key(snapshotID, name string) string { return "snapshots/" + snapshotID + "/" + name }

// Store moves whole artifacts; small enough that streaming buys nothing.
type Store interface {
	Put(ctx context.Context, snapshotID, name string, r io.Reader, size int64) error
	Get(ctx context.Context, snapshotID, name string) (io.ReadCloser, error)
	Delete(ctx context.Context, snapshotID string) error
	List(ctx context.Context, snapshotID string) ([]FileEntry, error)
}

// S3Store is the production Store; integration against live S3/MinIO
// happens on the test host (task 6.2), unit coverage uses MemStore.
type S3Store struct {
	client *minio.Client
	bucket string
}

// DialS3 connects with static credentials to any S3-compatible endpoint.
func DialS3(endpoint, bucket, accessKey, secretKey string, useSSL bool) (*S3Store, error) {
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, err
	}
	return &S3Store{client: client, bucket: bucket}, nil
}

func (s *S3Store) Put(ctx context.Context, snapshotID, name string, r io.Reader, size int64) error {
	_, err := s.client.PutObject(ctx, s.bucket, key(snapshotID, name), r, size, minio.PutObjectOptions{})
	return err
}

func (s *S3Store) Get(ctx context.Context, snapshotID, name string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key(snapshotID, name), minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	return obj, nil
}

// Delete removes every object of one generation.
func (s *S3Store) Delete(ctx context.Context, snapshotID string) error {
	prefix := "snapshots/" + snapshotID + "/"
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return obj.Err
		}
		if err := s.client.RemoveObject(ctx, s.bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
			return err
		}
	}
	return nil
}

func (s *S3Store) List(ctx context.Context, snapshotID string) ([]FileEntry, error) {
	prefix := "snapshots/" + snapshotID + "/"
	var entries []FileEntry
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		entries = append(entries, FileEntry{Path: strings.TrimPrefix(obj.Key, prefix), Size: obj.Size})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

// MemStore is the in-memory Store for unit tests and local-only runs.
type MemStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{data: map[string][]byte{}}
}

func (m *MemStore) Put(_ context.Context, snapshotID, name string, r io.Reader, _ int64) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.data[key(snapshotID, name)] = raw
	m.mu.Unlock()
	return nil
}

func (m *MemStore) Get(_ context.Context, snapshotID, name string) (io.ReadCloser, error) {
	m.mu.Lock()
	raw, ok := m.data[key(snapshotID, name)]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no such object %s", key(snapshotID, name))
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func (m *MemStore) Delete(_ context.Context, snapshotID string) error {
	m.mu.Lock()
	for k := range m.data {
		if strings.HasPrefix(k, "snapshots/"+snapshotID+"/") {
			delete(m.data, k)
		}
	}
	m.mu.Unlock()
	return nil
}

func (m *MemStore) List(_ context.Context, snapshotID string) ([]FileEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := "snapshots/" + snapshotID + "/"
	var entries []FileEntry
	for k, v := range m.data {
		if strings.HasPrefix(k, prefix) {
			entries = append(entries, FileEntry{Path: strings.TrimPrefix(k, prefix), Size: int64(len(v))})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

// PutManifest stores a generation manifest as manifest.json.
func PutManifest(ctx context.Context, s Store, m Manifest) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.Put(ctx, m.SnapshotID, "manifest.json", bytes.NewReader(raw), int64(len(raw)))
}

// GetManifest reads a generation manifest back.
func GetManifest(ctx context.Context, s Store, snapshotID string) (Manifest, error) {
	var m Manifest
	rc, err := s.Get(ctx, snapshotID, "manifest.json")
	if err != nil {
		return m, err
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, err
	}
	return m, nil
}

// PutFileCompressed streams a zstd-compressed artifact under name+".zst".
func PutFileCompressed(ctx context.Context, s Store, snapshotID, name, localPath, tmpDir string) (FileEntry, error) {
	_ = tmpDir // streaming needs no scratch space
	src, err := os.Open(localPath)
	if err != nil {
		return FileEntry{}, err
	}
	pr, pw := io.Pipe()
	hash := sha256.New()
	count := &byteCounter{Reader: io.TeeReader(pr, hash)}
	var rawSize int64
	data := &dataCounter{}
	done := make(chan error, 1)
	go func() {
		zw, err := zstd.NewWriter(pw, zstd.WithEncoderLevel(zstd.SpeedFastest))
		if err != nil {
			src.Close()
			_ = pw.CloseWithError(err)
			done <- err
			return
		}
		n, copyErr := io.Copy(zw, io.TeeReader(src, data))
		rawSize = n
		src.Close()
		closeErr := zw.Close()
		if copyErr != nil {
			_ = pw.CloseWithError(copyErr)
			done <- copyErr
			return
		}
		if closeErr != nil {
			_ = pw.CloseWithError(closeErr)
			done <- closeErr
			return
		}
		_ = pw.Close()
		done <- nil
	}()
	stored := name + ".zst"
	if err := s.Put(ctx, snapshotID, stored, count, -1); err != nil {
		_ = pr.Close()
		<-done
		return FileEntry{}, err
	}
	if err := <-done; err != nil {
		return FileEntry{}, err
	}
	return FileEntry{Path: stored, Size: count.n, Encoding: EncodingZstd, SHA256: hex.EncodeToString(hash.Sum(nil)), RawSize: rawSize, DataBytes: data.total()}, nil
}

// SparseBlock is the granularity at which a pull skips all-zero data (internal/local copySparse).
const SparseBlock = 64 << 10

var zeroBlock [SparseBlock]byte

// dataCounter tallies the bytes of every SparseBlock-aligned block holding a non-zero byte. It
// reads the content, not the file's allocation, which compression, deduplication or delayed
// allocation on the host filesystem would understate.
type dataCounter struct {
	off   int64
	dirty bool
	n     int64
}

func (c *dataCounter) Write(p []byte) (int, error) {
	written := len(p)
	for len(p) > 0 {
		chunk := p[:min(int64(len(p)), SparseBlock-c.off%SparseBlock)]
		if !c.dirty && !bytes.Equal(chunk, zeroBlock[:len(chunk)]) {
			c.dirty = true
		}
		c.off += int64(len(chunk))
		if c.off%SparseBlock == 0 {
			if c.dirty {
				c.n += SparseBlock
			}
			c.dirty = false
		}
		p = p[len(chunk):]
	}
	return written, nil
}

// total includes a trailing partial block, which a pull writes at its own length.
func (c *dataCounter) total() int64 {
	if c.dirty {
		return c.n + c.off%SparseBlock
	}
	return c.n
}

// byteCounter tallies compressed bytes as the store drains the pipe.
type byteCounter struct {
	io.Reader
	n int64
}

func (c *byteCounter) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.n += int64(n)
	return n, err
}

// flights serializes downloads per destination so concurrent loads share one result.
var flights = &flightGroup{calls: map[string]*flightCall{}}

type flightCall struct {
	done chan struct{}
	err  error
}

type flightGroup struct {
	mu    sync.Mutex
	calls map[string]*flightCall
}

// do runs fn once per key at a time; callers arriving meanwhile wait and share its error.
func (g *flightGroup) do(ctx context.Context, key string, fn func() error) error {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		select {
		case <-c.done:
			return c.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c := &flightCall{done: make(chan struct{}), err: errors.New("download aborted")}
	g.calls[key] = c
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.calls, key)
		g.mu.Unlock()
		close(c.done)
	}()
	c.err = fn()
	return c.err
}

// expectedLocalSize is the size the finished local file must have, if known.
func expectedLocalSize(entry FileEntry) (int64, bool) {
	if entry.Encoding != EncodingZstd {
		return entry.Size, true
	}
	if entry.RawSize > 0 || entry.SHA256 != "" {
		return entry.RawSize, true
	}
	return 0, false
}

// verifiedSuffix names the sidecar recording that a download passed verification.
const verifiedSuffix = ".verified"

// verifiedRecord is "<localSize> <storedSize> <sha256>" for a completed download.
func verifiedRecord(localSize int64, entry FileEntry) string {
	return fmt.Sprintf("%d %d %s\n", localSize, entry.Size, entry.SHA256)
}

// trustedLocal reports whether localPath is a complete copy of entry: its size matches
// the manifest, or it matches the sidecar written after a verified download.
func trustedLocal(entry FileEntry, localPath string) bool {
	info, err := os.Stat(localPath)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if want, ok := expectedLocalSize(entry); ok {
		return info.Size() == want
	}
	raw, err := os.ReadFile(localPath + verifiedSuffix)
	if err != nil {
		return false
	}
	return string(raw) == verifiedRecord(info.Size(), entry)
}

// EnsureFile makes localPath hold a verified copy of entry, downloading it at most once at a time.
// A copy that cannot be trusted is replaced by an atomic rename, so the path never disappears.
func EnsureFile(ctx context.Context, s Store, snapshotID string, entry FileEntry, localPath string) error {
	return flights.do(ctx, localPath, func() error {
		if trustedLocal(entry, localPath) {
			return nil
		}
		// Any old sidecar describes the file about to be replaced.
		_ = os.Remove(localPath + verifiedSuffix)
		if err := GetFileAuto(ctx, s, snapshotID, entry, localPath); err != nil {
			return err
		}
		info, err := os.Stat(localPath)
		if err != nil {
			return err
		}
		return writeFileAtomic(localPath+verifiedSuffix, []byte(verifiedRecord(info.Size(), entry)))
	})
}

// writeFileAtomic writes data to path through a temp file and rename.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	ok = true
	return nil
}

// syncDir flushes a directory entry; best effort because some platforms cannot open directories.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// GetFileAuto downloads one manifest entry, transparently decompressing.
// The bytes land in a unique temp file that is verified against the manifest
// (stored size, and sha256 when recorded), fsynced, and renamed into place, so
// a partial download is never visible at localPath.
func GetFileAuto(ctx context.Context, s Store, snapshotID string, entry FileEntry, localPath string) error {
	return GetFileAutoLimit(ctx, s, snapshotID, entry, localPath, 0)
}

// ErrTooLarge marks an object that unpacks past the caller's limit.
var ErrTooLarge = errors.New("object unpacks past its limit")

// GetFileAutoLimit is GetFileAuto refusing an object that unpacks to more than maxRaw bytes
// (0 means no limit), so a small upload cannot expand into a disk-filling file.
func GetFileAutoLimit(ctx context.Context, s Store, snapshotID string, entry FileEntry, localPath string, maxRaw int64) (err error) {
	rc, err := s.Get(ctx, snapshotID, entry.Path)
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(localPath), filepath.Base(localPath)+".tmp-")
	if err != nil {
		return err
	}
	tmp := out.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = out.Close()
			_ = os.Remove(tmp)
		}
	}()
	hash := sha256.New()
	// Never read more than one byte past the declared stored size: enough to see the mismatch.
	var body io.Reader = rc
	if entry.Size > 0 {
		body = io.LimitReader(rc, entry.Size+1)
	}
	stored := &byteCounter{Reader: io.TeeReader(body, hash)}
	var src io.Reader = stored
	var dec *zstd.Decoder
	if entry.Encoding == EncodingZstd {
		if dec, err = zstd.NewReader(stored, zstd.WithDecoderConcurrency(1)); err != nil {
			return err
		}
		src = dec
	}
	if maxRaw > 0 {
		src = io.LimitReader(src, maxRaw+1)
	}
	var written int64
	written, err = io.Copy(out, src)
	if dec != nil {
		dec.Close()
	}
	if err != nil {
		return err
	}
	if maxRaw > 0 && written > maxRaw {
		return fmt.Errorf("download %s/%s: %w (%d bytes)", snapshotID, entry.Path, ErrTooLarge, maxRaw)
	}
	// Drain so the stored-byte count and digest cover the whole object.
	if _, err = io.Copy(io.Discard, stored); err != nil {
		return err
	}
	if stored.n != entry.Size {
		return fmt.Errorf("download %s/%s: got %d stored bytes, manifest says %d", snapshotID, entry.Path, stored.n, entry.Size)
	}
	if entry.SHA256 != "" {
		if got := hex.EncodeToString(hash.Sum(nil)); got != entry.SHA256 {
			return fmt.Errorf("download %s/%s: sha256 %s does not match manifest %s", snapshotID, entry.Path, got, entry.SHA256)
		}
	}
	if err = out.Sync(); err != nil {
		return err
	}
	if err = out.Chmod(0o644); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, localPath); err != nil {
		return err
	}
	renamed = true
	syncDir(filepath.Dir(localPath))
	return nil
}

// PutFile uploads one local artifact file.
func PutFile(ctx context.Context, s Store, snapshotID, name, localPath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return s.Put(ctx, snapshotID, name, f, info.Size())
}

// GetFile downloads one artifact to a local path.
func GetFile(ctx context.Context, s Store, snapshotID, name, localPath string) error {
	rc, err := s.Get(ctx, snapshotID, name)
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	out, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, rc)
	return err
}
