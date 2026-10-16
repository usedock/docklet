package local

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
	networks   map[string]bool
	bridges    map[string]string // bridge interface name a network was created with, if any
	stopped    map[string]bool   // container exists but is not running
	attached   map[string]bool   // container is connected to its network
	ports      map[string]string // published host port per container
	tokens     map[string]string
	daemonDown bool
	images     map[string]bool
	builds     int
	runtimes   string // docker info output
	runErr     error
	exitOnRun  bool // container is created but exits at once
	agentDead  bool // container runs but the agent never answers
	buildStdin []byte
	commitErr  error // docker commit fails
	copyErr    error // the work-volume copy helper fails
	copyFailAt int   // with copyErr, only the nth copy (1-based) fails; 0 means every copy
	copies     int
	drv        *fakeDriver
	noNet      map[string]bool     // containers run with --network none
	streams    [][]string          // docker exec stream calls
	hostRules  map[string][]string // host block per chain: bridge, then DNS servers
	hostErr    error               // the host-block install helper fails
	hostRmErr  error               // the host-block remove helper fails
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{containers: map[string]net.Listener{}, volumes: map[string]bool{}, networks: map[string]bool{}, bridges: map[string]string{}, stopped: map[string]bool{}, attached: map[string]bool{}, noNet: map[string]bool{}, ports: map[string]string{}, tokens: map[string]string{}, images: map[string]bool{}, hostRules: map[string][]string{},
		runtimes: `{"runc":{"path":"runc"},"runsc":{"path":"/usr/local/bin/runsc"}}`, drv: newFakeDriver()}
}

var publishRE = regexp.MustCompile(`^127\.0\.0\.1:(\d+):2222$`)

