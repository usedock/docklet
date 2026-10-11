package host

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/usedock/docklet/internal/disk"
	"github.com/usedock/docklet/internal/store"
)

// An imported dock whose manifest names a Dockyard image this host pulled reports that image when
// exported again, through the layout `docklet image pull` really writes (dock pull of a pushed dock).
func TestImportedDockReportsItsPulledDockyardImageOnExport(t *testing.T) {
	ctx := context.Background()
	h := fullHost(t)
	imageDir := t.TempDir()
	withImageDir(t, imageDir)
	// The host's default base is the Dockyard ubuntu-24.04; node-22 is pulled beside it.
	for name, marker := range map[string]byte{"ubuntu-24.04": 'U', "node-22": 'N'} {
		raw := rawImage()
		raw[0] = marker
		if _, err := newRelease(t, name, raw).puller(imageDir).Pull(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	pointer := filepath.Join(imageDir, "base.ext4")
	h.cfg.ImageBase = pointer
	h.cfg.Snaps.BaseImage = pointer

	upload := filepath.Join(t.TempDir(), "disk")
	if err := os.WriteFile(upload, []byte("uploaded-disk-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry, err := store.PutFileCompressed(ctx, h.cfg.Snaps.Store, "imp", "rootfs.ext4", upload, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// What the control plane writes at complete (wholeImageManifest).
	if err := store.PutManifest(ctx, h.cfg.Snaps.Store, store.Manifest{
		SnapshotID: "imp", DockID: "dock-9", Kind: "full", CPUFamily: "fam-1", Image: "node-22",
		Source: store.SourceImport, SHA256: entry.SHA256, SizeBytes: entry.Size,
		Files: []store.FileEntry{{Path: entry.Path, Size: entry.Size, Encoding: store.EncodingZstd, SHA256: entry.SHA256}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Provision(ctx, "dock-9", resSmall, "node-22", "imp"); err != nil {
		t.Fatal(err)
	}
	rootfs := filepath.Join(h.jailRoot("dock-9"), "rootfs.ext4")
	if got := diskImage(pointer, rootfs); got != "node-22" {
		t.Fatalf("restored dock image = %q, want node-22 (label %q)", got, readLabel(rootfs))
	}
	// The control plane lets a fleet host read only generations it owns or was told to load, so the
	// capture's own manifest cannot be read back.
	h.cfg.Snaps.Store = readOnlyImported{Store: h.cfg.Snaps.Store}
	id, _, _, err := h.SnapshotCreateFull(ctx, "dock-9", nil, "full", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.GenerationImage(ctx, id); got != "node-22" {
		t.Fatalf("exported image = %q, want node-22", got)
	}
}

// readOnlyImported reads back only the imported generation.
type readOnlyImported struct{ store.Store }

func (s readOnlyImported) Get(ctx context.Context, snapshotID, name string) (io.ReadCloser, error) {
	if snapshotID != "imp" {
		return nil, errors.New("generation_forbidden")
	}
	return s.Store.Get(ctx, snapshotID, name)
}

func diskImage(pointer, rootfs string) string { return disk.DockImage(pointer, rootfs) }

func readLabel(rootfs string) string {
	raw, _ := os.ReadFile(rootfs + ".base")
	return string(raw)
}
