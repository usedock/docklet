package host

import (
	"bufio"
	"context"
	"net"

	"github.com/usedock/docklet/internal/egress"
	"github.com/usedock/docklet/internal/guestagent"
)

// Terminal frame kinds; the guest agent, the fleet relay, and the browser share them.
const (
	TerminalData   = guestagent.TerminalData
	TerminalResize = guestagent.TerminalResize
	TerminalExit   = guestagent.TerminalExit
)

// MaxTerminalFrame bounds one frame payload in either direction.
const MaxTerminalFrame = guestagent.MaxTerminalFrame

// ErrTerminalUnavailable marks a guest agent that predates the pty op.
var ErrTerminalUnavailable = guestagent.ErrTerminalUnavailable

// Terminal is one login shell on a pty in a dock's guest, spoken to in frames.
type Terminal = guestagent.Terminal

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
	conn, err := h.agentDial()(ctx, guestagent.Addr(v.guestIP))
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
	return guestagent.NewTerminal(conn, request)
}

// agentRefusal is the error code a guest agent answered a stream handshake with.
type agentRefusal = guestagent.Refusal

func handshakeAgentStream(conn net.Conn, request fastAgentRequest) (*bufio.Reader, error) {
	return guestagent.Handshake(conn, request)
}

// agentDial opens raw guest-agent connections, overridable in tests.
func (h *Host) agentDial() func(ctx context.Context, addr string) (net.Conn, error) {
	if h.cfg.AgentDial != nil {
		return h.cfg.AgentDial
	}
	return guestagent.Dial
}