func (f *fakeDocker) run(_ context.Context, stdin io.Reader, args ...string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	last := args[len(args)-1]
	gone := errors.New("exit status 1")
	if f.daemonDown && args[0] != "info" {
		return "", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?", gone
	}
	switch args[0] {
	case "info":
		return f.runtimes, "", nil
	case "commit":
		if f.commitErr != nil {
			return "", "Error: " + f.commitErr.Error(), f.commitErr
		}
		if _, ok := f.containers[args[1]]; !ok {
			return "", "Error response from daemon: No such container: " + args[1], gone
		}
		f.images[last] = true
		return "sha256:x\n", "", nil
	case "tag":
		if !f.images[args[1]] {
			return "", "Error response from daemon: No such image: " + args[1], gone
		}
		f.images[last] = true
		return "", "", nil
	case "image":
		if args[1] == "rm" {
			if !f.images[last] {
				return "", "Error response from daemon: No such image: " + last, gone
			}
			delete(f.images, last)
			return "Untagged: " + last, "", nil
		}
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
		if i := slices.Index(args, "--network"); i >= 0 && args[i+1] == "host" {
			return f.hostHelper(args)
		}
		if slices.Contains(args, "--rm") {
			// The work-volume copy helper.
			f.copies++
			if f.copyErr != nil && (f.copyFailAt == 0 || f.copyFailAt == f.copies) {
				return "", "docker: " + f.copyErr.Error(), f.copyErr
			}
			for i, a := range args {
				if a == "-v" && strings.HasSuffix(args[i+1], ":/to") {
					f.volumes[strings.TrimSuffix(args[i+1], ":/to")] = true
				}
			}
			return "", "", nil
		}
		if f.runErr != nil {
			return "", "docker: " + f.runErr.Error(), f.runErr
		}
		var name, port, cfg string
		var noNet bool
		for i, a := range args {
			switch {
			case a == "--network" && args[i+1] == "none":
				noNet = true
			case a == "--network" && !f.networks[args[i+1]]:
				return "", "docker: Error response from daemon: network " + args[i+1] + " not found", gone
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
			// With no published port the agent listens on a private loopback port the exec stream reaches.
			if l, err = net.Listen("tcp", "127.0.0.1:"+port); err != nil {
				return "", "port is already allocated", err
			}
			if noNet {
				port = strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
			}
			f.tokens[name] = c.Token
			go f.drv.serve(0, l, c.Token)
		}
		f.containers[name] = l
		f.ports[name] = port
		f.noNet[name] = noNet
		f.attached[name] = !noNet
		return "cid\n", "", nil
	case "inspect":
		_, ok := f.containers[last]
		if !ok {
			return "false\n", "Error: No such container", gone
		}
		running := !f.stopped[last] && !f.exitOnRun
		if strings.Contains(strings.Join(args, " "), "State.Status") {
			if running {
				return "running\n", "", nil
			}
			return "exited\n", "", nil
		}
		if !running {
			return "false\n", "", nil
		}
		return "true\n", "", nil
	case "stop":
		if _, ok := f.containers[last]; !ok {
			return "", "Error response from daemon: No such container: " + last, gone
		}
		if l := f.containers[last]; l != nil {
			l.Close()
			f.containers[last] = nil
		}
		f.stopped[last] = true
		return "", "", nil
	case "start":
		if _, ok := f.containers[last]; !ok {
			return "", "Error response from daemon: No such container: " + last, gone
		}
		if !f.noNet[last] && (!f.attached[last] || !f.networks[last+"-net"]) {
			return "", "Error response from daemon: network not found", gone
		}
		l, err := net.Listen("tcp", "127.0.0.1:"+f.ports[last])
		if err != nil {
			return "", "Error response from daemon: driver failed programming external connectivity: Bind for 127.0.0.1:" + f.ports[last] + " failed: port is already allocated", err
		}
		f.containers[last] = l
		delete(f.stopped, last)
		go f.drv.serve(0, l, f.tokens[last])
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
		delete(f.stopped, last)
		delete(f.attached, last)
		return "", "", nil
	case "network":
		switch args[1] {
		case "create":
			if f.networks[last] {
				return "", "Error response from daemon: network with name " + last + " already exists", gone
			}
			f.networks[last] = true
			f.bridges[last] = ""
			for i, a := range args {
				if a == "-o" && strings.HasPrefix(args[i+1], "com.docker.network.bridge.name=") {
					f.bridges[last] = strings.TrimPrefix(args[i+1], "com.docker.network.bridge.name=")
				}
			}
			return "nid", "", nil
		case "inspect":
			if !f.networks[last] {
				return "", "Error response from daemon: network " + last + " not found", gone
			}
			if f.bridges[last] == "" {
				return "<no value>\n", "", nil
			}
			return f.bridges[last] + "\n", "", nil
		case "connect":
			ctr := last
			if !f.networks[args[len(args)-2]] {
				return "", "Error response from daemon: network " + args[len(args)-2] + " not found", gone
			}
			if f.attached[ctr] {
				return "", "Error response from daemon: endpoint with name " + ctr + " already exists in network", gone
			}
			f.attached[ctr] = true
			return "", "", nil
		case "disconnect":
			if !f.attached[last] {
				return "", "Error response from daemon: container " + last + " is not connected to network", gone
			}
			delete(f.attached, last)
			return "", "", nil
		case "rm":
		default:
			return "", "", errors.New("unexpected docker network " + args[1])
		}
		if !f.networks[last] {
			return "", "Error response from daemon: network " + last + " not found", gone
		}
		for ctr := range f.attached {
			if ctr+"-net" == last {
				return "", "Error response from daemon: network " + last + " has active endpoints", gone
			}
		}
		delete(f.networks, last)
		delete(f.bridges, last)
		return "", "", nil
	case "volume":
		delete(f.volumes, last)
		return "", "", nil
	case "logs":
		return "", "boom from the container", nil
	}
	return "", "", errors.New("unexpected docker " + args[0])
}

// stream is the fake of docker exec -i <container> python3 -c <relay>: it pipes to the
// container's agent the way the relay does, and half-closes on the stdin close.
func (f *fakeDocker) stream(_ context.Context, args ...string) (*ExecPipe, error) {
	f.mu.Lock()
	f.streams = append(f.streams, args)
	l, ok := f.containers[args[2]]
	port := f.ports[args[2]]
	f.mu.Unlock()
	if !ok || l == nil || args[0] != "exec" || args[1] != "-i" || args[3] != "python3" {
		return nil, errors.New("Error response from daemon: container is not running")
	}
	conn, err := net.Dial("tcp", "127.0.0.1:"+port)
	if err != nil {
		return nil, err
	}
	return &ExecPipe{In: halfCloser{conn.(*net.TCPConn)}, Out: conn, Close: conn.Close}, nil
}

// halfCloser turns Close into the relay's shutdown of the socket's write side.
type halfCloser struct{ c *net.TCPConn }

func (h halfCloser) Write(b []byte) (int, error) { return h.c.Write(b) }
func (h halfCloser) Close() error                { return h.c.CloseWrite() }

func (f *fakeDocker) callsWith(verb string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if c[0] == verb && !isHostHelper(c) {
			out = append(out, c)
		}
	}
	return out
}

