package local

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/usedock/docklet/internal/dockyard"

	"github.com/usedock/docklet/internal/guestagent"
)

// fakeDriver is an in-memory hypervisor: starting a "VM" serves a guest agent on the port.
type fakeDriver struct {
	mu             sync.Mutex
	preflightErr   error
	nextPID        int
	alive          map[int]bool
	listeners      map[int]net.Listener
	starts         []LaunchSpec
	overlays       []string
	resizes        []int
	flattens       []string
	neverAnswer    bool // VM starts but its agent never answers
	dieOnStart     bool // VM exits right after start
	bindFailures   int  // the next N starts exit because the forwarded port could not be bound
	ignorePoweroff bool
	// importExit makes the import script fail with this exit code.
	importExit int
	// driveData and driveMarker record, per start with an import drive, the drive's bytes and whether
	// the importing marker existed; scripts records every exec script.
	driveData   [][]byte
	driveMarker []bool
	scripts     []string
}

func newFakeDriver() *fakeDriver {
	return &fakeDriver{nextPID: 1000, alive: map[int]bool{}, listeners: map[int]net.Listener{}}
}

func (f *fakeDriver) Preflight(context.Context) error { return f.preflightErr }

func (f *fakeDriver) CreateOverlay(_ context.Context, base, disk string) error {
	f.mu.Lock()
	f.overlays = append(f.overlays, base)
	f.mu.Unlock()
	return os.WriteFile(disk, []byte("overlay-of:"+base), 0o600)
}

func (f *fakeDriver) ResizeDisk(_ context.Context, _ string, gib int) error {
	f.mu.Lock()
	f.resizes = append(f.resizes, gib)
	f.mu.Unlock()
	return nil
}

func (f *fakeDriver) ConvertRaw(_ context.Context, raw, qcow2 string) error {
	b, err := os.ReadFile(raw)
	if err != nil {
		return err
	}
	return os.WriteFile(qcow2, append([]byte("qcow2:"), b...), 0o600)
}

func (f *fakeDriver) FlattenDisk(_ context.Context, qcow2, raw string) error {
	b, err := os.ReadFile(qcow2)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.flattens = append(f.flattens, qcow2)
	f.mu.Unlock()
	return os.WriteFile(raw, b, 0o600)
}

func (f *fakeDriver) Start(_ context.Context, spec LaunchSpec) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextPID++
	pid := f.nextPID
	f.starts = append(f.starts, spec)
	if spec.ImportDrive != "" {
		b, _ := os.ReadFile(spec.ImportDrive)
		_, merr := os.Stat(filepath.Join(filepath.Dir(spec.Disk), gvImportingMarker))
		f.driveData, f.driveMarker = append(f.driveData, b), append(f.driveMarker, merr == nil)
	}
	if f.bindFailures > 0 {
		f.bindFailures--
		_ = os.WriteFile(spec.StderrLog, []byte("qemu-system-x86_64: Could not set up host forwarding rule 'tcp:127.0.0.1:1-:2222'"), 0o600)
		return pid, nil
	}
	if f.dieOnStart {
		return pid, nil
	}
	f.alive[pid] = true
	if f.neverAnswer {
		return pid, nil
	}
	l, err := net.Listen("tcp", "127.0.0.1:"+itoa(spec.HostPort))
	if err != nil {
		return 0, err
	}
	f.listeners[pid] = l
	go f.serve(pid, l, spec.Token)
	return pid, nil
}

func (f *fakeDriver) serve(pid int, l net.Listener, token string) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			line, err := bufio.NewReader(conn).ReadBytes('\n')
			if err != nil {
				return
			}
			var req guestagent.Request
			_ = json.Unmarshal(line, &req)
			reply := func(r guestagent.Response) {
				b, _ := json.Marshal(r)
				_, _ = conn.Write(append(b, '\n'))
			}
			if req.Token != token {
				reply(guestagent.Response{Error: "invalid_token"})
				return
			}
			switch req.Op {
			case "ping":
				reply(guestagent.Response{OK: true})
			case "exec":
				if strings.Contains(req.Script, "poweroff") {
					reply(guestagent.Response{OK: true})
					if !f.ignorePoweroff {
						f.Kill(pid)
					}
					return
				}
				f.mu.Lock()
				f.scripts = append(f.scripts, req.Script)
				f.mu.Unlock()
				if strings.Contains(req.Script, "DOCK_IMPORT") && f.importExit != 0 {
					reply(guestagent.Response{OK: true, ExitCode: f.importExit, Stderr: "tar: boom"})
					return
				}
				reply(guestagent.Response{OK: true, ExitCode: 0, Stdout: "ran:" + req.Script})
			default:
				reply(guestagent.Response{Error: "unsupported_operation"})
			}
		}()
	}
}

func (f *fakeDriver) Alive(pid int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive[pid]
}

func (f *fakeDriver) Kill(pid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alive[pid] = false
	if l := f.listeners[pid]; l != nil {
		l.Close()
		delete(f.listeners, pid)
	}
	return nil
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

type fakeCatalog struct {
	ix  *dockyard.Index
	err error
}

func (c fakeCatalog) Index(context.Context) (*dockyard.Index, error) { return c.ix, c.err }

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func zstdBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	w.Close()
	return buf.Bytes()
}

// imageServer serves a tiny fake image and returns its index entry.
func imageServer(t *testing.T, arch string) (dockyard.Image, *httptest.Server, map[string]int) {
	t.Helper()
	files := map[string][]byte{
		dockyard.FileKernel: []byte("kernel-bytes"),
		dockyard.FileInitrd: []byte("initrd-bytes"),
		dockyard.FileRootfs: zstdBytes(t, []byte("rootfs-raw-bytes")),
	}
	hits := map[string]int{}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		mu.Lock()
		hits[name]++
		mu.Unlock()
		b, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	im := dockyard.Image{Name: "test-img", Arch: arch, Description: "a test image", Files: map[string]dockyard.File{}}
	for n, b := range files {
		im.Files[n] = dockyard.File{URL: srv.URL + "/" + n, Size: int64(len(b)), SHA256: sha(b)}
	}
	return im, srv, hits
}

// newTestEngine returns an engine with a fake driver and one cached-able fake image.
func newTestEngine(t *testing.T) (*Engine, *fakeDriver, map[string]int) {
	t.Helper()
	im, _, hits := imageServer(t, Arch())
	im.Name = DefaultImage
	drv := newFakeDriver()
	dir := t.TempDir()
	e := &Engine{
		Dir: dir, Driver: drv, Arch: Arch(),
		Catalog:     fakeCatalog{ix: &dockyard.Index{Version: "t", Images: []dockyard.Image{im}}},
		Cache:       &Cache{Dir: dir + "/images", Driver: drv},
		BootTimeout: 5e9, StopTimeout: 5e9,
	}
	return e, drv, hits
}
