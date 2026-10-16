package local

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// importDocker wraps the fake docker: the import helper (docker run --rm with the disk mounted)
// is answered here and recorded, everything else goes to the fake.
type importDocker struct {
	*fakeDocker
	helper         [][]string
	stdout, stderr string
	err            error
}

func (d *importDocker) run(ctx context.Context, stdin io.Reader, args ...string) (string, string, error) {
	if args[0] == "run" && slices.Contains(args, "--rm") && slices.ContainsFunc(args, func(a string) bool { return strings.HasSuffix(a, ":"+gvDiskPath+":ro") }) {
		d.fakeDocker.mu.Lock()
		d.helper = append(d.helper, args)
		d.fakeDocker.calls = append(d.fakeDocker.calls, args)
		if d.err == nil {
			for i, a := range args {
				if a == "-v" && strings.HasSuffix(args[i+1], ":"+gvWorkSrc) {
					d.fakeDocker.volumes[strings.TrimSuffix(args[i+1], ":"+gvWorkSrc)] = true
				}
			}
		}
		d.fakeDocker.mu.Unlock()
		return d.stdout, d.stderr, d.err
	}
	return d.fakeDocker.run(ctx, stdin, args...)
}

func newImportEngine(t *testing.T) (*Engine, *importDocker, *bytes.Buffer) {
	t.Helper()
	e, fd := newGVisorEngine(t)
	id := &importDocker{fakeDocker: fd}
	e.GVisor.Run = id.run
	var log bytes.Buffer
	e.Progress = &log
	return e, id, &log
}

