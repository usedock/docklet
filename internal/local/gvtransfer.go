package local

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Building blocks for moving a gVisor dock's work directory to and from a Dockyard-style ext4
// disk with e2fsprogs only (no mount, no loop device, no host root).

const (
	// gvWorkSrc is where the helper mounts the dock's work volume.
	gvWorkSrc = "/work"
	// gvWorkDest is the work directory inside the target ext4 image.
	gvWorkDest = "/home/user/work"
	// gvTargetUID is the uid/gid of "user" in the Dockyard base; the gVisor image uses gvUID.
	gvTargetUID = 1000
	// gvMaxScriptLine stays under the debugfs -f line buffer (BUFSIZ, 8192 on glibc).
	gvMaxScriptLine = 8000
)

// gvListFormat is the find -printf format of one listing record: ten NUL-terminated fields.
//
//	type (%y), permission bits in octal (%m), uid (%U), gid (%G), mtime seconds (%T@),
//	size (%s), symlink target (%l), device:inode (%D:%i), link count (%n), path below /work (%P).
const gvListFormat = `%y\0%m\0%U\0%G\0%T@\0%s\0%l\0%D:%i\0%n\0%P\0`

// gvListArgs is the find command that produces the listing the script builder parses.
// find walks parents before children but the builder does not rely on it.
func gvListArgs() []string { return []string{"find", gvWorkSrc, "-printf", gvListFormat} }

// diskMount is one -v of a disk helper. Source is a volume name or an absolute host path.
type diskMount struct {
	Source, Target string
	ReadOnly       bool
}

// diskHelperArgs is the docker run of a one-shot e2fsprogs helper: it runs under the given gVisor
// runtime (the one the dock uses) with no network, as root in the sandbox, with every capability
// dropped except caps and no privilege gain. caps are plain names such as "CHOWN". Root with no
// capabilities cannot read the dock user's private files, so reading the work volume needs
// DAC_READ_SEARCH; changing ownership of mounted files needs CHOWN, DAC_OVERRIDE and FOWNER.
// Mount the work volume read-only when the helper only reads it.
func diskHelperArgs(runtime, image string, caps []string, mounts []diskMount, script string, args ...string) []string {
	a := []string{"run", "--rm", "--runtime=" + runtime, "--network", "none", "--user", "0:0", "--cap-drop", "ALL"}
	for _, c := range caps {
		a = append(a, "--cap-add", c)
	}
	a = append(a, "--security-opt", "no-new-privileges")
	for _, m := range mounts {
		v := m.Source + ":" + m.Target
		if m.ReadOnly {
			v += ":ro"
		}
		a = append(a, "-v", v)
	}
	a = append(a, "--entrypoint", "sh", image, "-c", script, "sh")
	return append(a, args...)
}

// debugfsQuote quotes one debugfs argument. libss (debugfs's parser) has no backslash escape:
// inside double quotes only a doubled quote means a literal quote, and a backslash is literal.
func debugfsQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

type gvEntry struct {
	kind      byte // f d l
	perm      uint32
	uid, gid  int
	mtime     int64
	link      string
	inode     string
	nlink     int
	path      string // relative to /work, "" is the root
	src, dst  string
	hardOf    *gvEntry // set on the second and later names of a hard-linked file
	linksSeen int      // names seen for a hard-linked file, set on the first one
}

