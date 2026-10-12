package local

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func spec() LaunchSpec {
	return LaunchSpec{Kernel: "/k/vmlinuz", Initrd: "/k/initrd.img", Disk: "/d/disk.qcow2", SerialLog: "/d/serial.log",
		HostPort: 41234, ConfigFile: "/d/guest.json", Token: "tok", CPUs: 4, MemoryMiB: 3072, Network: "on"}
}

func argAfter(t *testing.T, args []string, flag string) string {
	t.Helper()
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("%s missing in %v", flag, args)
	}
	return args[i+1]
}

func TestArgvPerAccelerator(t *testing.T) {
	cases := []struct {
		accel, wantAccel, wantCPU string
	}{
		{"kvm", "kvm", "host"},
		{"hvf", "hvf", "host"},
		{"whpx", "whpx,kernel-irqchip=off", "qemu64"},
		{"tcg", "tcg", "max"},
	}
	for _, c := range cases {
		args := BuildArgv(spec(), "amd64", c.accel)
		if got := argAfter(t, args, "-accel"); got != c.wantAccel {
			t.Errorf("%s: -accel %q, want %q", c.accel, got, c.wantAccel)
		}
		if got := argAfter(t, args, "-cpu"); got != c.wantCPU {
			t.Errorf("%s: -cpu %q, want %q", c.accel, got, c.wantCPU)
		}
		if argAfter(t, args, "-machine") != "q35" || argAfter(t, args, "-smp") != "4" || argAfter(t, args, "-m") != "3072" {
			t.Errorf("%s: machine/smp/m wrong in %v", c.accel, args)
		}
	}
}

func TestArgvBootNetworkAndConsole(t *testing.T) {
	args := BuildArgv(spec(), "amd64", "kvm")
	if got := argAfter(t, args, "-kernel"); got != "/k/vmlinuz" {
		t.Fatalf("kernel %q", got)
	}
	if got := argAfter(t, args, "-initrd"); got != "/k/initrd.img" {
		t.Fatalf("initrd %q", got)
	}
	if got := argAfter(t, args, "-drive"); got != "file=/d/disk.qcow2,if=virtio,format=qcow2" {
		t.Fatalf("drive %q", got)
	}
	if got := argAfter(t, args, "-netdev"); got != "user,id=n0,ipv6=off,hostfwd=tcp:127.0.0.1:41234-:2222" {
		t.Fatalf("netdev %q", got)
	}
	if got := argAfter(t, args, "-serial"); got != "file:/d/serial.log" {
		t.Fatalf("serial %q", got)
	}
	if argAfter(t, args, "-display") != "none" {
		t.Fatal("no display")
	}
	if got := argAfter(t, args, "-append"); got != "console=ttyS0 root=/dev/vda rw rootfstype=ext4 rootwait" {
		t.Fatalf("cmdline %q must carry no config", got)
	}
	if got := argAfter(t, args, "-fw_cfg"); got != "name=opt/usedock/cfg,file=/d/guest.json" {
		t.Fatalf("fw_cfg %q", got)
	}
}

func TestNoSecretInAnyArgv(t *testing.T) {
	s := spec()
	s.Token = "SECRET-TOKEN-VALUE"
	for _, goarch := range []string{"amd64", "arm64"} {
		for _, accel := range []string{"kvm", "hvf", "whpx", "tcg"} {
			for _, network := range []string{"on", "off"} {
				s.Network = network
				joined := strings.Join(BuildArgv(s, goarch, accel), " ")
				if strings.Contains(joined, "SECRET") || strings.Contains(joined, "dock.cfg") {
					t.Fatalf("%s/%s/%s: the token must not reach the argv: %s", goarch, accel, network, joined)
				}
				if !strings.Contains(joined, "-fw_cfg name=opt/usedock/cfg,file=/d/guest.json") {
					t.Fatalf("%s/%s/%s: guest config must go through fw_cfg: %s", goarch, accel, network, joined)
				}
			}
		}
	}
}

