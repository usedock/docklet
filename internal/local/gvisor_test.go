package local

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDocker simulates the docker CLI: run starts a container whose agent answers on the
// published port, and it records every call.
type fakeDocker struct {
	mu         sync.Mutex
	calls      [][]string
	containers map[string]net.Listener
	volumes    map[string]bool
	images     map[string]bool
	builds     int
	runtimes   string // docker info output
	runErr     error
	exitOnRun  bool // container is created but exits at once
	agentDead  bool // container runs but the agent never answers
	buildStdin []byte
	drv        *fakeDriver
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{containers: map[string]net.Listener{}, volumes: map[string]bool{}, images: map[string]bool{},
		runtimes: `{"runc":{"path":"runc"},"runsc":{"path":"/usr/local/bin/runsc"}}`, drv: newFakeDriver()}
}

var publishRE = regexp.MustCompile(`^127\.0\.0\.1:(\d+):2222$`)

func (f *fakeDocker) run(_ context.Context, stdin io.Reader, args ...string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	last := args[len(args)-1]
	gone := errors.New("exit status 1")
	switch args[0] {
	case "info":
		return f.runtimes, "", nil
	case "image":
		if f.images[last] {
			return "", "", nil
		}
		return "", "Error: No such image: " + last, gone
	case "build":
		f.builds++
		f.buildStdin, _ = io.ReadAll(stdin)
		f.images[args[2]] = true
		return "", "", nil
	case "run":
		if f.runErr != nil {
			return "", "docker: " + f.runErr.Error(), f.runErr
		}
		var name, port, cfg string
		for i, a := range args {
			switch {
			case a == "--name":
				name = args[i+1]
			case a == "-p":
				port = publishRE.FindStringSubmatch(args[i+1])[1]
			case strings.HasSuffix(a, ":/etc/dock-fast-agent.json:ro"):
				cfg = strings.TrimSuffix(a, ":/etc/dock-fast-agent.json:ro")
			case a == "-v" && strings.Contains(args[i+1], "-work:/home/user/work"):
				f.volumes[strings.SplitN(args[i+1], ":", 2)[0]] = true
			}
		}
		var l net.Listener
		if !f.exitOnRun && !f.agentDead {
			raw, err := os.ReadFile(cfg)
			if err != nil {
				return "", err.Error(), err
			}
			var c GuestConfig
			_ = json.Unmarshal(raw, &c)
			if l, err = net.Listen("tcp", "127.0.0.1:"+port); err != nil {
				return "", "port is already allocated", err
			}
			go f.drv.serve(0, l, c.Token)
		}
		f.containers[name] = l
		return "cid\n", "", nil
	case "inspect":
		if _, ok := f.containers[last]; !ok || f.exitOnRun {
			return "false\n", "Error: No such container", gone
		}
		return "true\n", "", nil
	case "stop":
		return "", "", nil
	case "rm":
		l, ok := f.containers[last]
		if !ok {
			return "", "Error: No such container: " + last, gone
		}
		if l != nil {
			l.Close()
		}
		delete(f.containers, last)
		return "", "", nil
	case "volume":
		delete(f.volumes, last)
		return "", "", nil
	case "logs":
		return "", "boom from the container", nil
	}
	return "", "", errors.New("unexpected docker " + args[0])
}

func (f *fakeDocker) callsWith(verb string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if c[0] == verb {
			out = append(out, c)
		}
	}
	return out
}

func newGVisorEngine(t *testing.T) (*Engine, *fakeDocker) {
	t.Helper()
	e, _, _ := newTestEngine(t)
	fd := newFakeDocker()
	e.GVisor = &GVisor{GOOS: "linux", Run: fd.run, LookPath: func(string) (string, error) { return "/usr/bin/docker", nil }}
	return e, fd
}

