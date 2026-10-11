package fc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestChownFileRefusesSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, link := range []struct{ name, to string }{{"live", target}, {"dangling", filepath.Join(dir, "missing")}} {
		path := filepath.Join(dir, link.name)
		if err := os.Symlink(link.to, path); err != nil {
			t.Fatal(err)
		}
		if err := ChownFile(path, os.Getuid(), os.Getgid()); err == nil {
			t.Fatalf("%s symlink chowned, want refusal", link.name)
		}
	}
	if err := ChownFile(dir, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("directory chowned, want refusal")
	}
	if err := ChownFile(target, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("regular file refused: %v", err)
	}
}
