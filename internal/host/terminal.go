package host

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/usedock/docklet/internal/egress"
)

// Terminal frame kinds; the guest agent, the fleet relay, and the browser share them.
const (
	TerminalData   byte = 0
	TerminalResize byte = 1
	TerminalExit   byte = 2
)

// MaxTerminalFrame bounds one frame payload in either direction.
const MaxTerminalFrame = 64 << 10

const terminalHandshakeTimeout = 10 * time.Second

// ErrTerminalUnavailable marks a guest agent that predates the pty op.
var ErrTerminalUnavailable = errors.New("terminal_unavailable")

// Terminal is one login shell on a pty in a dock's guest, spoken to in frames.
type Terminal struct {
	conn net.Conn
	r    *bufio.Reader
}

// OpenTerminal starts a login shell as the dock user; closing the terminal hangs it up.
func (h *Host) OpenTerminal(ctx context.Context, dockID string, cols, rows int) (*Terminal, error) {
	v, err := h.fastVM(dockID)
	if err != nil {
		return nil, err
	}
	var env map[string]string
	if v.egressGateway != "" {
		env = egress.ProxyEnv(v.egressGateway, h.cfg.EgressPort)
	}
	conn, err := h.agentDial()(ctx, net.JoinHostPort(v.guestIP, fmt.Sprint(fastAgentPort)))
	if err != nil {
		return nil, err
	}
	t, err := handshakeTerminal(conn, fastAgentRequest{Token: v.fastToken, Op: "pty", Cols: cols, Rows: rows, Env: env})
	if err != nil {
		conn.Close()
		return nil, err
	}
	return t, nil
}

func handshakeTerminal(conn net.Conn, request fastAgentRequest) (*Terminal, error) {
	r, err := handshakeAgentStream(conn, request)
	if err != nil {
		var refused agentRefusal
		if errors.As(err, &refused) && refused == "unsupported_operation" {
			return nil, fmt.Errorf("%w: guest agent predates terminals", ErrTerminalUnavailable)
		}
		return nil, fmt.Errorf("guest terminal: %w", err)
	}
	return &Terminal{conn: conn, r: r}, nil
}

// agentRefusal is the error code a guest agent answered a stream handshake with.
type agentRefusal string

func (e agentRefusal) Error() string { return string(e) }

// handshakeAgentStream sends one request line and reads the guest's verdict; the
// returned reader holds any stream bytes that arrived with it.
func handshakeAgentStream(conn net.Conn, request fastAgentRequest) (*bufio.Reader, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(terminalHandshakeTimeout)); err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(conn, MaxTerminalFrame)
	line, err := r.ReadSlice('\n')
	if err != nil {
		return nil, fmt.Errorf("handshake: %w", err)
	}
	var response fastAgentResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return nil, fmt.Errorf("handshake: %w", err)
	}
	if !response.OK {
		return nil, agentRefusal(response.Error)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	return r, nil
}

// ReadFrame returns the next guest frame: shell output or its exit status.
func (t *Terminal) ReadFrame() (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(t.r, header[:]); err != nil {
		return 0, nil, err
	}
	size := binary.BigEndian.Uint32(header[1:])
	if size > MaxTerminalFrame {
		return 0, nil, fmt.Errorf("guest terminal frame of %d bytes exceeds limit", size)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(t.r, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

// WriteFrame sends keystrokes or a resize to the guest shell.
func (t *Terminal) WriteFrame(kind byte, payload []byte) error {
	if len(payload) > MaxTerminalFrame {
		return fmt.Errorf("terminal frame of %d bytes exceeds limit", len(payload))
	}
	frame := make([]byte, 5+len(payload))
	frame[0] = kind
	binary.BigEndian.PutUint32(frame[1:], uint32(len(payload)))
	copy(frame[5:], payload)
	_, err := t.conn.Write(frame)
	return err
}

func (t *Terminal) Close() error { return t.conn.Close() }

// agentDial opens raw guest-agent connections, overridable in tests.
func (h *Host) agentDial() func(ctx context.Context, addr string) (net.Conn, error) {
	if h.cfg.AgentDial != nil {
		return h.cfg.AgentDial
	}
	return func(ctx context.Context, addr string) (net.Conn, error) {
		dialer := net.Dialer{Timeout: time.Second}
		return dialer.DialContext(ctx, "tcp", addr)
	}
}
