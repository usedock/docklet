package uid

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAssignIsStableAndUnique(t *testing.T) {
	dir := t.TempDir()
	// A tiny range forces hash collisions, so probing must keep ids unique.
	a, err := Open(dir, 5000, 8)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]string{}
	for _, id := range []string{"d1", "d2", "d3", "d4", "d5", "d6", "d7", "d8"} {
		got, err := a.Assign(id)
		if err != nil {
			t.Fatal(err)
		}
		if got < 5000 || got >= 5008 {
			t.Fatalf("%s uid %d outside range", id, got)
		}
		if other, dup := seen[got]; dup {
			t.Fatalf("%s and %s share uid %d", id, other, got)
		}
		seen[got] = id
		if again, _ := a.Assign(id); again != got {
			t.Fatalf("%s reassigned %d -> %d", id, got, again)
		}
	}
	if _, err := a.Assign("d9"); err == nil {
		t.Fatal("exhausted range must fail, never share a uid")
	}
}

func TestAssignPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	a, _ := Open(dir, 200000, 65536)
	want, err := a.Assign("dock-1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(dir, 200000, 65536)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := b.Get("dock-1"); !ok || got != want {
		t.Fatalf("after restart uid = %d,%v want %d", got, ok, want)
	}
	// A new dock must not take a live dock's id even when its hash collides.
	small, _ := Open(t.TempDir(), 10, 1)
	first, _ := small.Assign("a")
	if _, err := small.Assign("b"); err == nil {
		t.Fatalf("second dock got a live uid %d", first)
	}
}

func TestReleaseFreesForReuse(t *testing.T) {
	dir := t.TempDir()
	a, _ := Open(dir, 10, 1)
	if _, err := a.Assign("a"); err != nil {
		t.Fatal(err)
	}
	if err := a.Release("a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Get("a"); ok {
		t.Fatal("released dock still holds a uid")
	}
	if got, err := a.Assign("b"); err != nil || got != 10 {
		t.Fatalf("reuse = %d,%v", got, err)
	}
	b, _ := Open(dir, 10, 1)
	if _, ok := b.Get("a"); ok {
		t.Fatal("release must persist")
	}
}

func TestOpenRejectsBadRange(t *testing.T) {
	for _, c := range [][2]int{{0, 10}, {10, 0}, {-1, 5}, {4294967290, 100}} {
		if _, err := Open(t.TempDir(), c[0], c[1]); err == nil {
			t.Fatalf("range %v accepted", c)
		}
	}
}

func TestCheckRange(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	passwd := write("passwd", "root:x:0:0::/root:/bin/sh\nubuntu:x:1000:1000::/home/ubuntu:/bin/bash\nweird:x:200500:1::/:/bin/false\n")
	subuid := write("subuid", "ubuntu:100000:65536\nother:165536:65536\n")
	subgid := write("subgid", "ubuntu:100000:65536\n")
	group := write("group", "root:x:0:\ndeploy:x:300100:ubuntu\n")
	missing := filepath.Join(dir, "nope")
	if err := CheckRange(2000000000, 65536, passwd, group, subuid, subgid); err != nil {
		t.Fatalf("clean range refused: %v", err)
	}
	if err := CheckRange(200000, 65536, missing, missing, missing, missing); err != nil {
		t.Fatalf("missing files must not clash: %v", err)
	}
	if err := CheckRange(200000, 65536, passwd, missing, missing, missing); err == nil {
		t.Fatal("passwd uid 200500 inside range must be refused")
	}
	if err := CheckRange(200000, 65536, missing, missing, subuid, missing); err == nil {
		t.Fatal("range overlapping subuid 165536+65536 must be refused")
	}
	if err := CheckRange(100000, 10, missing, missing, missing, subgid); err == nil {
		t.Fatal("range overlapping subgid must be refused")
	}
	// The dock's gid equals its uid, so a host group inside the range would grant its files.
	if err := CheckRange(300000, 65536, missing, group, missing, missing); err == nil {
		t.Fatal("group gid 300100 inside range must be refused")
	}
}

func TestSaveFsyncsFileAndDir(t *testing.T) {
	var synced []string
	old := fsync
	fsync = func(f *os.File) error {
		synced = append(synced, f.Name())
		return nil
	}
	defer func() { fsync = old }()
	dir := t.TempDir()
	a, err := Open(dir, 5000, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Assign("d1"); err != nil {
		t.Fatal(err)
	}
	if len(synced) != 2 || synced[0] != filepath.Join(dir, "uids.json.tmp") || synced[1] != dir {
		t.Fatalf("fsync calls = %v, want temp file then dir", synced)
	}
	if _, err := os.Stat(filepath.Join(dir, "uids.json.tmp")); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
	b, err := Open(dir, 5000, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Get("d1"); !ok {
		t.Fatal("allocation not persisted")
	}
}
