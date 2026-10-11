package disk

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// copyRunner stands in for cp in Provision.
func copyRunner(name string, args ...string) error {
	if name != "cp" {
		return errors.New("unexpected " + name)
	}
	raw, err := os.ReadFile(args[len(args)-2])
	if err != nil {
		return err
	}
	return os.WriteFile(args[len(args)-1], raw, 0o644)
}

// installBase places a distinct image under bases/<fp>/ beside pointer.
func installBase(t *testing.T, pointer string, marker byte) string {
	t.Helper()
	staged := filepath.Join(t.TempDir(), "staged.ext4")
	writeImage(t, staged, 2*BlockSize)
	writeAt(t, staged, 0, []byte{marker})
	raw := readAll(t, staged)
	sum := sha256.Sum256(raw)
	fp := hex.EncodeToString(sum[:])
	dest := BasePath(pointer, fp)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return fp
}

func TestCurrentBaseResolvesThePointer(t *testing.T) {
	pointer := filepath.Join(t.TempDir(), "base.ext4")
	fp := installBase(t, pointer, 1)
	if err := SetCurrentBase(pointer, fp); err != nil {
		t.Fatal(err)
	}
	path, got, err := CurrentBase(pointer)
	if err != nil {
		t.Fatal(err)
	}
	if got != fp {
		t.Fatalf("current fingerprint = %q, want %q", got, fp)
	}
	if want, _ := filepath.EvalSymlinks(BasePath(pointer, fp)); path != want {
		t.Fatalf("current path = %q, want %q", path, want)
	}
	// Repointing is a rename, so it never leaves the pointer missing.
	next := installBase(t, pointer, 2)
	if err := SetCurrentBase(pointer, next); err != nil {
		t.Fatal(err)
	}
	if _, got, _ := CurrentBase(pointer); got != next {
		t.Fatalf("after repoint fingerprint = %q, want %q", got, next)
	}
	if _, err := os.Stat(BasePath(pointer, fp)); err != nil {
		t.Fatalf("repointing must keep the old base: %v", err)
	}
}

func TestCurrentBaseOfALegacyPlainFileHasNoAddress(t *testing.T) {
	pointer := filepath.Join(t.TempDir(), "base.ext4")
	writeImage(t, pointer, BlockSize)
	path, fp, err := CurrentBase(pointer)
	if err != nil {
		t.Fatal(err)
	}
	if fp != "" {
		t.Fatalf("plain file fingerprint = %q, want empty", fp)
	}
	if want, _ := filepath.EvalSymlinks(pointer); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
}

func TestFindBaseFindsANonCurrentBase(t *testing.T) {
	pointer := filepath.Join(t.TempDir(), "base.ext4")
	old := installBase(t, pointer, 1)
	current := installBase(t, pointer, 2)
	if err := SetCurrentBase(pointer, current); err != nil {
		t.Fatal(err)
	}
	path, err := FindBase(pointer, old)
	if err != nil {
		t.Fatal(err)
	}
	if path != BasePath(pointer, old) {
		t.Fatalf("path = %q, want %q", path, BasePath(pointer, old))
	}
}

func TestFindBaseRefusesAMissingBase(t *testing.T) {
	pointer := filepath.Join(t.TempDir(), "base.ext4")
	current := installBase(t, pointer, 1)
	if err := SetCurrentBase(pointer, current); err != nil {
		t.Fatal(err)
	}
	missing := hex.EncodeToString(make([]byte, 32))
	if _, err := FindBase(pointer, missing); !errors.Is(err, ErrBaseUnavailable) {
		t.Fatalf("err = %v, want ErrBaseUnavailable", err)
	}
	if _, err := FindBase(pointer, "../../etc"); !errors.Is(err, ErrBaseUnavailable) {
		t.Fatalf("err = %v, want ErrBaseUnavailable for a malformed fingerprint", err)
	}
}

func TestFindBaseAcceptsAnUnmigratedPlainBase(t *testing.T) {
	pointer := filepath.Join(t.TempDir(), "base.ext4")
	writeImage(t, pointer, BlockSize)
	fp, err := Fingerprint(pointer)
	if err != nil {
		t.Fatal(err)
	}
	path, err := FindBase(pointer, fp)
	if err != nil {
		t.Fatal(err)
	}
	if path != pointer {
		t.Fatalf("path = %q, want %q", path, pointer)
	}
}

func TestProvisionFromBaseLabelsTheRootfs(t *testing.T) {
	dir := t.TempDir()
	pointer := filepath.Join(dir, "base.ext4")
	fp := installBase(t, pointer, 7)
	if err := SetCurrentBase(pointer, fp); err != nil {
		t.Fatal(err)
	}
	rootfs := filepath.Join(dir, "jail", "rootfs.ext4")
	if err := ProvisionFromBase(copyRunner, pointer, rootfs); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readAll(t, rootfs), readAll(t, BasePath(pointer, fp))) {
		t.Fatal("rootfs is not a copy of the current base")
	}
	if got := BaseLabel(rootfs); got != fp {
		t.Fatalf("label = %q, want %q", got, fp)
	}
}

