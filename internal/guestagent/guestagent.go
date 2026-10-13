// Package guestagent is the OS-neutral client for the guest agent that runs in every
// dock: one JSON request per TCP connection, authenticated by a token. The agent program
// itself lives in internal/host so the image build and the protocol stay together.
package guestagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// Port is the TCP port the guest agent listens on inside the guest.
const Port = 2222

// Addr returns host:Port.
func Addr(host string) string { return net.JoinHostPort(host, strconv.Itoa(Port)) }

// Request is one agent request line.
type Request struct {
	Token          string            `json:"token"`
	Op             string            `json:"op"`
	Script         string            `json:"script,omitempty"`
	Path           string            `json:"path,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"`
	NewToken       string            `json:"newToken,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	OutputLimit    int               `json:"outputLimit,omitempty"`
	Cols           int               `json:"cols,omitempty"`
	Rows           int               `json:"rows,omitempty"`
	Port           int               `json:"port,omitempty"`
}

// Response is the agent's answer line.
type Response struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	ExitCode  int    `json:"exitCode,omitempty"`
	Stdout    string `json:"stdout,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	Content   string `json:"content,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	TimedOut  bool   `json:"timedOut,omitempty"`
}

// MaxExecOutput bounds stdout and stderr each; the guest keeps the head.
const MaxExecOutput = 1 << 20

// Guest replies are read through these caps so a misbehaving guest cannot balloon memory.
const (
	maxExecReply = 16 << 20
	maxReadReply = 64 << 20
)

const (
	dialTimeout      = time.Second
	handshakeTimeout = 10 * time.Second
)

// Refusal is the error code a guest agent answered with.
type Refusal string

func (e Refusal) Error() string { return string(e) }

// DialFunc opens one connection to a guest agent; the default is TCP, a dock with no
// network reaches its agent another way.
type DialFunc func(ctx context.Context) (net.Conn, error)

// TCPDial dials the agent at addr (host:port).
func TCPDial(addr string) DialFunc {
	return func(ctx context.Context) (net.Conn, error) { return Dial(ctx, addr) }
}

// Call sends one request to the agent at addr (host:port) and returns its reply.
// Exec holds the connection for the script's own budget plus a minute of slack.
func Call(ctx context.Context, addr, token string, request Request) (Response, error) {
	return CallDial(ctx, TCPDial(addr), token, request)
}

// CallDial is Call over any connection dial opens.
func CallDial(ctx context.Context, dial DialFunc, token string, request Request) (Response, error) {
	request.Token = token
	payload, err := json.Marshal(request)
	if err != nil {
		return Response{}, err
	}
	conn, err := dial(ctx)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	deadline := time.Now().Add(time.Duration(request.TimeoutSeconds)*time.Second + time.Minute)
	if err := conn.SetDeadline(deadline); err != nil {
		return Response{}, err
	}
	// Closing on cancel unblocks a long exec read.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return Response{}, err
	}
	limit := int64(maxExecReply)
	if request.Op == "read" {
		limit = maxReadReply
	}
	var response Response
	if err := json.NewDecoder(io.LimitReader(conn, limit)).Decode(&response); err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, err
	}
	if !response.OK {
		return Response{}, fmt.Errorf("fast guest agent: %s", response.Error)
	}
	return response, nil
}

// Dial opens a raw connection to the agent at addr.
func Dial(ctx context.Context, addr string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: dialTimeout}
	return dialer.DialContext(ctx, "tcp", addr)
}

// Handshake sends one request line and reads the guest's verdict; the returned reader
// holds any stream bytes that arrived with it. A refusal is returned as a Refusal.
func Handshake(conn net.Conn, request Request) (*bufio.Reader, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(conn, MaxTerminalFrame)
	line, err := r.ReadSlice('\n')
	if err != nil {
		return nil, fmt.Errorf("handshake: %w", err)
	}
	var response Response
	if err := json.Unmarshal(line, &response); err != nil {
		return nil, fmt.Errorf("handshake: %w", err)
	}
	if !response.OK {
		return nil, Refusal(response.Error)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	return r, nil
}

// IsRefusal reports whether err is the agent refusing with code.
func IsRefusal(err error, code string) bool {
	var refused Refusal
	return errors.As(err, &refused) && string(refused) == code
}
