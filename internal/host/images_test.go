package host

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	apiv1 "github.com/usedock/docklet/api/v1"
	"github.com/usedock/docklet/internal/disk"
	"github.com/usedock/docklet/internal/dockyard"
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func zst(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeRelease serves a dockyard.json and the files it names from a test server.
type fakeRelease struct {
	srv   *httptest.Server
	index []byte
	files map[string][]byte
	mu    sync.Mutex
	paths []string
}

// newRelease publishes image name with the given raw rootfs. The kernel and
// initrd are in the index (the real one lists them) so a pull that fetched
// them would show in paths.
func newRelease(t *testing.T, name string, raw []byte) *fakeRelease {
	t.Helper()
	r := &fakeRelease{files: map[string][]byte{}}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.paths = append(r.paths, req.URL.Path)
		body, ok := r.files[req.URL.Path]
		r.mu.Unlock()
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(r.srv.Close)
	img := dockyard.Image{Name: name, Arch: "amd64", Files: map[string]dockyard.File{}}
	for role, suffix := range map[string]string{dockyard.FileRootfs: "rootfs.ext4.zst", dockyard.FileKernel: "vmlinuz", dockyard.FileInitrd: "initrd.img"} {
		body := []byte("kernel-" + role)
		if role == dockyard.FileRootfs {
			body = zst(t, raw)
		}
		path := "/" + name + "-amd64." + suffix
		r.files[path] = body
		img.Files[role] = dockyard.File{URL: r.srv.URL + path, Size: int64(len(body)), SHA256: sum(body)}
	}
	ix := &dockyard.Index{Version: "images-test", Images: []dockyard.Image{img}}
	var err error
	if r.index, err = ix.Marshal(); err != nil {
		t.Fatal(err)
	}
	r.files["/dockyard.json"] = r.index
	return r
}

func (r *fakeRelease) puller(dir string) *Puller {
	return &Puller{Dir: dir, IndexURL: r.srv.URL + "/dockyard.json", IndexSHA256: sum(r.index), Arch: "amd64", Client: r.srv.Client()}
}

// rawImage is ext4-like: data, a long hole, data, a trailing hole.
func rawImage() []byte {
	raw := bytes.Repeat([]byte("A"), 100<<10)
	raw = append(raw, make([]byte, 400<<10)...)
	raw = append(raw, bytes.Repeat([]byte("B"), 3000)...)
	return append(raw, make([]byte, 200<<10)...)
}

func withImageDir(t *testing.T, dir string) {
	t.Helper()
	SetImageDir(dir)
	t.Cleanup(func() { SetImageDir("") })
}

func TestPullInstallsAndResolves(t *testing.T) {
	raw := rawImage()
	rel := newRelease(t, "node-22", raw)
	dir := t.TempDir()
	withImageDir(t, dir)

	if _, err := ResolveImage("node-22"); !errors.Is(err, ErrImageUnavailable) {
		t.Fatalf("before pull err = %v, want image_unavailable", err)
	}
	img, err := rel.puller(dir).Pull(context.Background(), "node-22")
	if err != nil {
		t.Fatal(err)
	}
	if img.Fingerprint != sum(raw) || img.Version != "images-test" {
		t.Fatalf("pulled = %+v", img)
	}
	if got, err := ResolveImage("node-22"); err != nil || got != "node-22" {
		t.Fatalf("after pull = %q, %v", got, err)
	}
	// The base is content-addressed like the built base and byte-exact (holes included).
	base := filepath.Join(dir, "bases", sum(raw), "base.ext4")
	if got, err := os.ReadFile(base); err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("installed base differs from the rootfs (err %v)", err)
	}
	// New docks clone it and are labelled with its fingerprint, so deltas diff against it.
	var copied string
	run := func(_ string, args ...string) error { copied = args[len(args)-2]; return nil }
	dest := filepath.Join(t.TempDir(), "rootfs.ext4")
	pointer := basePointerFor(filepath.Join(dir, "base.ext4"), "node-22")
	if err := disk.ProvisionFromBase(run, pointer, dest); err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.EvalSymlinks(base); copied != want {
		t.Fatalf("clone source = %s, want %s", copied, want)
	}
	if disk.BaseLabel(dest) != sum(raw) {
		t.Fatalf("dock label = %q, want %s", disk.BaseLabel(dest), sum(raw))
	}
	// The host kernel stays: the image's vmlinuz and initrd are never fetched.
	for _, p := range rel.paths {
		if strings.HasSuffix(p, ".vmlinuz") || strings.HasSuffix(p, ".initrd.img") {
			t.Fatalf("pull fetched %s", p)
		}
	}
	if got := ListInstalled(dir); len(got) != 1 || got[0].Name != "node-22" || got[0].Version != "images-test" {
		t.Fatalf("ListInstalled = %+v", got)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "bases", ".pull-*")); len(leftovers) != 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
}

