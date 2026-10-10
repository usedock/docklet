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
