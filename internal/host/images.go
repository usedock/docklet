package host

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"

	apiv1 "github.com/usedock/docklet/api/v1"
	"github.com/usedock/docklet/internal/disk"
	"github.com/usedock/docklet/internal/dockyard"
)

// Dockyard images on a fleet host. A pulled image is a raw ext4 base installed
// content-addressed like the built base, at <images>/bases/<sha256>/base.ext4,
// and named by the symlink <images>/images/<name>/base.ext4 pointing at it, so
// a dock's disk labels, delta generations and the heartbeat base list all work
// unchanged. Firecracker keeps the HOST kernel (vmlinux and initrd.img staged by
// install-kernel.sh): the image's vmlinuz and initrd.img are never downloaded,
// because the host kernel is what the jailer and snapshot compatibility rely on.

const (
	imagesDir = "images"
	// maxIndexBytes bounds the dockyard.json download.
	maxIndexBytes = 4 << 20
	// maxRootfsBytes bounds the decompressed rootfs: a hostile or corrupt
	// archive cannot fill the disk.
	maxRootfsBytes = 32 << 30
)

// imageMeta is the record beside an installed image, shown by `image ls`.
type imageMeta struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Arch        string `json:"arch"`
	Fingerprint string `json:"fingerprint"`
}

// InstalledImage is one pulled Dockyard image.
type InstalledImage struct {
	Name        string
	Version     string
	Fingerprint string
}

// servedDir is the image dir ResolveImage reads; set once by the daemon.
var servedDir atomic.Pointer[string]

// SetImageDir tells ResolveImage where pulled images live (the image dir that
// holds base.ext4). An empty dir serves only the built base.
func SetImageDir(dir string) {
	if dir == "" {
		servedDir.Store(nil)
		return
	}
	servedDir.Store(&dir)
}

func imagePointer(dir, name string) string {
	return filepath.Join(dir, imagesDir, name, "base.ext4")
}

// installedImage reports whether name is a pulled image under dir.
func installedImage(dir, name string) bool {
	if !dockyard.ValidName(name) || name == apiv1.DefaultImage {
		return false
	}
	info, err := os.Stat(imagePointer(dir, name))
	return err == nil && info.Mode().IsRegular()
}

// basePointerFor is the base new docks of image clone: the current built base
// for the default image, the pulled image's own base otherwise.
func basePointerFor(imageBase, image string) string {
	if image == "" || image == apiv1.DefaultImage {
		return imageBase
	}
	return imagePointer(filepath.Dir(imageBase), image)
}

