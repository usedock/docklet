package fc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/operations"

	apiv1 "github.com/usedock/docklet/api/v1"
)

var testRes = apiv1.Resources{CPUs: 2, MemoryMiB: 4096, DiskGiB: 20}

func baseSpec() BootSpec {
	return BootSpec{
		DockID: "dock-1", Resources: testRes,
		KernelImage: "/images/vmlinux", InitrdImage: "/images/initrd.img", JailRoot: "/srv/jailer/firecracker/dock-1/root",
		Firecracker: "/usr/bin/firecracker", Jailer: "jailer", JailBase: "/srv/jailer",
		UID: os.Getuid(), GID: os.Getgid(), TapName: "tap-d1", GuestMAC: "AA:FC:00:00:00:01",
		KernelArgs: "console=ttyS0", TrackDirty: true,
	}
}

func TestMachineConfig(t *testing.T) {
	cfg, err := MachineConfig(baseSpec())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SocketPath != SocketName {
		t.Fatalf("socket = %s", cfg.SocketPath)
	}
	if cfg.KernelImagePath != KernelName || len(cfg.Drives) != 1 || cfg.Drives[0].PathOnHost == nil || *cfg.Drives[0].PathOnHost != RootfsName {
		t.Fatalf("jail-relative paths = %+v", cfg)
	}
	if cfg.MachineCfg.VcpuCount == nil || *cfg.MachineCfg.VcpuCount != 2 {
		t.Fatalf("vcpu = %+v, want 2", cfg.MachineCfg.VcpuCount)
	}
	if cfg.MachineCfg.MemSizeMib == nil || *cfg.MachineCfg.MemSizeMib != 4096 {
		t.Fatalf("mem = %+v, want 4096", cfg.MachineCfg.MemSizeMib)
	}
	if !cfg.MachineCfg.TrackDirtyPages {
		t.Fatal("dirty tracking must be on for diff snapshots")
	}
	if cfg.JailerCfg == nil || cfg.JailerCfg.CgroupVersion != "2" {
		t.Fatalf("jailer = %+v", cfg.JailerCfg)
	}
	if cfg.JailerCfg.Daemonize {
		t.Fatal("jailer must stay foreground so the SDK tracks the real process")
	}
	if cfg.ForwardSignals == nil {
		t.Fatal("signal forwarding must be explicitly empty so restarts never TERM guests")
	}
	if cfg.Snapshot.MemFilePath != "" {
		t.Fatal("fresh boot must not carry a snapshot")
	}
	if cfg.InitrdPath != InitrdName {
		t.Fatalf("initrd = %s", cfg.InitrdPath)
	}
}

func TestMachineConfigKernelOnlyBoot(t *testing.T) {
	spec := baseSpec()
	spec.InitrdImage = ""
	cfg, err := MachineConfig(spec)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InitrdPath != "" {
		t.Fatalf("initrd = %s, want empty without staged image", cfg.InitrdPath)
	}
}

func TestMachineConfigInvalidResources(t *testing.T) {
	for _, res := range []apiv1.Resources{{}, {CPUs: 2, MemoryMiB: 4096}, {CPUs: 0, MemoryMiB: 4096, DiskGiB: 20}, {CPUs: 2, MemoryMiB: 64, DiskGiB: 20}} {
		spec := baseSpec()
		spec.Resources = res
		if _, err := MachineConfig(spec); !errors.Is(err, ErrInvalidResources) {
			t.Fatalf("resources %+v: err = %v, want ErrInvalidResources", res, err)
		}
	}
}

func TestApplyLimitsWritesCgroupFiles(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "vm")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ApplyLimits(dir, apiv1.Resources{CPUs: 3, MemoryMiB: 1024, DiskGiB: 10}); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{"cpu.max": "300000 100000\n", "memory.max": "1073741824\n"} {
		raw, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil || string(raw) != want {
			t.Fatalf("%s = %q, %v; want %q", file, raw, err, want)
		}
	}
	if err := ApplyLimits(dir, apiv1.Resources{}); !errors.Is(err, ErrInvalidResources) {
		t.Fatalf("zero resources must fail closed, got %v", err)
	}
}

