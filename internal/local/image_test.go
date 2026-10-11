package local

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/usedock/docklet/internal/dockyard"
)

func TestCacheEnsureVerifiesConvertsAndCaches(t *testing.T) {
	im, _, hits := imageServer(t, "amd64")
	drv := newFakeDriver()
	c := &Cache{Dir: t.TempDir(), Driver: drv}
	if c.Cached(im) {
		t.Fatal("fresh cache must be empty")
	}
	p, err := c.Ensure(context.Background(), im)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := os.ReadFile(p.Base)
	if string(base) != "qcow2:rootfs-raw-bytes" {
		t.Fatalf("base must be the decompressed rootfs converted once, got %q", base)
	}
	if k, _ := os.ReadFile(p.Kernel); string(k) != "kernel-bytes" {
		t.Fatalf("kernel %q", k)
	}
	if left, _ := filepath.Glob(filepath.Join(p.Dir, "*.raw")); len(left) > 0 {
		t.Fatal("the raw disk must not be kept")
	}
	if !c.Cached(im) {
		t.Fatal("image must be cached after Ensure")
	}
	if _, err := c.Ensure(context.Background(), im); err != nil {
		t.Fatal(err)
	}
	if hits[dockyard.FileRootfs] != 1 || hits[dockyard.FileKernel] != 1 {
		t.Fatalf("a cached image must not download again: %v", hits)
	}
}

func TestCacheRejectsACorruptedDownload(t *testing.T) {
	im, _, _ := imageServer(t, "amd64")
	f := im.Files[dockyard.FileKernel]
	f.SHA256 = strings.Repeat("0", 64)
	im.Files[dockyard.FileKernel] = f
	c := &Cache{Dir: t.TempDir(), Driver: newFakeDriver()}
	_, err := c.Ensure(context.Background(), im)
	if codeOf(err) != "verify_failed" {
		t.Fatalf("a wrong sha256 must fail verification, got %v", err)
	}
	if c.Cached(im) {
		t.Fatal("a failed image must not be marked ready")
	}
	if _, serr := os.Stat(filepath.Join(c.imageDir(im), dockyard.FileKernel)); !os.IsNotExist(serr) {
		t.Fatal("the unverified file must be removed, not kept under its final name")
	}
}

func TestCacheRejectsOversizedBody(t *testing.T) {
	im, _, _ := imageServer(t, "amd64")
	f := im.Files[dockyard.FileInitrd]
	f.Size = 3 // the server sends more than the index promises
	im.Files[dockyard.FileInitrd] = f
	c := &Cache{Dir: t.TempDir(), Driver: newFakeDriver()}
	if _, err := c.Ensure(context.Background(), im); codeOf(err) != "verify_failed" {
		t.Fatalf("got %v", err)
	}
}

func TestCacheRefusesPlainHTTPToTheInternet(t *testing.T) {
	im, _, _ := imageServer(t, "amd64")
	f := im.Files[dockyard.FileKernel]
	f.URL = "http://example.com/vmlinuz"
	im.Files[dockyard.FileKernel] = f
	c := &Cache{Dir: t.TempDir(), Driver: newFakeDriver()}
	if _, err := c.Ensure(context.Background(), im); codeOf(err) != "insecure_url" {
		t.Fatalf("got %v", err)
	}
}

func TestVerifyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	_ = os.WriteFile(p, []byte("abc"), 0o600)
	good := dockyard.File{Size: 3, SHA256: sha([]byte("abc"))}
	if err := verifyFile(p, good); err != nil {
		t.Fatal(err)
	}
	if err := verifyFile(p, dockyard.File{Size: 4, SHA256: good.SHA256}); err == nil {
		t.Fatal("size mismatch must fail")
	}
	if err := verifyFile(p, dockyard.File{Size: 3, SHA256: sha([]byte("abd"))}); err == nil {
		t.Fatal("hash mismatch must fail")
	}
}

func TestFindImage(t *testing.T) {
	im, _, _ := imageServer(t, "amd64")
	ix := &dockyard.Index{Version: "t", Images: []dockyard.Image{im}}
	if got, err := findImage(ix, "test-img", "amd64"); err != nil || got.Description != "a test image" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := findImage(ix, "test-img", "arm64"); codeOf(err) != "image_not_found" {
		t.Fatalf("wrong arch must not match, got %v", err)
	}
	if _, err := findImage(ix, "nope", "amd64"); err == nil || !strings.Contains(err.Error(), "test-img") {
		t.Fatalf("unknown image must list the available ones, got %v", err)
	}
	evil := im
	evil.Name = "../../outside"
	if _, err := findImage(&dockyard.Index{Version: "t", Images: []dockyard.Image{evil}}, evil.Name, "amd64"); codeOf(err) != "bad_index" {
		t.Fatalf("an image name with a path must be refused, got %v", err)
	}
	delete(im.Files, dockyard.FileKernel)
	if _, err := findImage(&dockyard.Index{Version: "t", Images: []dockyard.Image{im}}, "test-img", "amd64"); codeOf(err) != "bad_index" {
		t.Fatalf("an image without a kernel is unusable, got %v", err)
	}
}

