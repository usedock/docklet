package main

import (
	"errors"
	"fmt"
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
