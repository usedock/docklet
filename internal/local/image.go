package local

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// DockyardTag pins the Dockyard release the CLI reads its index from. DOCK_DOCKYARD_URL
// overrides the full index URL.
const DockyardTag = "dockyard-v1"

// DockyardIndexURL is the default index location for this CLI build.
const DockyardIndexURL = "https://github.com/usedock/docklet/releases/download/" + DockyardTag + "/dockyard.json"

// DefaultImage is used when a dock names no image.
const DefaultImage = "ubuntu-24.04"

// File names every Dockyard image provides.
const (
	FileRootfs = "rootfs.ext4.zst"
	FileKernel = "vmlinuz"
	FileInitrd = "initrd.img"
)

// ImageFile is one downloadable artifact of an image.
type ImageFile struct {
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Image is one catalog entry. This mirrors the Dockyard index entry; it is a local copy
// of the shape so the engine does not depend on the index builder.
type Image struct {
	Name        string               `json:"name"`
	Arch        string               `json:"arch"`
	Description string               `json:"description,omitempty"`
	Files       map[string]ImageFile `json:"files"`
}

// Size is the total download size of the image.
func (i Image) Size() int64 {
	var n int64
	for _, f := range i.Files {
		n += f.Size
	}
	return n
}

// Index is the Dockyard catalog.
type Index struct {
	Images []Image `json:"images"`
}

// ParseIndex decodes and validates an index document.
func ParseIndex(raw []byte) (*Index, error) {
	var ix Index
	if err := json.Unmarshal(raw, &ix); err != nil {
		return nil, errf("bad_index", "the Dockyard index is not valid JSON: %v", err)
	}
	for _, im := range ix.Images {
		if im.Name == "" || im.Arch == "" {
			return nil, errf("bad_index", "an index entry has no name or arch")
		}
		for _, need := range []string{FileRootfs, FileKernel, FileInitrd} {
			f, ok := im.Files[need]
			if !ok || f.URL == "" || len(f.SHA256) != 64 {
				return nil, errf("bad_index", "image %s/%s lacks a valid %s entry", im.Name, im.Arch, need)
			}
		}
	}
	return &ix, nil
}

// Find returns the image for name and arch.
func (ix *Index) Find(name, arch string) (Image, error) {
	for _, im := range ix.Images {
		if im.Name == name && im.Arch == arch {
			return im, nil
		}
	}
	var have []string
	for _, im := range ix.Images {
		if im.Arch == arch {
			have = append(have, im.Name)
		}
	}
	sort.Strings(have)
	return Image{}, errf("image_not_found", "no image %q for %s in Dockyard (available: %s)", name, arch, strings.Join(have, ", "))
}

// Catalog supplies the image index.
type Catalog interface {
	Index(ctx context.Context) (*Index, error)
}

// HTTPCatalog fetches the index over HTTP(S).
type HTTPCatalog struct {
	URL    string
	Client *http.Client
}

// NewCatalog returns the catalog for this build, honoring DOCK_DOCKYARD_URL.
func NewCatalog(getenv func(string) string) *HTTPCatalog {
	u := getenv("DOCK_DOCKYARD_URL")
	if u == "" {
		u = DockyardIndexURL
	}
	return &HTTPCatalog{URL: u}
}

func (c *HTTPCatalog) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Index downloads and parses the index.
func (c *HTTPCatalog) Index(ctx context.Context) (*Index, error) {
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
	return ParseIndex(raw)
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
}

func (c *Cache) logf(format string, a ...any) {
	if c.Progress != nil {
		fmt.Fprintf(c.Progress, format+"\n", a...)
	}
}

func (c *Cache) imageDir(im Image) string {
	sum := im.Files[FileRootfs].SHA256
	if len(sum) > 12 {
		sum = sum[:12]
	}
	return filepath.Join(c.Dir, im.Name+"-"+im.Arch+"-"+sum)
}

func (c *Cache) paths(im Image) CachedImage {
	d := c.imageDir(im)
	return CachedImage{Dir: d, Kernel: filepath.Join(d, FileKernel), Initrd: filepath.Join(d, FileInitrd), Base: filepath.Join(d, "base.qcow2")}
}

// Cached reports whether im is fully downloaded, verified and converted.
func (c *Cache) Cached(im Image) bool {
	p := c.paths(im)
	_, err := os.Stat(filepath.Join(p.Dir, ".ready"))
	return err == nil
}

// Ensure makes im available locally, downloading and converting on first use.
func (c *Cache) Ensure(ctx context.Context, im Image) (CachedImage, error) {
	p := c.paths(im)
	if c.Cached(im) {
		return p, nil
	}
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return p, errf("cache_error", "%v", err)
	}
	for _, name := range []string{FileKernel, FileInitrd, FileRootfs} {
		dest := filepath.Join(p.Dir, name)
		if st, err := os.Stat(dest); err == nil && st.Size() == im.Files[name].Size {
			if err := VerifyFile(dest, im.Files[name]); err == nil {
				continue
			}
		}
		c.logf("downloading %s (%s)", name, humanBytes(im.Files[name].Size))
		if err := c.download(ctx, im.Files[name], dest); err != nil {
			return p, err
		}
	}
	c.logf("preparing %s disk", im.Name)
	raw := filepath.Join(p.Dir, "rootfs.raw")
	if err := decompress(filepath.Join(p.Dir, FileRootfs), raw); err != nil {
		return p, err
	}
	defer os.Remove(raw)
	tmpBase := p.Base + ".part"
	_ = os.Remove(tmpBase)
	if err := c.Driver.ConvertRaw(ctx, raw, tmpBase); err != nil {
		return p, err
	}
	if err := os.Rename(tmpBase, p.Base); err != nil {
		return p, errf("cache_error", "%v", err)
	}
	if err := os.WriteFile(filepath.Join(p.Dir, ".ready"), nil, 0o600); err != nil {
		return p, errf("cache_error", "%v", err)
	}
	// The compressed rootfs is no longer needed once the base exists.
	_ = os.Remove(filepath.Join(p.Dir, FileRootfs))
	return p, nil
}

// VerifyFile checks a file against its index entry (size, then sha256).
func VerifyFile(path string, want ImageFile) error {
	f, err := os.Open(path)
	if err != nil {
		return errf("verify_failed", "%v", err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return errf("verify_failed", "%v", err)
	}
	if want.Size > 0 && n != want.Size {
		return errf("verify_failed", "%s is %d bytes, the index says %d", filepath.Base(path), n, want.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want.SHA256) {
		return errf("verify_failed", "%s has sha256 %s, the index says %s", filepath.Base(path), got, want.SHA256)
	}
	return nil
}

// download fetches f to dest through a .part file and keeps it only if it verifies.
func (c *Cache) download(ctx context.Context, f ImageFile, dest string) error {
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
	part := dest + ".part"
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
	if err := VerifyFile(part, f); err != nil {
		_ = os.Remove(part)
		return err
	}
	if err := os.Rename(part, dest); err != nil {
		return errf("cache_error", "%v", err)
	}
	return nil
}

func decompress(src, dest string) error {
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
	if _, err := io.Copy(out, zr); err != nil {
		out.Close()
		return errf("decompress_failed", "%v", err)
	}
	return out.Close()
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
