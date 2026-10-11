package local

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/usedock/docklet/internal/guestagent"
)

// GuestConfig is the guest agent config, the same shape internal/host writes to
// /etc/dock-fast-agent.json. Local docks pass it on the kernel command line.
type GuestConfig struct {
	Token     string   `json:"token"`
	Workspace string   `json:"workspace"`
	Roots     []string `json:"roots"`
	// Hostname is applied by the guest at boot when it is a valid host name.
	Hostname string `json:"hostname,omitempty"`
}

// DefaultGuestConfig is the config for a dock with the given agent token.
func DefaultGuestConfig(token string) GuestConfig {
	return GuestConfig{Token: token, Workspace: "/home/user/work", Roots: []string{"/home/user/work", "/run/dock-fast"}}
}

var hostnameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,62})$`)

// FWCfgName is the fw_cfg entry the guest reads its agent config from, at
// /sys/firmware/qemu_fw_cfg/by_name/opt/usedock/cfg/raw.
const FWCfgName = "opt/usedock/cfg"

// WriteGuestConfig writes the guest agent config as JSON, readable by the owner only.
func WriteGuestConfig(path, token, hostname string) error {
	cfg := DefaultGuestConfig(token)
	if hostnameRE.MatchString(hostname) {
		cfg.Hostname = hostname
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// LaunchSpec is everything needed to boot one dock.
type LaunchSpec struct {
	Kernel, Initrd, Disk string
	SerialLog, StderrLog string
	HostPort             int
	// ConfigFile holds the guest agent config as JSON; it reaches the guest through
	// fw_cfg, never the command line, because other users can read a process argv.
	ConfigFile string
	// Token is the agent token. It is not part of the argv.
	Token           string
	CPUs, MemoryMiB int
	// Network is "on" or "off"; off restricts the guest from the outside world.
	Network string
}

// Driver is the hypervisor side of the engine. QEMU is the real one; tests use a fake.
type Driver interface {
	// Preflight fails with an actionable error when docks cannot run on this machine.
	Preflight(ctx context.Context) error
	CreateOverlay(ctx context.Context, base, disk string) error
	ResizeDisk(ctx context.Context, disk string, sizeGiB int) error
	ConvertRaw(ctx context.Context, raw, qcow2 string) error
	// FlattenDisk writes the whole guest-visible disk of a qcow2 (backing chain included) to a raw file.
	FlattenDisk(ctx context.Context, qcow2, raw string) error
	Start(ctx context.Context, spec LaunchSpec) (pid int, err error)
	Alive(pid int) bool
	Kill(pid int) error
}

// qemuArch maps a Go architecture to the QEMU system emulator suffix.
func qemuArch(goarch string) (string, error) {
	switch goarch {
	case "amd64":
		return "x86_64", nil
	case "arm64":
		return "aarch64", nil
	}
	return "", errf("unsupported_arch", "local docks support amd64 and arm64, not %s", goarch)
}

// BuildArgv returns the qemu arguments (without the binary) for one dock.
func BuildArgv(l LaunchSpec, goarch, accel string) []string {
	console := "ttyS0"
	var args []string
	if goarch == "arm64" {
		console = "ttyAMA0"
		args = append(args, "-machine", "virt")
	} else {
		args = append(args, "-machine", "q35")
	}
	switch accel {
	case "whpx":
		// WHPX has no in-kernel irqchip and cannot expose the host CPU model.
		args = append(args, "-accel", "whpx,kernel-irqchip=off", "-cpu", "qemu64")
	case "kvm", "hvf":
		args = append(args, "-accel", accel, "-cpu", "host")
	default:
		args = append(args, "-accel", "tcg", "-cpu", "max")
	}
	netdev := "user,id=n0,hostfwd=tcp:127.0.0.1:" + strconv.Itoa(l.HostPort) + "-:" + strconv.Itoa(guestagent.Port)
	if l.Network == "off" {
		netdev += ",restrict=on"
	}
	cmdline := "console=" + console + " root=/dev/vda rw rootfstype=ext4 rootwait"
	args = append(args,
		"-smp", strconv.Itoa(l.CPUs),
		"-m", strconv.Itoa(l.MemoryMiB),
		"-nodefaults", "-no-user-config",
		"-display", "none", "-monitor", "none",
		"-serial", "file:"+l.SerialLog,
		"-kernel", l.Kernel,
		"-initrd", l.Initrd,
		"-append", cmdline,
		"-fw_cfg", "name="+FWCfgName+",file="+strings.ReplaceAll(l.ConfigFile, ",", ",,"),
		"-drive", "file="+strings.ReplaceAll(l.Disk, ",", ",,")+",if=virtio,format=qcow2",
		"-netdev", netdev,
		"-device", "virtio-net-pci,netdev=n0",
		"-device", "virtio-rng-pci",
	)
	return args
}

// Accelerator picks the hardware accelerator for goos. It never falls back to software
// emulation unless DOCK_LOCAL_ACCEL=tcg asks for it.
func Accelerator(goos string, getenv func(string) string, kvmUsable func() error) (string, error) {
	switch v := getenv("DOCK_LOCAL_ACCEL"); v {
	case "":
	case "tcg":
		return "tcg", nil
	default:
		return "", errf("invalid_accel", "DOCK_LOCAL_ACCEL=%q is not supported; the only override is tcg (slow software emulation, for tests)", v)
	}
	switch goos {
	case "linux":
		if err := kvmUsable(); err != nil {
			return "", errf("no_accelerator", "KVM is not usable (%v). Check that the CPU supports virtualization, that it is enabled in firmware, and that your user can read and write /dev/kvm (for example: sudo usermod -aG kvm $USER, then log in again)", err)
		}
		return "kvm", nil
	case "windows":
		return "whpx", nil
	case "darwin":
		return "hvf", nil
	}
	return "", errf("unsupported_os", "local docks are not supported on %s", goos)
}

func kvmUsable() error {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return f.Close()
}

// WHPXFix is the fix text for a machine without Windows Hypervisor Platform.
const WHPXFix = "Windows Hypervisor Platform is not available. In an elevated PowerShell run:\n" +
	"  Enable-WindowsOptionalFeature -Online -FeatureName HypervisorPlatform\n" +
	"then reboot, and check that virtualization is enabled in firmware."

// InstallHint says how to get QEMU on goos.
func InstallHint(goos string) string {
	switch goos {
	case "windows":
		return "Install QEMU (winget install SoftwareFreedomConservancy.QEMU, or https://www.qemu.org/download/#windows) or set DOCK_QEMU to qemu-system-x86_64.exe."
	case "darwin":
		return "Install QEMU with: brew install qemu (or set DOCK_QEMU)."
	}
	return "Install QEMU, for example: sudo apt install qemu-system-x86 qemu-utils (or set DOCK_QEMU)."
}

// searchDirs lists the standard install directories to look in after PATH.
func searchDirs(goos string, getenv func(string) string) []string {
	switch goos {
	case "windows":
		dirs := []string{`C:\Program Files\qemu`, `C:\Program Files (x86)\qemu`}
		if v := getenv("ProgramFiles"); v != "" {
			dirs = append(dirs, v+`\qemu`)
		}
		if v := getenv("LOCALAPPDATA"); v != "" {
			dirs = append(dirs, v+`\Programs\qemu`)
		}
		return dirs
	case "darwin":
		return []string{"/opt/homebrew/bin", "/usr/local/bin", "/opt/local/bin"}
	}
	return []string{"/usr/bin", "/usr/local/bin"}
}

// findTool locates name via the env override, PATH, then the standard directories.
func findTool(name, envVar, goos string, getenv func(string) string, lookPath func(string) (string, error), exists func(string) bool) (string, error) {
	if goos == "windows" {
		name += ".exe"
	}
	if envVar != "" {
		if v := getenv(envVar); v != "" {
			if !exists(v) {
				return "", errf("qemu_not_found", "%s=%s does not exist", envVar, v)
			}
			return v, nil
		}
	}
	if p, err := lookPath(name); err == nil {
		return p, nil
	}
	sep := "/"
	if goos == "windows" {
		sep = `\`
	}
	for _, d := range searchDirs(goos, getenv) {
		if p := d + sep + name; exists(p) {
			return p, nil
		}
	}
	return "", errf("qemu_not_found", "%s was not found on PATH or in the standard install directories. %s", name, InstallHint(goos))
}

// QEMU drives qemu-system and qemu-img. Fields left nil use the real machine.
type QEMU struct {
	GOOS, GOARCH string
	Getenv       func(string) string
	// Run executes a command and returns its combined output; nil uses os/exec.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// Probe starts a command and reports whether it was still running after wait.
	Probe func(ctx context.Context, name string, args []string, wait time.Duration) (running bool, out []byte, err error)
	// KVM reports whether /dev/kvm is usable; nil checks the device.
	KVM func() error

	system, img string
}

// NewQEMU returns a driver for this machine.
func NewQEMU(getenv func(string) string) *QEMU {
	return &QEMU{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Getenv: getenv}
}

func (q *QEMU) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if q.Run != nil {
		return q.Run(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (q *QEMU) probe(ctx context.Context, name string, args []string, wait time.Duration) (bool, []byte, error) {
	if q.Probe != nil {
		return q.Probe(ctx, name, args, wait)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return false, nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return false, out.Bytes(), err
	case <-time.After(wait):
		cancel()
		<-done
		return true, out.Bytes(), nil
	}
}

func (q *QEMU) locate() error {
	if q.system != "" {
		return nil
	}
	arch, err := qemuArch(q.GOARCH)
	if err != nil {
		return err
	}
	exists := func(p string) bool { st, err := os.Stat(p); return err == nil && !st.IsDir() }
	sys, err := findTool("qemu-system-"+arch, "DOCK_QEMU", q.GOOS, q.Getenv, exec.LookPath, exists)
	if err != nil {
		return err
	}
	// qemu-img normally sits next to the emulator.
	imgName := "qemu-img"
	if q.GOOS == "windows" {
		imgName += ".exe"
	}
	var img string
	if v := q.Getenv("DOCK_QEMU_IMG"); v != "" {
		img = v
	} else if sib := filepath.Join(filepath.Dir(sys), imgName); exists(sib) {
		img = sib
	} else if img, err = findTool("qemu-img", "", q.GOOS, q.Getenv, exec.LookPath, exists); err != nil {
		return err
	}
	q.system, q.img = sys, img
	return nil
}

// Binaries returns the located emulator and qemu-img paths.
func (q *QEMU) Binaries() (system, img string, err error) {
	if err := q.locate(); err != nil {
		return "", "", err
	}
	return q.system, q.img, nil
}

// Accel is the accelerator this machine will use.
func (q *QEMU) Accel() (string, error) {
	kvm := q.KVM
	if kvm == nil {
		kvm = kvmUsable
	}
	return Accelerator(q.GOOS, q.Getenv, kvm)
}

// Preflight checks QEMU, qemu-img and the accelerator.
func (q *QEMU) Preflight(ctx context.Context) error {
	if err := q.locate(); err != nil {
		return err
	}
	accel, err := q.Accel()
	if err != nil {
		return err
	}
	return q.CheckAccel(ctx, accel)
}

// CheckAccel asks QEMU whether it was built with accel and, for WHPX, boots a probe
// machine because the feature can be installed yet unusable (virtualization off).
func (q *QEMU) CheckAccel(ctx context.Context, accel string) error {
	if accel == "tcg" {
		return nil
	}
	if err := q.locate(); err != nil {
		return err
	}
	out, err := q.run(ctx, q.system, "-accel", "help")
	if err != nil {
		return errf("qemu_failed", "%s -accel help failed: %v", q.system, err)
	}
	if !strings.Contains(string(out), accel) {
		if accel == "whpx" {
			return errf("no_accelerator", "this QEMU build has no whpx accelerator. Use the official QEMU for Windows build. %s", InstallHint(q.GOOS))
		}
		return errf("no_accelerator", "this QEMU build has no %s accelerator", accel)
	}
	if accel != "whpx" {
		return nil
	}
	running, pout, perr := q.probe(ctx, q.system, []string{"-accel", "whpx,kernel-irqchip=off", "-machine", "q35", "-display", "none", "-nodefaults", "-monitor", "none", "-S"}, 3*time.Second)
	if running {
		return nil
	}
	msg := strings.TrimSpace(string(pout))
	if perr != nil && msg == "" {
		msg = perr.Error()
	}
	return errf("no_accelerator", "QEMU could not start a Windows Hypervisor Platform machine (%s).\n%s", msg, WHPXFix)
}

// CreateOverlay makes disk a qcow2 overlay on base; the backing path is absolute.
func (q *QEMU) CreateOverlay(ctx context.Context, base, disk string) error {
	if err := q.locate(); err != nil {
		return err
	}
	abs, err := filepath.Abs(base)
	if err != nil {
		return err
	}
	if out, err := q.run(ctx, q.img, "create", "-f", "qcow2", "-b", abs, "-F", "qcow2", disk); err != nil {
		return errf("qemu_img_failed", "qemu-img create: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ResizeDisk grows the virtual size of disk to sizeGiB.
func (q *QEMU) ResizeDisk(ctx context.Context, disk string, sizeGiB int) error {
	if err := q.locate(); err != nil {
		return err
	}
	if out, err := q.run(ctx, q.img, "resize", disk, fmt.Sprintf("%dG", sizeGiB)); err != nil {
		if strings.Contains(string(out), "shrink") {
			return errf("invalid_argument", "--disk %d GiB is smaller than the image's own disk; use a larger size", sizeGiB)
		}
		return errf("qemu_img_failed", "qemu-img resize: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ConvertRaw converts a raw disk image into a qcow2 file.
func (q *QEMU) ConvertRaw(ctx context.Context, raw, qcow2 string) error {
	if err := q.locate(); err != nil {
		return err
	}
	if out, err := q.run(ctx, q.img, "convert", "-f", "raw", "-O", "qcow2", raw, qcow2); err != nil {
		return errf("qemu_img_failed", "qemu-img convert: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// FlattenDisk converts a qcow2 disk, following its backing file, into a sparse raw image.
func (q *QEMU) FlattenDisk(ctx context.Context, qcow2, raw string) error {
	if err := q.locate(); err != nil {
		return err
	}
	if out, err := q.run(ctx, q.img, "convert", "-f", "qcow2", "-O", "raw", qcow2, raw); err != nil {
		return errf("qemu_img_failed", "qemu-img convert: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Start launches qemu detached from this process and returns its pid.
func (q *QEMU) Start(ctx context.Context, spec LaunchSpec) (int, error) {
	if err := q.locate(); err != nil {
		return 0, err
	}
	accel, err := q.Accel()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(q.system, BuildArgv(spec, q.GOARCH, accel)...)
	detach(cmd)
	if spec.StderrLog != "" {
		f, err := os.OpenFile(spec.StderrLog, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return 0, errf("start_failed", "%v", err)
		}
		defer f.Close()
		cmd.Stderr, cmd.Stdout = f, f
	}
	if err := cmd.Start(); err != nil {
		return 0, errf("start_failed", "start %s: %v", q.system, err)
	}
	// Reap the child if this process outlives it; otherwise it simply stays detached.
	go func() { _ = cmd.Wait() }()
	return cmd.Process.Pid, nil
}

// Alive reports whether pid is a running process.
func (q *QEMU) Alive(pid int) bool { return pidAlive(pid) }

// StartStamp identifies the process behind pid so a reused pid is not mistaken for the VM.
func (q *QEMU) StartStamp(pid int) (uint64, bool) { return procStart(pid) }

// Kill ends pid immediately.
func (q *QEMU) Kill(pid int) error { return killPid(pid) }
