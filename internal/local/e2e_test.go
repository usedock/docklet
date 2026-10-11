//go:build e2e

package local

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestE2ELocalDock boots a real dock with QEMU and the host accelerator. It needs QEMU, a
// usable accelerator and a Dockyard index with DefaultImage for this architecture
// (DOCK_DOCKYARD_URL overrides the index); it skips cleanly when any of them is missing.
func TestE2ELocalDock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	t.Setenv("DOCK_DATA_DIR", t.TempDir())
	e, err := NewEngine(os.Getenv, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	// Once a release is pinned, CI sets DOCK_E2E_REQUIRE so a missing accelerator or image fails.
	skip := t.Skipf
	if os.Getenv("DOCK_E2E_REQUIRE") != "" && PinnedIndexURL() != "" {
		skip = t.Fatalf
	}
	if err := e.Driver.Preflight(ctx); err != nil {
		skip("local docks cannot run here: %v", err)
	}
	if _, err := e.ResolveImage(ctx, DefaultImage); err != nil {
		skip("no Dockyard image available: %v", err)
	}

	d, err := e.New(ctx, Spec{Name: "e2e", CPUs: 2, MemoryMiB: 1024, DiskGiB: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), d.ID) })
	if d.State != "ready" {
		t.Fatalf("state %s", d.State)
	}
	res, err := e.Exec(ctx, "e2e", "uname -s && echo data > /home/user/work/f && cat /home/user/work/f", 60*time.Second)
	if err != nil || res.ExitCode != 0 || !strings.Contains(res.Stdout, "Linux") {
		t.Fatalf("exec %+v %v", res, err)
	}

	res, err = e.Exec(ctx, "e2e", "docker --version && hostname", 60*time.Second)
	if err != nil || res.ExitCode != 0 || !strings.Contains(res.Stdout, "Docker version") || !strings.Contains(res.Stdout, "e2e") {
		t.Fatalf("docker and hostname in the image: %+v %v", res, err)
	}
	if _, err := e.Stop(ctx, "e2e", false); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.Get(ctx, "e2e"); st.State != "archived" {
		t.Fatalf("stopped dock state %s", st.State)
	}
	bs, err := e.Branch(ctx, "e2e", 1, "e2e-branch", Spec{})
	if err != nil || len(bs) != 1 {
		t.Fatalf("branch %v %v", bs, err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), bs[0].ID) })
	if _, err := e.Resume(ctx, "e2e"); err != nil {
		t.Fatal(err)
	}
	res, err = e.Exec(ctx, "e2e-branch", "cat /home/user/work/f", 60*time.Second)
	if err != nil || strings.TrimSpace(res.Stdout) != "data" {
		t.Fatalf("a branch must carry the disk of its source: %+v %v", res, err)
	}
	if err := e.Remove(ctx, "e2e-branch"); err != nil {
		t.Fatal(err)
	}
	if err := e.Remove(ctx, "e2e"); err != nil {
		t.Fatal(err)
	}
}
