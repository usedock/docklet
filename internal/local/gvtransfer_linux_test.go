//go:build linux

package local

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// e2fsTool finds an e2fsprogs binary, also in sbin dirs that a user PATH may leave out.
func e2fsTool(t *testing.T, name string) string {
	t.Helper()
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, d := range []string{"/usr/sbin", "/sbin"} {
		if p := filepath.Join(d, name); fileExists(p) {
			return p
		}
	}
	if os.Getenv("DOCK_REQUIRE_E2FSPROGS") == "1" {
		t.Fatalf("%s not found and DOCK_REQUIRE_E2FSPROGS=1", name)
	}
	t.Skipf("%s not found (install e2fsprogs)", name)
	return ""
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

type realE2fs struct{ mkfs, debugfs, e2fsck string }

func needE2fs(t *testing.T) realE2fs {
	return realE2fs{e2fsTool(t, "mkfs.ext4"), e2fsTool(t, "debugfs"), e2fsTool(t, "e2fsck")}
}

// newBaseImage makes an ext4 image with /home/user/work, like the Dockyard base.
func (e realE2fs) newBaseImage(t *testing.T) string {
	t.Helper()
	img := filepath.Join(t.TempDir(), "disk.ext4")
	runTool(t, e.mkfs, "-q", "-F", "-b", "4096", "-I", "256", img, "64M")
	runTool(t, e.debugfs, "-w", "-R", "mkdir /home", img)
	runTool(t, e.debugfs, "-w", "-R", "mkdir /home/user", img)
	runTool(t, e.debugfs, "-w", "-R", "mkdir /home/user/work", img)
	return img
}

func runTool(t *testing.T, name string, args ...string) (string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	c := exec.Command(name, args...)
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		t.Fatalf("%s %v: %v\n%s%s", name, args, err, out.String(), errb.String())
	}
	return out.String(), errb.String()
}

// listTree runs the real find with gvListArgs against dir in place of /work and rewrites
// uid/gid to the gVisor dock user (1001), except for paths in rootOwned which get 0:0.
func listTree(t *testing.T, dir string, rootOwned map[string]bool) []byte {
	t.Helper()
	a := gvListArgs()
	if a[1] != gvWorkSrc {
		t.Fatalf("find args %v", a)
	}
	out, err := exec.Command(a[0], dir, a[2], a[3]).Output()
	if err != nil {
		t.Fatal(err)
	}
	parts := bytes.Split(bytes.TrimSuffix(out, []byte{0}), []byte{0})
	var b strings.Builder
	for i := 0; i+10 <= len(parts); i += 10 {
		f := make([]string, 10)
		for j := range f {
			f[j] = string(parts[i+j])
		}
		if rootOwned[f[9]] {
			f[2], f[3] = "0", "0"
		} else {
			f[2], f[3] = strconv.Itoa(gvUID), strconv.Itoa(gvUID)
		}
		b.WriteString(rec(f[0], f[1], f[2], f[3], f[4], f[5], f[6], f[7], f[8], f[9]))
	}
	return []byte(b.String())
}

