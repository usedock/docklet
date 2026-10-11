package guestagent

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeAgent serves the line protocol; handle gets each request and the connection.
func fakeAgent(t *testing.T, handle func(req Request, conn net.Conn, r *bufio.Reader)) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				line, err := r.ReadBytes('\n')
				if err != nil {
					return
				}
				var req Request
				_ = json.Unmarshal(line, &req)
				handle(req, conn, r)
			}()
		}
	}()
	return l.Addr().String()
}

func reply(conn net.Conn, v any) {
	b, _ := json.Marshal(v)
	_, _ = conn.Write(append(b, '\n'))
}

func TestCallSendsTokenAndDecodesExec(t *testing.T) {
	var got Request
	addr := fakeAgent(t, func(req Request, conn net.Conn, _ *bufio.Reader) {
		got = req
		if req.Token != "tok" {
			reply(conn, Response{Error: "invalid_token"})
			return
		}
		reply(conn, Response{OK: true, ExitCode: 3, Stdout: "out", Stderr: "err"})
	})
	res, err := Call(context.Background(), addr, "tok", Request{Op: "exec", Script: "echo hi", TimeoutSeconds: 5})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || res.Stdout != "out" || res.Stderr != "err" {
		t.Fatalf("response %+v", res)
	}
	if got.Op != "exec" || got.Script != "echo hi" || got.Token != "tok" {
		t.Fatalf("request %+v", got)
	}
	if _, err := Call(context.Background(), addr, "bad", Request{Op: "ping"}); err == nil || !strings.Contains(err.Error(), "invalid_token") {
		t.Fatalf("wrong token must be refused, got %v", err)
	}
}

func TestCallStopsOnContextCancel(t *testing.T) {
	addr := fakeAgent(t, func(_ Request, conn net.Conn, r *bufio.Reader) {
		_, _ = io.Copy(io.Discard, r) // never answers
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Call(ctx, addr, "t", Request{Op: "exec", TimeoutSeconds: 60})
	if err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("cancel must abort the call, err=%v after %v", err, time.Since(start))
	}
}

func TestTerminalFramesAndRefusal(t *testing.T) {
	addr := fakeAgent(t, func(req Request, conn net.Conn, r *bufio.Reader) {
		if req.Op != "pty" {
			reply(conn, Response{Error: "unsupported_operation"})
			return
		}
		reply(conn, Response{OK: true})
		// echo one frame back, then exit 7
		header := make([]byte, 5)
		_, _ = io.ReadFull(r, header)
		payload := make([]byte, binary.BigEndian.Uint32(header[1:]))
		_, _ = io.ReadFull(r, payload)
		out := append([]byte{TerminalData, 0, 0, 0, byte(len(payload))}, payload...)
		out = append(out, TerminalExit, 0, 0, 0, 4, 0, 0, 0, 7)
		_, _ = conn.Write(out)
	})
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	term, err := NewTerminal(conn, Request{Token: "t", Op: "pty", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if err := term.WriteFrame(TerminalData, []byte("ls\n")); err != nil {
		t.Fatal(err)
	}
	_ = term.SetReadDeadline(time.Now().Add(5 * time.Second))
	kind, payload, err := term.ReadFrame()
	if err != nil || kind != TerminalData || string(payload) != "ls\n" {
		t.Fatalf("frame %d %q %v", kind, payload, err)
	}
	kind, payload, err = term.ReadFrame()
	if err != nil || kind != TerminalExit || binary.BigEndian.Uint32(payload) != 7 {
		t.Fatalf("exit frame %d %v %v", kind, payload, err)
	}

	conn2, _ := net.Dial("tcp", addr)
	defer conn2.Close()
	if _, err := NewTunnel(conn2, "t", 80); !errors.Is(err, ErrTunnelUnavailable) {
		t.Fatalf("unsupported op must map to ErrTunnelUnavailable, got %v", err)
	}
}

func TestTunnelKeepsServerFirstBytes(t *testing.T) {
	addr := fakeAgent(t, func(req Request, conn net.Conn, r *bufio.Reader) {
		if req.Port == 1 {
			reply(conn, Response{Error: "port_closed"})
			return
		}
		b, _ := json.Marshal(Response{OK: true})
		_, _ = conn.Write(append(b, []byte("\nhello")...))
	})
	conn, _ := net.Dial("tcp", addr)
	tun, err := NewTunnel(conn, "t", 8080)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	_ = tun.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 5)
	if _, err := io.ReadFull(tun, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("got %q %v", buf, err)
	}
	conn2, _ := net.Dial("tcp", addr)
	defer conn2.Close()
	if _, err := NewTunnel(conn2, "t", 1); !errors.Is(err, ErrPortClosed) {
		t.Fatalf("want ErrPortClosed, got %v", err)
	}
}
