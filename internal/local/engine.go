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
const Runtime = RuntimeQEMU

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
	// Runtime is "qemu" (default) or "gvisor".
	Runtime string
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
	// GVisor runs gvisor docks; nil uses the real Docker CLI.
	GVisor *GVisor
	Arch   string
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

// stamper is a Driver that can tell one process from a later one with the same pid.
type stamper interface {
	StartStamp(pid int) (uint64, bool)
}

// livePID is the dock's VM pid when that process is still running and is the one this
// engine started, else 0. After a crash or a reboot the pid file can name an unrelated
// process that reused the pid; it must never be reported running or killed.
func (e *Engine) livePID(id string) int {
	pid := e.readInt(id, "pid")
	if pid <= 0 || !e.Driver.Alive(pid) {
		return 0
	}
	st, ok := e.Driver.(stamper)
	if !ok {
		return pid
	}
	raw, err := os.ReadFile(e.file(id, "pidstart"))
	if err != nil {
		// Without a recorded start time the pid cannot be trusted.
		return 0
	}
	want, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0
	}
	if got, ok := st.StartStamp(pid); !ok || got != want {
		return 0
	}
	return pid
}

func (e *Engine) alive(id string) bool { return e.livePID(id) > 0 }

// runtime is the dock's runtime; records written before gvisor existed have none and are qemu.
func (r Record) runtime() string {
	if r.Runtime == "" {
		return RuntimeQEMU
	}
	return r.Runtime
}

