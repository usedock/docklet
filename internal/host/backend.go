package host

import (
	"context"

	apiv1 "github.com/usedock/docklet/api/v1"
	"github.com/usedock/docklet/internal/fc"
)

// Backend owns guest processes on this host. The Firecracker SDK driver
// and the Docker container driver both satisfy it; orchestration above
// (allocator, cgroup limits, pool, reconciler) stays backend-agnostic.
type Backend interface {
	Boot(ctx context.Context, spec fc.BootSpec) (*fc.Handle, error)
	Adopt(dockID, jailRoot string, res apiv1.Resources) (*fc.Handle, error)
	Pause(ctx context.Context, h *fc.Handle) error
	Resume(ctx context.Context, h *fc.Handle) error
	CreateSnapshot(ctx context.Context, h *fc.Handle, snapshotType string) error
	SnapshotPaths(h *fc.Handle) (memPath, statePath string)
	Terminate(h *fc.Handle) error
}

// SDK driver satisfies Backend; container driver asserts the same.
var _ Backend = (*fc.SDKDriver)(nil)
