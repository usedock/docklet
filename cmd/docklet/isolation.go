package main

import (
	"fmt"
	"strconv"

	"github.com/usedock/docklet/internal/fc"
	"github.com/usedock/docklet/internal/uid"
)

// Upper bounds for limit env values so MBps * MiB cannot overflow and a typo is refused.
const (
	maxMBps = 1_000_000
	maxIOPS = 10_000_000
	maxPids = 4_194_304
)

// isolation is the per-dock uid range and the I/O and process limits.
type isolation struct {
	// UIDBase 0 means the deprecated shared DOCKLET_UID/GID.
	UIDBase int
	UIDSize int
	Limits  fc.Limits
}

// parseIsolation reads the isolation env; any malformed value is an error so a
// typo never silently disables a limit.
func parseIsolation(env func(string) string) (isolation, error) {
	iso := isolation{UIDBase: uid.DefaultBase, UIDSize: uid.DefaultSize}
	var err error
	if iso.UIDBase, err = parseNonNeg(env, "DOCKLET_UID_BASE", uid.DefaultBase); err != nil {
		return iso, err
	}
	if iso.UIDSize, err = parseNonNeg(env, "DOCKLET_UID_SIZE", uid.DefaultSize); err != nil {
		return iso, err
	}
	if iso.UIDBase > 0 {
		if err := uid.ValidRange(iso.UIDBase, iso.UIDSize); err != nil {
			return iso, fmt.Errorf("DOCKLET_UID_BASE/DOCKLET_UID_SIZE: %w", err)
		}
	}
	for _, f := range []struct {
		key string
		dst *int
		max int
	}{
		{"DOCKLET_DISK_MBPS", &iso.Limits.DiskMBps, maxMBps},
		{"DOCKLET_DISK_IOPS", &iso.Limits.DiskIOPS, maxIOPS},
		{"DOCKLET_NET_MBPS", &iso.Limits.NetMBps, maxMBps},
		{"DOCKLET_PIDS_LIMIT", &iso.Limits.PidsLimit, maxPids},
	} {
		if *f.dst, err = parseNonNeg(env, f.key, fc.LimitAuto); err != nil {
			return iso, err
		}
		if *f.dst > f.max {
			return iso, fmt.Errorf("%s %d exceeds the maximum %d", f.key, *f.dst, f.max)
		}
	}
	return iso, nil
}

// parseNonNeg returns fallback when unset and refuses anything but a whole number >= 0.
func parseNonNeg(env func(string) string, key string, fallback int) (int, error) {
	raw := env(key)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s %q must be a whole number >= 0 (0 = unlimited or disabled)", key, raw)
	}
	return n, nil
}
