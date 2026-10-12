//go:build e2e

package local

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/usedock/docklet/internal/dockyard"
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
	if os.Getenv("DOCK_E2E_REQUIRE") != "" && dockyard.DockyardIndexURL != "" {
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

// TestE2ELocalGVisorDock runs a real gVisor dock through Docker and runsc. It skips cleanly
// when Docker or runsc is missing, unless DOCK_E2E_REQUIRE_GVISOR is set (CI).
func TestE2ELocalGVisorDock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	t.Setenv("DOCK_DATA_DIR", t.TempDir())
	e, err := NewEngine(os.Getenv, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.gvisor().Preflight(ctx); err != nil {
		if os.Getenv("DOCK_E2E_REQUIRE_GVISOR") != "" {
			t.Fatalf("gvisor docks cannot run here: %v", err)
		}
		t.Skipf("gvisor docks cannot run here: %v", err)
	}
	d, err := e.New(ctx, Spec{Name: "e2egv", Runtime: RuntimeGVisor, CPUs: 1, MemoryMiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), d.ID) })
	if d.State != "ready" || d.Runtime != RuntimeGVisor {
		t.Fatalf("dock %+v", d)
	}
	run := func(script string) string {
		t.Helper()
		res, err := e.Exec(ctx, "e2egv", script, 60*time.Second)
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("%q: %+v %v", script, res, err)
		}
		return res.Stdout
	}
	if out := run("uname -s; id -u"); !strings.Contains(out, "Linux") || !strings.Contains(out, "1001") {
		t.Fatalf("uname/id: %q", out)
	}
	// The dock's own bridge network must still give it DNS and internet egress (network on).
	if out := run("curl -sS -o /dev/null -w '%{http_code}' --max-time 20 https://github.com"); !strings.HasPrefix(out, "2") && !strings.HasPrefix(out, "3") {
		t.Fatalf("egress: %q", out)
	}
	run("echo keep > /home/user/work/f")
	// Root in the sandbox: sudo works, and a change to the rootfs (a package) must survive stop/resume.
	if out := run("sudo id -u"); strings.TrimSpace(out) != "0" {
		t.Fatalf("sudo id: %q", out)
	}
	run("sudo touch /usr/local/bin/e2e-marker")
	if res, err := e.Exec(ctx, "e2egv", "sudo apt-get update -qq && sudo apt-get install -y -qq tree && command -v tree", 280*time.Second); err != nil || res.ExitCode != 0 {
		t.Fatalf("apt-get install: %+v %v", res, err)
	}

	run("(python3 -m http.server 3000 >/dev/null 2>&1 &) ; sleep 1")
	conn, err := e.OpenPort(ctx, "e2egv", 3000)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
	head := make([]byte, 8)
	if _, err := io.ReadFull(conn, head); err != nil || !strings.HasPrefix(string(head), "HTTP/") {
		t.Fatalf("port forward: %q %v", head, err)
	}
	conn.Close()

	if _, err := e.Stop(ctx, "e2egv", false); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.Get(ctx, "e2egv"); st.State != "archived" {
		t.Fatalf("stopped dock state %s", st.State)
	}
	if _, err := e.Resume(ctx, "e2egv"); err != nil {
		t.Fatal(err)
	}
	if out := run("cat /home/user/work/f"); strings.TrimSpace(out) != "keep" {
		t.Fatalf("the work volume must survive stop and resume: %q", out)
	}
	if out := run("ls /usr/local/bin/e2e-marker && command -v tree"); !strings.Contains(out, "e2e-marker") || !strings.Contains(out, "tree") {
		t.Fatalf("the whole rootfs must survive stop and resume: %q", out)
	}
	// The published port and DNS still work on the reconnected network.
	if out := run("curl -sS -o /dev/null -w '%{http_code}' --max-time 20 https://github.com"); !strings.HasPrefix(out, "2") && !strings.HasPrefix(out, "3") {
		t.Fatalf("egress after resume: %q", out)
	}
	run("(python3 -m http.server 3001 >/dev/null 2>&1 &) ; sleep 1")
	conn, err = e.OpenPort(ctx, "e2egv", 3001)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
	if _, err := io.ReadFull(conn, head); err != nil || !strings.HasPrefix(string(head), "HTTP/") {
		t.Fatalf("port forward after resume: %q %v", head, err)
	}
	conn.Close()
	if err := e.Remove(ctx, "e2egv"); err != nil {
		t.Fatal(err)
	}
}
