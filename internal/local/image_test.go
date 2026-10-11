package local

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if _, err := os.Stat(filepath.Join(p.Dir, "rootfs.raw")); !os.IsNotExist(err) {
		t.Fatal("the raw disk must not be kept")
	}
	if !c.Cached(im) {
		t.Fatal("image must be cached after Ensure")
	}
	if _, err := c.Ensure(context.Background(), im); err != nil {
		t.Fatal(err)
	}
	if hits[FileRootfs] != 1 || hits[FileKernel] != 1 {
		t.Fatalf("a cached image must not download again: %v", hits)
	}
}

func TestCacheRejectsACorruptedDownload(t *testing.T) {
	im, _, _ := imageServer(t, "amd64")
	f := im.Files[FileKernel]
	f.SHA256 = strings.Repeat("0", 64)
	im.Files[FileKernel] = f
	c := &Cache{Dir: t.TempDir(), Driver: newFakeDriver()}
	_, err := c.Ensure(context.Background(), im)
	if codeOf(err) != "verify_failed" {
		t.Fatalf("a wrong sha256 must fail verification, got %v", err)
	}
	if c.Cached(im) {
		t.Fatal("a failed image must not be marked ready")
	}
	if _, serr := os.Stat(filepath.Join(c.imageDir(im), FileKernel)); !os.IsNotExist(serr) {
		t.Fatal("the unverified file must be removed, not kept under its final name")
	}
}

func TestCacheRejectsOversizedBody(t *testing.T) {
	im, _, _ := imageServer(t, "amd64")
	f := im.Files[FileInitrd]
	f.Size = 3 // the server sends more than the index promises
	im.Files[FileInitrd] = f
	c := &Cache{Dir: t.TempDir(), Driver: newFakeDriver()}
	if _, err := c.Ensure(context.Background(), im); codeOf(err) != "verify_failed" {
		t.Fatalf("got %v", err)
	}
}

func TestCacheRefusesPlainHTTPToTheInternet(t *testing.T) {
	im, _, _ := imageServer(t, "amd64")
	f := im.Files[FileKernel]
	f.URL = "http://example.com/vmlinuz"
	im.Files[FileKernel] = f
	c := &Cache{Dir: t.TempDir(), Driver: newFakeDriver()}
	if _, err := c.Ensure(context.Background(), im); codeOf(err) != "insecure_url" {
		t.Fatalf("got %v", err)
	}
}

func TestVerifyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	_ = os.WriteFile(p, []byte("abc"), 0o600)
	good := ImageFile{Size: 3, SHA256: sha([]byte("abc"))}
	if err := VerifyFile(p, good); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFile(p, ImageFile{Size: 4, SHA256: good.SHA256}); err == nil {
		t.Fatal("size mismatch must fail")
	}
	if err := VerifyFile(p, ImageFile{Size: 3, SHA256: sha([]byte("abd"))}); err == nil {
		t.Fatal("hash mismatch must fail")
	}
}

func TestParseIndexAndFind(t *testing.T) {
	im, _, _ := imageServer(t, "amd64")
	raw, _ := json.Marshal(Index{Images: []Image{im}})
	ix, err := ParseIndex(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ix.Find("test-img", "amd64"); err != nil || got.Description != "a test image" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := ix.Find("test-img", "arm64"); codeOf(err) != "image_not_found" {
		t.Fatalf("wrong arch must not match, got %v", err)
	}
	if _, err := ix.Find("nope", "amd64"); err == nil || !strings.Contains(err.Error(), "test-img") {
		t.Fatalf("unknown image must list the available ones, got %v", err)
	}
	for _, bad := range []string{`{`, `{"images":[{"name":"a","arch":"amd64","files":{}}]}`, `{"images":[{"arch":"amd64"}]}`} {
		if _, err := ParseIndex([]byte(bad)); codeOf(err) != "bad_index" {
			t.Errorf("%s must be rejected, got %v", bad, err)
		}
	}
}

func TestHTTPCatalog(t *testing.T) {
	im, _, _ := imageServer(t, "amd64")
	raw, _ := json.Marshal(Index{Images: []Image{im}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(raw)
	}))
	defer srv.Close()
	ix, err := NewCatalog(env(map[string]string{"DOCK_DOCKYARD_URL": srv.URL + "/dockyard.json"})).Index(context.Background())
	if err != nil || len(ix.Images) != 1 {
		t.Fatalf("%v %v", ix, err)
	}
	if _, err := (&HTTPCatalog{URL: srv.URL + "/missing.json"}).Index(context.Background()); codeOf(err) != "index_unavailable" {
		t.Fatalf("got %v", err)
	}
	if got := NewCatalog(env(nil)).URL; got != DockyardIndexURL || !strings.Contains(got, DockyardTag) {
		t.Fatalf("default index URL %q must use the pinned tag", got)
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
