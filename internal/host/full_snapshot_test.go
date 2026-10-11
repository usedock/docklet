package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/usedock/docklet/api/v1"
	"github.com/usedock/docklet/internal/disk"
	"github.com/usedock/docklet/internal/fc"
	"github.com/usedock/docklet/internal/store"
)

// fullHost is a test host with a dead VM (so a capture is a disk-only freeze) and a dock disk.
func fullHost(t *testing.T) *Host {
	t.Helper()
	h, driver, dir := testHost(t)
	jail := filepath.Join(dir, "jail", "firecracker", "dock-9", "root")
	if err := os.MkdirAll(jail, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jail, "rootfs.ext4"), []byte("dock-disk-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.cfg.JailBase = filepath.Join(dir, "jail")
	driver.pauseErr = fc.ErrNoMicroVM
	return h
}

// withBase points the host's snapshots at a held current base fp.
func withBase(t *testing.T, h *Host, fp string) string {
	t.Helper()
	pointer := filepath.Join(t.TempDir(), "base.ext4")
	held := disk.BasePath(pointer, fp)
	if err := os.MkdirAll(filepath.Dir(held), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(held, []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := disk.SetCurrentBase(pointer, fp); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	h.cfg.Snaps.BaseImage = pointer
	return pointer
}

func TestSnapshotCreateFullWritesAWholeImageWithItsChecksum(t *testing.T) {
	ctx := context.Background()
	h := fullHost(t)
	// A base is configured, so an ordinary capture would be a delta.
	withBase(t, h, strings.Repeat("a", 64))

	id, size, _, err := h.SnapshotCreateFull(ctx, "dock-9", nil, apiv1.SnapshotFull, false, false)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := store.GetManifest(ctx, h.cfg.Snaps.Store, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 1 || manifest.Files[0].Path != "rootfs.ext4.zst" {
		t.Fatalf("files = %+v, want one rootfs.ext4.zst (never a delta)", manifest.Files)
	}
	rc, err := h.cfg.Snaps.Store.Get(ctx, id, "rootfs.ext4.zst")
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := io.ReadAll(rc)
	_ = rc.Close()
	sum := sha256.Sum256(stored)
	if got := h.GenerationSHA256(ctx, id); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("GenerationSHA256 = %q, want %x", got, sum)
	}
	if size != int64(len(stored)) || manifest.SizeBytes != size {
		t.Fatalf("size = %d, manifest sizeBytes = %d, stored = %d", size, manifest.SizeBytes, len(stored))
	}
	if got := h.GenerationBase(ctx, id); got != "" {
		t.Fatalf("a whole image must not name a base fingerprint, got %q", got)
	}
}

func TestGenerationImageNamesTheDockyardBase(t *testing.T) {
	ctx := context.Background()
	h := fullHost(t)
	fp := strings.Repeat("b", 64)
	pointer := withBase(t, h, fp)

	// Host-built base: no Dockyard image.
	id, _, _, err := h.SnapshotCreateFull(ctx, "dock-9", nil, apiv1.SnapshotFull, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.GenerationImage(ctx, id); got != "" {
		t.Fatalf("host-built base reported image %q", got)
	}
	// Pulled from Dockyard: reported by name.
	if err := disk.RecordBaseSource(pointer, fp, apiv1.DefaultImage, "2026.10"); err != nil {
		t.Fatal(err)
	}
	id, _, _, err = h.SnapshotCreateFull(ctx, "dock-9", nil, apiv1.SnapshotFull, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.GenerationImage(ctx, id); got != apiv1.DefaultImage {
		t.Fatalf("image = %q, want %q", got, apiv1.DefaultImage)
	}
}

func TestSnapshotCreateFullRefusesContainerDocks(t *testing.T) {
	h, _, _, _ := testContainerHost(t)
	ctx := context.Background()
	if _, _, _, err := h.SnapshotCreateFull(ctx, "dock-1", nil, apiv1.SnapshotFull, false, false); err == nil || !strings.Contains(err.Error(), "microvm") {
		t.Fatalf("err = %v, want a microvm-only refusal", err)
	}
	if h.GenerationImage(ctx, "x") != "" || h.GenerationSHA256(ctx, "x") != "" {
		t.Fatal("a container host reports no image or checksum")
	}
}
