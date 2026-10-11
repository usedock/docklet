package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/usedock/docklet/internal/agent"
	"github.com/usedock/docklet/internal/host"
)

func TestTunnelErrorCodes(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{fmt.Errorf("%w: nothing is listening on port 3000", host.ErrPortClosed), "port_closed"},
		{fmt.Errorf("%w: guest agent predates port tunnels", host.ErrTunnelUnavailable), "tunnel_unavailable"},
		{fmt.Errorf("%w: fast runtime dock-1 not found", host.ErrExecUnavailable), "tunnel_failed"},
		{errors.New("dial tcp 10.0.0.2:2222: connection refused"), "tunnel_failed"},
	}
	for _, tc := range cases {
		var tunnelErr *agent.TunnelError
		if !errors.As(tunnelError(tc.err), &tunnelErr) || tunnelErr.Code != tc.code {
			t.Errorf("%v: want code %s, got %+v", tc.err, tc.code, tunnelErr)
		}
		if !errors.Is(tunnelErr, tc.err) {
			t.Errorf("%v: cause must be kept", tc.err)
		}
	}
}

func TestParseOvercommit(t *testing.T) {
	for raw, want := range map[string]float64{"": 1, "1.5": 1.5, "0.5": 0.5} {
		if got, err := parseOvercommit(raw); err != nil || got != want {
			t.Errorf("%q: got %v, %v, want %v", raw, got, err, want)
		}
	}
	for _, raw := range []string{"0", "-1", "abc", "NaN", "Inf", "+Inf"} {
		if _, err := parseOvercommit(raw); err == nil {
			t.Errorf("%q must be refused", raw)
		}
	}
}

type failListener struct{ net.Listener }

func (failListener) Accept() (net.Conn, error) { return nil, errors.New("boom") }
func (failListener) Close() error              { return nil }
func (failListener) Addr() net.Addr            { return &net.TCPAddr{} }

func TestServeEgressFatalOnServeError(t *testing.T) {
	var got string
	serveEgress(failListener{}, http.NotFoundHandler(), func(f string, a ...any) { got = f })
	if got == "" {
		t.Fatal("fatal not called after serve error")
	}
}

func TestServeEgressCleanShutdownNotFatal(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	serveEgress(l, http.NotFoundHandler(), func(string, ...any) { t.Fatal("fatal on clean close") })
}
