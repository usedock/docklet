// Package host orchestrates one bare-metal machine: TAP and IP setup,
// rootfs provisioning and network gating, Firecracker boot, snapshot
// generations, stats, and status inventory. The HTTP layer translates.
package host

import (
	"archive/tar"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	stdnet "net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/usedock/docklet/api/v1"
	"github.com/usedock/docklet/internal/disk"
	"github.com/usedock/docklet/internal/egress"
	"github.com/usedock/docklet/internal/exec"
	"github.com/usedock/docklet/internal/fc"
	"github.com/usedock/docklet/internal/ids"
	"github.com/usedock/docklet/internal/net"
	"github.com/usedock/docklet/internal/snapmgr"
	"github.com/usedock/docklet/internal/stats"
	"github.com/usedock/docklet/internal/uid"
)

// Config wires one host; zero values are rejected by New.
type Config struct {
	HostID         string
	Family         string
	ImageBase      string
	KernelImage    string
	InitrdImage    string
	VMsDir         string
	FirecrackerBin string
	JailerBin      string
	JailBase       string
	CgroupBase     string
	Subnet         string
	// UID/GID own guest workspace files and are the jailer id only when UIDBase is 0.
	UID int
	GID int
	// UIDBase and UIDSize reserve the range each microvm dock draws its jailer uid/gid from; 0 UIDBase keeps the shared UID/GID (deprecated).
	UIDBase int
	UIDSize int
	// Limits are the host-wide I/O, bandwidth and pids limits, resolved per dock size.
	Limits fc.Limits
	// Chown overrides os.Chown for dock files (tests only).
	Chown     func(path string, uid, gid int) error
	Driver    Backend
	Snaps     *snapmgr.Coordinator
	Stats     stats.Reader
	NetRunner exec.Runner
	Mount     disk.Mounter
	// PoolSize pre-boots fast VMs for instant claims; 0 disables the pool.
	PoolSize int
	// Egress confines restricted guests to allowlisted hosts via the proxy on EgressPort; nil refuses restricted boots.
	Egress     *egress.Proxy
	EgressPort int
	// EgressAllowPrivate lists private CIDRs guests may still reach.
	EgressAllowPrivate []string
	// CapacityVCPU and CapacityMemMiB budget the summed requested resources of live docks; 0 leaves that resource unbounded.
	CapacityVCPU   int64
	CapacityMemMiB int64
	// DiskTotalBytes reports host disk capacity for allocated-disk admission; nil leaves it off.
	DiskTotalBytes func() int64
	// DiskOvercommit scales DiskTotalBytes into the allocatable disk budget; 0 means 1.0.
	DiskOvercommit float64
	// DiskFreeBytes reports free host disk for gvisor work disks; nil leaves disk admission off.
	DiskFreeBytes func() int64
	// AgentCall overrides guest-agent calls (tests only).
	AgentCall func(ctx context.Context, ip, token string, req fastAgentRequest) (fastAgentResponse, error)
	// AgentDial overrides raw guest-agent connections for terminals (tests only).
	AgentDial func(ctx context.Context, addr string) (stdnet.Conn, error)
}

// vm tracks live per-dock host state.
type vm struct {
	// res is the size this machine holds against the capacity budget.
	res           apiv1.Resources
	tap           string
	guestIP       string
	pid           int
	running       bool
	fastToken     string
	fastWorkspace string
	// backing is the pool slot id a claimed dock still runs under; empty when the dock id is its own.
	backing string
	// ephemeral marks Fast Run runtimes and pool slots, which the docks table never tracks.
	ephemeral bool
	// egressGateway is set while the guest is confined to its allowlist.
	egressGateway string
	// cpu is the previous stats reading, so CPU is reported as a rate.
	cpu stats.CPUMark
	// disk caches the last disk-usage walk; it is too costly for every sample.
	disk diskMark
}

type diskMark struct {
	bytes int64
	at    time.Time
}

// diskUsageTTL bounds how stale a reported disk usage may be.
const diskUsageTTL = time.Minute

type fastSpec struct {
	token     string
	workspace string
	egress    []string
	ephemeral bool
}

// Host is the orchestrator.
type Host struct {
	cfg       Config
	alloc     *net.Allocator
	mu        sync.Mutex
	vms       map[string]*vm
	pending   map[string]apiv1.Resources
	fastSpecs map[string]fastSpec
	pl        *pool
	uids      *uid.Allocator
}

// growDisk is disk.Grow; tests swap it for a scaled-down grow so no test
// materialises or compresses a multi-GiB image.
var growDisk = disk.Grow

// New validates config and opens the IP allocator.
func New(cfg Config, stateDir string) (*Host, error) {
	if cfg.HostID == "" || cfg.ImageBase == "" || cfg.Driver == nil || cfg.Snaps == nil {
		return nil, fmt.Errorf("host missing HostID, ImageBase, Driver, or Snaps")
	}
	alloc, err := net.OpenAllocator(stateDir, cfg.Subnet)
	if err != nil {
		return nil, err
	}
	h := &Host{cfg: cfg, alloc: alloc, vms: map[string]*vm{}, pending: map[string]apiv1.Resources{}, fastSpecs: map[string]fastSpec{}}
	if cfg.UIDBase > 0 {
		size := cfg.UIDSize
		if size == 0 {
			size = uid.DefaultSize
		}
		if h.uids, err = uid.Open(stateDir, cfg.UIDBase, size); err != nil {
			return nil, err
		}
	}
	if cfg.PoolSize > 0 {
		h.pl = newPool(cfg.PoolSize)
	}
	h.Rescan()
	return h, nil
}

