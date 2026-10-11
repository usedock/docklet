// Package fc drives Firecracker microVMs through firecracker-go-sdk under
// the jailer, with cgroup limits per dock resources. Files firecracker touches
// (kernel, initrd, rootfs, socket, logs, snapshots) live inside the per-dock
// jail root and are addressed jail-relative; the host stages them there
// before boot. Resume is a fresh boot from the frozen rootfs generation
// (reboot semantics); memory snapshots serve fork and template captures.
package fc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/operations"

	apiv1 "github.com/usedock/docklet/api/v1"
)

// Jail-relative filenames inside one dock jail root. The log name is
// per-dock and relative so both the host-side file creator and the jailed
// firecracker resolve their own copy: <cwd>/logs/<dock>.log on the host,
// <jail>/root/logs/<dock>.log inside the chroot.
const (
	SocketName = "api.sock"
	RootfsName = "rootfs.ext4"
	KernelName = "vmlinux"
	InitrdName = "initrd.img"
	MemName    = "snap.mem"
	StateName  = "snap.state"
)

// BootSpec is everything needed to start one dock microVM. KernelImage and
// InitrdImage are host source files staged into the jail at boot.
type BootSpec struct {
	DockID      string
	Resources   apiv1.Resources
	KernelImage string
	InitrdImage string
	JailRoot    string
	Firecracker string
	Jailer      string
	JailBase    string
	UID         int
	GID         int
	TapName     string
	GuestMAC    string
	KernelArgs  string
	TrackDirty  bool
	// Limits are this dock's resolved I/O and process limits; zero is unlimited.
	Limits Effective
	// AttachNetwork wires the network namespace held by pid before the guest
	// starts; container drivers call it, microVM drivers ignore it.
	AttachNetwork func(pid int) error
}

// Handle identifies a running microVM.
type Handle struct {
	DockID     string
	JailRoot   string
	SocketPath string
	Pid        int
}

// Driver owns microVM processes on this host.
type Driver interface {
	Boot(ctx context.Context, spec BootSpec) (*Handle, error)
	// Adopt rebuilds the client for a VM that outlived a restart, from
	// the sidecar BootSpec written at provision time. No process is spawned.
	Adopt(dockID, jailRoot string, res apiv1.Resources) (*Handle, error)
	Pause(ctx context.Context, h *Handle) error
	Resume(ctx context.Context, h *Handle) error
	CreateSnapshot(ctx context.Context, h *Handle, snapshotType string) error
	SnapshotPaths(h *Handle) (memPath, statePath string)
	// Terminate SIGTERMs the VM process (SIGKILL fallback), keeping files.
	Terminate(h *Handle) error
}

// SDKDriver is the Firecracker-backed Driver; needs /dev/kvm at runtime.
type SDKDriver struct {
	// FirecrackerBin is the configured binary; Terminate matches it so custom names are still stopped.
	FirecrackerBin string
	mu             sync.Mutex
	machines       map[string]*firecracker.Machine
}

// NewSDKDriver returns a driver with an empty machine registry.
func NewSDKDriver() *SDKDriver {
	return &SDKDriver{machines: map[string]*firecracker.Machine{}}
}

// WaitSocketHandlerName gates the handler chain until the VMM socket exists.
const WaitSocketHandlerName = "dock.WaitSocket"

// SocketWaitStrategy stages nothing itself (files are pre-staged into the
// jail) but inserts a socket wait right after the VMM spawns: the SDK dials
// instantly, while a cold firecracker on a small host needs seconds.
type SocketWaitStrategy struct {
	Timeout time.Duration
}

