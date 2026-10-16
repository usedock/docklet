package local

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// tent is one entry of a test tar: the header and, for a regular file, its content.
type tent struct {
	h    tar.Header
	body string
}

func tfile(name, body string) tent {
	return tent{h: tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Uid: 1001, Gid: 1001}, body: body}
}
func tdir(name string) tent {
	return tent{h: tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755, Uid: 1001, Gid: 1001}}
}
func tsym(name, to string) tent {
	return tent{h: tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: to, Mode: 0o777}}
}
func thard(name, to string) tent {
	return tent{h: tar.Header{Name: name, Typeflag: tar.TypeLink, Linkname: to, Mode: 0o644}}
}
func tspecial(name string, typ byte) tent {
	return tent{h: tar.Header{Name: name, Typeflag: typ, Mode: 0o666}}
}

// tarBytes encodes entries like the fleet host's `tar -C <workdir> -cf - .`.
func tarBytes(t *testing.T, ents ...tent) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range ents {
		h := e.h
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeTar(t *testing.T, ents ...tent) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "work.tar")
	if err := os.WriteFile(p, tarBytes(t, ents...), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// readTar returns the entries of a tar by name.
func readTar(t *testing.T, path string) (names []string, hdrs map[string]tar.Header, bodies map[string]string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	hdrs, bodies = map[string]tar.Header{}, map[string]string{}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names, hdrs, bodies
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		names = append(names, h.Name)
		hdrs[h.Name], bodies[h.Name] = *h, string(b)
	}
}

