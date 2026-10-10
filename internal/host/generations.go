package host

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/usedock/docklet/internal/disk"
)

// GenerationTree lists recorded paths of a generation without booting,
// downloading the frozen rootfs from the store when absent locally.
func (h *Host) GenerationTree(ctx context.Context, snapshotID string) ([]disk.Entry, error) {
	if err := checkIDs(snapshotID); err != nil {
		return nil, err
	}
	mount, unmount, workspaceOnly, err := h.mountGeneration(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unmount() }()
	if workspaceOnly {
		return disk.TreeAll(mount)
	}
	return disk.Tree(mount)
}

// GenerationFile reads one file out of a generation without booting.
func (h *Host) GenerationFile(ctx context.Context, snapshotID, relPath string) ([]byte, error) {
	if err := checkIDs(snapshotID); err != nil {
		return nil, err
	}
	mount, unmount, _, err := h.mountGeneration(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unmount() }()
	return disk.ReadFile(mount, relPath)
}

// mountGeneration also reports whether the root holds only a container workspace.
func (h *Host) mountGeneration(ctx context.Context, snapshotID string) (string, func() error, bool, error) {
	if snapshotID == "" || snapshotID == "latest" {
		return "", nil, false, fmt.Errorf("generation ref required")
	}
	// Container generations are plain tarballs: extract to scratch and serve
	// from there, never contacting the machine.
	if genPath, err := h.cfg.Snaps.PrepareResume(ctx, snapshotID); err == nil && strings.HasSuffix(genPath, ".tar") {
		dir, err := extractTarball(genPath)
		if err != nil {
			return "", nil, false, err
		}
		return dir, func() error { return os.RemoveAll(dir) }, true, nil
	}
	dir, err := os.MkdirTemp("", "gen")
	if err != nil {
		return "", nil, false, err
	}
	// A KVM generation is a block delta: materialize the rootfs it describes
	// before mounting, so reads never touch the machine that captured it.
	genPath := filepath.Join(dir, "rootfs.ext4")
	if err := h.cfg.Snaps.MaterializeDisk(ctx, snapshotID, genPath); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, false, err
	}
	if err != nil {
		return "", nil, false, err
	}
	unmount := func() error {
		mountErr := h.cfg.Mount.Unmount(dir)
		removeErr := os.RemoveAll(dir)
		if mountErr != nil {
			return mountErr
		}
		return removeErr
	}
	if err := h.cfg.Mount.Mount(genPath, dir, true); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, false, err
	}
	return dir, unmount, false, nil
}

// extractTarball unpacks a container generation tarball to scratch,
// refusing absolute paths and escapes outside the destination.
func extractTarball(tarPath string) (string, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	dir, err := os.MkdirTemp("", "gentar")
	if err != nil {
		return "", err
	}
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return dir, nil
		}
		if err != nil {
			_ = os.RemoveAll(dir)
			return "", err
		}
		clean := filepath.Clean("/" + hdr.Name)
		target := filepath.Join(dir, clean)
		if !strings.HasPrefix(target, dir+string(os.PathSeparator)) && target != dir {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("tar entry escapes: %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				_ = os.RemoveAll(dir)
				return "", err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				_ = os.RemoveAll(dir)
				return "", err
			}
			raw, err := io.ReadAll(tr)
			if err != nil {
				_ = os.RemoveAll(dir)
				return "", err
			}
			if err := os.WriteFile(target, raw, 0o644); err != nil {
				_ = os.RemoveAll(dir)
				return "", err
			}
		}
	}
}
