package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/usedock/docklet/internal/dockyard"
	"github.com/usedock/docklet/internal/guestagent"
	"github.com/usedock/docklet/internal/local"
)

// fakeVM is an in-memory local.Driver: a started "VM" serves a guest agent on its port.
type fakeVM struct {
	mu        sync.Mutex
	pid       int
	alive     map[int]bool
	listeners map[int]net.Listener
	preflight error
	starts    []local.LaunchSpec
}

func newFakeVM() *fakeVM {
	return &fakeVM{pid: 500, alive: map[int]bool{}, listeners: map[int]net.Listener{}}
}

func (f *fakeVM) Preflight(context.Context) error { return f.preflight }
func (f *fakeVM) CreateOverlay(_ context.Context, base, disk string) error {
	return os.WriteFile(disk, []byte("overlay"), 0o600)
}
func (f *fakeVM) ResizeDisk(context.Context, string, int) error { return nil }
func (f *fakeVM) ConvertRaw(_ context.Context, raw, q string) error {
	b, _ := os.ReadFile(raw)
	return os.WriteFile(q, b, 0o600)
}
func (f *fakeVM) Alive(pid int) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.alive[pid] }
func (f *fakeVM) Kill(pid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alive[pid] = false
	if l := f.listeners[pid]; l != nil {
		l.Close()
		delete(f.listeners, pid)
	}
	return nil
}

func (f *fakeVM) Start(_ context.Context, s local.LaunchSpec) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pid++
	pid := f.pid
	f.starts = append(f.starts, s)
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(s.HostPort))
	if err != nil {
		return 0, err
	}
	f.alive[pid] = true
	f.listeners[pid] = l
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				line, _ := bufio.NewReader(conn).ReadBytes('\n')
				var req guestagent.Request
				_ = json.Unmarshal(line, &req)
				resp := guestagent.Response{OK: req.Token == s.Token}
				if !resp.OK {
					resp.Error = "invalid_token"
				} else if req.Op == "exec" {
					if strings.Contains(req.Script, "poweroff") {
						f.Kill(pid)
					}
					resp.Stdout = "out:" + req.Script
					resp.ExitCode = 0
					if strings.Contains(req.Script, "exit7") {
						resp.ExitCode = 7
					}
				}
				b, _ := json.Marshal(resp)
				_, _ = conn.Write(append(b, '\n'))
				if req.Op == "tcp" && resp.OK {
					_, _ = io.Copy(conn, conn) // echo server on the dock port
				}
			}()
		}
	}()
	return pid, nil
}

type staticCatalog struct{ ix *dockyard.Index }

func (c staticCatalog) Index(context.Context) (*dockyard.Index, error) { return c.ix, nil }

// localEnv is a testEnv with no API key and a fake local engine.
func newLocalEnv(t *testing.T) (*testEnv, *fakeVM) {
	t.Helper()
	te := newEnv(t)
	te.env = map[string]string{}
	files := map[string][]byte{dockyard.FileKernel: []byte("k"), dockyard.FileInitrd: []byte("i"), dockyard.FileRootfs: zstdOf(t, "root")}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	im := dockyard.Image{Name: local.DefaultImage, Arch: local.Arch(), Description: "Ubuntu test", Files: map[string]dockyard.File{}}
	for n, b := range files {
		s := sha256.Sum256(b)
		im.Files[n] = dockyard.File{URL: srv.URL + "/" + n, Size: int64(len(b)), SHA256: hex.EncodeToString(s[:])}
	}
	other := im
	other.Name, other.Arch = "other-arch", "s390x"
	drv := newFakeVM()
	dir := t.TempDir()
	te.app.Engine = &local.Engine{
		Dir: dir, Driver: drv, Arch: local.Arch(),
		Catalog:     staticCatalog{&dockyard.Index{Version: "t", Images: []dockyard.Image{im, other}}},
		Cache:       &local.Cache{Dir: dir + "/images", Driver: drv},
		BootTimeout: 5e9, StopTimeout: 5e9,
	}
	return te, drv
}