// aliveRec reports whether the dock's VM or container is running.
func (e *Engine) aliveRec(ctx context.Context, r Record) bool {
	if r.runtime() == RuntimeGVisor {
		return e.gvisor().running(ctx, r.ID)
	}
	return e.alive(r.ID)
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
func (e *Engine) state(ctx context.Context, r Record) string {
	if !e.aliveRec(ctx, r) {
		return "archived"
	}
	if e.ping(ctx, r.ID) {
		return "ready"
	}
	return "starting"
}

func (e *Engine) view(ctx context.Context, r Record) Dock {
	return Dock{ID: r.ID, Name: r.Name, State: e.state(ctx, r), Image: r.Image, Runtime: r.runtime(),
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
	if s.Runtime == "" {
		s.Runtime = RuntimeQEMU
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
	case s.Runtime != RuntimeQEMU && s.Runtime != RuntimeGVisor:
		return s, errf("invalid_runtime", "runtime must be qemu or gvisor, not %q", s.Runtime)
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
	diskSet := spec.DiskGiB != 0
	spec, err := spec.withDefaults()
	if err != nil {
		return Dock{}, err
	}
	if spec.Name != "" {
		if _, err := e.Lookup(spec.Name); err == nil {
			return Dock{}, errf("dock_name_taken", "Dock name already in use")
		}
	}
	if spec.Runtime == RuntimeGVisor {
		return e.newGVisor(ctx, spec, diskSet)
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
	if err := e.mkDockDir(rec.ID); err != nil {
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
	if err := e.addRecord(rec); err != nil {
		return fail(err)
	}
	e.logf("booting %s", rec.ID)
	if err := e.boot(ctx, rec); err != nil {
		_ = e.Remove(ctx, rec.ID)
		return Dock{}, err
	}
	return e.view(ctx, rec), nil
}

// addRecord stores rec, re-checking name uniqueness under the store lock: another dock
// command may have taken the name since the early check.
func (e *Engine) addRecord(rec Record) error {
	return e.store().Update(func(recs *[]Record) error {
		if rec.Name != "" {
			for _, r := range *recs {
				if r.Name == rec.Name {
					return errf("dock_name_taken", "Dock name already in use")
				}
			}
		}
		*recs = append(*recs, rec)
		return nil
	})
}

// mkDockDir creates a dock directory inside a data dir that only this user can read: the
// dock directory holds its agent token.
func (e *Engine) mkDockDir(id string) error {
	if err := os.MkdirAll(e.Dir, 0o700); err != nil {
		return err
	}
	if err := secureDir(e.Dir); err != nil {
		return fmt.Errorf("restrict %s to this user: %w", e.Dir, err)
	}
	return os.MkdirAll(e.store().DockDir(id), 0o700)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// bootAttempts bounds retries when QEMU cannot bind the forwarded port because another
// process took it between choosing it and QEMU starting.
const bootAttempts = 3

// boot starts the VM for rec and waits for its agent.
func (e *Engine) boot(ctx context.Context, rec Record) error {
	kernel, initrd := filepath.Join(rec.ImageDir, dockyard.FileKernel), filepath.Join(rec.ImageDir, dockyard.FileInitrd)
	if _, err := os.Stat(kernel); err != nil {
		return errf("image_missing", "the image files of %s are gone (%s); run: dock images pull %s", rec.ID, rec.ImageDir, rec.Image)
	}
	tok, err := e.token(rec.ID)
	if err != nil {
		return err
	}
	cfg := e.file(rec.ID, "guest.json")
	// A copied disk carries its source's sshd host keys: the first boot after the copy drops them.
	_, resetErr := os.Stat(e.file(rec.ID, resetKeysMarker))
	opts := GuestOptions{ResetHostKeys: resetErr == nil, BlockGateway: rec.Network != "off"}
	if err := WriteGuestConfig(cfg, tok, rec.Name, opts); err != nil {
		return errf("store_error", "%v", err)
	}
	for attempt := 1; ; attempt++ {
		err := e.bootOnce(ctx, rec, kernel, initrd, cfg, tok)
		if err == nil && opts.ResetHostKeys {
			_ = os.Remove(e.file(rec.ID, resetKeysMarker))
		}
		if err == nil || attempt >= bootAttempts || ErrCode(err) != "boot_failed" || !strings.Contains(e.logTail(rec.ID), "host forwarding") {
			return err
		}
		e.logf("port taken, retrying with another one")
	}
}

func (e *Engine) bootOnce(ctx context.Context, rec Record, kernel, initrd, cfg, tok string) error {
	port, err := freePort()
	if err != nil {
		return errf("start_failed", "no free local port: %v", err)
	}
	if err := os.WriteFile(e.file(rec.ID, "port"), []byte(strconv.Itoa(port)), 0o600); err != nil {
		return errf("store_error", "%v", err)
	}
	pid, err := e.Driver.Start(ctx, LaunchSpec{
		Kernel: kernel, Initrd: initrd, Disk: e.file(rec.ID, "disk.qcow2"),
		SerialLog: e.file(rec.ID, "serial.log"), StderrLog: e.file(rec.ID, "qemu.log"),
		HostPort: port, ConfigFile: cfg, Token: tok, CPUs: rec.CPUs, MemoryMiB: rec.MemoryMiB, Network: rec.Network,
	})
	if err != nil {
		return err
	}
	if st, ok := e.Driver.(stamper); ok {
		start, ok := st.StartStamp(pid)
		if !ok {
			_ = e.Driver.Kill(pid)
			return errf("boot_failed", "the VM exited during boot.%s", e.logTail(rec.ID))
		}
		if err := os.WriteFile(e.file(rec.ID, "pidstart"), []byte(strconv.FormatUint(start, 10)), 0o600); err != nil {
			_ = e.Driver.Kill(pid)
			return errf("store_error", "%v", err)
		}
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
		// The pid must still be our VM before anything is trusted on the forwarded port:
		// if QEMU died (for example it could not bind the port) another process may hold it.
		if e.livePID(id) != pid {
			return errf("boot_failed", "the VM exited during boot.%s", e.logTail(id))
		}
		if e.ping(ctx, id) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			// Same rule as stopVM: never kill a pid that is no longer our VM.
			if e.livePID(id) == pid {
				_ = e.Driver.Kill(pid)
			}
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
	if !e.aliveRec(context.Background(), rec) {
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
	stop := e.stopVM
	if rec.runtime() == RuntimeGVisor {
		stop = e.stopGVisor
	}
	if err := stop(ctx, rec.ID, force); err != nil {
		return Dock{}, err
	}
	return e.view(ctx, rec), nil
}

func (e *Engine) stopVM(ctx context.Context, id string, force bool) error {
	pid := e.livePID(id)
	if pid == 0 {
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
		for e.livePID(id) == pid && time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	// The VM may have exited during the wait and its pid been given to an unrelated process,
	// so the identity (start stamp included) is checked again right before the kill; a
	// changed or missing identity means the VM is already stopped.
	if e.livePID(id) == pid {
		if err := e.Driver.Kill(pid); err != nil {
			return errf("stop_failed", "%v", err)
		}
		for range 50 {
			if e.livePID(id) != pid {
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
	_ = os.Remove(e.file(id, "pidstart"))
	_ = os.Remove(e.file(id, "port"))
}

// Resume boots a stopped dock; a running dock is returned as is.
func (e *Engine) Resume(ctx context.Context, ref string) (Dock, error) {
	rec, err := e.Lookup(ref)
	if err != nil {
		return Dock{}, err
	}
	if e.aliveRec(ctx, rec) {
		return e.view(ctx, rec), nil
	}
	if rec.runtime() == RuntimeGVisor {
		image, suid, err := e.gvPrepare(ctx)
		if err != nil {
			return Dock{}, err
		}
		if err := e.resumeGVisor(ctx, rec, image, suid); err != nil {
			return Dock{}, err
		}
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

// resetKeysMarker names the file in a dock dir that asks for fresh sshd host keys at the next boot.
const resetKeysMarker = "reset-hostkeys"

// markResetKeys flags a dock made from a copy of another disk; boot consumes it after the first start.
func (e *Engine) markResetKeys(id string) error {
	if err := os.WriteFile(e.file(id, resetKeysMarker), nil, 0o600); err != nil {
		return errf("store_error", "%v", err)
	}
	return nil
}

// Branch makes count independent docks from a copy of the dock's disk. There is no live
// VM fork: a running source is stopped for the copy and resumed afterwards.
func (e *Engine) Branch(ctx context.Context, ref string, count int, name string, over Spec) ([]Dock, error) {
	src, err := e.Lookup(ref)
	if err != nil {
		return nil, err
	}
	if src.runtime() == RuntimeGVisor {
		return nil, gvUnsupported("branch")
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
			if err := e.mkDockDir(rec.ID); err != nil {
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
			if err := e.markResetKeys(rec.ID); err != nil {
				_ = e.removeFiles(rec.ID)
				return err
			}
			if err := e.addRecord(rec); err != nil {
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
	if rec.runtime() == RuntimeGVisor {
		// With no docker CLI at all (uninstalled) the record must still be removable; a docker
		// that is installed but whose daemon is down fails with docker_unavailable so the
		// user can start it and retry without leaking the volume.
		if g := e.gvisor(); !g.hasDocker() {
			e.logf("warning: docker was not found, so the container %s, volume %s and network %s could not be removed; remove them by hand if Docker comes back",
				gvContainer(rec.ID), gvVolume(rec.ID), gvNetwork(rec.ID))
		} else {
			if err := e.removeGVisor(ctx, rec.ID); err != nil {
				return err
			}
		}
	} else if err := e.stopVM(ctx, rec.ID, true); err != nil {
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
