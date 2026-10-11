// Package snapmgr orchestrates snapshot generations: freezing rootfs files,
// capturing memory for fork/template sources, uploading manifests to the
// store, and preparing resume generations with CPU family enforcement.
//
// Lifecycle note: resume is a fresh boot from the frozen rootfs (reboot
// semantics per spec); memory snapshots serve fork and template captures.
package snapmgr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/usedock/docklet/internal/cpufamily"
	"github.com/usedock/docklet/internal/disk"
	dockexec "github.com/usedock/docklet/internal/exec"
	"github.com/usedock/docklet/internal/fc"
	"github.com/usedock/docklet/internal/store"
)

// ErrIncompatible is returned when a generation targets another CPU family.
type ErrIncompatible struct {
	Snapshot string
	Family   string
}

func (e *ErrIncompatible) Error() string {
	return fmt.Sprintf("snapshot %s family %s incompatible with this host", e.Snapshot, e.Family)
}

// Coordinator owns local generation layout and store traffic.
type Coordinator struct {
	Driver     fc.Driver
	Store      store.Store
	Mount      disk.Mounter
	VMsDir     string
	GensDir    string
	HostFamily string
	Runner     dockexec.Runner
	// BaseImage is the pointer to the current KVM golden image; every base the
	// host holds sits content-addressed beside it (see disk.BasePath). When
	// set, a generation stores only the blocks that differ from its dock's
	// base, because shipping the base per dock is what makes a capture slow.
	// Empty falls back to whole-image generations.
	BaseImage string
}

// FreezeRootfs pins the dock rootfs as a generation and uploads it with its
// manifest. The VM must already be paused or shut down.
//
// With a base image configured the generation is a delta: the base image
// data in a fresh rootfs is identical to the base, so shipping it would upload
// gigabytes every capture. The delta carries only the blocks the guest changed,
// which is proportional to the dock's actual work.
func (c *Coordinator) FreezeRootfs(ctx context.Context, dockID, rootfsSrc, snapshotID, kind, cpuFamily string, parentID *string) (store.Manifest, error) {
	if c.BaseImage == "" {
		genPath := filepath.Join(c.GensDir, snapshotID, "rootfs.ext4")
		if err := disk.Freeze(c.Runner, rootfsSrc, genPath); err != nil {
			return store.Manifest{}, err
		}
		return c.freezeFile(ctx, dockID, genPath, "rootfs.ext4", snapshotID, kind, cpuFamily, parentID)
	}
	dir := filepath.Join(c.GensDir, snapshotID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return store.Manifest{}, err
	}
	base, err := c.baseFor(rootfsSrc)
	if err != nil {
		return store.Manifest{}, err
	}
	deltaPath := filepath.Join(dir, "rootfs.delta")
	if _, err := disk.WriteDelta(base, rootfsSrc, deltaPath); err != nil {
		return store.Manifest{}, err
	}
	fingerprint, err := disk.Fingerprint(base)
	if err != nil {
		return store.Manifest{}, err
	}
	info, err := os.Stat(rootfsSrc)
	if err != nil {
		return store.Manifest{}, err
	}
	entry, err := store.PutFileCompressed(ctx, c.Store, snapshotID, "rootfs.delta", deltaPath, c.GensDir)
	if err != nil {
		return store.Manifest{}, err
	}
	manifest := store.Manifest{
		SnapshotID: snapshotID,
		DockID:     dockID,
		ParentID:   parentID,
		Kind:       kind,
		CPUFamily:  cpuFamily,
		Files:      []store.FileEntry{entry},
		// The delta is meaningless without the exact base it was built against,
		// so the identity travels with the generation and restore validates it.
		BaseFingerprint: fingerprint,
		ImageSize:       info.Size(),
	}
	if err := store.PutManifest(ctx, c.Store, manifest); err != nil {
		return store.Manifest{}, err
	}
	return manifest, nil
}

