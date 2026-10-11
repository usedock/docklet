package guestagent

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Terminal frame kinds; the guest agent, the fleet relay, and the browser share them.
const (
	TerminalData   byte = 0
	TerminalResize byte = 1
	TerminalExit   byte = 2
)

// MaxTerminalFrame bounds one frame payload in either direction.
const MaxTerminalFrame = 64 << 10

// ErrTerminalUnavailable marks a guest agent that predates the pty op.
var ErrTerminalUnavailable = errors.New("terminal_unavailable")

// Terminal is one login shell on a pty in a dock's guest, spoken to in frames.
type Terminal struct {
	conn net.Conn
	r    *bufio.Reader
}

// NewTerminal performs the pty handshake on conn. On error the caller closes conn.
func NewTerminal(conn net.Conn, request Request) (*Terminal, error) {
	r, err := Handshake(conn, request)
	if err != nil {
		if IsRefusal(err, "unsupported_operation") {
			return nil, fmt.Errorf("%w: guest agent predates terminals", ErrTerminalUnavailable)
		}
		return nil, fmt.Errorf("guest terminal: %w", err)
	}
	return &Terminal{conn: conn, r: r}, nil
}

// ReadFrame returns the next guest frame: shell output or its exit status.
func (t *Terminal) ReadFrame() (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(t.r, header[:]); err != nil {
		return 0, nil, err
	}
	size := binary.BigEndian.Uint32(header[1:])
	if size > MaxTerminalFrame {
		return 0, nil, fmt.Errorf("guest terminal frame of %d bytes exceeds limit", size)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(t.r, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

// WriteFrame sends keystrokes or a resize to the guest shell.
func (t *Terminal) WriteFrame(kind byte, payload []byte) error {
	if len(payload) > MaxTerminalFrame {
		return fmt.Errorf("terminal frame of %d bytes exceeds limit", len(payload))
	}
	frame := make([]byte, 5+len(payload))
	frame[0] = kind
	binary.BigEndian.PutUint32(frame[1:], uint32(len(payload)))
	copy(frame[5:], payload)
	_, err := t.conn.Write(frame)
	return err
}

// SetReadDeadline bounds the next ReadFrame.
func (t *Terminal) SetReadDeadline(d time.Time) error { return t.conn.SetReadDeadline(d) }

// Close hangs the shell up.
func (t *Terminal) Close() error { return t.conn.Close() }
