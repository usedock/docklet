package snapmgr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