// FreezeRootfsFull pins the dock rootfs as a whole image, never a delta: the
// object is rootfs.ext4.zst and the manifest carries its sha256 and size, so it
// can leave this host (dock pull) and be verified wherever it lands. Image names
// the Dockyard image the dock's base came from, empty for a host-built base.
// The local copy is dropped after upload: an export is a one-off, not a resume.
func (c *Coordinator) FreezeRootfsFull(ctx context.Context, dockID, rootfsSrc, snapshotID, kind, cpuFamily string, parentID *string) (store.Manifest, error) {
	genPath := filepath.Join(c.GensDir, snapshotID, "rootfs.ext4")
	defer func() { _ = os.RemoveAll(filepath.Dir(genPath)) }()
	if err := disk.Freeze(c.Runner, rootfsSrc, genPath); err != nil {
		return store.Manifest{}, err
	}
	entry, err := store.PutFileCompressed(ctx, c.Store, snapshotID, "rootfs.ext4", genPath, c.GensDir)
	if err != nil {
		return store.Manifest{}, err
	}
	manifest := store.Manifest{
		SnapshotID: snapshotID,
		DockID:     dockID,
		ParentID:   parentID,
		Kind:       kind,
		CPUFamily:  cpuFamily,
		Files:      []store.FileEntry{entry},
		SHA256:     entry.SHA256,
		SizeBytes:  entry.Size,
	}
	if c.BaseImage != "" {
		manifest.Image = disk.DockImage(c.BaseImage, rootfsSrc)
	}
	if err := store.PutManifest(ctx, c.Store, manifest); err != nil {
		return store.Manifest{}, err
	}
	return manifest, nil
}

// baseFor picks the base a dock's capture diffs against: the one its rootfs
// derives from while the host still holds it, so a newer current image never
// turns a small delta into a whole-disk one; else the current base.
func (c *Coordinator) baseFor(rootfs string) (string, error) {
	if fp := disk.BaseLabel(rootfs); fp != "" {
		if path, err := disk.FindBase(c.BaseImage, fp); err == nil {
			return path, nil
		}
	}
	path, _, err := disk.CurrentBase(c.BaseImage)
	return path, err
}

// GenerationBase names the base a stored generation was built against; empty
// for whole-image and container generations or when the manifest is unreadable.
func (c *Coordinator) GenerationBase(ctx context.Context, snapshotID string) string {
	manifest, err := store.GetManifest(ctx, c.Store, snapshotID)
	if err != nil {
		return ""
	}
	return manifest.BaseFingerprint
}

// FreezeContainerState pins a container dock's workspace state: a tar of the
// dock's workspace, which is the durable unit a resume must reproduce.
func (c *Coordinator) FreezeContainerState(ctx context.Context, dockID, statePath, snapshotID, kind, cpuFamily string, parentID *string) (store.Manifest, error) {
	dir := filepath.Join(c.GensDir, snapshotID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return store.Manifest{}, err
	}
	genPath := filepath.Join(dir, "state.tar")
	if err := os.Rename(statePath, genPath); err != nil {
		raw, err := os.ReadFile(statePath)
		if err != nil {
			return store.Manifest{}, err
		}
		if err := os.WriteFile(genPath, raw, 0o644); err != nil {
			return store.Manifest{}, err
		}
	}
	return c.freezeFiles(ctx, dockID, genPath, snapshotID, kind, cpuFamily, parentID)
}

// GenerationPath locates one artifact inside a downloaded generation.
func (c *Coordinator) GenerationPath(snapshotID, name string) string {
	return filepath.Join(c.GensDir, snapshotID, name)
}

// freezeFiles uploads every artifact of one generation under its own manifest.
func (c *Coordinator) freezeFiles(ctx context.Context, dockID, genPath, snapshotID, kind, cpuFamily string, parentID *string) (store.Manifest, error) {
	entry, err := store.PutFileCompressed(ctx, c.Store, snapshotID, filepath.Base(genPath), genPath, c.GensDir)
	if err != nil {
		return store.Manifest{}, err
	}
	manifest := store.Manifest{
		SnapshotID: snapshotID,
		DockID:     dockID,
		ParentID:   parentID,
		Kind:       kind,
		CPUFamily:  cpuFamily,
		Files:      []store.FileEntry{entry},
	}
	if err := store.PutManifest(ctx, c.Store, manifest); err != nil {
		return store.Manifest{}, err
	}
	return manifest, nil
}

func (c *Coordinator) freezeFile(ctx context.Context, dockID, genPath, name, snapshotID, kind, cpuFamily string, parentID *string) (store.Manifest, error) {
	entry, err := store.PutFileCompressed(ctx, c.Store, snapshotID, name, genPath, c.GensDir)
	if err != nil {
		return store.Manifest{}, err
	}
	manifest := store.Manifest{
		SnapshotID: snapshotID,
		DockID:     dockID,
		ParentID:   parentID,
		Kind:       kind,
		CPUFamily:  cpuFamily,
		Files:      []store.FileEntry{entry},
	}
	if err := store.PutManifest(ctx, c.Store, manifest); err != nil {
		return store.Manifest{}, err
	}
	return manifest, nil
}

