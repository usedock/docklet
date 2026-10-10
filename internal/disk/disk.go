// Package disk manages per-dock rootfs files: provisioning from the base
// image, freezing generations at stop time, and reading files back from a
// frozen generation through a loop mount without booting the machine.
package disk

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	dockexec "github.com/usedock/docklet/internal/exec"
)

// RecordedPaths are the disk locations snapshots capture. Deletions count
// as changes; the base image, machine identity, and caches are excluded by
// construction because only these paths enter the generation.
var RecordedPaths = []string{
	"home/user",
	"var/lib/docker/volumes",
	"etc",
	"usr",
	"opt",
	"root",
	"srv",
	"var/spool/cron",
	"var/lib/dpkg",
}

// Provision copies the base image to a dock rootfs. Images are mostly
// empty (1-2GB of data in a 20GB file), so sparse copies keep forks fast;
// reflink is attempted first where the filesystem supports it.
func Provision(run dockexec.Runner, base, dest string) error {
	if run == nil {
		run = dockexec.Run
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := run("cp", "--reflink=auto", "--sparse=always", base, dest); err != nil {
		return fmt.Errorf("copy base image: %w", err)
	}
	return nil
}

// Grow extends a sparse rootfs to gib GiB. It never shrinks: a smaller request
// keeps the image size. The filesystem inside is grown at guest boot by
// systemd-growfs (the image's fstab root entry carries x-systemd.growfs; see
// scripts/build-image.sh).
func Grow(rootfs string, gib int) error {
	info, err := os.Stat(rootfs)
	if err != nil {
		return err
	}
	if want := int64(gib) << 30; want > info.Size() {
		if err := os.Truncate(rootfs, want); err != nil {
			return fmt.Errorf("grow rootfs: %w", err)
		}
	}
	return nil
}

// Freeze pins the current rootfs as an immutable generation file.
func Freeze(run dockexec.Runner, rootfs, generation string) error {
	if run == nil {
		run = dockexec.Run
	}
	if err := os.MkdirAll(filepath.Dir(generation), 0o755); err != nil {
		return err
	}
	return run("cp", "--reflink=auto", "--sparse=always", rootfs, generation)
}

// Mounter attaches a rootfs file for inspection (readonly) or for
// provisioning writes such as network gating (!readonly).
type Mounter interface {
	Mount(rootfs, target string, readonly bool) error
	Unmount(target string) error
}

// Mount options for guest-controlled images. nosymfollow needs Linux 5.10 and
// util-linux 2.36; older hosts retry without it and rely on the code-level checks.
const (
	ReadonlyMountOpts = "loop,ro,noload,nosymfollow,nodev,nosuid,noexec"
	readonlyFallback  = "loop,ro,noload,nodev,nosuid,noexec"
	WritableMountOpts = "loop,rw,nosymfollow,nodev,nosuid"
	writableFallback  = "loop,rw,nodev,nosuid"
)

// mountWithFallback tries the hardened options, then the set without nosymfollow.
func mountWithFallback(run func(opts string) error, readonly bool) error {
	// Generations freeze a running guest, so the journal is dirty: noload
	// inspects without replaying it. Writable mounts keep recovery.
	opts, fallback := WritableMountOpts, writableFallback
	if readonly {
		opts, fallback = ReadonlyMountOpts, readonlyFallback
	}
	if err := run(opts); err != nil {
		return run(fallback)
	}
	return nil
}

// refuseSymlinks Lstats each component of rel under root (the last only when
// includeLast) and fails with ErrNotRegular on a symlink; a missing component ends the check.
func refuseSymlinks(root, rel string, includeLast bool) error {
	parts := strings.Split(strings.Trim(filepath.ToSlash(filepath.Clean("/"+rel)), "/"), "/")
	if !includeLast {
		parts = parts[:len(parts)-1]
	}
	cur := filepath.Clean(root)
	for _, part := range parts {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: symlink at %q", ErrNotRegular, part)
		}
	}
	return nil
}

// ExecMounter loop-mounts through mount(8); docklet runs as root on metal.
type ExecMounter struct{}

func (ExecMounter) Mount(rootfs, target string, readonly bool) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	return mountWithFallback(func(opts string) error {
		return exec.Command("mount", "-o", opts, rootfs, target).Run()
	}, readonly)
}

func (ExecMounter) Unmount(target string) error {
	return exec.Command("umount", target).Run()
}

// Entry is one file in a generation tree.
type Entry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// Tree lists recorded paths under a mounted generation root.
func Tree(root string) ([]Entry, error) {
	return walk(root, RecordedPaths)
}

// TreeAll lists every file under root, for generations that hold only a workspace.
func TreeAll(root string) ([]Entry, error) {
	return walk(root, []string{"."})
}

func walk(root string, bases []string) ([]Entry, error) {
	entries := []Entry{}
	for _, recorded := range bases {
		base := filepath.Join(root, recorded)
		// Walk only Lstats the last component, so vet the intermediate ones.
		if err := refuseSymlinks(root, recorded, true); err != nil {
			if os.IsNotExist(err) || errors.Is(err, ErrNotRegular) {
				continue
			}
			return nil, err
		}
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if info.IsDir() {
				return nil
			}
			// Lstat-based walk: skip symlinks, devices, fifos and sockets.
			if !info.Mode().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			entries = append(entries, Entry{Path: rel, Size: info.Size()})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

// MaxFileBytes caps a single raw file read; the server also caps the JSON-encoded
// reply (binary can expand up to 6x) so it fits the agent relay's 8 MiB cap.
const MaxFileBytes = 4 << 20

// ErrNotRegular is returned for symlinks, devices, fifos, sockets and directories.
var ErrNotRegular = errors.New("not a regular file")

// ErrTooLarge is returned when a file exceeds MaxFileBytes.
var ErrTooLarge = errors.New("file too large")

// openRegular opens relPath under root, refusing symlinks in any component.
func openRegular(root, relPath string) (*os.File, int64, error) {
	clean := filepath.Clean("/" + relPath)
	if clean == "/" {
		return nil, 0, ErrNotRegular
	}
	cur := filepath.Clean(root)
	parts := strings.Split(strings.TrimPrefix(filepath.ToSlash(clean), "/"), "/")
	var last os.FileInfo
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			return nil, 0, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, 0, fmt.Errorf("%w: symlink at %q", ErrNotRegular, part)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return nil, 0, fmt.Errorf("%w: %q is not a directory", ErrNotRegular, part)
		}
		last = info
	}
	if !last.Mode().IsRegular() {
		return nil, 0, ErrNotRegular
	}
	if last.Size() > MaxFileBytes {
		return nil, 0, ErrTooLarge
	}
	f, err := os.Open(cur)
	if err != nil {
		return nil, 0, err
	}
	// Re-check the opened handle so a swap between Lstat and Open is caught.
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || !os.SameFile(fi, last) {
		_ = f.Close()
		return nil, 0, ErrNotRegular
	}
	return f, fi.Size(), nil
}

// ReadFile returns exact bytes for one regular file inside the mounted
// root, refusing symlinks, special files, escapes and oversized files.
func ReadFile(root, relPath string) ([]byte, error) {
	f, _, err := openRegular(root, relPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > MaxFileBytes {
		return nil, ErrTooLarge
	}
	return raw, nil
}
