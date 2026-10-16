package local

import (
	"archive/tar"
	"cmp"
	"context"
	"hash/maphash"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Pulling a gVisor fleet dock: the export is state.tar.zst, a tar of the dock's work directory
// as the fleet host wrote it (`tar -C <workdir> --exclude=./lost+found -cf - .`: entries are
// "./"-prefixed, with a "./" root directory entry). The tar is untrusted. It is rewritten in Go
// into a canonical tar that holds only safe entries, then unpacked by a runsc helper that is
// read-only, has no network and mounts only the new dock's work volume.

const (
	// gvTarPath is where the helper sees the canonical tar.
	gvTarPath = "/x/work.tar"
	// gvTarOwner is the dock user the unpacked files belong to.
	gvTarOwner = 1001
	// workTarMaxEntries bounds the entries of a work tar (and the memory used to check them).
	workTarMaxEntries = 5_000_000
	// workTarBlock is the disk block size an entry is assumed to take.
	workTarBlock = 4096
)

// workTarStats describes a checked work tar.
type workTarStats struct {
	Entries int
	// Declared is the sum of the sizes of all regular files.
	Declared int64
	// Usage estimates what the tree takes on disk (blocks, directories).
	Usage int64
}

// pathTree records the kind of every path the archive creates, including the directories a
// deeper entry implies, so a path never changes kind. Nodes are keyed by their parent and a
// 128-bit hash of their name, so a walk is linear in the name and memory is per node.
type pathTree struct {
	s1, s2 maphash.Seed
	m      map[pathKey]pathNode
}

type pathKey struct{ parent, h1, h2 uint64 }

// pathNode is a path's id (the root is 0) and kind: d directory, f file or hard link, l symlink.
type pathNode struct {
	id   uint64
	kind byte
}

func newPathTree() *pathTree {
	return &pathTree{s1: maphash.MakeSeed(), s2: maphash.MakeSeed(), m: map[pathKey]pathNode{}}
}

func (t *pathTree) key(parent uint64, name string) pathKey {
	return pathKey{parent, maphash.String(t.s1, name), maphash.String(t.s2, name)}
}

// add records p (not the root) as kind, adding the directories above it that are not known
// yet. It fails when a path above p is not a directory or p already has another kind.
func (t *pathTree) add(p string, kind byte) error {
	parent := uint64(0)
	for {
		name, rest, deeper := strings.Cut(p, "/")
		k := t.key(parent, name)
		n, ok := t.m[k]
		if !ok {
			if len(t.m) >= workTarMaxEntries {
				return errf("import_too_big", "the work archive has more than %d paths", workTarMaxEntries)
			}
			n = pathNode{id: uint64(len(t.m)) + 1, kind: 'd'}
			if !deeper {
				n.kind = kind
			}
			t.m[k] = n
		}
		if !deeper {
			if n.kind != kind {
				return errf("invalid_archive", "the work archive reuses the path %q for a different kind of entry", p)
			}
			return nil
		}
		if n.kind != 'd' {
			return errf("invalid_archive", "the work archive writes %q below a symlink or file", p)
		}
		parent, p = n.id, rest
	}
}

// kind returns the kind of a path the archive created, or 0.
func (t *pathTree) kind(p string) byte {
	parent := uint64(0)
	for {
		name, rest, deeper := strings.Cut(p, "/")
		n, ok := t.m[t.key(parent, name)]
		if !ok {
			return 0
		}
		if !deeper {
			return n.kind
		}
		if n.kind != 'd' {
			return 0
		}
		parent, p = n.id, rest
	}
}

// cleanTarName turns an entry name into a path relative to the work directory. "" is the root.
// It refuses absolute names and any ".." component.
func cleanTarName(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) {
		return "", errf("invalid_archive", "the work archive has an entry with an empty or invalid name")
	}
	if strings.HasPrefix(name, "/") {
		return "", errf("invalid_archive", "the work archive has an absolute path: %q", name)
	}
	for _, c := range strings.Split(name, "/") {
		if c == ".." {
			return "", errf("invalid_archive", "the work archive has a path that leaves the work directory: %q", name)
		}
	}
	p := path.Clean(name)
	if p == "." {
		return "", nil
	}
	return p, nil
}