func TestBaseLabelIgnoresGarbage(t *testing.T) {
	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")
	if got := BaseLabel(rootfs); got != "" {
		t.Fatalf("missing label = %q, want empty", got)
	}
	if err := os.WriteFile(rootfs+baseLabelSuffix, []byte("not-a-digest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := BaseLabel(rootfs); got != "" {
		t.Fatalf("garbage label = %q, want empty", got)
	}
}

func TestListBasesIncludesAnUnmigratedPlainBase(t *testing.T) {
	pointer := filepath.Join(t.TempDir(), "base.ext4")
	writeImage(t, pointer, BlockSize)
	fp, err := Fingerprint(pointer)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := ListBases(pointer)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0] != fp {
		t.Fatalf("listed = %v, want the plain current base %s", listed, fp)
	}
}

func TestBaseSourceNamesTheDockyardImage(t *testing.T) {
	pointer := filepath.Join(t.TempDir(), "base.ext4")
	pulled := installBase(t, pointer, 1)
	built := installBase(t, pointer, 2)
	if err := RecordBaseSource(pointer, pulled, "ubuntu-24.04", "2026.10"); err != nil {
		t.Fatal(err)
	}
	if got := BaseSourceName(pointer, pulled); got != "ubuntu-24.04" {
		t.Fatalf("pulled base source = %q", got)
	}
	// A base built on the host has no Dockyard source.
	if got := BaseSourceName(pointer, built); got != "" {
		t.Fatalf("host-built base source = %q, want empty", got)
	}
	if got := BaseSourceName(pointer, "not-a-fingerprint"); got != "" {
		t.Fatalf("invalid fingerprint source = %q", got)
	}
	if err := RecordBaseSource(pointer, "../x", "n", ""); err == nil {
		t.Fatal("an invalid fingerprint must be refused")
	}
}

func TestBaseSourceFallsBackToImageRecords(t *testing.T) {
	// A base pulled before sources were recorded is still named by images/<name>/image.json.
	pointer := filepath.Join(t.TempDir(), "base.ext4")
	fp := installBase(t, pointer, 3)
	dir := filepath.Join(filepath.Dir(pointer), "images", "node-22")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "image.json"), []byte(`{"name":"node-22","fingerprint":"`+fp+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := BaseSourceName(pointer, fp); got != "node-22" {
		t.Fatalf("source = %q, want node-22", got)
	}
}

func TestDockImageFollowsTheLabelThenTheCurrentBase(t *testing.T) {
	pointer := filepath.Join(t.TempDir(), "base.ext4")
	pulled := installBase(t, pointer, 1)
	built := installBase(t, pointer, 2)
	if err := RecordBaseSource(pointer, pulled, "ubuntu-24.04", ""); err != nil {
		t.Fatal(err)
	}
	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")

	if err := SetCurrentBase(pointer, built); err != nil {
		t.Fatal(err)
	}
	if got := DockImage(pointer, rootfs); got != "" {
		t.Fatalf("unlabeled dock on a host-built current base = %q, want empty", got)
	}
	// The label wins over the current base, so a newer default never relabels an old dock.
	if err := LabelBase(rootfs, pulled); err != nil {
		t.Fatal(err)
	}
	if got := DockImage(pointer, rootfs); got != "ubuntu-24.04" {
		t.Fatalf("labeled dock = %q", got)
	}
	if err := LabelBase(rootfs, built); err != nil {
		t.Fatal(err)
	}
	if err := SetCurrentBase(pointer, pulled); err != nil {
		t.Fatal(err)
	}
	if got := DockImage(pointer, rootfs); got != "" {
		t.Fatalf("dock labeled with a host-built base = %q, want empty", got)
	}
	// An unlabeled dock follows the current base.
	_ = os.Remove(rootfs + baseLabelSuffix)
	if got := DockImage(pointer, rootfs); got != "ubuntu-24.04" {
		t.Fatalf("unlabeled dock on the pulled current base = %q", got)
	}
}

func TestImageFingerprintResolvesNamedAndDefaultImages(t *testing.T) {
	pointer := filepath.Join(t.TempDir(), "base.ext4")
	def := installBase(t, pointer, 1)
	named := installBase(t, pointer, 2)
	if err := RecordBaseSource(pointer, def, "ubuntu-24.04", ""); err != nil {
		t.Fatal(err)
	}
	if err := SetCurrentBase(pointer, def); err != nil {
		t.Fatal(err)
	}
	imgDir := filepath.Join(filepath.Dir(pointer), "images", "node-22")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join("..", "..", BasesDir, named, baseFile), filepath.Join(imgDir, baseFile)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := ImageFingerprint(pointer, "node-22"); got != named {
		t.Fatalf("node-22 = %q, want %q", got, named)
	}
	if got := ImageFingerprint(pointer, "ubuntu-24.04"); got != def {
		t.Fatalf("ubuntu-24.04 = %q, want the current base %q", got, def)
	}
	for _, name := range []string{"", "python-3.12", "../x"} {
		if got := ImageFingerprint(pointer, name); got != "" {
			t.Fatalf("%q = %q, want empty", name, got)
		}
	}
}
