package dockyard

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestParseCatalogShipped(t *testing.T) {
	c, err := LoadCatalog(filepath.Join("..", "..", "images", "catalog.toml"))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range c.Images {
		names[e.Name] = true
		if e.Setup != "" {
			if _, err := os.Stat(filepath.Join("..", "..", "images", e.Setup)); err != nil {
				t.Errorf("%s: setup script missing: %v", e.Name, err)
			}
		}
	}
	for _, want := range []string{"ubuntu-24.04", "node-22", "python-3.12", "browser"} {
		if !names[want] {
			t.Errorf("catalog lacks %s", want)
		}
	}
}

func TestParseCatalogErrors(t *testing.T) {
	cases := map[string]string{
		"empty":        "",
		"bad name":     "[[image]]\nname='Bad Name'\nsize_mib=1\narch=['amd64']\n",
		"no size":      "[[image]]\nname='a'\narch=['amd64']\n",
		"no arch":      "[[image]]\nname='a'\nsize_mib=1\n",
		"unknown base": "[[image]]\nname='a'\nbase='b'\nsize_mib=1\narch=['amd64']\n",
		"duplicate":    "[[image]]\nname='a'\nsize_mib=1\narch=['amd64']\n[[image]]\nname='a'\nsize_mib=1\narch=['amd64']\n",
		"setup path":   "[[image]]\nname='a'\nsetup='../x.sh'\nsize_mib=1\narch=['amd64']\n",
		"bad arch":     "[[image]]\nname='a'\nsize_mib=1\narch=['../x']\n",
	}
	for name, in := range cases {
		if _, err := ParseCatalog([]byte(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestBuildIndexRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c, err := ParseCatalog([]byte("[[image]]\nname='ubuntu-24.04'\ndescription='d'\nsize_mib=1\narch=['amd64']\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"rootfs.ext4.zst", "vmlinuz", "initrd.img"} {
		writeFile(t, dir, "ubuntu-24.04-amd64."+s, "content of "+s)
	}
	ix, err := c.BuildIndex("images-1", "https://example.test/dl/images-1", dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ix.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseIndex(b)
	if err != nil {
		t.Fatal(err)
	}
	img, ok := back.Find("ubuntu-24.04", "amd64")
	if !ok {
		t.Fatal("image not found")
	}
	k := img.Files[FileKernel]
	if k.URL != "https://example.test/dl/images-1/ubuntu-24.04-amd64.vmlinuz" || k.SHA256 != sum("content of vmlinuz") || k.Size != int64(len("content of vmlinuz")) {
		t.Errorf("kernel entry wrong: %+v", k)
	}
	if _, ok := back.Find("ubuntu-24.04", "arm64"); ok {
		t.Error("arm64 should not be found")
	}
	if _, ok := back.Find("nope", "amd64"); ok {
		t.Error("unknown name should not be found")
	}
}

func TestBuildIndexMissingFile(t *testing.T) {
	c, _ := ParseCatalog([]byte("[[image]]\nname='a'\nsize_mib=1\narch=['amd64']\n"))
	if _, err := c.BuildIndex("v", "u", t.TempDir()); err == nil {
		t.Fatal("expected error for missing files")
	}
}

// fullImage is an index image entry with all three files.
func fullImage(name, arch string) string {
	f := `{"url":"u","size":1,"sha256":"` + sum("x") + `"}`
	return `{"name":"` + name + `","arch":"` + arch + `","files":{"rootfs.ext4.zst":` + f + `,"vmlinuz":` + f + `,"initrd.img":` + f + `}}`
}

func TestParseIndexErrors(t *testing.T) {
	cases := map[string]string{
		"not json":   "{",
		"no version": `{"images":[]}`,
		"no name":    `{"version":"v","images":[{"arch":"amd64"}]}`,
		"bad sha":    `{"version":"v","images":[` + strings.Replace(fullImage("a", "amd64"), sum("x"), "zz", 1) + `]}`,
		"no url":     `{"version":"v","images":[` + strings.Replace(fullImage("a", "amd64"), `"url":"u",`, "", 1) + `]}`,
		"duplicate":  `{"version":"v","images":[` + fullImage("a", "amd64") + `,` + fullImage("a", "amd64") + `]}`,
		"no files":   `{"version":"v","images":[{"name":"a","arch":"amd64","files":{}}]}`,
		"traversal":  `{"version":"v","images":[` + fullImage("../../x", "amd64") + `]}`,
		"bad arch":   `{"version":"v","images":[` + fullImage("a", "../amd64") + `]}`,
	}
	if _, err := ParseIndex([]byte(`{"version":"v","images":[` + fullImage("a", "amd64") + `]}`)); err != nil {
		t.Fatalf("complete image rejected: %v", err)
	}
	for name, in := range cases {
		if _, err := ParseIndex([]byte(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestVerifyFile(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "f", "hello")
	good := File{URL: "u", Size: 5, SHA256: sum("hello")}
	if err := VerifyFile(p, good); err != nil {
		t.Fatalf("good file: %v", err)
	}
	upper := good
	upper.SHA256 = strings.ToUpper(good.SHA256)
	if err := VerifyFile(p, upper); err != nil {
		t.Fatalf("uppercase hash: %v", err)
	}
	bad := good
	bad.SHA256 = sum("other")
	if err := VerifyFile(p, bad); err == nil {
		t.Error("bad hash accepted")
	}
	wrong := good
	wrong.Size = 6
	if err := VerifyFile(p, wrong); err == nil {
		t.Error("wrong size accepted")
	}
	if err := VerifyFile(filepath.Join(dir, "missing"), good); err == nil {
		t.Error("missing file accepted")
	}
}
