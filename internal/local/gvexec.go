package local

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/usedock/docklet/internal/guestagent"
)

// gvRelay runs inside a network-off dock (python3 is in the image): it connects to the guest
// agent on the container's loopback and pipes stdin and stdout through it, both ways. EOF on
// stdin half-closes the socket, so request and response framing work like on a TCP connection.
var gvRelay = `import os,socket,sys,threading
s=socket.create_connection(("127.0.0.1",` + strconv.Itoa(guestagent.Port) + `))
def up():
    try:
        while True:
            d=os.read(0,65536)
            if not d:
                break
            s.sendall(d)
    except OSError:
        pass
    try:
        s.shutdown(socket.SHUT_WR)
    except OSError:
        pass
threading.Thread(target=up,daemon=True).start()
o=sys.stdout.buffer
while True:
    d=s.recv(65536)
    if not d:
        break
    o.write(d)
    o.flush()
`

// ExecPipe is a running docker command used as a byte stream.
type ExecPipe struct {
	// In is the command's stdin; closing it half-closes the stream.
	In io.WriteCloser
	// Out is the command's stdout.
	Out io.Reader
	// Close ends the command.
	Close func() error
}

// DockerStream starts the docker CLI with args and returns its stdin and stdout as a stream.
type DockerStream func(ctx context.Context, args ...string) (*ExecPipe, error)

func execStream(_ context.Context, args ...string) (*ExecPipe, error) {
	// Not CommandContext: the stream outlives a dial, and its owner closes it.
	cmd := exec.Command("docker", args...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var once sync.Once
	return &ExecPipe{In: in, Out: out, Close: func() error {
		once.Do(func() {
			_ = in.Close()
			_ = cmd.Process.Kill()
			go func() { _ = cmd.Wait() }()
		})
		return nil
	}}, nil
}

// execDial opens the agent of a network-off container through docker exec.
func (g *GVisor) execDial(container string) guestagent.DialFunc {
	return func(ctx context.Context) (net.Conn, error) {
		start := g.Stream
		if start == nil {
			start = execStream
		}
		p, err := start(ctx, "exec", "-i", container, "python3", "-c", gvRelay)
		if err != nil {
			return nil, err
		}
		return newExecConn(p), nil
	}
}

// execConn adapts an ExecPipe to net.Conn. A deadline that expires closes the stream.
type execConn struct {
	p *ExecPipe

	mu      sync.Mutex
	timer   *time.Timer
	expired bool
	closed  sync.Once
}

func newExecConn(p *ExecPipe) *execConn { return &execConn{p: p} }

func (c *execConn) Read(b []byte) (int, error) {
	n, err := c.p.Out.Read(b)
	if err != nil && c.timedOut() {
		err = os.ErrDeadlineExceeded
	}
	return n, err
}

func (c *execConn) Write(b []byte) (int, error) {
	n, err := c.p.In.Write(b)
	if err != nil && c.timedOut() {
		err = os.ErrDeadlineExceeded
	}
	return n, err
}

// CloseWrite half-closes the stream: the relay then shuts down the socket's write side.
func (c *execConn) CloseWrite() error { return c.p.In.Close() }

func (c *execConn) Close() error {
	var err error
	c.closed.Do(func() {
		c.mu.Lock()
		if c.timer != nil {
			c.timer.Stop()
		}
		c.mu.Unlock()
		err = c.p.Close()
	})
	return err
}

func (c *execConn) timedOut() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.expired
}

// SetDeadline arms (or with the zero time clears) one timer for both directions.
func (c *execConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if t.IsZero() {
		return nil
	}
	c.timer = time.AfterFunc(time.Until(t), func() {
		c.mu.Lock()
		c.expired = true
		c.mu.Unlock()
		_ = c.Close()
	})
	return nil
}

func (c *execConn) SetReadDeadline(t time.Time) error  { return c.SetDeadline(t) }
func (c *execConn) SetWriteDeadline(t time.Time) error { return c.SetDeadline(t) }

type execAddr struct{}

func (execAddr) Network() string { return "docker-exec" }
func (execAddr) String() string  { return "docker-exec:" + strconv.Itoa(guestagent.Port) }

func (c *execConn) LocalAddr() net.Addr  { return execAddr{} }
func (c *execConn) RemoteAddr() net.Addr { return execAddr{} }
