package local

import (
	"reflect"
	"strings"
	"testing"
)

// rec builds one NUL-terminated listing record in gvListFormat order.
func rec(typ, mode, uid, gid, mtime, size, link, inode, nlink, path string) string {
	return strings.Join([]string{typ, mode, uid, gid, mtime, size, link, inode, nlink, path}, "\x00") + "\x00"
}

func TestBuildDebugfsScriptGolden(t *testing.T) {
	listing := rec("d", "755", "1001", "1001", "1700000000.5", "4096", "", "64:1", "3", "") +
		rec("d", "755", "1001", "1001", "1700000001.0", "4096", "", "64:2", "2", "src") +
		rec("f", "755", "1001", "1001", "1700000002.0", "10", "", "64:3", "1", "src/run.sh") +
		rec("f", "644", "1001", "1001", "1700000003.0", "5", "", "64:4", "2", "src/a b.txt") +
		rec("f", "644", "1001", "1001", "1700000003.0", "5", "", "64:4", "2", "src/hard.txt") +
		rec("f", "600", "0", "0", "1700000004.0", "7", "", "64:5", "1", `src/q"uo\te.txt`) +
		rec("l", "777", "1001", "1001", "1700000005.0", "9", "src/run.sh", "64:6", "1", "link") +
		rec("f", "4755", "1234", "1001", "1700000006.0", "1", "", "64:7", "1", "suid")
	want := `mkdir "/home/user/work"
symlink "/home/user/work/link" "src/run.sh"
mkdir "/home/user/work/src"
write "/work/src/a b.txt" "/home/user/work/src/a b.txt"
ln "/home/user/work/src/a b.txt" "/home/user/work/src/hard.txt"
write "/work/src/q""uo\te.txt" "/home/user/work/src/q""uo\te.txt"
write "/work/src/run.sh" "/home/user/work/src/run.sh"
write "/work/suid" "/home/user/work/suid"
sif "/home/user/work/suid" uid 1234
sif "/home/user/work/suid" gid 1000
sif "/home/user/work/suid" mode 0104755
sif "/home/user/work/suid" mtime @1700000006
sif "/home/user/work/src/run.sh" uid 1000
sif "/home/user/work/src/run.sh" gid 1000
sif "/home/user/work/src/run.sh" mode 0100755
sif "/home/user/work/src/run.sh" mtime @1700000002
sif "/home/user/work/src/q""uo\te.txt" uid 0
sif "/home/user/work/src/q""uo\te.txt" gid 0
sif "/home/user/work/src/q""uo\te.txt" mode 0100600
sif "/home/user/work/src/q""uo\te.txt" mtime @1700000004
sif "/home/user/work/src/a b.txt" uid 1000
sif "/home/user/work/src/a b.txt" gid 1000
sif "/home/user/work/src/a b.txt" mode 0100644
sif "/home/user/work/src/a b.txt" mtime @1700000003
sif "/home/user/work/src/a b.txt" links_count 2
sif "/home/user/work/src" uid 1000
sif "/home/user/work/src" gid 1000
sif "/home/user/work/src" mode 040755
sif "/home/user/work/src" mtime @1700000001
sif "/home/user/work/link" uid 1000
sif "/home/user/work/link" gid 1000
sif "/home/user/work/link" mode 0120777
sif "/home/user/work/link" mtime @1700000005
sif "/home/user/work" uid 1000
sif "/home/user/work" gid 1000
sif "/home/user/work" mode 040755
sif "/home/user/work" mtime @1700000000
`
	got, warns, err := buildDebugfsScript([]byte(listing))
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings %v", warns)
	}
	if got != want {
		t.Fatalf("script mismatch\n--- got\n%s--- want\n%s", got, want)
	}
}

func TestBuildDebugfsScriptSortsParentsFirstAndAddsMissingRoot(t *testing.T) {
	listing := rec("f", "644", "1001", "1001", "1.0", "1", "", "1:2", "1", "a/b/c") +
		rec("d", "755", "1001", "1001", "1.0", "1", "", "1:3", "2", "a/b") +
		rec("d", "755", "1001", "1001", "1.0", "1", "", "1:4", "2", "a")
	got, _, err := buildDebugfsScript([]byte(listing))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(got, "\n")
	want := []string{`mkdir "/home/user/work"`, `mkdir "/home/user/work/a"`, `mkdir "/home/user/work/a/b"`, `write "/work/a/b/c" "/home/user/work/a/b/c"`}
	if !reflect.DeepEqual(lines[:4], want) {
		t.Fatalf("creation order %v", lines[:4])
	}
	if !strings.Contains(got, `sif "/home/user/work" uid 1000`) {
		t.Fatal("a missing root record must still set the work directory owner")
	}
}

