package host

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
)

var (
	// ErrTunnelUnavailable marks a guest agent that predates the tcp op.
	ErrTunnelUnavailable = errors.New("tunnel_unavailable")
	// ErrPortClosed means nothing in the guest accepted a connection on the port.
	ErrPortClosed = errors.New("port_closed")
)

// tunnelConn reads through the handshake buffer so bytes a server sends first are kept.
type tunnelConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *tunnelConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// OpenPort opens one TCP stream to a port on the guest's loopback through its agent.
func (h *Host) OpenPort(ctx context.Context, dockID string, port int) (net.Conn, error) {
	v, err := h.fastVM(dockID)
	if err != nil {
		return nil, err
	}
	conn, err := h.agentDial()(ctx, net.JoinHostPort(v.guestIP, fmt.Sprint(fastAgentPort)))
	if err != nil {
		return nil, err
	}
	r, err := handshakeAgentStream(conn, fastAgentRequest{Token: v.fastToken, Op: "tcp", Port: port})
	if err != nil {
		conn.Close()
		var refused agentRefusal
		if errors.As(err, &refused) {
			switch refused {
			case "unsupported_operation":
				return nil, fmt.Errorf("%w: guest agent predates port tunnels", ErrTunnelUnavailable)
			case "port_closed":
				return nil, fmt.Errorf("%w: nothing is listening on port %d", ErrPortClosed, port)
			}
		}
		return nil, fmt.Errorf("guest tunnel: %w", err)
	}
	return &tunnelConn{Conn: conn, r: r}, nil
}