func rawDisk(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "disk.raw")
	if err := os.WriteFile(p, []byte("ext4"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestImportWorkHelperArgs(t *testing.T) {
	ctx := context.Background()
	e, d, _ := newImportEngine(t)
	disk := rawDisk(t)
	dock, err := e.ImportWork(ctx, ImportWork{Name: "pulled", RawPath: disk, CPUs: 2, MemoryMiB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if dock.Runtime != RuntimeGVisor || dock.State != "archived" {
		t.Fatalf("a pulled gvisor dock must be registered stopped: %+v", dock)
	}
	rec, err := e.Lookup("pulled")
	if err != nil || rec.Image != "dock-local-gvisor" || rec.Runtime != RuntimeGVisor || rec.CPUs != 2 || rec.MemoryMiB != 1024 || rec.Network != "on" {
		t.Fatalf("record %+v %v", rec, err)
	}
	if len(d.helper) != 1 {
		t.Fatalf("helper calls: %v", d.helper)
	}
	a := d.helper[0]
	joined := " " + strings.Join(a, " ") + " "
	for _, want := range []string{
		" --runtime=" + rec.gvRuntime() + " ", " --network none ", " --user 0:0 ", " --cap-drop ALL ",
		" --cap-add CHOWN ", " --cap-add DAC_OVERRIDE ", " --cap-add FOWNER ", " no-new-privileges ",
		" " + gvVolume(rec.ID) + ":/work ", " --entrypoint sh ",
		// The untrusted disk can only write to the work volume, and a failed import can remove the helper.
		" --read-only ", " --name " + gvImporter(rec.ID) + " ",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("helper args lack %q: %v", want, a)
		}
	}
	abs, _ := filepath.Abs(disk)
	if !slices.Contains(a, abs+":/x/disk.raw:ro") {
		t.Errorf("the disk must be mounted read-only at /x/disk.raw: %v", a)
	}
	if strings.Count(joined, " --cap-add ") != 3 {
		t.Errorf("only CHOWN, DAC_OVERRIDE and FOWNER are added: %v", a)
	}
	script := a[slices.Index(a, "-c")+1]
	for _, want := range []string{`rdump /home/user/work`, "chown -hR --from=1000 1001 /work", "chown -hR --from=:1000 :1001 /work", "-type l -user 0 -exec chown -h 1001:1001", "-mindepth 1 -maxdepth 1"} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	// No qemu: no conversion and no VM.
	if _, err := os.Stat(e.file(rec.ID, "disk.qcow2")); err == nil {
		t.Error("a gvisor import must not create a qcow2")
	}
	if len(e.Driver.(*fakeDriver).starts) != 0 {
		t.Error("a gvisor import must not start a VM")
	}
	if len(d.callsWith("run")) != 1 {
		t.Errorf("the import must not start the dock: %v", d.callsWith("run"))
	}
}

func TestImportWorkFirstResumeBootsFreshWithoutGoneLog(t *testing.T) {
	ctx := context.Background()
	e, d, log := newImportEngine(t)
	if _, err := e.ImportWork(ctx, ImportWork{Name: "pulled", RawPath: rawDisk(t)}); err != nil {
		t.Fatal(err)
	}
	st, err := e.Resume(ctx, "pulled")
	if err != nil || st.State != "ready" {
		t.Fatalf("resume: %+v %v", st, err)
	}
	if strings.Contains(log.String(), "is gone") {
		t.Fatalf("an imported dock's first resume must not claim its container is gone:\n%s", log)
	}
	if !strings.Contains(log.String(), "imported work directory") {
		t.Fatalf("expected a fresh-start log line:\n%s", log)
	}
	rec, _ := e.Lookup("pulled")
	if !d.volumes[gvVolume(rec.ID)] {
		t.Fatal("the dock must run on the imported volume")
	}
	if _, err := os.Stat(e.file(rec.ID, gvFreshMarker)); err == nil {
		t.Fatal("the marker must go once the dock has booted")
	}
	// A later lost container is reported again.
	if _, err := e.Stop(ctx, "pulled", false); err != nil {
		t.Fatal(err)
	}
	delete(d.containers, gvContainer(rec.ID))
	delete(d.stopped, gvContainer(rec.ID))
	log.Reset()
	if _, err := e.Resume(ctx, "pulled"); err != nil || !strings.Contains(log.String(), "is gone") {
		t.Fatalf("a lost container must still be reported: %v\n%s", err, log)
	}
}

func TestImportWorkNetworkOff(t *testing.T) {
	e, _, _ := newImportEngine(t)
	if _, err := e.ImportWork(context.Background(), ImportWork{Name: "off", RawPath: rawDisk(t), Network: "off"}); err != nil {
		t.Fatal(err)
	}
	if rec, _ := e.Lookup("off"); rec.Network != "off" {
		t.Fatalf("network: %+v", rec)
	}
}

func TestImportWorkNameCheckRunsFirst(t *testing.T) {
	ctx := context.Background()
	e, d, _ := newImportEngine(t)
	if _, err := e.ImportWork(ctx, ImportWork{Name: "bad name!", RawPath: rawDisk(t)}); ErrCode(err) != "invalid_argument" {
		t.Fatalf("invalid name: %v", err)
	}
	if _, err := e.ImportWork(ctx, ImportWork{Name: "taken", RawPath: rawDisk(t)}); err != nil {
		t.Fatal(err)
	}
	d.fakeDocker.calls = nil
	if _, err := e.ImportWork(ctx, ImportWork{Name: "taken", RawPath: rawDisk(t)}); ErrCode(err) != "dock_name_taken" {
		t.Fatalf("taken name: %v", err)
	}
	if len(d.fakeDocker.calls) != 0 {
		t.Fatalf("a bad name must fail before docker is touched: %v", d.fakeDocker.calls)
	}
}

func TestImportWorkRejectsBadSettings(t *testing.T) {
	e, d, _ := newImportEngine(t)
	if _, err := e.ImportWork(context.Background(), ImportWork{Name: "x", RawPath: rawDisk(t), CPUs: 99}); ErrCode(err) != "invalid_argument" || len(d.fakeDocker.calls) != 0 {
		t.Fatalf("cpus: %v %v", err, d.fakeDocker.calls)
	}
}

func TestImportWorkFailureLeavesNothing(t *testing.T) {
	cases := map[string]func(d *importDocker){
		"helper fails": func(d *importDocker) { d.err, d.stderr = errors.New("exit status 1"), "chown: boom" },
		"debugfs error": func(d *importDocker) {
			d.stderr = gvDebugfsPrefix + "rdump: Ext2 inode is not a directory while dumping\n"
		},
		"daemon down": func(d *importDocker) {
			d.err, d.stderr = errors.New("exit status 1"), "Cannot connect to the Docker daemon"
		},
		"debugfs bad magic": func(d *importDocker) {
			d.stderr = gvDebugfsPrefix + "debugfs: Bad magic number in super-block while trying to open /x/disk.raw\n"
		},
		"cancelled": func(d *importDocker) { d.err = context.Canceled },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e, d, _ := newImportEngine(t)
			setup(d)
			_, err := e.ImportWork(context.Background(), ImportWork{Name: "doomed", RawPath: rawDisk(t)})
			if err == nil {
				t.Fatal("expected a failure")
			}
			if len(d.helper) != 1 {
				t.Fatalf("helper calls: %d", len(d.helper))
			}
			if recs, _ := e.List(context.Background()); len(recs) != 0 {
				t.Fatalf("the record must be removed: %+v", recs)
			}
			if len(d.volumes) != 0 {
				t.Fatalf("the volume must be removed: %v", d.volumes)
			}
			entries, _ := os.ReadDir(e.Dir)
			for _, en := range entries {
				if strings.HasPrefix(en.Name(), "dk_") {
					t.Fatalf("the dock directory must be removed: %s", en.Name())
				}
			}
			// The helper container goes first: a cancelled docker run leaves it running on the volume.
			helperRm, volRm := -1, -1
			for i, c := range d.fakeDocker.calls {
				if c[0] == "rm" && slices.Contains(c, "-f") && strings.HasSuffix(c[len(c)-1], "-import") && helperRm < 0 {
					helperRm = i
				}
				if c[0] == "volume" && c[1] == "rm" && strings.HasSuffix(c[len(c)-1], "-work") {
					volRm = i
				}
			}
			if volRm < 0 {
				t.Fatal("docker volume rm was not called")
			}
			if helperRm < 0 || helperRm > volRm {
				t.Fatalf("docker rm -f of the import helper must come before the volume rm: %v", d.fakeDocker.calls)
			}
		})
	}
}

func TestImportWorkDebugfsBannerIsNotAnError(t *testing.T) {
	e, d, _ := newImportEngine(t)
	d.stderr = gvDebugfsPrefix + "debugfs 1.47.0 (5-Feb-2023)\nWARNING: docker noise\n"
	if _, err := e.ImportWork(context.Background(), ImportWork{Name: "ok", RawPath: rawDisk(t)}); err != nil {
		t.Fatal(err)
	}
}

func TestImportWorkMissingWorkdirIsAnEmptyDock(t *testing.T) {
	e, d, log := newImportEngine(t)
	d.stdout = gvNoWorkdirMarker + "\n"
	if _, err := e.ImportWork(context.Background(), ImportWork{Name: "empty", RawPath: rawDisk(t)}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "no /home/user/work") {
		t.Fatalf("expected a log line:\n%s", log)
	}
	if rec, err := e.Lookup("empty"); err != nil || !d.volumes[gvVolume(rec.ID)] {
		t.Fatalf("the dock and volume must exist: %v", err)
	}
}

func TestImportScriptShape(t *testing.T) {
	// The script handles a disk without the work directory before it stages anything.
	i := strings.Index(gvImportScript, "File not found")
	j := strings.Index(gvImportScript, "rdump")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("script:\n%s", gvImportScript)
	}
	// The volume root is handed to the dock user even when nothing was copied.
	if !strings.Contains(gvImportScript[strings.Index(gvImportScript, "esac"):], "chown 1001:1001 /work") {
		t.Fatalf("script:\n%s", gvImportScript)
	}
}

