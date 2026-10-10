package host

import (
	"errors"
	"fmt"

	apiv1 "github.com/usedock/docklet/api/v1"
)

// ErrCapacityExhausted refuses a boot whose requested resources would overcommit the host budget.
var ErrCapacityExhausted = errors.New("capacity_exhausted")

// diskBudgetBytes is the allocatable disk: host capacity times the overcommit ratio; 0 means unbounded.
func (h *Host) diskBudgetBytes() int64 {
	if h.cfg.DiskTotalBytes == nil {
		return 0
	}
	total := h.cfg.DiskTotalBytes()
	if total <= 0 {
		return 0
	}
	ratio := h.cfg.DiskOvercommit
	if ratio <= 0 {
		ratio = 1
	}
	return int64(float64(total) * ratio)
}

// reserve admits a boot of res under the host budget and holds it until the
// returned release runs; callers defer release so a boot that ends tracked
// (vms) stops being counted twice. An unset budget admits (a negative memory
// budget means none is left).
func (h *Host) reserve(id string, res apiv1.Resources) (release func(), err error) {
	checkDisk := h.cfg.DiskFreeBytes != nil && h.usesVeth() && !h.hasWorkDisk(id)
	diskBudget := h.diskBudgetBytes()
	if h.cfg.CapacityVCPU <= 0 && h.cfg.CapacityMemMiB == 0 && !checkDisk && diskBudget == 0 {
		return func() {}, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	vcpu, mem, diskGiB := int64(res.CPUs), int64(res.MemoryMiB), int64(res.DiskGiB)
	// allocGiB is every dock's allocated disk plus this boot, sparse or not.
	allocGiB := diskGiB
	add := func(owner string, r apiv1.Resources, pending bool) {
		if owner == id {
			return
		}
		vcpu += int64(r.CPUs)
		mem += int64(r.MemoryMiB)
		allocGiB += int64(r.DiskGiB)
		// Live disks are sparse and already show in free space; only boots in flight are unwritten.
		if pending {
			diskGiB += int64(r.DiskGiB)
		}
	}
	for owner, v := range h.vms {
		add(owner, v.res, false)
	}
	for owner, r := range h.pending {
		add(owner, r, true)
	}
	if h.cfg.CapacityVCPU > 0 && vcpu > h.cfg.CapacityVCPU {
		return nil, fmt.Errorf("%w: %d vCPU requested would exceed the %d vCPU budget", ErrCapacityExhausted, vcpu, h.cfg.CapacityVCPU)
	}
	if budget := max(h.cfg.CapacityMemMiB, 0); h.cfg.CapacityMemMiB != 0 && mem > budget {
		return nil, fmt.Errorf("%w: %d MiB requested would exceed the %d MiB budget", ErrCapacityExhausted, mem, budget)
	}
	if diskBudget > 0 && allocGiB<<30 > diskBudget {
		return nil, fmt.Errorf("%w: %d GiB of allocated disk would exceed the %d GiB disk budget", ErrCapacityExhausted, allocGiB, diskBudget>>30)
	}
	if checkDisk {
		if free := h.cfg.DiskFreeBytes(); diskGiB<<30 > free {
			return nil, fmt.Errorf("%w: %d GiB of work disk requested would exceed the %d GiB free on the host", ErrCapacityExhausted, diskGiB, free>>30)
		}
	}
	h.pending[id] = res
	return func() {
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
	}, nil
}

// hasWorkDisk reports a dock that already owns its bounded disk, which a resume does not allocate again.
func (h *Host) hasWorkDisk(dockID string) bool {
	w, ok := h.cfg.Driver.(interface{ WorkDiskExists(dockID string) bool })
	return ok && w.WorkDiskExists(dockID)
}
