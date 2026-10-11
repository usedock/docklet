package snapmgr

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

type fakeDriver struct{ snapshots int }

func (f *fakeDriver) Boot(_ context.Context, _ fc.BootSpec) (*fc.Handle, error) {
	return &fc.Handle{}, nil
}
func (f *fakeDriver) Pause(_ context.Context, _ *fc.Handle) error  { return nil }
func (f *fakeDriver) Resume(_ context.Context, _ *fc.Handle) error { return nil }
func (f *fakeDriver) Adopt(_ string, _ string, _ apiv1.Resources) (*fc.Handle, error) {
	return &fc.Handle{}, nil
}
func (f *fakeDriver) Terminate(_ *fc.Handle) error { return nil }
func (f *fakeDriver) CreateSnapshot(_ context.Context, h *fc.Handle, _ string) error {
	f.snapshots++
	memPath, snapPath := f.SnapshotPaths(h)
	if err := os.MkdirAll(filepath.Dir(memPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(memPath, []byte("mem"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(snapPath, []byte("state"), 0o644)
}
func (f *fakeDriver) SnapshotPaths(h *fc.Handle) (string, string) {
	dir := filepath.Join(os.TempDir(), "snaptest", h.DockID)
	return filepath.Join(dir, "mem"), filepath.Join(dir, "state")
}

func coordinator(t *testing.T) *Coordinator {
	t.Helper()
	return &Coordinator{
		Driver:     &fakeDriver{},
		Store:      store.NewMemStore(),
		VMsDir:     t.TempDir(),
		GensDir:    t.TempDir(),
		HostFamily: "GenuineIntel:142:10",
		Runner: func(name string, args ...string) error {
			if name != "cp" {
				return errors.New("unexpected " + name)
			}
			raw, err := os.ReadFile(args[len(args)-2])
			if err != nil {
				return err
			}
			return os.WriteFile(args[len(args)-1], raw, 0o644)
		},
	}
}

func TestFreezeAndPrepare(t *testing.T) {
	ctx := context.Background()
	c := coordinator(t)
	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("disk-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := c.FreezeRootfs(ctx, "d1", rootfs, "s1", "full", "GenuineIntel:142:10", nil)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Files[0].Encoding != store.EncodingZstd || manifest.Files[0].Size <= 0 {
		t.Fatalf("manifest = %+v", manifest)
	}
	genPath, err := c.PrepareResume(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(genPath)
	if string(raw) != "disk-bytes" {
		t.Fatalf("generation = %q", raw)
	}
}

func TestPrepareRefusesCrossFamily(t *testing.T) {
	ctx := context.Background()
	c := coordinator(t)
	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.FreezeRootfs(ctx, "d1", rootfs, "s9", "full", "AuthenticAMD:1:1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PrepareResume(ctx, "s9"); err == nil {
		t.Fatal("cross-family restore must refuse")
	} else {
		var incompat *ErrIncompatible
		if !errors.As(err, &incompat) || incompat.Family != "AuthenticAMD:1:1" {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestCaptureMemoryUploads(t *testing.T) {
	ctx := context.Background()
	c := coordinator(t)
	if err := c.CaptureMemory(ctx, &fc.Handle{DockID: "d1"}, "s2", "Diff"); err != nil {
		t.Fatal(err)
	}
	listed, err := c.Store.List(ctx, "s2")
	if err != nil || len(listed) != 2 {
		t.Fatalf("list = %+v,%v", listed, err)
	}
}

// image writes a deterministic raw image of the given size, matching the
// fixtures the disk package uses so deltas exercise real block comparisons.
func image(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	block := make([]byte, disk.BlockSize)
	for i := range block {
		block[i] = byte(i)
	}
	for off := int64(0); off < size; off += int64(len(block)) {
		n := int64(len(block))
		if remaining := size - off; remaining < n {
			n = remaining
		}
		if _, err := f.Write(block[:n]); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFreezeRootfsWritesDeltaAgainstTheBase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.ext4")
	rootfs := filepath.Join(dir, "rootfs.ext4")
	image(t, base, 4*disk.BlockSize)
	if err := os.WriteFile(rootfs, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfs, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// One block of guest work: a delta must carry that block, not the image.
	f, err := os.OpenFile(rootfs, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("guest change"), 2*disk.BlockSize); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	c := coordinator(t)
	c.BaseImage = base
	manifest, err := c.FreezeRootfs(ctx, "d1", rootfs, "s1", "full", "GenuineIntel:142:10", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 1 || manifest.Files[0].Path != "rootfs.delta.zst" {
		t.Fatalf("manifest files = %+v, want a delta artifact", manifest.Files)
	}
	// The base identity travels with the generation, or a restore on another
	// host could apply the delta to the wrong image.
	if manifest.BaseFingerprint == "" {
		t.Fatal("manifest is missing the base fingerprint")
	}
	if manifest.ImageSize != 4*disk.BlockSize {
		t.Fatalf("image size = %d, want %d", manifest.ImageSize, 4*disk.BlockSize)
	}
	// A one-block change must not ship the whole image.
	if manifest.Files[0].Size >= 3*disk.BlockSize {
		t.Fatalf("delta is %d bytes, want far less than the image", manifest.Files[0].Size)
	}
}

func TestMaterializeDiskRebuildsTheRootfsFromBaseAndDelta(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.ext4")
	rootfs := filepath.Join(dir, "rootfs.ext4")
	image(t, base, 3*disk.BlockSize)
	raw, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfs, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(rootfs, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("dock state"), 1*disk.BlockSize); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	c := coordinator(t)
	c.BaseImage = base
	if _, err := c.FreezeRootfs(ctx, "d1", rootfs, "s1", "full", "GenuineIntel:142:10", nil); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(dir, "restored.ext4")
	if err := c.MaterializeDisk(ctx, "s1", restored); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(rootfs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(restored)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("restored %d bytes, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("restored image differs at byte %d", i)
		}
	}
}

func TestMaterializeDiskRefusesADeltaFromAnotherBase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.ext4")
	other := filepath.Join(dir, "other.ext4")
	rootfs := filepath.Join(dir, "rootfs.ext4")
	image(t, base, 2*disk.BlockSize)
	image(t, other, 2*disk.BlockSize)
	// The two bases must actually differ, or this test would pass for the
	// wrong reason (identical content legitimately shares a fingerprint).
	of, err := os.OpenFile(other, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := of.WriteAt([]byte("different base"), 0); err != nil {
		t.Fatal(err)
	}
	_ = of.Close()
	raw, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfs, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	c := coordinator(t)
	c.BaseImage = base
	if _, err := c.FreezeRootfs(ctx, "d1", rootfs, "s1", "full", "GenuineIntel:142:10", nil); err != nil {
		t.Fatal(err)
	}
	// A host whose base image differs must refuse rather than write corrupt
	// blocks: silently applying the wrong delta is worse than failing.
	c.BaseImage = other
	err = c.MaterializeDisk(ctx, "s1", filepath.Join(dir, "restored.ext4"))
	if !errors.Is(err, disk.ErrBaseUnavailable) {
		t.Fatalf("err = %v, want a base unavailable refusal", err)
	}
}

// addBase installs a distinct image content-addressed beside pointer.
func addBase(t *testing.T, pointer string, marker byte) string {
	t.Helper()
	staged := filepath.Join(t.TempDir(), "staged.ext4")
	image(t, staged, 3*disk.BlockSize)
	raw, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] = marker
	sum := sha256.Sum256(raw)
	fp := hex.EncodeToString(sum[:])
	dest := disk.BasePath(pointer, fp)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return fp
}

// dockOnBase provisions a rootfs from the current base and dirties one block.
func dockOnBase(t *testing.T, c *Coordinator, rootfs string) {
	t.Helper()
	if err := disk.ProvisionFromBase(c.Runner, c.BaseImage, rootfs); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(rootfs, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("dock state"), disk.BlockSize); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

func TestRebuiltBaseKeepsOlderGenerationsRestorable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := coordinator(t)
	c.BaseImage = filepath.Join(dir, "images", "base.ext4")
	old := addBase(t, c.BaseImage, 1)
	if err := disk.SetCurrentBase(c.BaseImage, old); err != nil {
		t.Fatal(err)
	}
	rootfs := filepath.Join(dir, "jail", "rootfs.ext4")
	dockOnBase(t, c, rootfs)
	first, err := c.FreezeRootfs(ctx, "d1", rootfs, "s1", "full", "GenuineIntel:142:10", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.BaseFingerprint != old {
		t.Fatalf("base = %q, want %q", first.BaseFingerprint, old)
	}
	// A rebuild installs a new base and flips the pointer; the old one stays.
	rebuilt := addBase(t, c.BaseImage, 2)
	if err := disk.SetCurrentBase(c.BaseImage, rebuilt); err != nil {
		t.Fatal(err)
	}
	// The running dock keeps diffing against the base it was provisioned from.
	second, err := c.FreezeRootfs(ctx, "d1", rootfs, "s2", "full", "GenuineIntel:142:10", nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.BaseFingerprint != old {
		t.Fatalf("capture after rebuild diffed against %q, want the dock's own base %q", second.BaseFingerprint, old)
	}
	if got := c.GenerationBase(ctx, "s2"); got != old {
		t.Fatalf("generation base = %q, want %q", got, old)
	}
	restored := filepath.Join(dir, "restored", "rootfs.ext4")
	if err := c.MaterializeDisk(ctx, "s1", restored); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(rootfs)
	got, _ := os.ReadFile(restored)
	if string(got) != string(want) {
		t.Fatal("restore onto the non-current base differs from the captured rootfs")
	}
	if label := disk.BaseLabel(restored); label != old {
		t.Fatalf("restored label = %q, want %q", label, old)
	}
}

func TestMaterializeDiskRefusesBeforeDownloadWhenTheBaseIsMissing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := coordinator(t)
	c.BaseImage = filepath.Join(dir, "a", "base.ext4")
	fp := addBase(t, c.BaseImage, 1)
	if err := disk.SetCurrentBase(c.BaseImage, fp); err != nil {
		t.Fatal(err)
	}
	rootfs := filepath.Join(dir, "jail", "rootfs.ext4")
	dockOnBase(t, c, rootfs)
	if _, err := c.FreezeRootfs(ctx, "d1", rootfs, "s1", "full", "GenuineIntel:142:10", nil); err != nil {
		t.Fatal(err)
	}
	// Another host sharing the store but holding a different base.
	other := coordinator(t)
	other.Store = c.Store
	other.BaseImage = filepath.Join(dir, "b", "base.ext4")
	if err := disk.SetCurrentBase(other.BaseImage, addBase(t, other.BaseImage, 9)); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(dir, "restored.ext4")
	err := other.MaterializeDisk(ctx, "s1", restored)
	if !errors.Is(err, disk.ErrBaseUnavailable) || !strings.Contains(err.Error(), fp) {
		t.Fatalf("err = %v, want ErrBaseUnavailable naming %s", err, fp)
	}
	if _, err := os.Stat(other.GenerationPath("s1", "rootfs.delta")); !os.IsNotExist(err) {
		t.Fatalf("delta must not be downloaded before the base check, stat err = %v", err)
	}
	if _, err := os.Stat(restored); !os.IsNotExist(err) {
		t.Fatalf("nothing may be written on refusal, stat err = %v", err)
	}
}

func TestFreezeContainerStateKeepsWorkspaceState(t *testing.T) {
	ctx := context.Background()
	c := coordinator(t)
	state := filepath.Join(t.TempDir(), "state.tar")
	if err := os.WriteFile(state, []byte("workspace-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := c.FreezeContainerState(ctx, "d1", state, "s9", "full", "GenuineIntel:142:10", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 1 || manifest.Files[0].Path != "state.tar.zst" {
		t.Fatalf("manifest = %+v, want one workspace artifact", manifest)
	}
	if _, err := c.PrepareResume(ctx, "s9"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(c.GenerationPath("s9", "state.tar"))
	if string(raw) != "workspace-bytes" {
		t.Fatalf("generation = %q", raw)
	}
}

func TestPrune(t *testing.T) {
	ctx := context.Background()
	c := coordinator(t)
	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.FreezeRootfs(ctx, "d1", rootfs, "s3", "full", "f", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Prune(ctx, "s3"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetManifest(ctx, c.Store, "s3"); err == nil {
		t.Fatal("pruned generation must be gone from the store")
	}
}

func TestPrepareResumeReplacesAPartialGeneration(t *testing.T) {
	ctx := context.Background()
	c := coordinator(t)
	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("disk-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.FreezeRootfs(ctx, "d1", rootfs, "s1", "full", "GenuineIntel:142:10", nil); err != nil {
		t.Fatal(err)
	}
	genPath := filepath.Join(c.GensDir, "s1", "rootfs.ext4")
	if err := os.WriteFile(genPath, []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := c.PrepareResume(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(got); string(raw) != "disk-bytes" {
		t.Fatalf("generation = %q, want the full image", raw)
	}
}

func TestMaterializeDiskRemovesAPartiallyPatchedDestOnError(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.ext4")
	rootfs := filepath.Join(dir, "rootfs.ext4")
	image(t, base, 2*disk.BlockSize)
	raw, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	raw[10] ^= 0xff
	if err := os.WriteFile(rootfs, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	c := coordinator(t)
	c.BaseImage = base
	if _, err := c.FreezeRootfs(ctx, "d1", rootfs, "s1", "full", "GenuineIntel:142:10", nil); err != nil {
		t.Fatal(err)
	}
	// Same-size corruption: the trailer's record count no longer matches.
	deltaPath := c.GenerationPath("s1", "rootfs.delta")
	delta, err := os.ReadFile(deltaPath)
	if err != nil {
		t.Fatal(err)
	}
	delta[len(delta)-12] ^= 0x7f
	if err := os.WriteFile(deltaPath, delta, 0o644); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(dir, "restored.ext4")
	if err := c.MaterializeDisk(ctx, "s1", restored); err == nil {
		t.Fatal("corrupt delta must fail")
	}
	if _, err := os.Stat(restored); !os.IsNotExist(err) {
		t.Fatalf("partially patched dest must be removed, stat err = %v", err)
	}
}

// A rootfs grown past its base by a dock's diskGiB still captures and restores.
func TestFreezeRootfsCapturesADiskGrownPastTheBase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.ext4")
	rootfs := filepath.Join(dir, "rootfs.ext4")
	image(t, base, 2*disk.BlockSize)
	raw, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfs, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(rootfs, 4*disk.BlockSize); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(rootfs, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("grown state"), 3*disk.BlockSize); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	c := coordinator(t)
	c.BaseImage = base
	manifest, err := c.FreezeRootfs(ctx, "d1", rootfs, "s1", "full", "GenuineIntel:142:10", nil)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ImageSize != 4*disk.BlockSize {
		t.Fatalf("manifest image size = %d, want %d", manifest.ImageSize, 4*disk.BlockSize)
	}
	restored := filepath.Join(dir, "restored.ext4")
	if err := c.MaterializeDisk(ctx, "s1", restored); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(rootfs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(restored)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("restored %d bytes differ from the grown rootfs (%d bytes)", len(got), len(want))
	}
}

// fullFixture returns a coordinator with a Dockyard-recorded current base and a
// dock rootfs that differs from it by one block.
func fullFixture(t *testing.T) (c *Coordinator, rootfs, base, fp string) {
	t.Helper()
	dir := t.TempDir()
	pointer := filepath.Join(dir, "base.ext4")
	fp = addBase(t, pointer, 7)
	if err := disk.SetCurrentBase(pointer, fp); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := disk.RecordBaseSource(pointer, fp, "ubuntu-24.04", "2026.10"); err != nil {
		t.Fatal(err)
	}
	base = disk.BasePath(pointer, fp)
	rootfs = filepath.Join(dir, "rootfs.ext4")
	raw, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	raw[disk.BlockSize+3] ^= 0xff
	if err := os.WriteFile(rootfs, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	c = coordinator(t)
	c.BaseImage = pointer
	return c, rootfs, base, fp
}

func TestFreezeRootfsFullWritesAWholeChecksummedImage(t *testing.T) {
	ctx := context.Background()
	c, rootfs, _, _ := fullFixture(t)
	manifest, err := c.FreezeRootfsFull(ctx, "d1", rootfs, "s1", "full", "GenuineIntel:142:10", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Even with a base configured a full capture is never a delta.
	if len(manifest.Files) != 1 || manifest.Files[0].Path != "rootfs.ext4.zst" {
		t.Fatalf("files = %+v, want rootfs.ext4.zst", manifest.Files)
	}
	if manifest.BaseFingerprint != "" {
		t.Fatalf("a whole image must not pin a base fingerprint: %q", manifest.BaseFingerprint)
	}
	rc, err := c.Store.Get(ctx, "s1", "rootfs.ext4.zst")
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := io.ReadAll(rc)
	_ = rc.Close()
	sum := sha256.Sum256(stored)
	if manifest.SHA256 != hex.EncodeToString(sum[:]) || manifest.SizeBytes != int64(len(stored)) {
		t.Fatalf("manifest sha256/size = %s/%d, stored object is %x/%d", manifest.SHA256, manifest.SizeBytes, sum, len(stored))
	}
	// The persisted manifest carries the same fields (what the control plane reads).
	persisted, err := store.GetManifest(ctx, c.Store, "s1")
	if err != nil || persisted.SHA256 != manifest.SHA256 || persisted.SizeBytes != manifest.SizeBytes {
		t.Fatalf("persisted manifest = %+v, %v", persisted, err)
	}
	if c.GenerationSHA256(ctx, "s1") != manifest.SHA256 {
		t.Fatal("GenerationSHA256 disagrees with the manifest")
	}
	if _, err := os.Stat(filepath.Join(c.GensDir, "s1", "rootfs.ext4")); !os.IsNotExist(err) {
		t.Fatalf("the local copy of an export must be dropped: %v", err)
	}
	// It restores to exactly the dock's disk.
	restored := filepath.Join(t.TempDir(), "restored.ext4")
	if err := c.MaterializeDisk(ctx, "s1", restored); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(rootfs)
	got, _ := os.ReadFile(restored)
	if !bytes.Equal(got, want) {
		t.Fatal("restored disk differs from the dock disk")
	}
}

func TestFreezeRootfsFullReportsTheDockyardBaseImage(t *testing.T) {
	ctx := context.Background()
	c, rootfs, _, _ := fullFixture(t)
	manifest, err := c.FreezeRootfsFull(ctx, "d1", rootfs, "s1", "full", "GenuineIntel:142:10", nil)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Image != "ubuntu-24.04" || c.GenerationImage(ctx, "s1") != "ubuntu-24.04" {
		t.Fatalf("image = %q / %q, want ubuntu-24.04", manifest.Image, c.GenerationImage(ctx, "s1"))
	}
	// A dock labeled with a base the host built itself reports none.
	built := addBase(t, c.BaseImage, 9)
	if err := disk.LabelBase(rootfs, built); err != nil {
		t.Fatal(err)
	}
	manifest, err = c.FreezeRootfsFull(ctx, "d1", rootfs, "s2", "full", "GenuineIntel:142:10", nil)
	if err != nil || manifest.Image != "" {
		t.Fatalf("host-built base image = %q, %v; want empty", manifest.Image, err)
	}
	// No base configured at all (whole-image host): nothing to report, still checksummed.
	bare := coordinator(t)
	manifest, err = bare.FreezeRootfsFull(ctx, "d1", rootfs, "s3", "full", "GenuineIntel:142:10", nil)
	if err != nil || manifest.Image != "" || manifest.SHA256 == "" {
		t.Fatalf("no-base capture = %+v, %v", manifest, err)
	}
}

// tamper rewrites the stored object of a snapshot with flipped bytes of the same length.
func tamper(t *testing.T, c *Coordinator, snapshotID, name string) {
	t.Helper()
	ctx := context.Background()
	rc, err := c.Store.Get(ctx, snapshotID, name)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(rc)
	_ = rc.Close()
	raw[len(raw)/2] ^= 0x55
	if err := c.Store.Put(ctx, snapshotID, name, bytes.NewReader(raw), int64(len(raw))); err != nil {
		t.Fatal(err)
	}
}

func TestMaterializeDiskRefusesATamperedWholeImage(t *testing.T) {
	ctx := context.Background()
	c, rootfs, _, _ := fullFixture(t)
	if _, err := c.FreezeRootfsFull(ctx, "d1", rootfs, "s1", "full", "GenuineIntel:142:10", nil); err != nil {
		t.Fatal(err)
	}
	tamper(t, c, "s1", "rootfs.ext4.zst")
	dest := filepath.Join(t.TempDir(), "restored.ext4")
	err := c.MaterializeDisk(ctx, "s1", dest)
	if err == nil {
		t.Fatalf("err = %v, want a refusal of the tampered object", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("a refused restore left a disk behind: %v", statErr)
	}
	if leftovers, _ := filepath.Glob(dest + "*"); len(leftovers) != 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
}

func writeTemp(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// controlPlaneManifest writes a whole-image manifest the way an import does: only the
// manifest-level sha256/sizeBytes/image and a bare file entry.
func controlPlaneManifest(t *testing.T, c *Coordinator, snapshotID string, mutate func(*store.Manifest)) {
	t.Helper()
	ctx := context.Background()
	entry, err := store.PutFileCompressed(ctx, c.Store, snapshotID, "rootfs.ext4", writeTemp(t, []byte("imported-disk")), c.GensDir)
	if err != nil {
		t.Fatal(err)
	}
	m := store.Manifest{
		SnapshotID: snapshotID, DockID: "d1", Kind: "full", CPUFamily: "GenuineIntel:142:10",
		Files:  []store.FileEntry{{Path: entry.Path}},
		SHA256: entry.SHA256, SizeBytes: entry.Size, Image: "ubuntu-24.04",
	}
	mutate(&m)
	if err := store.PutManifest(ctx, c.Store, m); err != nil {
		t.Fatal(err)
	}
}

func TestMaterializeDiskVerifiesManifestLevelChecksum(t *testing.T) {
	ctx := context.Background()
	c := coordinator(t)

	t.Run("good", func(t *testing.T) {
		controlPlaneManifest(t, c, "good", func(*store.Manifest) {})
		dest := filepath.Join(t.TempDir(), "restored.ext4")
		if err := c.MaterializeDisk(ctx, "good", dest); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(dest); string(got) != "imported-disk" {
			t.Fatalf("restored = %q", got)
		}
	})
	t.Run("wrong sha256", func(t *testing.T) {
		controlPlaneManifest(t, c, "badsum", func(m *store.Manifest) { m.SHA256 = strings.Repeat("ab", 32) })
		d := filepath.Join(t.TempDir(), "r.ext4")
		if err := c.MaterializeDisk(ctx, "badsum", d); err == nil || !strings.Contains(err.Error(), "sha256") {
			t.Fatalf("err = %v, want a sha256 refusal", err)
		}
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Fatalf("partial disk left: %v", err)
		}
	})
	t.Run("wrong size", func(t *testing.T) {
		controlPlaneManifest(t, c, "badsize", func(m *store.Manifest) { m.SizeBytes++ })
		d := filepath.Join(t.TempDir(), "r.ext4")
		if err := c.MaterializeDisk(ctx, "badsize", d); err == nil {
			t.Fatal("a size mismatch must be refused")
		}
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Fatalf("partial disk left: %v", err)
		}
	})
	t.Run("manifest disagrees with its entry", func(t *testing.T) {
		controlPlaneManifest(t, c, "conflict", func(m *store.Manifest) {
			m.Files[0].SHA256 = strings.Repeat("cd", 32)
		})
		if err := c.MaterializeDisk(ctx, "conflict", filepath.Join(t.TempDir(), "r.ext4")); err == nil || !strings.Contains(err.Error(), "disagrees") {
			t.Fatalf("err = %v, want a disagreement refusal", err)
		}
	})
	t.Run("no checksum restores as before", func(t *testing.T) {
		// Legacy generations record neither value at manifest level: nothing to verify.
		controlPlaneManifest(t, c, "legacy", func(m *store.Manifest) { m.SHA256, m.SizeBytes, m.Image = "", 0, "" })
		rc, err := c.Store.Get(ctx, "legacy", "rootfs.ext4.zst")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(rc)
		_ = rc.Close()
		m, _ := store.GetManifest(ctx, c.Store, "legacy")
		m.Files[0].Size = int64(len(raw))
		if err := store.PutManifest(ctx, c.Store, m); err != nil {
			t.Fatal(err)
		}
		if err := c.MaterializeDisk(ctx, "legacy", filepath.Join(t.TempDir(), "r.ext4")); err != nil {
			t.Fatal(err)
		}
	})
}

func TestMaterializeDiskLabelsAnImportedDiskWithItsDockyardBase(t *testing.T) {
	ctx := context.Background()
	c, _, _, fp := fullFixture(t)
	controlPlaneManifest(t, c, "imp", func(*store.Manifest) {})
	dest := filepath.Join(t.TempDir(), "restored.ext4")
	if err := c.MaterializeDisk(ctx, "imp", dest); err != nil {
		t.Fatal(err)
	}
	if got := disk.BaseLabel(dest); got != fp {
		t.Fatalf("label = %q, want the held ubuntu-24.04 base %q", got, fp)
	}
	if got := disk.DockImage(c.BaseImage, dest); got != "ubuntu-24.04" {
		t.Fatalf("dock image = %q", got)
	}
}


// importedManifest stores raw as the control plane records a dock push (dock#122): source import,
// manifest-level sha256/sizeBytes/image, the entry's sha256 and size, and the cpuFamily of the host
// it placed the import on.
func importedManifest(t *testing.T, c *Coordinator, snapshotID string, raw []byte, mutate func(*store.Manifest)) {
	t.Helper()
	ctx := context.Background()
	entry, err := store.PutFileCompressed(ctx, c.Store, snapshotID, "rootfs.ext4", writeTemp(t, raw), c.GensDir)
	if err != nil {
		t.Fatal(err)
	}
	m := store.Manifest{
		SnapshotID: snapshotID, DockID: "imp_1", Kind: "full", CPUFamily: c.HostFamily, Image: "ubuntu-24.04",
		Source: store.SourceImport, SHA256: entry.SHA256, SizeBytes: entry.Size,
		Files: []store.FileEntry{{Path: entry.Path, Size: entry.Size, Encoding: store.EncodingZstd, SHA256: entry.SHA256}},
	}
	mutate(&m)
	if err := store.PutManifest(ctx, c.Store, m); err != nil {
		t.Fatal(err)
	}
}

func TestMaterializeDiskRestoresAnImportedDisk(t *testing.T) {
	ctx := context.Background()
	c := coordinator(t)
	importedManifest(t, c, "up", []byte("uploaded-disk"), func(*store.Manifest) {})
	dest := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := c.MaterializeDiskWithin(ctx, "up", dest, 1<<30); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "uploaded-disk" {
		t.Fatalf("restored = %q", got)
	}
	// The CPU family check stays: the control plane writes the placed host's family.
	importedManifest(t, c, "other", []byte("x"), func(m *store.Manifest) { m.CPUFamily = "AuthenticAMD:1:1" })
	var inc *ErrIncompatible
	if err := c.MaterializeDiskWithin(ctx, "other", filepath.Join(t.TempDir(), "r"), 1<<30); !errors.As(err, &inc) {
		t.Fatalf("err = %v, want ErrIncompatible", err)
	}
}

func TestMaterializeDiskRefusesAnImportWithoutManifestChecksumOrSize(t *testing.T) {
	ctx := context.Background()
	c := coordinator(t)
	for name, mutate := range map[string]func(*store.Manifest){
		"no sha256":    func(m *store.Manifest) { m.SHA256 = "" },
		"no sizeBytes": func(m *store.Manifest) { m.SizeBytes = 0 },
		// The entry still carries both: an import is judged by its manifest-level fields alone.
		"neither": func(m *store.Manifest) { m.SHA256, m.SizeBytes = "", 0 },
		"a delta": func(m *store.Manifest) { m.Files[0].Path = "rootfs.delta.zst" },
	} {
		t.Run(name, func(t *testing.T) {
			importedManifest(t, c, "bad", []byte("uploaded-disk"), mutate)
			dest := filepath.Join(t.TempDir(), "rootfs.ext4")
			if err := c.MaterializeDiskWithin(ctx, "bad", dest, 1<<30); err == nil || !strings.Contains(err.Error(), "imported disk") {
				t.Fatalf("err = %v, want an import refusal", err)
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatalf("a refused import left a disk: %v", err)
			}
		})
	}
	// A generation naming a Dockyard image is never legacy either.
	importedManifest(t, c, "named", []byte("d"), func(m *store.Manifest) {
		m.Source, m.SHA256, m.SizeBytes, m.Files[0].SHA256 = "", "", 0, ""
	})
	if err := c.MaterializeDisk(ctx, "named", filepath.Join(t.TempDir(), "r")); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("err = %v, want a refusal of an image-named generation without a sha256", err)
	}
}

func TestMaterializeDiskCapsAnImportAtTheDockDisk(t *testing.T) {
	ctx := context.Background()
	c := coordinator(t)
	// 4 MiB of zeros compresses to a few hundred bytes: the upload must not unpack past the dock.
	importedManifest(t, c, "bomb", make([]byte, 4<<20), func(*store.Manifest) {})
	dir := t.TempDir()
	dest := filepath.Join(dir, "rootfs.ext4")
	if err := c.MaterializeDiskWithin(ctx, "bomb", dest, 1<<20); !errors.Is(err, store.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("a refused import left files: %v", left)
	}
	// Without the dock's size there is nothing to hold it to.
	if err := c.MaterializeDisk(ctx, "bomb", dest); err == nil {
		t.Fatal("an import restored without a size limit")
	}
	// Exactly the dock's size is fine.
	if err := c.MaterializeDiskWithin(ctx, "bomb", dest, 4<<20); err != nil {
		t.Fatal(err)
	}
}

func TestMaterializeDiskHoldsAHostCaptureToItsRecordedRawSize(t *testing.T) {
	ctx := context.Background()
	c := coordinator(t)
	importedManifest(t, c, "host", make([]byte, 1<<20), func(m *store.Manifest) {
		m.Source, m.Image = "", ""
		m.Files[0].RawSize = 1 << 10 // a lying manifest: the object unpacks far past it
	})
	dest := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := c.MaterializeDisk(ctx, "host", dest); !errors.Is(err, store.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestMaterializeDiskMarksAWholeImageFromAnUnheldImageUnknown(t *testing.T) {
	ctx := context.Background()
	// The current base is the Dockyard ubuntu-24.04, but the import says node-22, which this host lacks.
	c, _, _, _ := fullFixture(t)
	importedManifest(t, c, "node", []byte("node-disk"), func(m *store.Manifest) { m.Image = "node-22" })
	dest := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := c.MaterializeDiskWithin(ctx, "node", dest, 1<<30); err != nil {
		t.Fatal(err)
	}
	if got := disk.DockImage(c.BaseImage, dest); got != "" {
		t.Fatalf("dock image = %q, want empty (export_base_unsupported), never the current base", got)
	}
	m, err := c.FreezeRootfsFull(ctx, "d1", dest, "again", "full", c.HostFamily, nil)
	if err != nil || m.Image != "" {
		t.Fatalf("recapture image = %q, %v", m.Image, err)
	}
	// A host-written whole image that names no image is just as unknown.
	importedManifest(t, c, "built", []byte("built-disk"), func(m *store.Manifest) { m.Source, m.Image = "", "" })
	dest2 := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := c.MaterializeDisk(ctx, "built", dest2); err != nil {
		t.Fatal(err)
	}
	if got := disk.DockImage(c.BaseImage, dest2); got != "" {
		t.Fatalf("dock image = %q, want empty", got)
	}
}