// CaptureMemory snapshots guest memory for fork/template sources. Memory
// lands in the jail root; uploads read it back from there.
func (c *Coordinator) CaptureMemory(ctx context.Context, handle *fc.Handle, snapshotID, snapshotType string) error {
	if err := c.Driver.CreateSnapshot(ctx, handle, snapshotType); err != nil {
		return err
	}
	memPath, snapPath := c.Driver.SnapshotPaths(handle)
	if err := store.PutFile(ctx, c.Store, snapshotID, "mem", memPath); err != nil {
		return err
	}
	return store.PutFile(ctx, c.Store, snapshotID, "state", snapPath)
}

// PrepareResume downloads a generation, refusing cross-family restores
// before anything boots. It returns the local rootfs generation path.
func (c *Coordinator) PrepareResume(ctx context.Context, snapshotID string) (string, error) {
	manifest, err := store.GetManifest(ctx, c.Store, snapshotID)
	if err != nil {
		return "", err
	}
	if !cpufamily.Compatible(manifest.CPUFamily, c.HostFamily) {
		return "", &ErrIncompatible{Snapshot: snapshotID, Family: manifest.CPUFamily}
	}
	entry, ferr := findRootfs(manifest)
	if ferr != nil {
		return "", ferr
	}
	// Each backend's artifact keeps its own name so a container delta and an
	// ext4 image can never be confused for one another on disk.
	local := strings.TrimSuffix(entry.Path, ".zst")
	genPath := filepath.Join(c.GensDir, snapshotID, local)
	if err := store.EnsureFile(ctx, c.Store, snapshotID, entry, genPath); err != nil {
		return "", err
	}
	return genPath, nil
}

// MaterializeDisk reconstructs a generation's rootfs at dest, refusing
// cross-family restores before anything is written.
//
// A delta generation is rebuilt onto the exact base it names, looked up among
// the bases this host holds, so the transfer stays proportional to what the
// dock changed. A missing base fails with disk.ErrBaseUnavailable before any
// download. A legacy whole-image generation is downloaded as before.
func (c *Coordinator) MaterializeDisk(ctx context.Context, snapshotID, dest string) error {
	return c.MaterializeDiskWithin(ctx, snapshotID, dest, 0)
}

// MaterializeDiskWithin is MaterializeDisk for a dock of at most maxDisk bytes (0 when unknown).
//
// An imported generation (source "import": a disk a user uploaded) must carry the manifest-level
// sha256 and sizeBytes and may not unpack past maxDisk, so a small upload cannot fill the host.
func (c *Coordinator) MaterializeDiskWithin(ctx context.Context, snapshotID, dest string, maxDisk int64) error {
	manifest, err := store.GetManifest(ctx, c.Store, snapshotID)
	if err != nil {
		return err
	}
	if !cpufamily.Compatible(manifest.CPUFamily, c.HostFamily) {
		return &ErrIncompatible{Snapshot: snapshotID, Family: manifest.CPUFamily}
	}
	entry, err := findRootfs(manifest)
	if err != nil {
		return err
	}
	wholeExt4 := entry.Path == "rootfs.ext4.zst" || entry.Path == "rootfs.ext4"
	imported := manifest.Source == store.SourceImport
	if imported && (!wholeExt4 || manifest.SHA256 == "" || manifest.SizeBytes <= 0) {
		return fmt.Errorf("generation %s refused: an imported disk needs a whole rootfs.ext4 with manifest sha256 and sizeBytes", snapshotID)
	}
	if entry.Path != "rootfs.delta.zst" && entry.Path != "rootfs.delta" {
		entry, err = verifiedEntry(manifest, entry)
		if err != nil {
			return fmt.Errorf("generation %s refused: %w", snapshotID, err)
		}
		// A generation that names a Dockyard image was written after checksums existed: a missing
		// sha256 means tampering, not a legacy capture.
		if manifest.Image != "" && entry.SHA256 == "" {
			return fmt.Errorf("generation %s refused: whole image without a sha256", snapshotID)
		}
		// A host capture unpacks to the size it recorded; an upload to at most the dock's disk.
		limit := entry.RawSize
		if imported {
			if maxDisk <= 0 {
				return fmt.Errorf("generation %s refused: an imported disk needs the dock's disk size", snapshotID)
			}
			limit = maxDisk
		}
		if err := store.GetFileAutoLimit(ctx, c.Store, snapshotID, entry, dest, limit); err != nil {
			return fmt.Errorf("generation %s: %w", snapshotID, err)
		}
		if c.BaseImage == "" || !wholeExt4 {
			return nil
		}
		// Label the disk with the held base of the Dockyard image the manifest names, so later
		// captures report that image and diff against it. Otherwise mark it unknown: the current
		// base says nothing about where a whole image came from.
		if manifest.Image != "" {
			if fp := disk.ImageFingerprint(c.BaseImage, manifest.Image); fp != "" {
				return disk.LabelBase(dest, fp)
			}
		}
		return disk.LabelUnknownBase(dest)
	}
	if c.BaseImage == "" {
		return fmt.Errorf("generation %s needs a base image to rebuild onto, but none is configured", snapshotID)
	}
	if manifest.BaseFingerprint == "" {
		return fmt.Errorf("generation %s is a delta without a base fingerprint", snapshotID)
	}
	base, err := disk.FindBase(c.BaseImage, manifest.BaseFingerprint)
	if err != nil {
		return fmt.Errorf("generation %s: %w", snapshotID, err)
	}
	deltaPath := c.GenerationPath(snapshotID, "rootfs.delta")
	if err := store.EnsureFile(ctx, c.Store, snapshotID, entry, deltaPath); err != nil {
		return err
	}
	if err := disk.ApplyDelta(base, deltaPath, dest); err != nil {
		// A half-patched image must not be mistaken for a restored one.
		_ = os.Remove(dest)
		return err
	}
	return disk.LabelBase(dest, manifest.BaseFingerprint)
}

