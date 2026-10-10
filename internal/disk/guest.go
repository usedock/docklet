package disk

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Guest confines host-side writes into a mounted guest rootfs. The guest
// controls every symlink inside the image, so each path is resolved inside
// the mount (os.Root) and any symlinked component is refused outright.
type Guest struct {
	dir  string
	root *os.Root
}

// OpenGuest opens a mounted guest rootfs for confined writes.
func OpenGuest(dir string) (*Guest, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Guest{dir: dir, root: root}, nil
}

// Close releases the root handle.
func (g *Guest) Close() error { return g.root.Close() }

func (g *Guest) rel(path string) string {
	return strings.TrimPrefix(filepath.Clean("/"+path), "/")
}

// vet refuses symlinks in any existing component; missing components pass.
func (g *Guest) vet(rel string, includeLast bool) error {
	if rel == "" {
		return nil
	}
	err := refuseSymlinks(g.dir, rel, includeLast)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return fmt.Errorf("guest path %q: %w", rel, err)
}

// MkdirAll creates a directory tree inside the guest.
func (g *Guest) MkdirAll(path string, perm os.FileMode) error {
	rel := g.rel(path)
	if rel == "" {
		return nil
	}
	if err := g.vet(rel, true); err != nil {
		return err
	}
	return g.root.MkdirAll(rel, perm)
}

// WriteFile writes a file inside the guest, creating parents.
func (g *Guest) WriteFile(path string, data []byte, perm os.FileMode) error {
	rel := g.rel(path)
	if err := g.vet(rel, true); err != nil {
		return err
	}
	if fi, err := g.root.Lstat(rel); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("guest path %q: %w", rel, ErrNotRegular)
	}
	if err := g.root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	return g.root.WriteFile(rel, data, perm)
}

// Remove unlinks a path inside the guest; a missing path is not an error.
// The final component may itself be a symlink: it is unlinked, never followed.
func (g *Guest) Remove(path string) error {
	rel := g.rel(path)
	if err := g.vet(rel, false); err != nil {
		return err
	}
	if err := g.root.Remove(rel); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Chown changes ownership of a non-symlink path inside the guest.
func (g *Guest) Chown(path string, uid, gid int) error {
	rel := g.rel(path)
	if err := g.vet(rel, true); err != nil {
		return err
	}
	return g.root.Chown(rel, uid, gid)
}

// Symlink creates a link inside the guest, replacing an existing one.
func (g *Guest) Symlink(target, path string) error {
	rel := g.rel(path)
	if err := g.vet(rel, false); err != nil {
		return err
	}
	if err := g.root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	if err := g.Remove(path); err != nil {
		return err
	}
	return g.root.Symlink(target, rel)
}