// Rescan re-tracks microVMs that outlived a docklet restart or were missed
// while booting during the last pass: a dialable API socket plus the sidecar
// spec rebuilds the driver client. Anything else stays untracked for the
// reconciler to fence. Returns newly adopted dock ids. Safe to run often.
func (h *Host) Rescan() []string {
	var adopted []string
	assigned := h.alloc.ByDock()
	entries, err := os.ReadDir(filepath.Join(h.cfg.JailBase, "firecracker"))
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dockID := entry.Name()
		jail := filepath.Join(h.cfg.JailBase, "firecracker", dockID, "root")
		// A claimed pool slot is tracked under its runtime id, never as the slot itself.
		owner := readClaim(jail)
		if owner == "" {
			owner = dockID
		}
		h.mu.Lock()
		_, tracked := h.vms[owner]
		for _, v := range h.vms {
			tracked = tracked || v.backing == dockID
		}
		h.mu.Unlock()
		if tracked {
			continue
		}
		iface := TapFor(dockID)
		if h.usesVeth() {
			iface = net.VethFor(dockID)
		} else {
			sock := filepath.Join(jail, fc.SocketName)
			conn, err := stdnet.DialTimeout("unix", sock, time.Second)
			if err != nil {
				continue
			}
			_ = conn.Close()
		}
		res, err := fc.ReadSidecar(jail)
		if err != nil {
			continue
		}
		handle, err := h.cfg.Driver.Adopt(dockID, jail, res)
		if err != nil {
			continue
		}
		token, workspace := readExecAgent(jail)
		live := &vm{res: res, tap: iface, guestIP: assigned[owner], pid: handle.Pid, running: true, fastToken: token, fastWorkspace: workspace}
		if owner != dockID {
			live.backing = dockID
			live.ephemeral = true
		}
		h.mu.Lock()
		h.vms[owner] = live
		h.mu.Unlock()
		adopted = append(adopted, owner)
	}
	return adopted
}

// claimSidecar names the runtime id a claimed pool slot serves, so a restart re-tracks it under that id.
const claimSidecar = "claimed-by"

// readClaim returns the runtime id a slot jail was claimed by, or empty.
func readClaim(jail string) string {
	raw, err := os.ReadFile(filepath.Join(jail, claimSidecar))
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(raw))
	if checkIDs(id) != nil {
		return ""
	}
	return id
}

// TapFor derives a stable <=15 char TAP name per dock.
func TapFor(dockID string) string {
	sum := md5.Sum([]byte(dockID))
	return "dt-" + hex.EncodeToString(sum[:])[:8]
}

// vethAttacher moves a dock's veth peer into the namespace the driver holds
// for it, before the guest starts.
func (h *Host) vethAttacher(veth, guestIP, gateway, mask string) func(pid int) error {
	return func(pid int) error {
		return net.AttachVeth(h.cfg.NetRunner, veth, pid, guestIP, gateway, mask)
	}
}

// usesVeth reports whether the backend runs containers on veth pairs
// instead of microVMs on TAP devices.
func (h *Host) usesVeth() bool {
	v, ok := h.cfg.Driver.(interface{ UseVeth() bool })
	return ok && v.UseVeth()
}

// diskOnly reports whether the backend captures disk-only generations
// with no memory images.
func (h *Host) diskOnly() bool {
	v, ok := h.cfg.Driver.(interface{ DiskOnly() bool })
	return ok && v.DiskOnly()
}

// ifaceFor returns the host-side interface name for a dock.
func (h *Host) ifaceFor(dockID string) string {
	if h.usesVeth() {
		return net.VethFor(dockID)
	}
	return TapFor(dockID)
}

// dockOwner returns the uid/gid a microvm dock's jailer, TAP and disk belong to:
// the dock's own id from the reserved range, or the shared UID/GID when no range is set.
func (h *Host) dockOwner(dockID string) (int, int, error) {
	if h.uids == nil {
		return h.cfg.UID, h.cfg.GID, nil
	}
	id, err := h.uids.Assign(dockID)
	return id, id, err
}

// releaseOwner frees a destroyed dock's uid for reuse.
func (h *Host) releaseOwner(dockID string) {
	if h.uids != nil {
		_ = h.uids.Release(dockID)
	}
}

func (h *Host) chown(path string, owner, group int) error {
	if h.cfg.Chown != nil {
		return h.cfg.Chown(path, owner, group)
	}
	return os.Chown(path, owner, group)
}

// ensureIface creates and raises the host-side interface for a dock.
func (h *Host) ensureIface(iface string, owner int) error {
	if h.usesVeth() {
		return net.EnsureVeth(h.cfg.NetRunner, iface)
	}
	// A TAP left by an older boot keeps its old owner (the shared uid before per-dock ids), and
	// firecracker cannot open a TAP it does not own: recreate it so the owner always matches.
	if h.uids != nil {
		_ = net.DeleteTAP(h.cfg.NetRunner, iface)
	}
	return net.EnsureTAP(h.cfg.NetRunner, iface, owner)
}

// deleteIface removes the host-side interface, ignoring absence.
func (h *Host) deleteIface(iface string) error {
	if h.usesVeth() {
		return net.DeleteVeth(h.cfg.NetRunner, iface)
	}
	return net.DeleteTAP(h.cfg.NetRunner, iface)
}

// MacFor derives a stable locally-administered guest MAC per dock.
func MacFor(dockID string) string {
	sum := md5.Sum([]byte("mac:" + dockID))
	return fmt.Sprintf("AA:FC:%02X:%02X:%02X:%02X", sum[0], sum[1], sum[2], sum[3])
}

