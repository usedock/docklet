package disk

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestProvisionAndFreeze(t *testing.T) {
	var calls [][]string
	run := func(name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil
	}
	dir := t.TempDir()
	if err := Provision(run, dir+"/base.ext4", dir+"/dock/rootfs.ext4"); err != nil {
		t.Fatal(err)
	}
	if err := Freeze(run, dir+"/dock/rootfs.ext4", dir+"/gens/g1.ext4"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0][0] != "cp" || calls[0][1] != "--reflink=auto" {
		t.Fatalf("calls = %v", calls)
	}
	for _, call := range calls {
		found := false
		for _, arg := range call {
			found = found || arg == "--sparse=always"
		}
		if !found {
			t.Fatalf("sparse copy missing in %v", call)
		}
	}
	if _, err := os.Stat(dir + "/dock"); err != nil {
		t.Fatal("parent dirs must be created")
	}
}

func TestProvisionError(t *testing.T) {
	run := func(name string, args ...string) error { return errors.New("no space") }
	if err := Provision(run, "/base", t.TempDir()+"/rootfs"); err == nil {
		t.Fatal("copy failure must surface")
	}
}

func TestTreeAndRead(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"home/user/app/main.go":     "package main",
		"home/user/.dock-info.json": "{}",
		"etc/cron.d/job":            "* * * * * root true",
	}
	for path, content := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := Tree(root)
	if err != nil || len(entries) != 3 {
		t.Fatalf("entries = %+v,%v", entries, err)
	}
	raw, err := ReadFile(root, "home/user/app/main.go")
	if err != nil || string(raw) != "package main" {
		t.Fatalf("read = %q,%v", raw, err)
	}
	if _, err := ReadFile(root, "../../etc/passwd"); err == nil {
		t.Fatal("escape must be refused")
	}
}

func TestTreeEmptyIsNotNil(t *testing.T) {
	entries, err := Tree(t.TempDir())
	if err != nil || entries == nil || len(entries) != 0 {
		t.Fatalf("entries = %#v,%v", entries, err)
	}
	all, err := TreeAll(t.TempDir())
	if err != nil || all == nil {
		t.Fatalf("all = %#v,%v", all, err)
	}
}

func symlinkRoot(t *testing.T) (root, outside string) {
	t.Helper()
	root, outside = t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("host-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "home/user"), 0o755))
	must(os.WriteFile(filepath.Join(root, "home/user/ok.txt"), []byte("ok"), 0o644))
	must(os.Symlink(secret, filepath.Join(root, "home/user/abs")))
	must(os.Symlink("../../../"+filepath.Base(outside)+"/secret", filepath.Join(root, "home/user/rel")))
	must(os.Symlink(outside, filepath.Join(root, "home/user/dirlink")))
	must(os.Symlink(outside, filepath.Join(root, "etc")))
	return root, outside
}

func TestReadFileRefusesSymlinks(t *testing.T) {
	root, _ := symlinkRoot(t)
	for _, p := range []string{"home/user/abs", "home/user/rel", "home/user/dirlink/secret", "etc/secret"} {
		if raw, err := ReadFile(root, p); err == nil {
			t.Fatalf("%s read %q", p, raw)
		}
	}
	raw, err := ReadFile(root, "home/user/ok.txt")
	if err != nil || string(raw) != "ok" {
		t.Fatalf("raw=%q err=%v", raw, err)
	}
}

func TestReadFileRefusesNonRegular(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(root, "d"); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("dir err = %v", err)
	}
	if _, err := ReadFile(root, ""); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("root err = %v", err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if _, err := ReadFile(root, "fifo"); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("fifo err = %v", err)
	}
	if err := syscall.Mknod(filepath.Join(root, "null"), syscall.S_IFCHR|0o666, 1*256+3); err != nil {
		t.Skipf("mknod needs root: %v", err)
	}
	if _, err := ReadFile(root, "null"); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("device err = %v", err)
	}
}

func TestReadFileRefusesOversized(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "big")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxFileBytes + 1); err != nil {
		t.Skipf("sparse truncate: %v", err)
	}
	_ = f.Close()
	if _, err := ReadFile(root, "big"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
}

func TestTreeSkipsSymlinksAndSpecialFiles(t *testing.T) {
	root, _ := symlinkRoot(t)
	_ = syscall.Mkfifo(filepath.Join(root, "home/user/fifo"), 0o644)
	entries, err := Tree(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "home/user/ok.txt" {
		t.Fatalf("entries = %+v", entries)
	}
	all, err := TreeAll(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range all {
		if e.Path != "home/user/ok.txt" {
			t.Fatalf("unexpected %s", e.Path)
		}
	}
}

func TestReadonlyMountOpts(t *testing.T) {
	for _, o := range []string{"ro", "noload", "nosymfollow", "nodev", "nosuid", "noexec"} {
		if !strings.Contains(","+ReadonlyMountOpts+",", ","+o+",") {
			t.Fatalf("missing %s", o)
		}
	}
}

func TestTreeSkipsIntermediateSymlinkBase(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "lib/dpkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "lib/dpkg/status"), []byte("host"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "var")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/ok"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := Tree(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "etc/ok" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestMountWithFallback(t *testing.T) {
	var tried []string
	err := mountWithFallback(func(opts string) error {
		tried = append(tried, opts)
		if strings.Contains(opts, "nosymfollow") {
			return errors.New("EINVAL")
		}
		return nil
	}, true)
	if err != nil || len(tried) != 2 || strings.Contains(tried[1], "nosymfollow") || !strings.Contains(tried[1], "noexec") {
		t.Fatalf("err=%v tried=%v", err, tried)
	}
	tried = nil
	if err := mountWithFallback(func(opts string) error { tried = append(tried, opts); return nil }, true); err != nil || len(tried) != 1 {
		t.Fatalf("err=%v tried=%v", err, tried)
	}
}

func TestWritableMountOptsAndFallback(t *testing.T) {
	var tried []string
	_ = mountWithFallback(func(opts string) error {
		tried = append(tried, opts)
		if strings.Contains(opts, "nosymfollow") {
			return errors.New("EINVAL")
		}
		return nil
	}, false)
	if len(tried) != 2 || tried[0] != WritableMountOpts || strings.Contains(tried[1], "nosymfollow") || !strings.Contains(tried[1], "nodev,nosuid") || strings.Contains(tried[1], "noexec") {
		t.Fatalf("tried = %v", tried)
	}
}

func TestGrowExtendsSparselyAndNeverShrinks(t *testing.T) {
	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Grow(rootfs, 2); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(rootfs); err != nil || info.Size() != 2<<30 {
		t.Fatalf("grown = %v, %v; want %d bytes", info, err, int64(2)<<30)
	}
	if err := Grow(rootfs, 1); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(rootfs); err != nil || info.Size() != 2<<30 {
		t.Fatalf("a smaller request must keep the size, got %v, %v", info, err)
	}
	raw := make([]byte, 5)
	f, err := os.Open(rootfs)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.ReadAt(raw, 0); err != nil || string(raw) != "image" {
		t.Fatalf("grow must keep the image content, got %q, %v", raw, err)
	}
}