// ListInstalled returns the pulled images under dir, sorted by name.
func ListInstalled(dir string) []InstalledImage {
	entries, _ := os.ReadDir(filepath.Join(dir, imagesDir))
	var out []InstalledImage
	for _, e := range entries {
		if !e.IsDir() || !installedImage(dir, e.Name()) {
			continue
		}
		img := InstalledImage{Name: e.Name()}
		if raw, err := os.ReadFile(filepath.Join(dir, imagesDir, e.Name(), "image.json")); err == nil {
			var m imageMeta
			if json.Unmarshal(raw, &m) == nil {
				img.Version, img.Fingerprint = m.Version, m.Fingerprint
			}
		}
		out = append(out, img)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ServedImages names every image a host serves: the built base when present
// plus each pulled image. This is the heartbeat `images` list.
func ServedImages(dir string) []string {
	served := []string{}
	if _, err := os.Stat(filepath.Join(dir, "base.ext4")); err == nil {
		served = append(served, apiv1.DefaultImage)
	}
	for _, img := range ListInstalled(dir) {
		served = append(served, img.Name)
	}
	sort.Strings(served)
	return served
}

// ErrImageNotInCatalog marks a pull of a name the verified index lacks.
var ErrImageNotInCatalog = errors.New("image not in the Dockyard catalog")

// Puller fetches Dockyard images into an image dir.
type Puller struct {
	// Dir is the image dir (holds base.ext4 and bases/).
	Dir string
	// IndexURL and IndexSHA256 pin the release; the index is refused unless it
	// hashes to IndexSHA256.
	IndexURL    string
	IndexSHA256 string
	// Arch defaults to the host architecture.
	Arch   string
	Client *http.Client
	// MaxRootfsBytes caps the decompressed rootfs; 0 means maxRootfsBytes.
	MaxRootfsBytes int64
}

// NewPuller pins the release this binary trusts.
func NewPuller(dir string) *Puller {
	return &Puller{
		Dir:         dir,
		IndexURL:    dockyard.DockyardIndexURL,
		IndexSHA256: dockyard.DockyardIndexSHA256,
		Client:      &http.Client{Timeout: 30 * time.Minute},
	}
}

// Pull installs the named catalog image. The index must match the pin, the
// rootfs archive must match the index's size and SHA-256, and the decompressed
// image must stay under the cap; anything else installs nothing.
func (p *Puller) Pull(ctx context.Context, name string) (InstalledImage, error) {
	if !dockyard.ValidName(name) {
		return InstalledImage{}, fmt.Errorf("invalid image name %q", name)
	}
	if len(p.IndexSHA256) != 64 {
		return InstalledImage{}, errors.New("no pinned index checksum, refusing to trust the index")
	}
	raw, err := p.fetchBytes(ctx, p.IndexURL, maxIndexBytes)
	if err != nil {
		return InstalledImage{}, fmt.Errorf("fetch index: %w", err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != strings.ToLower(p.IndexSHA256) {
		return InstalledImage{}, errors.New("dockyard index does not match the pinned sha256, refusing it")
	}
	ix, err := dockyard.ParseIndex(raw)
	if err != nil {
		return InstalledImage{}, err
	}
	arch := p.Arch
	if arch == "" {
		arch = runtime.GOARCH
	}
	img, ok := ix.Find(name, arch)
	if !ok {
		return InstalledImage{}, fmt.Errorf("%w: %q for %s", ErrImageNotInCatalog, name, arch)
	}
	rootfs := img.Files[dockyard.FileRootfs]

	bases := filepath.Join(p.Dir, disk.BasesDir)
	if err := os.MkdirAll(bases, 0o755); err != nil {
		return InstalledImage{}, err
	}
	removeStalePulls(bases)
	// One pull per image name at a time; the lock sits in images/, where
	// ListInstalled skips non-directories.
	if err := os.MkdirAll(filepath.Join(p.Dir, imagesDir), 0o755); err != nil {
		return InstalledImage{}, err
	}
	unlock, err := lockFile(filepath.Join(p.Dir, imagesDir, ".lock-"+name))
	if err != nil {
		return InstalledImage{}, fmt.Errorf("lock %s: %w", name, err)
	}
	defer unlock()
	archive, err := os.CreateTemp(bases, ".pull-*.zst")
	if err != nil {
		return InstalledImage{}, err
	}
	defer func() { _ = os.Remove(archive.Name()) }()
	if err := p.download(ctx, rootfs, archive); err != nil {
		_ = archive.Close()
		return InstalledImage{}, err
	}
	if err := archive.Close(); err != nil {
		return InstalledImage{}, err
	}
	if err := dockyard.VerifyFile(archive.Name(), rootfs); err != nil {
		return InstalledImage{}, err
	}
	fp, tmp, err := p.decompress(archive.Name(), bases)
	if err != nil {
		return InstalledImage{}, err
	}
	defer func() { _ = os.Remove(tmp) }() // gone after a successful rename
	if err := p.install(name, fp, tmp, ix.Version, arch); err != nil {
		return InstalledImage{}, err
	}
	return InstalledImage{Name: name, Version: ix.Version, Fingerprint: fp}, nil
}

// stalePullAge is how old a bases/.pull-* temp file must be before a new pull
// treats it as the leftover of a crashed pull and removes it.
const stalePullAge = time.Hour

// removeStalePulls deletes crashed pulls' temp files (the ext4 can be 32 GiB
// sparse). Younger files may belong to a pull of another name still running.
func removeStalePulls(bases string) {
	entries, _ := os.ReadDir(bases)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), ".pull-") {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > stalePullAge {
			_ = os.Remove(filepath.Join(bases, e.Name()))
		}
	}
}

func (p *Puller) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return http.DefaultClient
}

func (p *Puller) fetchBytes(ctx context.Context, url string, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	if err := p.get(ctx, url, limit, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// download streams the archive, never reading past the size the index states.
func (p *Puller) download(ctx context.Context, f dockyard.File, dst io.Writer) error {
	return p.get(ctx, f.URL, f.Size, dst)
}

func (p *Puller) get(ctx context.Context, url string, limit int64, dst io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	n, err := io.Copy(dst, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("GET %s: more than %d bytes", url, limit)
	}
	return nil
}

// decompress unpacks the verified archive into a temp file beside the bases,
// sparse, hashing as it writes. It returns the sha256 of the raw image (the
// base fingerprint) and the temp path.
func (p *Puller) decompress(archive, bases string) (fp, tmp string, err error) {
	in, err := os.Open(archive)
	if err != nil {
		return "", "", err
	}
	defer in.Close()
	dec, err := zstd.NewReader(in)
	if err != nil {
		return "", "", err
	}
	defer dec.Close()
	out, err := os.CreateTemp(bases, ".pull-*.ext4")
	if err != nil {
		return "", "", err
	}
	fail := func(err error) (string, string, error) {
		_ = out.Close()
		_ = os.Remove(out.Name())
		return "", "", err
	}
	limit := p.MaxRootfsBytes
	if limit <= 0 {
		limit = maxRootfsBytes
	}
	h := sha256.New()
	size, err := copySparse(out, io.TeeReader(io.LimitReader(dec, limit+1), h))
	if err != nil {
		return fail(fmt.Errorf("decompress rootfs: %w", err))
	}
	if size > limit {
		return fail(fmt.Errorf("rootfs expands past the %d byte cap", limit))
	}
	if err := out.Truncate(size); err != nil {
		return fail(err)
	}
	if err := out.Sync(); err != nil {
		return fail(err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(out.Name())
		return "", "", err
	}
	return hex.EncodeToString(h.Sum(nil)), out.Name(), nil
}

// copySparse writes src to dst, seeking over all-zero blocks so an ext4 image
// stays sparse on disk. It returns the logical size.
func copySparse(dst *os.File, src io.Reader) (int64, error) {
	const block = 64 << 10
	buf := make([]byte, block)
	zero := make([]byte, block)
	var size int64
	for {
		n, err := io.ReadFull(src, buf)
		if n > 0 {
			if bytes.Equal(buf[:n], zero[:n]) {
				if _, serr := dst.Seek(int64(n), io.SeekCurrent); serr != nil {
					return size, serr
				}
			} else if _, werr := dst.Write(buf[:n]); werr != nil {
				return size, werr
			}
			size += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return size, nil
		}
		if err != nil {
			return size, err
		}
	}
}

// install moves the temp image to bases/<fp>/base.ext4 (kept if already held)
// and atomically points images/<name>/base.ext4 at it.
func (p *Puller) install(name, fp, tmp, version, arch string) error {
	baseDir := filepath.Join(p.Dir, disk.BasesDir, fp)
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return err
	}
	final := filepath.Join(baseDir, "base.ext4")
	if _, err := os.Stat(final); err == nil {
		_ = os.Remove(tmp)
	} else if err := os.Rename(tmp, final); err != nil {
		return err
	}
	// World-readable like the built base: the jailer copies from it.
	if err := os.Chmod(final, 0o644); err != nil {
		return err
	}
	if err := disk.RecordBaseSource(filepath.Join(p.Dir, "base.ext4"), fp, name, version); err != nil {
		return err
	}
	dir := filepath.Join(p.Dir, imagesDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	meta, err := json.Marshal(imageMeta{Name: name, Version: version, Arch: arch, Fingerprint: fp})
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, "image.json"), meta); err != nil {
		return err
	}
	if name == apiv1.DefaultImage {
		// The default image is the host's current base: <images>/base.ext4 itself, not a named pointer.
		return p.makeCurrent(fp)
	}
	next := filepath.Join(dir, ".base.ext4.next-"+randSuffix())
	if err := os.Symlink(filepath.Join("..", "..", disk.BasesDir, fp, "base.ext4"), next); err != nil {
		return err
	}
	if err := os.Rename(next, filepath.Join(dir, "base.ext4")); err != nil {
		_ = os.Remove(next)
		return err
	}
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0o644)
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}
	return werr
}

// randSuffix is a unique temp-name suffix.
func randSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// makeCurrent points <images>/base.ext4 at the held base fp. A legacy plain
// base.ext4 is first kept under bases/<its fingerprint> (hard link, so the
// pointer never disappears) because delta generations name it.
func (p *Puller) makeCurrent(fp string) error {
	pointer := filepath.Join(p.Dir, "base.ext4")
	if info, err := os.Lstat(pointer); err == nil && info.Mode().IsRegular() {
		old, err := disk.Fingerprint(pointer)
		if err != nil {
			return err
		}
		if _, err := os.Stat(disk.BasePath(pointer, old)); err != nil {
			held := disk.BasePath(pointer, old)
			if err := os.MkdirAll(filepath.Dir(held), 0o755); err != nil {
				return err
			}
			if err := os.Link(pointer, held); err != nil && !os.IsExist(err) {
				return fmt.Errorf("keep the existing base: %w", err)
			}
		}
	}
	return disk.SetCurrentBase(pointer, fp)
}