func TestDebugfsRdumpErrors(t *testing.T) {
	got := debugfsRdumpErrors("docker: noise\n" + gvDebugfsPrefix + "debugfs 1.47.0 (5-Feb-2023)\n" + gvDebugfsPrefix + "\n" + gvDebugfsPrefix + "rdump: Operation not permitted while changing ownership of x\n")
	if len(got) != 1 || !strings.HasPrefix(got[0], "rdump: Operation not permitted") {
		t.Fatalf("got %q", got)
	}
	if got := debugfsRdumpErrors(""); len(got) != 0 {
		t.Fatalf("got %q", got)
	}
}

func TestPullRuntime(t *testing.T) {
	ctx := context.Background()
	noAccel := errf("no_accelerator", "no KVM here.")
	cases := []struct {
		name, flag, env, want, code string
		pre                         error
		hint                        bool
	}{
		{name: "default is qemu", want: RuntimeQEMU},
		{name: "env gvisor", env: "gvisor", want: RuntimeGVisor, pre: noAccel},
		{name: "flag beats env", flag: "gvisor", env: "qemu", want: RuntimeGVisor, pre: noAccel},
		{name: "flag qemu beats env gvisor", flag: "qemu", env: "gvisor", want: RuntimeQEMU},
		{name: "no accelerator", pre: noAccel, code: "no_accelerator", hint: true},
		{name: "qemu missing", env: "qemu", pre: errf("qemu_not_found", "qemu-system-x86_64 was not found"), code: "qemu_not_found", hint: true},
		{name: "other preflight error has no hint", pre: errf("boot_failed", "weird"), code: "boot_failed"},
		{name: "invalid runtime", flag: "docker", code: "invalid_runtime"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, _ := newGVisorEngine(t)
			drv := e.Driver.(*fakeDriver)
			drv.preflightErr = c.pre
			got, err := e.PullRuntime(ctx, c.flag, c.env)
			if c.code == "" {
				if err != nil || got != c.want {
					t.Fatalf("got %q %v", got, err)
				}
				return
			}
			if ErrCode(err) != c.code {
				t.Fatalf("code %q, err %v", ErrCode(err), err)
			}
			if has := strings.Contains(err.Error(), "pass --runtime gvisor"); has != c.hint {
				t.Fatalf("hint=%v: %v", has, err)
			}
		})
	}
}

func TestPullRuntimeGVisorPreflight(t *testing.T) {
	ctx := context.Background()
	e, _ := newGVisorEngine(t)
	if got, err := e.PullRuntime(ctx, RuntimeGVisor, ""); err != nil || got != RuntimeGVisor {
		t.Fatalf("got %q %v", got, err)
	}
	// gVisor that cannot run here fails before the export, not after it.
	e.GVisor.GOOS = "darwin"
	if _, err := e.PullRuntime(ctx, "", RuntimeGVisor); ErrCode(err) != "unsupported_os" {
		t.Fatalf("want unsupported_os, got %v", err)
	}
	e.GVisor.GOOS = "linux"
	e.GVisor.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	if _, err := e.PullRuntime(ctx, RuntimeGVisor, ""); ErrCode(err) != "docker_not_found" {
		t.Fatalf("want docker_not_found, got %v", err)
	}
}