// sanitizeWorkTar reads the tar at src and writes a canonical copy to dst holding only what is
// safe to unpack: directories, regular files, symlinks and hard links, with names that stay
// inside the work directory, nothing written through a symlink and every owner set to the dock
// user. Anything else (device nodes, fifos, an absolute path, "..", a hard link to something
// that is not an earlier regular file, a path below a symlink or file) refuses the whole
// archive. A tar whose regular files declare more than maxDeclared bytes is refused too.
// Rewriting the headers means the unpacking tar never sees the original, so a parser
// difference between Go and GNU tar cannot smuggle an entry past this check.
func sanitizeWorkTar(src, dst string, maxDeclared int64) (st workTarStats, err error) {
	in, err := os.Open(src)
	if err != nil {
		return st, errf("import_failed", "%v", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return st, errf("import_failed", "%v", err)
	}
	defer func() {
		if cerr := out.Close(); err == nil && cerr != nil {
			err = errf("import_failed", "%v", cerr)
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	tr := tar.NewReader(in)
	tw := tar.NewWriter(out)
	paths := newPathTree()
	for {
		hdr, rerr := tr.Next()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return st, errf("invalid_archive", "the work archive is not a valid tar: %v", rerr)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if st.Entries++; st.Entries > workTarMaxEntries {
			return st, errf("import_too_big", "the work archive has more than %d entries", workTarMaxEntries)
		}
		p, err := cleanTarName(hdr.Name)
		if err != nil {
			return st, err
		}
		nh := &tar.Header{Mode: hdr.Mode & 0o7777, Uid: gvTarOwner, Gid: gvTarOwner, ModTime: hdr.ModTime.Truncate(time.Second)}
		var kind byte
		switch hdr.Typeflag {
		case tar.TypeDir:
			kind, nh.Typeflag, nh.Name = 'd', tar.TypeDir, "./"
			if p != "" {
				nh.Name = p + "/"
			}
			st.Usage += workTarBlock
		case tar.TypeReg, tar.TypeRegA:
			if p == "" {
				return st, errf("invalid_archive", "the work archive has a file named %q", hdr.Name)
			}
			if hdr.Size < 0 {
				return st, errf("invalid_archive", "the work archive has a negative file size: %q", hdr.Name)
			}
			kind, nh.Typeflag, nh.Name, nh.Size = 'f', tar.TypeReg, p, hdr.Size
			if st.Declared += hdr.Size; st.Declared > maxDeclared {
				return st, errf("import_too_big", "the work archive declares more than %s of files", HumanBytes(maxDeclared))
			}
			st.Usage += (hdr.Size+workTarBlock-1)/workTarBlock*workTarBlock + 512
		case tar.TypeSymlink:
			if p == "" || hdr.Linkname == "" || strings.ContainsRune(hdr.Linkname, 0) || len(hdr.Linkname) > 4096 {
				return st, errf("invalid_archive", "the work archive has a bad symlink: %q", hdr.Name)
			}
			kind, nh.Typeflag, nh.Name, nh.Linkname = 'l', tar.TypeSymlink, p, hdr.Linkname
			st.Usage += 512
		case tar.TypeLink:
			target, err := cleanTarName(hdr.Linkname)
			if err != nil {
				return st, err
			}
			if p == "" || target == "" || paths.kind(target) != 'f' {
				return st, errf("invalid_archive", "the work archive has a hard link %q that does not point at an earlier file", hdr.Name)
			}
			kind, nh.Typeflag, nh.Name, nh.Linkname = 'f', tar.TypeLink, p, target
			st.Usage += 512
		default:
			return st, errf("invalid_archive", "the work archive has an entry of an unsupported type (%q): %q; only files, directories, symlinks and hard links are accepted", string(hdr.Typeflag), hdr.Name)
		}
		// Nothing may sit below a symlink or a file, and a path keeps its kind: a symlink must
		// not replace a directory (or the reverse) so that a later entry lands outside the tree.
		if p != "" {
			if err := paths.add(p, kind); err != nil {
				return st, err
			}
		}
		if err := tw.WriteHeader(nh); err != nil {
			return st, errf("import_failed", "%v", err)
		}
		if nh.Typeflag == tar.TypeReg {
			if n, cerr := io.Copy(tw, tr); cerr != nil || n != hdr.Size {
				return st, errf("invalid_archive", "the work archive is truncated at %q", hdr.Name)
			}
		}
	}
	if err := tw.Close(); err != nil {
		return st, errf("import_failed", "%v", err)
	}
	return st, nil
}

// gvImportTarScript unpacks gvTarPath into a stage directory of /work. $1 is a random stage
// name and $2 the most KiB the volume may take; the unpack runs in the background and is
// killed once the whole (fresh) volume is over the limit. The tar was rewritten by
// sanitizeWorkTar, so every owner is already the dock user, but tar runs with --no-same-owner
// and everything is chowned again afterwards. The stage's contents then move up to /work.
const gvImportTarScript = `set -e
s=` + gvWorkSrc + `/.stage-$1
e=$s.err
mkdir "$s"
tar -x -p --numeric-owner --no-same-owner -f ` + gvTarPath + ` -C "$s" 2>"$e" &
p=$!
while :; do
  alive=0
  kill -0 $p 2>/dev/null && alive=1
  u=$(du -sk ` + gvWorkSrc + ` 2>/dev/null | cut -f1 || true)
  if [ "${u:-0}" -gt "$2" ]; then
    kill -9 $p 2>/dev/null || true
    echo ` + gvImportTooBigMarker + `
    exit 3
  fi
  [ $alive = 1 ] || break
  sleep 1
done
wait $p || { cat "$e" >&2; exit 1; }
rm -f "$e"
chmod --reference="$s" ` + gvWorkSrc + `
find "$s" -mindepth 1 -maxdepth 1 -exec mv -t ` + gvWorkSrc + ` {} +
rm -rf "$s"
chown -hR 1001:1001 ` + gvWorkSrc + `
`

// gvImportTarArgs is the docker run of the tar import helper for dock id. Its rootfs is
// read-only, so the only thing the tar can write to is the new dock's own work volume.
func gvImportTarArgs(runtime, image, id, tarPath, stage string, limitKiB int64) []string {
	a := diskHelperArgs(runtime, image, gvImportCaps, []diskMount{
		{Source: tarPath, Target: gvTarPath, ReadOnly: true},
		{Source: gvVolume(id), Target: gvWorkSrc},
	}, gvImportTarScript, stage, strconv.FormatInt(limitKiB, 10))
	return append([]string{a[0], "--name", gvImporter(id), "--read-only"}, a[1:]...)
}

// ImportWorkTar describes a pulled, decompressed work tar that becomes a new gVisor dock.
type ImportWorkTar struct {
	Name string
	// TarPath is the decompressed tar.
	TarPath   string
	CPUs      int
	MemoryMiB int
	// Network is "on" (default) or "off".
	Network string
	// MaxBytes caps the bytes of files the tar may declare; 0 means DefaultMaxRawBytes.
	MaxBytes int64
}

// ImportWorkTar registers a stopped gVisor dock whose work volume holds the files of the tar.
// The first resume boots it. Nothing is left behind on failure.
func (e *Engine) ImportWorkTar(ctx context.Context, in ImportWorkTar) (Dock, error) {
	if err := e.CheckName(in.Name); err != nil {
		return Dock{}, err
	}
	spec, err := Spec{Name: in.Name, CPUs: in.CPUs, MemoryMiB: in.MemoryMiB, Network: in.Network, Runtime: RuntimeGVisor}.withDefaults()
	if err != nil {
		return Dock{}, err
	}
	src, err := filepath.Abs(in.TarPath)
	if err != nil {
		return Dock{}, errf("invalid_argument", "%v", err)
	}
	image, suid, err := e.gvPrepare(ctx)
	if err != nil {
		return Dock{}, err
	}
	// The canonical copy lives beside the download, so it goes with the pull's temp directory.
	clean := src + ".clean"
	defer os.Remove(clean)
	st, err := sanitizeWorkTar(src, clean, cmp.Or(in.MaxBytes, DefaultMaxRawBytes))
	if err != nil {
		return Dock{}, err
	}
	limit, byFree := e.gvImportLimit(ctx, st.Usage)
	if byFree && limit < st.Usage {
		return Dock{}, errf("insufficient_space", "the work directory needs about %s but Docker's data directory has only %s free (keeping %s spare); free some space, then pull again", HumanBytes(st.Usage), HumanBytes(limit), HumanBytes(gvImportMargin))
	}
	rec, err := e.gvNewRecord(spec, false, suid, true)
	if err != nil {
		return Dock{}, err
	}
	g := e.gvisor()
	fail := func(err error) (Dock, error) {
		bg := context.WithoutCancel(ctx)
		_, _, _ = g.docker(bg, nil, "rm", "-f", gvImporter(rec.ID))
		_ = g.removeVolume(bg, rec.ID)
		_ = e.dropRecord(rec.ID)
		_ = e.removeFiles(rec.ID)
		return Dock{}, err
	}
	e.logf("unpacking the work directory into %s", rec.ID)
	stdout, stderr, err := g.docker(ctx, nil, gvImportTarArgs(rec.gvRuntime(), image, rec.ID, clean, NewID(), limit>>10)...)
	if strings.Contains(stdout, gvImportTooBigMarker) {
		if byFree {
			return fail(errf("insufficient_space", "the work directory is larger than the %s free in Docker's data directory (keeping %s spare); free some space, then pull again", HumanBytes(limit), HumanBytes(gvImportMargin)))
		}
		return fail(errf("import_too_big", "the work directory grew past %s while it was unpacked, more than the archive declares", HumanBytes(limit)))
	}
	if err != nil {
		return fail(dockerFailure("import_failed", "unpack the work directory", err, lastLines(stderr, 20)))
	}
	if err := os.WriteFile(e.file(rec.ID, gvFreshMarker), nil, 0o600); err != nil {
		return fail(errf("store_error", "%v", err))
	}
	if err := os.Remove(e.file(rec.ID, gvImportingMarker)); err != nil {
		return fail(errf("store_error", "%v", err))
	}
	return e.view(ctx, rec), nil
}