func TestSocketWaitInsertedAfterStart(t *testing.T) {
	list := firecracker.HandlerList{}.Append(firecracker.StartVMMHandler)
	handlers := &firecracker.Handlers{FcInit: list}
	err := SocketWaitStrategy{}.AdaptHandlers(handlers)
	if err != nil {
		t.Fatal(err)
	}
	if !handlers.FcInit.Has(WaitSocketHandlerName) {
		t.Fatal("wait handler missing after adapt")
	}
	bare := &firecracker.Handlers{}
	berr := SocketWaitStrategy{}.AdaptHandlers(bare)
	if berr == nil {
		t.Fatal("missing StartVMM must fail loudly")
	}
}

func TestLinkResolvesSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.img")
	if err := os.WriteFile(real, []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	pointer := filepath.Join(dir, "pointer.img")
	if err := os.Symlink(real, pointer); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "jail", "vmlinux")
	if err := link(pointer, dst); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(dst)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("staged file must be a regular file: %+v %v", info, err)
	}
	raw, _ := os.ReadFile(dst)
	if string(raw) != "kernel" {
		t.Fatalf("content = %q", raw)
	}
}

func TestAdoptBuildsDialableHandle(t *testing.T) {
	d := NewSDKDriver()
	h, err := d.Adopt("dock-9", t.TempDir(), testRes)
	if err != nil {
		t.Fatal(err)
	}
	if h.DockID != "dock-9" || h.JailRoot == "" || h.SocketPath == "" {
		t.Fatalf("handle = %+v", h)
	}
	if _, err := d.Adopt("dock-9", t.TempDir(), apiv1.Resources{}); err == nil {
		t.Fatal("invalid resources must fail")
	}
}

func TestAdoptKeepsPid(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "firecracker.pid"), []byte("4242\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := NewSDKDriver().Adopt("dock-9", dir, testRes)
	if err != nil {
		t.Fatal(err)
	}
	if h.Pid != 4242 {
		t.Fatalf("pid = %d, want 4242 from the pid file", h.Pid)
	}
	h, err = NewSDKDriver().Adopt("dock-9", t.TempDir(), testRes)
	if err != nil {
		t.Fatal(err)
	}
	if h.Pid != 0 {
		t.Fatalf("pid = %d, want 0 without a pid file", h.Pid)
	}
}
func TestTerminateMissingPidIsGone(t *testing.T) {
	d := NewSDKDriver()
	if err := d.Terminate(&Handle{DockID: "ghost", JailRoot: t.TempDir()}); err != nil {
		t.Fatalf("missing pid file must read as gone: %v", err)
	}
}

func TestSidecarRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := apiv1.Resources{CPUs: 4, MemoryMiB: 8192, DiskGiB: 40}
	if err := WriteSidecar(dir, "dock-7", want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSidecar(dir)
	if err != nil || got != want {
		t.Fatalf("resources = %+v %v, want %+v", got, err, want)
	}
	if _, err := ReadSidecar(t.TempDir()); err == nil {
		t.Fatal("missing sidecar must fail")
	}
}

func TestSnapshotTypeOpt(t *testing.T) {
	params := operations.NewCreateSnapshotParamsWithContext(context.Background())
	SnapshotTypeOpt("Diff")(params)
	if params.Body == nil || params.Body.SnapshotType != "Diff" {
		t.Fatalf("body = %+v", params.Body)
	}
}

func TestRegistryMiss(t *testing.T) {
	d := NewSDKDriver()
	err := d.Pause(context.Background(), &Handle{DockID: "ghost"})
	if err == nil {
		t.Fatal("pause on unknown dock must fail")
	}
}