func TestBuildDebugfsScriptRefusals(t *testing.T) {
	for name, listing := range map[string]string{
		"newline in name": rec("f", "644", "1001", "1001", "1.0", "1", "", "1:2", "1", "bad\nname"),
		"newline in link": rec("l", "777", "1001", "1001", "1.0", "1", "x\ny", "1:2", "1", "l"),
		"long name":       rec("f", "644", "1001", "1001", "1.0", "1", "", "1:2", "1", strings.Repeat("a", 9000)),
	} {
		_, _, err := buildDebugfsScript([]byte(listing))
		if ErrCode(err) != "push_unsupported_name" {
			t.Errorf("%s: want push_unsupported_name, got %v", name, err)
		}
	}
	if _, _, err := buildDebugfsScript([]byte("d\x00755\x00")); ErrCode(err) != "push_bad_listing" {
		t.Fatalf("a truncated listing must be refused, got %v", err)
	}
	if _, _, err := buildDebugfsScript([]byte(rec("f", "zz", "1", "1", "1.0", "1", "", "1:2", "1", "x"))); ErrCode(err) != "push_bad_listing" {
		t.Fatalf("a bad mode must be refused, got %v", err)
	}
}

func TestBuildDebugfsScriptSkipsSpecialFiles(t *testing.T) {
	listing := rec("p", "644", "1001", "1001", "1.0", "0", "", "1:2", "1", "fifo") +
		rec("s", "644", "1001", "1001", "1.0", "0", "", "1:3", "1", "sock") +
		rec("c", "644", "0", "0", "1.0", "0", "", "1:4", "1", "dev/null") +
		rec("f", "644", "1001", "1001", "1.0", "1", "", "1:5", "1", "keep")
	got, warns, err := buildDebugfsScript([]byte(listing))
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 3 || !strings.Contains(warns[0], "/work/fifo") || !strings.Contains(warns[2], "/work/dev/null") {
		t.Fatalf("warnings %v", warns)
	}
	if strings.Contains(got, "fifo") || strings.Contains(got, "sock") || !strings.Contains(got, `write "/work/keep"`) {
		t.Fatalf("script %s", got)
	}
}

func TestDebugfsQuote(t *testing.T) {
	if got := debugfsQuote(`a "b" \c`); got != `"a ""b"" \c"` {
		t.Fatalf("got %s", got)
	}
}

func TestGvListArgs(t *testing.T) {
	a := gvListArgs()
	if len(a) != 4 || a[0] != "find" || a[1] != "/work" || a[2] != "-printf" || strings.Count(a[3], `\0`) != 10 {
		t.Fatalf("find args %v", a)
	}
}

func TestDiskHelperArgsShape(t *testing.T) {
	got := diskHelperArgs("runsc", "img:1", []string{"CHOWN"},
		[]diskMount{{Source: "dock-local-x-work", Target: "/work", ReadOnly: true}, {Source: "/tmp/d", Target: "/out"}},
		"debugfs -f -", "a", "b")
	want := []string{"run", "--rm", "--runtime=runsc", "--network", "none", "--user", "0:0", "--cap-drop", "ALL", "--cap-add", "CHOWN",
		"--security-opt", "no-new-privileges", "-v", "dock-local-x-work:/work:ro", "-v", "/tmp/d:/out",
		"--entrypoint", "sh", "img:1", "-c", "debugfs -f -", "sh", "a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	// No caps asked for means none added.
	if s := strings.Join(diskHelperArgs("runsc-suid", "i", nil, nil, "true"), " "); strings.Contains(s, "--cap-add") || !strings.Contains(s, "--runtime=runsc-suid") {
		t.Fatalf("args %s", s)
	}
}

func TestGVDockerfileHasE2fsprogs(t *testing.T) {
	if !strings.Contains(gvDockerfile, "e2fsprogs") {
		t.Fatal("the gVisor image needs e2fsprogs for push and pull")
	}
}
