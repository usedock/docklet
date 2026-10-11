package host

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/usedock/docklet/internal/guestagent"
)

// dialAgentTunnel opens a tcp stream through the real guest agent at addr.
func dialAgentTunnel(t *testing.T, addr, token string, port int) (net.Conn, error) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	tunnel, err := guestagent.NewTunnel(conn, token, port)
	if err != nil {
		conn.Close()
		return nil, err
	}
	t.Cleanup(func() { tunnel.Close() })
	return tunnel, nil
}

func TestGuestTunnelRelaysBytesToALoopbackPort(t *testing.T) {
	addr := startGuestAgent(t, "tok")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Server-first bytes must survive the handshake buffer.
		_, _ = conn.Write([]byte("hello\n"))
		line, _ := bufio.NewReader(conn).ReadString('\n')
		_, _ = conn.Write([]byte("echo:" + line))
	}()
	tunnel, err := dialAgentTunnel(t, addr, "tok", listener.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	_ = tunnel.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := tunnel.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(tunnel)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "hello\necho:ping\n" {
		t.Fatalf("tunnel output: got %q", out)
	}
}

func TestGuestTunnelReportsAClosedPort(t *testing.T) {
	addr := startGuestAgent(t, "tok")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	_, err = dialAgentTunnel(t, addr, "tok", port)
	if !errors.Is(err, ErrPortClosed) {
		t.Fatalf("closed port must be refused as port_closed, got %v", err)
	}
}

func TestGuestTunnelRefusesTheAgentPort(t *testing.T) {
	addr := startGuestAgent(t, "tok")
	_, agentPort, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(agentPort)
	_, err := dialAgentTunnel(t, addr, "tok", port)
	if err == nil || !strings.Contains(err.Error(), "invalid_port") {
		t.Fatalf("agent port must be refused, got %v", err)
	}
}

func TestOpenPortMapsAgentRefusals(t *testing.T) {
	ctx := context.Background()
	h, _, _ := testHost(t)
	stubAgent(t, h)
	if _, err := h.ProvisionWithAgent(ctx, "dock-1", resSmall, "", "", nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		reply string
		want  error
	}{
		{"unsupported_operation", ErrTunnelUnavailable},
		{"port_closed", ErrPortClosed},
	} {
		var got fastAgentRequest
		h.cfg.AgentDial = func(_ context.Context, _ string) (net.Conn, error) {
			guest, hostEnd := net.Pipe()
			go func() {
				defer guest.Close()
				line, _ := bufio.NewReader(guest).ReadBytes('\n')
				_ = json.Unmarshal(line, &got)
				_, _ = guest.Write([]byte(`{"ok":false,"error":"` + tc.reply + `"}` + "\n"))
			}()
			return hostEnd, nil
		}
		if _, err := h.OpenPort(ctx, "dock-1", 3000); !errors.Is(err, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.reply, err, tc.want)
		}
		if got.Op != "tcp" || got.Port != 3000 || len(got.Token) != 64 {
			t.Fatalf("tcp request must carry the agent token and port, got %+v", got)
		}
	}
}

func TestOpenPortNeedsAnAgentDock(t *testing.T) {
	h, _, _ := testHost(t)
	if _, err := h.OpenPort(context.Background(), "missing", 3000); !errors.Is(err, ErrExecUnavailable) {
		t.Fatalf("unknown dock must be exec_unavailable, got %v", err)
	}
}
