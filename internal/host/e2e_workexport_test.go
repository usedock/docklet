//go:build e2e && linux

package host

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	apiv1 "github.com/usedock/docklet/api/v1"
	"github.com/usedock/docklet/internal/container"
	"github.com/usedock/docklet/internal/fc"
	"github.com/usedock/docklet/internal/store"
)

// realExportDriver runs the capture path (pause, work dir, tar, resume) on the real container
// driver and a real runsc container; everything else stays fake.
type realExportDriver struct {
	*fakeVethDriver
	real             *container.Driver
	pausedDuringSave string
}

func (d *realExportDriver) Pause(ctx context.Context, h *fc.Handle) error {
	return d.real.Pause(ctx, h)
}
func (d *realExportDriver) Resume(ctx context.Context, h *fc.Handle) error {
	return d.real.Resume(ctx, h)
}
func (d *realExportDriver) WorkDir(dockID string) (string, error) { return d.real.WorkDir(dockID) }
func (d *realExportDriver) SaveState(dir, dest string) error {
	d.pausedDuringSave = dockerState(nil, container.ContainerName(exportDock))
	return d.real.SaveState(dir, dest)
}

const exportDock = "e2e-work-export"

func dockerState(t *testing.T, name string) string {
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Paused}} {{.State.Running}}", name).CombinedOutput()
	if err != nil && t != nil {
		t.Fatalf("docker inspect: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestE2EWorkExportOfARunningGVisorDock exports a live runsc dock whose work dir holds a FIFO,
// a socket and a symlink out of the dock, and checks the capture neither hangs nor follows the
// link and leaves the dock running.
func TestE2EWorkExportOfARunningGVisorDock(t *testing.T) {
	require := os.Getenv("DOCK_E2E_REQUIRE_GVISOR") != ""
	if os.Getuid() != 0 {
		if require {
			t.Fatal("the work export e2e needs root (loop mount, host tar)")
		}
		t.Skip("needs root")
	}
	if _, err := exec.LookPath("runsc"); err != nil {
		if require {
			t.Fatal("runsc not installed")
		}
		t.Skip("runsc not installed")
	}
	h, fake, _, _ := testContainerHost(t)
	// A short state dir keeps the socket path under the 108-byte unix limit.
	stateDir, err := os.MkdirTemp("", "wx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	real := &container.Driver{Image: "ubuntu:24.04", StateDir: stateDir}
	drv := &realExportDriver{fakeVethDriver: fake, real: real}
	h.cfg.Driver = drv
	writeJailSidecar(t, h, exportDock, resSmall)

	work, err := real.EnsureWorkDisk(exportDock, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = real.RemoveWorkDisk(exportDock) })
	name := container.ContainerName(exportDock)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	// A busy writer inside the dock, plus a symlink pointing at a host secret.
	script := `cd /home/user/work && echo hi > file && ln -s /etc/shadow leak && i=0; while :; do i=$((i+1)); echo $i > busy; done`
	if out, err := exec.Command("docker", "run", "-d", "--runtime="+container.Runtime, "--name", name,
		"--user", "1001:1001", "-v", work+":/home/user/work", "ubuntu:24.04", "sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v: %s", err, out)
	}
	// A FIFO nobody writes to and a live socket in the work dir: tar must not block on either.
	if err := syscall.Mkfifo(filepath.Join(work, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	sock, err := net.Listen("unix", filepath.Join(work, "sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer sock.Close()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Lstat(filepath.Join(work, "leak")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the dock never wrote its work dir")
		}
		time.Sleep(200 * time.Millisecond)
	}

	type result struct {
		id   string
		size int64
		err  error
	}
	done := make(chan result, 1)
	go func() {
		id, size, _, err := h.SnapshotCreateFull(context.Background(), exportDock, nil, apiv1.SnapshotFull, true, false)
		done <- result{id, size, err}
	}()
	var res result
	select {
	case res = <-done:
	case <-time.After(2 * time.Minute):
		t.Fatal("the work export hung")
	}
	if res.err != nil {
		t.Fatal(res.err)
	}
	if drv.pausedDuringSave != "true true" {
		t.Fatalf("container state during the tar = %q, want paused", drv.pausedDuringSave)
	}
	if got := dockerState(t, name); got != "false true" {
		t.Fatalf("container state after the export = %q, want running and not paused", got)
	}
	if _, err := os.Stat(filepath.Join(h.jailRoot(exportDock), exportPausedMarker)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pause marker left behind: %v", err)
	}
	// The dock keeps working after the thaw.
	// The writer truncates before each write, so sample until two distinct counts show up.
	seen := map[string]bool{}
	for i := 0; i < 100 && len(seen) < 2; i++ {
		if b, _ := os.ReadFile(filepath.Join(work, "busy")); len(bytes.TrimSpace(b)) > 0 {
			seen[string(b)] = true
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(seen) < 2 {
		t.Fatalf("the dock's writer stalled after the export: %v", seen)
	}

	manifest, err := store.GetManifest(context.Background(), h.cfg.Snaps.Store, res.id)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 1 || manifest.Files[0].Path != "state.tar.zst" {
		t.Fatalf("files = %+v", manifest.Files)
	}
	rc, err := h.cfg.Snaps.Store.Get(context.Background(), res.id, "state.tar.zst")
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := io.ReadAll(rc)
	_ = rc.Close()
	sum := sha256.Sum256(stored)
	if manifest.SHA256 != hex.EncodeToString(sum[:]) || manifest.SizeBytes != int64(len(stored)) || res.size != int64(len(stored)) {
		t.Fatalf("manifest sha=%s size=%d, answer size=%d, stored %d bytes", manifest.SHA256, manifest.SizeBytes, res.size, len(stored))
	}
	zr, err := zstd.NewReader(bytes.NewReader(stored))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		t.Fatal(err)
	}
	if manifest.DataBytes != int64(len(raw)) || manifest.DataBytes <= 0 {
		t.Fatalf("dataBytes = %d, raw tar = %d", manifest.DataBytes, len(raw))
	}
	members := map[string]*tar.Header{}
	contents := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		members[hdr.Name] = hdr
		body, _ := io.ReadAll(tr)
		contents[hdr.Name] = string(body)
	}
	if hdr := members["./pipe"]; hdr == nil || hdr.Typeflag != tar.TypeFifo {
		t.Fatalf("pipe member = %+v, want a FIFO entry", hdr)
	}
	if hdr := members["./leak"]; hdr == nil || hdr.Typeflag != tar.TypeSymlink || hdr.Linkname != "/etc/shadow" || contents["./leak"] != "" {
		t.Fatalf("leak member = %+v, want the symlink itself, never the host file", hdr)
	}
	if contents["./file"] != "hi\n" || members["./file"].Uid != container.DockUID {
		t.Fatalf("file member = %+v %q", members["./file"], contents["./file"])
	}
	for _, name := range []string{"./sock", "./lost+found", "./lost+found/"} {
		if members[name] != nil {
			t.Fatalf("unexpected member %s", name)
		}
	}
	for name := range members {
		if !strings.HasPrefix(name, "./") && name != "." && name != "./" {
			t.Fatalf("member %q is not ./-prefixed", name)
		}
	}
}
