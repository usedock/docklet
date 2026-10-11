package fc

import (
	"fmt"

	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"

	apiv1 "github.com/usedock/docklet/api/v1"
)

// LimitAuto asks for the per-size default of a Limits field.
const LimitAuto = -1

// Host-wide I/O and process limits. LimitAuto derives a per-size default from
// the dock's resources, 0 is unlimited, a positive value is used as is. The
// zero Limits is therefore fully unlimited.
type Limits struct {
	DiskMBps  int
	DiskIOPS  int
	NetMBps   int
	PidsLimit int
}

// Effective is one dock's resolved limits; 0 means unlimited.
type Effective struct {
	DiskMBps  int
	DiskIOPS  int
	NetMBps   int
	PidsLimit int
}

// Per-size defaults: per vCPU, capped so a large dock cannot claim a whole host.
const (
	defaultDiskMBpsPerCPU = 100
	defaultDiskMBpsCap    = 1000
	defaultDiskIOPSPerCPU = 2000
	defaultDiskIOPSCap    = 20000
	defaultNetMBpsPerCPU  = 25
	defaultNetMBpsCap     = 250
	// DefaultPidsLimit is flat: process count does not track vCPU count.
	DefaultPidsLimit = 4096
)

func scaled(perCPU, limit, cpus int) int {
	v := perCPU * cpus
	if v > limit {
		return limit
	}
	return v
}

func resolve(v, auto int) int {
	if v == LimitAuto {
		return auto
	}
	return v
}

// For resolves the limits for one dock size.
func (l Limits) For(r apiv1.Resources) Effective {
	return Effective{
		DiskMBps:  resolve(l.DiskMBps, scaled(defaultDiskMBpsPerCPU, defaultDiskMBpsCap, r.CPUs)),
		DiskIOPS:  resolve(l.DiskIOPS, scaled(defaultDiskIOPSPerCPU, defaultDiskIOPSCap, r.CPUs)),
		NetMBps:   resolve(l.NetMBps, scaled(defaultNetMBpsPerCPU, defaultNetMBpsCap, r.CPUs)),
		PidsLimit: resolve(l.PidsLimit, DefaultPidsLimit),
	}
}

func (e Effective) String() string {
	f := func(v int) string {
		if v <= 0 {
			return "unlimited"
		}
		return fmt.Sprint(v)
	}
	return fmt.Sprintf("diskMBps=%s diskIOPS=%s netMBps=%s pids=%s", f(e.DiskMBps), f(e.DiskIOPS), f(e.NetMBps), f(e.PidsLimit))
}

// bucket refills size tokens every second and allows one extra second of burst.
func bucket(size int64) *models.TokenBucket {
	return &models.TokenBucket{
		Size:         i64(size),
		OneTimeBurst: i64(size),
		RefillTime:   i64(1000),
	}
}

func i64(v int64) *int64 { return &v }

const mib = 1024 * 1024

// driveLimiter is nil when neither bandwidth nor ops is limited.
func driveLimiter(e Effective) *models.RateLimiter {
	rl := &models.RateLimiter{}
	if e.DiskMBps > 0 {
		rl.Bandwidth = bucket(int64(e.DiskMBps) * mib)
	}
	if e.DiskIOPS > 0 {
		rl.Ops = bucket(int64(e.DiskIOPS))
	}
	if rl.Bandwidth == nil && rl.Ops == nil {
		return nil
	}
	return rl
}

// netLimiter caps guest traffic in one direction; nil when unlimited.
func netLimiter(e Effective) *models.RateLimiter {
	if e.NetMBps <= 0 {
		return nil
	}
	return &models.RateLimiter{Bandwidth: bucket(int64(e.NetMBps) * mib)}
}