// checkIDs rejects unsafe ids before they reach a path join; optional ids may be empty.
func checkIDs(required string, optional ...string) error {
	if err := ids.Check("id", required); err != nil {
		return err
	}
	for _, id := range optional {
		if id == "" {
			continue
		}
		if err := ids.Check("id", id); err != nil {
			return err
		}
	}
	return nil
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ErrLatestRefused rejects the host-local "latest" ref; callers pass an explicit snapshot id.
var ErrLatestRefused = errors.New("snapshot ref 'latest' is not accepted: pass an explicit snapshot id")

func checkSnapshotRef(snapshotID string) error {
	if snapshotID == "latest" {
		return ErrLatestRefused
	}
	return nil
}

// jailRoot is the per-dock jail staging dir; firecracker paths are relative to it.
func (h *Host) jailRoot(dockID string) string {
	return filepath.Join(h.cfg.JailBase, "firecracker", dockID, "root")
}

// backingID is the id the dock's runtime resources are named by.
func (h *Host) backingID(dockID string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if v, ok := h.vms[dockID]; ok && v.backing != "" {
		return v.backing
	}
	return dockID
}

func (h *Host) handle(dockID string) *fc.Handle {
	return &fc.Handle{DockID: dockID, JailRoot: h.jailRoot(dockID)}
}

// baseKernelArgs skips cloud metadata waits that stall boot 30-60s.
const baseKernelArgs = "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw rootfstype=ext4 rootwait tsc=reliable clocksource=kvm-clock cloud-init=disabled ds=nocloud"

// ErrImageUnavailable refuses an image this host does not serve.
var ErrImageUnavailable = errors.New("image_unavailable")

// ResolveImage defaults an empty image and rejects any but the one base image.
func ResolveImage(image string) (string, error) {
	switch image {
	case "":
		return apiv1.DefaultImage, nil
	case apiv1.DefaultImage:
		return image, nil
	}
	return "", fmt.Errorf("%w: %q", ErrImageUnavailable, image)
}

// Provision boots a dock, forking generation snapshotID when set. A
// tracked dock refuses double boot; an untracked leftover with the same id
// (crash or restart orphan) is fenced before its TAP is reused.
func (h *Host) Provision(ctx context.Context, dockID string, res apiv1.Resources, image, snapshotID string) (string, error) {
	if err := checkIDs(dockID, snapshotID); err != nil {
		return "", err
	}
	if _, err := ResolveImage(image); err != nil {
		return "", err
	}
	if err := fc.ValidateResources(res); err != nil {
		return "", err
	}
	h.mu.Lock()
	_, tracked := h.vms[dockID]
	h.mu.Unlock()
	if tracked {
		return "", fmt.Errorf("dock %s already provisioned", dockID)
	}
	release, err := h.reserve(dockID, res)
	if err != nil {
		return "", err
	}
	defer release()
	// Fence a leftover VM by its recorded pid, never by a process-name pattern.
	if !h.usesVeth() {
		fc.FenceStale(h.jailRoot(dockID), dockID, h.cfg.FirecrackerBin)
	}
	if err := checkSnapshotRef(snapshotID); err != nil {
		return "", err
	}
	resolved := snapshotID
	// The jailer builds the chroot device nodes itself and aborts if they
	// already exist: always start from a clean jail dir (the rootfs is
	// re-provisioned below; failures preserve it as .failed).
	_ = os.RemoveAll(h.jailRoot(dockID))
	rootfs := filepath.Join(h.jailRoot(dockID), fc.RootfsName)
	isContainer := h.usesVeth()
	var owner, ownerGID int
	if !isContainer {
		if owner, ownerGID, err = h.dockOwner(dockID); err != nil {
			return "", fmt.Errorf("provision uid: %w", err)
		}
		if resolved != "" {
			if err := h.cfg.Snaps.MaterializeDisk(ctx, resolved, rootfs); err != nil {
				return "", err
			}
		} else {
			if err := disk.ProvisionFromBase(h.cfg.NetRunner, h.cfg.ImageBase, rootfs); err != nil {
				return "", err
			}
		}
		if err := growDisk(rootfs, res.DiskGiB); err != nil {
			return "", err
		}
		// The guest runs dropped to UID/GID and needs the disk writable.
		if err := h.chown(rootfs, owner, ownerGID); err != nil {
			return "", err
		}
	} else {
		// Containers boot the OCI base image, not an ext4 file. A fork or
		// branch restores the source generation onto the volume before boot so
		// the dock starts with its files already in place.
		if resolved != "" {
			if err := h.restoreContainerState(ctx, dockID, res, resolved); err != nil {
				return "", err
			}
		}
		_ = rootfs
	}
	tap := h.ifaceFor(dockID)
	if err := h.ensureIface(tap, owner); err != nil {
		return "", fmt.Errorf("provision network: %w", err)
	}
	guestIP, gateway, mask, err := h.alloc.Assign(dockID)
	if err != nil {
		return "", fmt.Errorf("provision ip: %w", err)
	}
	// The TAP answers ARP for the gateway and NATs guest egress; both are
	// idempotent so repeat provisions and reboots never stack state.
	if err := net.EnsureGateway(h.cfg.NetRunner, tap, gateway, guestIP); err != nil {
		_ = h.alloc.Release(dockID)
		_ = h.deleteIface(tap)
		return "", fmt.Errorf("provision network: %w", err)
	}
	if err := net.EnsureNAT(h.cfg.NetRunner, h.cfg.Subnet, h.cfg.EgressAllowPrivate, h.cfg.EgressPort); err != nil {
		_ = h.alloc.Release(dockID)
		_ = h.deleteIface(tap)
		return "", fmt.Errorf("provision network: %w", err)
	}
	// Gate every boot, including forks: the disk may carry a previous
	// dock's network unit and machine identity.
	if !isContainer {
		if err := h.gateRootfs(rootfs, guestIP, gateway, mask, MacFor(dockID)); err != nil {
			_ = h.alloc.Release(dockID)
			_ = h.deleteIface(tap)
			return "", err
		}
	}
	h.mu.Lock()
	fast, isFast := h.fastSpecs[dockID]
	h.mu.Unlock()
	if isFast && !isContainer {
		if err := h.installFastAgent(rootfs, fast.token, fast.workspace); err != nil {
			h.mu.Lock()
			delete(h.fastSpecs, dockID)
			h.mu.Unlock()
			_ = h.alloc.Release(dockID)
			_ = h.deleteIface(tap)
			return "", fmt.Errorf("provision fast agent: %w", err)
		}
	}
	if isFast && isContainer {
		// Containers bake the agent; only the claim token config is
		// staged, mounted read-only for the supervised sidecar.
		if err := h.stageContainerFastAgent(dockID, fast.token, fast.workspace); err != nil {
			h.mu.Lock()
			delete(h.fastSpecs, dockID)
			h.mu.Unlock()
			_ = h.alloc.Release(dockID)
			_ = h.deleteIface(tap)
			return "", fmt.Errorf("provision fast agent: %w", err)
		}
	}
	spec := fc.BootSpec{
		DockID: dockID, Resources: res,
		KernelImage: h.cfg.KernelImage, InitrdImage: h.cfg.InitrdImage, JailRoot: h.jailRoot(dockID),
		Firecracker: h.cfg.FirecrackerBin, Jailer: h.cfg.JailerBin, JailBase: h.cfg.JailBase,
		UID: owner, GID: ownerGID, Limits: h.cfg.Limits.For(res),
		TapName: tap, GuestMAC: MacFor(dockID),
		KernelArgs: baseKernelArgs,
		TrackDirty: true,
	}
	if isContainer {
		spec.AttachNetwork = h.vethAttacher(tap, guestIP, gateway, mask)
	}
	bootHandle, err := h.cfg.Driver.Boot(ctx, spec)
	if err != nil {
		_ = h.alloc.Release(dockID)
		_ = h.deleteIface(tap)
		// Preserve the failed jail for diagnosis instead of deleting it;
		// the next provision for this dock replaces it. No pkill fence:
		// with a foreground jailer the SDK tracks the real process, so a
		// Boot error means nothing is running; fences only murder healthy
		// daemons orphaned by earlier failures (reconciler fences those).
		_ = os.RemoveAll(h.jailRoot(dockID) + ".failed")
		_ = os.Rename(h.jailRoot(dockID), h.jailRoot(dockID)+".failed")
		return "", fmt.Errorf("provision boot: %w", err)
	}
	_ = os.RemoveAll(h.jailRoot(dockID) + ".failed")
	// Enforce per-dock cgroup limits before tracking: a VM without quota
	// could starve its neighbor, so limit failures fail the provision.
	if err := h.enforceLimits(bootHandle.Pid, res); err != nil {
		_ = h.cfg.Driver.Terminate(bootHandle)
		_ = h.alloc.Release(dockID)
		_ = h.deleteIface(tap)
		return "", fmt.Errorf("provision cgroup: %w", err)
	}
	var egressGateway string
	if isFast && len(fast.egress) > 0 {
		if err := h.restrictEgress(guestIP, fast.egress); err != nil {
			_ = h.cfg.Driver.Terminate(bootHandle)
			_ = h.alloc.Release(dockID)
			_ = h.deleteIface(tap)
			return "", fmt.Errorf("provision egress: %w", err)
		}
		egressGateway = gateway
	}
	h.mu.Lock()
	live := &vm{res: res, tap: tap, guestIP: guestIP, pid: bootHandle.Pid, running: true, egressGateway: egressGateway}
	if isFast {
		live.fastToken = fast.token
		live.fastWorkspace = fast.workspace
		live.ephemeral = fast.ephemeral
		delete(h.fastSpecs, dockID)
		h.persistExecAgent(dockID, fast)
	}
	h.vms[dockID] = live
	h.mu.Unlock()
	return guestIP, nil
}

// enforceLimits resolves the jailer cgroup of a booted microVM and writes
// the dock's cpu and memory limits. Every boot path calls it: Provision and SnapshotLoad.
func (h *Host) enforceLimits(pid int, res apiv1.Resources) error {
	dir, err := h.cfg.Stats.CgroupDir(pid)
	if err != nil {
		return err
	}
	return fc.ApplyLimits(dir, res)
}

// stageContainerFastAgent records the pool claim token for the supervised
// container sidecar. Same config shape as the microVM unit.
func (h *Host) stageContainerFastAgent(dockID, token, workspace string) error {
	config, err := json.Marshal(map[string]any{
		"token":     token,
		"workspace": workspace,
		"roots":     []string{workspace, "/run/dock-fast"},
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(h.jailRoot(dockID), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(h.jailRoot(dockID), "fast-agent.json"), config, 0o644)
}

// gateRootfs records the static network for this dock inside the fresh
// rootfs. The image carries no network config of its own: the kernel ignores ip= (no CONFIG_IP_PNP),
// so a MAC-matched networkd unit is the only static addressing path.
func (h *Host) gateRootfs(rootfs string, guestIP, gateway, mask, mac string) error {
	dir, err := os.MkdirTemp("", "gate")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := h.cfg.Mount.Mount(rootfs, dir, false); err != nil {
		return err
	}
	if err := writeGateFiles(dir, guestIP, gateway, mask, mac); err != nil {
		_ = h.cfg.Mount.Unmount(dir)
		return err
	}
	return h.cfg.Mount.Unmount(dir)
}

// writeGateFiles records static networking into a mounted rootfs.
func writeGateFiles(dir string, guestIP, gateway, mask, mac string) error {
	g, err := disk.OpenGuest(dir)
	if err != nil {
		return err
	}
	defer g.Close()
	if err := g.MkdirAll("etc", 0o755); err != nil {
		return err
	}
	// A forked disk carries the source identity; fresh images carry the
	// build's. Either way the dock regenerates its own at boot.
	for _, stale := range []string{"etc/machine-id", "var/lib/dbus/machine-id"} {
		if err := g.Remove(stale); err != nil {
			return err
		}
	}
	unit := "[Match]\nMACAddress=" + mac + "\n\n[Network]\nAddress=" + guestIP + "/" + mask + "\nGateway=" + gateway + "\nDNS=1.1.1.1\nDHCP=no\n"
	if err := g.WriteFile("etc/systemd/network/10-dock.network", []byte(unit), 0o644); err != nil {
		return err
	}
	// networkd is not enabled by default in debootstrap images.
	return g.Symlink("/lib/systemd/system/systemd-networkd.service", "etc/systemd/system/multi-user.target.wants/systemd-networkd.service")
}

// Pause freezes a running microVM.
func (h *Host) Pause(ctx context.Context, dockID string) error {
	if err := checkIDs(dockID); err != nil {
		return err
	}
	return h.cfg.Driver.Pause(ctx, h.handle(dockID))
}

// SnapshotCreate captures a generation: the frozen rootfs plus an optional
// memory image. ResumeAfter continues a paused VM; a full capture without it
// is terminal (archived docks free host CPU/RAM, resume fresh-boots).
// The gvisor runtime captures disk-only generations via export (see 4.1).
func (h *Host) SnapshotCreate(ctx context.Context, dockID string, parentID *string, kind apiv1.SnapshotKind, resumeAfter, memory bool) (snapshotID string, sizeBytes int64, cpuFamily string, err error) {
	parent := ""
	if parentID != nil {
		parent = *parentID
	}
	if err := checkIDs(dockID, parent); err != nil {
		return "", 0, "", err
	}
	snapshotID = newID()
	if h.diskOnly() {
		return h.snapshotContainer(ctx, dockID, parentID, kind, resumeAfter, snapshotID)
	}
	if err := h.cfg.Driver.Pause(ctx, h.handle(dockID)); err != nil {
		// The VM is gone (crash, restart race) but its disk may remain: a
		// full capture degrades to a disk-only freeze instead of failing
		// the dock. Anything else still fails fast.
		if kind == apiv1.SnapshotFull && errors.Is(err, fc.ErrNoMicroVM) {
			return h.freezeOnly(ctx, dockID, parentID, snapshotID)
		}
		return "", 0, "", err
	}
	// A non-terminal capture (resumeAfter) must never leave the dock frozen on failure;
	// a terminal capture is expected to stop the guest, so its errors leave it for the caller.
	captured := false
	if resumeAfter {
		defer func() {
			if !captured {
				_ = h.cfg.Driver.Resume(context.WithoutCancel(ctx), h.handle(dockID))
			}
		}()
	}
	if memory && !h.diskOnly() {
		if err := h.cfg.Snaps.CaptureMemory(ctx, h.handle(dockID), snapshotID, "Full"); err != nil {
			return "", 0, "", err
		}
	}
	manifest, err := h.cfg.Snaps.FreezeRootfs(ctx, dockID, filepath.Join(h.jailRoot(dockID), fc.RootfsName), snapshotID, string(kind), h.cfg.Family, parentID)
	if err != nil {
		return "", 0, "", err
	}
	if resumeAfter {
		if err := h.cfg.Driver.Resume(ctx, h.handle(dockID)); err != nil {
			return "", 0, "", err
		}
		captured = true
	} else if kind == apiv1.SnapshotFull {
		// Terminal capture: the archived dock keeps its files but frees
		// host CPU and RAM; resume boots fresh from the generation.
		_ = h.cfg.Driver.Terminate(h.handle(dockID))
		cleanupCgroup(h.cfg.CgroupBase, dockID)
		markStopped(h, dockID)
	}
	var total int64
	for _, f := range manifest.Files {
		total += f.Size
	}
	return snapshotID, total, manifest.CPUFamily, nil
}

// snapshotContainer captures a dock's state as a tar of its workspace volume.
//
// The workspace is the dock's durable unit: it is the working directory every
// agent writes to, and it is a host directory, so a capture is a single
// host-side tar instead of a copy of the whole container filesystem. That is
// what keeps a capture in the seconds range rather than minutes.
func (h *Host) snapshotContainer(ctx context.Context, dockID string, parentID *string, kind apiv1.SnapshotKind, resumeAfter bool, snapshotID string) (string, int64, string, error) {
	state, ok := h.cfg.Driver.(interface {
		WorkDir(dockID string) (string, error)
		SaveState(dir, dest string) error
	})
	if !ok {
		return "", 0, "", fmt.Errorf("container driver lacks state capture")
	}
	dir, err := state.WorkDir(dockID)
	if err != nil {
		return "", 0, "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, "", err
	}
	tmp, err := os.MkdirTemp("", "containergen")
	if err != nil {
		return "", 0, "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	stateTar := filepath.Join(tmp, "state.tar")
	if err := state.SaveState(dir, stateTar); err != nil {
		return "", 0, "", err
	}
	manifest, err := h.cfg.Snaps.FreezeContainerState(ctx, dockID, stateTar, snapshotID, string(kind), h.cfg.Family, parentID)
	if err != nil {
		return "", 0, "", err
	}
	if !resumeAfter && kind == apiv1.SnapshotFull {
		_ = h.cfg.Driver.Terminate(h.handle(dockID))
		cleanupCgroup(h.cfg.CgroupBase, dockID)
		markStopped(h, dockID)
	}
	var total int64
	for _, f := range manifest.Files {
		total += f.Size
	}
	return snapshotID, total, manifest.CPUFamily, nil
}

// restoreContainerState writes a generation's workspace onto the dock's work disk.
// It runs before boot so the dock never starts without its files.
func (h *Host) restoreContainerState(ctx context.Context, dockID string, res apiv1.Resources, resolved string) error {
	state, ok := h.cfg.Driver.(interface {
		EnsureWorkDisk(dockID string, diskGiB int) (string, error)
		RestoreState(dir, tarPath string) error
	})
	if !ok {
		return fmt.Errorf("container driver lacks state restore")
	}
	if _, err := h.cfg.Snaps.PrepareResume(ctx, resolved); err != nil {
		return err
	}
	dir, err := state.EnsureWorkDisk(dockID, res.DiskGiB)
	if err != nil {
		return err
	}
	if err := state.RestoreState(dir, h.cfg.Snaps.GenerationPath(resolved, "state.tar")); err != nil {
		return fmt.Errorf("restore state: %w", err)
	}
	return nil
}

// appendTarEntry rewrites a tar with one extra file entry appended. The delta
// is small by construction, so rewriting beats carrying a second artifact.
func appendTarEntry(tarPath, name string, content []byte) error {
	src, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer src.Close()
	rewritten := tarPath + ".tmp"
	out, err := os.Create(rewritten)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(out)
	tr := tar.NewReader(src)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = tw.Close()
			_ = out.Close()
			return err
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = tw.Close()
			_ = out.Close()
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			_ = tw.Close()
			_ = out.Close()
			return err
		}
	}
	hdr := &tar.Header{
		Name: strings.TrimPrefix(name, "/"),
		Mode: 0o644,
		Size: int64(len(content)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		_ = tw.Close()
		_ = out.Close()
		return err
	}
	if _, err := tw.Write(content); err != nil {
		_ = tw.Close()
		_ = out.Close()
		return err
	}
	if err := tw.Close(); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(rewritten, tarPath)
}

func (h *Host) freezeOnly(ctx context.Context, dockID string, parentID *string, snapshotID string) (string, int64, string, error) {
	rootfs := filepath.Join(h.jailRoot(dockID), fc.RootfsName)
	if _, err := os.Stat(rootfs); err != nil {
		return "", 0, "", err
	}
	manifest, err := h.cfg.Snaps.FreezeRootfs(ctx, dockID, rootfs, snapshotID, string(apiv1.SnapshotFull), h.cfg.Family, parentID)
	if err != nil {
		return "", 0, "", err
	}
	var total int64
	for _, f := range manifest.Files {
		total += f.Size
	}
	return snapshotID, total, manifest.CPUFamily, nil
}

// GenerationBase names the base image a microvm generation needs to restore, so
// the control plane can place its resumes only on hosts holding that base.
func (h *Host) GenerationBase(ctx context.Context, snapshotID string) string {
	if h.diskOnly() || h.cfg.Snaps.BaseImage == "" {
		return ""
	}
	return h.cfg.Snaps.GenerationBase(ctx, snapshotID)
}

// SnapshotLoad boots a dock from a generation with family enforcement.
func (h *Host) SnapshotLoad(ctx context.Context, dockID string, res apiv1.Resources, snapshotID string) (string, error) {
	if err := checkIDs(dockID, snapshotID); err != nil {
		return "", err
	}
	if err := fc.ValidateResources(res); err != nil {
		return "", err
	}
	if err := checkSnapshotRef(snapshotID); err != nil {
		return "", err
	}
	if snapshotID == "" {
		return "", errors.New("snapshot load requires an explicit snapshot id")
	}
	release, err := h.reserve(dockID, res)
	if err != nil {
		return "", err
	}
	defer release()
	resolved := snapshotID
	_ = os.RemoveAll(h.jailRoot(dockID))
	if h.usesVeth() {
		return h.snapshotLoadContainer(ctx, dockID, res, resolved)
	}
	rootfs := filepath.Join(h.jailRoot(dockID), fc.RootfsName)
	owner, ownerGID, err := h.dockOwner(dockID)
	if err != nil {
		return "", fmt.Errorf("load uid: %w", err)
	}
	if err := h.cfg.Snaps.MaterializeDisk(ctx, resolved, rootfs); err != nil {
		return "", err
	}
	if err := growDisk(rootfs, res.DiskGiB); err != nil {
		return "", err
	}
	if err := h.chown(rootfs, owner, ownerGID); err != nil {
		return "", err
	}
	tap := h.ifaceFor(dockID)
	if err := h.ensureIface(tap, owner); err != nil {
		return "", fmt.Errorf("load network: %w", err)
	}
	guestIP, gateway, mask, err := h.alloc.Assign(dockID)
	if err != nil {
		return "", fmt.Errorf("load ip: %w", err)
	}
	// Gateway and NAT persist per boot, not per host: a load after a host
	// reboot rebuilds the same first hop as a fresh provision.
	if err := net.EnsureGateway(h.cfg.NetRunner, tap, gateway, guestIP); err != nil {
		_ = h.alloc.Release(dockID)
		_ = h.deleteIface(tap)
		return "", fmt.Errorf("load network: %w", err)
	}
	if err := net.EnsureNAT(h.cfg.NetRunner, h.cfg.Subnet, h.cfg.EgressAllowPrivate, h.cfg.EgressPort); err != nil {
		_ = h.alloc.Release(dockID)
		_ = h.deleteIface(tap)
		return "", fmt.Errorf("load network: %w", err)
	}
	// Gating is per boot: the frozen disk may carry a previous dock's network
	// unit and machine identity.
	if err := h.gateRootfs(rootfs, guestIP, gateway, mask, MacFor(dockID)); err != nil {
		_ = h.alloc.Release(dockID)
		_ = h.deleteIface(tap)
		return "", err
	}
	h.mu.Lock()
	fast, isFast := h.fastSpecs[dockID]
	h.mu.Unlock()
	if isFast {
		if err := h.installFastAgent(rootfs, fast.token, fast.workspace); err != nil {
			_ = h.alloc.Release(dockID)
			_ = h.deleteIface(tap)
			return "", fmt.Errorf("load fast agent: %w", err)
		}
	}
	bootHandle, err := h.cfg.Driver.Boot(ctx, fc.BootSpec{
		DockID: dockID, Resources: res,
		KernelImage: h.cfg.KernelImage, InitrdImage: h.cfg.InitrdImage, JailRoot: h.jailRoot(dockID),
		Firecracker: h.cfg.FirecrackerBin, Jailer: h.cfg.JailerBin, JailBase: h.cfg.JailBase,
		UID: owner, GID: ownerGID, Limits: h.cfg.Limits.For(res),
		TapName: tap, GuestMAC: MacFor(dockID),
		KernelArgs: baseKernelArgs + " clock_realtime=true",
		TrackDirty: true,
	})
	if err != nil {
		_ = h.alloc.Release(dockID)
		_ = h.deleteIface(tap)
		_ = os.RemoveAll(h.jailRoot(dockID) + ".failed")
		_ = os.Rename(h.jailRoot(dockID), h.jailRoot(dockID)+".failed")
		return "", fmt.Errorf("load boot: %w", err)
	}
	if err := h.enforceLimits(bootHandle.Pid, res); err != nil {
		_ = h.cfg.Driver.Terminate(bootHandle)
		_ = h.alloc.Release(dockID)
		_ = h.deleteIface(tap)
		return "", fmt.Errorf("load cgroup: %w", err)
	}
	h.mu.Lock()
	live := &vm{res: res, tap: tap, guestIP: guestIP, pid: bootHandle.Pid, running: true}
	if isFast {
		live.fastToken = fast.token
		live.fastWorkspace = fast.workspace
		live.ephemeral = fast.ephemeral
		delete(h.fastSpecs, dockID)
		h.persistExecAgent(dockID, fast)
	}
	h.vms[dockID] = live
	h.mu.Unlock()
	return guestIP, nil
}

// snapshotLoadContainer boots a gvisor container from a generation: the base OCI
// image plus the recorded workspace state. Family is enforced via PrepareResume
// before anything starts, and reboot semantics hold (files return; running
// processes do not).
func (h *Host) snapshotLoadContainer(ctx context.Context, dockID string, res apiv1.Resources, resolved string) (string, error) {
	if _, err := h.cfg.Snaps.PrepareResume(ctx, resolved); err != nil {
		return "", err
	}
	// A previous container may still exist (crash, or a stop whose removal did
	// not complete) and would keep the dock's old files; Terminate is
	// idempotent when absent.
	_ = h.cfg.Driver.Terminate(h.handle(dockID))
	if err := h.restoreContainerState(ctx, dockID, res, resolved); err != nil {
		return "", err
	}
	h.mu.Lock()
	fast, isFast := h.fastSpecs[dockID]
	h.mu.Unlock()
	if isFast {
		if err := h.stageContainerFastAgent(dockID, fast.token, fast.workspace); err != nil {
			return "", fmt.Errorf("load fast agent: %w", err)
		}
	}
	tap := h.ifaceFor(dockID)
	if err := h.ensureIface(tap, 0); err != nil {
		return "", fmt.Errorf("load network: %w", err)
	}
	guestIP, gateway, mask, err := h.alloc.Assign(dockID)
	if err != nil {
		return "", fmt.Errorf("load ip: %w", err)
	}
	if err := net.EnsureGateway(h.cfg.NetRunner, tap, gateway, guestIP); err != nil {
		_ = h.alloc.Release(dockID)
		_ = h.deleteIface(tap)
		return "", fmt.Errorf("load network: %w", err)
	}
	if err := net.EnsureNAT(h.cfg.NetRunner, h.cfg.Subnet, h.cfg.EgressAllowPrivate, h.cfg.EgressPort); err != nil {
		_ = h.alloc.Release(dockID)
		_ = h.deleteIface(tap)
		return "", fmt.Errorf("load network: %w", err)
	}
	bootHandle, err := h.cfg.Driver.Boot(ctx, fc.BootSpec{
		DockID: dockID, Resources: res, JailRoot: h.jailRoot(dockID), Limits: h.cfg.Limits.For(res),
		AttachNetwork: h.vethAttacher(tap, guestIP, gateway, mask),
	})
	if err != nil {
		_ = h.alloc.Release(dockID)
		_ = h.deleteIface(tap)
		_ = os.RemoveAll(h.jailRoot(dockID) + ".failed")
		_ = os.Rename(h.jailRoot(dockID), h.jailRoot(dockID)+".failed")
		return "", fmt.Errorf("load boot: %w", err)
	}
	if err := h.enforceLimits(bootHandle.Pid, res); err != nil {
		_ = h.cfg.Driver.Terminate(bootHandle)
		_ = h.alloc.Release(dockID)
		_ = h.deleteIface(tap)
		return "", fmt.Errorf("load cgroup: %w", err)
	}
	h.mu.Lock()
	live := &vm{res: res, tap: tap, guestIP: guestIP, pid: bootHandle.Pid, running: true}
	if isFast {
		live.fastToken = fast.token
		live.fastWorkspace = fast.workspace
		live.ephemeral = fast.ephemeral
		delete(h.fastSpecs, dockID)
		h.persistExecAgent(dockID, fast)
	}
	h.vms[dockID] = live
	h.mu.Unlock()
	return guestIP, nil
}

// execAgentSidecar holds an agent dock's token beside its jail so Rescan can re-attach exec.
const execAgentSidecar = "exec-agent.json"

// persistExecAgent is best effort: without it exec only stops surviving docklet restarts.
func (h *Host) persistExecAgent(dockID string, spec fastSpec) {
	if spec.ephemeral {
		return
	}
	raw, err := json.Marshal(map[string]string{"token": spec.token, "workspace": spec.workspace})
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(h.jailRoot(dockID), execAgentSidecar), raw, 0o600)
}

// readExecAgent returns empty values for docks booted without the agent.
func readExecAgent(jail string) (string, string) {
	raw, err := os.ReadFile(filepath.Join(jail, execAgentSidecar))
	if err != nil {
		return "", ""
	}
	var spec struct {
		Token     string `json:"token"`
		Workspace string `json:"workspace"`
	}
	if json.Unmarshal(raw, &spec) != nil {
		return "", ""
	}
	return spec.Token, spec.Workspace
}

// Destroy shuts the microVM down and frees its IP and slot.
func (h *Host) Destroy(ctx context.Context, dockID string) error {
	if err := checkIDs(dockID); err != nil {
		return err
	}
	// A claimed pool dock runs under its slot id: that names the container, jail and work disk.
	backing := h.backingID(dockID)
	_ = h.cfg.Driver.Terminate(h.handle(backing))
	cleanupCgroup(h.cfg.CgroupBase, backing)
	h.releaseEgress(dockID)
	_ = h.alloc.Release(dockID)
	// Claimed pool VMs keep their slot TAP, so delete the tracked one.
	tap := h.ifaceFor(backing)
	h.mu.Lock()
	if v, ok := h.vms[dockID]; ok && v.tap != "" {
		tap = v.tap
	}
	delete(h.vms, dockID)
	h.mu.Unlock()
	_ = h.deleteIface(tap)
	_ = os.RemoveAll(h.jailRoot(backing))
	// A failed boot's preserved jail holds a disk owned by this dock's uid, which is about to be reused.
	_ = os.RemoveAll(h.jailRoot(backing) + ".failed")
	_ = os.RemoveAll(filepath.Join(h.cfg.VMsDir, backing))
	h.releaseOwner(backing)
	// A gvisor dock owns a loop-mounted work disk that must be unmounted before it is removed.
	if w, ok := h.cfg.Driver.(interface{ RemoveWorkDisk(dockID string) error }); ok {
		return w.RemoveWorkDisk(backing)
	}
	return nil
}

// EgressGateway is the guest-facing address the egress proxy binds to.
func (h *Host) EgressGateway() string {
	return h.alloc.Gateway()
}

// restrictEgress confines a guest IP to its allowlist; fails closed without a proxy.
func (h *Host) restrictEgress(guestIP string, allow []string) error {
	if h.cfg.Egress == nil || h.cfg.EgressPort == 0 {
		return errors.New("egress proxy not configured")
	}
	if err := egress.Restrict(h.cfg.NetRunner, guestIP, h.cfg.EgressPort); err != nil {
		egress.Unrestrict(h.cfg.NetRunner, guestIP, h.cfg.EgressPort)
		return err
	}
	h.cfg.Egress.Allow(guestIP, allow)
	return nil
}

// releaseEgress always clears rules for the dock's IP, even when the policy
// predates a docklet restart, so a reused IP never inherits a confinement.
func (h *Host) releaseEgress(dockID string) {
	if h.cfg.Egress == nil || h.cfg.EgressPort == 0 {
		return
	}
	h.mu.Lock()
	ip := ""
	if v, ok := h.vms[dockID]; ok {
		ip = v.guestIP
	}
	h.mu.Unlock()
	if ip == "" {
		ip = h.alloc.ByDock()[dockID]
	}
	if ip == "" {
		return
	}
	egress.Unrestrict(h.cfg.NetRunner, ip, h.cfg.EgressPort)
	h.cfg.Egress.Revoke(ip)
}

// cleanupCgroup removes the jailer cgroup after its processes are gone.
// Best effort: a live cgroup refuses removal and stays for the reconciler.
func cleanupCgroup(base, dockID string) {
	if base == "" {
		return
	}
	_ = os.RemoveAll(filepath.Join(base, "firecracker", dockID))
}

// markStopped drops a dock from the running set; its files stay for resume.
func markStopped(h *Host, dockID string) {
	h.releaseEgress(dockID)
	h.mu.Lock()
	delete(h.vms, dockID)
	h.mu.Unlock()
}

// Running lists the tracked microVM inventory for heartbeat/status.
func (h *Host) Running() []apiv1.RunningVM {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]apiv1.RunningVM, 0, len(h.vms))
	for dockID, v := range h.vms {
		state := "running"
		if !v.running {
			state = "paused"
		}
		kind := "dock"
		if v.ephemeral {
			kind = "fast"
		}
		out = append(out, apiv1.RunningVM{DockID: dockID, State: state, Kind: kind})
	}
	return out
}

