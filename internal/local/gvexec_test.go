package local

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/usedock/docklet/internal/guestagent"
)

// pipeExec is a fake docker exec: what the host writes comes out of fromHost, and what the test
// writes to toHost is what the host reads.
func pipeExec() (p *ExecPipe, fromHost *io.PipeReader, toHost *io.PipeWriter, closed *bool) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	closed = new(bool)
	return &ExecPipe{In: inW, Out: outR, Close: func() error { *closed = true; _ = outW.Close(); return nil }}, inR, outW, closed
}

func TestExecConnFramesBothWaysAndHalfCloses(t *testing.T) {
	p, fromHost, toHost, closed := pipeExec()
	c := newExecConn(p)
	go func() {
		defer toHost.Close()
		got, _ := io.ReadAll(fromHost) // returns once the host half-closes
		_, _ = toHost.Write(append([]byte("echo:"), got...))
	}()
	if _, err := c.Write([]byte("req\n")); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(c)
	if err != nil || string(out) != "echo:req\n" {
		t.Fatalf("read %q %v", out, err)
	}
	if err := c.Close(); err != nil || !*closed {
		t.Fatalf("close must end the exec: %v %v", err, *closed)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestExecConnDeadlineClosesTheStream(t *testing.T) {
	p, _, _, closed := pipeExec()
	c := newExecConn(p)
	_ = c.SetDeadline(time.Now().Add(50 * time.Millisecond))
	_, err := c.Read(make([]byte, 1))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read after the deadline: %v", err)
	}
	c.mu.Lock()
	expired := c.expired
	c.mu.Unlock()
	if !expired {
		t.Fatal("the deadline must have fired")
	}
	_ = closed
	// A cleared deadline never fires.
	p2, _, toHost, _ := pipeExec()
	c2 := newExecConn(p2)
	_ = c2.SetDeadline(time.Now().Add(50 * time.Millisecond))
	_ = c2.SetDeadline(time.Time{})
	time.Sleep(150 * time.Millisecond)
	go func() { _, _ = toHost.Write([]byte("x")) }()
	b := make([]byte, 1)
	if _, err := c2.Read(b); err != nil || c2.timedOut() {
		t.Fatalf("cleared deadline: %v", err)
	}
}

// The agent protocol runs unchanged over the exec stream: Call, a pty handshake and a tunnel.
func TestExecDialSpeaksTheAgentProtocol(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	d, err := e.New(ctx, Spec{Name: "off", Runtime: RuntimeGVisor, Network: "off"})
	if err != nil {
		t.Fatal(err)
	}
	rec := mustLookup(t, e, "off")
	tok, _ := e.token(d.ID)
	res, err := guestagent.CallDial(ctx, e.dial(rec), tok, guestagent.Request{Op: "exec", Script: "echo hi", TimeoutSeconds: 5})
	if err != nil || res.Stdout != "ran:echo hi" {
		t.Fatalf("call %+v %v", res, err)
	}
	if _, err := e.OpenPort(ctx, "off", 8080); !errors.Is(err, guestagent.ErrTunnelUnavailable) {
		t.Fatalf("a port open must reach the agent (the fake lacks the tcp op): %v", err)
	}
	if _, err := e.OpenTerminal(ctx, "off", 80, 24); !errors.Is(err, guestagent.ErrTerminalUnavailable) {
		t.Fatalf("a terminal open must reach the agent: %v", err)
	}
	if len(fd.streams) < 3 {
		t.Fatalf("exec, port and ssh must each go through docker exec: %v", fd.streams)
	}
	for _, s := range fd.streams {
		if len(s) != 6 || s[0] != "exec" || s[1] != "-i" || s[2] != "dock-local-"+d.ID || s[3] != "python3" || s[4] != "-c" || s[5] != gvRelay {
			t.Fatalf("stream argv %q", s)
		}
	}
}

// The relay program itself, run with a local python against a TCP server in place of the agent.
func TestRelayProgramPipesBinaryBothWaysWithHalfClose(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		if py, err = exec.LookPath("python"); err != nil {
			t.Skip("no python on this machine")
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		got, _ := io.ReadAll(conn) // the relay half-closes after stdin EOF
		_, _ = conn.Write(append([]byte("reply:"), got...))
	}()
	port := l.Addr().(*net.TCPAddr).Port
	prog := strings.Replace(gvRelay, strconv.Itoa(guestagent.Port), strconv.Itoa(port), 1)
	cmd := exec.Command(py, "-c", prog)
	payload := bytes.Repeat([]byte{0, 1, 2, 10, 0xff, 13}, 40000) // binary and larger than one pipe buffer
	cmd.Stdin = bytes.NewReader(payload)
	done := make(chan struct{})
	var out []byte
	go func() {
		out, err = cmd.Output()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("relay did not finish")
	}
	if err != nil || !bytes.Equal(out, append([]byte("reply:"), payload...)) {
		t.Fatalf("relay output %d bytes, err %v", len(out), err)
	}
}

func TestNetworkOffGVisorArgvHasNoNetworkAndNoPort(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	d, err := e.New(ctx, Spec{Name: "off", Runtime: RuntimeGVisor, Network: "off"})
	if err != nil || d.State != "ready" {
		t.Fatalf("new %+v %v", d, err)
	}
	argv := strings.Join(fd.callsWith("run")[0], " ")
	if !strings.Contains(argv, "--network none") {
		t.Fatalf("argv: %s", argv)
	}
	for _, bad := range []string{" -p ", "-net ", "resolv.conf", "--privileged"} {
		if strings.Contains(argv, bad) {
			t.Fatalf("argv must not contain %q: %s", bad, argv)
		}
	}
	for _, c := range fd.calls {
		if c[0] == "network" {
			t.Fatalf("a network-off dock must never touch docker networks: %v", c)
		}
	}
	if _, err := os.Stat(e.file(d.ID, "port")); err == nil {
		t.Fatal("a network-off dock publishes no port, so it records none")
	}
	rec := mustLookup(t, e, "off")
	if rec.Network != "off" || rec.runtime() != RuntimeGVisor {
		t.Fatalf("record %+v", rec)
	}
	res, err := e.Exec(ctx, "off", "echo hi", 0)
	if err != nil || !strings.Contains(res.Stdout, "ran:echo hi") {
		t.Fatalf("exec %+v %v", res, err)
	}
	if len(fd.streams) == 0 {
		t.Fatal("exec must go through docker exec")
	}
}

func TestNetworkOffGVisorStopResumeRemoveSkipNetworks(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	d, err := e.New(ctx, Spec{Name: "off", Runtime: RuntimeGVisor, Network: "off"})
	if err != nil {
		t.Fatal(err)
	}
	if st, err := e.Stop(ctx, "off", false); err != nil || st.State != "archived" {
		t.Fatalf("stop %+v %v", st, err)
	}
	if _, err := e.Exec(ctx, "off", "x", 0); codeOf(err) != "dock_not_running" {
		t.Fatalf("exec on stopped: %v", err)
	}
	rd, err := e.Resume(ctx, "off")
	if err != nil || rd.State != "ready" {
		t.Fatalf("resume %+v %v", rd, err)
	}
	if n := len(fd.callsWith("run")); n != 1 {
		t.Fatalf("resume must docker start, not run: %d runs", n)
	}
	if res, err := e.Exec(ctx, "off", "again", 0); err != nil || !strings.Contains(res.Stdout, "again") {
		t.Fatalf("exec after resume %+v %v", res, err)
	}
	if err := e.Remove(ctx, "off"); err != nil {
		t.Fatal(err)
	}
	for _, c := range fd.calls {
		if c[0] == "network" {
			t.Fatalf("no network verb expected: %v", c)
		}
	}
	if len(fd.containers) != 0 || fd.volumes["dock-local-"+d.ID+"-work"] {
		t.Fatalf("rm leftovers: %v %v", fd.containers, fd.volumes)
	}
}

func TestNetworkOffGVisorBootFailureAndLostContainer(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	fd.exitOnRun = true
	if _, err := e.New(ctx, Spec{Runtime: RuntimeGVisor, Network: "off"}); codeOf(err) != "boot_failed" {
		t.Fatalf("exit during boot: %v", err)
	}
	if recs, _ := e.store().List(); len(recs) != 0 || len(fd.containers) != 0 {
		t.Fatalf("leftovers: %v %v", recs, fd.containers)
	}
	fd.exitOnRun = false
	if _, err := e.New(ctx, Spec{Name: "off", Runtime: RuntimeGVisor, Network: "off"}); err != nil {
		t.Fatal(err)
	}
	// docker prune removed the container: resume recreates it, still without a network.
	if _, err := e.Stop(ctx, "off", true); err != nil {
		t.Fatal(err)
	}
	id := "dock-local-" + mustLookup(t, e, "off").ID
	delete(fd.containers, id)
	delete(fd.stopped, id)
	if d, err := e.Resume(ctx, "off"); err != nil || d.State != "ready" {
		t.Fatalf("resume of a lost container %+v %v", d, err)
	}
	runs := fd.callsWith("run")
	if last := strings.Join(runs[len(runs)-1], " "); !strings.Contains(last, "--network none") || strings.Contains(last, " -p ") {
		t.Fatalf("recreated container: %s", last)
	}
}

func mustLookup(t *testing.T, e *Engine, ref string) Record {
	t.Helper()
	r, err := e.Lookup(ref)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNetworkOffGVisorBranchKeepsNetworkOff(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	src, err := e.New(ctx, Spec{Name: "off", Runtime: RuntimeGVisor, Network: "off"})
	if err != nil {
		t.Fatal(err)
	}
	bs, err := e.Branch(ctx, "off", 2, "", Spec{})
	if err != nil || len(bs) != 2 {
		t.Fatalf("branch %+v %v", bs, err)
	}
	for _, b := range bs {
		rec := mustLookup(t, e, b.ID)
		if rec.Network != "off" || rec.Parent != src.ID {
			t.Fatalf("branch record %+v", rec)
		}
		if res, err := e.Exec(ctx, b.ID, "hi", 0); err != nil || !strings.Contains(res.Stdout, "ran:hi") {
			t.Fatalf("exec in branch %+v %v", res, err)
		}
	}
	for _, c := range fd.calls {
		if c[0] == "network" {
			t.Fatalf("branching a network-off dock must not touch networks: %v", c)
		}
	}
	for _, c := range fd.callsWith("run") {
		if !slices.Contains(c, "--rm") {
			if a := strings.Join(c, " "); !strings.Contains(a, "--network none") || strings.Contains(a, " -p ") {
				t.Fatalf("branch run: %s", a)
			}
		}
	}
	if st, _ := e.Get(ctx, "off"); st.State != "ready" {
		t.Fatalf("source must be resumed: %s", st.State)
	}
}

// A network-on dock keeps the TCP dial and its own network; no exec stream is ever used.
func TestNetworkOnDocksKeepTCPDial(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	if _, err := e.New(ctx, Spec{Name: "on", Runtime: RuntimeGVisor}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Exec(ctx, "on", "hi", 0); err != nil {
		t.Fatal(err)
	}
	if len(fd.streams) != 0 {
		t.Fatalf("network-on docks use the published port: %v", fd.streams)
	}
	// A QEMU dock with network off still dials its forwarded port.
	q, drv, _ := newTestEngine(t)
	if _, err := q.New(ctx, Spec{Name: "q", Network: "off"}); err != nil {
		t.Fatal(err)
	}
	if res, err := q.Exec(ctx, "q", "hi", 0); err != nil || !strings.Contains(res.Stdout, "ran:hi") {
		t.Fatalf("qemu exec %+v %v", res, err)
	}
	if len(drv.starts) != 1 || drv.starts[0].Network != "off" {
		t.Fatalf("qemu launch %+v", drv.starts)
	}
}

func TestNetworkOffRemoveWithoutDockerDoesNotNameANetwork(t *testing.T) {
	ctx := context.Background()
	e, _ := newGVisorEngine(t)
	d, err := e.New(ctx, Spec{Name: "off", Runtime: RuntimeGVisor, Network: "off"})
	if err != nil {
		t.Fatal(err)
	}
	e.GVisor.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	var progress strings.Builder
	e.Progress = &progress
	if err := e.Remove(ctx, "off"); err != nil {
		t.Fatal(err)
	}
	out := progress.String()
	if !strings.Contains(out, "dock-local-"+d.ID+" ") || !strings.Contains(out, "dock-local-"+d.ID+"-work") || strings.Contains(out, "-net") {
		t.Fatalf("warning must list the container and volume only: %q", out)
	}
}

// A docker exec that refuses (paused or just-stopped container) says why instead of a bare EOF,
// but only when nothing came through the stream.
func TestExecConnReportsDockerStderrOnlyBeforeAnyData(t *testing.T) {
	refused := func(data string) *execConn {
		outR, outW := io.Pipe()
		go func() {
			if data != "" {
				_, _ = outW.Write([]byte(data))
			}
			_ = outW.Close()
		}()
		return newExecConn(&ExecPipe{In: nopWriteCloser{io.Discard}, Out: outR, Close: func() error { return nil },
			Stderr: func() string { return "Error response from daemon: container is paused" }})
	}
	_, err := io.ReadAll(refused(""))
	if err == nil || !strings.Contains(err.Error(), "container is paused") {
		t.Fatalf("want docker's message, got %v", err)
	}
	c := refused("data")
	b := make([]byte, 4)
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(b); err != io.EOF {
		t.Fatalf("after data the plain EOF must stay: %v", err)
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func TestTailBufferKeepsTheEnd(t *testing.T) {
	var tb tailBuffer
	_, _ = tb.Write(bytes.Repeat([]byte("a"), stderrTailBytes))
	_, _ = tb.Write([]byte("END\n"))
	if s := tb.String(); len(s) > stderrTailBytes || !strings.HasSuffix(s, "END") {
		t.Fatalf("tail %d bytes", len(s))
	}
}

// A relay that cannot reach the agent prints a Python traceback; the user sees its last line
// labelled as an unreachable agent, while docker's own refusals keep the docker label.
func TestExecConnStderrIsOneAccurateLine(t *testing.T) {
	traceback := "Traceback (most recent call last):\n  File \"<string>\", line 2, in <module>\n  File \"/usr/lib/python3.12/socket.py\", line 852, in create_connection\n    raise exceptions[0]\nConnectionRefusedError: [Errno 111] Connection refused\n\n"
	for _, tc := range []struct{ stderr, want string }{
		{traceback, "guest agent unreachable: ConnectionRefusedError: [Errno 111] Connection refused"},
		{"Error response from daemon: container is paused\n", "docker exec failed: Error response from daemon: container is paused"},
		{"warning: x\nError: No such container: dock-local-x\n", "docker exec failed: Error: No such container: dock-local-x"},
	} {
		err := stderrError(tc.stderr)
		if err.Error() != tc.want || strings.Contains(err.Error(), "\n") || strings.Contains(err.Error(), "Traceback") {
			t.Errorf("stderr %q: got %q, want %q", tc.stderr, err, tc.want)
		}
	}
}
