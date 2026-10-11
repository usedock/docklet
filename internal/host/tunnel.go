package host

import (
	"context"
	"net"

	"github.com/usedock/docklet/internal/guestagent"
)

var (
	// ErrTunnelUnavailable marks a guest agent that predates the tcp op.
	ErrTunnelUnavailable = guestagent.ErrTunnelUnavailable
	// ErrPortClosed means nothing in the guest accepted a connection on the port.
	ErrPortClosed = guestagent.ErrPortClosed
)

// OpenPort opens one TCP stream to a port on the guest's loopback through its agent.
func (h *Host) OpenPort(ctx context.Context, dockID string, port int) (net.Conn, error) {
	v, err := h.fastVM(dockID)
	if err != nil {
		return nil, err
	}
	conn, err := h.agentDial()(ctx, guestagent.Addr(v.guestIP))
	if err != nil {
		return nil, err
	}
	tunnel, err := guestagent.NewTunnel(conn, v.fastToken, port)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return tunnel, nil
}