// buildDebugfsScript turns a listing made with gvListArgs into a debugfs -f script that recreates
// the tree under gvWorkDest, plus warnings for entries it skipped (devices, fifos, sockets).
// Everything is created first and metadata is set last, children before parents, because adding
// a child changes its parent's mtime. debugfs exits 0 even when a command fails, so the caller
// checks stderr with debugfsErrors, which allows the "already exists" of the first mkdir (the
// Dockyard base has gvWorkDest). Names debugfs cannot express (newline, carriage return,
// over-long lines) are refused with push_unsupported_name.
func buildDebugfsScript(listing []byte) (string, []string, error) {
	parts := bytes.Split(listing, []byte{0})
	if n := len(parts); n > 0 && len(parts[n-1]) == 0 {
		parts = parts[:n-1]
	}
	const fields = 10
	if len(parts)%fields != 0 {
		return "", nil, errf("push_bad_listing", "the work directory listing is truncated (%d fields)", len(parts))
	}
	var warnings []string
	var entries []*gvEntry
	var root *gvEntry
	for i := 0; i < len(parts); i += fields {
		f := make([]string, fields)
		for j := range f {
			f[j] = string(parts[i+j])
		}
		typ, path := f[0], f[9]
		for _, s := range []string{path, f[6]} {
			// debugfs -f reads a line up to \n, then cuts it at the first \r.
			if strings.ContainsAny(s, "\n\r") {
				return "", nil, errf("push_unsupported_name", "%q has a newline or carriage return in its name or link target, which cannot be copied into the disk image; rename it first", path)
			}
		}
		if typ != "f" && typ != "d" && typ != "l" {
			warnings = append(warnings, fmt.Sprintf("skipped %s (file type %q is not copied)", displayPath(path), typ))
			continue
		}
		perm, err1 := strconv.ParseUint(f[1], 8, 32)
		uid, err2 := strconv.Atoi(f[2])
		gid, err3 := strconv.Atoi(f[3])
		secs, _, _ := strings.Cut(f[4], ".")
		mtime, err4 := strconv.ParseInt(secs, 10, 64)
		nlink, err5 := strconv.Atoi(f[8])
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil || perm > 07777 || uid < 0 || gid < 0 {
			return "", nil, errf("push_bad_listing", "cannot read the listing record of %q", path)
		}
		e := &gvEntry{kind: typ[0], perm: uint32(perm), uid: mapID(uid), gid: mapID(gid), mtime: mtime, link: f[6], inode: f[7], nlink: nlink, path: path}
		if path == "" {
			if typ != "d" {
				return "", nil, errf("push_bad_listing", "the work directory root is not a directory")
			}
			root = e
			continue
		}
		e.dst = gvWorkDest + "/" + path
		e.src = gvWorkSrc + "/" + path
		entries = append(entries, e)
	}
	// A parent path is a prefix of its children, so byte order puts parents first.
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].path < entries[j].path })

	var create, meta []string
	add := func(dst *[]string, cmd string, args ...string) error {
		line := cmd
		for _, a := range args {
			line += " " + a
		}
		if len(line) > gvMaxScriptLine {
			return errf("push_unsupported_name", "a path is too long to copy into the disk image (%d bytes)", len(line))
		}
		*dst = append(*dst, line)
		return nil
	}
	if root == nil {
		root = &gvEntry{kind: 'd', perm: 0o755, uid: gvTargetUID, gid: gvTargetUID}
	}
	rootDst := debugfsQuote(gvWorkDest)
	create = append(create, "mkdir "+rootDst)

	firstOf := map[string]*gvEntry{}
	for _, e := range entries {
		q := debugfsQuote(e.dst)
		var err error
		switch e.kind {
		case 'd':
			err = add(&create, "mkdir", q)
		case 'l':
			err = add(&create, "symlink", q, debugfsQuote(e.link))
		case 'f':
			if first, ok := firstOf[e.inode]; ok && e.nlink > 1 {
				e.hardOf = first
				first.linksSeen++
				err = add(&create, "ln", debugfsQuote(first.dst), q)
			} else {
				if e.nlink > 1 {
					firstOf[e.inode] = e
					e.linksSeen = 1
				}
				err = add(&create, "write", debugfsQuote(e.src), q)
			}
		}
		if err != nil {
			return "", nil, err
		}
	}

	setMeta := func(q string, e *gvEntry) {
		meta = append(meta,
			fmt.Sprintf("sif %s uid %d", q, e.uid),
			fmt.Sprintf("sif %s gid %d", q, e.gid),
			fmt.Sprintf("sif %s mode 0%o", q, typeBits(e.kind)|e.perm),
			fmt.Sprintf("sif %s mtime @%d", q, e.mtime))
	}
	// Children before parents, so a directory's mtime is final once its children exist.
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.hardOf != nil {
			continue
		}
		q := debugfsQuote(e.dst)
		setMeta(q, e)
		if e.linksSeen > 1 {
			meta = append(meta, fmt.Sprintf("sif %s links_count %d", q, e.linksSeen))
		}
	}
	setMeta(rootDst, root)

	var b strings.Builder
	for _, l := range append(create, meta...) {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String(), warnings, nil
}

// mapID turns the gVisor image's user (gvUID) into the Dockyard base's user; other ids are kept.
func mapID(id int) int {
	if id == gvUID {
		return gvTargetUID
	}
	return id
}

func typeBits(kind byte) uint32 {
	switch kind {
	case 'd':
		return 0o040000
	case 'l':
		return 0o120000
	}
	return 0o100000
}

func displayPath(p string) string {
	if p == "" {
		return gvWorkSrc
	}
	return gvWorkSrc + "/" + p
}

// debugfsErrors returns the error lines of a debugfs -f run of a buildDebugfsScript script. It
// skips the version banner and one "already exists" pair from the first mkdir, of gvWorkDest.
func debugfsErrors(stderr string) []string {
	rootExists := []string{
		`ext2fs_mkdir: Ext2 directory already exists while creating directory "` + gvWorkDest[strings.LastIndex(gvWorkDest, "/")+1:] + `"`,
		"mkdir: Ext2 directory already exists",
	}
	var errs []string
	for _, l := range strings.Split(stderr, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "" || debugfsBanner.MatchString(l):
		case len(rootExists) > 0 && l == rootExists[0]:
			rootExists = rootExists[1:]
		default:
			errs = append(errs, l)
		}
	}
	return errs
}

var debugfsBanner = regexp.MustCompile(`^debugfs [0-9]+\.[0-9.]+ \(`)