func TestGVisorRoundTrip(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	d, err := e.New(ctx, Spec{Name: "gv", Runtime: RuntimeGVisor, CPUs: 3, MemoryMiB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if d.Runtime != "gvisor" || d.State != "ready" {
		t.Fatalf("dock %+v", d)
	}
	run := fd.callsWith("run")
	if len(run) != 1 {
		t.Fatalf("runs %v", run)
	}
	argv := strings.Join(run[0], " ")
	port := e.readInt(d.ID, "port")
	cfg := e.file(d.ID, "fast-agent.json")
	for _, want := range []string{
		"run -d --runtime=runsc --name dock-local-" + d.ID,
		"-p 127.0.0.1:" + itoa(port) + ":2222",
		"--cpus 3", "--memory 1024m", "--user 1001:1001", "--cap-drop=ALL", "--security-opt no-new-privileges",
		"--pids-limit 1024", "--tmpfs /tmp",
		"-v dock-local-" + d.ID + "-work:/home/user/work",
		"-v " + cfg + ":/etc/dock-fast-agent.json:ro",
		" dock-local-gvisor:",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv lacks %q: %s", want, argv)
		}
	}
	if strings.Contains(argv, "--privileged") {
		t.Error("never privileged")
	}
	tok, _ := e.token(d.ID)
	if raw, _ := os.ReadFile(cfg); !strings.Contains(string(raw), tok) || strings.Contains(argv, tok) {
		t.Fatalf("agent config %q must hold the token and the argv must not", raw)
	}

	res, err := e.Exec(ctx, "gv", "echo hi", 0)
	if err != nil || !strings.Contains(res.Stdout, "ran:echo hi") {
		t.Fatalf("exec %+v %v", res, err)
	}

	st, err := e.Stop(ctx, "gv", false)
	if err != nil || st.State != "archived" {
		t.Fatalf("stop %+v %v", st, err)
	}
	if !fd.volumes["dock-local-"+d.ID+"-work"] {
		t.Fatal("stop must keep the work volume")
	}
	if len(fd.containers) != 0 {
		t.Fatalf("stop must remove the container: %v", fd.containers)
	}
	if _, err := e.Exec(ctx, "gv", "x", 0); codeOf(err) != "dock_not_running" {
		t.Fatalf("exec on a stopped dock: %v", err)
	}

	rd, err := e.Resume(ctx, "gv")
	if err != nil || rd.State != "ready" || rd.Runtime != "gvisor" {
		t.Fatalf("resume %+v %v", rd, err)
	}
	if n := len(fd.callsWith("run")); n != 2 {
		t.Fatalf("resume must run a new container, runs %d", n)
	}
	if fd.builds != 1 {
		t.Fatalf("image must be built once, builds %d", fd.builds)
	}

	if err := e.Remove(ctx, "gv"); err != nil {
		t.Fatal(err)
	}
	if fd.volumes["dock-local-"+d.ID+"-work"] || len(fd.containers) != 0 {
		t.Fatalf("rm must remove container and volume: %v %v", fd.containers, fd.volumes)
	}
	if recs, _ := e.store().List(); len(recs) != 0 {
		t.Fatalf("records %v", recs)
	}
	if _, err := os.Stat(e.store().DockDir(d.ID)); !os.IsNotExist(err) {
		t.Fatal("dock dir must be gone")
	}
}

func TestGVisorRemoveIsIdempotentWhenTheContainerIsGone(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	if _, err := e.New(ctx, Spec{Name: "gone", Runtime: RuntimeGVisor}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Stop(ctx, "gone", true); err != nil {
		t.Fatal(err)
	}
	// Container and volume are already gone (removed behind our back).
	fd.volumes = map[string]bool{}
	if err := e.Remove(ctx, "gone"); err != nil {
		t.Fatalf("rm of an already removed container: %v", err)
	}
}

func TestGVisorImageBuiltOnlyWhenMissing(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	var progress strings.Builder
	e.Progress = &progress
	if _, err := e.New(ctx, Spec{Name: "a", Runtime: RuntimeGVisor}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.New(ctx, Spec{Name: "b", Runtime: RuntimeGVisor}); err != nil {
		t.Fatal(err)
	}
	if fd.builds != 1 || strings.Count(progress.String(), "building the gVisor dock image") != 1 {
		t.Fatalf("builds %d, progress %q", fd.builds, progress.String())
	}
	tag, _ := GVisorImageTag()
	if !fd.images[tag] || !regexp.MustCompile(`^dock-local-gvisor:[0-9a-f]{12}$`).MatchString(tag) {
		t.Fatalf("tag %q images %v", tag, fd.images)
	}
	for _, c := range fd.callsWith("build") {
		if strings.Join(c, " ") != "build -t "+tag+" -" {
			t.Fatalf("build argv %v", c)
		}
	}
	for _, name := range []string{"Dockerfile", "dock-fast-agent.py", "dock-entrypoint"} {
		if !strings.Contains(string(fd.buildStdin), name) {
			t.Errorf("build context lacks %s", name)
		}
	}
}

func TestGVisorPreflightClassification(t *testing.T) {
	ctx := context.Background()
	okLook := func(string) (string, error) { return "docker", nil }
	info := func(out, errOut string, err error) DockerRun {
		return func(context.Context, io.Reader, ...string) (string, string, error) { return out, errOut, err }
	}
	cases := []struct {
		name string
		g    *GVisor
		code string
		has  string
	}{
		{"not linux", &GVisor{GOOS: "windows", LookPath: okLook}, "unsupported_os", "Linux"},
		{"no docker", &GVisor{GOOS: "linux", LookPath: func(string) (string, error) { return "", errors.New("nope") }}, "docker_not_found", "gvisor.dev"},
		{"daemon down", &GVisor{GOOS: "linux", LookPath: okLook,
			Run: info("", "permission denied while trying to connect to the Docker daemon socket", errors.New("exit status 1"))}, "docker_unavailable", "permission denied"},
		{"no runsc", &GVisor{GOOS: "linux", LookPath: okLook, Run: info(`{"runc":{"path":"runc"}}`, "", nil)}, "runsc_not_found", "sudo runsc install"},
		{"garbage", &GVisor{GOOS: "linux", LookPath: okLook, Run: info("not json", "", nil)}, "docker_unavailable", "unexpected output"},
		{"ok", &GVisor{GOOS: "linux", LookPath: okLook, Run: info(`{"runc":{},"runsc":{}}`, "", nil)}, "", ""},
	}
	for _, c := range cases {
		err := c.g.Preflight(ctx)
		if codeOf(err) != c.code {
			t.Errorf("%s: code %q, err %v", c.name, codeOf(err), err)
		}
		if c.has != "" && (err == nil || !strings.Contains(err.Error(), c.has)) {
			t.Errorf("%s: error %v lacks %q", c.name, err, c.has)
		}
	}
}

func TestGVisorPreflightFailsBeforeAnyBuildOrRecord(t *testing.T) {
	e, fd := newGVisorEngine(t)
	fd.runtimes = `{"runc":{}}`
	_, err := e.New(context.Background(), Spec{Name: "x", Runtime: RuntimeGVisor})
	if codeOf(err) != "runsc_not_found" {
		t.Fatalf("got %v", err)
	}
	if len(fd.callsWith("build")) != 0 || len(fd.callsWith("run")) != 0 || len(fd.callsWith("image")) != 0 {
		t.Fatalf("nothing may run before preflight passes: %v", fd.calls)
	}
	if recs, _ := e.store().List(); len(recs) != 0 {
		t.Fatalf("records %v", recs)
	}
}

func TestGVisorUnsupportedVerbsAndOptions(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	if _, err := e.New(ctx, Spec{Runtime: RuntimeGVisor, Network: "off"}); codeOf(err) != "not_supported" || !strings.Contains(err.Error(), "--network off") {
		t.Fatalf("network off: %v", err)
	}
	if len(fd.calls) != 0 {
		t.Fatalf("a rejected spec must not touch docker: %v", fd.calls)
	}
	if _, err := e.New(ctx, Spec{Runtime: "lxc"}); codeOf(err) != "invalid_runtime" {
		t.Fatalf("unknown runtime: %v", err)
	}
	if _, err := e.New(ctx, Spec{Name: "gv", Runtime: RuntimeGVisor}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Branch(ctx, "gv", 1, "", Spec{}); codeOf(err) != "not_supported" {
		t.Fatalf("branch: %v", err)
	}
	rec, _ := e.Lookup("gv")
	if err := e.CheckPushSpace(rec); codeOf(err) != "not_supported" {
		t.Fatalf("push space: %v", err)
	}
	if _, err := e.Flatten(ctx, "gv", t.TempDir(), true); codeOf(err) != "not_supported" {
		t.Fatalf("push: %v", err)
	}
}

func TestGVisorDiskFlagWarns(t *testing.T) {
	e, _ := newGVisorEngine(t)
	var progress strings.Builder
	e.Progress = &progress
	if _, err := e.New(context.Background(), Spec{Runtime: RuntimeGVisor, DiskGiB: 20}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(progress.String(), "--disk is not enforced") {
		t.Fatalf("progress %q", progress.String())
	}
	e2, _ := newGVisorEngine(t)
	progress.Reset()
	e2.Progress = &progress
	if _, err := e2.New(context.Background(), Spec{Runtime: RuntimeGVisor}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(progress.String(), "--disk") {
		t.Fatalf("no warning without --disk: %q", progress.String())
	}
}

func TestGVisorBootFailuresCleanUp(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	fd.exitOnRun = true
	_, err := e.New(ctx, Spec{Runtime: RuntimeGVisor})
	if codeOf(err) != "boot_failed" || !strings.Contains(err.Error(), "boom from the container") {
		t.Fatalf("exited container: %v", err)
	}
	if recs, _ := e.store().List(); len(recs) != 0 || len(fd.containers) != 0 || len(fd.volumes) != 0 {
		t.Fatalf("failed dock must leave nothing: %v %v %v", recs, fd.containers, fd.volumes)
	}

	e2, fd2 := newGVisorEngine(t)
	fd2.agentDead = true
	e2.BootTimeout = 700 * time.Millisecond
	if _, err := e2.New(ctx, Spec{Runtime: RuntimeGVisor}); codeOf(err) != "boot_timeout" {
		t.Fatalf("silent agent: %v", err)
	}
	if len(fd2.containers) != 0 {
		t.Fatal("a timed out container must be removed")
	}

	e3, fd3 := newGVisorEngine(t)
	fd3.runErr = errors.New("exit status 125")
	if _, err := e3.New(ctx, Spec{Runtime: RuntimeGVisor}); codeOf(err) != "boot_failed" {
		t.Fatalf("docker run failure: %v", err)
	}
}

func TestGVisorRetriesWhenThePortIsTaken(t *testing.T) {
	e, fd := newGVisorEngine(t)
	fd.runErr = errors.New("port is already allocated")
	if _, err := e.New(context.Background(), Spec{Runtime: RuntimeGVisor}); codeOf(err) != "boot_failed" {
		t.Fatalf("got %v", err)
	}
	if n := len(fd.callsWith("run")); n != bootAttempts {
		t.Fatalf("retries must be bounded at %d, runs %d", bootAttempts, n)
	}
}

func TestRecordRuntimeBackCompat(t *testing.T) {
	e, _ := newGVisorEngine(t)
	old := `[{"id":"dk_old","name":"old","image":"x","imageDir":"/i","cpus":1,"memoryMiB":512,"diskGiB":1,"network":"on","createdAt":"2026-01-01T00:00:00Z"}]`
	if err := os.WriteFile(e.Dir+"/docks.json", []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	docks, err := e.List(context.Background())
	if err != nil || len(docks) != 1 || docks[0].Runtime != "qemu" {
		t.Fatalf("a record without runtime is qemu: %+v %v", docks, err)
	}
	raw, _ := json.Marshal(Record{ID: "a"})
	if strings.Contains(string(raw), "runtime") {
		t.Fatalf("empty runtime must be omitted: %s", raw)
	}
}