// Stats samples host-truth counters for one tracked dock, resolving the
// jailer cgroup through the live microVM process.
func (h *Host) Stats(_ context.Context, dockID string) (apiv1.StatsSample, error) {
	h.mu.Lock()
	v, ok := h.vms[dockID]
	h.mu.Unlock()
	if !ok {
		return apiv1.StatsSample{}, fmt.Errorf("no tracked microVM for dock %s", dockID)
	}
	dir, err := h.cfg.Stats.CgroupDir(v.pid)
	if err != nil {
		return apiv1.StatsSample{}, err
	}
	h.mu.Lock()
	prev := v.cpu
	h.mu.Unlock()
	sample, mark, err := h.cfg.Stats.Sample(dockID, dir, v.tap, prev, time.Now())
	if err != nil {
		return sample, err
	}
	h.mu.Lock()
	v.cpu = mark
	h.mu.Unlock()
	sample.DiskUsedBytes = h.diskUsed(dockID, v)
	return sample, nil
}

// diskUsed reports host blocks held by the dock's writable disk: the sparse
// rootfs image for microVMs, the workspace volume for containers. A failed
// walk keeps the last good value rather than reporting an empty disk.
func (h *Host) diskUsed(dockID string, v *vm) int64 {
	h.mu.Lock()
	last := v.disk
	h.mu.Unlock()
	if !last.at.IsZero() && time.Since(last.at) < diskUsageTTL {
		return last.bytes
	}
	backing := h.backingID(dockID)
	path := filepath.Join(h.jailRoot(backing), fc.RootfsName)
	if h.usesVeth() {
		// Never WorkDir here: it mounts, and a sample racing a stop would remount the disk.
		vols, ok := h.cfg.Driver.(interface {
			WorkDiskPath(dockID string) (string, error)
		})
		if !ok {
			return last.bytes
		}
		dir, err := vols.WorkDiskPath(backing)
		if err != nil {
			return last.bytes
		}
		path = dir
	}
	used, err := stats.Allocated(path)
	if err != nil {
		return last.bytes
	}
	h.mu.Lock()
	v.disk = diskMark{bytes: used, at: time.Now()}
	h.mu.Unlock()
	return used
}
