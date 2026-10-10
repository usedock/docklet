package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

type frame struct {
	kind    byte
	payload []byte
}

// fakeGuest is an in-memory guest shell: it emits queued output and records input.
type fakeGuest struct {
	out    chan frame
	mu     sync.Mutex
	in     []frame
	closed chan struct{}
	once   sync.Once
}

func newFakeGuest() *fakeGuest {
	return &fakeGuest{out: make(chan frame, 8), closed: make(chan struct{})}
}

func (g *fakeGuest) ReadFrame() (byte, []byte, error) {
	select {
	case f := <-g.out:
		return f.kind, f.payload, nil
	case <-g.closed:
		return 0, nil, io.EOF
	}
}

func (g *fakeGuest) WriteFrame(kind byte, payload []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.in = append(g.in, frame{kind, append([]byte(nil), payload...)})
	return nil
}

func (g *fakeGuest) Close() error {
	g.once.Do(func() { close(g.closed) })
	return nil
}

func (g *fakeGuest) received() []frame {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]frame(nil), g.in...)
}

func waitClosed(t *testing.T, g *fakeGuest) {
	t.Helper()
	select {
	case <-g.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("guest shell was never hung up")
	}
}

// terminalAgent wires an agent whose control plane is relay and whose results land in results.
func terminalAgent(t *testing.T, relay http.Handler, guest *fakeGuest, openErr error) (Config, *fakeTransport, *[]string) {
	t.Helper()
	server := httptest.NewServer(relay)
	t.Cleanup(server.Close)
	var dialed []string
	transport := &fakeTransport{handler: func(recorded) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"ok":true,"data":{"accepted":true}}`), nil
	}}
	c := Config{
		FleetURL: server.URL,
		Token:    "fleet-token",
		HostID:   "host-1",
		Client:   transport,
		Logf:     func(string, ...any) {},
		OpenTerminal: func(_ context.Context, dockID string, cols, rows int) (TerminalStream, error) {
			dialed = append(dialed, dockID)
			if openErr != nil {
				return nil, openErr
			}
			if cols != 120 || rows != 40 {
				t.Errorf("guest size: got %dx%d, want 120x40", cols, rows)
			}
			return guest, nil
		},
	}
	return c, transport, &dialed
}

func terminalCommand() command {
	payload, _ := json.Marshal(terminalOpen{DockID: "dock-1", SessionID: "sess-1", Secret: "s3cret", Cols: 120, Rows: 40})
	return command{ID: "cmd-1", Op: terminalOp, IdempotencyToken: "terminal:sess-1", Payload: payload}
}

func postedResult(t *testing.T, transport *fakeTransport) resultRequest {
	t.Helper()
	calls := transport.callsMatching("/v1/fleet/commands/cmd-1/result")
	if len(calls) != 1 {
		t.Fatalf("want one posted result, got %d", len(calls))
	}
	var result resultRequest
	if err := json.Unmarshal(calls[0].body, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestTerminalBridgesGuestAndRelay(t *testing.T) {
	guest := newFakeGuest()
	var header http.Header
	var path string
	fromGuest := make(chan []byte, 4)
	relay := websocket.Server{Handler: func(ws *websocket.Conn) {
		header = ws.Request().Header.Clone()
		path = ws.Request().URL.Path
		_ = websocket.Message.Send(ws, []byte{terminalData, 'l', 's', '\n'})
		_ = websocket.Message.Send(ws, []byte{terminalResize, 0, 132, 0, 50})
		for deadline := time.Now().Add(5 * time.Second); len(guest.received()) < 2 && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}
		guest.out <- frame{terminalData, []byte("hello")}
		guest.out <- frame{terminalExit, []byte{0, 0, 0, 7}}
		for range 2 {
			var message []byte
			if err := websocket.Message.Receive(ws, &message); err != nil {
				return
			}
			fromGuest <- message
		}
	}}
	c, transport, _ := terminalAgent(t, relay, guest, nil)
	c.openTerminal(context.Background(), terminalCommand())

	if result := postedResult(t, transport); !result.Ok || result.HostID != "host-1" {
		t.Fatalf("an established bridge must report ok, got %+v", result)
	}
	if got := string(<-fromGuest); got != "\x00hello" {
		t.Fatalf("guest output: got %q", got)
	}
	if path != "/v1/fleet/terminals/sess-1" || header.Get("Authorization") != "Bearer fleet-token" || header.Get("X-Host-Id") != "host-1" || header.Get("X-Terminal-Secret") != "s3cret" {
		t.Fatalf("relay dial must authenticate the session, got %s %v", path, header)
	}
	if got := <-fromGuest; got[0] != terminalExit || got[4] != 7 {
		t.Fatalf("guest exit: got %v", got)
	}
	waitClosed(t, guest)
	in := guest.received()
	if len(in) != 2 || in[0].kind != terminalData || string(in[0].payload) != "ls\n" || in[1].kind != terminalResize || in[1].payload[1] != 132 {
		t.Fatalf("relay input must reach the guest as frames, got %+v", in)
	}
}

func TestTerminalRefusesRelayFramesOtherThanInput(t *testing.T) {
	guest := newFakeGuest()
	relay := websocket.Server{Handler: func(ws *websocket.Conn) {
		_ = websocket.Message.Send(ws, []byte{terminalExit, 0, 0, 0, 0})
		var discard []byte
		_ = websocket.Message.Receive(ws, &discard)
	}}
	c, _, _ := terminalAgent(t, relay, guest, nil)
	c.openTerminal(context.Background(), terminalCommand())
	waitClosed(t, guest)
	if in := guest.received(); len(in) != 0 {
		t.Fatalf("an exit frame from the relay must never reach the guest, got %+v", in)
	}
}

func TestTerminalHangsUpWhenTheAgentStops(t *testing.T) {
	guest := newFakeGuest()
	relay := websocket.Server{Handler: func(ws *websocket.Conn) {
		var discard []byte
		_ = websocket.Message.Receive(ws, &discard)
	}}
	c, _, _ := terminalAgent(t, relay, guest, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.openTerminal(ctx, terminalCommand())
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	waitClosed(t, guest)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("bridge outlived the agent")
	}
}

func TestTerminalReportsAGuestThatCannotOpen(t *testing.T) {
	relayed := false
	relay := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { relayed = true })
	c, transport, _ := terminalAgent(t, relay, newFakeGuest(), errors.New("terminal_unavailable: guest agent predates terminals"))
	c.openTerminal(context.Background(), terminalCommand())
	result := postedResult(t, transport)
	if result.Ok || result.Code != "terminal_unavailable" || !strings.Contains(result.Message, "predates") {
		t.Fatalf("guest failure must post terminal_unavailable, got %+v", result)
	}
	if relayed {
		t.Fatal("no relay may be dialed without a guest shell")
	}
}

func TestTerminalHangsUpTheGuestWhenTheRelayRefuses(t *testing.T) {
	guest := newFakeGuest()
	relay := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	c, transport, _ := terminalAgent(t, relay, guest, nil)
	c.openTerminal(context.Background(), terminalCommand())
	if result := postedResult(t, transport); result.Ok || result.Code != "host_unreachable" {
		t.Fatalf("refused relay must post host_unreachable, got %+v", result)
	}
	waitClosed(t, guest)
}

func TestTerminalRefusesIncompletePayload(t *testing.T) {
	c, transport, dialed := terminalAgent(t, http.NotFoundHandler(), newFakeGuest(), nil)
	cmd := terminalCommand()
	cmd.Payload = json.RawMessage(`{"dockId":"dock-1","sessionId":"sess-1"}`)
	c.openTerminal(context.Background(), cmd)
	if result := postedResult(t, transport); result.Ok || result.Code != "host_error" {
		t.Fatalf("missing secret must be refused, got %+v", result)
	}
	if len(*dialed) != 0 {
		t.Fatal("no guest shell may start for an incomplete command")
	}
}
