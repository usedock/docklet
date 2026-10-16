package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	if got, err := h.GenerationSHA256(ctx, id); err != nil || got != hex.EncodeToString(sum[:]) {
		t.Fatalf("GenerationSHA256 = %q (%v), want %x", got, err, sum)
	}
	if size != int64(len(stored)) || manifest.SizeBytes != size {
		t.Fatalf("size = %d, manifest sizeBytes = %d, stored = %d", size, manifest.SizeBytes, len(stored))
	}
	if got, err := h.GenerationDataBytes(ctx, id); err != nil || got <= 0 {
		t.Fatalf("GenerationDataBytes = %d (%v), want the disk's allocated bytes", got, err)
	}
	if got, err := h.GenerationBase(ctx, id); err != nil || got != "" {
		t.Fatalf("a whole image must not name a base fingerprint, got %q (%v)", got, err)
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
	if got, err := h.GenerationImage(ctx, id); err != nil || got != "" {
		t.Fatalf("host-built base reported image %q (%v)", got, err)
	}
	// Pulled from Dockyard: reported by name.
	if err := disk.RecordBaseSource(pointer, fp, apiv1.DefaultImage, "2026.10"); err != nil {
		t.Fatal(err)
	}
	id, _, _, err = h.SnapshotCreateFull(ctx, "dock-9", nil, apiv1.SnapshotFull, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := h.GenerationImage(ctx, id); err != nil || got != apiv1.DefaultImage {
		t.Fatalf("image = %q (%v), want %q", got, err, apiv1.DefaultImage)
	}
}

func TestSnapshotCreateFullExportsAContainerDocksWorkTar(t *testing.T) {
	ctx := context.Background()
	h, driver, _, _ := testContainerHost(t)
	var pausedDuringSave bool
	driver.saveFn = func() { pausedDuringSave = driver.paused == 1 && driver.resumed == 0 }

	id, size, _, err := h.SnapshotCreateFull(ctx, "dock-1", nil, apiv1.SnapshotFull, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !pausedDuringSave || driver.paused != 1 || driver.resumed != 1 {
		t.Fatalf("pausedDuringSave=%v paused=%d resumed=%d, want a pause around SaveState and one resume", pausedDuringSave, driver.paused, driver.resumed)
	}
	manifest, err := store.GetManifest(ctx, h.cfg.Snaps.Store, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 1 || manifest.Files[0].Path != "state.tar.zst" {
		t.Fatalf("files = %+v, want one state.tar.zst", manifest.Files)
	}
	rc, err := h.cfg.Snaps.Store.Get(ctx, id, "state.tar.zst")
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := io.ReadAll(rc)
	_ = rc.Close()
	sum := sha256.Sum256(stored)
	if got, err := h.GenerationSHA256(ctx, id); err != nil || got != hex.EncodeToString(sum[:]) {
		t.Fatalf("GenerationSHA256 = %q (%v), want the stored object's %x", got, err, sum)
	}
	if size != int64(len(stored)) || manifest.SizeBytes != size {
		t.Fatalf("size = %d, manifest sizeBytes = %d, stored = %d", size, manifest.SizeBytes, len(stored))
	}
	// The fake tar holds one 8-byte member: 512 header + 512 padded data + 1024 end blocks.
	if got, err := h.GenerationDataBytes(ctx, id); err != nil || got != 2048 || manifest.DataBytes != 2048 {
		t.Fatalf("GenerationDataBytes = %d (%v), want the raw tar size 2048", got, err)
	}
	if got, err := h.GenerationImage(ctx, id); err != nil || got != apiv1.DefaultImage {
		t.Fatalf("GenerationImage = %q (%v), want %q", got, err, apiv1.DefaultImage)
	}
	if got, err := h.GenerationBase(ctx, id); err != nil || got != "" {
		t.Fatalf("GenerationBase = %q (%v), want none", got, err)
	}
}

func TestSnapshotCreateFullResumesAfterAFailedCapture(t *testing.T) {
	ctx := context.Background()
	h, driver, _, _ := testContainerHost(t)
	driver.saveErr = errors.New("tar failed")
	if _, _, _, err := h.SnapshotCreateFull(ctx, "dock-1", nil, apiv1.SnapshotFull, false, false); err == nil {
		t.Fatal("want the tar failure")
	}
	if driver.paused != 1 || driver.resumed != 1 {
		t.Fatalf("paused=%d resumed=%d, a failed capture must thaw the container", driver.paused, driver.resumed)
	}
}

func TestSnapshotCreateFullResumesWhenCancelledAndOnPanic(t *testing.T) {
	h, driver, _, _ := testContainerHost(t)
	ctx, cancel := context.WithCancel(context.Background())
	driver.saveFn = cancel
	driver.saveErr = context.Canceled
	_, _, _, _ = h.SnapshotCreateFull(ctx, "dock-1", nil, apiv1.SnapshotFull, true, false)
	if driver.paused != 1 || driver.resumed != 1 {
		t.Fatalf("cancelled: paused=%d resumed=%d", driver.paused, driver.resumed)
	}
	driver.saveErr = nil
	driver.saveFn = func() { panic("boom") }
	func() {
		defer func() { _ = recover() }()
		_, _, _, _ = h.SnapshotCreateFull(context.Background(), "dock-1", nil, apiv1.SnapshotFull, true, false)
	}()
	if driver.paused != 2 || driver.resumed != 2 {
		t.Fatalf("panic: paused=%d resumed=%d", driver.paused, driver.resumed)
	}
}

func TestSnapshotCreateFullSkipsThePauseForAStoppedDock(t *testing.T) {
	ctx := context.Background()
	h, driver, _, _ := testContainerHost(t)
	driver.pauseErr = fmt.Errorf("%w: dock-1", fc.ErrNoMicroVM)
	id, _, _, err := h.SnapshotCreateFull(ctx, "dock-1", nil, apiv1.SnapshotFull, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if driver.resumed != 0 || driver.saved != 1 {
		t.Fatalf("resumed=%d saved=%d, a stopped dock is captured without a thaw", driver.resumed, driver.saved)
	}
	if sum, err := h.GenerationSHA256(ctx, id); err != nil || sum == "" {
		t.Fatalf("sha = %q (%v)", sum, err)
	}
}

func TestSnapshotCreateFullFailsWhenThePauseFails(t *testing.T) {
	h, driver, _, _ := testContainerHost(t)
	driver.pauseErr = errors.New("docker pause: boom")
	if _, _, _, err := h.SnapshotCreateFull(context.Background(), "dock-1", nil, apiv1.SnapshotFull, false, false); err == nil || driver.saved != 0 {
		t.Fatalf("err = %v saved = %d, an inconsistent tar must not be taken", err, driver.saved)
	}
}

func TestOrdinaryContainerCaptureDoesNotPause(t *testing.T) {
	h, driver, _, _ := testContainerHost(t)
	id, _, _, err := h.SnapshotCreate(context.Background(), "dock-1", nil, apiv1.SnapshotFull, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if driver.paused != 0 {
		t.Fatalf("paused = %d, the stop-time capture keeps its behaviour", driver.paused)
	}
	manifest, _ := store.GetManifest(context.Background(), h.cfg.Snaps.Store, id)
	if manifest.SHA256 != "" || manifest.SizeBytes != 0 {
		t.Fatalf("an ordinary container capture must not claim a checksum: %+v", manifest)
	}
}

func TestSnapshotCreateFullRetriesAFailedThaw(t *testing.T) {
	h, driver, _, _ := testContainerHost(t)
	writeJailSidecar(t, h, "dock-1", resSmall)
	marker := filepath.Join(h.jailRoot("dock-1"), exportPausedMarker)
	var markedDuringSave bool
	driver.saveFn = func() { _, err := os.Stat(marker); markedDuringSave = err == nil }
	driver.resumeErrs = []error{errors.New("docker unpause: transient")}
	if _, _, _, err := h.SnapshotCreateFull(context.Background(), "dock-1", nil, apiv1.SnapshotFull, true, false); err != nil {
		t.Fatalf("a thaw that succeeds on retry fails the capture: %v", err)
	}
	if driver.resumed != 1 || !markedDuringSave {
		t.Fatalf("resumed=%d markedDuringSave=%v, want the retried thaw and a marker while paused", driver.resumed, markedDuringSave)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker left after a thaw: %v", err)
	}
	// Every thaw failing: the capture fails and the marker stays for Rescan.
	driver.resumeErrs = []error{errors.New("a"), errors.New("b"), errors.New("c")}
	if _, _, _, err := h.SnapshotCreateFull(context.Background(), "dock-1", nil, apiv1.SnapshotFull, true, false); err == nil {
		t.Fatal("want the thaw failure surfaced")
	}
	if len(driver.resumeErrs) != 0 {
		t.Fatalf("thaw attempts left %d errors unused, want three attempts", len(driver.resumeErrs))
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker gone although the dock is still frozen: %v", err)
	}
}

func TestRescanThawsAContainerAnInterruptedExportFroze(t *testing.T) {
	h, driver, _, _ := testContainerHost(t)
	writeJailSidecar(t, h, "dock-1", resSmall)
	writeJailSidecar(t, h, "dock-2", resSmall)
	marker := filepath.Join(h.jailRoot("dock-1"), exportPausedMarker)
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if adopted := h.Rescan(); len(adopted) != 2 {
		t.Fatalf("adopted = %v", adopted)
	}
	if driver.resumed != 1 {
		t.Fatalf("resumed = %d, want only the dock the export left paused thawed", driver.resumed)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker left after the thaw: %v", err)
	}
}