func TestSanitizeWorkTarKeepsAVariedTree(t *testing.T) {
	mt := time.Unix(1600000000, 0)
	long := strings.Repeat("d", 60) + "/" + strings.Repeat("e", 60) + "/file.txt"
	secret := tfile("./secret.txt", "s3cret\n")
	secret.h.Mode, secret.h.ModTime, secret.h.Uid = 0o600, mt, 1000
	priv := tdir("./private/")
	priv.h.Mode = 0o700
	setuid := tfile("./tool", "x")
	setuid.h.Mode = 0o4755
	src := writeTar(t,
		tdir("./"), tdir("./sub"), secret, priv, tfile("./private/inner.txt", "inner"),
		tfile("./sp ace\"q.txt", "q"), tfile("./.hidden", ""), tfile("./"+long, "long"), setuid,
		tsym("./link", "secret.txt"), tsym("./abs", "/etc/passwd"), tsym("./up", "../../outside"),
		thard("./hard", "./secret.txt"), tfile("noprefix.txt", "np"), tdir("./sub/"),
	)
	dst := filepath.Join(t.TempDir(), "clean.tar")
	st, err := sanitizeWorkTar(src, dst, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if st.Entries != 15 || st.Declared != int64(len("s3cret\n")+len("inner")+1+0+4+1+2) || st.Usage <= st.Declared {
		t.Fatalf("stats %+v", st)
	}
	names, hdrs, bodies := readTar(t, dst)
	for _, n := range []string{"./", "sub/", "secret.txt", "private/", "private/inner.txt", "sp ace\"q.txt", ".hidden", long, "link", "abs", "up", "hard", "noprefix.txt"} {
		if !slices.Contains(names, n) {
			t.Errorf("missing %q in %v", n, names)
		}
	}
	for n, h := range hdrs {
		if h.Uid != 1001 || h.Gid != 1001 || h.Uname != "" || h.Gname != "" {
			t.Errorf("%s: owner %d:%d %q %q", n, h.Uid, h.Gid, h.Uname, h.Gname)
		}
	}
	if h := hdrs["secret.txt"]; h.Mode != 0o600 || !h.ModTime.Equal(mt) || bodies["secret.txt"] != "s3cret\n" {
		t.Errorf("secret.txt: %+v %q", h, bodies["secret.txt"])
	}
	if hdrs["private/"].Mode != 0o700 || hdrs["tool"].Mode != 0o4755 || bodies[long] != "long" {
		t.Errorf("modes %+v %+v", hdrs["private/"], hdrs["tool"])
	}
	if h := hdrs["hard"]; h.Typeflag != tar.TypeLink || h.Linkname != "secret.txt" {
		t.Errorf("hard link %+v", h)
	}
	for n, to := range map[string]string{"link": "secret.txt", "abs": "/etc/passwd", "up": "../../outside"} {
		if h := hdrs[n]; h.Typeflag != tar.TypeSymlink || h.Linkname != to {
			t.Errorf("symlink %s: %+v", n, h)
		}
	}
}

func TestSanitizeWorkTarRefusesWhatEscapesOrIsNotAFile(t *testing.T) {
	cases := map[string][]tent{
		"absolute path":        {tdir("./"), tfile("/etc/passwd", "x")},
		"absolute dir":         {tdir("/etc/")},
		"dotdot":               {tdir("./"), tfile("../evil", "x")},
		"dotdot after dot":     {tdir("./"), tfile("./../evil", "x")},
		"dotdot in the middle": {tdir("./"), tdir("./a"), tfile("./a/../../evil", "x")},
		"only dotdot":          {tdir("..")},
		"empty name":           {tent{h: tar.Header{Name: "", Typeflag: tar.TypeReg}}},
		"write through symlink": {
			tdir("./"), tsym("./a", "/etc"), tfile("./a/passwd", "root::0:0"),
		},
		"directory below symlink": {
			tdir("./"), tsym("./a", "/etc"), tdir("./a/sub"),
		},
		"deep write through symlink": {
			tdir("./"), tdir("./x"), tsym("./x/a", ".."), tfile("./x/a/b/c", "x"),
		},
		"symlink replaced by a directory": {
			tdir("./"), tsym("./a", "/etc"), tdir("./a/"), tfile("./a/passwd", "x"),
		},
		"directory replaced by a symlink": {
			tdir("./"), tdir("./a/"), tsym("./a", "/etc"), tfile("./a/passwd", "x"),
		},
		"file replaced by a symlink":  {tdir("./"), tfile("./a", "x"), tsym("./a", "/etc")},
		"below a regular file":        {tdir("./"), tfile("./a", "x"), tfile("./a/b", "x")},
		"hard link to an absolute":    {tdir("./"), thard("./h", "/etc/passwd")},
		"hard link outside":           {tdir("./"), thard("./h", "../../etc/passwd")},
		"hard link to a missing file": {tdir("./"), thard("./h", "./nothing")},
		"hard link to a later file":   {tdir("./"), thard("./h", "./f"), tfile("./f", "x")},
		"hard link to a symlink":      {tdir("./"), tsym("./s", "/etc/passwd"), thard("./h", "./s")},
		"hard link to a directory":    {tdir("./"), tdir("./d"), thard("./h", "./d")},
		"hard link through a symlink": {tdir("./"), tfile("./f", "x"), tsym("./s", "/tmp"), thard("./s/h", "./f")},
		"character device":            {tdir("./"), tspecial("./null", tar.TypeChar)},
		"block device":                {tdir("./"), tspecial("./sda", tar.TypeBlock)},
		"fifo":                        {tdir("./"), tspecial("./pipe", tar.TypeFifo)},
		"file named dot":              {tfile(".", "x")},
		"empty symlink target":        {tsym("./s", "")},
	}
	for name, ents := range cases {
		t.Run(name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "clean.tar")
			_, err := sanitizeWorkTar(writeTar(t, ents...), dst, 1<<20)
			if ErrCode(err) != "invalid_archive" {
				t.Fatalf("got %v", err)
			}
			if _, err := os.Stat(dst); err == nil {
				t.Fatal("a refused archive leaves no output")
			}
		})
	}
}

// writeHugeHeader writes one header that declares 1 TiB and nothing else.
func writeHugeHeader(t *testing.T, w io.Writer) {
	t.Helper()
	tw := tar.NewWriter(w)
	if err := tw.WriteHeader(&tar.Header{Name: "./big", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1 << 40}); err != nil {
		t.Fatal(err)
	}
}

