package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/usedock/docklet/internal/ids"

	"golang.org/x/net/websocket"
)

const (
	terminalOp = "terminal.open"
	// maxConcurrentTerminals bounds live shells relayed by this host.
	maxConcurrentTerminals = 64
	terminalDialTimeout    = 15 * time.Second
	// Mirrors host.MaxTerminalFrame plus the kind byte.
	maxTerminalMessage = 64<<10 + 1
)

// Terminal frame kinds relayed between the guest and the control plane.
const (
	terminalData   byte = 0
	terminalResize byte = 1
	terminalExit   byte = 2
)

// TerminalStream is one guest shell spoken to in frames.
type TerminalStream interface {
	ReadFrame() (byte, []byte, error)
	WriteFrame(kind byte, payload []byte) error
	Close() error
}

// TerminalOpener starts a guest shell for a dock on this host.
type TerminalOpener func(ctx context.Context, dockID string, cols, rows int) (TerminalStream, error)

type terminalOpen struct {
	DockID    string `json:"dockId"`
	SessionID string `json:"sessionId"`
	Secret    string `json:"secret"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}

// openTerminal starts the guest shell, dials the control plane's relay for the
// session, and reports the command once both legs are up. The bridge then runs
// until either side hangs up.
func (c Config) openTerminal(ctx context.Context, cmd command) {
	fail := func(code, message string) {
		c.post(ctx, cmd, resultRequest{HostID: c.HostID, Ok: false, Code: code, Message: message})
	}
	if c.OpenTerminal == nil {
		fail("terminal_unavailable", "this host does not serve terminals")
		return
	}
	var req terminalOpen
	if err := json.Unmarshal(cmd.Payload, &req); err != nil || !ids.Valid(req.DockID) || req.SessionID == "" || req.Secret == "" {
		fail("host_error", "terminal.open needs dockId, sessionId, and secret")
		return
	}
	guest, err := c.OpenTerminal(ctx, req.DockID, req.Cols, req.Rows)
	if err != nil {
		fail("terminal_unavailable", err.Error())
		return
	}
	relay, err := c.dialRelay(ctx, "/v1/fleet/terminals/"+url.PathEscape(req.SessionID), "X-Terminal-Secret", req.Secret)
	if err != nil {
		guest.Close()
		fail("host_unreachable", err.Error())
		return
	}
	c.post(ctx, cmd, resultRequest{HostID: c.HostID, Ok: true})
	bridgeTerminal(ctx, guest, relay)
}

// dialRelay opens the control plane's WebSocket for one session, proving it with the session secret.
func (c Config) dialRelay(ctx context.Context, path, secretHeader, secret string) (*websocket.Conn, error) {
	location, err := url.Parse(c.FleetURL + path)
	if err != nil {
		return nil, err
	}
	switch location.Scheme {
	case "https":
		location.Scheme = "wss"
	case "http":
		location.Scheme = "ws"
	default:
		return nil, errors.New("fleet url must be http or https")
	}
	origin, err := url.Parse(c.FleetURL)
	if err != nil {
		return nil, err
	}
	config, err := websocket.NewConfig(location.String(), origin.String())
	if err != nil {
		return nil, err
	}
	config.Header = http.Header{
		"Authorization": {"Bearer " + c.bearer()},
		// Newer backends ignore this; kept so docklets can be upgraded before the backend.
		"X-Host-Id":  {c.HostID},
		secretHeader: {secret},
	}
	ctx, cancel := context.WithTimeout(ctx, terminalDialTimeout)
	defer cancel()
	conn, err := config.DialContext(ctx)
	if err != nil {
		return nil, err
	}
	conn.MaxPayloadBytes = maxTerminalMessage
	conn.PayloadType = websocket.BinaryFrame
	return conn, nil
}

// bridgeTerminal relays frames both ways until either side or ctx ends; the relay
// may only send keystrokes and resizes.
func bridgeTerminal(ctx context.Context, guest TerminalStream, relay *websocket.Conn) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			guest.Close()
			relay.Close()
		})
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer closeBoth()
		for {
			kind, payload, err := guest.ReadFrame()
			if err != nil {
				return
			}
			if err := websocket.Message.Send(relay, append([]byte{kind}, payload...)); err != nil {
				return
			}
			if kind == terminalExit {
				return
			}
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			closeBoth()
		case <-done:
		}
	}()
	defer func() { <-done }()
	defer closeBoth()
	for {
		var message []byte
		if err := websocket.Message.Receive(relay, &message); err != nil || len(message) == 0 {
			return
		}
		kind := message[0]
		if kind != terminalData && kind != terminalResize {
			return
		}
		if err := guest.WriteFrame(kind, message[1:]); err != nil {
			return
		}
	}
}