func TestWriteGuestConfigIsPlainJSONForTheOwnerOnly(t *testing.T) {
	p := filepath.Join(t.TempDir(), "guest.json")
	if err := WriteGuestConfig(p, "tok", "my-dock", GuestOptions{}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	var cfg GuestConfig
	if err := json.Unmarshal(raw, &cfg); err != nil || cfg.Token != "tok" || cfg.Hostname != "my-dock" || cfg.Workspace != "/home/user/work" || len(cfg.Roots) != 2 {
		t.Fatalf("guest config %q %v", raw, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", st.Mode().Perm())
		}
	}
}

func TestArgvEscapesCommasInConfigPath(t *testing.T) {
	s := spec()
	s.ConfigFile = `C:\Users\a,b\guest.json`
	if got := argAfter(t, BuildArgv(s, "amd64", "whpx"), "-fw_cfg"); got != `name=opt/usedock/cfg,file=C:\Users\a,,b\guest.json` {
		t.Fatalf("fw_cfg %q", got)
	}
}

func TestArgvNetworkAlwaysDisablesIPv6(t *testing.T) {
	for _, network := range []string{"on", "off", ""} {
		s := spec()
		s.Network = network
		got := argAfter(t, BuildArgv(s, "amd64", "kvm"), "-netdev")
		if !strings.HasPrefix(got, "user,id=n0,ipv6=off,") {
			t.Fatalf("network %q: netdev %q must disable IPv6", network, got)
		}
	}
}

func TestArgvNetworkOffRestricts(t *testing.T) {
	s := spec()
	s.Network = "off"
	if got := argAfter(t, BuildArgv(s, "amd64", "kvm"), "-netdev"); !strings.HasSuffix(got, ",restrict=on") || !strings.Contains(got, "hostfwd=tcp:127.0.0.1:41234-:2222") {
		t.Fatalf("netdev %q must restrict but keep the agent forward", got)
	}
}

func TestArgvArm64UsesVirtAndAMAConsole(t *testing.T) {
	args := BuildArgv(spec(), "arm64", "hvf")
	if argAfter(t, args, "-machine") != "virt" || !strings.Contains(argAfter(t, args, "-append"), "console=ttyAMA0") {
		t.Fatalf("args %v", args)
	}
}

func TestArgvEscapesCommasInDiskPath(t *testing.T) {
	s := spec()
	s.Disk = `C:\Users\a,b\disk.qcow2`
	if got := argAfter(t, BuildArgv(s, "amd64", "whpx"), "-drive"); !strings.HasPrefix(got, `file=C:\Users\a,,b\disk.qcow2,`) {
		t.Fatalf("drive %q", got)
	}
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestAcceleratorPerOS(t *testing.T) {
	okKVM := func() error { return nil }
	noKVM := func() error { return errors.New("permission denied") }
	for goos, want := range map[string]string{"linux": "kvm", "windows": "whpx", "darwin": "hvf"} {
		got, err := Accelerator(goos, env(nil), okKVM)
		if err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", goos, got, err, want)
		}
	}
	if _, err := Accelerator("linux", env(nil), noKVM); codeOf(err) != "no_accelerator" || !strings.Contains(err.Error(), "/dev/kvm") {
		t.Fatalf("missing KVM must be refused with a fix, got %v", err)
	}
	if _, err := Accelerator("freebsd", env(nil), okKVM); codeOf(err) != "unsupported_os" {
		t.Fatalf("got %v", err)
	}
	// Software emulation only by explicit opt-in, even when KVM is missing.
	if got, err := Accelerator("linux", env(map[string]string{"DOCK_LOCAL_ACCEL": "tcg"}), noKVM); err != nil || got != "tcg" {
		t.Fatalf("tcg opt-in: %q %v", got, err)
	}
	if _, err := Accelerator("linux", env(map[string]string{"DOCK_LOCAL_ACCEL": "kvm"}), okKVM); codeOf(err) != "invalid_accel" {
		t.Fatalf("only tcg may be forced, got %v", err)
	}
}

func TestAcceleratorKVMErrors(t *testing.T) {
	pe := func(err error) error { return &fs.PathError{Op: "open", Path: "/dev/kvm", Err: err} }
	_, err := Accelerator("linux", env(nil), func() error { return pe(fs.ErrPermission) })
	if codeOf(err) != "no_accelerator" || !strings.Contains(err.Error(), "usermod -aG kvm") || strings.Contains(err.Error(), "firmware") {
		t.Fatalf("permission: %v", err)
	}
	_, err = Accelerator("linux", env(nil), func() error { return pe(fs.ErrNotExist) })
	if codeOf(err) != "no_accelerator" || !strings.Contains(err.Error(), "nested") || strings.Contains(err.Error(), "usermod") {
		t.Fatalf("missing: %v", err)
	}
	_, err = Accelerator("linux", env(nil), func() error { return errors.New("boom") })
	if codeOf(err) != "no_accelerator" || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("other: %v", err)
	}
}

