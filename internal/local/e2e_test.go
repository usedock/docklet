//go:build e2e

package local

import (
	"context"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/usedock/docklet/internal/dockyard"
	"github.com/usedock/docklet/internal/guestagent"
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
	if rec, _ := e.Lookup("e2egv"); rec.GVisorRuntime != "runsc-suid" {
		t.Fatalf("CI registers runsc-suid, the dock must use it: %q", rec.GVisorRuntime)
	}
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

	// Branch: the new dock carries the rootfs changes and the work files, and stays independent.
	bs, err := e.Branch(ctx, "e2egv", 1, "e2egv-branch", Spec{})
	if err != nil || len(bs) != 1 || bs[0].State != "ready" || bs[0].Runtime != RuntimeGVisor {
		t.Fatalf("branch %+v %v", bs, err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), bs[0].ID) })
	if st, _ := e.Get(ctx, "e2egv"); st.State != "ready" {
		t.Fatalf("a running source must be resumed after branching, state %s", st.State)
	}
	brun := func(script string) string {
		t.Helper()
		res, err := e.Exec(ctx, "e2egv-branch", script, 60*time.Second)
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("branch %q: %+v %v", script, res, err)
		}
		return res.Stdout
	}
	if out := brun("cat /home/user/work/f; stat -c %u /home/user/work/f; ls /usr/local/bin/e2e-marker; command -v tree"); !strings.Contains(out, "keep") || !strings.Contains(out, "1001") || !strings.Contains(out, "e2e-marker") || !strings.Contains(out, "tree") {
		t.Fatalf("the branch must carry the work files (owned by uid 1001) and the rootfs changes: %q", out)
	}
	brun("echo branch-only > /home/user/work/g; sudo touch /usr/local/bin/branch-marker")
	if res, _ := e.Exec(ctx, "e2egv", "test ! -e /home/user/work/g && test ! -e /usr/local/bin/branch-marker", 60*time.Second); res.ExitCode != 0 {
		t.Fatalf("changes in the branch must not show in the source: %+v", res)
	}
	run("echo source-only > /home/user/work/h")
	if res, _ := e.Exec(ctx, "e2egv-branch", "test ! -e /home/user/work/h", 60*time.Second); res.ExitCode != 0 {
		t.Fatalf("changes in the source must not show in the branch: %+v", res)
	}
	branchImage := gvBranchImage(bs[0].ID)
	if err := e.Remove(ctx, "e2egv-branch"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.gvisor().docker(ctx, nil, "image", "inspect", branchImage); err == nil {
		t.Fatalf("rm must remove the branch image %s", branchImage)
	}
	if _, _, err := e.gvisor().docker(ctx, nil, "volume", "inspect", gvVolume(bs[0].ID)); err == nil {
		t.Fatal("rm must remove the branch volume")
	}
	if err := e.Remove(ctx, "e2egv"); err != nil {
		t.Fatal(err)
	}
}