func indexServer(t *testing.T) (url, sum string) {
	t.Helper()
	im, _, _ := imageServer(t, "amd64")
	raw, err := (&dockyard.Index{Version: "images-test", Images: []dockyard.Image{im}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/dockyard.json", sha(raw)
}

func TestCatalogVerifiesTheIndexAgainstItsPin(t *testing.T) {
	url, sum := indexServer(t)
	ctx := context.Background()
	ix, err := (&HTTPCatalog{URL: url, SHA256: sum}).Index(ctx)
	if err != nil || len(ix.Images) != 1 {
		t.Fatalf("a matching pin must load: %v %v", ix, err)
	}
	// A swapped index is refused before it is parsed.
	_, err = (&HTTPCatalog{URL: url, SHA256: strings.Repeat("0", 64)}).Index(ctx)
	if codeOf(err) != "index_mismatch" {
		t.Fatalf("a wrong pin must refuse the index, got %v", err)
	}
	if _, err := (&HTTPCatalog{URL: strings.Replace(url, "dockyard.json", "missing.json", 1), SHA256: sum}).Index(ctx); codeOf(err) != "index_unavailable" {
		t.Fatalf("got %v", err)
	}
}

func TestCatalogOverrideWarnsAndVerifiesWhenAHashIsGiven(t *testing.T) {
	url, sum := indexServer(t)
	var warn bytes.Buffer
	c := NewCatalog(env(map[string]string{"DOCK_DOCKYARD_URL": url}), &warn)
	if !c.Unpinned || !strings.Contains(warn.String(), "unpinned and unverified") {
		t.Fatalf("an unhashed override must warn, got %q", warn.String())
	}
	if _, err := c.Index(context.Background()); err != nil {
		t.Fatalf("the override loads with the warning: %v", err)
	}
	warn.Reset()
	c = NewCatalog(env(map[string]string{"DOCK_DOCKYARD_URL": url, "DOCK_DOCKYARD_SHA256": strings.ToUpper(sum)}), &warn)
	if !strings.Contains(warn.String(), "not the release pinned") || c.SHA256 != sum {
		t.Fatalf("warning %q sha %q", warn.String(), c.SHA256)
	}
	if _, err := c.Index(context.Background()); err != nil {
		t.Fatal(err)
	}
	c = NewCatalog(env(map[string]string{"DOCK_DOCKYARD_URL": url, "DOCK_DOCKYARD_SHA256": strings.Repeat("a", 64)}), nil)
	if _, err := c.Index(context.Background()); codeOf(err) != "index_mismatch" {
		t.Fatalf("a wrong DOCK_DOCKYARD_SHA256 must refuse, got %v", err)
	}
}

func TestDoctorOnFakeDriver(t *testing.T) {
	e, _, _ := newTestEngine(t)
	checks := e.Doctor(context.Background())
	names := map[string]bool{}
	for _, c := range checks {
		names[c.Name] = c.OK
	}
	if !names["dockyard"] || !names["default image"] {
		t.Fatalf("checks %+v", checks)
	}
}

func TestDecompressIsCapped(t *testing.T) {
	im, _, _ := imageServer(t, "amd64")
	c := &Cache{Dir: t.TempDir(), Driver: newFakeDriver(), MaxRawBytes: 4}
	_, err := c.Ensure(context.Background(), im)
	if codeOf(err) != "decompress_failed" || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("a rootfs larger than the cap must be refused, got %v", err)
	}
	if c.Cached(im) {
		t.Fatal("a refused image must not be marked ready")
	}
	entries, _ := os.ReadDir(c.imageDir(im))
	for _, f := range entries {
		if strings.HasSuffix(f.Name(), ".raw") {
			t.Fatalf("partial raw disk %s must be removed", f.Name())
		}
	}
	c.MaxRawBytes = 0
	if _, err := c.Ensure(context.Background(), im); err != nil {
		t.Fatalf("the default cap must allow a normal image: %v", err)
	}
}

func TestEnsureRemovesStalePartFiles(t *testing.T) {
	im, _, _ := imageServer(t, "amd64")
	c := &Cache{Dir: t.TempDir(), Driver: newFakeDriver()}
	dir := c.imageDir(im)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale, fresh := filepath.Join(dir, "vmlinuz.part"), filepath.Join(dir, "initrd.img.part")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ensure(context.Background(), im); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("a .part file older than an hour must be removed")
	}
	// The fresh one belongs to a download that may still be running (it was replaced by this one's own).
	_ = fresh
}

func TestCatalogUsesTheSharedDockyardPin(t *testing.T) {
	c := NewCatalog(env(nil), nil)
	if c.URL != dockyard.DockyardIndexURL || c.SHA256 != dockyard.DockyardIndexSHA256 {
		t.Fatalf("catalog pin = %q %q, want the internal/dockyard pin", c.URL, c.SHA256)
	}
}
