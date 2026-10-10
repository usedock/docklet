// Command gen-fast-agent prints the guest-agent source baked into the OCI
// gvisor image, so the gvisor sidecar runs the identical protocol as the
// microVM systemd unit. Run from the docklet root:
// go run ./cmd/gen-fast-agent > scripts/fast-agent.py
package main

import (
	"fmt"

	"github.com/usedock/docklet/internal/host"
)

func main() {
	fmt.Print(host.FastAgentProgramText())
}
