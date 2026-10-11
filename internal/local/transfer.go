package local

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

// TempDir creates a private working directory under the data dir for a push or pull.
// Callers remove it on every exit path.
func (e *Engine) TempDir() (string, error) {
	if err := os.MkdirAll(e.Dir, 0o700); err != nil {
		return "", errf("store_error", "%v", err)
	}
	if err := secureDir(e.Dir); err != nil {
		return "", errf("store_error", "restrict %s to this user: %v", e.Dir, err)
	}
	tmp := filepath.Join(e.Dir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return "", errf("store_error", "%v", err)
	}
	d, err := os.MkdirTemp(tmp, "xfer-")
	if err != nil {
		return "", errf("store_error", "%v", err)
	}
	return d, nil
}

// Flat is a stopped dock's disk flattened to a raw image. Only the disk is ever read: the
// token, guest.json and runtime files stay behind.
type Flat struct {
	Record  Record
	RawPath string
}

// Flatten flattens the dock's disk into raw inside dir. A running dock is refused unless stop
// is set, in which case it is shut down first so the disk is consistent.
func (e *Engine) Flatten(ctx context.Context, ref, dir string, stop bool) (*Flat, error) {
	rec, err := e.Lookup(ref)
	if err != nil {
		return nil, err
	}
	if e.alive(rec.ID) {
		if !stop {
			return nil, errf("dock_running", "dock %s is running; stop it first or pass --stop", ref)
		}
		e.logf("stopping %s", ref)
		if err := e.stopVM(ctx, rec.ID, false); err != nil {
			return nil, err
		}
	}
	raw := filepath.Join(dir, "disk.raw")
	e.logf("flattening the disk of %s", ref)
	if err := e.Driver.FlattenDisk(ctx, e.file(rec.ID, "disk.qcow2"), raw); err != nil {
		return nil, err
	}
	// The guest keeps its agent config, token included, in /etc/dock-fast-agent.json on this
	// disk; rotating the token kills the copy that leaves. The next boot sends the new one.
	if err := e.rotateToken(rec.ID); err != nil {
		return nil, err
	}
	return &Flat{Record: rec, RawPath: raw}, nil
}

// rotateToken replaces a stopped dock's agent token, atomically so a crash never leaves it empty.
func (e *Engine) rotateToken(id string) error {
	tmp := e.file(id, "token.tmp")
	if err := os.WriteFile(tmp, []byte(NewToken()), 0o600); err != nil {
		return errf("store_error", "%v", err)
	}
	if err := os.Rename(tmp, e.file(id, "token")); err != nil {
		_ = os.Remove(tmp)
		return errf("store_error", "%v", err)
	}
	return nil
}

// ctxReader fails reads once ctx is done so a long file copy stops on Ctrl-C.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// CompressFile writes src as one zstd stream to dst and returns the sha256 (hex) and size of dst.
func CompressFile(ctx context.Context, src, dst string) (string, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, errf("transfer_failed", "%v", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", 0, errf("transfer_failed", "%v", err)
	}
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(out, h)}
	zw, err := zstd.NewWriter(cw)
	if err != nil {
		out.Close()
		return "", 0, errf("transfer_failed", "%v", err)
	}
	_, err = io.Copy(zw, ctxReader{ctx, in})
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, errf("transfer_failed", "compress the disk: %v", err)
	}
	return hex.EncodeToString(h.Sum(nil)), cw.n, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// FileSHA256 returns the hex sha256 of a file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DecompressFile expands a zstd file to dst, refusing output larger than max bytes.
func DecompressFile(src, dst string, max int64) error { return decompress(src, dst, max) }

// EnsureImage resolves a Dockyard image by name and makes sure its kernel, initrd and base are cached.
func (e *Engine) EnsureImage(ctx context.Context, name string) (CachedImage, error) {
	im, err := e.ResolveImage(ctx, name)
	if err != nil {
		return CachedImage{}, err
	}
	return e.Cache.Ensure(ctx, im)
}

// Import describes a pulled disk to register as a stopped local dock.
type Import struct {
	Name, Image string
	Cached      CachedImage
	RawPath     string
	CPUs        int
	MemoryMiB   int
	DiskGiB     int
}

// CheckName fails with dock_name_taken when a local dock already uses name.
func (e *Engine) CheckName(name string) error {
	if name == "" {
		return nil
	}
	if _, err := e.Lookup(name); err == nil {
		return errf("dock_name_taken", "a local dock named %q already exists; pass --name to pick another", name)
	}
	return nil
}

// Import converts the raw disk into a standalone qcow2 (no backing file) and registers a
// stopped dock. The name is re-checked under the store lock.
func (e *Engine) Import(ctx context.Context, in Import) (Dock, error) {
	if in.Name != "" && !nameRE.MatchString(in.Name) {
		return Dock{}, errf("invalid_argument", "invalid dock name %q (letters, digits, '.', '_', '-')", in.Name)
	}
	if err := e.CheckName(in.Name); err != nil {
		return Dock{}, err
	}
	rec := Record{ID: NewID(), Name: in.Name, Image: in.Image, ImageDir: in.Cached.Dir, CPUs: in.CPUs, MemoryMiB: in.MemoryMiB,
		DiskGiB: in.DiskGiB, Network: "on", CreatedAt: e.now()}
	if err := e.mkDockDir(rec.ID); err != nil {
		return Dock{}, errf("store_error", "%v", err)
	}
	fail := func(err error) (Dock, error) {
		_ = e.removeFiles(rec.ID)
		return Dock{}, err
	}
	if err := e.Driver.ConvertRaw(ctx, in.RawPath, e.file(rec.ID, "disk.qcow2")); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(e.file(rec.ID, "token"), []byte(NewToken()), 0o600); err != nil {
		return fail(errf("store_error", "%v", err))
	}
	if err := e.addRecord(rec); err != nil {
		return fail(err)
	}
	return e.view(ctx, rec), nil
}
