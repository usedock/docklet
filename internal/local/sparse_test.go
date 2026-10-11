package local

import (
	"bytes"
	"context"
	"crypto/sha256"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// sparseInput is 64 MiB of zeros with a little data at the start, the middle, an unaligned
// spot and the end.
func sparseInput() []byte {
	b := make([]byte, 64<<20)
	r := rand.New(rand.NewSource(1))
	for _, off := range []int{0, 32 << 20, 40<<20 + 12345, len(b) - 100} {
		r.Read(b[off : off+100])
	}
	return b
}

func TestDecompressSparseIsIdenticalAndSmall(t *testing.T) {
	in := sparseInput()
	dir := t.TempDir()
	src := filepath.Join(dir, "in.zst")
	if err := os.WriteFile(src, zstdBytes(t, in), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out.raw")
	if err := DecompressFile(context.Background(), src, dst, int64(len(in))); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != sha256.Sum256(in) || len(got) != len(in) {
		t.Fatalf("sparse output differs from the input (len %d, want %d)", len(got), len(in))
	}
	// NTFS reports allocation lazily for cached writes; flush so the figure is real.
	if f, err := os.OpenFile(dst, os.O_RDWR, 0); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
	alloc := allocatedBytes(dst)
	if alloc <= 0 {
		t.Fatalf("allocated %d bytes for a disk holding data", alloc)
	}
	// Four small data regions; filesystems round allocation up (APFS took 8 MiB), but a
	// non-sparse write would take all 64 MiB.
	if alloc > int64(len(in))/4 {
		t.Fatalf("allocated %d bytes for a mostly-zero %d byte disk", alloc, len(in))
	}
}

func TestDecompressTrailingZerosKeepSize(t *testing.T) {
	in := make([]byte, 3*sparseBlock+17)
	in[5] = 1
	dir := t.TempDir()
	src := filepath.Join(dir, "in.zst")
	if err := os.WriteFile(src, zstdBytes(t, in), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out.raw")
	if err := DecompressFile(context.Background(), src, dst, int64(len(in))); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, in) {
		t.Fatalf("got %d bytes, want %d identical", len(got), len(in))
	}
}

func TestDecompressRefusesOverMax(t *testing.T) {
	in := make([]byte, 4*sparseBlock)
	dir := t.TempDir()
	src := filepath.Join(dir, "in.zst")
	if err := os.WriteFile(src, zstdBytes(t, in), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out.raw")
	if err := DecompressFile(context.Background(), src, dst, int64(len(in))-1); codeOf(err) != "decompress_failed" {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("the output must be removed")
	}
}