func newGVisorEngine(t *testing.T) (*Engine, *fakeDocker) {
	t.Helper()
	e, _, _ := newTestEngine(t)
	fd := newFakeDocker()
	e.GVisor = &GVisor{GOOS: "linux", Run: fd.run, Stream: fd.stream, LookPath: func(string) (string, error) { return "/usr/bin/docker", nil }}
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
		"--network dock-local-" + d.ID + "-net",
		"-p 127.0.0.1:" + itoa(port) + ":2222",
		"--cpus 3", "--memory 1024m", "--user 1001:1001",
		"--pids-limit 1024", "--tmpfs /tmp",
		"-v dock-local-" + d.ID + "-work:/home/user/work",
		"-v " + cfg + ":/etc/dock-fast-agent.json:ro",
		"-v " + e.file(d.ID, "resolv.conf") + ":/etc/resolv.conf:ro",
		" dock-local-gvisor:",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv lacks %q: %s", want, argv)
		}
	}
	for _, bad := range []string{"--privileged", "--cap-drop", "no-new-privileges"} {
		if strings.Contains(argv, bad) {
			t.Errorf("argv must not contain %s: %s", bad, argv)
		}
	}
	tok, _ := e.token(d.ID)
	if raw, _ := os.ReadFile(cfg); !strings.Contains(string(raw), tok) || strings.Contains(argv, tok) {
		t.Fatalf("agent config %q must hold the token and the argv must not", raw)
	}

	res, err := e.Exec(ctx, "gv", "echo hi", 0)
	if err != nil || !strings.Contains(res.Stdout, "ran:echo hi") {
		t.Fatalf("exec %+v %v", res, err)
	}

	rmBefore := len(fd.callsWith("rm"))
	st, err := e.Stop(ctx, "gv", false)
	if err != nil || st.State != "archived" {
		t.Fatalf("stop %+v %v", st, err)
	}
	if !fd.volumes["dock-local-"+d.ID+"-work"] {
		t.Fatal("stop must keep the work volume")
	}
	if _, kept := fd.containers["dock-local-"+d.ID]; !kept || !fd.stopped["dock-local-"+d.ID] {
		t.Fatalf("stop must keep the stopped container (its rootfs): %v %v", fd.containers, fd.stopped)
	}
	if len(fd.networks) != 0 {
		t.Fatalf("a stopped dock must not hold a network: %v", fd.networks)
	}
	if e.readInt(d.ID, "port") != port {
		t.Fatal("the published port is fixed for the container; the port file must stay")
	}
	if len(fd.callsWith("rm")) != rmBefore {
		t.Fatalf("stop must not remove the container: %v", fd.callsWith("rm"))
	}
	if _, err := e.Exec(ctx, "gv", "x", 0); codeOf(err) != "dock_not_running" {
		t.Fatalf("exec on a stopped dock: %v", err)
	}

	rd, err := e.Resume(ctx, "gv")
	if err != nil || rd.State != "ready" || rd.Runtime != "gvisor" {
		t.Fatalf("resume %+v %v", rd, err)
	}
	if n := len(fd.callsWith("run")); n != 1 {
		t.Fatalf("resume must start the same container, not run a new one: %d runs", n)
	}
	// Order on resume: network create, connect, then start.
	var order []string
	for _, c := range fd.calls {
		switch {
		case c[0] == "network" && c[1] == "create":
			order = append(order, "create")
		case c[0] == "network" && c[1] == "connect":
			order = append(order, "connect")
		case c[0] == "start":
			order = append(order, "start")
		}
	}
	if got := strings.Join(order, ","); got != "create,create,connect,start" {
		t.Fatalf("resume order (first create is from new): %s", got)
	}
	if e.readInt(d.ID, "port") != port {
		t.Fatal("resume must reuse the published port")
	}
	if fd.builds != 1 {
		t.Fatalf("image must be built once, builds %d", fd.builds)
	}

	if err := e.Remove(ctx, "gv"); err != nil {
		t.Fatal(err)
	}
	if fd.volumes["dock-local-"+d.ID+"-work"] || len(fd.containers) != 0 || len(fd.networks) != 0 {
		t.Fatalf("rm must remove container, volume and network: %v %v %v", fd.containers, fd.volumes, fd.networks)
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
	if _, err := e.New(ctx, Spec{Runtime: RuntimeGVisor, Network: "sideways"}); codeOf(err) != "invalid_argument" {
		t.Fatalf("bad network: %v", err)
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
	if recs, _ := e.store().List(); len(recs) != 0 || len(fd.containers) != 0 || len(fd.volumes) != 0 || len(fd.networks) != 0 {
		t.Fatalf("failed dock must leave nothing: %v %v %v", recs, fd.containers, fd.volumes)
	}

	e2, fd2 := newGVisorEngine(t)
	fd2.agentDead = true
	e2.BootTimeout = 700 * time.Millisecond
	if _, err := e2.New(ctx, Spec{Runtime: RuntimeGVisor}); codeOf(err) != "boot_timeout" {
		t.Fatalf("silent agent: %v", err)
	}
	if len(fd2.containers) != 0 || len(fd2.networks) != 0 {
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

func TestGVisorAgentConfigMountIsAbsolute(t *testing.T) {
	e, fd := newGVisorEngine(t)
	// DOCK_DATA_DIR=data: docker would read a relative -v source as a volume name.
	t.Chdir(filepath.Dir(e.Dir))
	e.Dir = filepath.Base(e.Dir)
	if _, err := e.New(context.Background(), Spec{Name: "rel", Runtime: RuntimeGVisor}); err != nil {
		t.Fatal(err)
	}
	for _, a := range fd.callsWith("run")[0] {
		if src, ok := strings.CutSuffix(a, ":"+gvAgentConfigPath+":ro"); ok && !filepath.IsAbs(src) {
			t.Fatalf("agent config mount source %q is relative", src)
		}
	}
}

func TestGVisorResolvConfUsesUpstreamResolvers(t *testing.T) {
	files := map[string]string{}
	read := func(p string) ([]byte, error) {
		if s, ok := files[p]; ok {
			return []byte(s), nil
		}
		return nil, os.ErrNotExist
	}
	// Plain host: its IPv4 resolvers, IPv6 and duplicates dropped.
	files["/etc/resolv.conf"] = "search lan\nnameserver 10.0.0.1\nnameserver 10.0.0.1\nnameserver fe80::1\nnameserver 1.1.1.1\n"
	if got := string(gvResolvConf(read)); got != "nameserver 10.0.0.1\nnameserver 1.1.1.1\n" {
		t.Fatalf("plain host: %q", got)
	}
	// systemd-resolved stub: the container cannot reach 127.0.0.53, use the real upstreams.
	files["/etc/resolv.conf"] = "nameserver 127.0.0.53\noptions edns0\n"
	files["/run/systemd/resolve/resolv.conf"] = "nameserver 168.63.129.16\n"
	if got := string(gvResolvConf(read)); got != "nameserver 168.63.129.16\n" {
		t.Fatalf("resolved stub: %q", got)
	}
	// Nothing usable: Docker's own fallback.
	delete(files, "/run/systemd/resolve/resolv.conf")
	if got := string(gvResolvConf(read)); got != "nameserver 8.8.8.8\nnameserver 8.8.4.4\n" {
		t.Fatalf("fallback: %q", got)
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

func TestGVisorNetworkIsPerDockAndIdempotent(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	a, err := e.New(ctx, Spec{Name: "a", Runtime: RuntimeGVisor})
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.New(ctx, Spec{Name: "b", Runtime: RuntimeGVisor})
	if err != nil {
		t.Fatal(err)
	}
	if !fd.networks["dock-local-"+a.ID+"-net"] || !fd.networks["dock-local-"+b.ID+"-net"] || len(fd.networks) != 2 {
		t.Fatalf("each dock needs its own network: %v", fd.networks)
	}
	// A network left behind by a crash must not break resume.
	if _, err := e.Stop(ctx, "a", true); err != nil {
		t.Fatal(err)
	}
	fd.networks["dock-local-"+a.ID+"-net"] = true
	fd.bridges["dock-local-"+a.ID+"-net"] = gvBridge(a.ID)
	if _, err := e.Resume(ctx, "a"); err != nil {
		t.Fatalf("resume with an existing network: %v", err)
	}
	// rm when the network is already gone.
	delete(fd.networks, "dock-local-"+a.ID+"-net")
	if err := e.Remove(ctx, "a"); err != nil {
		t.Fatalf("rm with the network already removed: %v", err)
	}
	if err := e.Remove(ctx, "b"); err != nil || len(fd.networks) != 0 {
		t.Fatalf("rm must remove the network: %v %v", err, fd.networks)
	}
}

func TestGVisorRemoveWithoutDockerStillClearsTheRecord(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	d, err := e.New(ctx, Spec{Name: "orphan", Runtime: RuntimeGVisor})
	if err != nil {
		t.Fatal(err)
	}
	e.GVisor.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	var progress strings.Builder
	e.Progress = &progress
	before := len(fd.calls)
	if err := e.Remove(ctx, "orphan"); err != nil {
		t.Fatalf("rm without docker: %v", err)
	}
	if len(fd.calls) != before {
		t.Fatal("no docker call without docker")
	}
	for _, want := range []string{"warning", "dock-local-" + d.ID, "dock-local-" + d.ID + "-work", "dock-local-" + d.ID + "-net"} {
		if !strings.Contains(progress.String(), want) {
			t.Errorf("warning lacks %q: %q", want, progress.String())
		}
	}
	if recs, _ := e.store().List(); len(recs) != 0 {
		t.Fatalf("record must be gone: %v", recs)
	}
}

func TestGVisorRemoveWithDaemonDownKeepsTheRecord(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	if _, err := e.New(ctx, Spec{Name: "down", Runtime: RuntimeGVisor}); err != nil {
		t.Fatal(err)
	}
	fd.daemonDown = true
	if err := e.Remove(ctx, "down"); codeOf(err) != "docker_unavailable" {
		t.Fatalf("got %v", err)
	}
	if recs, _ := e.store().List(); len(recs) != 1 {
		t.Fatal("the record must stay so rm can be retried")
	}
	fd.daemonDown = false
	if err := e.Remove(ctx, "down"); err != nil {
		t.Fatalf("retry after starting Docker: %v", err)
	}
}

func TestGVisorResumeFailsClearlyWhenThePortIsTaken(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	d, err := e.New(ctx, Spec{Name: "busy", Runtime: RuntimeGVisor})
	if err != nil {
		t.Fatal(err)
	}
	port := e.readInt(d.ID, "port")
	if _, err := e.Stop(ctx, "busy", false); err != nil {
		t.Fatal(err)
	}
	rmBefore := len(fd.callsWith("rm"))
	l, err := net.Listen("tcp", "127.0.0.1:"+itoa(port))
	if err != nil {
		t.Skipf("cannot take the port: %v", err)
	}
	_, err = e.Resume(ctx, "busy")
	if codeOf(err) != "port_in_use" || !strings.Contains(err.Error(), itoa(port)) {
		t.Fatalf("got %v", err)
	}
	if _, kept := fd.containers["dock-local-"+d.ID]; !kept || len(fd.callsWith("rm")) != rmBefore {
		t.Fatal("the container must be kept, never recreated")
	}
	l.Close()
	if st, err := e.Resume(ctx, "busy"); err != nil || st.State != "ready" {
		t.Fatalf("resume once the port is free: %+v %v", st, err)
	}

	// The race where docker itself reports the port as taken after our check.
	if _, err := e.Stop(ctx, "busy", false); err != nil {
		t.Fatal(err)
	}
	fd.mu.Lock()
	fd.ports["dock-local-"+d.ID] = itoa(port)
	fd.mu.Unlock()
	blocker, err := net.Listen("tcp", "127.0.0.1:"+itoa(port))
	if err != nil {
		t.Skipf("cannot take the port: %v", err)
	}
	defer blocker.Close()
	if _, err := e.Resume(ctx, "busy"); codeOf(err) != "port_in_use" {
		t.Fatalf("got %v", err)
	}
	if len(fd.networks) != 0 {
		t.Fatalf("a failed resume must release the network: %v", fd.networks)
	}
}

func TestGVisorResumeRecreatesALostContainer(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	d, err := e.New(ctx, Spec{Name: "lost", Runtime: RuntimeGVisor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Stop(ctx, "lost", false); err != nil {
		t.Fatal(err)
	}
	delete(fd.containers, "dock-local-"+d.ID) // docker prune
	delete(fd.stopped, "dock-local-"+d.ID)
	st, err := e.Resume(ctx, "lost")
	if err != nil || st.State != "ready" || len(fd.callsWith("run")) != 2 {
		t.Fatalf("resume of a lost container: %+v %v", st, err)
	}
}

const (
	rtPlain = `{"runc":{"path":"runc"},"runsc":{"path":"/usr/local/bin/runsc"}}`
	rtSUID  = `{"runc":{"path":"runc"},"runsc":{"path":"/usr/local/bin/runsc"},"runsc-suid":{"path":"/usr/local/bin/runsc","runtimeArgs":["--allow-suid","--overlay2=none"]}}`
)

func TestGVisorRuntimeChoice(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, runtimes, want string
		note                 bool
	}{
		{"plain runsc", rtPlain, "runsc", true},
		{"runsc-suid", rtSUID, "runsc-suid", false},
		// Registered without the flag, sudo would silently fail: treated as missing.
		{"runsc-suid without --allow-suid", `{"runsc":{"path":"/usr/local/bin/runsc"},"runsc-suid":{"path":"/usr/local/bin/runsc"}}`, "runsc", true},
		// Never accepted blindly: it must be a runsc binary.
		{"runsc-suid without --overlay2=none", `{"runsc":{"path":"/usr/local/bin/runsc"},"runsc-suid":{"path":"/usr/local/bin/runsc","runtimeArgs":["--allow-suid"]}}`, "runsc", true},
		{"runsc-suid that is not runsc", `{"runsc":{"path":"/usr/local/bin/runsc"},"runsc-suid":{"path":"/usr/bin/evil","runtimeArgs":["--allow-suid","--overlay2=none"]}}`, "runsc", true},
	} {
		e, fd := newGVisorEngine(t)
		fd.runtimes = tc.runtimes
		var progress strings.Builder
		e.Progress = &progress
		d, err := e.New(ctx, Spec{Name: "rt", Runtime: RuntimeGVisor})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		argv := strings.Join(fd.callsWith("run")[0], " ")
		if !strings.Contains(argv, " --runtime="+tc.want+" ") {
			t.Errorf("%s: argv %s", tc.name, argv)
		}
		rec, _ := e.Lookup(d.ID)
		if rec.GVisorRuntime != tc.want {
			t.Errorf("%s: record runtime %q", tc.name, rec.GVisorRuntime)
		}
		noted := strings.Contains(progress.String(), "sudo will not work") && strings.Contains(progress.String(), "runsc install --runtime=runsc-suid -- --allow-suid --overlay2=none")
		if noted != tc.note {
			t.Errorf("%s: note printed=%v, progress %q", tc.name, noted, progress.String())
		}
	}
}

func TestGVisorResumeKeepsTheRecordedRuntime(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	fd.runtimes = rtSUID
	d, err := e.New(ctx, Spec{Name: "keep", Runtime: RuntimeGVisor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Stop(ctx, "keep", false); err != nil {
		t.Fatal(err)
	}
	// docker start reuses the container runtime; a started dock is never switched.
	if _, err := e.Resume(ctx, "keep"); err != nil || len(fd.callsWith("run")) != 1 {
		t.Fatalf("resume: %v", err)
	}
	// A lost container recreated while runsc-suid is gone must not silently fall back to runsc.
	if _, err := e.Stop(ctx, "keep", false); err != nil {
		t.Fatal(err)
	}
	delete(fd.containers, "dock-local-"+d.ID)
	delete(fd.stopped, "dock-local-"+d.ID)
	fd.runtimes = rtPlain
	if _, err := e.Resume(ctx, "keep"); codeOf(err) != "runtime_missing" {
		t.Fatalf("got %v", err)
	}
	fd.runtimes = rtSUID
	if _, err := e.Resume(ctx, "keep"); err != nil {
		t.Fatalf("recreate with the recorded runtime: %v", err)
	}
	if argv := strings.Join(fd.callsWith("run")[1], " "); !strings.Contains(argv, "--runtime=runsc-suid ") {
		t.Fatalf("argv %s", argv)
	}
}

func TestDoctorGVisorRowReportsSudo(t *testing.T) {
	look := func(string) (string, error) { return "docker", nil }
	run := func(out string) DockerRun {
		return func(context.Context, io.Reader, ...string) (string, string, error) { return out, "", nil }
	}
	e := doctorEngine(t, "linux", nil, &GVisor{GOOS: "linux", LookPath: look, Run: run(rtSUID)})
	c, _ := doctorRow(e, "gvisor")
	if !c.OK || !c.Optional || !strings.Contains(c.Detail, "sudo and a persistent rootfs") || c.Fix != "" {
		t.Fatalf("suid row %+v", c)
	}
	e = doctorEngine(t, "linux", nil, &GVisor{GOOS: "linux", LookPath: look, Run: run(rtPlain)})
	c, _ = doctorRow(e, "gvisor")
	if !c.OK || !c.Optional || !strings.Contains(c.Detail, "no sudo") || c.Fix != GVisorSUIDFix {
		t.Fatalf("plain row %+v", c)
	}
}

func TestRecordGVisorRuntimeBackCompat(t *testing.T) {
	var r Record
	if err := json.Unmarshal([]byte(`{"id":"dk_1","runtime":"gvisor"}`), &r); err != nil || r.gvRuntime() != "runsc" {
		t.Fatalf("a record without gvisorRuntime uses runsc: %+v %v", r, err)
	}
	raw, _ := json.Marshal(Record{ID: "a"})
	if strings.Contains(string(raw), "gvisorRuntime") {
		t.Fatalf("empty field must be omitted: %s", raw)
	}
}

// branchCalls returns the verb (plus subverb for network and image) of each docker call, in order.
func branchCalls(fd *fakeDocker) []string {
	var out []string
	for _, c := range fd.calls {
		switch {
		case isHostHelper(c):
			out = append(out, "block")
		case c[0] == "run" && slices.Contains(c, "--rm"):
			out = append(out, "copy")
		case c[0] == "network" || c[0] == "image":
			out = append(out, c[0]+"-"+c[1])
		default:
			out = append(out, c[0])
		}
	}
	return out
}

func TestGVisorBranchRunningSource(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	src, err := e.New(ctx, Spec{Name: "gv", Runtime: RuntimeGVisor, CPUs: 2, MemoryMiB: 768})
	if err != nil {
		t.Fatal(err)
	}
	fd.calls = nil
	bs, err := e.Branch(ctx, "gv", 1, "gv2", Spec{})
	if err != nil || len(bs) != 1 {
		t.Fatalf("branch %+v %v", bs, err)
	}
	b := bs[0]
	if b.ID == src.ID || b.Name != "gv2" || b.Runtime != RuntimeGVisor || b.State != "ready" {
		t.Fatalf("branch %+v", b)
	}
	rec, _ := e.Lookup("gv2")
	if rec.Parent != src.ID || rec.CPUs != 2 || rec.MemoryMiB != 768 || rec.gvRuntime() != gvRuntimePlain {
		t.Fatalf("record %+v", rec)
	}
	srcTok, _ := e.token(src.ID)
	if tok, _ := e.token(b.ID); tok == "" || tok == srcTok {
		t.Fatal("a branch needs its own token")
	}
	if e.readInt(b.ID, "port") == e.readInt(src.ID, "port") {
		t.Fatal("a branch needs its own port")
	}

	// Source stopped, rootfs committed, volume copied, source resumed, then the branch is booted.
	order := strings.Join(branchCalls(fd), ",")
	idx := func(s string) int { return strings.Index(order, s) }
	if !(idx("stop,") >= 0 && idx("stop,") < idx("commit") && idx("commit") < idx("copy") && idx("copy") < idx("start") && idx("start") < idx("run")) {
		t.Fatalf("order: %s", order)
	}
	img := "dock-local-gvisor-branch:" + b.ID
	if c := fd.callsWith("commit"); len(c) != 1 || strings.Join(c[0], " ") != "commit dock-local-"+src.ID+" "+img {
		t.Fatalf("commit %v", c)
	}
	var helper string
	for _, c := range fd.callsWith("run") {
		if slices.Contains(c, "--rm") {
			helper = strings.Join(c, " ")
		}
	}
	for _, want := range []string{"--runtime=runsc ", "--network none", "--user 0:0", "--entrypoint cp",
		"-v dock-local-" + src.ID + "-work:/from:ro", "-v dock-local-" + b.ID + "-work:/to", " dock-local-gvisor:", "-a /from/. /to/"} {
		if !strings.Contains(helper, want) {
			t.Errorf("copy helper lacks %q: %s", want, helper)
		}
	}
	runs := fd.callsWith("run")
	last := strings.Join(runs[len(runs)-1], " ")
	for _, want := range []string{"--name dock-local-" + b.ID + " ", "--network dock-local-" + b.ID + "-net", "--cpus 2", "--memory 768m", "-v dock-local-" + b.ID + "-work:/home/user/work", " " + img} {
		if !strings.Contains(last, want) {
			t.Errorf("branch run lacks %q: %s", want, last)
		}
	}
	if st, _ := e.Get(ctx, "gv"); st.State != "ready" {
		t.Fatalf("a running source must be resumed, state %s", st.State)
	}
	if !fd.images[img] {
		t.Fatal("the committed image must exist")
	}

	// rm of the branch removes its image and nothing of the source or the base image.
	if err := e.Remove(ctx, "gv2"); err != nil {
		t.Fatal(err)
	}
	base, _ := GVisorImageTag()
	if fd.images[img] || !fd.images[base] {
		t.Fatalf("rm must drop only the branch image: %v", fd.images)
	}
	if fd.volumes["dock-local-"+b.ID+"-work"] {
		t.Fatal("rm must remove the branch volume")
	}
	if !fd.volumes["dock-local-"+src.ID+"-work"] {
		t.Fatal("the source volume must stay")
	}
	if err := e.Remove(ctx, "gv"); err != nil {
		t.Fatalf("rm of a dock without a branch image must tolerate the missing image: %v", err)
	}
}

func TestGVisorBranchStoppedSourceStaysStopped(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	if _, err := e.New(ctx, Spec{Name: "gv", Runtime: RuntimeGVisor}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Stop(ctx, "gv", false); err != nil {
		t.Fatal(err)
	}
	fd.calls = nil
	bs, err := e.Branch(ctx, "gv", 2, "", Spec{CPUs: 4})
	if err != nil || len(bs) != 2 {
		t.Fatalf("branch %+v %v", bs, err)
	}
	if len(fd.callsWith("stop")) != 0 || len(fd.callsWith("start")) != 0 {
		t.Fatalf("a stopped source must be left alone: %v", fd.calls)
	}
	if st, _ := e.Get(ctx, "gv"); st.State != "archived" {
		t.Fatalf("source state %s", st.State)
	}
	// Two branches share one commit.
	if n := len(fd.callsWith("commit")); n != 1 {
		t.Fatalf("commits %d", n)
	}
	if n := len(fd.callsWith("tag")); n != 1 {
		t.Fatalf("tags %d", n)
	}
	for _, b := range bs {
		if !fd.images["dock-local-gvisor-branch:"+b.ID] {
			t.Fatalf("branch %s has no image", b.ID)
		}
		if rec, _ := e.Lookup(b.ID); rec.CPUs != 4 {
			t.Fatalf("cpus override: %+v", rec)
		}
	}
	// Removing one branch keeps the other's image.
	if err := e.Remove(ctx, bs[0].ID); err != nil {
		t.Fatal(err)
	}
	if !fd.images["dock-local-gvisor-branch:"+bs[1].ID] || fd.images["dock-local-gvisor-branch:"+bs[0].ID] {
		t.Fatalf("images %v", fd.images)
	}
	if _, err := e.Branch(ctx, "gv", 2, "x", Spec{}); codeOf(err) != "invalid_argument" {
		t.Fatalf("a name needs a single branch: %v", err)
	}
}

func TestGVisorBranchFailureLeavesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*fakeDocker)
	}{
		{"copy fails", func(fd *fakeDocker) { fd.copyErr = errors.New("cp failed") }},
		{"commit fails", func(fd *fakeDocker) { fd.commitErr = errors.New("commit failed") }},
		{"branch container does not start", func(fd *fakeDocker) { fd.runErr = errors.New("boom") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			e, fd := newGVisorEngine(t)
			src, err := e.New(ctx, Spec{Name: "gv", Runtime: RuntimeGVisor})
			if err != nil {
				t.Fatal(err)
			}
			images := len(fd.images)
			tc.set(fd)
			if _, err := e.Branch(ctx, "gv", 1, "", Spec{}); err == nil {
				t.Fatal("branch must fail")
			}
			recs, _ := e.store().List()
			if len(recs) != 1 || recs[0].ID != src.ID {
				t.Fatalf("records %+v", recs)
			}
			if len(fd.containers) != 1 || len(fd.volumes) != 1 || len(fd.images) != images || len(fd.networks) != 1 {
				t.Fatalf("leftovers: containers %v volumes %v images %v networks %v", fd.containers, fd.volumes, fd.images, fd.networks)
			}
			if st, _ := e.Get(ctx, "gv"); st.State != "ready" {
				t.Fatalf("the source must be resumed after a failed branch, state %s", st.State)
			}
		})
	}
}

func TestGVisorBranchRuntimeMissing(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	fd.runtimes = rtSUID
	if _, err := e.New(ctx, Spec{Name: "gv", Runtime: RuntimeGVisor}); err != nil {
		t.Fatal(err)
	}
	if rec, _ := e.Lookup("gv"); rec.GVisorRuntime != gvRuntimeSUID {
		t.Fatalf("runtime %q", rec.GVisorRuntime)
	}
	fd.runtimes = `{"runc":{"path":"runc"},"runsc":{"path":"/usr/local/bin/runsc"}}`
	fd.calls = nil
	if _, err := e.Branch(ctx, "gv", 1, "", Spec{}); codeOf(err) != "runtime_missing" {
		t.Fatalf("branch: %v", err)
	}
	for _, v := range []string{"stop", "commit", "run"} {
		if len(fd.callsWith(v)) != 0 {
			t.Fatalf("a refused branch must not touch the source (%s): %v", v, fd.calls)
		}
	}
	if recs, _ := e.store().List(); len(recs) != 1 {
		t.Fatalf("records %+v", recs)
	}
}

func TestGVisorBranchPrunedSourceUsesBaseImage(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	src, err := e.New(ctx, Spec{Name: "gv", Runtime: RuntimeGVisor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Stop(ctx, "gv", false); err != nil {
		t.Fatal(err)
	}
	delete(fd.containers, "dock-local-"+src.ID) // docker container prune
	delete(fd.stopped, "dock-local-"+src.ID)
	fd.calls = nil
	bs, err := e.Branch(ctx, "gv", 2, "", Spec{})
	if err != nil || len(bs) != 2 {
		t.Fatalf("branch %+v %v", bs, err)
	}
	base, _ := GVisorImageTag()
	if n := len(fd.callsWith("tag")); n != 0 {
		t.Fatalf("nothing was committed, so nothing may be tagged: %d", n)
	}
	for _, b := range bs {
		if fd.images["dock-local-gvisor-branch:"+b.ID] {
			t.Fatalf("branch %s must not have its own image", b.ID)
		}
		var run string
		for _, c := range fd.callsWith("run") {
			if slices.Contains(c, "dock-local-"+b.ID) {
				run = strings.Join(c, " ")
			}
		}
		if !strings.HasSuffix(run, " "+base) {
			t.Fatalf("branch %s must start from the base image: %s", b.ID, run)
		}
		if !fd.volumes["dock-local-"+b.ID+"-work"] {
			t.Fatalf("branch %s must get the work volume copy", b.ID)
		}
		if err := e.Remove(ctx, b.ID); err != nil {
			t.Fatalf("rm of a branch without an image: %v", err)
		}
	}
	if !fd.images[base] || !fd.volumes["dock-local-"+src.ID+"-work"] {
		t.Fatalf("the base image and the source volume must stay: %v %v", fd.images, fd.volumes)
	}
	if _, err := e.Lookup(src.ID); err != nil {
		t.Fatal(err)
	}
}

func TestGVisorBranchLaterFailureRemovesEarlierBranches(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	src, err := e.New(ctx, Spec{Name: "gv", Runtime: RuntimeGVisor})
	if err != nil {
		t.Fatal(err)
	}
	images := len(fd.images)
	fd.copyErr, fd.copyFailAt = errors.New("cp failed"), 3
	if _, err := e.Branch(ctx, "gv", 3, "", Spec{}); codeOf(err) != "branch_failed" {
		t.Fatalf("branch: %v", err)
	}
	if recs, _ := e.store().List(); len(recs) != 1 || recs[0].ID != src.ID {
		t.Fatalf("records %+v", recs)
	}
	if len(fd.images) != images || len(fd.volumes) != 1 || len(fd.containers) != 1 {
		t.Fatalf("leftovers: images %v volumes %v containers %v", fd.images, fd.volumes, fd.containers)
	}
	if st, _ := e.Get(ctx, "gv"); st.State != "ready" {
		t.Fatalf("the source must be resumed, state %s", st.State)
	}
}
