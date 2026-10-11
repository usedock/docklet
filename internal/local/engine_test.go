package local

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func codeOf(err error) string {
	var le *Error
	if errors.As(err, &le) {
		return le.Code
	}
	return ""
}

func TestNewExecStopResumeBranchRemove(t *testing.T) {
	ctx := context.Background()
	e, drv, hits := newTestEngine(t)

	d, err := e.New(ctx, Spec{Name: "web", CPUs: 3, MemoryMiB: 1024, DiskGiB: 7})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d.ID, "dk_") || d.State != "ready" || d.Runtime != Runtime || d.CPUs != 3 || d.MemoryMiB != 1024 || d.DiskGiB != 7 || d.Image != DefaultImage {
		t.Fatalf("dock %+v", d)
	}
	if len(drv.resizes) != 1 || drv.resizes[0] != 7 {
		t.Fatalf("disk must be sized to the request, resizes %v", drv.resizes)
	}
	if len(drv.starts) != 1 || drv.starts[0].CPUs != 3 || drv.starts[0].Network != "on" {
		t.Fatalf("launch %+v", drv.starts)
	}
	if hits[FileKernel] != 1 || hits[FileRootfs] != 1 {
		t.Fatalf("image must download once, hits %v", hits)
	}
	if _, err := e.New(ctx, Spec{Name: "web"}); codeOf(err) != "dock_name_taken" {
		t.Fatalf("duplicate name must be refused, got %v", err)
	}

	res, err := e.Exec(ctx, "web", "echo hi", 0)
	if err != nil || res.Stdout != "ran:echo hi" || res.ExitCode != 0 {
		t.Fatalf("exec %+v %v", res, err)
	}

	st, err := e.Stop(ctx, d.ID, false)
	if err != nil || st.State != "archived" {
		t.Fatalf("stop %+v %v", st, err)
	}
	if _, err := os.Stat(e.file(d.ID, "disk.qcow2")); err != nil {
		t.Fatalf("stop must keep the disk: %v", err)
	}
	if _, err := e.Exec(ctx, "web", "true", 0); codeOf(err) != "dock_not_running" {
		t.Fatalf("exec on a stopped dock, got %v", err)
	}

	// Branch of a stopped dock copies the disk and boots the copies.
	if err := os.WriteFile(e.file(d.ID, "disk.qcow2"), []byte("my-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	bs, err := e.Branch(ctx, "web", 2, "", Spec{})
	if err != nil || len(bs) != 2 {
		t.Fatalf("branch %+v %v", bs, err)
	}
	for _, b := range bs {
		got, _ := os.ReadFile(e.file(b.ID, "disk.qcow2"))
		if string(got) != "my-data" || b.State != "ready" || b.ID == d.ID {
			t.Fatalf("branch %+v disk %q", b, got)
		}
	}
	tokA, _ := e.token(bs[0].ID)
	tokB, _ := e.token(bs[1].ID)
	if tokA == tokB || tokA == "" {
		t.Fatal("each branch needs its own agent token")
	}
	if _, err := e.Branch(ctx, "web", 1, "web-2", Spec{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Branch(ctx, "web", 1, "web-2", Spec{}); codeOf(err) != "dock_name_taken" {
		t.Fatalf("branch name must be unique, got %v", err)
	}

	r, err := e.Resume(ctx, "web")
	if err != nil || r.State != "ready" {
		t.Fatalf("resume %+v %v", r, err)
	}
	// Branching a running dock stops it for the copy and resumes it.
	if _, err := e.Branch(ctx, "web", 1, "", Spec{CPUs: 1}); err != nil {
		t.Fatal(err)
	}
	if g, _ := e.Get(ctx, "web"); g.State != "ready" {
		t.Fatalf("source must be resumed after branching, state %s", g.State)
	}

	all, _ := e.List(ctx)
	if len(all) != 5 {
		t.Fatalf("want 5 docks, got %d", len(all))
	}
	if err := e.Remove(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Lookup("web"); codeOf(err) != "dock_not_found" {
		t.Fatalf("removed dock must be gone, got %v", err)
	}
	if _, err := os.Stat(e.store().DockDir(d.ID)); !os.IsNotExist(err) {
		t.Fatalf("remove must delete the dock dir: %v", err)
	}
}

func TestNewRefusesWithoutHypervisorBeforeDownloading(t *testing.T) {
	e, drv, hits := newTestEngine(t)
	drv.preflightErr = errf("no_accelerator", "KVM is not usable")
	_, err := e.New(context.Background(), Spec{})
	if codeOf(err) != "no_accelerator" {
		t.Fatalf("got %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("nothing may be downloaded when the hypervisor is missing: %v", hits)
	}
}

func TestNewCleansUpWhenTheVMDies(t *testing.T) {
	e, drv, _ := newTestEngine(t)
	drv.dieOnStart = true
	_, err := e.New(context.Background(), Spec{Name: "x"})
	if codeOf(err) != "boot_failed" {
		t.Fatalf("got %v", err)
	}
	if recs, _ := e.store().List(); len(recs) != 0 {
		t.Fatalf("failed dock must not stay in the store: %v", recs)
	}
}

func TestBootTimeoutKillsTheVM(t *testing.T) {
	e, drv, _ := newTestEngine(t)
	drv.neverAnswer = true
	e.BootTimeout = 600 * time.Millisecond
	_, err := e.New(context.Background(), Spec{})
	if codeOf(err) != "boot_timeout" {
		t.Fatalf("got %v", err)
	}
	for pid, alive := range drv.alive {
		if alive {
			t.Fatalf("vm %d still alive after the boot timeout", pid)
		}
	}
}

func TestStopKillsAfterTimeoutWhenGuestIgnoresPoweroff(t *testing.T) {
	ctx := context.Background()
	e, drv, _ := newTestEngine(t)
	e.StopTimeout = 400 * time.Millisecond
	d, err := e.New(ctx, Spec{Name: "stuck"})
	if err != nil {
		t.Fatal(err)
	}
	drv.ignorePoweroff = true
	st, err := e.Stop(ctx, "stuck", false)
	if err != nil || st.State != "archived" {
		t.Fatalf("stop %+v %v", st, err)
	}
	if e.alive(d.ID) {
		t.Fatal("vm must be killed after the graceful timeout")
	}
}

func TestSpecValidationAndDefaults(t *testing.T) {
	s, err := (Spec{}).withDefaults()
	if err != nil || s.CPUs != DefaultCPUs || s.MemoryMiB != DefaultMemoryMiB || s.DiskGiB != DefaultDiskGiB || s.Network != "on" {
		t.Fatalf("defaults %+v %v", s, err)
	}
	for _, bad := range []Spec{{CPUs: 99}, {MemoryMiB: 10}, {Network: "maybe"}} {
		if _, err := bad.withDefaults(); codeOf(err) != "invalid_argument" {
			t.Errorf("%+v must be invalid, got %v", bad, err)
		}
	}
}

func TestNetworkOffIsPassedToTheLaunch(t *testing.T) {
	e, drv, _ := newTestEngine(t)
	if _, err := e.New(context.Background(), Spec{Network: "off"}); err != nil {
		t.Fatal(err)
	}
	if drv.starts[0].Network != "off" {
		t.Fatalf("launch %+v", drv.starts[0])
	}
}