// AdaptHandlers inserts the wait after process spawn.
func (s SocketWaitStrategy) AdaptHandlers(handlers *firecracker.Handlers) error {
	if !handlers.FcInit.Has(firecracker.StartVMMHandlerName) {
		return fmt.Errorf("start handler missing, cannot gate socket wait")
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	wait := firecracker.Handler{
		Name: WaitSocketHandlerName,
		Fn: func(ctx context.Context, m *firecracker.Machine) error {
			root := filepath.Join(m.Cfg.JailerCfg.ChrootBaseDir, "firecracker", m.Cfg.JailerCfg.ID, "root")
			target := root
			if !filepath.IsAbs(m.Cfg.SocketPath) {
				target = filepath.Join(root, m.Cfg.SocketPath)
			} else {
				target = m.Cfg.SocketPath
			}
			deadline := time.Now().Add(timeout)
			for {
				if _, err := os.Stat(target); err == nil {
					return nil
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("api socket %s never appeared", target)
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
			}
		},
	}
	handlers.FcInit = handlers.FcInit.AppendAfter(firecracker.StartVMMHandlerName, wait)
	return nil
}

// MachineConfig builds the SDK machine configuration for a boot.
func MachineConfig(spec BootSpec) (firecracker.Config, error) {
	if err := ValidateResources(spec.Resources); err != nil {
		return firecracker.Config{}, err
	}
	id := spec.DockID
	if len(id) > 64 {
		id = id[:64]
	}
	uid, gid := spec.UID, spec.GID
	cfg := firecracker.Config{
		SocketPath:      SocketName,
		LogPath:         "logs/" + spec.DockID + ".log",
		LogLevel:        "Warning",
		KernelImagePath: KernelName,
		KernelArgs:      spec.KernelArgs,
		MachineCfg: models.MachineConfiguration{
			VcpuCount:       firecracker.Int64(int64(spec.Resources.CPUs)),
			MemSizeMib:      firecracker.Int64(int64(spec.Resources.MemoryMiB)),
			TrackDirtyPages: spec.TrackDirty,
		},
		Drives: []models.Drive{{
			DriveID:      firecracker.String("rootfs"),
			PathOnHost:   firecracker.String(RootfsName),
			IsRootDevice: firecracker.Bool(true),
			IsReadOnly:   firecracker.Bool(false),
			RateLimiter:  driveLimiter(spec.Limits),
		}},
		InitrdPath: initrdName(spec),
		NetworkInterfaces: firecracker.NetworkInterfaces{{
			StaticConfiguration: &firecracker.StaticNetworkConfiguration{
				MacAddress:  spec.GuestMAC,
				HostDevName: spec.TapName,
			},
			InRateLimiter:  netLimiter(spec.Limits),
			OutRateLimiter: netLimiter(spec.Limits),
		}},
		VMID: spec.DockID,
		// Foreground jailer on purpose: with --daemonize the SDK tracks
		// the exiting parent instead of the daemon, reporting healthy VMs
		// as exited. The jailer execs firecracker in place, so tracking is
		// exact; orphans reparent to init and the reconciler fences them.
		// Empty ForwardSignals keeps docklet restarts from TERMing guests.
		ForwardSignals: []os.Signal{},
		JailerCfg: &firecracker.JailerConfig{
			GID:            &gid,
			UID:            &uid,
			ID:             id,
			NumaNode:       firecracker.Int(0),
			ExecFile:       spec.Firecracker,
			JailerBinary:   spec.Jailer,
			ChrootBaseDir:  spec.JailBase,
			ChrootStrategy: SocketWaitStrategy{},
			Daemonize:      false,
			CgroupVersion:  "2",
		},
	}
	return cfg, nil
}

// initrdName enables the staged initrd, or empty for kernel-only boot.
func initrdName(spec BootSpec) string {
	if spec.InitrdImage == "" {
		return ""
	}
	return InitrdName
}

// SnapshotTypeOpt sets Full or Diff on a snapshot create call.
func SnapshotTypeOpt(snapshotType string) firecracker.CreateSnapshotOpt {
	return func(p *operations.CreateSnapshotParams) {
		if p.Body == nil {
			p.Body = &models.SnapshotCreateParams{}
		}
		p.Body.SnapshotType = snapshotType
	}
}

// ErrInvalidResources marks a size that cannot boot.
var ErrInvalidResources = errors.New("invalid resources")

// ValidateResources rejects a size that cannot boot, so no unconstrained VM runs.
func ValidateResources(r apiv1.Resources) error {
	if r.CPUs < 1 || r.MemoryMiB < 128 || r.DiskGiB < 1 {
		return fmt.Errorf("%w %+v: need cpus>=1, memoryMiB>=128, diskGiB>=1", ErrInvalidResources, r)
	}
	return nil
}

// ApplyLimits writes cgroupv2 limits for one dock: cpu.max quota over a 100000
// period plus memory.max bytes. Call after boot once the cgroup exists. The
// jailer creates it without enabling controllers, so the parent delegates
// cpu+memory+io first (the interface files do not exist until it does).
func ApplyLimits(cgroupDir string, r apiv1.Resources) error {
	if err := ValidateResources(r); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(cgroupDir), "cgroup.subtree_control"), []byte("+cpu +memory +io"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cgroupDir, "cpu.max"), []byte(strconv.Itoa(r.CPUs*100000)+" 100000\n"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cgroupDir, "memory.max"), []byte(strconv.FormatInt(int64(r.MemoryMiB)*1024*1024, 10)+"\n"), 0o644)
}

