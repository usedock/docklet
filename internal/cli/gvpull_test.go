package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/usedock/docklet/internal/local"
)

// pullDocker is a fake docker for a gVisor pull: it answers the preflight and the image check,
// records the import helper and reports every dock as stopped.
type pullDocker struct {
	mu      sync.Mutex
	calls   [][]string
	helpers [][]string
}

func (g *pullDocker) run(_ context.Context, _ io.Reader, args ...string) (string, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, args)
	switch args[0] {
	case "info":
		return `{"runsc":{}}`, "", nil
	case "inspect":
		return "false\n", "", nil
	case "image", "rm":
		return "", "", nil
	case "volume":
		return "", "", nil
	case "run":
		g.helpers = append(g.helpers, args)
		return "", "", nil
	}
	return "", "unexpected docker " + args[0], errors.New("exit status 1")
}

// gvisorPullEnv is a transfer env whose engine can run gVisor docks on a fake docker.
func gvisorPullEnv(t *testing.T) (*testEnv, *fakePlane, *pullDocker) {
	t.Helper()
	te, p := transferEnv(t, "direct")
	g := &pullDocker{}
	te.app.Engine.GVisor = &local.GVisor{GOOS: "linux", Run: g.run, LookPath: func(string) (string, error) { return "/usr/bin/docker", nil }}
	return te, p, g
}

const pullNote = "note: only /home/user/work is copied into the gVisor dock; packages and files outside it on the remote dock are not."

func TestPullGVisorImportsTheWorkDirectory(t *testing.T) {
	te, p, g := gvisorPullEnv(t)
	// A remote dock with no Dockyard base still pulls into gVisor: there is no base to check.
	p.readyExport(zstdOf(t, diskBytes), "")
	mustExit(t, te, 0, "pull", remoteID, "--from", "lab", "--name", "gvback", "--runtime", "gvisor", "--json")
	d := jsonOut(t, te).(map[string]any)
	if d["name"] != "gvback" || d["state"] != "archived" || d["runtime"] != "gvisor" || d["cpus"] != 3.0 || d["memoryMiB"] != 768.0 {
		t.Fatalf("dock %v", d)
	}
	if !strings.Contains(te.errb.String(), pullNote) {
		t.Fatalf("stderr %q", te.errb)
	}
	if len(g.helpers) != 1 {
		t.Fatalf("import helpers: %v", g.helpers)
	}
	if !slices.ContainsFunc(g.helpers[0], func(a string) bool { return strings.HasSuffix(a, ":/x/disk.raw:ro") }) {
		t.Fatalf("the disk must be mounted read-only: %v", g.helpers[0])
	}
	rec, err := te.app.Engine.Lookup("gvback")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(te.app.Engine.Dir, "docks", rec.ID)
	if _, err := os.Stat(filepath.Join(dir, "disk.qcow2")); err == nil {
		t.Fatal("a gVisor pull must not make a qcow2")
	}
	if rec.ImageDir != "" {
		t.Fatalf("a gVisor pull needs no Dockyard base image: %q", rec.ImageDir)
	}
	if len(tmpEntries(t, te)) != 0 {
		t.Fatalf("temp files left behind: %v", tmpEntries(t, te))
	}
}

func TestPullGVisorFromTheEnvDefault(t *testing.T) {
	te, p, g := gvisorPullEnv(t)
	p.readyExport(zstdOf(t, diskBytes), "ubuntu-24.04")
	te.env["DOCK_LOCAL_RUNTIME"] = "gvisor"
	mustExit(t, te, 0, "pull", remoteID, "--from", "lab", "--name", "viaenv")
	if len(g.helpers) != 1 || !strings.Contains(te.errb.String(), pullNote) {
		t.Fatalf("helpers %v stderr %q", g.helpers, te.errb)
	}
	if !strings.Contains(te.out.String(), "pulled") {
		t.Fatalf("stdout %q", te.out)
	}
	// An explicit --runtime qemu beats the env.
	te.errb.Reset()
	mustExit(t, te, 0, "pull", remoteID, "--from", "lab", "--name", "viaflag", "--runtime", "qemu")
	rec, err := te.app.Engine.Lookup("viaflag")
	if err != nil || rec.Runtime == local.RuntimeGVisor || strings.Contains(te.errb.String(), pullNote) {
		t.Fatalf("rec %+v err %v stderr %q", rec, err, te.errb)
	}
}

func TestPullQEMUPreflightFailureStopsBeforeTheExport(t *testing.T) {
	te, p, g := gvisorPullEnv(t)
	p.readyExport(zstdOf(t, diskBytes), "ubuntu-24.04")
	te.app.Engine.Driver.(*fakeVM).preflight = &local.Error{Code: "no_accelerator", Message: "KVM is not usable"}
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--name", "nokvm")
	if !strings.Contains(te.errb.String(), "no_accelerator") || !strings.Contains(te.errb.String(), "pass --runtime gvisor") {
		t.Fatalf("stderr %q", te.errb)
	}
	if p.exportPosts != 0 || len(p.downloads) != 0 {
		t.Fatalf("a failed preflight must stop before the export: posts %d downloads %v", p.exportPosts, p.downloads)
	}
	// The hint works: the same pull with --runtime gvisor goes through.
	mustExit(t, te, 0, "pull", remoteID, "--from", "lab", "--name", "nokvm", "--runtime", "gvisor")
	if len(g.helpers) != 1 {
		t.Fatalf("helpers %v", g.helpers)
	}
}

func TestPullGVisorPreflightAndFlagErrorsStopBeforeTheExport(t *testing.T) {
	te, p, _ := gvisorPullEnv(t)
	p.readyExport(zstdOf(t, diskBytes), "ubuntu-24.04")
	te.app.Engine.GVisor.GOOS = "darwin"
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--name", "x", "--runtime", "gvisor")
	if !strings.Contains(te.errb.String(), "unsupported_os") {
		t.Fatalf("stderr %q", te.errb)
	}
	te.errb.Reset()
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--name", "x", "--runtime", "lxc")
	if !strings.Contains(te.errb.String(), "invalid_runtime") {
		t.Fatalf("stderr %q", te.errb)
	}
	if p.exportPosts != 0 {
		t.Fatalf("exports %d", p.exportPosts)
	}
}

func TestPullGVisorNameCollisionFailsBeforeExporting(t *testing.T) {
	te, p, g := gvisorPullEnv(t)
	p.readyExport(zstdOf(t, diskBytes), "ubuntu-24.04")
	mustExit(t, te, 0, "new", "taken")
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--name", "taken", "--runtime", "gvisor")
	if !strings.Contains(te.errb.String(), "dock_name_taken") || p.exportPosts != 0 || len(g.helpers) != 0 {
		t.Fatalf("stderr %q exports %d helpers %v", te.errb, p.exportPosts, g.helpers)
	}
}

func TestPullRefusesARemoteGVisorDock(t *testing.T) {
	te, p, _ := gvisorPullEnv(t)
	p.dockRuntime = "gvisor"
	for _, args := range [][]string{{"pull", remoteID, "--from", "lab"}, {"pull", remoteID, "--from", "lab", "--runtime", "gvisor"}} {
		te.errb.Reset()
		mustExit(t, te, 1, args...)
		got := te.errb.String()
		if !strings.Contains(got, "not_supported") || !strings.Contains(got, "no disk image to export") || strings.Contains(got, "only /home/user/work persists") {
			t.Fatalf("stderr %q", got)
		}
	}
	if p.exportPosts != 0 {
		t.Fatalf("exports %d", p.exportPosts)
	}
}
