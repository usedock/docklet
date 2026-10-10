package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type fixtureMount struct{}

func (fixtureMount) Mount(_ string, target string, readonly bool) error {
	if !readonly {
		return errors.New("generation reads must mount read-only")
	}
	for path, content := range map[string]string{
		"home/user/app/main.go":     "package main",
		"home/user/.dock-info.json": "{}",
	} {
		full := filepath.Join(target, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}
func (fixtureMount) Unmount(_ string) error { return nil }

func generationHost(t *testing.T) *Host {
	t.Helper()
	h, _, _ := testHost(t)
	h.cfg.Mount = fixtureMount{}
	return h
}

func frozen(t *testing.T, h *Host) string {
	t.Helper()
	ctx := context.Background()
	rootfs := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := h.cfg.Snaps.FreezeRootfs(ctx, "d1", rootfs, "gen-1", "full", "fam-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	return manifest.SnapshotID
}

func TestGenerationTree(t *testing.T) {
	ctx := context.Background()
	h := generationHost(t)
	entries, err := h.GenerationTree(ctx, frozen(t, h))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Path != "home/user/.dock-info.json" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestGenerationFile(t *testing.T) {
	ctx := context.Background()
	h := generationHost(t)
	raw, err := h.GenerationFile(ctx, frozen(t, h), "home/user/app/main.go")
	if err != nil || string(raw) != "package main" {
		t.Fatalf("file = %q %v", raw, err)
	}
	if _, err := h.GenerationFile(ctx, "gen-1", "../../etc/passwd"); err == nil {
		t.Fatal("escape must be refused")
	}
}

func TestGenerationUnknown(t *testing.T) {
	if _, err := generationHost(t).GenerationTree(context.Background(), "missing"); err == nil {
		t.Fatal("unknown generation must fail")
	}
}