// WriteSidecar records the boot resources beside the jail for post-restart adopt.
func WriteSidecar(jailRoot, dockID string, r apiv1.Resources) error {
	raw, err := json.Marshal(struct {
		DockID    string          `json:"dockId"`
		Resources apiv1.Resources `json:"resources"`
	}{dockID, r})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(jailRoot, "spec.json"), raw, 0o644)
}

// ReadSidecar returns the boot resources recorded at provision.
func ReadSidecar(jailRoot string) (apiv1.Resources, error) {
	raw, err := os.ReadFile(filepath.Join(jailRoot, "spec.json"))
	if err != nil {
		return apiv1.Resources{}, err
	}
	var spec struct {
		Resources apiv1.Resources `json:"resources"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return apiv1.Resources{}, err
	}
	if err := ValidateResources(spec.Resources); err != nil {
		return apiv1.Resources{}, fmt.Errorf("sidecar: %w", err)
	}
	return spec.Resources, nil
}

// link stages one host file into the jail root by hard link, resolving
// symlinks first: a linked symlink dangles inside the chroot.
func link(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	_ = os.Remove(dst)
	return os.Link(real, dst)
}

// crossDeviceHint explains the one failure mode that is a layout mistake rather
// than a fault: staging hard-links the kernel, so the image dir and the jail
// base must live on the same filesystem.
func crossDeviceHint(err error) string {
	if errors.Is(err, syscall.EXDEV) {
		return ": DOCKLET_JAIL_BASE and DOCKLET_IMAGE_DIR must be on the same filesystem (see DEPLOY.md)"
	}
	return ""
}

// prepareJail writes the sidecar and the dock-owned log, and stages the
// kernel and initrd. Firecracker runs dropped to UID/GID and must own its log.
func prepareJail(spec BootSpec, chown func(path string, uid, gid int) error) error {
	root := spec.JailRoot
	if err := os.MkdirAll(filepath.Join(root, "logs"), 0o755); err != nil {
		return err
	}
	if err := WriteSidecar(root, spec.DockID, spec.Resources); err != nil {
		return err
	}
	jailLog := filepath.Join(root, "logs", spec.DockID+".log")
	if err := os.WriteFile(jailLog, nil, 0o644); err != nil {
		return err
	}
	if err := chown(jailLog, spec.UID, spec.GID); err != nil {
		return err
	}
	if err := link(spec.KernelImage, filepath.Join(root, KernelName)); err != nil {
		return fmt.Errorf("stage kernel: %w%s", err, crossDeviceHint(err))
	}
	if spec.InitrdImage != "" {
		if err := link(spec.InitrdImage, filepath.Join(root, InitrdName)); err != nil {
			return fmt.Errorf("stage initrd: %w%s", err, crossDeviceHint(err))
		}
	}
	return nil
}

// Boot stages kernel/initrd into the jail root and starts one microVM.
// The rootfs must already be provisioned at JailRoot/RootfsName.
func (d *SDKDriver) Boot(ctx context.Context, spec BootSpec) (*Handle, error) {
	if _, err := MachineConfig(spec); err != nil {
		return nil, err
	}
	root := spec.JailRoot
	if err := prepareJail(spec, ChownFile); err != nil {
		return nil, err
	}
	cfg, err := MachineConfig(spec)
	if err != nil {
		return nil, err
	}
	machine, err := firecracker.NewMachine(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := machine.Start(ctx); err != nil {
		return nil, err
	}
	pid, err := machine.PID()
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.machines[spec.DockID] = machine
	d.mu.Unlock()
	return &Handle{DockID: spec.DockID, JailRoot: root, SocketPath: filepath.Join(root, SocketName), Pid: pid}, nil
}

// ErrNoMicroVM marks calls for VMs this driver never booted or adopted:
// callers fall back to disk-level work instead of failing the dock.
var ErrNoMicroVM = errors.New("no microVM for dock")

func (d *SDKDriver) machineFor(dockID string) (*firecracker.Machine, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	machine, ok := d.machines[dockID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoMicroVM, dockID)
	}
	return machine, nil
}

// Pause freezes vCPUs for a consistent snapshot.
func (d *SDKDriver) Pause(ctx context.Context, h *Handle) error {
	machine, err := d.machineFor(h.DockID)
	if err != nil {
		return err
	}
	return machine.PauseVM(ctx)
}

// Resume continues a paused microVM.
func (d *SDKDriver) Resume(ctx context.Context, h *Handle) error {
	machine, err := d.machineFor(h.DockID)
	if err != nil {
		return err
	}
	return machine.ResumeVM(ctx)
}

// CreateSnapshot captures memory state into the jail root; type is Full or
// Diff. Paths go to firecracker jail-relative (its CWD is the jail root);
// SnapshotPaths exposes the same files host-side for upload.
func (d *SDKDriver) CreateSnapshot(ctx context.Context, h *Handle, snapshotType string) error {
	machine, err := d.machineFor(h.DockID)
	if err != nil {
		return err
	}
	return machine.CreateSnapshot(ctx, MemName, StateName, SnapshotTypeOpt(snapshotType))
}

// SnapshotPaths returns host paths of the jail memory/state files.
func (d *SDKDriver) SnapshotPaths(h *Handle) (string, string) {
	return filepath.Join(h.JailRoot, MemName), filepath.Join(h.JailRoot, StateName)
}

// Adopt rebuilds the API client for a VM that outlived a restart. The
// socket path is absolute host-side: no jail translation is needed because
// nothing is spawned, only dialed.
func (d *SDKDriver) Adopt(dockID, jailRoot string, res apiv1.Resources) (*Handle, error) {
	if err := ValidateResources(res); err != nil {
		return nil, err
	}
	socket := filepath.Join(jailRoot, SocketName)
	cfg := firecracker.Config{
		SocketPath: socket,
		LogLevel:   "Warning",
		VMID:       dockID,
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(int64(res.CPUs)),
			MemSizeMib: firecracker.Int64(int64(res.MemoryMiB)),
		},
		ForwardSignals: []os.Signal{},
	}
	machine, err := firecracker.NewMachine(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.machines[dockID] = machine
	d.mu.Unlock()
	// The pid file survives restarts; stats and cgroup resolution need it.
	pid, _ := pidOf(jailRoot)
	return &Handle{DockID: dockID, JailRoot: jailRoot, SocketPath: socket, Pid: pid}, nil
}

// readPid returns the jailed firecracker pid recorded at spawn.
func pidOf(jailRoot string) (int, error) {
	raw, err := os.ReadFile(filepath.Join(jailRoot, "firecracker.pid"))
	if err != nil {
		return 0, err
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &pid); err != nil {
		return 0, err
	}
	return pid, nil
}

// cmdlineHasID matches NUL-separated argv of this dock's firecracker: the exact --id pair and,
// when bin is set, the configured binary (path or base name), else any firecracker binary.
func cmdlineHasID(raw []byte, dockID, bin string) bool {
	args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	if len(args) == 0 {
		return false
	}
	if bin != "" {
		if args[0] != bin && filepath.Base(args[0]) != filepath.Base(bin) {
			return false
		}
	} else if !strings.Contains(filepath.Base(args[0]), "firecracker") {
		return false
	}
	for i := 1; i+1 < len(args); i++ {
		if args[i] == "--id" && args[i+1] == dockID {
			return true
		}
	}
	return false
}

// ownsPID reports whether pid under procRoot is this dock's firecracker.
func ownsPID(procRoot string, pid int, dockID, bin string) bool {
	raw, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
	return err == nil && cmdlineHasID(raw, dockID, bin)
}

// matchPIDs returns the dock's firecracker pids: the pid file's when it still matches,
// else every process under procRoot with the exact --id (the pid file is missing or stale).
func matchPIDs(procRoot, jailRoot, dockID, bin string) []int {
	if pid, err := pidOf(jailRoot); err == nil && ownsPID(procRoot, pid, dockID, bin) {
		return []int{pid}
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err == nil && ownsPID(procRoot, pid, dockID, bin) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// FenceStale stops any leftover firecracker for dockID by exact --id match, never by pattern.
// bin is the configured firecracker binary; empty accepts any firecracker-named binary.
func FenceStale(jailRoot, dockID, bin string) {
	for _, pid := range matchPIDs("/proc", jailRoot, dockID, bin) {
		killPID("/proc", pid, dockID, bin)
	}
}

// killPID sends SIGTERM, then SIGKILL after a grace period if the pid is still this dock's firecracker.
func killPID(procRoot string, pid int, dockID, bin string) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	// The pid may have exited and been recycled during the grace period.
	if ownsPID(procRoot, pid, dockID, bin) {
		_ = proc.Signal(syscall.SIGKILL)
	}
}

// Terminate stops the VM process without touching files: SIGTERM, then
// SIGKILL after a grace period. Missing processes are already gone.
func (d *SDKDriver) Terminate(h *Handle) error {
	pid, err := pidOf(h.JailRoot)
	if err != nil {
		return nil
	}
	if ownsPID("/proc", pid, h.DockID, d.FirecrackerBin) {
		killPID("/proc", pid, h.DockID, d.FirecrackerBin)
	}
	d.mu.Lock()
	delete(d.machines, h.DockID)
	d.mu.Unlock()
	return nil
}
