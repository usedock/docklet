package cli

import (
	"archive/tar"
	"bytes"
	"slices"
	"strings"
	"testing"
)

const workNote = "note: the remote dock runs on gVisor; the new dock starts from a fresh ubuntu-24.04 with only /home/user/work copied."

// workTarOf builds a work tar the way the fleet host does: "./"-prefixed names and a root entry.
func workTarOf(t *testing.T, hdrs ...tar.Header) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	all := append([]tar.Header{{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}}, hdrs...)
	for _, h := range all {
		h := h
		if h.Typeflag == tar.TypeReg || h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
			h.Size = 5
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte("hello"))
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// gvisorRemote makes the fake plane offer a work-tar export of a gVisor dock.
func gvisorRemote(t *testing.T, p *fakePlane, tarball string) {
	t.Helper()
	p.dockRuntime = "gvisor"
	p.readyExport(zstdOf(t, tarball), "ubuntu-24.04")
	p.exportState["format"] = "work-tar"
	p.exportState["dataBytes"] = len(tarball)
	p.exportObject = "state.tar.zst"
}

func TestPullGVisorRemoteWorkTar(t *testing.T) {
	te, p, g := gvisorPullEnv(t)
	gvisorRemote(t, p, workTarOf(t, tar.Header{Name: "./a.txt", Mode: 0o644}, tar.Header{Name: "./d/", Typeflag: tar.TypeDir, Mode: 0o700}))
	mustExit(t, te, 0, "pull", remoteID, "--from", "lab", "--name", "fromgv", "--runtime", "gvisor", "--json")
	if got, ok := p.exportBody["formats"].([]any); !ok || len(got) != 1 || got[0] != "work-tar" {
		t.Fatalf("export body %v", p.exportBody)
	}
	if !slices.Equal(p.objects, []string{"state.tar.zst"}) {
		t.Fatalf("downloaded %v", p.objects)
	}
	d := jsonOut(t, te).(map[string]any)
	if d["name"] != "fromgv" || d["runtime"] != "gvisor" || d["source"] != "work" || d["cpus"] != 3.0 {
		t.Fatalf("dock %v", d)
	}
	if !strings.Contains(te.errb.String(), workNote) || strings.Contains(te.errb.String(), pullNote) {
		t.Fatalf("stderr %q", te.errb)
	}
	if len(g.helpers) != 1 || !slices.ContainsFunc(g.helpers[0], func(a string) bool { return strings.HasSuffix(a, ":/x/work.tar:ro") }) {
		t.Fatalf("helpers %v", g.helpers)
	}
	if len(tmpEntries(t, te)) != 0 {
		t.Fatalf("temp files left behind: %v", tmpEntries(t, te))
	}
}

func TestPullGVisorRemoteWorkTarHumanOutputAndEnvRuntime(t *testing.T) {
	te, p, g := gvisorPullEnv(t)
	gvisorRemote(t, p, workTarOf(t))
	te.env["DOCK_LOCAL_RUNTIME"] = "gvisor"
	mustExit(t, te, 0, "pull", remoteID, "--from", "lab", "--name", "viaenv")
	if !strings.Contains(te.out.String(), "pulled") || !strings.Contains(te.errb.String(), workNote) || len(g.helpers) != 1 {
		t.Fatalf("stdout %q stderr %q", te.out, te.errb)
	}
}

func TestPullGVisorRemoteWorkTarChecksum(t *testing.T) {
	te, p, g := gvisorPullEnv(t)
	gvisorRemote(t, p, workTarOf(t))
	p.exportState["sha256"] = strings.Repeat("0", 64)
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--name", "bad", "--runtime", "gvisor")
	if len(g.helpers) != 0 {
		t.Fatalf("a bad checksum must not reach the helper: %v", g.helpers)
	}
}

func TestPullGVisorRemoteRefusalsAreMapped(t *testing.T) {
	for _, code := range []string{"runtime_unsupported", "not_supported"} {
		te, p, g := gvisorPullEnv(t)
		gvisorRemote(t, p, workTarOf(t))
		p.exportErr = &apiErr{code: code, msg: "client too old"}
		mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--runtime", "gvisor")
		if got := te.errb.String(); !strings.Contains(got, "this control plane (or the dock's host) cannot export gVisor docks yet") || strings.Contains(got, "push needs a microVM host") {
			t.Fatalf("%s: stderr %q", code, got)
		}
		if len(g.helpers) != 0 {
			t.Fatalf("helpers %v", g.helpers)
		}
	}
}

func TestPullGVisorRemoteFailedExportIsMapped(t *testing.T) {
	te, p, _ := gvisorPullEnv(t)
	p.dockRuntime = "gvisor"
	p.exportState = map[string]any{"state": "failed", "error": "runtime_unsupported"}
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--runtime", "gvisor")
	if !strings.Contains(te.errb.String(), "cannot export gVisor docks yet") {
		t.Fatalf("stderr %q", te.errb)
	}
}

