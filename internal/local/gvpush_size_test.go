package local

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestZstContentSizeAndBaseFits(t *testing.T) {
	dir := t.TempDir()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	withSize := filepath.Join(dir, "sized.zst")
	if err := os.WriteFile(withSize, enc.EncodeAll(make([]byte, 5000), nil), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := zstContentSize(withSize); got != 5000 {
		t.Fatalf("declared size %d", got)
	}
	// A missing file has no size.
	if got := zstContentSize(filepath.Join(dir, "missing")); got != 0 {
		t.Fatalf("missing file: %d", got)
	}
	if err := baseFits("img", 3<<30, 2<<30, 2); ErrCode(err) != "push_too_big" || !strings.Contains(err.Error(), "--disk") {
		t.Fatalf("a base larger than the disk: %v", err)
	}
	if err := baseFits("img", 0, 2<<30, 2); err != nil {
		t.Fatalf("an unknown size is let through: %v", err)
	}
	if err := baseFits("img", 2<<30, 2<<30, 2); err != nil {
		t.Fatalf("a base that fits: %v", err)
	}
}

func TestZstContentSizeOfTheRealDockyardImage(t *testing.T) {
	// The first 64 bytes of ubuntu-24.04-amd64.rootfs.ext4.zst (images-2026.10.11.4): build.sh's
	// zstd -19 -T0 records the 2 GiB raw size, so a --disk below it is refused before the stop.
	head, err := hex.DecodeString("28b52ffd8468000000801c1500869e6840f074aa358c84a12788646aa23e1a4858c41d6438258d34390d4a996adfe950957ec8c6e578d673561c5043be4f8a7a")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "rootfs.ext4.zst")
	if err := os.WriteFile(p, head, 0o600); err != nil {
		t.Fatal(err)
	}
	got := zstContentSize(p)
	if got != 2<<30 {
		t.Fatalf("declared size %d, want 2 GiB", got)
	}
	if err := baseFits("ubuntu-24.04", got, 1<<30, 1); ErrCode(err) != "push_too_big" {
		t.Fatalf("--disk 1gb under a 2 GiB base: %v", err)
	}
	if err := baseFits("ubuntu-24.04", got, 2<<30, 2); err != nil {
		t.Fatalf("--disk 2gb: %v", err)
	}
}
