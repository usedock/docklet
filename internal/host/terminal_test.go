package host

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startGuestAgent runs the real guest agent program as this user on a loopback port.
func startGuestAgent(t *testing.T, token string) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("/bin/bash not installed")
	}
	dir := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	config, _ := json.Marshal(map[string]any{"token": token, "workspace": dir, "roots": []string{dir}})
	configPath := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	program := strings.ReplaceAll(fastAgentProgram, "/etc/dock-fast-agent.json", configPath)
	program = strings.Replace(program, "server.bind(('0.0.0.0', 2222))", fmt.Sprintf("server.bind(('127.0.0.1', %d))", port), 1)
	programPath := filepath.Join(dir, "agent.py")
	if err := os.WriteFile(programPath, []byte(program), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", programPath)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for range 100 {
		if conn, err := net.Dial("tcp", addr); err == nil {
			conn.Close()
			return addr
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("guest agent never listened")
	return ""
}

func dialAgentTerminal(t *testing.T, addr, token string, cols, rows int) *Terminal {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	term, err := handshakeTerminal(conn, fastAgentRequest{Token: token, Op: "pty", Cols: cols, Rows: rows})
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { term.Close() })
	return term
}

// readUntil collects shell output until want appears, failing on exit or timeout.
func readUntil(t *testing.T, term *Terminal, want string) string {
	t.Helper()
	_ = term.SetReadDeadline(time.Now().Add(10 * time.Second))
	var out strings.Builder
	for !strings.Contains(out.String(), want) {
		kind, payload, err := term.ReadFrame()
		if err != nil {
			t.Fatalf("waiting for %q: %v (output %q)", want, err, out.String())
		}
		if kind == TerminalExit {
			t.Fatalf("shell exited while waiting for %q (output %q)", want, out.String())
		}
		out.Write(payload)
	}
	return out.String()
}

func TestGuestTerminalRunsAnInteractiveShell(t *testing.T) {
	addr := startGuestAgent(t, "tok")
	term := dialAgentTerminal(t, addr, "tok", 100, 40)
	if err := term.WriteFrame(TerminalData, []byte("echo marker-$((6*7))\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, term, "marker-42")
	if err := term.WriteFrame(TerminalData, []byte("tty; stty size\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, term, "40 100")
	resize := make([]byte, 4)
	binary.BigEndian.PutUint16(resize[0:], 132)
	binary.BigEndian.PutUint16(resize[2:], 50)
	if err := term.WriteFrame(TerminalResize, resize); err != nil {
		t.Fatal(err)
	}
	if err := term.WriteFrame(TerminalData, []byte("stty size\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, term, "50 132")
	if err := term.WriteFrame(TerminalData, []byte("exit 3\n")); err != nil {
		t.Fatal(err)
	}
	for {
		kind, payload, err := term.ReadFrame()
		if err != nil {
			t.Fatalf("shell exit never reported: %v", err)
		}
		if kind == TerminalExit {
			if code := int32(binary.BigEndian.Uint32(payload)); code != 3 {
				t.Fatalf("exit status: got %d, want 3", code)
			}
			return
		}
	}
}

func TestGuestTerminalRefusesAWrongToken(t *testing.T) {
	addr := startGuestAgent(t, "tok")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := handshakeTerminal(conn, fastAgentRequest{Token: "wrong", Op: "pty"}); err == nil || !strings.Contains(err.Error(), "invalid_token") {
		t.Fatalf("wrong token must be refused, got %v", err)
	}
}

func TestGuestTerminalHangsUpTheShellOnDisconnect(t *testing.T) {
	addr := startGuestAgent(t, "tok")
	term := dialAgentTerminal(t, addr, "tok", 80, 24)
	if err := term.WriteFrame(TerminalData, []byte("echo pid-$$-end\n")); err != nil {
		t.Fatal(err)
	}
	pidLine := regexp.MustCompile(`pid-(\d+)-end`)
	out := ""
	for !pidLine.MatchString(out) {
		out += readUntil(t, term, "-end")
	}
	pid, _ := strconv.Atoi(pidLine.FindStringSubmatch(out)[1])
	term.Close()
	for range 50 {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("shell %d still running after disconnect", pid)
}

func TestOpenTerminalReportsAnAgentWithoutPty(t *testing.T) {
	ctx := context.Background()
	h, _, _ := testHost(t)
	stubAgent(t, h)
	if _, err := h.ProvisionWithAgent(ctx, "dock-1", resSmall, "", "", nil); err != nil {
		t.Fatal(err)
	}
	var got fastAgentRequest
	h.cfg.AgentDial = func(_ context.Context, _ string) (net.Conn, error) {
		guest, hostEnd := net.Pipe()
		go func() {
			defer guest.Close()
			line, _ := bufio.NewReader(guest).ReadBytes('\n')
			_ = json.Unmarshal(line, &got)
			_, _ = guest.Write([]byte(`{"ok":false,"error":"unsupported_operation"}` + "\n"))
		}()
		return hostEnd, nil
	}
	_, err := h.OpenTerminal(ctx, "dock-1", 120, 30)
	if !errors.Is(err, ErrTerminalUnavailable) {
		t.Fatalf("old agent must report terminal_unavailable, got %v", err)
	}
	if got.Op != "pty" || got.Cols != 120 || got.Rows != 30 || len(got.Token) != 64 {
		t.Fatalf("pty request must carry the agent token and size, got %+v", got)
	}
}

func TestOpenTerminalNeedsAnAgentDock(t *testing.T) {
	h, _, _ := testHost(t)
	if _, err := h.OpenTerminal(context.Background(), "missing", 80, 24); !errors.Is(err, ErrExecUnavailable) {
		t.Fatalf("unknown dock must be exec_unavailable, got %v", err)
	}
}
