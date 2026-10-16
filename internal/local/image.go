package local

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/usedock/docklet/internal/dockyard"
)

// DefaultImage is used when a dock names no image.
const DefaultImage = "ubuntu-24.04"

// Image sizes sum the download size of an image.
func ImageSize(im dockyard.Image) int64 {
	var n int64
	for _, f := range im.Files {
		n += f.Size
	}
	return n
}

var imageNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}$`)

// findImage returns the image for name and arch, or an error listing what exists.
func findImage(ix *dockyard.Index, name, arch string) (dockyard.Image, error) {
	if im, ok := ix.Find(name, arch); ok {
		// The name and arch become a cache directory name; an unpinned index must not escape it.
		if !imageNameRE.MatchString(im.Name) || (im.Arch != "amd64" && im.Arch != "arm64") {
			return dockyard.Image{}, errf("bad_index", "image name %q or arch %q is not allowed", im.Name, im.Arch)
		}
		for _, need := range []string{dockyard.FileRootfs, dockyard.FileKernel, dockyard.FileInitrd} {
			if f, ok := im.Files[need]; !ok || f.URL == "" {
				return dockyard.Image{}, errf("bad_index", "image %s/%s lacks a %s entry", name, arch, need)
			}
		}
		return *im, nil
	}
	var have []string
	for _, im := range ix.Images {
		if im.Arch == arch {
			have = append(have, im.Name)
		}
	}
	sort.Strings(have)
	return dockyard.Image{}, errf("image_not_found", "no image %q for %s in Dockyard (available: %s)", name, arch, strings.Join(have, ", "))
}

// Catalog supplies the image index.
type Catalog interface {
	Index(ctx context.Context) (*dockyard.Index, error)
}

// HTTPCatalog fetches the index over HTTP(S) and verifies it against SHA256 before parsing.
type HTTPCatalog struct {
	URL    string
	SHA256 string // expected sha256 of the index; empty means unverified
	Client *http.Client
	// Unpinned is set when the URL comes from DOCK_DOCKYARD_URL instead of the pinned release.
	Unpinned bool
}

// NewCatalog returns the catalog for this build. The default is the pinned release; with
// DOCK_DOCKYARD_URL the index is unpinned (a warning is written to warn) and is verified
// only if DOCK_DOCKYARD_SHA256 is set.
func NewCatalog(getenv func(string) string, warn io.Writer) *HTTPCatalog {
	if u := getenv("DOCK_DOCKYARD_URL"); u != "" {
		c := &HTTPCatalog{URL: u, SHA256: strings.ToLower(getenv("DOCK_DOCKYARD_SHA256")), Unpinned: true}
		if warn != nil {
			if c.SHA256 == "" {
				fmt.Fprintln(warn, "warning: DOCK_DOCKYARD_URL is set: the Dockyard index is unpinned and unverified (set DOCK_DOCKYARD_SHA256 to verify it)")
			} else {
				fmt.Fprintln(warn, "warning: DOCK_DOCKYARD_URL is set: the Dockyard index is not the release pinned in this dock build")
			}
		}
		return c
	}
	return &HTTPCatalog{URL: dockyard.DockyardIndexURL, SHA256: dockyard.DockyardIndexSHA256}
}

func (c *HTTPCatalog) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Index downloads the index, checks its sha256 and parses it.
func (c *HTTPCatalog) Index(ctx context.Context) (*dockyard.Index, error) {
	if c.URL == "" {
		return nil, errf("no_dockyard_release", "no Dockyard release pinned yet; set DOCK_DOCKYARD_URL (and DOCK_DOCKYARD_SHA256) to use another index")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return nil, errf("index_unavailable", "%v", err)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, errf("index_unavailable", "cannot reach Dockyard at %s: %v", c.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errf("index_unavailable", "Dockyard index %s answered %s", c.URL, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, errf("index_unavailable", "%v", err)
	}
	if c.SHA256 != "" {
		sum := sha256.Sum256(raw)
		if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, c.SHA256) {
			return nil, errf("index_mismatch", "the Dockyard index at %s has sha256 %s, expected %s; refusing to use it", c.URL, got, c.SHA256)
		}
	}
	ix, err := dockyard.ParseIndex(raw)
	if err != nil {
		return nil, errf("bad_index", "%v", err)
	}
	return ix, nil
}

// CachedImage holds the paths of a ready image.
type CachedImage struct {
	Dir, Kernel, Initrd, Base string
}

// Cache stores verified images under Dir and converts them once to a qcow2 base.
type Cache struct {
	Dir    string
	Driver Driver
	HTTP   *http.Client
	// Progress receives human progress lines; nil is silent.
	Progress io.Writer
	// MaxRawBytes caps the decompressed rootfs so a bad index cannot fill the disk; zero means 32 GiB.
	MaxRawBytes int64
}

// DefaultMaxRawBytes is the decompressed rootfs cap when Cache.MaxRawBytes is zero.
const DefaultMaxRawBytes = 32 << 30

// staleDownload is how old a .part file must be before it is considered abandoned.
const staleDownload = time.Hour

// cleanStale removes abandoned partial downloads and conversions under the cache dir.
func (c *Cache) cleanStale() {
	dirs, _ := os.ReadDir(c.Dir)
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(c.Dir, d.Name()))
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".part") {
				continue
			}
			if info, err := f.Info(); err == nil && time.Since(info.ModTime()) > staleDownload {
				_ = os.Remove(filepath.Join(c.Dir, d.Name(), f.Name()))
			}
		}
	}
}

func (c *Cache) logf(format string, a ...any) {
	if c.Progress != nil {
		fmt.Fprintf(c.Progress, format+"\n", a...)
	}
}

func (c *Cache) imageDir(im dockyard.Image) string {
	sum := im.Files[dockyard.FileRootfs].SHA256
	if len(sum) > 12 {
		sum = sum[:12]
	}
	return filepath.Join(c.Dir, im.Name+"-"+im.Arch+"-"+sum)
}

func (c *Cache) paths(im dockyard.Image) CachedImage {
	d := c.imageDir(im)
	return CachedImage{Dir: d, Kernel: filepath.Join(d, dockyard.FileKernel), Initrd: filepath.Join(d, dockyard.FileInitrd), Base: filepath.Join(d, "base.qcow2")}
}

// Cached reports whether im is fully downloaded, verified and converted.
func (c *Cache) Cached(im dockyard.Image) bool {
	p := c.paths(im)
	_, err := os.Stat(filepath.Join(p.Dir, ".ready"))
	return err == nil
}

// Ensure makes im available locally, downloading and converting on first use.
func (c *Cache) Ensure(ctx context.Context, im dockyard.Image) (CachedImage, error) {
	p := c.paths(im)
	if c.Cached(im) {
		return p, nil
	}
	c.cleanStale()
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return p, errf("cache_error", "%v", err)
	}
	for _, name := range []string{dockyard.FileKernel, dockyard.FileInitrd, dockyard.FileRootfs} {
		dest := filepath.Join(p.Dir, name)
		if st, err := os.Stat(dest); err == nil && st.Size() == im.Files[name].Size {
			if err := verifyFile(dest, im.Files[name]); err == nil {
				continue
			}
		}
		c.logf("downloading %s (%s)", name, humanBytes(im.Files[name].Size))
		if err := c.download(ctx, im.Files[name], dest); err != nil {
			// Another dock command may have finished the same file or the whole image.
			if c.Cached(im) {
				return p, nil
			}
			if verifyFile(dest, im.Files[name]) == nil {
				continue
			}
			return p, err
		}
	}
	// Temp names are per call: two dock commands may prepare the same image at once.
	c.logf("preparing %s disk", im.Name)
	tag := randomHex(4)
	raw := filepath.Join(p.Dir, "rootfs."+tag+".raw")
	defer os.Remove(raw)
	tmpBase := p.Base + "." + tag + ".part"
	defer os.Remove(tmpBase)
	if err := decompress(ctx, filepath.Join(p.Dir, dockyard.FileRootfs), raw, c.maxRaw()); err != nil {
		if c.Cached(im) {
			return p, nil
		}
		return p, err
	}
	if err := c.Driver.ConvertRaw(ctx, raw, tmpBase); err != nil {
		return p, err
	}
	if err := os.Rename(tmpBase, p.Base); err != nil {
		// On Windows a base already in use by a dock cannot be replaced; it is the same image.
		if c.Cached(im) {
			return p, nil
		}
		return p, errf("cache_error", "%v", err)
	}
	if err := os.WriteFile(filepath.Join(p.Dir, ".ready"), nil, 0o600); err != nil {
		return p, errf("cache_error", "%v", err)
	}
	// The compressed rootfs is no longer needed once the base exists.
	_ = os.Remove(filepath.Join(p.Dir, dockyard.FileRootfs))
	return p, nil
}

// EnsureRootfs makes the verified compressed rootfs (rootfs.ext4.zst) of im available and returns
// its path. It never converts the disk, so it needs no qemu-img, and it keeps the file for later
// calls. Ensure removes the file after it builds the qcow2 base, so a converted image downloads
// the rootfs again here.
func (c *Cache) EnsureRootfs(ctx context.Context, im dockyard.Image) (string, error) {
	p := c.paths(im)
	dest := filepath.Join(p.Dir, dockyard.FileRootfs)
	want := im.Files[dockyard.FileRootfs]
	if st, err := os.Stat(dest); err == nil && st.Size() == want.Size && verifyFile(dest, want) == nil {
		return dest, nil
	}
	c.cleanStale()
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return "", errf("cache_error", "%v", err)
	}
	c.logf("downloading %s (%s)", dockyard.FileRootfs, humanBytes(want.Size))
	if err := c.download(ctx, want, dest); err != nil {
		// Another dock command may have finished the same file.
		if verifyFile(dest, want) == nil {
			return dest, nil
		}
		return "", err
	}
	return dest, nil
}

// verifyFile checks a file against its index entry (size, then sha256).
func verifyFile(path string, want dockyard.File) error {
	if err := dockyard.VerifyFile(path, want); err != nil {
		return errf("verify_failed", "%v", err)
	}
	return nil
}

// download fetches f to dest through a .part file and keeps it only if it verifies.
func (c *Cache) download(ctx context.Context, f dockyard.File, dest string) error {
	if !strings.HasPrefix(f.URL, "https://") && !strings.HasPrefix(f.URL, "http://127.0.0.1") && !strings.HasPrefix(f.URL, "http://localhost") {
		return errf("insecure_url", "refusing to download %s: only https URLs are allowed", f.URL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return errf("download_failed", "%v", err)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return errf("download_failed", "%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errf("download_failed", "%s answered %s", f.URL, resp.Status)
	}
	part := dest + "." + randomHex(4) + ".part"
	out, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return errf("cache_error", "%v", err)
	}
	limit := f.Size
	if limit <= 0 {
		limit = 64 << 30
	}
	// One byte past the expected size proves the body is longer than the index says.
	_, err = io.Copy(out, io.LimitReader(resp.Body, limit+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(part)
		return errf("download_failed", "%v", err)
	}
	if err := verifyFile(part, f); err != nil {
		_ = os.Remove(part)
		return err
	}
	if err := os.Rename(part, dest); err != nil {
		return errf("cache_error", "%v", err)
	}
	return nil
}

func (c *Cache) maxRaw() int64 {
	if c.MaxRawBytes > 0 {
		return c.MaxRawBytes
	}
	return DefaultMaxRawBytes
}

func decompress(ctx context.Context, src, dest string, max int64) error {
	in, err := os.Open(src)
	if err != nil {
		return errf("cache_error", "%v", err)
	}
	defer in.Close()
	zr, err := zstd.NewReader(in)
	if err != nil {
		return errf("decompress_failed", "%v", err)
	}
	defer zr.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return errf("cache_error", "%v", err)
	}
	// Zero runs are skipped, not written, so a mostly-empty disk takes only its data.
	_ = markSparse(out)
	// One byte past the cap proves the stream is larger than allowed.
	n, err := copySparse(out, io.LimitReader(ctxReader{ctx, zr}, max+1))
	if err == nil && n > max {
		err = fmt.Errorf("the decompressed disk is larger than the %s limit", humanBytes(max))
	}
	if err != nil {
		out.Close()
		_ = os.Remove(dest)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errf("decompress_failed", "%v", err)
	}
	return out.Close()
}

// sparseBlock is the granularity at which all-zero input is skipped instead of written.
const sparseBlock = 64 << 10

// copySparse copies r to f (an empty file at offset 0) and returns the bytes copied. Blocks
// of zeros are seeked over so the file stays sparse; its size is set at the end.
func copySparse(f *os.File, r io.Reader) (int64, error) {
	buf := make([]byte, sparseBlock)
	var total int64
	holeEnd := false
	for {
		n, err := readBlock(r, buf)
		if n > 0 {
			if isZero(buf[:n]) {
				if _, serr := f.Seek(int64(n), io.SeekCurrent); serr != nil {
					return total, serr
				}
				holeEnd = true
			} else {
				if _, werr := f.Write(buf[:n]); werr != nil {
					return total, werr
				}
				holeEnd = false
			}
			total += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return total, err
		}
	}
	if holeEnd {
		if err := f.Truncate(total); err != nil {
			return total, err
		}
	}
	return total, nil
}

// readBlock fills buf and returns r's own error: io.ReadFull would turn a truncated stream's
// io.ErrUnexpectedEOF into what looks like a clean short final block.
func readBlock(r io.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// Arch is the image architecture for this machine.
func Arch() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "amd64"
}

func humanBytes(n int64) string {
	const mib = 1 << 20
	if n >= 1<<30 {
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%d MiB", (n+mib-1)/mib)
}

// HumanBytes renders a size for tables.
func HumanBytes(n int64) string { return humanBytes(n) }
