package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// outsideTree builds a host directory the guest must never be able to touch.
func outsideTree(t *testing.T) string {
	t.Helper()
	out := t.TempDir()
	for _, f := range []string{"machine-id", "lib/dbus/machine-id", "systemd/network/keep"} {
		p := filepath.Join(out, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("host"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			raw, _ := os.ReadFile(p)
			got[p] = string(raw)
		} else if !info.IsDir() {
			got[p] = "!" + info.Mode().String()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func sameTree(t *testing.T, before, after map[string]string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("outside tree changed: %v -> %v", before, after)
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("outside %s changed: %q -> %q", k, v, after[k])
		}
	}
}

func TestGateFilesNeverFollowGuestSymlinks(t *testing.T) {
	for _, rel := range []string{
		"etc", "etc/systemd", "etc/systemd/network", "etc/systemd/system",
		"etc/systemd/system/multi-user.target.wants", "var", "var/lib", "var/lib/dbus",
	} {
		t.Run(rel, func(t *testing.T) {
			out := outsideTree(t)
			before := snapshotDir(t, out)
			root := t.TempDir()
			p := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(out, p); err != nil {
				t.Fatal(err)
			}
			err := writeGateFiles(root, "192.168.127.2", "192.168.127.1", "24", "AA:FC:00:00:00:01", false)
			if err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("want symlink refusal, got %v", err)
			}
			sameTree(t, before, snapshotDir(t, out))
		})
	}
}

func TestGateFilesRefuseSymlinkedFile(t *testing.T) {
	out := outsideTree(t)
	before := snapshotDir(t, out)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc/systemd/network"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(out, "machine-id"), filepath.Join(root, "etc/systemd/network/10-dock.network")); err != nil {
		t.Fatal(err)
	}
	if err := writeGateFiles(root, "192.168.127.2", "192.168.127.1", "24", "AA:FC:00:00:00:01", false); err == nil {
		t.Fatal("symlinked target file must be refused")
	}
	sameTree(t, before, snapshotDir(t, out))
}

func TestGateFilesUnlinkMachineIDSymlinkWithoutFollowing(t *testing.T) {
	out := outsideTree(t)
	before := snapshotDir(t, out)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(out, "machine-id"), filepath.Join(root, "etc/machine-id")); err != nil {
		t.Fatal(err)
	}
	if err := writeGateFiles(root, "192.168.127.2", "192.168.127.1", "24", "AA:FC:00:00:00:01", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "etc/machine-id")); !os.IsNotExist(err) {
		t.Fatalf("link must be unlinked: %v", err)
	}
	sameTree(t, before, snapshotDir(t, out))
}

type seedMount struct {
	seed  func(target string) error
	check func(target string)
}

func (m seedMount) Mount(_ string, target string, _ bool) error { return m.seed(target) }
func (m seedMount) Unmount(target string) error {
	if m.check != nil {
		m.check(target)
	}
	return nil
}

func TestInstallFastAgentNeverFollowsGuestSymlinks(t *testing.T) {
	cases := map[string][]string{
		"workspace":   {"home"},
		"run":         {"run"},
		"etc":         {"etc"},
		"usr":         {"usr"},
		"usr/local":   {"usr/local"},
		"etc/systemd": {"etc/systemd"},
		"system":      {"etc/systemd/system"},
		"wants":       {"etc/systemd/system/multi-user.target.wants"},
	}
	for name, links := range cases {
		t.Run(name, func(t *testing.T) {
			out := outsideTree(t)
			before := snapshotDir(t, out)
			h := &Host{cfg: Config{UID: os.Getuid(), GID: os.Getgid(), Mount: seedMount{seed: func(target string) error {
				for _, l := range links {
					p := filepath.Join(target, l)
					if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
						return err
					}
					if err := os.Symlink(out, p); err != nil {
						return err
					}
				}
				return nil
			}}}}
			err := h.installFastAgent("rootfs", "tok", "/home/user")
			if err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("want symlink refusal, got %v", err)
			}
			sameTree(t, before, snapshotDir(t, out))
		})
	}
}

func TestInstallFastAgentWritesInsideMount(t *testing.T) {
	var files []string
	h := &Host{cfg: Config{UID: os.Getuid(), GID: os.Getgid(), Mount: seedMount{
		seed: func(string) error { return nil },
		check: func(target string) {
			for p := range snapshotDir(t, target) {
				rel, _ := filepath.Rel(target, p)
				files = append(files, filepath.ToSlash(rel))
			}
		},
	}}}
	if err := h.installFastAgent("rootfs", "tok", "/home/user"); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"usr/local/libexec/dock-fast-agent.py":                               false,
		"etc/dock-fast-agent.json":                                           false,
		"etc/systemd/system/dock-fast-agent.service":                         false,
		"etc/systemd/system/multi-user.target.wants/dock-fast-agent.service": false,
	}
	for _, f := range files {
		if _, ok := want[f]; ok {
			want[f] = true
		}
	}
	for f, seen := range want {
		if !seen {
			t.Fatalf("missing %s in %v", f, files)
		}
	}
}
