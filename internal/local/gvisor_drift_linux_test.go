//go:build linux

package local

import (
	"testing"

	"github.com/usedock/docklet/internal/host"
)

// internal/host only builds on Linux, so the agent check runs there (CI).
func TestEmbeddedAgentMatchesHost(t *testing.T) {
	if lf(gvAgentProgram) != lf(host.FastAgentProgramText()) {
		t.Fatal("internal/local/assets/dock-fast-agent.py drifted from host.FastAgentProgramText; regenerate: go run ./cmd/gen-fast-agent > internal/local/assets/dock-fast-agent.py")
	}
}
