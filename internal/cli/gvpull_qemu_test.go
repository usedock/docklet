package cli

import (
	"archive/tar"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/usedock/docklet/internal/local"
)

// qemuNote is the fresh-base note of a gVisor remote pulled into a qemu dock.
func qemuNote(image string) string {
	return "note: the remote dock runs on gVisor; the new dock starts from a fresh " + image + " with only /home/user/work copied."
}

// qemuWorkPull is a transfer env whose plane offers a work-tar export of a gVisor dock, and the
// fake VM the engine boots; a fake docker makes any gVisor path visible.
func qemuWorkPull(t *testing.T, tarball string) (*testEnv, *fakePlane, *fakeVM, *pullDocker) {
	t.Helper()
	te, p, g := gvisorPullEnv(t)
	gvisorRemote(t, p, tarball)
	return te, p, te.app.Engine.Driver.(*fakeVM), g
}

func TestPullGVisorRemoteIntoQEMU(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		env  string
	}{
		"default":    {[]string{"pull", remoteID, "--from", "lab", "--name", "fromgv", "--json"}, ""},
		"flag":       {[]string{"pull", remoteID, "--from", "lab", "--name", "fromgv", "--json", "--runtime", "qemu"}, ""},
		"env":        {[]string{"pull", remoteID, "--from", "lab", "--name", "fromgv", "--json"}, "qemu"},
		"flag > env": {[]string{"pull", remoteID, "--from", "lab", "--name", "fromgv", "--json", "--runtime", "qemu"}, "gvisor"},
	} {
		te, p, vm, g := qemuWorkPull(t, workTarOf(t, tar.Header{Name: "./a.txt", Mode: 0o644}))
		if tc.env != "" {
			te.env["DOCK_LOCAL_RUNTIME"] = tc.env
		}
		mustExit(t, te, 0, tc.args...)
		if got, ok := p.exportBody["formats"].([]any); !ok || len(got) != 1 || got[0] != "work-tar" {
			t.Fatalf("%s: export body %v", name, p.exportBody)
		}
		d := jsonOut(t, te).(map[string]any)
		// The disk is the export's 4 GiB raised to the 10 GiB default, so the base always fits.
		if d["name"] != "fromgv" || d["runtime"] != "qemu" || d["source"] != "work" || d["state"] != "archived" ||
			d["cpus"] != 3.0 || d["memoryMiB"] != 768.0 || d["diskGiB"] != 10.0 || d["image"] != "ubuntu-24.04" {
			t.Fatalf("%s: dock %v", name, d)
		}
		if !strings.Contains(te.errb.String(), qemuNote("ubuntu-24.04")) {
			t.Fatalf("%s: stderr %q", name, te.errb)
		}
		if len(g.helpers) != 0 {
			t.Fatalf("%s: a qemu target must not run a gVisor helper: %v", name, g.helpers)
		}
		if len(vm.starts) != 1 || !strings.HasSuffix(vm.starts[0].ImportDrive, "work.tar.clean") {
			t.Fatalf("%s: the one boot must carry the import drive: %+v", name, vm.starts)
		}
		rec, err := te.app.Engine.Lookup("fromgv")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(te.app.Engine.Dir, "docks", rec.ID, "importing")); err == nil {
			t.Fatalf("%s: the importing marker must be gone", name)
		}
		if len(tmpEntries(t, te)) != 0 {
			t.Fatalf("%s: temp files left behind: %v", name, tmpEntries(t, te))
		}
	}
}

func TestPullGVisorRemoteIntoQEMUHumanOutput(t *testing.T) {
	te, _, _, _ := qemuWorkPull(t, workTarOf(t))
	mustExit(t, te, 0, "pull", remoteID, "--from", "lab", "--name", "human")
	if !strings.Contains(te.out.String(), "pulled") || !strings.Contains(te.out.String(), "state=archived") || !strings.Contains(te.errb.String(), "preparing the ubuntu-24.04 image") {
		t.Fatalf("stdout %q stderr %q", te.out, te.errb)
	}
}