func TestPullRefusesUnverified(t *testing.T) {
	raw := rawImage()
	dir := t.TempDir()
	withImageDir(t, dir)
	ctx := context.Background()
	archive := "/node-22-amd64.rootfs.ext4.zst"

	t.Run("index does not match the pin", func(t *testing.T) {
		p := newRelease(t, "node-22", raw).puller(dir)
		p.IndexSHA256 = strings.Repeat("0", 64)
		if _, err := p.Pull(ctx, "node-22"); err == nil || !strings.Contains(err.Error(), "pinned") {
			t.Fatalf("err = %v, want a pin mismatch", err)
		}
	})
	t.Run("no pin configured", func(t *testing.T) {
		p := newRelease(t, "node-22", raw).puller(dir)
		p.IndexSHA256 = ""
		if _, err := p.Pull(ctx, "node-22"); err == nil {
			t.Fatal("pull without a pin must fail")
		}
	})
	t.Run("archive tampered after indexing", func(t *testing.T) {
		rel := newRelease(t, "node-22", raw)
		tampered := bytes.Clone(rel.files[archive])
		tampered[len(tampered)/2] ^= 0xff
		rel.files[archive] = tampered
		if _, err := rel.puller(dir).Pull(ctx, "node-22"); err == nil {
			t.Fatal("tampered archive must be refused")
		}
	})
	t.Run("server sends more than the indexed size", func(t *testing.T) {
		rel := newRelease(t, "node-22", raw)
		rel.files[archive] = append(bytes.Clone(rel.files[archive]), make([]byte, 4096)...)
		if _, err := rel.puller(dir).Pull(ctx, "node-22"); err == nil {
			t.Fatal("oversized download must be refused")
		}
	})
	t.Run("decompressed size over the cap", func(t *testing.T) {
		p := newRelease(t, "node-22", raw).puller(dir)
		p.MaxRootfsBytes = 64 << 10
		if _, err := p.Pull(ctx, "node-22"); err == nil || !strings.Contains(err.Error(), "cap") {
			t.Fatalf("err = %v, want the expansion cap", err)
		}
	})
	if _, err := ResolveImage("node-22"); !errors.Is(err, ErrImageUnavailable) {
		t.Fatalf("a refused pull left node-22 resolvable: %v", err)
	}
	if got := ListInstalled(dir); len(got) != 0 {
		t.Fatalf("refused pulls installed %+v", got)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "bases", ".pull-*")); len(leftovers) != 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
}

func TestPullRefusesUnknownAndUnlisted(t *testing.T) {
	p := newRelease(t, "node-22", rawImage()).puller(t.TempDir())
	ctx := context.Background()
	if _, err := p.Pull(ctx, "python-3.12"); !errors.Is(err, ErrImageNotInCatalog) {
		t.Fatalf("unknown image err = %v", err)
	}
	// The default image is pullable now, but only when the release lists it.
	if _, err := p.Pull(ctx, apiv1.DefaultImage); !errors.Is(err, ErrImageNotInCatalog) {
		t.Fatalf("default image missing from the release err = %v, want not in catalog", err)
	}
	if _, err := p.Pull(ctx, "../evil"); err == nil {
		t.Fatal("a path-like name must be refused")
	}
	p.Arch = "arm64"
	if _, err := p.Pull(ctx, "node-22"); !errors.Is(err, ErrImageNotInCatalog) {
		t.Fatalf("other arch err = %v, want not in catalog", err)
	}
}

