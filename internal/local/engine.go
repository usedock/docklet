// Package local is the dock CLI's built-in engine: docks run as QEMU virtual machines on
// the user's own machine, with no account and no control plane.
package local

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/usedock/docklet/internal/dockyard"
	"github.com/usedock/docklet/internal/guestagent"
)

// Runtime is the runtime name local docks report in API-shaped output.
const Runtime = "qemu"

// Defaults for a dock that sets no size.
const (
	DefaultCPUs      = 2
	DefaultMemoryMiB = 2048
	DefaultDiskGiB   = 10
)

// Dock is the API-shaped view of a local dock (same JSON keys as the cloud API).
type Dock struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	State       string `json:"state"`
	Image       string `json:"image"`
	Runtime     string `json:"runtime"`
	Environment string `json:"environment"`
	CPUs        int    `json:"cpus"`
	MemoryMiB   int    `json:"memoryMiB"`
	DiskGiB     int    `json:"diskGiB"`
}

// Spec is a request to create a dock; zero fields take defaults.
type Spec struct {
	Name, Image, Network     string
	CPUs, MemoryMiB, DiskGiB int
}

// ExecResult is what a command run in a dock produced.
type ExecResult struct {
	ExitCode   int    `json:"exitCode"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMs int64  `json:"durationMs"`
	Truncated  bool   `json:"truncated"`
}

// Engine manages local docks under Dir.
type Engine struct {
	Dir     string
	Driver  Driver
	Catalog Catalog
	Cache   *Cache
	Arch    string
	// BootTimeout bounds waiting for the guest agent after boot; zero means two minutes.
	BootTimeout time.Duration
	// StopTimeout bounds a graceful shutdown before the VM is killed; zero means 30 seconds.
	StopTimeout time.Duration
	// Progress receives human progress lines; nil is silent.
	Progress io.Writer
	Now      func() time.Time
}

// NewEngine returns the engine for this machine, rooted at the user data directory.
func NewEngine(getenv func(string) string, progress io.Writer) (*Engine, error) {
	dir, err := DataDir(getenv)
	if err != nil {
		return nil, err
	}
	drv := NewQEMU(getenv)
	return &Engine{
		Dir:      dir,
		Driver:   drv,
		Catalog:  NewCatalog(getenv, progress),
		Cache:    &Cache{Dir: filepath.Join(dir, "images"), Driver: drv, Progress: progress},
		Arch:     Arch(),
		Progress: progress,
	}, nil
}

func (e *Engine) store() *Store { return &Store{Dir: e.Dir} }

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) logf(format string, a ...any) {
	if e.Progress != nil {
		fmt.Fprintf(e.Progress, format+"\n", a...)
	}
}

func (e *Engine) bootTimeout() time.Duration {
	if e.BootTimeout > 0 {
		return e.BootTimeout
	}
	return 2 * time.Minute
}

func (e *Engine) stopTimeout() time.Duration {
	if e.StopTimeout > 0 {
		return e.StopTimeout
	}
	return 30 * time.Second
}

// runtime files of one dock.
func (e *Engine) file(id, name string) string { return filepath.Join(e.store().DockDir(id), name) }

func (e *Engine) readInt(id, name string) int {
	raw, err := os.ReadFile(e.file(id, name))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return n
}

func (e *Engine) token(id string) (string, error) {
	raw, err := os.ReadFile(e.file(id, "token"))
	if err != nil {
		return "", errf("dock_broken", "dock %s has no token file: %v", id, err)
	}
	return strings.TrimSpace(string(raw)), nil
}

func (e *Engine) agentAddr(id string) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(e.readInt(id, "port")))
}

func (e *Engine) alive(id string) bool {
	pid := e.readInt(id, "pid")
	return pid > 0 && e.Driver.Alive(pid)
}

func (e *Engine) ping(ctx context.Context, id string) bool {
	tok, err := e.token(id)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	_, err = guestagent.Call(ctx, e.agentAddr(id), tok, guestagent.Request{Op: "ping"})
	return err == nil
}

// State of a dock: ready (agent answers), starting (vm up, agent not yet), archived (stopped).
func (e *Engine) state(ctx context.Context, id string) string {
	if !e.alive(id) {
		return "archived"
	}
	if e.ping(ctx, id) {
		return "ready"
	}
	return "starting"
}

func (e *Engine) view(ctx context.Context, r Record) Dock {
	return Dock{ID: r.ID, Name: r.Name, State: e.state(ctx, r.ID), Image: r.Image, Runtime: Runtime,
		Environment: "local", CPUs: r.CPUs, MemoryMiB: r.MemoryMiB, DiskGiB: r.DiskGiB}
}

// List returns every local dock.
func (e *Engine) List(ctx context.Context) ([]Dock, error) {
	recs, err := e.store().List()
	if err != nil {
		return nil, err
	}
	out := make([]Dock, 0, len(recs))
	for _, r := range recs {
		out = append(out, e.view(ctx, r))
	}
	return out, nil
}

// Lookup resolves a dock id or unique name.
func (e *Engine) Lookup(ref string) (Record, error) {
	recs, err := e.store().List()
	if err != nil {
		return Record{}, err
	}
	for _, r := range recs {
		if r.ID == ref {
			return r, nil
		}
	}
	for _, r := range recs {
		if r.Name == ref {
			return r, nil
		}
	}
	return Record{}, errf("dock_not_found", "no dock named %q", ref)
}

// Get returns one dock by id or name.
func (e *Engine) Get(ctx context.Context, ref string) (Dock, error) {
	r, err := e.Lookup(ref)
	if err != nil {
		return Dock{}, err
	}
	return e.view(ctx, r), nil
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

func (s Spec) withDefaults() (Spec, error) {
	if s.CPUs == 0 {
		s.CPUs = DefaultCPUs
	}
	if s.MemoryMiB == 0 {
		s.MemoryMiB = DefaultMemoryMiB
	}
	if s.DiskGiB == 0 {
		s.DiskGiB = DefaultDiskGiB
	}
	if s.Network == "" {
		s.Network = "on"
	}
	switch {
	case s.Name != "" && !nameRE.MatchString(s.Name):
		return s, errf("invalid_argument", "invalid dock name %q (letters, digits, '.', '_', '-')", s.Name)
	case s.CPUs < 1 || s.CPUs > 64:
		return s, errf("invalid_argument", "cpus must be 1-64")
	case s.MemoryMiB < 256:
		return s, errf("invalid_argument", "memory must be at least 256 MiB")
	case s.DiskGiB < 1:
		return s, errf("invalid_argument", "disk must be at least 1 GiB")
	case s.Network != "on" && s.Network != "off":
		return s, errf("invalid_argument", "network must be on or off")
	}
	return s, nil
}

// ResolveImage finds the catalog entry for name (or the default) on this architecture.
func (e *Engine) ResolveImage(ctx context.Context, name string) (dockyard.Image, error) {
	if name == "" {
		name = DefaultImage
	}
	ix, err := e.Catalog.Index(ctx)
	if err != nil {
		return dockyard.Image{}, err
	}
	return findImage(ix, name, e.Arch)
}

// New creates a dock and boots it.
func (e *Engine) New(ctx context.Context, spec Spec) (Dock, error) {
	spec, err := spec.withDefaults()
	if err != nil {
		return Dock{}, err
	}
	if spec.Name != "" {
		if _, err := e.Lookup(spec.Name); err == nil {
			return Dock{}, errf("dock_name_taken", "Dock name already in use")
		}
	}
	// Fail on a missing hypervisor before downloading anything.
	if err := e.Driver.Preflight(ctx); err != nil {
		return Dock{}, err
	}
	im, err := e.ResolveImage(ctx, spec.Image)
	if err != nil {
		return Dock{}, err
	}
	cached, err := e.Cache.Ensure(ctx, im)
	if err != nil {
		return Dock{}, err
	}
	rec := Record{ID: NewID(), Name: spec.Name, Image: im.Name, ImageDir: cached.Dir, CPUs: spec.CPUs, MemoryMiB: spec.MemoryMiB,
		DiskGiB: spec.DiskGiB, Network: spec.Network, CreatedAt: e.now()}
	dir := e.store().DockDir(rec.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Dock{}, errf("store_error", "%v", err)
	}
	fail := func(err error) (Dock, error) {
		_ = e.removeFiles(rec.ID)
		return Dock{}, err
	}
	disk := e.file(rec.ID, "disk.qcow2")
	if err := e.Driver.CreateOverlay(ctx, cached.Base, disk); err != nil {
		return fail(err)
	}
	if err := e.Driver.ResizeDisk(ctx, disk, rec.DiskGiB); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(e.file(rec.ID, "token"), []byte(NewToken()), 0o600); err != nil {
		return fail(errf("store_error", "%v", err))
	}
	if err := e.store().Update(func(recs *[]Record) error {
		if rec.Name != "" {
			for _, r := range *recs {
				if r.Name == rec.Name {
					return errf("dock_name_taken", "Dock name already in use")
				}
			}
		}
		*recs = append(*recs, rec)
		return nil
	}); err != nil {
		return fail(err)
	}
	e.logf("booting %s", rec.ID)
	if err := e.boot(ctx, rec); err != nil {
		_ = e.Remove(ctx, rec.ID)
		return Dock{}, err
	}
	return e.view(ctx, rec), nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// boot starts the VM for rec and waits for its agent.
func (e *Engine) boot(ctx context.Context, rec Record) error {
	kernel, initrd := filepath.Join(rec.ImageDir, dockyard.FileKernel), filepath.Join(rec.ImageDir, dockyard.FileInitrd)
	if _, err := os.Stat(kernel); err != nil {
		return errf("image_missing", "the image files of %s are gone (%s); run: dock images pull %s", rec.ID, rec.ImageDir, rec.Image)
	}
	port, err := freePort()
	if err != nil {
		return errf("start_failed", "no free local port: %v", err)
	}
	tok, err := e.token(rec.ID)
	if err != nil {
		return err
	}
	if err := os.WriteFile(e.file(rec.ID, "port"), []byte(strconv.Itoa(port)), 0o600); err != nil {
		return errf("store_error", "%v", err)
	}
	pid, err := e.Driver.Start(ctx, LaunchSpec{
		Kernel: kernel, Initrd: initrd, Disk: e.file(rec.ID, "disk.qcow2"),
		SerialLog: e.file(rec.ID, "serial.log"), StderrLog: e.file(rec.ID, "qemu.log"),
		HostPort: port, Token: tok, CPUs: rec.CPUs, MemoryMiB: rec.MemoryMiB, Network: rec.Network,
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(e.file(rec.ID, "pid"), []byte(strconv.Itoa(pid)), 0o600); err != nil {
		_ = e.Driver.Kill(pid)
		return errf("store_error", "%v", err)
	}
	return e.waitReady(ctx, rec.ID, pid)
}

func (e *Engine) waitReady(ctx context.Context, id string, pid int) error {
	deadline := time.NewTimer(e.bootTimeout())
	defer deadline.Stop()
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		if e.ping(ctx, id) {
			return nil
		}
		if !e.Driver.Alive(pid) {
			return errf("boot_failed", "the VM exited during boot.%s", e.logTail(id))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			_ = e.Driver.Kill(pid)
			return errf("boot_timeout", "the guest agent did not answer within %s.%s", e.bootTimeout(), e.logTail(id))
		case <-tick.C:
		}
	}
}

// logTail returns the last lines of the qemu and serial logs for error messages.
func (e *Engine) logTail(id string) string {
	var sb strings.Builder
	for _, name := range []string{"qemu.log", "serial.log"} {
		raw, err := os.ReadFile(e.file(id, name))
		if err != nil || len(raw) == 0 {
			continue
		}
		lines := strings.Split(strings.TrimRight(string(raw), "\r\n"), "\n")
		if len(lines) > 12 {
			lines = lines[len(lines)-12:]
		}
		fmt.Fprintf(&sb, "\n--- %s ---\n%s", name, strings.Join(lines, "\n"))
	}
	return sb.String()
}

// Exec runs script in the dock through the guest agent and returns its result.
func (e *Engine) Exec(ctx context.Context, ref, script string, timeout time.Duration) (ExecResult, error) {
	rec, tok, err := e.running(ref)
	if err != nil {
		return ExecResult{}, err
	}
	seconds := int(timeout / time.Second)
	if seconds < 1 {
		seconds = 300
	}
	start := e.now()
	res, err := guestagent.Call(ctx, e.agentAddr(rec.ID), tok, guestagent.Request{
		Op: "exec", Script: script, TimeoutSeconds: seconds, OutputLimit: guestagent.MaxExecOutput,
	})
	if err != nil {
		return ExecResult{}, errf("exec_failed", "%v", err)
	}
	return ExecResult{ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr, Truncated: res.Truncated,
		DurationMs: e.now().Sub(start).Milliseconds()}, nil
}

func (e *Engine) running(ref string) (Record, string, error) {
	rec, err := e.Lookup(ref)
	if err != nil {
		return rec, "", err
	}
	if !e.alive(rec.ID) {
		return rec, "", errf("dock_not_running", "dock %s is stopped; run: dock resume %s", ref, ref)
	}
	tok, err := e.token(rec.ID)
	return rec, tok, err
}

// OpenTerminal opens a login shell on a pty in the dock.
func (e *Engine) OpenTerminal(ctx context.Context, ref string, cols, rows int) (*guestagent.Terminal, error) {
	rec, tok, err := e.running(ref)
	if err != nil {
		return nil, err
	}
	conn, err := guestagent.Dial(ctx, e.agentAddr(rec.ID))
	if err != nil {
		return nil, errf("exec_failed", "%v", err)
	}
	t, err := guestagent.NewTerminal(conn, guestagent.Request{Token: tok, Op: "pty", Cols: cols, Rows: rows})
	if err != nil {
		conn.Close()
		return nil, err
	}
	return t, nil
}

// OpenPort opens one stream to a port on the dock's loopback through the guest agent.
func (e *Engine) OpenPort(ctx context.Context, ref string, port int) (net.Conn, error) {
	rec, tok, err := e.running(ref)
	if err != nil {
		return nil, err
	}
	conn, err := guestagent.Dial(ctx, e.agentAddr(rec.ID))
	if err != nil {
		return nil, errf("exec_failed", "%v", err)
	}
	tun, err := guestagent.NewTunnel(conn, tok, port)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return tun, nil
}

// Stop shuts the dock down (graceful through the agent, then killed) and keeps its disk.
func (e *Engine) Stop(ctx context.Context, ref string, force bool) (Dock, error) {
	rec, err := e.Lookup(ref)
	if err != nil {
		return Dock{}, err
	}
	if err := e.stopVM(ctx, rec.ID, force); err != nil {
		return Dock{}, err
	}
	return e.view(ctx, rec), nil
}

func (e *Engine) stopVM(ctx context.Context, id string, force bool) error {
	pid := e.readInt(id, "pid")
	if pid <= 0 || !e.Driver.Alive(pid) {
		e.clearRuntime(id)
		return nil
	}
	if !force {
		if tok, err := e.token(id); err == nil {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			// The call may fail once the guest powers off under it; that is the success path.
			_, _ = guestagent.Call(cctx, e.agentAddr(id), tok, guestagent.Request{
				Op: "exec", Script: "sync; (sudo -n poweroff || poweroff) >/dev/null 2>&1 &", TimeoutSeconds: 5,
			})
			cancel()
		}
		deadline := time.Now().Add(e.stopTimeout())
		for e.Driver.Alive(pid) && time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	if e.Driver.Alive(pid) {
		if err := e.Driver.Kill(pid); err != nil {
			return errf("stop_failed", "%v", err)
		}
		for range 50 {
			if !e.Driver.Alive(pid) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	e.clearRuntime(id)
	return nil
}

func (e *Engine) clearRuntime(id string) {
	_ = os.Remove(e.file(id, "pid"))
	_ = os.Remove(e.file(id, "port"))
}

// Resume boots a stopped dock; a running dock is returned as is.
func (e *Engine) Resume(ctx context.Context, ref string) (Dock, error) {
	rec, err := e.Lookup(ref)
	if err != nil {
		return Dock{}, err
	}
	if e.alive(rec.ID) {
		return e.view(ctx, rec), nil
	}
	if err := e.Driver.Preflight(ctx); err != nil {
		return Dock{}, err
	}
	if err := e.boot(ctx, rec); err != nil {
		return Dock{}, err
	}
	return e.view(ctx, rec), nil
}

// Branch makes count independent docks from a copy of the dock's disk. There is no live
// VM fork: a running source is stopped for the copy and resumed afterwards.
func (e *Engine) Branch(ctx context.Context, ref string, count int, name string, over Spec) ([]Dock, error) {
	src, err := e.Lookup(ref)
	if err != nil {
		return nil, err
	}
	if count < 1 {
		count = 1
	}
	if name != "" && count > 1 {
		return nil, errf("invalid_argument", "a name needs a single branch")
	}
	if over.DiskGiB != 0 && over.DiskGiB != src.DiskGiB {
		return nil, errf("invalid_argument", "a local branch keeps its source's disk size (%d GiB)", src.DiskGiB)
	}
	if name != "" {
		if _, err := e.Lookup(name); err == nil {
			return nil, errf("dock_name_taken", "Dock name already in use")
		}
	}
	if err := e.Driver.Preflight(ctx); err != nil {
		return nil, err
	}
	wasRunning := e.alive(src.ID)
	if wasRunning {
		e.logf("stopping %s for a consistent copy", ref)
		if err := e.stopVM(ctx, src.ID, false); err != nil {
			return nil, err
		}
	}
	var made []Record
	copyErr := func() error {
		for range count {
			rec := src
			rec.ID, rec.Name, rec.Parent, rec.CreatedAt = NewID(), name, src.ID, e.now()
			if over.CPUs > 0 {
				rec.CPUs = over.CPUs
			}
			if over.MemoryMiB > 0 {
				rec.MemoryMiB = over.MemoryMiB
			}
			if err := os.MkdirAll(e.store().DockDir(rec.ID), 0o700); err != nil {
				return errf("store_error", "%v", err)
			}
			if err := copyFile(e.file(src.ID, "disk.qcow2"), e.file(rec.ID, "disk.qcow2")); err != nil {
				_ = e.removeFiles(rec.ID)
				return err
			}
			if err := os.WriteFile(e.file(rec.ID, "token"), []byte(NewToken()), 0o600); err != nil {
				_ = e.removeFiles(rec.ID)
				return errf("store_error", "%v", err)
			}
			if err := e.store().Update(func(recs *[]Record) error {
				*recs = append(*recs, rec)
				return nil
			}); err != nil {
				_ = e.removeFiles(rec.ID)
				return err
			}
			made = append(made, rec)
		}
		return nil
	}()
	var resumeErr error
	if wasRunning {
		resumeErr = e.boot(ctx, src)
	}
	if copyErr != nil {
		return nil, copyErr
	}
	var docks []Dock
	for _, rec := range made {
		if err := e.boot(ctx, rec); err != nil {
			return docks, err
		}
		docks = append(docks, e.view(ctx, rec))
	}
	if resumeErr != nil {
		return docks, resumeErr
	}
	return docks, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return errf("store_error", "%v", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return errf("store_error", "%v", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return errf("store_error", "copy disk: %v", err)
	}
	return out.Close()
}

// Remove stops the dock and deletes its disk and record.
func (e *Engine) Remove(ctx context.Context, ref string) error {
	rec, err := e.Lookup(ref)
	if err != nil {
		return err
	}
	if err := e.stopVM(ctx, rec.ID, true); err != nil {
		return err
	}
	if err := e.store().Update(func(recs *[]Record) error {
		kept := (*recs)[:0]
		for _, r := range *recs {
			if r.ID != rec.ID {
				kept = append(kept, r)
			}
		}
		*recs = kept
		return nil
	}); err != nil {
		return err
	}
	return e.removeFiles(rec.ID)
}

// removeFiles deletes a dock directory, retrying briefly because Windows keeps a dead
// process's files locked for a moment.
func (e *Engine) removeFiles(id string) error {
	dir := e.store().DockDir(id)
	var err error
	for range 20 {
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errf("remove_failed", "%v", err)
}