func TestSanitizeWorkTarRefusesHugeDeclaredSizes(t *testing.T) {
	// A header that declares 1 TiB with no data behind it: refused on the header alone.
	var buf bytes.Buffer
	writeHugeHeader(t, &buf)
	src := filepath.Join(t.TempDir(), "huge.tar")
	if err := os.WriteFile(src, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sanitizeWorkTar(src, filepath.Join(t.TempDir(), "c.tar"), 1<<30); ErrCode(err) != "import_too_big" {
		t.Fatalf("got %v", err)
	}
	// Many small files add up.
	ents := []tent{tdir("./")}
	for _, n := range []string{"a", "b", "c"} {
		ents = append(ents, tfile("./"+n, strings.Repeat("x", 100)))
	}
	if _, err := sanitizeWorkTar(writeTar(t, ents...), filepath.Join(t.TempDir(), "c.tar"), 250); ErrCode(err) != "import_too_big" {
		t.Fatalf("got %v", err)
	}
}

func TestSanitizeWorkTarRefusesBrokenTars(t *testing.T) {
	good := tarBytes(t, tdir("./"), tfile("./f", strings.Repeat("x", 2000)))
	for name, b := range map[string][]byte{
		"not a tar": bytes.Repeat([]byte("hello world"), 200),
		"truncated": good[:1024+100],
	} {
		src := filepath.Join(t.TempDir(), "x.tar")
		if err := os.WriteFile(src, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := sanitizeWorkTar(src, filepath.Join(t.TempDir(), "c.tar"), 1<<20); ErrCode(err) != "invalid_archive" {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestSanitizeWorkTarSkipsGlobalHeaders(t *testing.T) {
	src := writeTar(t, tdir("./"), tent{h: tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "x"}}}, tfile("./f", "x"))
	dst := filepath.Join(t.TempDir(), "c.tar")
	if _, err := sanitizeWorkTar(src, dst, 1<<20); err != nil {
		t.Fatal(err)
	}
	if names, _, _ := readTar(t, dst); !slices.Equal(names, []string{"./", "f"}) {
		t.Fatalf("names %v", names)
	}
}

func TestImportWorkTarHelper(t *testing.T) {
	e, d, _ := newImportEngine(t)
	src := writeTar(t, tdir("./"), tfile("./f", "hello"), tsym("./l", "f"))
	var mounted string
	d.during = func(args []string) {
		for i, a := range args {
			if a == "-v" && strings.HasSuffix(args[i+1], ":"+gvTarPath+":ro") {
				mounted = strings.TrimSuffix(args[i+1], ":"+gvTarPath+":ro")
			}
		}
		// The helper gets the rewritten tar, not the download.
		if _, _, bodies := readTar(t, mounted); bodies["f"] != "hello" || mounted == src {
			t.Errorf("mounted %s (%v)", mounted, bodies)
		}
	}
	dock, err := e.ImportWorkTar(context.Background(), ImportWorkTar{Name: "tarimp", TarPath: src, CPUs: 1, MemoryMiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	if dock.State != "archived" || dock.Runtime != RuntimeGVisor {
		t.Fatalf("dock %+v", dock)
	}
	a := strings.Join(d.helper[0], " ")
	for _, want := range []string{"--read-only", "--network none", "--cap-drop ALL", "--cap-add CHOWN", ":" + gvTarPath + ":ro", ":" + gvWorkSrc, "--name " + gvImporter(dock.ID)} {
		if !strings.Contains(a, want) {
			t.Errorf("helper args lack %q: %s", want, a)
		}
	}
	// Only the work volume is writable: no other mount without :ro.
	var rw int
	for i, x := range d.helper[0] {
		if x == "-v" && !strings.HasSuffix(d.helper[0][i+1], ":ro") {
			rw++
		}
	}
	if rw != 1 {
		t.Errorf("%d writable mounts: %s", rw, a)
	}
	for _, want := range []string{"--no-same-owner", "--numeric-owner", "du -sk", "kill -9", "chown -hR 1001:1001"} {
		if !strings.Contains(gvImportTarScript, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if _, err := os.Stat(mounted); err == nil {
		t.Error("the rewritten tar must be removed after the import")
	}
	if _, err := os.Stat(e.file(dock.ID, gvImportingMarker)); err == nil {
		t.Error("a finished import has no importing marker")
	}
	if _, err := os.Stat(e.file(dock.ID, gvFreshMarker)); err != nil {
		t.Errorf("the first resume must be a first start: %v", err)
	}
}

func TestImportWorkTarRefusalRunsNoHelper(t *testing.T) {
	e, d, _ := newImportEngine(t)
	src := writeTar(t, tdir("./"), tsym("./a", "/etc"), tfile("./a/passwd", "x"))
	_, err := e.ImportWorkTar(context.Background(), ImportWorkTar{Name: "evil", TarPath: src})
	if ErrCode(err) != "invalid_archive" || len(d.helper) != 0 {
		t.Fatalf("err %v helpers %v", err, d.helper)
	}
	if recs, _ := e.List(context.Background()); len(recs) != 0 || len(d.volumes) != 0 {
		t.Fatalf("nothing may be left: %+v %v", recs, d.volumes)
	}
}

func TestImportWorkTarDeclaredSizeIsCapped(t *testing.T) {
	e, d, _ := newImportEngine(t)
	src := writeTar(t, tdir("./"), tfile("./f", strings.Repeat("x", 1000)))
	_, err := e.ImportWorkTar(context.Background(), ImportWorkTar{Name: "big", TarPath: src, MaxBytes: 999})
	if ErrCode(err) != "import_too_big" || len(d.helper) != 0 {
		t.Fatalf("err %v helpers %v", err, d.helper)
	}
}

func TestImportWorkTarHelperLimitAndFailures(t *testing.T) {
	src := writeTar(t, tdir("./"), tfile("./f", "hello"))
	t.Run("size limit", func(t *testing.T) {
		e, d, _ := newImportEngine(t)
		if _, err := e.ImportWorkTar(context.Background(), ImportWorkTar{Name: "lim", TarPath: src}); err != nil {
			t.Fatal(err)
		}
		a := d.helper[0]
		// The usage estimate of this tiny tree plus the 64 MiB slack, in KiB.
		if got := a[len(a)-1]; got == "" || got == "0" {
			t.Fatalf("limit %q", got)
		}
	})
	t.Run("too big", func(t *testing.T) {
		e, d, _ := newImportEngine(t)
		d.stdout, d.err = gvImportTooBigMarker+"\n", errors.New("exit status 3")
		_, err := e.ImportWorkTar(context.Background(), ImportWorkTar{Name: "grow", TarPath: src})
		if ErrCode(err) != "import_too_big" {
			t.Fatalf("got %v", err)
		}
		if recs, _ := e.List(context.Background()); len(recs) != 0 || len(d.volumes) != 0 {
			t.Fatalf("nothing may be left: %+v %v", recs, d.volumes)
		}
	})
	t.Run("tar fails", func(t *testing.T) {
		e, d, _ := newImportEngine(t)
		d.stderr, d.err = "tar: ./f: Cannot open", errors.New("exit status 1")
		_, err := e.ImportWorkTar(context.Background(), ImportWorkTar{Name: "fails", TarPath: src})
		if ErrCode(err) != "import_failed" || !strings.Contains(err.Error(), "Cannot open") {
			t.Fatalf("got %v", err)
		}
		if recs, _ := e.List(context.Background()); len(recs) != 0 || len(d.volumes) != 0 {
			t.Fatalf("nothing may be left: %+v %v", recs, d.volumes)
		}
	})
	t.Run("name taken", func(t *testing.T) {
		e, d, _ := newImportEngine(t)
		if _, err := e.ImportWorkTar(context.Background(), ImportWorkTar{Name: "dup", TarPath: src}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.ImportWorkTar(context.Background(), ImportWorkTar{Name: "dup", TarPath: src}); ErrCode(err) != "dock_name_taken" || len(d.helper) != 1 {
			t.Fatalf("got %v", err)
		}
	})
}
