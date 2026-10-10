package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// tunnelAgent wires an agent whose relay is served by relay and whose guest port is guest.
func tunnelAgent(t *testing.T, relay http.HandlerFunc, guest net.Conn, openErr error) (Config, *fakeTransport, *[]int) {
	t.Helper()
	server := httptest.NewServer(relay)
	t.Cleanup(server.Close)
	var ports []int
	transport := &fakeTransport{handler: func(recorded) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"ok":true,"data":{"accepted":true}}`), nil
	}}
	c := Config{
		FleetURL: server.URL,
		Token:    "fleet-token",
		HostID:   "host-1",
		Client:   transport,
		Logf:     func(string, ...any) {},
		OpenTunnel: func(_ context.Context, dockID string, port int) (net.Conn, error) {
			ports = append(ports, port)
			if openErr != nil {
				return nil, openErr
			}
			if dockID != "dock-1" {
				t.Errorf("dock: got %q", dockID)
			}
			return guest, nil
		},
	}
	return c, transport, &ports
}

func tunnelCommand() command {
	payload, _ := json.Marshal(tunnelOpen{DockID: "dock-1", SessionID: "sess-1", Secret: "s3cret", Port: 3000})
	return command{ID: "cmd-1", Op: tunnelOp, IdempotencyToken: "tunnel:sess-1", Payload: payload}
}

func TestTunnelBridgesGuestPortAndRelay(t *testing.T) {
	guest, guestEnd := net.Pipe()
	headers := make(chan http.Header, 1)
	fromGuest := make(chan []byte, 1)
	relay := websocket.Server{Handler: func(ws *websocket.Conn) {
		if ws.Request().URL.Path != "/v1/fleet/tunnels/sess-1" {
			t.Errorf("relay path: %s", ws.Request().URL.Path)
		}
		headers <- ws.Request().Header.Clone()
		_ = websocket.Message.Send(ws, []byte("GET / HTTP/1.1\r\n"))
		var message []byte
		if err := websocket.Message.Receive(ws, &message); err == nil {
			fromGuest <- message
		}
	}}
	go func() {
		line, _ := bufio.NewReader(guestEnd).ReadString('\n')
		if line != "GET / HTTP/1.1\r\n" {
			t.Errorf("guest got %q", line)
		}
		_, _ = guestEnd.Write([]byte("HTTP/1.1 200 OK\r\n"))
	}()
	c, transport, ports := tunnelAgent(t, relay.ServeHTTP, guest, nil)
	c.openTunnel(context.Background(), tunnelCommand())

	header := <-headers
	if header.Get("Authorization") != "Bearer fleet-token" || header.Get("X-Host-Id") != "host-1" || header.Get("X-Tunnel-Secret") != "s3cret" {
		t.Fatalf("relay dial headers: %v", header)
	}
	if got := <-fromGuest; string(got) != "HTTP/1.1 200 OK\r\n" {
		t.Fatalf("relay got %q", got)
	}
	if len(*ports) != 1 || (*ports)[0] != 3000 {
		t.Fatalf("guest port: got %v", *ports)
	}
	if result := postedResult(t, transport); !result.Ok {
		t.Fatalf("tunnel must report success, got %+v", result)
	}
	// The relay hanging up closes the guest stream.
	_ = guestEnd.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(guestEnd); err != nil {
		t.Fatalf("guest stream must close cleanly, got %v", err)
	}
}

func TestTunnelReportsAClosedPort(t *testing.T) {
	relay := func(w http.ResponseWriter, _ *http.Request) { t.Error("relay must not be dialed") }
	c, transport, _ := tunnelAgent(t, relay, nil, &TunnelError{Code: "port_closed", Err: errors.New("nothing is listening on port 3000")})
	c.openTunnel(context.Background(), tunnelCommand())
	result := postedResult(t, transport)
	if result.Ok || result.Code != "port_closed" {
		t.Fatalf("closed port must report port_closed, got %+v", result)
	}
}

func TestTunnelReportsAnUntaggedFailureAsFailed(t *testing.T) {
	relay := func(w http.ResponseWriter, _ *http.Request) { t.Error("relay must not be dialed") }
	c, transport, _ := tunnelAgent(t, relay, nil, errors.New("dial tcp 10.0.0.2:2222: connection refused"))
	c.openTunnel(context.Background(), tunnelCommand())
	if result := postedResult(t, transport); result.Ok || result.Code != "tunnel_failed" {
		t.Fatalf("untagged failure must report tunnel_failed, got %+v", result)
	}
}

func TestTunnelClosesTheGuestWhenTheRelayRefuses(t *testing.T) {
	guest, guestEnd := net.Pipe()
	relay := func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "forbidden", http.StatusForbidden) }
	c, transport, _ := tunnelAgent(t, relay, guest, nil)
	c.openTunnel(context.Background(), tunnelCommand())
	result := postedResult(t, transport)
	if result.Ok || result.Code != "host_unreachable" {
		t.Fatalf("refused relay must report host_unreachable, got %+v", result)
	}
	_ = guestEnd.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := guestEnd.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("guest stream must be closed, got %v", err)
	}
}

func TestTunnelRefusesIncompletePayload(t *testing.T) {
	relay := func(w http.ResponseWriter, _ *http.Request) { t.Error("relay must not be dialed") }
	c, transport, ports := tunnelAgent(t, relay, nil, nil)
	cmd := tunnelCommand()
	cmd.Payload = json.RawMessage(`{"dockId":"dock-1","sessionId":"sess-1","secret":"s","port":0}`)
	c.openTunnel(context.Background(), cmd)
	if result := postedResult(t, transport); result.Ok || result.Code != "host_error" {
		t.Fatalf("bad payload must fail, got %+v", result)
	}
	if len(*ports) != 0 {
		t.Fatal("guest must not be dialed for a bad payload")
	}
}

func TestTunnelRefusedWithoutAnOpener(t *testing.T) {
	relay := func(w http.ResponseWriter, _ *http.Request) {}
	c, transport, _ := tunnelAgent(t, relay, nil, nil)
	c.OpenTunnel = nil
	c.openTunnel(context.Background(), tunnelCommand())
	if result := postedResult(t, transport); result.Ok || result.Code != "tunnel_unavailable" {
		t.Fatalf("host without tunnels must refuse, got %+v", result)
	}
}