func TestPullGVisorRemoteIntoQEMUImageFallback(t *testing.T) {
	for name, tc := range []struct {
		base, dock, want string
	}{
		{"debian-12", "other-img", "debian-12"}, // the export's base wins
		{"", "debian-12", "debian-12"},          // else the remote dock's image
		{"", "-", local.DefaultImage},           // else the default
	} {
		te, p, _, _ := qemuWorkPull(t, workTarOf(t))
		// The catalog gains debian-12 and other-img.
		cat := te.app.Engine.Catalog.(staticCatalog)
		for _, n := range []string{"debian-12", "other-img"} {
			im := cat.ix.Images[0]
			im.Name = n
			cat.ix.Images = append(cat.ix.Images, im)
		}
		p.exportState["baseImage"] = tc.base
		p.dockImage = tc.dock
		mustExit(t, te, 0, "pull", remoteID, "--from", "lab", "--name", "img", "--json")
		d := jsonOut(t, te).(map[string]any)
		if d["image"] != tc.want || !strings.Contains(te.errb.String(), qemuNote(tc.want)) {
			t.Fatalf("case %d: dock %v stderr %q", name, d, te.errb)
		}
		if strings.Contains(te.errb.String(), "fresh other-img") && tc.want != "other-img" {
			t.Fatalf("case %d: the note names an image that was not used: %q", name, te.errb)
		}
	}
}

func TestPullGVisorRemoteIntoQEMUUnknownImageFailsBeforeDownload(t *testing.T) {
	te, p, vm, _ := qemuWorkPull(t, workTarOf(t))
	p.exportState["baseImage"] = "no-such-image"
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--name", "x")
	if len(p.downloads) != 0 || len(vm.starts) != 0 {
		t.Fatalf("downloads %v starts %v", p.downloads, vm.starts)
	}
	if _, err := te.app.Engine.Lookup("x"); err == nil {
		t.Fatal("no dock may be left")
	}
}

func TestPullGVisorRemoteIntoQEMUFailureCleansUp(t *testing.T) {
	// The guest fails to unpack: the new dock is removed, its VM is stopped and no temp file stays.
	te, _, vm, _ := qemuWorkPull(t, workTarOf(t, tar.Header{Name: "./a.txt", Mode: 0o644}))
	vm.failImport = true
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--name", "doomed")
	if !strings.Contains(te.errb.String(), "import_failed") {
		t.Fatalf("stderr %q", te.errb)
	}
	if docks, err := te.app.Engine.List(t.Context()); err != nil || len(docks) != 0 {
		t.Fatalf("docks %v err %v", docks, err)
	}
	if ents, _ := os.ReadDir(filepath.Join(te.app.Engine.Dir, "docks")); len(ents) != 0 {
		t.Fatalf("dock directories left: %v", ents)
	}
	for pid := range vm.alive {
		if vm.Alive(pid) {
			t.Fatalf("VM %d still runs", pid)
		}
	}
	if len(tmpEntries(t, te)) != 0 {
		t.Fatalf("temp files left behind: %v", tmpEntries(t, te))
	}
	// The name is free again.
	vm.failImport = false
	mustExit(t, te, 0, "pull", remoteID, "--from", "lab", "--name", "doomed")
}

func TestPullGVisorRemoteIntoQEMURejectsAHostileTar(t *testing.T) {
	te, _, vm, _ := qemuWorkPull(t, workTarOf(t, tar.Header{Name: "../evil", Mode: 0o644}))
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--name", "evil")
	if !strings.Contains(te.errb.String(), "invalid_archive") || len(vm.starts) != 0 {
		t.Fatalf("stderr %q starts %v", te.errb, vm.starts)
	}
	if _, err := te.app.Engine.Lookup("evil"); err == nil {
		t.Fatal("no dock may be left")
	}
	if len(tmpEntries(t, te)) != 0 {
		t.Fatalf("temp files left behind: %v", tmpEntries(t, te))
	}
}

func TestPullGVisorRemoteIntoQEMUPreflightFailsBeforeTheExport(t *testing.T) {
	te, p, vm, _ := qemuWorkPull(t, workTarOf(t))
	vm.preflight = errors.New("no hypervisor")
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--name", "x")
	if p.exportPosts != 0 {
		t.Fatalf("exports %d", p.exportPosts)
	}
}

func TestPullGVisorRemoteIntoQEMUOldControlPlane(t *testing.T) {
	te, p, vm, _ := qemuWorkPull(t, workTarOf(t))
	p.exportErr = &apiErr{code: "runtime_unsupported", msg: "client too old"}
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--name", "x")
	if !strings.Contains(te.errb.String(), "cannot export gVisor docks yet") || len(vm.starts) != 0 {
		t.Fatalf("stderr %q", te.errb)
	}
}
