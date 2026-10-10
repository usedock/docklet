package disk

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestGuestRefusesSymlinksAndConfines(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "etc")); err != nil {
		t.Fatal(err)
	}
	g, err := OpenGuest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := g.WriteFile("etc/x", []byte("x"), 0o644); err == nil {
		t.Fatal("write through symlink must fail")
	}
	if err := g.MkdirAll("etc/sub", 0o755); err == nil {
		t.Fatal("mkdir through symlink must fail")
	}
	if err := g.Remove("etc/x"); err == nil {
		t.Fatal("remove through symlink must fail")
	}
	if err := g.Chown("etc", os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("chown of symlink must fail")
	}
	if err := g.Symlink("/a", "etc/l"); err == nil {
		t.Fatal("symlink through symlink must fail")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("outside touched: %v", entries)
	}
	if err := g.WriteFile("ok/dir/f", []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := g.Symlink("/target", "ok/dir/l"); err != nil {
		t.Fatal(err)
	}
	if err := g.Symlink("/target2", "ok/dir/l"); err != nil {
		t.Fatalf("replace link: %v", err)
	}
	if err := g.Remove("ok/missing"); err != nil {
		t.Fatal(err)
	}
}

func TestGuestWriteFileRefusesNonRegularTarget(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	g, err := OpenGuest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := g.WriteFile("fifo", []byte("x"), 0o644); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("fifo err = %v", err)
	}
	if err := g.WriteFile("new/ok", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := g.WriteFile("new/ok", []byte("y"), 0o644); err != nil {
		t.Fatalf("overwrite regular: %v", err)
	}
}