func TestDoctorKVMPermissionHasFix(t *testing.T) {
	e, _, _ := newTestEngine(t)
	e.Driver = &QEMU{GOOS: "linux", GOARCH: "amd64", Getenv: env(nil), system: "/q/qemu", img: "/q/qemu-img",
		KVM: func() error { return &fs.PathError{Op: "open", Path: "/dev/kvm", Err: fs.ErrPermission} }}
	for _, c := range e.Doctor(context.Background()) {
		if c.Name != "accelerator" {
			continue
		}
		if c.OK || !strings.Contains(c.Fix, "usermod") || !strings.Contains(c.Detail, "permission denied") {
			t.Fatalf("accelerator row %+v", c)
		}
		return
	}
	t.Fatal("no accelerator row")
}

func TestKVMMessagesMentionGVisor(t *testing.T) {
	pe := func(err error) error { return &fs.PathError{Op: "open", Path: "/dev/kvm", Err: err} }
	for name, kerr := range map[string]error{"permission": pe(fs.ErrPermission), "missing": pe(fs.ErrNotExist), "other": errors.New("boom")} {
		_, err := Accelerator("linux", env(nil), func() error { return kerr })
		if err == nil || !strings.HasSuffix(err.Error(), GVisorHint) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !strings.HasPrefix(kvmMessage(pe(fs.ErrPermission)), "KVM is present but your user cannot open") {
		t.Error("the permission case must keep leading with the group fix")
	}
	for _, goos := range []string{"windows", "darwin"} {
		if a, err := Accelerator(goos, env(nil), func() error { return errors.New("x") }); err != nil || strings.Contains(a, "gvisor") {
			t.Errorf("%s: %q %v", goos, a, err)
		}
	}
}

func doctorEngine(t *testing.T, goos string, kvm error, gv *GVisor) *Engine {
	t.Helper()
	e, _, _ := newTestEngine(t)
	e.Driver = &QEMU{GOOS: goos, GOARCH: "amd64", Getenv: env(nil), system: "/q/qemu", img: "/q/qemu-img", KVM: func() error { return kvm }}
	e.GVisor = gv
	return e
}

func doctorRow(e *Engine, name string) (Check, bool) {
	for _, c := range e.Doctor(context.Background()) {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

func TestDoctorGVisorRow(t *testing.T) {
	look := func(string) (string, error) { return "docker", nil }
	run := func(out string) DockerRun {
		return func(context.Context, io.Reader, ...string) (string, string, error) { return out, "", nil }
	}
	noKVM := &fs.PathError{Op: "open", Path: "/dev/kvm", Err: fs.ErrNotExist}

	e := doctorEngine(t, "linux", noKVM, &GVisor{GOOS: "linux", LookPath: look, Run: run(`{"runsc":{}}`)})
	c, ok := doctorRow(e, "gvisor")
	if !ok || !c.OK || !c.Optional || !strings.Contains(c.Detail, "dock new --runtime gvisor") {
		t.Fatalf("gvisor ok row %+v", c)
	}
	if a, _ := doctorRow(e, "accelerator"); a.OK || !strings.Contains(a.Fix, "dock new --runtime gvisor") || !strings.Contains(a.Detail, GVisorHint) {
		t.Fatalf("accelerator row %+v", a)
	}

	e = doctorEngine(t, "linux", noKVM, &GVisor{GOOS: "linux", LookPath: look, Run: run(`{"runc":{}}`)})
	c, ok = doctorRow(e, "gvisor")
	if !ok || c.OK || !c.Optional || !strings.Contains(c.Detail, "runsc") || !strings.Contains(c.Fix, "runsc install") {
		t.Fatalf("gvisor missing row %+v", c)
	}
	if a, _ := doctorRow(e, "accelerator"); strings.Contains(a.Fix, "--runtime gvisor") {
		t.Fatalf("no gvisor suggestion in the fix when gvisor cannot run: %+v", a)
	}

	e = doctorEngine(t, "windows", nil, &GVisor{GOOS: "windows"})
	if _, ok := doctorRow(e, "gvisor"); ok {
		t.Fatal("no gvisor row off Linux")
	}
}

func TestFindToolOrder(t *testing.T) {
	missing := func(string) (string, error) { return "", errors.New("not on PATH") }
	exists := func(set ...string) func(string) bool {
		return func(p string) bool { return slices.Contains(set, p) }
	}
	// DOCK_QEMU wins over everything.
	got, err := findTool("qemu-system-x86_64", "DOCK_QEMU", "linux", env(map[string]string{"DOCK_QEMU": "/opt/q"}),
		func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil }, exists("/opt/q"))
	if err != nil || got != "/opt/q" {
		t.Fatalf("override: %q %v", got, err)
	}
	if _, err := findTool("qemu-system-x86_64", "DOCK_QEMU", "linux", env(map[string]string{"DOCK_QEMU": "/nope"}), missing, exists()); codeOf(err) != "qemu_not_found" {
		t.Fatalf("a bad override must fail, got %v", err)
	}
	// PATH next.
	got, _ = findTool("qemu-system-x86_64", "DOCK_QEMU", "linux", env(nil), func(string) (string, error) { return "/p/qemu", nil }, exists())
	if got != "/p/qemu" {
		t.Fatalf("path: %q", got)
	}
	// Standard install dirs last.
	got, err = findTool("qemu-system-x86_64", "DOCK_QEMU", "windows", env(nil), missing, exists(`C:\Program Files\qemu\qemu-system-x86_64.exe`))
	if err != nil || got != `C:\Program Files\qemu\qemu-system-x86_64.exe` {
		t.Fatalf("windows dir: %q %v", got, err)
	}
	got, err = findTool("qemu-system-aarch64", "DOCK_QEMU", "darwin", env(nil), missing, exists("/opt/homebrew/bin/qemu-system-aarch64"))
	if err != nil || got != "/opt/homebrew/bin/qemu-system-aarch64" {
		t.Fatalf("homebrew: %q %v", got, err)
	}
	_, err = findTool("qemu-system-x86_64", "DOCK_QEMU", "windows", env(nil), missing, exists())
	if codeOf(err) != "qemu_not_found" || !strings.Contains(err.Error(), "winget") {
		t.Fatalf("missing QEMU on Windows must hint the install, got %v", err)
	}
}

func windowsQEMU(run func(ctx context.Context, name string, args ...string) ([]byte, error), probe func(ctx context.Context, name string, args []string, wait time.Duration) (bool, []byte, error)) *QEMU {
	return &QEMU{GOOS: "windows", GOARCH: "amd64", Getenv: env(nil), Run: run, Probe: probe, system: `C:\q\qemu-system-x86_64.exe`, img: `C:\q\qemu-img.exe`}
}

func TestWHPXReadiness(t *testing.T) {
	ctx := context.Background()
	helpWith := func(s string) func(context.Context, string, ...string) ([]byte, error) {
		return func(context.Context, string, ...string) ([]byte, error) { return []byte(s), nil }
	}
	running := func(context.Context, string, []string, time.Duration) (bool, []byte, error) { return true, nil, nil }

	if err := windowsQEMU(helpWith("Accelerators supported in QEMU binary:\ntcg\nwhpx\n"), running).CheckAccel(ctx, "whpx"); err != nil {
		t.Fatalf("ready machine: %v", err)
	}
	// A QEMU build without whpx.
	err := windowsQEMU(helpWith("tcg\n"), running).CheckAccel(ctx, "whpx")
	if codeOf(err) != "no_accelerator" || !strings.Contains(err.Error(), "whpx") {
		t.Fatalf("got %v", err)
	}
	// whpx is built in but the probe machine cannot start: WHP is off.
	dead := func(context.Context, string, []string, time.Duration) (bool, []byte, error) {
		return false, []byte("qemu-system-x86_64.exe: WHPX: No accelerator found, hr=00000000"), errors.New("exit status 1")
	}
	err = windowsQEMU(helpWith("whpx"), dead).CheckAccel(ctx, "whpx")
	if codeOf(err) != "no_accelerator" || !strings.Contains(err.Error(), "Enable-WindowsOptionalFeature -Online -FeatureName HypervisorPlatform") || !strings.Contains(err.Error(), "reboot") {
		t.Fatalf("a failed probe must carry the fix text, got %v", err)
	}
	if !strings.Contains(err.Error(), "No accelerator found") {
		t.Fatalf("qemu output must be shown: %v", err)
	}
}

func TestPreflightReportsMissingQEMUWithoutProbing(t *testing.T) {
	q := &QEMU{GOOS: "linux", GOARCH: "amd64", Getenv: env(map[string]string{"DOCK_QEMU": "/definitely/not/here"})}
	if err := q.Preflight(context.Background()); codeOf(err) != "qemu_not_found" {
		t.Fatalf("got %v", err)
	}
	q = &QEMU{GOOS: "linux", GOARCH: "riscv64", Getenv: env(nil)}
	if err := q.Preflight(context.Background()); codeOf(err) != "unsupported_arch" {
		t.Fatalf("got %v", err)
	}
}

func TestOverlayUsesAbsoluteBackingFile(t *testing.T) {
	var got []string
	q := &QEMU{GOOS: "linux", GOARCH: "amd64", Getenv: env(nil), system: "/q/qemu", img: "/q/qemu-img",
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			got = append([]string{name}, args...)
			return nil, nil
		}}
	if err := q.CreateOverlay(context.Background(), "base.qcow2", "disk.qcow2"); err != nil {
		t.Fatal(err)
	}
	if got[0] != "/q/qemu-img" || got[1] != "create" || !slices.Contains(got, "-F") || got[len(got)-1] != "disk.qcow2" {
		t.Fatalf("argv %v", got)
	}
	if b := argAfter(t, got, "-b"); b == "base.qcow2" {
		t.Fatalf("backing file %q must be absolute", b)
	}
}

func TestGuestConfigShapeMatchesTheHostAgentFile(t *testing.T) {
	raw, _ := json.Marshal(DefaultGuestConfig("t"))
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for _, k := range []string{"token", "workspace", "roots"} {
		if _, ok := m[k]; !ok {
			t.Errorf("guest config lacks %q", k)
		}
	}
}

func TestGuestConfigHostnameOnlyWhenValid(t *testing.T) {
	for name, want := range map[string]string{"web": "web", "my.dock": "", "a_b": "", "": "", "-x": ""} {
		p := filepath.Join(t.TempDir(), "g.json")
		if err := WriteGuestConfig(p, "t", name, GuestOptions{}); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(p)
		var cfg GuestConfig
		_ = json.Unmarshal(raw, &cfg)
		if cfg.Hostname != want {
			t.Errorf("name %q: hostname %q, want %q", name, cfg.Hostname, want)
		}
	}
}

func TestResizeBelowTheImageSizeExplains(t *testing.T) {
	q := &QEMU{GOOS: "linux", GOARCH: "amd64", Getenv: env(nil), system: "/q/qemu", img: "/q/qemu-img",
		Run: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("qemu-img: Use the --shrink option to perform a shrink operation."), errors.New("exit status 1")
		}}
	if err := q.ResizeDisk(context.Background(), "d.qcow2", 1); codeOf(err) != "invalid_argument" {
		t.Fatalf("got %v", err)
	}
}

func TestGuestConfigAsksToBlockTheGatewayOnlyWhenNetworkIsOn(t *testing.T) {
	for blocked, want := range map[bool]bool{true: true, false: false} {
		p := filepath.Join(t.TempDir(), "g.json")
		if err := WriteGuestConfig(p, "t", "web", GuestOptions{BlockGateway: blocked}); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(p)
		if got := strings.Contains(string(raw), `"block_gateway":true`); got != want {
			t.Errorf("blockGateway=%v: config %s", blocked, raw)
		}
	}
}