func TestNoConfigDefaultsToLocalContext(t *testing.T) {
	te, _ := newLocalEnv(t)
	mustExit(t, te, 0, "context", "ls", "--json")
	rows := jsonOut(t, te).([]any)
	if rows[0].(map[string]any)["name"] != "local" || rows[0].(map[string]any)["current"] != true || rows[1].(map[string]any)["current"] != false {
		t.Fatalf("a fresh install must use local: %v", rows)
	}
	mustExit(t, te, 0, "ls")
	if len(te.api.reqs) != 0 {
		t.Fatal("the local context must not call any API")
	}
}

func TestCloudWinsWhenAKeyIsConfigured(t *testing.T) {
	te, _ := newLocalEnv(t)
	te.env["DOCK_API_URL"] = te.api.srv.URL
	te.env["DOCK_API_KEY"] = "dock_k"
	mustExit(t, te, 0, "ls")
	if len(te.api.reqs) == 0 {
		t.Fatal("a configured API key must route to the control plane")
	}
	// A stored key does too.
	te2, _ := newLocalEnv(t)
	te2.app.Stdin = strings.NewReader("dock_stored\n")
	mustExit(t, te2, 0, "login")
	cfg, _ := LoadConfig(te2.cfgPath)
	if cfg.Current != CloudContext || cfg.Contexts[CloudContext].APIKey != "dock_stored" {
		t.Fatalf("login from local must switch to cloud: %+v", cfg)
	}
	// An explicit context beats key detection, and local is selectable by name.
	mustExit(t, te2, 0, "context", "use", "local")
	mustExit(t, te2, 0, "context", "ls", "--json")
	if rows := jsonOut(t, te2).([]any); rows[0].(map[string]any)["current"] != true {
		t.Fatalf("context use local: %v", rows)
	}
	mustExit(t, te2, 1, "context", "add", "local", "--url", "http://x.test")
	mustExit(t, te2, 1, "context", "rm", "local")
	te2.app.Stdin = strings.NewReader("dock_k\n")
	mustExit(t, te2, 1, "login", "--context", "local")
}

func TestLocalLifecycleSharesJSONShape(t *testing.T) {
	te, drv := newLocalEnv(t)
	mustExit(t, te, 0, "new", "web", "--cpus", "2", "--memory", "1gb", "--disk", "5gb", "--json")
	d := jsonOut(t, te).(map[string]any)
	for _, k := range []string{"id", "name", "state", "image", "runtime", "environment", "cpus", "memoryMiB", "diskGiB"} {
		if _, ok := d[k]; !ok {
			t.Errorf("new --json lacks %q (same keys as the API): %v", k, d)
		}
	}
	if d["state"] != "ready" || d["name"] != "web" || d["runtime"] != "qemu" || d["memoryMiB"] != 1024.0 || !strings.HasPrefix(d["id"].(string), "dk_") {
		t.Fatalf("dock %v", d)
	}
	if len(drv.starts) != 1 || drv.starts[0].CPUs != 2 {
		t.Fatalf("launch %+v", drv.starts)
	}

	mustExit(t, te, 0, "ls")
	if !strings.Contains(te.out.String(), "web") || !strings.Contains(te.out.String(), "ready") {
		t.Fatalf("ls %q", te.out)
	}

	mustExit(t, te, 0, "exec", "web", "--", "echo", "hi there")
	if te.out.String() != "out:echo 'hi there'" {
		t.Fatalf("exec out %q", te.out)
	}
	mustExit(t, te, 0, "exec", "web", "--cwd", "/tmp", "--json", "--", "ls")
	if res := jsonOut(t, te).(map[string]any); res["stdout"] != "out:cd '/tmp' && ls" || res["exitCode"] != 0.0 {
		t.Fatalf("exec json %v", res)
	}
	mustExit(t, te, 7, "exec", "web", "--", "exit7")

	mustExit(t, te, 0, "stop", "web")
	if !strings.Contains(te.out.String(), "state=archived") {
		t.Fatalf("stop %q", te.out)
	}
	mustExit(t, te, 0, "ls")
	if strings.Contains(te.out.String(), "web") {
		t.Fatalf("a stopped dock is hidden without --all: %q", te.out)
	}
	mustExit(t, te, 0, "ls", "--all")
	if !strings.Contains(te.out.String(), "archived") {
		t.Fatalf("ls --all %q", te.out)
	}
	mustExit(t, te, 1, "exec", "web", "--", "true")
	if !strings.Contains(te.errb.String(), "dock_not_running") {
		t.Fatalf("err %q", te.errb)
	}
	mustExit(t, te, 0, "resume", "web")
	mustExit(t, te, 0, "branch", "web", "--count", "2", "--json")
	if arr := jsonOut(t, te).([]any); len(arr) != 2 {
		t.Fatalf("branch --count 2 prints a list: %v", arr)
	}
	mustExit(t, te, 0, "branch", "web", "--name", "web-copy", "--json")
	if one := jsonOut(t, te).(map[string]any); one["name"] != "web-copy" {
		t.Fatalf("branch --count 1 prints one dock: %v", one)
	}
	mustExit(t, te, 0, "rm", "web-copy")
	mustExit(t, te, 1, "rm", "web-copy")
	if !strings.Contains(te.errb.String(), "dock_not_found") {
		t.Fatalf("err %q", te.errb)
	}
}