func TestPullFailureKeepsTheInstalledImage(t *testing.T) {
	raw := rawImage()
	dir := t.TempDir()
	withImageDir(t, dir)
	ctx := context.Background()
	if _, err := newRelease(t, "node-22", raw).puller(dir).Pull(ctx, "node-22"); err != nil {
		t.Fatal(err)
	}
	// A release with different content, cut off mid-download.
	newer := append(bytes.Clone(raw), 'x')
	bad := newRelease(t, "node-22", newer)
	archive := "/node-22-amd64.rootfs.ext4.zst"
	bad.files[archive] = bad.files[archive][:len(bad.files[archive])/2]
	if _, err := bad.puller(dir).Pull(ctx, "node-22"); err == nil {
		t.Fatal("truncated archive must fail")
	}
	pointer := basePointerFor(filepath.Join(dir, "base.ext4"), "node-22")
	if got, err := os.ReadFile(pointer); err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("installed image changed after a failed pull (err %v)", err)
	}
	// A good re-pull of new content repoints atomically and keeps the old base held.
	if _, err := newRelease(t, "node-22", newer).puller(dir).Pull(ctx, "node-22"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bases", sum(raw), "base.ext4")); err != nil {
		t.Fatalf("old base was removed: %v", err)
	}
	if got, _ := os.ReadFile(pointer); !bytes.Equal(got, newer) {
		t.Fatal("pointer did not move to the new image")
	}
}

