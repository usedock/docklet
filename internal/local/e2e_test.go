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

// gvProbeScript prints OPEN or BLOCKED per target ("tcp|udp,label,host,port"); it probes in parallel.
const gvProbeScript = `import socket, sys, threading
def tcp(h, p):
    try:
        socket.create_connection((h, p), 3).close()
        return True
    except Exception:
        return False
def udp(h, p):
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.settimeout(2)
    try:
        s.sendto(b"ping", (h, p))
        s.recv(16)
        return True
    except Exception:
        return False
    finally:
        s.close()
res = {}
def probe(a):
    kind, label, h, p = a.split(",")
    res[a] = (label, h, p, kind, (tcp if kind == "tcp" else udp)(h, int(p)))
ts = [threading.Thread(target=probe, args=(a,)) for a in sys.argv[1:]]
[t.start() for t in ts]
[t.join() for t in ts]
for a in sys.argv[1:]:
    label, h, p, kind, ok = res[a]
    print(label, kind, h, p, "OPEN" if ok else "BLOCKED")
`

// TestE2ELocalGVisorHostBlocked proves a network-on gVisor dock cannot reach services on the host
// (through the bridge gateway or any other host address), even as root in the dock, while internet,
// DNS, the agent and dock port keep working, and that the block is installed and removed with the dock.
func TestE2ELocalGVisorHostBlocked(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
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
	g := e.gvisor()

	// Host services: every interface, and loopback only, over TCP; a UDP echo; an IPv6 listener.
	listen := func(network, addr string) net.Listener {
		l, err := net.Listen(network, addr)
		if err != nil {
			return nil
		}
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		t.Cleanup(func() { l.Close() })
		return l
	}
	port := func(l net.Listener) string { return strconv.Itoa(l.Addr().(*net.TCPAddr).Port) }
	anyL, loL, v6L := listen("tcp4", "0.0.0.0:0"), listen("tcp4", "127.0.0.1:0"), listen("tcp6", "[::]:0")
	if anyL == nil || loL == nil {
		t.Fatal("cannot listen on the host")
	}
	pc, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 64)
		for {
			n, a, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(buf[:n], a)
		}
	}()
	udpPort := strconv.Itoa(pc.LocalAddr().(*net.UDPAddr).Port)
	// The listeners work: the host itself reaches each of them (so BLOCKED below is the block).
	for _, a := range []string{"127.0.0.1:" + port(anyL), "127.0.0.1:" + port(loL)} {
		c, err := net.Dial("tcp", a)
		if err != nil {
			t.Fatalf("the host cannot reach its own listener %s: %v", a, err)
		}
		c.Close()
	}

	d, err := e.New(ctx, Spec{Name: "e2ehb", Runtime: RuntimeGVisor, CPUs: 1, MemoryMiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), d.ID) })
	run := func(name, script string) ExecResult {
		t.Helper()
		res, err := e.Exec(ctx, name, script, 150*time.Second)
		if err != nil {
			t.Fatalf("%q: %v", script, err)
		}
		return res
	}
	// prepare puts the probe in the dock and starts a loopback listener the probe must see as OPEN.
	prepare := func(name string) {
		t.Helper()
		run(name, "cat > /tmp/probe.py <<'PYEOF'\n"+gvProbeScript+"PYEOF")
		run(name, "(python3 -m http.server 3100 --bind 127.0.0.1 >/dev/null 2>&1 &) ; sleep 1")
	}
	prepare("e2ehb")

	netGateway := func(netName string) string {
		t.Helper()
		out, stderr, err := g.docker(ctx, nil, "network", "inspect", netName, "--format", "{{range .IPAM.Config}}{{.Gateway}}{{end}}")
		if err != nil || strings.TrimSpace(out) == "" {
			t.Fatalf("gateway of %s: %v %s", netName, err, stderr)
		}
		return strings.TrimSpace(out)
	}
	// Every IPv4 address the host owns, plus docker0.
	hostIPs := map[string]bool{"172.17.0.1": true}
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ip, _, err := net.ParseCIDR(a.String()); err == nil && ip.To4() != nil && !ip.IsLoopback() {
				hostIPs[ip.String()] = true
			}
		}
	}
	// Set below: a runc container publishing on every host address.
	pubPort, pubIP := "", ""
	targetsFor := func(id string) []string {
		gw := netGateway(gvNetwork(id))
		hosts := map[string]bool{gw: true}
		for ip := range hostIPs {
			hosts[ip] = true
		}
		var args []string
		for h := range hosts {
			args = append(args, "tcp,any,"+h+","+port(anyL), "udp,udp,"+h+","+udpPort)
			if pubPort != "" {
				args = append(args, "tcp,published,"+h+","+pubPort)
			}
		}
		if pubIP != "" {
			args = append(args, "tcp,published-direct,"+pubIP+",8000")
		}
		// The 127.0.0.1-only listener is never reachable, directly or through the gateway.
		args = append(args, "tcp,loopback,127.0.0.1,"+port(loL), "tcp,loopback-gw,"+gw+","+port(loL))
		if v6L != nil {
			if bi, err := net.InterfaceByName(gvBridge(id)); err == nil {
				addrs, _ := bi.Addrs()
				for _, a := range addrs {
					if ip, _, err := net.ParseCIDR(a.String()); err == nil && ip.To4() == nil && ip.IsLinkLocalUnicast() {
						args = append(args, "tcp,v6-linklocal,"+ip.String()+"%eth0,"+port(v6L))
					}
				}
			}
		}
		return args
	}
	quote := func(args []string) string { return "'" + strings.Join(args, "' '") + "'" }
	blocked := func(name, id, how, prefix string) {
		t.Helper()
		res := run(name, prefix+"python3 /tmp/probe.py "+quote(append(targetsFor(id), "tcp,control-self,127.0.0.1,3100")))
		if res.ExitCode != 0 {
			t.Fatalf("%s probe: %+v", how, res)
		}
		if !strings.Contains(res.Stdout, "control-self tcp 127.0.0.1 3100 OPEN") {
			t.Fatalf("%s: the probe cannot see an open port, so BLOCKED proves nothing: %s", how, res.Stdout)
		}
		for _, line := range strings.Split(res.Stdout, "\n") {
			if strings.HasSuffix(line, " OPEN") && !strings.HasPrefix(line, "control-self ") {
				t.Fatalf("%s: the dock reaches a host service: %q\nall:\n%s", how, line, res.Stdout)
			}
		}
		if !strings.Contains(res.Stdout, "BLOCKED") {
			t.Fatalf("%s: no result: %s", how, res.Stdout)
		}
	}
	// ruleList is what the host firewall holds, read through the same kind of helper the block uses.
	ruleList := func() string {
		t.Helper()
		image, err := g.EnsureImage(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		out, stderr, err := g.docker(ctx, nil, hostHelperArgs(image, "iptables -w -S; iptables-legacy -w -S 2>/dev/null; ip6tables -w -S 2>/dev/null; true")...)
		if err != nil {
			t.Fatalf("read the host rules: %v %s", err, stderr)
		}
		return out
	}
	hasRules := func(id string) bool {
		out := ruleList()
		return strings.Contains(out, "-j "+gvChain(id)) && strings.Contains(out, "-N "+gvChain(id)) && strings.Contains(out, "-i "+gvBridge(id))
	}
	noTrace := func(id string) {
		t.Helper()
		if out := ruleList(); strings.Contains(out, gvChain(id)) || strings.Contains(out, gvBridge(id)) {
			t.Fatalf("rules of %s are left on the host:\n%s", id, out)
		}
	}
	if !hasRules(d.ID) {
		t.Fatalf("the host has no rules for %s:\n%s", d.ID, ruleList())
	}

	blocked("e2ehb", d.ID, "as the dock user", "")
	blocked("e2ehb", d.ID, "as root in the dock", "sudo ")
	// Root tries to remove any in-sandbox filter and change routes: the block lives on the host.
	run("e2ehb", "sudo sh -c 'iptables -F; iptables -P OUTPUT ACCEPT; iptables-legacy -F; ip route flush cache' >/dev/null 2>&1; true")
	blocked("e2ehb", d.ID, "as root after flushing iptables", "sudo ")

	// Another container (runc, default bridge) publishes on 0.0.0.0, like a user's database. From
	// another bridge Docker's userland proxy takes it on INPUT; without the proxy, Docker's own
	// network isolation drops the DNATed connection in FORWARD.
	pubImage, err := g.EnsureImage(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	pubName := "dock-e2e-pub-" + d.ID
	t.Cleanup(func() { _, _, _ = g.docker(context.Background(), nil, "rm", "-f", pubName) })
	if _, stderr, err := g.docker(ctx, nil, "run", "-d", "--runtime=runc", "--name", pubName, "-p", "0.0.0.0::8000",
		"--entrypoint", "python3", pubImage, "-m", "http.server", "8000"); err != nil {
		t.Fatalf("published container: %v %s", err, stderr)
	}
	pubOut, _, err := g.docker(ctx, nil, "port", pubName, "8000/tcp")
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	first := strings.TrimSpace(strings.Split(strings.TrimSpace(pubOut), "\n")[0])
	pubPort = first[strings.LastIndex(first, ":")+1:]
	ipOut, _, _ := g.docker(ctx, nil, "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", pubName)
	pubIP = strings.TrimSpace(ipOut)
	for i := 0; ; i++ {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+pubPort, 2*time.Second)
		if err == nil {
			c.Close()
			break
		}
		if i == 30 {
			t.Fatalf("the published container never listened on %s: %v", pubPort, err)
		}
		time.Sleep(time.Second)
	}
	blocked("e2ehb", d.ID, "as root, against a published container port", "sudo ")

	// Internet by IP and by name (DNS from the host resolvers), and dock port, still work.
	if res := run("e2ehb", "curl -sS -o /dev/null -w '%{http_code}' --max-time 20 https://github.com"); res.ExitCode != 0 || !strings.HasPrefix(res.Stdout, "2") && !strings.HasPrefix(res.Stdout, "3") {
		t.Fatalf("egress by name: %+v", res)
	}
	if res := run("e2ehb", "getent hosts github.com"); res.ExitCode != 0 {
		t.Fatalf("dns: %+v", res)
	}
	if res := run("e2ehb", "python3 /tmp/probe.py tcp,ip,1.1.1.1,443"); !strings.Contains(res.Stdout, "ip tcp 1.1.1.1 443 OPEN") {
		t.Fatalf("egress by IP: %+v", res)
	}
	portHead := func(name, p string) {
		t.Helper()
		run(name, "(python3 -m http.server "+p+" >/dev/null 2>&1 &) ; sleep 1")
		n, _ := strconv.Atoi(p)
		conn, err := e.OpenPort(ctx, name, n)
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
	portHead("e2ehb", "3000")

	// Stop removes the rules; resume installs them again.
	if _, err := e.Stop(ctx, "e2ehb", false); err != nil {
		t.Fatal(err)
	}
	noTrace(d.ID)
	// A network left by an older dock (Docker's br-<id> bridge name, the container still attached,
	// as after a host reboot) must not let resume run the dock behind a rule for another interface.
	if _, stderr, err := g.docker(ctx, nil, "network", "create", "--driver", "bridge", gvNetwork(d.ID)); err != nil {
		t.Fatalf("old-style network: %v %s", err, stderr)
	}
	if _, stderr, err := g.docker(ctx, nil, "network", "connect", gvNetwork(d.ID), gvContainer(d.ID)); err != nil {
		t.Fatalf("attach to the old-style network: %v %s", err, stderr)
	}
	if _, err := e.Resume(ctx, "e2ehb"); err != nil {
		t.Fatal(err)
	}
	if !hasRules(d.ID) {
		t.Fatalf("resume must reinstall the rules:\n%s", ruleList())
	}
	prepare("e2ehb")
	blocked("e2ehb", d.ID, "as root after resume", "sudo ")
	if res := run("e2ehb", "curl -sS -o /dev/null -w '%{http_code}' --max-time 20 https://github.com"); res.ExitCode != 0 || !strings.HasPrefix(res.Stdout, "2") && !strings.HasPrefix(res.Stdout, "3") {
		t.Fatalf("egress after resume: %+v", res)
	}
	portHead("e2ehb", "3001")

	// A branch has its own bridge and rules, and cannot reach the host nor its source's network.
	bs, err := e.Branch(ctx, "e2ehb", 1, "e2ehb-branch", Spec{})
	if err != nil || len(bs) != 1 {
		t.Fatalf("branch %+v %v", bs, err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), bs[0].ID) })
	if !hasRules(bs[0].ID) || !hasRules(d.ID) {
		t.Fatalf("source and branch each need rules:\n%s", ruleList())
	}
	prepare("e2ehb-branch")
	blocked("e2ehb-branch", bs[0].ID, "in the branch, as root", "sudo ")
	// The branch's address on its own network is not reachable from the source (Docker isolates the networks).
	prepare("e2ehb") // the branch stopped and resumed the source, which cleared its /tmp
	out, _, err := g.docker(ctx, nil, "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", gvContainer(bs[0].ID))
	if bip := strings.TrimSpace(out); err != nil || bip == "" {
		t.Fatalf("branch address: %q %v", out, err)
	} else if res := run("e2ehb", "python3 /tmp/probe.py tcp,other-dock,"+bip+",2222"); !strings.Contains(res.Stdout, "other-dock tcp "+bip+" 2222 BLOCKED") {
		t.Fatalf("a dock must not reach another dock's network: %+v", res)
	}
	if err := e.Remove(ctx, "e2ehb-branch"); err != nil {
		t.Fatal(err)
	}
	noTrace(bs[0].ID)
	if !hasRules(d.ID) {
		t.Fatal("removing the branch must keep the source's rules")
	}

	if err := e.Remove(ctx, "e2ehb"); err != nil {
		t.Fatal(err)
	}
	noTrace(d.ID)
	if out, _, _ := g.docker(ctx, nil, "network", "ls", "--filter", "name="+gvNetwork(d.ID), "--format", "{{.Name}}"); strings.TrimSpace(out) != "" {
		t.Fatalf("rm must remove the network: %q", out)
	}
}