func TestLocalNewErrorsAndFlags(t *testing.T) {
	te, drv := newLocalEnv(t)
	mustExit(t, te, 0, "new", "a")
	mustExit(t, te, 1, "new", "a")
	if te.errb.String() != "error: dock_name_taken: Dock name already in use\n" {
		t.Fatalf("err %q", te.errb)
	}
	mustExit(t, te, 2, "new", "--workspace", "w")
	mustExit(t, te, 0, "new", "offline", "--network", "off")
	if drv.starts[len(drv.starts)-1].Network != "off" {
		t.Fatalf("launch %+v", drv.starts)
	}
	drv.preflight = &local.Error{Code: "no_accelerator", Message: "KVM is not usable"}
	mustExit(t, te, 1, "new", "b")
	if !strings.Contains(te.errb.String(), "no_accelerator: KVM") {
		t.Fatalf("err %q", te.errb)
	}
	mustExit(t, te, 2, "port", "a", "80", "--rm")
	mustExit(t, te, 2, "ssh", "a", "--print")
}

func TestNetworkFlagIsLocalOnly(t *testing.T) {
	te := newEnv(t)
	mustExit(t, te, 2, "new", "x", "--network", "off")
	if len(te.api.reqs) != 0 {
		t.Fatal("no API call for a usage error")
	}
}

