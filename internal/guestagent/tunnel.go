package guestagent

import (
	"bufio"
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

// NewTunnel performs the tcp handshake on conn and returns a stream to the guest port.
// On error the caller closes conn.
func NewTunnel(conn net.Conn, token string, port int) (net.Conn, error) {
	r, err := Handshake(conn, Request{Token: token, Op: "tcp", Port: port})
	if err != nil {
		switch {
		case IsRefusal(err, "unsupported_operation"):
			return nil, fmt.Errorf("%w: guest agent predates port tunnels", ErrTunnelUnavailable)
		case IsRefusal(err, "port_closed"):
			return nil, fmt.Errorf("%w: nothing is listening on port %d", ErrPortClosed, port)
		}
		return nil, fmt.Errorf("guest tunnel: %w", err)
	}
	return &tunnelConn{Conn: conn, r: r}, nil
}
