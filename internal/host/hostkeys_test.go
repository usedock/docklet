package host

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/usedock/docklet/internal/store"
)

func seedHostKeys(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "etc/ssh"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range append([]string{"sshd_config"}, hostKeyFiles...) {
		if err := os.WriteFile(filepath.Join(dir, "etc/ssh", name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGateFilesResetHostKeysOnlyWhenAsked(t *testing.T) {
	keep := t.TempDir()
	seedHostKeys(t, keep)
	if err := writeGateFiles(keep, "192.168.127.2", "192.168.127.1", "24", "AA:FC:00:00:00:01", false); err != nil {
		t.Fatal(err)
	}
	for _, name := range hostKeyFiles {
		if _, err := os.Stat(filepath.Join(keep, "etc/ssh", name)); err != nil {
			t.Fatalf("resume must keep %s: %v", name, err)
		}
	}
	reset := t.TempDir()
	seedHostKeys(t, reset)
	if err := writeGateFiles(reset, "192.168.127.2", "192.168.127.1", "24", "AA:FC:00:00:00:01", true); err != nil {
		t.Fatal(err)
	}
	for _, name := range hostKeyFiles {
		if _, err := os.Stat(filepath.Join(reset, "etc/ssh", name)); !os.IsNotExist(err) {
			t.Fatalf("a copy must lose %s", name)
		}
	}
	if _, err := os.Stat(filepath.Join(reset, "etc/ssh/sshd_config")); err != nil {
		t.Fatalf("sshd_config must stay: %v", err)
	}
	// A base without the image's keygen drop-in must still get keys back at boot.
	unit, err := os.ReadFile(filepath.Join(reset, sshKeygenDropIn))
	if err != nil || !strings.Contains(string(unit), "ExecStartPre=/usr/bin/ssh-keygen -A") {
		t.Fatalf("a copy must get the keygen drop-in: %q, %v", unit, err)
	}
	if _, err := os.Stat(filepath.Join(keep, sshKeygenDropIn)); !os.IsNotExist(err) {
		t.Fatalf("a resume must not touch ssh.service: %v", err)
	}
}

func TestCopiedDisk(t *testing.T) {
	ctx := context.Background()
	h, _, _, _ := testContainerHost(t)
	put := func(id, dock, source string) {
		t.Helper()
		if err := store.PutManifest(ctx, h.cfg.Snaps.Store, store.Manifest{SnapshotID: id, DockID: dock, Source: source}); err != nil {
			t.Fatal(err)
		}
	}
	put("own", "dock-1", "")
	put("other", "dock-2", "")
	put("upload", "dock-1", store.SourceImport)
	cases := []struct {
		name      string
		dock, id  string
		provision bool
		want      bool
	}{
		{"fresh base image", "dock-1", "", true, false},
		{"resume own generation", "dock-1", "own", false, false},
		{"reprovision own generation", "dock-1", "own", true, false},
		{"branch or template of another dock", "dock-1", "other", true, true},
		{"load of another dock's generation", "dock-1", "other", false, true},
		{"import creates a dock", "dock-1", "upload", true, true},
		{"resume of an imported dock", "dock-1", "upload", false, false},
	}
	for _, c := range cases {
		got, err := h.copiedDisk(ctx, c.dock, c.id, c.provision)
		if err != nil || got != c.want {
			t.Fatalf("%s: got %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	if _, err := h.copiedDisk(ctx, "dock-1", "missing", true); err == nil {
		t.Fatal("an unreadable manifest must fail, not skip the reset")
	}
}