func TestResolveImageServesInstalledOnly(t *testing.T) {
	dir := t.TempDir()
	withImageDir(t, dir)
	if got, err := ResolveImage(""); err != nil || got != apiv1.DefaultImage {
		t.Fatalf("empty = %q, %v", got, err)
	}
	for _, name := range []string{"node-22", "../x", "bases", "ubuntu-25.04"} {
		if _, err := ResolveImage(name); !errors.Is(err, ErrImageUnavailable) {
			t.Fatalf("%q err = %v, want image_unavailable", name, err)
		}
	}
	// A directory without a base file (half-removed install) is not served.
	if err := os.MkdirAll(filepath.Join(dir, "images", "node-22"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveImage("node-22"); !errors.Is(err, ErrImageUnavailable) {
		t.Fatalf("empty image dir was served: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "images", "node-22", "base.ext4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveImage("node-22"); err != nil || got != "node-22" {
		t.Fatalf("installed = %q, %v", got, err)
	}
	// With no image dir configured only the built base is served.
	SetImageDir("")
	if _, err := ResolveImage("node-22"); !errors.Is(err, ErrImageUnavailable) {
		t.Fatalf("unconfigured dir served node-22: %v", err)
	}
}

func TestServedImagesListsBuiltBaseAndPulled(t *testing.T) {
	dir := t.TempDir()
	if got := ServedImages(dir); got == nil || len(got) != 0 {
		t.Fatalf("empty host = %#v, want []", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "base.ext4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel := newRelease(t, "node-22", rawImage())
	if _, err := rel.puller(dir).Pull(context.Background(), "node-22"); err != nil {
		t.Fatal(err)
	}
	if got := ServedImages(dir); !reflect.DeepEqual(got, []string{"node-22", "ubuntu-24.04"}) {
		t.Fatalf("ServedImages = %v", got)
	}
}

func TestPullRemovesStalePullFiles(t *testing.T) {
	dir := t.TempDir()
	bases := filepath.Join(dir, "bases")
	if err := os.MkdirAll(bases, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(bases, ".pull-123.ext4")
	fresh := filepath.Join(bases, ".pull-456.zst")
	other := filepath.Join(bases, "keep.me")
	for _, f := range []string{stale, fresh, other} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	for _, f := range []string{stale, other} {
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := newRelease(t, "node-22", rawImage()).puller(dir).Pull(context.Background(), "node-22"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("a .pull-* file older than an hour must be removed")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a fresh .pull-* file may belong to a running pull: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("a non-.pull file was removed: %v", err)
	}
}

func TestConcurrentPullsOfOneNameSerialize(t *testing.T) {
	raw := rawImage()
	rel := newRelease(t, "node-22", raw)
	dir := t.TempDir()
	withImageDir(t, dir)
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := rel.puller(dir).Pull(context.Background(), "node-22")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent pull failed: %v", err)
		}
	}
	if got, err := os.ReadFile(basePointerFor(filepath.Join(dir, "base.ext4"), "node-22")); err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("installed image wrong (err %v)", err)
	}
	for _, pat := range []string{filepath.Join(dir, "bases", ".pull-*"), filepath.Join(dir, "images", "node-22", ".*")} {
		if left, _ := filepath.Glob(pat); len(left) != 0 {
			t.Fatalf("temp files left behind: %v", left)
		}
	}
}

func TestPullWaitsForTheNameLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockFile(filepath.Join(dir, "images", ".lock-node-22"))
	if err != nil {
		t.Fatal(err)
	}
	rel := newRelease(t, "node-22", rawImage())
	done := make(chan error, 1)
	go func() {
		_, err := rel.puller(dir).Pull(context.Background(), "node-22")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("pull finished while the name lock was held (err %v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pull did not proceed after the lock was released")
	}
}

func TestPullDefaultImageOnAFreshHostBecomesTheBase(t *testing.T) {
	raw := rawImage()
	dir := t.TempDir()
	withImageDir(t, dir)
	rel := newRelease(t, apiv1.DefaultImage, raw)
	for i := 0; i < 2; i++ { // pulling again is a no-op
		img, err := rel.puller(dir).Pull(context.Background(), apiv1.DefaultImage)
		if err != nil {
			t.Fatal(err)
		}
		if img.Name != apiv1.DefaultImage || img.Fingerprint != sum(raw) {
			t.Fatalf("pulled = %+v", img)
		}
	}
	pointer := filepath.Join(dir, "base.ext4")
	if got, err := os.ReadFile(pointer); err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("base.ext4 does not serve the pulled image (err %v)", err)
	}
	if _, fp, err := disk.CurrentBase(pointer); err != nil || fp != sum(raw) {
		t.Fatalf("current base = %q, %v; want %s", fp, err, sum(raw))
	}
	// Docks of it report the Dockyard image they run.
	if got := disk.BaseSourceName(pointer, sum(raw)); got != apiv1.DefaultImage {
		t.Fatalf("base source = %q, want %s", got, apiv1.DefaultImage)
	}
	// The default image is served through base.ext4 itself: no second named pointer.
	if _, err := os.Lstat(imagePointer(dir, apiv1.DefaultImage)); !os.IsNotExist(err) {
		t.Fatalf("default image got its own pointer: %v", err)
	}
	if got := ServedImages(dir); !reflect.DeepEqual(got, []string{apiv1.DefaultImage}) {
		t.Fatalf("ServedImages = %v", got)
	}
	if got := ListInstalled(dir); len(got) != 0 {
		t.Fatalf("ListInstalled = %+v, the default is not a named pull", got)
	}
	if got, err := ResolveImage(""); err != nil || got != apiv1.DefaultImage {
		t.Fatalf("ResolveImage = %q, %v", got, err)
	}
}

func TestPullDefaultImageKeepsAHostBuiltBaseHeld(t *testing.T) {
	dir := t.TempDir()
	withImageDir(t, dir)
	// An existing host: a plain base.ext4 some delta generations were built against.
	built := append(bytes.Clone(rawImage()), "host-built"...)
	pointer := filepath.Join(dir, "base.ext4")
	if err := os.WriteFile(pointer, built, 0o644); err != nil {
		t.Fatal(err)
	}
	old, err := disk.Fingerprint(pointer)
	if err != nil {
		t.Fatal(err)
	}
	raw := rawImage()
	if _, err := newRelease(t, apiv1.DefaultImage, raw).puller(dir).Pull(context.Background(), apiv1.DefaultImage); err != nil {
		t.Fatal(err)
	}
	if _, fp, err := disk.CurrentBase(pointer); err != nil || fp != sum(raw) {
		t.Fatalf("current base = %q, %v; want the pulled image", fp, err)
	}
	held, err := disk.FindBase(pointer, old)
	if err != nil {
		t.Fatalf("the host-built base must stay restorable: %v", err)
	}
	if got, _ := os.ReadFile(held); !bytes.Equal(got, built) {
		t.Fatal("the held host-built base changed")
	}
	// Heartbeat fingerprints list both bases.
	fps, err := disk.ListBases(pointer)
	if err != nil || !reflect.DeepEqual(fps, sortedStrings(old, sum(raw))) {
		t.Fatalf("ListBases = %v, %v", fps, err)
	}
	// The host-built base has no Dockyard source, the pulled one does.
	if got := disk.BaseSourceName(pointer, old); got != "" {
		t.Fatalf("host-built base source = %q", got)
	}
}

func sortedStrings(a, b string) []string {
	if a > b {
		a, b = b, a
	}
	return []string{a, b}
}

func TestPullNamedImageRecordsItsSource(t *testing.T) {
	raw := rawImage()
	dir := t.TempDir()
	if _, err := newRelease(t, "node-22", raw).puller(dir).Pull(context.Background(), "node-22"); err != nil {
		t.Fatal(err)
	}
	if got := disk.BaseSourceName(filepath.Join(dir, "base.ext4"), sum(raw)); got != "node-22" {
		t.Fatalf("base source = %q, want node-22", got)
	}
}
