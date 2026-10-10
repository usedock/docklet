package host

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"

	apiv1 "github.com/usedock/docklet/api/v1"
)

// poolSlot is one pre-booted fast VM waiting for a dock to claim it.
type poolSlot struct {
	id  string
	res apiv1.Resources
	key string
}

// pool keeps a small set of booted fast VMs per demand key so ProvisionFast
// usually claims instead of copying and booting. Misses fall back to the
// slow path; refill is best-effort and never fails a provision.
type pool struct {
	mu       sync.Mutex
	target   int
	idle     map[string][]poolSlot
	inflight int
}

// poolSpec is everything needed to boot one slot.
type poolSpec struct {
	res   apiv1.Resources
	image string
}

func newPool(target int) *pool {
	return &pool{target: target, idle: map[string][]poolSlot{}}
}

// manifestKey identifies interchangeable slots: same resources and image.
func manifestKey(res apiv1.Resources, image string) string {
	return fmt.Sprintf("%dc|%dm|%dg|%s", res.CPUs, res.MemoryMiB, res.DiskGiB, image)
}

func poolID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "pool-" + hex.EncodeToString(b[:])
}

// pop removes one idle slot for key.
func (p *pool) pop(key string) (poolSlot, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	slots := p.idle[key]
	if len(slots) == 0 {
		return poolSlot{}, false
	}
	slot := slots[len(slots)-1]
	p.idle[key] = slots[:len(slots)-1]
	return slot, true
}

// push returns a slot to idle when still under target, else reports it full.
func (p *pool) push(slot poolSlot) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.totalIdle() >= p.target {
		return false
	}
	p.idle[slot.key] = append(p.idle[slot.key], slot)
	return true
}

func (p *pool) totalIdle() int {
	n := 0
	for _, slots := range p.idle {
		n += len(slots)
	}
	return n
}

// claimRefill reserves one background boot slot; the caller must run it.
func (p *pool) claimRefill() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.totalIdle()+p.inflight >= p.target {
		return false
	}
	p.inflight++
	return true
}

func (p *pool) dropInflight() {
	p.mu.Lock()
	p.inflight--
	p.mu.Unlock()
}

// errFastExists marks a claim or provision racing an existing runtime id.
var errFastExists = fmt.Errorf("fast runtime already exists")

// claim moves a slot VM onto runtimeID: new agent token, same TAP and IP.
func (h *Host) claimSlot(ctx context.Context, slot poolSlot, runtimeID, token string, egressAllow []string) (string, error) {
	v, err := h.fastVM(slot.id)
	if err != nil {
		return "", err
	}
	if err := h.rekeyFastAgent(ctx, v.guestIP, v.fastToken, token); err != nil {
		_ = h.Destroy(ctx, slot.id)
		return "", fmt.Errorf("claim rekey: %w", err)
	}
	if err := h.alloc.Reassign(slot.id, runtimeID); err != nil {
		_ = h.Destroy(ctx, slot.id)
		return "", fmt.Errorf("claim ip: %w", err)
	}
	h.mu.Lock()
	_, exists := h.vms[runtimeID]
	if !exists {
		delete(h.vms, slot.id)
		h.vms[runtimeID] = &vm{res: v.res, tap: v.tap, guestIP: v.guestIP, pid: v.pid, running: v.running, fastToken: token, fastWorkspace: v.fastWorkspace, ephemeral: true, backing: slot.id}
	}
	h.mu.Unlock()
	if exists {
		_ = h.Destroy(ctx, slot.id)
		return "", fmt.Errorf("%w: %s", errFastExists, runtimeID)
	}
	if len(egressAllow) > 0 {
		// Warm slots boot unconfined; the claim confines before any run work.
		if err := h.restrictEgress(v.guestIP, egressAllow); err != nil {
			_ = h.Destroy(ctx, runtimeID)
			return "", fmt.Errorf("claim egress: %w", err)
		}
		h.mu.Lock()
		if live, ok := h.vms[runtimeID]; ok {
			live.egressGateway = h.alloc.Gateway()
		}
		h.mu.Unlock()
	}
	return v.guestIP, nil
}

// bootSlot provisions one pool VM and parks it idle; failures stay silent
// for the next demand to retry. The caller must hold a claimed refill slot.
func (h *Host) bootSlot(ctx context.Context, key string, spec poolSpec) {
	if h.pl == nil {
		return
	}
	defer h.pl.dropInflight()
	id := poolID()
	token := newID()
	h.mu.Lock()
	h.fastSpecs[id] = fastSpec{token: token, workspace: "/home/user/work", ephemeral: true}
	h.mu.Unlock()
	if _, err := h.Provision(ctx, id, spec.res, spec.image, ""); err != nil {
		h.mu.Lock()
		delete(h.fastSpecs, id)
		h.mu.Unlock()
		return
	}
	if err := h.waitFastAgent(ctx, id); err != nil {
		_ = h.Destroy(ctx, id)
		return
	}
	if !h.pl.push(poolSlot{id: id, res: spec.res, key: key}) {
		_ = h.Destroy(ctx, id)
	}
}

// refill tops the pool back to target in the background; misses never wait.
func (h *Host) refill(key string, spec poolSpec) {
	if h.pl == nil {
		return
	}
	for h.pl.claimRefill() {
		go h.bootSlot(context.WithoutCancel(context.Background()), key, spec)
	}
}
