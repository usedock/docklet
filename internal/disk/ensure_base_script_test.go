package disk

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ensureBaseRig runs a copy of scripts/ensure-base.sh beside stub siblings that
// log what they were asked to do, so selection and fallback are observable
// without Dockyard, a host kernel or a 20GB build.
type ensureBaseRig struct {
	t      *testing.T
	scr    string
	out    string
	log    string
	bin    string
	pathEx string
}

func newEnsureBaseRig(t *testing.T, pullExit int) *ensureBaseRig {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	r := &ensureBaseRig{t: t, scr: filepath.Join(dir, "scripts"), out: filepath.Join(dir, "images"), log: filepath.Join(dir, "calls.log"), bin: filepath.Join(dir, "docklet"), pathEx: filepath.Join(dir, "fakebin")}
	for _, d := range []string{r.scr, r.out, r.pathEx} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src, err := os.ReadFile(filepath.Join("..", "..", "scripts", "ensure-base.sh"))
	if err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(r.scr, "ensure-base.sh"), string(src))
	write(filepath.Join(r.scr, "install-kernel.sh"), "#!/usr/bin/env bash\necho install-kernel >> \""+r.log+"\"\ntouch \"$OUT_DIR/vmlinux\"\n")
	write(filepath.Join(r.scr, "extract-vmlinux.sh"), "#!/usr/bin/env bash\nexit 0\n")
	write(filepath.Join(r.scr, "build-image.sh"), "#!/usr/bin/env bash\necho build-image >> \""+r.log+"\"\ntouch \"$OUT_DIR/base.ext4\" \"$OUT_DIR/vmlinux\"\n")
	write(filepath.Join(r.pathEx, "debootstrap"), "#!/usr/bin/env bash\nexit 0\n")
	pull := "#!/usr/bin/env bash\necho \"docklet $*\" >> \"" + r.log + "\"\n"
	if pullExit == 0 {
		pull += "touch \"$DOCKLET_IMAGE_DIR/base.ext4\"\n"
	}
	pull += "exit " + string(rune('0'+pullExit)) + "\n"
	write(r.bin, pull)
	return r
}

func (r *ensureBaseRig) run(env ...string) (string, error) {
	r.t.Helper()
	cmd := exec.Command("bash", filepath.Join(r.scr, "ensure-base.sh"))
	cmd.Env = append(os.Environ(), "OUT_DIR="+r.out, "DOCKLET_BIN="+r.bin, "PATH="+r.pathEx+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (r *ensureBaseRig) calls() []string {
	raw, _ := os.ReadFile(r.log)
	var calls []string
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if l != "" {
			calls = append(calls, l)
		}
	}
	return calls
}

func (r *ensureBaseRig) wantCalls(want ...string) {
	r.t.Helper()
	got := strings.Join(r.calls(), "|")
	if got != strings.Join(want, "|") {
		r.t.Fatalf("calls = %q, want %q", got, strings.Join(want, "|"))
	}
}

func TestEnsureBasePullsDockyardUbuntuByDefault(t *testing.T) {
	r := newEnsureBaseRig(t, 0)
	if out, err := r.run(); err != nil {
		t.Fatalf("ensure-base: %v\n%s", err, out)
	}
	r.wantCalls("docklet image pull ubuntu-24.04", "install-kernel")
	stamp, err := os.ReadFile(filepath.Join(r.out, ".kernel-release"))
	if err != nil || strings.TrimSpace(string(stamp)) == "" {
		t.Fatalf("kernel stamp = %q, %v", stamp, err)
	}
}

func TestEnsureBaseFallsBackToBuildingWhenDockyardIsUnreachable(t *testing.T) {
	r := newEnsureBaseRig(t, 1)
	out, err := r.run()
	if err != nil {
		t.Fatalf("ensure-base: %v\n%s", err, out)
	}
	r.wantCalls("docklet image pull ubuntu-24.04", "build-image")
	if !strings.Contains(out, "building the base image") {
		t.Fatalf("the fallback must say so:\n%s", out)
	}
}

func TestEnsureBaseBuildsWhenThereIsNoDockletBinary(t *testing.T) {
	r := newEnsureBaseRig(t, 0)
	if out, err := r.run("DOCKLET_BIN=" + filepath.Join(r.out, "missing")); err != nil {
		t.Fatalf("ensure-base: %v\n%s", err, out)
	}
	r.wantCalls("build-image")
}

func TestEnsureBaseCanForceTheHostBuild(t *testing.T) {
	for _, env := range []string{"DOCKLET_BUILD_BASE=1", "DOCKLET_REBUILD_IMAGE=1"} {
		r := newEnsureBaseRig(t, 0)
		if out, err := r.run(env); err != nil {
			t.Fatalf("%s: %v\n%s", env, err, out)
		}
		r.wantCalls("build-image")
	}
}

func TestEnsureBaseKeepsABaseTheHostAlreadyHolds(t *testing.T) {
	r := newEnsureBaseRig(t, 0)
	for _, f := range []string{"base.ext4", "vmlinux"} {
		if err := os.WriteFile(filepath.Join(r.out, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := r.run(); err != nil {
		t.Fatalf("ensure-base: %v\n%s", err, out)
	}
	// An existing host keeps its host-built base: nothing pulled, built or restaged.
	r.wantCalls()
	got, _ := os.ReadFile(filepath.Join(r.out, "base.ext4"))
	if string(got) != "x" {
		t.Fatal("the existing base was replaced")
	}
}
