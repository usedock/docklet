package host

import (
	"errors"
	"fmt"

	apiv1 "github.com/usedock/docklet/api/v1"
)

// ErrCapacityExhausted refuses a boot whose requested resources would overcommit the host budget.
var ErrCapacityExhausted = errors.New("capacity_exhausted")

// reserve admits a boot of res under the host budget and holds it until the
// returned release runs; callers defer release so a boot that ends tracked
// (vms) stops being counted twice. An unset budget admits (a negative memory
// budget means none is left).
func (h *Host) reserve(id string, res apiv1.Resources) (release func(), err error) {
	if h.cfg.CapacityVCPU <= 0 && h.cfg.CapacityMemMiB == 0 {
		return func() {}, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	vcpu, mem := int64(res.CPUs), int64(res.MemoryMiB)
	add := func(owner string, r apiv1.Resources) {
		if owner == id {
			return
		}
		vcpu += int64(r.CPUs)
		mem += int64(r.MemoryMiB)
	}
	for owner, v := range h.vms {
		add(owner, v.res)
	}
	for owner, r := range h.pending {
		add(owner, r)
	}
	if h.cfg.CapacityVCPU > 0 && vcpu > h.cfg.CapacityVCPU {
		return nil, fmt.Errorf("%w: %d vCPU requested would exceed the %d vCPU budget", ErrCapacityExhausted, vcpu, h.cfg.CapacityVCPU)
	}
	if budget := max(h.cfg.CapacityMemMiB, 0); h.cfg.CapacityMemMiB != 0 && mem > budget {
		return nil, fmt.Errorf("%w: %d MiB requested would exceed the %d MiB budget", ErrCapacityExhausted, mem, budget)
	}
	h.pending[id] = res
	return func() {
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
	}, nil
}
