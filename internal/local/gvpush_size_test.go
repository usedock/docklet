package local

import (
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
