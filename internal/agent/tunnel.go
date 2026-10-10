package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"sync"

	"github.com/usedock/docklet/internal/ids"

	"golang.org/x/net/websocket"
)

const (
	tunnelOp = "tunnel.open"
	// maxConcurrentTunnels bounds live port streams relayed by this host.
	maxConcurrentTunnels = 512
	// tunnelChunk bounds one relayed message, within the relay's frame limit.
	tunnelChunk = 32 << 10
)

// TunnelOpener opens one TCP stream to a port inside a dock on this host.
type TunnelOpener func(ctx context.Context, dockID string, port int) (net.Conn, error)

// TunnelError carries the result code a failed tunnel open reports.
type TunnelError struct {
	Code string
	Err  error
}

func (e *TunnelError) Error() string { return e.Err.Error() }
func (e *TunnelError) Unwrap() error { return e.Err }

type tunnelOpen struct {
	DockID    string `json:"dockId"`
	SessionID string `json:"sessionId"`
	Secret    string `json:"secret"`
	Port      int    `json:"port"`
}

// openTunnel connects to the guest port, dials the control plane's relay for
// the session, and reports the command once both legs are up. Bytes then flow
// untouched until either side closes.
func (c Config) openTunnel(ctx context.Context, cmd command) {
	fail := func(code, message string) {
		c.post(ctx, cmd, resultRequest{HostID: c.HostID, Ok: false, Code: code, Message: message})
	}
	if c.OpenTunnel == nil {
		fail("tunnel_unavailable", "this host does not serve port tunnels")
		return
	}
	var req tunnelOpen
	if err := json.Unmarshal(cmd.Payload, &req); err != nil || !ids.Valid(req.DockID) || req.SessionID == "" || req.Secret == "" || req.Port < 1 || req.Port > 65535 {
		fail("host_error", "tunnel.open needs dockId, sessionId, secret, and port")
		return
	}
	guest, err := c.OpenTunnel(ctx, req.DockID, req.Port)
	if err != nil {
		code := "tunnel_failed"
		var tunnelErr *TunnelError
		if errors.As(err, &tunnelErr) {
			code = tunnelErr.Code
		}
		fail(code, err.Error())
		return
	}
	relay, err := c.dialRelay(ctx, "/v1/fleet/tunnels/"+url.PathEscape(req.SessionID), "X-Tunnel-Secret", req.Secret)
	if err != nil {
		guest.Close()
		fail("host_unreachable", err.Error())
		return
	}
	c.post(ctx, cmd, resultRequest{HostID: c.HostID, Ok: true})
	bridgeTunnel(ctx, guest, relay)
}

// bridgeTunnel relays raw bytes as binary messages; the first side to finish closes both.
func bridgeTunnel(ctx context.Context, guest net.Conn, relay *websocket.Conn) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			guest.Close()
			relay.Close()
		})
	}
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, tunnelChunk)
		for {
			n, err := guest.Read(buf)
			if n > 0 {
				if websocket.Message.Send(relay, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			var message []byte
			if websocket.Message.Receive(relay, &message) != nil {
				return
			}
			if _, err := guest.Write(message); err != nil {
				return
			}
		}
	}()
	select {
	case <-ctx.Done():
	case <-done:
	}
	closeBoth()
	<-done
}
