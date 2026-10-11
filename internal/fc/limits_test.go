package fc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/usedock/docklet/api/v1"
)

func TestLimitsForScalesAndCaps(t *testing.T) {
	auto := Limits{LimitAuto, LimitAuto, LimitAuto, LimitAuto}
	got := auto.For(apiv1.Resources{CPUs: 2})
	want := Effective{DiskMBps: 200, DiskIOPS: 4000, NetMBps: 50, PidsLimit: 4096}
	if got != want {
		t.Fatalf("2 vcpu = %+v, want %+v", got, want)
	}
	big := auto.For(apiv1.Resources{CPUs: 64})
	if big.DiskMBps != 1000 || big.DiskIOPS != 20000 || big.NetMBps != 250 {
		t.Fatalf("64 vcpu must hit caps, got %+v", big)
	}
	over := Limits{DiskMBps: 7, DiskIOPS: 0, NetMBps: 9, PidsLimit: 11}.For(apiv1.Resources{CPUs: 4})
	if over != (Effective{DiskMBps: 7, NetMBps: 9, PidsLimit: 11}) {
		t.Fatalf("explicit values and 0 = unlimited must pass through, got %+v", over)
	}
	if (Limits{}).For(apiv1.Resources{CPUs: 4}) != (Effective{}) {
		t.Fatal("zero Limits must be unlimited")
	}
}

func TestMachineConfigRateLimiters(t *testing.T) {
	spec := baseSpec()
	spec.Limits = Effective{DiskMBps: 100, DiskIOPS: 2000, NetMBps: 25}
	cfg, err := MachineConfig(spec)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cfg.Drives[0].RateLimiter)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"bandwidth":{"one_time_burst":104857600,"refill_time":1000,"size":104857600},"ops":{"one_time_burst":2000,"refill_time":1000,"size":2000}}`
	if string(raw) != want {
		t.Fatalf("drive rate_limiter = %s, want %s", raw, want)
	}
	iface := cfg.NetworkInterfaces[0]
	wantNet := `{"bandwidth":{"one_time_burst":26214400,"refill_time":1000,"size":26214400}}`
	for name, rl := range map[string]any{"in": iface.InRateLimiter, "out": iface.OutRateLimiter} {
		raw, _ := json.Marshal(rl)
		if string(raw) != wantNet {
			t.Fatalf("%s limiter = %s, want %s", name, raw, wantNet)
		}
	}
	// The drive config as sent to firecracker carries the rate_limiter key.
	full, _ := json.Marshal(cfg.Drives[0])
	if !json.Valid(full) || !strings.Contains(string(full), `"rate_limiter":`) {
		t.Fatalf("drive json = %s, want a rate_limiter block", full)
	}
}

func TestMachineConfigUnlimitedHasNoRateLimiter(t *testing.T) {
	cfg, err := MachineConfig(baseSpec())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Drives[0].RateLimiter != nil || cfg.NetworkInterfaces[0].InRateLimiter != nil || cfg.NetworkInterfaces[0].OutRateLimiter != nil {
		t.Fatal("zero limits must not emit rate limiters")
	}
	spec := baseSpec()
	spec.Limits = Effective{DiskIOPS: 500}
	cfg, _ = MachineConfig(spec)
	rl := cfg.Drives[0].RateLimiter
	if rl == nil || rl.Bandwidth != nil || rl.Ops == nil {
		t.Fatalf("ops-only limiter = %+v", rl)
	}
}

func TestMachineConfigJailerUIDGID(t *testing.T) {
	spec := baseSpec()
	spec.UID, spec.GID = 2000000123, 2000000123
	cfg, err := MachineConfig(spec)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JailerCfg.UID == nil || *cfg.JailerCfg.UID != 2000000123 || cfg.JailerCfg.GID == nil || *cfg.JailerCfg.GID != 2000000123 {
		t.Fatalf("jailer uid/gid = %v/%v, want the dock's own id", cfg.JailerCfg.UID, cfg.JailerCfg.GID)
	}
}

func TestPrepareJailChownsLogToDockUID(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	if err := os.WriteFile(kernel, []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := baseSpec()
	spec.JailRoot = filepath.Join(dir, "root")
	spec.KernelImage = kernel
	spec.InitrdImage = ""
	spec.UID, spec.GID = 2000000007, 2000000007
	type call struct {
		path     string
		uid, gid int
	}
	var calls []call
	err := prepareJail(spec, func(p string, u, g int) error {
		calls = append(calls, call{p, u, g})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wantLog := filepath.Join(spec.JailRoot, "logs", "dock-1.log")
	if len(calls) != 1 || calls[0] != (call{wantLog, 2000000007, 2000000007}) {
		t.Fatalf("chown calls = %+v, want only the log owned by the dock uid", calls)
	}
}