// applyScript writes the script with /work sources pointed at dir and runs it on img.
func (e realE2fs) applyScript(t *testing.T, img, dir, script string) string {
	t.Helper()
	script = strings.ReplaceAll(script, `write "`+gvWorkSrc+`/`, `write "`+dir+`/`)
	sp := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(sp, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	// debugfs exits 0 even when a command in the script fails, so callers must read stderr.
	_, stderr := runTool(t, e.debugfs, "-w", "-f", sp, img)
	return stderr
}

type inodeStat struct {
	typ          string
	mode         uint32
	uid, gid     int
	links        int
	mtime        int64
	fastLinkDest string
}

var (
	reType  = regexp.MustCompile(`Type: (\S+)\s+Mode:\s+([0-7]+)`)
	reOwner = regexp.MustCompile(`User:\s+(\d+)\s+Group:\s+(\d+)`)
	reLinks = regexp.MustCompile(`Links: (\d+)`)
	reMtime = regexp.MustCompile(`(?m)^\s*mtime: 0x([0-9a-f]+)`)
	reFast  = regexp.MustCompile(`Fast link dest: "(.*)"`)
)

func (e realE2fs) stat(t *testing.T, img, path string) inodeStat {
	t.Helper()
	out, errs := runTool(t, e.debugfs, "-R", "stat "+debugfsQuote(path), img)
	m := reType.FindStringSubmatch(out)
	o := reOwner.FindStringSubmatch(out)
	l := reLinks.FindStringSubmatch(out)
	mt := reMtime.FindStringSubmatch(out)
	if m == nil || o == nil || l == nil || mt == nil {
		t.Fatalf("stat %q: cannot parse\n%s%s", path, out, errs)
	}
	var s inodeStat
	s.typ = m[1]
	mode, _ := strconv.ParseUint(m[2], 8, 32)
	s.mode = uint32(mode)
	s.uid, _ = strconv.Atoi(o[1])
	s.gid, _ = strconv.Atoi(o[2])
	s.links, _ = strconv.Atoi(l[1])
	s.mtime, _ = strconv.ParseInt(mt[1], 16, 64)
	if f := reFast.FindStringSubmatch(out); f != nil {
		s.fastLinkDest = f[1]
	}
	return s
}

// TestDebugfsScriptAgainstRealE2fsprogs builds a varied tree, lists it with the real find,
// writes it onto a fresh ext4 image with the real debugfs and checks the result.
func TestDebugfsScriptAgainstRealE2fsprogs(t *testing.T) {
	e := needE2fs(t)
	src := t.TempDir()
	mk := func(rel, body string, perm os.FileMode) {
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), perm); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, perm); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"a b.txt":                 "space\n",
		`q"uote.txt`:              "quote\n",
		`back\slash.txt`:          "backslash\n",
		`dollar$HOME.txt`:         "dollar\n",
		"-leading-dash":           "dash\n",
		"unicodé/日本語/ファイル.txt":    "unicode\n",
		"nested/deep/er/file.go":  "package er\n",
		"empty":                   "",
		`tricky/" ""\ $x 'y'.txt`: "mixed\n",
		"tab\tname":               "tab\n",
		"hard1":                   "hardlinked\n",
	}
	for rel, body := range files {
		mk(rel, body, 0o644)
	}
	mk("run.sh", "#!/bin/sh\necho hi\n", 0o755)
	mk("suid", "x", 0o644)
	if err := os.Chmod(filepath.Join(src, "suid"), os.ModeSetuid|0o755); err != nil {
		t.Fatal(err)
	}
	mk("private", "secret\n", 0o600)
	if err := os.Mkdir(filepath.Join(src, "nested", "hard2dir"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(src, "hard1"), filepath.Join(src, "nested", "hard2dir", "hard2")); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("long-target/", 10) + "x"
	links := map[string]string{"link": "run.sh", "nested/up": "../a b.txt", `sym"q`: `q"uote.txt`, "dangling": "nowhere", "longlink": long}
	for l, target := range links {
		if err := os.Symlink(target, filepath.Join(src, l)); err != nil {
			t.Fatal(err)
		}
	}
	// Distinct mtimes, set children before parents so directory times stick.
	var all []string
	_ = filepath.Walk(src, func(p string, _ os.FileInfo, err error) error {
		if err == nil {
			all = append(all, p)
		}
		return nil
	})
	base := time.Unix(1_600_000_000, 0)
	for i := len(all) - 1; i >= 0; i-- {
		ts := base.Add(time.Duration(i) * time.Hour)
		tv := []unix.Timeval{unix.NsecToTimeval(ts.UnixNano()), unix.NsecToTimeval(ts.UnixNano())}
		if err := unix.Lutimes(all[i], tv); err != nil {
			t.Fatal(err)
		}
	}

	listing := listTree(t, src, map[string]bool{"suid": true})
	script, warns, err := buildDebugfsScript(listing)
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Fatalf("warnings %v", warns)
	}
	img := e.newBaseImage(t)
	stderr := e.applyScript(t, img, src, script)

	// The only stderr is the banner and the mkdir of the work dir the base already has.
	t.Logf("debugfs stderr:\n%s", stderr)
	if errs := debugfsErrors(stderr); len(errs) != 0 {
		t.Fatalf("debugfs errors %q\nscript:\n%s", errs, script)
	}
	if !strings.Contains(stderr, "already exists") {
		t.Fatalf("expected the existing work dir to be reported:\n%s", stderr)
	}

	// Compare every listed entry with what the image now holds.
	recs := bytes.Split(bytes.TrimSuffix(listing, []byte{0}), []byte{0})
	checked := 0
	for i := 0; i+10 <= len(recs); i += 10 {
		typ, rel := string(recs[i]), string(recs[i+9])
		perm, _ := strconv.ParseUint(string(recs[i+1]), 8, 32)
		uid, _ := strconv.Atoi(string(recs[i+2]))
		secs, _, _ := strings.Cut(string(recs[i+4]), ".")
		mtime, _ := strconv.ParseInt(secs, 10, 64)
		dst := gvWorkDest
		if rel != "" {
			dst += "/" + rel
		}
		s := e.stat(t, img, dst)
		wantType := map[string]string{"f": "regular", "d": "directory", "l": "symlink"}[typ]
		if s.typ != wantType || s.mode != uint32(perm) || s.uid != mapID(uid) || s.gid != mapID(uid) || s.mtime != mtime {
			t.Errorf("%q: got %+v, want type %s mode %o owner %d mtime %d", rel, s, wantType, perm, mapID(uid), mtime)
		}
		checked++
	}
	if checked < 25 {
		t.Fatalf("only %d entries checked", checked)
	}
	if s := e.stat(t, img, gvWorkDest+"/run.sh"); s.uid != gvTargetUID || s.mode != 0o755 {
		t.Errorf("uid 1001 must map to 1000 and exec bits stay: %+v", s)
	}
	if s := e.stat(t, img, gvWorkDest+"/suid"); s.uid != 0 || s.mode != 0o4755 {
		t.Errorf("root-owned setuid file: %+v", s)
	}
	for _, p := range []string{"/hard1", "/nested/hard2dir/hard2"} {
		if s := e.stat(t, img, gvWorkDest+p); s.links != 2 {
			t.Errorf("%s links_count %d, want 2", p, s.links)
		}
	}
	if s := e.stat(t, img, gvWorkDest+"/"+`sym"q`); s.fastLinkDest != `q"uote.txt` {
		t.Errorf("symlink target %q", s.fastLinkDest)
	}

	// Contents and symlink targets, read back with rdump.
	out := t.TempDir()
	c := exec.Command(e.debugfs, "-R", "rdump "+debugfsQuote(gvWorkDest)+" "+debugfsQuote(out), img)
	if b, err := c.CombinedOutput(); err != nil {
		t.Fatalf("rdump: %v\n%s", err, b)
	}
	got := filepath.Join(out, "work")
	for rel, body := range files {
		b, err := os.ReadFile(filepath.Join(got, rel))
		if err != nil || string(b) != body {
			t.Errorf("%q: content %q %v, want %q", rel, b, err, body)
		}
	}
	if b, err := os.ReadFile(filepath.Join(got, "nested", "hard2dir", "hard2")); err != nil || string(b) != "hardlinked\n" {
		t.Errorf("hard link content %q %v", b, err)
	}
	for l, target := range links {
		if g, err := os.Readlink(filepath.Join(got, l)); err != nil || g != target {
			t.Errorf("symlink %q -> %q %v, want %q", l, g, err, target)
		}
	}

	// The filesystem stays consistent.
	if b, err := exec.Command(e.e2fsck, "-fn", img).CombinedOutput(); err != nil {
		t.Fatalf("e2fsck -fn: %v\n%s", err, b)
	}
}

// TestDebugfsCarriageReturnName shows why names with \r are refused: debugfs -f cuts each script
// line at the first \r, so the command fails and the file is silently not copied.
func TestDebugfsCarriageReturnName(t *testing.T) {
	e := needE2fs(t)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "cr\rname"), []byte("cr\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := buildDebugfsScript(listTree(t, src, nil)); ErrCode(err) != "push_unsupported_name" {
		t.Fatalf("want push_unsupported_name, got %v", err)
	}
	img := e.newBaseImage(t)
	stderr := e.applyScript(t, img, src, "write "+debugfsQuote(src+"/cr\rname")+" "+debugfsQuote(gvWorkDest+"/cr\rname")+"\n")
	if errs := debugfsErrors(stderr); len(errs) != 1 || !strings.Contains(errs[0], "Unbalanced quotes") {
		t.Fatalf("debugfs errors %q", errs)
	}
}