func TestImagesListAndPull(t *testing.T) {
	te, _ := newLocalEnv(t)
	mustExit(t, te, 0, "images")
	out := te.out.String()
	if !strings.Contains(out, "NAME") || !strings.Contains(out, local.DefaultImage) || !strings.Contains(out, "Ubuntu test") || !strings.Contains(out, "no") {
		t.Fatalf("images %q", out)
	}
	if strings.Contains(out, "other-arch") {
		t.Fatalf("images for other architectures are hidden: %q", out)
	}
	mustExit(t, te, 0, "images", "pull", local.DefaultImage)
	if !strings.Contains(te.out.String(), "pulled "+local.DefaultImage) {
		t.Fatalf("pull %q", te.out)
	}
	mustExit(t, te, 0, "images", "--json")
	rows := jsonOut(t, te).([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["cached"] != true || rows[0].(map[string]any)["name"] != local.DefaultImage {
		t.Fatalf("images json %v", rows)
	}
	mustExit(t, te, 0, "images")
	if !strings.Contains(te.out.String(), "yes") {
		t.Fatalf("a pulled image shows as cached: %q", te.out)
	}
	mustExit(t, te, 1, "images", "pull", "nope")
	if !strings.Contains(te.errb.String(), "image_not_found") {
		t.Fatalf("err %q", te.errb)
	}
	mustExit(t, te, 2, "images", "pull")
}

func TestDoctor(t *testing.T) {
	te, drv := newLocalEnv(t)
	mustExit(t, te, 0, "doctor")
	if !strings.Contains(te.out.String(), "[ok") || !strings.Contains(te.out.String(), "dockyard") {
		t.Fatalf("doctor %q", te.out)
	}
	mustExit(t, te, 0, "doctor", "--json")
	if checks := jsonOut(t, te).([]any); len(checks) == 0 {
		t.Fatal("no checks")
	}
	drv.preflight = &local.Error{Code: "no_accelerator", Message: "nope"}
	mustExit(t, te, 1, "doctor")
	if !strings.Contains(te.out.String(), "[missing] driver") {
		t.Fatalf("doctor %q", te.out)
	}
}

func TestHelpDescribesLocalContext(t *testing.T) {
	te := newEnv(t)
	mustExit(t, te, 0, "help")
	for _, want := range []string{"local", "dock images", "doctor", "DOCK_QEMU"} {
		if !strings.Contains(te.out.String(), strings.TrimPrefix(want, "dock ")) {
			t.Errorf("usage lacks %q", want)
		}
	}
}

func zstdOf(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(s))
	w.Close()
	return buf.Bytes()
}

func TestLocalPortForwardRelaysThroughTheAgent(t *testing.T) {
	te, _ := newLocalEnv(t)
	mustExit(t, te, 0, "new", "web")
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lp := free.Addr().(*net.TCPAddr).Port
	free.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- te.app.Run(ctx, []string{"port", "web", "8080", "--local-port", strconv.Itoa(lp)}) }()
	var conn net.Conn
	for range 100 {
		if conn, err = net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(lp)); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if conn == nil {
		t.Fatalf("forward never listened: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = conn.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo through the tunnel: %q %v", buf, err)
	}
	conn.Close()
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d: %s", code, te.errb)
	}
	if !strings.Contains(te.out.String(), "http://127.0.0.1:"+strconv.Itoa(lp)) {
		t.Fatalf("out %q", te.out)
	}
}

func TestLocalPortForwardNeedsARunningDock(t *testing.T) {
	te, _ := newLocalEnv(t)
	mustExit(t, te, 0, "new", "web")
	mustExit(t, te, 0, "stop", "web")
	mustExit(t, te, 1, "port", "web", "8080")
	if !strings.Contains(te.errb.String(), "dock_not_running") {
		t.Fatalf("err %q", te.errb)
	}
}

type noReleaseCatalog struct{}

func (noReleaseCatalog) Index(context.Context) (*dockyard.Index, error) {
	return nil, &local.Error{Code: "no_dockyard_release", Message: "no Dockyard release pinned yet"}
}

func TestImagesWithoutAPinnedReleaseIsClean(t *testing.T) {
	te, _ := newLocalEnv(t)
	te.app.Engine.Catalog = noReleaseCatalog{}
	mustExit(t, te, 0, "images")
	if !strings.Contains(te.out.String(), "no Dockyard release pinned yet") {
		t.Fatalf("out %q", te.out)
	}
	mustExit(t, te, 0, "images", "--json")
	if rows := jsonOut(t, te).([]any); len(rows) != 0 {
		t.Fatalf("rows %v", rows)
	}
	mustExit(t, te, 1, "images", "pull", "ubuntu-24.04")
	if !strings.Contains(te.errb.String(), "no_dockyard_release: no Dockyard release pinned yet") {
		t.Fatalf("err %q", te.errb)
	}
	mustExit(t, te, 1, "doctor")
	if !strings.Contains(te.out.String(), "no Dockyard release pinned yet") {
		t.Fatalf("doctor %q", te.out)
	}
}