// TestE2ELocalGVisorNetworkOff runs a real --network off gVisor dock: the agent is reached through
// docker exec, and the sandbox has no internet, no DNS and no route to the host.
func TestE2ELocalGVisorNetworkOff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
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
	d, err := e.New(ctx, Spec{Name: "e2eoff", Runtime: RuntimeGVisor, Network: "off", CPUs: 1, MemoryMiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), d.ID) })
	if d.State != "ready" {
		t.Fatalf("dock %+v", d)
	}
	exec := func(script string) ExecResult {
		t.Helper()
		res, err := e.Exec(ctx, "e2eoff", script, 60*time.Second)
		if err != nil {
			t.Fatalf("%q: %v", script, err)
		}
		return res
	}
	if res := exec("uname -s; id -u"); res.ExitCode != 0 || !strings.Contains(res.Stdout, "Linux") || !strings.Contains(res.Stdout, "1001") {
		t.Fatalf("uname/id: %+v", res)
	}
	if res := exec("curl -sS --max-time 5 https://github.com"); res.ExitCode == 0 {
		t.Fatalf("a network-off dock must not reach the internet: %+v", res)
	}
	if res := exec("getent hosts github.com"); res.ExitCode == 0 {
		t.Fatalf("a network-off dock must not resolve names: %+v", res)
	}
	// A listener on every host interface must be unreachable from the dock, through the docker bridge gateway too.
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	gw := "172.17.0.1"
	if out, _, err := e.gvisor().docker(ctx, nil, "network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}"); err == nil && strings.TrimSpace(out) != "" {
		gw = strings.TrimSpace(out)
	}
	connect := "python3 -c \"import socket,sys; socket.create_connection(('" + gw + "', " + strconv.Itoa(l.Addr().(*net.TCPAddr).Port) + "), 3)\""
	if res := exec(connect); res.ExitCode == 0 {
		t.Fatalf("a network-off dock must not reach the host's docker gateway %s: %+v", gw, res)
	}
	// No interface but loopback, and no route out even to an IP literal (no DNS involved).
	connectIP := "python3 -c \"import socket; socket.create_connection(('1.1.1.1', 443), 3)\""
	if res := exec(connectIP); res.ExitCode == 0 {
		t.Fatalf("a network-off dock must not reach an internet address: %+v", res)
	}
	noNetwork := func(id string) {
		t.Helper()
		out, _, err := e.gvisor().docker(ctx, nil, "inspect", "--format", "{{.HostConfig.NetworkMode}}", gvContainer(id))
		if err != nil || strings.TrimSpace(out) != "none" {
			t.Fatalf("container of %s must run with --network none: %q %v", id, out, err)
		}
		if out, _, _ := e.gvisor().docker(ctx, nil, "network", "ls", "--filter", "name="+gvNetwork(id), "--format", "{{.Name}}"); strings.TrimSpace(out) != "" {
			t.Fatalf("a network-off dock must not own a docker network: %q", out)
		}
	}
	noNetwork(d.ID)

	// Closing an exec stream (the docker exec client is killed) must end the agent session in the sandbox.
	exec(`cat > /tmp/count.py <<'EOF'
import os, sys
needle = (sys.argv[1] + sys.argv[2]).encode()
n = 0
for p in os.listdir('/proc'):
    if not p.isdigit() or int(p) == os.getpid():
        continue
    try:
        if needle in open('/proc/' + p + '/cmdline', 'rb').read().replace(b'\0', b' '):
            n += 1
    except OSError:
        pass
print(n)
EOF`)
	count := func(a, b string) string {
		t.Helper()
		return strings.TrimSpace(exec("python3 /tmp/count.py '" + a + "' '" + b + "'").Stdout)
	}
	eventually := func(what string, ok func() bool) {
		t.Helper()
		for i := 0; i < 40; i++ {
			if ok() {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	term, err := e.OpenTerminal(ctx, "e2eoff", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if err := term.WriteFrame(guestagent.TerminalData, []byte("sleep 4242\n")); err != nil {
		t.Fatal(err)
	}
	eventually("the shell to start sleep", func() bool { return count("sleep 42", "42") == "1" })
	_ = term.Close()
	eventually("the shell to be hung up after the terminal closed", func() bool { return count("sleep 42", "42") == "0" })
	exec("(python3 -c \"import socket; s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind(('127.0.0.1', 4000)); s.listen(1); c,_=s.accept(); c.recv(1); open('/tmp/tunnel-closed', 'w').write('y')\" >/dev/null 2>&1 &) ; sleep 1")
	tun, err := e.OpenPort(ctx, "e2eoff", 4000)
	if err != nil {
		t.Fatal(err)
	}
	_ = tun.Close()
	eventually("the tunnel upstream to close", func() bool { return strings.TrimSpace(exec("cat /tmp/tunnel-closed 2>/dev/null").Stdout) == "y" })
	// Only the relay of the counting exec itself is left.
	eventually("relays to exit", func() bool { return count("import os,soc", "ket,sys") == "1" })

	exec("echo keep > /home/user/work/f")

	portHead := func(port string) {
		t.Helper()
		exec("(python3 -m http.server " + port + " >/dev/null 2>&1 &) ; sleep 1")
		n, _ := strconv.Atoi(port)
		conn, err := e.OpenPort(ctx, "e2eoff", n)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
		head := make([]byte, 8)
		if _, err := io.ReadFull(conn, head); err != nil || !strings.HasPrefix(string(head), "HTTP/") {
			t.Fatalf("port forward: %q %v", head, err)
		}
	}
	portHead("3000")

	if _, err := e.Stop(ctx, "e2eoff", false); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.Get(ctx, "e2eoff"); st.State != "archived" {
		t.Fatalf("stopped dock state %s", st.State)
	}
	if _, err := e.Resume(ctx, "e2eoff"); err != nil {
		t.Fatal(err)
	}
	if res := exec("cat /home/user/work/f"); strings.TrimSpace(res.Stdout) != "keep" {
		t.Fatalf("the work volume must survive stop and resume: %+v", res)
	}
	if res := exec("curl -sS --max-time 5 https://github.com"); res.ExitCode == 0 {
		t.Fatalf("still no internet after resume: %+v", res)
	}
	if res := exec(connectIP); res.ExitCode == 0 {
		t.Fatalf("still no route out after resume: %+v", res)
	}
	noNetwork(d.ID)
	portHead("3001")

	bs, err := e.Branch(ctx, "e2eoff", 1, "e2eoff-branch", Spec{})
	if err != nil || len(bs) != 1 || bs[0].State != "ready" {
		t.Fatalf("branch %+v %v", bs, err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), bs[0].ID) })
	noNetwork(bs[0].ID)
	res, err := e.Exec(ctx, "e2eoff-branch", "cat /home/user/work/f; curl -sS --max-time 5 https://github.com", 60*time.Second)
	if err != nil || res.ExitCode == 0 || !strings.Contains(res.Stdout, "keep") {
		t.Fatalf("a branch of a network-off dock has the files and no network: %+v %v", res, err)
	}
	if err := e.Remove(ctx, "e2eoff-branch"); err != nil {
		t.Fatal(err)
	}
	if err := e.Remove(ctx, "e2eoff"); err != nil {
		t.Fatal(err)
	}
	if out, _, _ := e.gvisor().docker(ctx, nil, "ps", "-a", "--filter", "name=dock-local-"+d.ID, "--format", "{{.Names}}"); strings.TrimSpace(out) != "" {
		t.Fatalf("rm must remove the container: %q", out)
	}
}