func TestPullGVisorRemoteGotADisk(t *testing.T) {
	te, p, g := gvisorPullEnv(t)
	// An older control plane that ignores the formats and answers with a disk.
	p.dockRuntime = "gvisor"
	p.readyExport(zstdOf(t, diskBytes), "ubuntu-24.04")
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--runtime", "gvisor")
	if !strings.Contains(te.errb.String(), "cannot export gVisor docks yet") || len(g.helpers) != 0 || len(p.downloads) != 0 {
		t.Fatalf("stderr %q helpers %v downloads %v", te.errb, g.helpers, p.downloads)
	}
}

func TestPullGVisorRemoteIntoQEMUFailsBeforeTheExport(t *testing.T) {
	for _, tc := range []struct {
		args []string
		env  string
	}{
		{[]string{"pull", remoteID, "--from", "lab"}, ""},
		{[]string{"pull", remoteID, "--from", "lab", "--runtime", "qemu"}, ""},
		{[]string{"pull", remoteID, "--from", "lab"}, "qemu"},
	} {
		te, p, _ := gvisorPullEnv(t)
		gvisorRemote(t, p, workTarOf(t))
		if tc.env != "" {
			te.env["DOCK_LOCAL_RUNTIME"] = tc.env
		}
		mustExit(t, te, 1, tc.args...)
		got := te.errb.String()
		if !strings.Contains(got, "#128") || !strings.Contains(got, "--runtime gvisor") || p.exportPosts != 0 {
			t.Fatalf("%v: stderr %q exports %d", tc.args, got, p.exportPosts)
		}
	}
}

func TestPullGVisorRemoteRejectsAHostileTar(t *testing.T) {
	for name, tarball := range map[string]string{
		"dotdot":       workTarOf(t, tar.Header{Name: "../evil", Mode: 0o644}),
		"absolute":     workTarOf(t, tar.Header{Name: "/etc/passwd", Mode: 0o644}),
		"below a fifo": workTarOf(t, tar.Header{Name: "./p", Typeflag: tar.TypeFifo, Mode: 0o644}, tar.Header{Name: "./p/x", Mode: 0o644}),
	} {
		te, p, g := gvisorPullEnv(t)
		gvisorRemote(t, p, tarball)
		mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--name", "evil", "--runtime", "gvisor")
		if !strings.Contains(te.errb.String(), "invalid_archive") || len(g.helpers) != 0 {
			t.Fatalf("%s: stderr %q helpers %v", name, te.errb, g.helpers)
		}
		if _, err := te.app.Engine.Lookup("evil"); err == nil {
			t.Fatalf("%s: no dock may be left", name)
		}
		if len(tmpEntries(t, te)) != 0 {
			t.Fatalf("%s: temp files left behind: %v", name, tmpEntries(t, te))
		}
	}
}

func TestPullDiskReportsSourceAndSendsNoFormats(t *testing.T) {
	te, p, _ := gvisorPullEnv(t)
	p.readyExport(zstdOf(t, diskBytes), "ubuntu-24.04")
	mustExit(t, te, 0, "pull", remoteID, "--from", "lab", "--name", "plain", "--json")
	if d := jsonOut(t, te).(map[string]any); d["source"] != "disk" {
		t.Fatalf("dock %v", d)
	}
	if len(p.exportBody) != 0 {
		t.Fatalf("a disk pull must keep sending {}: %v", p.exportBody)
	}
}

func TestPullGVisorRemoteWorkTarWithoutDataBytes(t *testing.T) {
	// The control plane stores a zero data size as null; the disk size then caps the tar.
	for _, v := range []any{nil, 0, "absent"} {
		te, p, g := gvisorPullEnv(t)
		gvisorRemote(t, p, workTarOf(t, tar.Header{Name: "./a.txt", Mode: 0o644}))
		if v == "absent" {
			delete(p.exportState, "dataBytes")
		} else {
			p.exportState["dataBytes"] = v
		}
		mustExit(t, te, 0, "pull", remoteID, "--from", "lab", "--name", "nodata", "--runtime", "gvisor")
		if len(g.helpers) != 1 {
			t.Fatalf("dataBytes %v: helpers %v stderr %q", v, g.helpers, te.errb)
		}
	}
}

func TestPullGVisorRemoteSkipsSpecialFilesWithAWarning(t *testing.T) {
	te, p, g := gvisorPullEnv(t)
	gvisorRemote(t, p, workTarOf(t, tar.Header{Name: "./pipe", Typeflag: tar.TypeFifo, Mode: 0o644},
		tar.Header{Name: "./null", Typeflag: tar.TypeChar, Mode: 0o666}, tar.Header{Name: "./a.txt", Mode: 0o644}))
	te.app.Engine.Progress = te.errb // as app.engine wires it to stderr
	mustExit(t, te, 0, "pull", remoteID, "--from", "lab", "--name", "withpipe", "--runtime", "gvisor")
	want := `warning: skipped 2 special files (named pipes and device nodes are not copied): "/home/user/work/pipe", "/home/user/work/null"`
	if !strings.Contains(te.errb.String(), want) || len(g.helpers) != 1 {
		t.Fatalf("stderr %q helpers %v", te.errb, g.helpers)
	}
}