// findRootfs locates the disk image entry, accepting container tarballs
// and legacy raw uploads.
func findRootfs(manifest store.Manifest) (store.FileEntry, error) {
	for _, entry := range manifest.Files {
		switch entry.Path {
		case "rootfs.ext4.zst", "rootfs.ext4", "rootfs.tar.zst", "rootfs.tar",
			// Container generations carry the dock's workspace state.
			"state.tar.zst", "state.tar",
			// KVM generations carry only the blocks that differ from the base.
			"rootfs.delta.zst", "rootfs.delta":
			return entry, nil
		}
	}
	return store.FileEntry{}, fmt.Errorf("no rootfs entry in snapshot %s", manifest.SnapshotID)
}

// Prune deletes one generation locally and in the store.
func (c *Coordinator) Prune(ctx context.Context, snapshotID string) error {
	_ = os.RemoveAll(filepath.Join(c.GensDir, snapshotID))
	return c.Store.Delete(ctx, snapshotID)
}

// verifiedEntry folds the manifest's whole-image sha256 and size into the file
// entry GetFileAuto checks, so a tampered or truncated object is refused and
// no partial disk is left. A manifest that records neither (a legacy
// generation) is restored as before. Manifest and entry values that disagree
// are refused outright.
func verifiedEntry(manifest store.Manifest, entry store.FileEntry) (store.FileEntry, error) {
	if entry.Encoding == "" && strings.HasSuffix(entry.Path, ".zst") {
		entry.Encoding = store.EncodingZstd
	}
	if manifest.SHA256 != "" {
		want := strings.ToLower(manifest.SHA256)
		if entry.SHA256 != "" && !strings.EqualFold(entry.SHA256, want) {
			return entry, fmt.Errorf("manifest sha256 %s disagrees with its file entry %s", manifest.SHA256, entry.SHA256)
		}
		entry.SHA256 = want
	}
	if manifest.SizeBytes > 0 {
		if entry.Size > 0 && entry.Size != manifest.SizeBytes {
			return entry, fmt.Errorf("manifest sizeBytes %d disagrees with its file entry %d", manifest.SizeBytes, entry.Size)
		}
		entry.Size = manifest.SizeBytes
	}
	return entry, nil
}

// GenerationImage names the Dockyard image a stored generation's disk derives
// from; empty for host-built bases, container generations or an unreadable manifest.
func (c *Coordinator) GenerationImage(ctx context.Context, snapshotID string) string {
	manifest, err := store.GetManifest(ctx, c.Store, snapshotID)
	if err != nil {
		return ""
	}
	return manifest.Image
}

// GenerationSHA256 is the sha256 of a stored whole-image generation's object.
func (c *Coordinator) GenerationSHA256(ctx context.Context, snapshotID string) string {
	manifest, err := store.GetManifest(ctx, c.Store, snapshotID)
	if err != nil {
		return ""
	}
	return manifest.SHA256
}
