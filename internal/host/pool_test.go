package host

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/usedock/docklet/api/v1"
)

// seedSlot parks a fake booted pool VM: tracked VM, allocated IP, idle entry.
func seedSlot(t *testing.T, h *Host, res apiv1.Resources) poolSlot {
	t.Helper()
	id := poolID()
	token := "pool-token"
	h.mu.Lock()
	h.vms[id] = &vm{tap: TapFor(id), guestIP: "", pid: 4242, running: true, fastToken: token, fastWorkspace: "/home/user/work"}
	h.mu.Unlock()
	ip, _, _, err := h.alloc.Assign(id)
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.vms[id].guestIP = ip
	h.mu.Unlock()
	slot := poolSlot{id: id, res: res, key: manifestKey(res, apiv1.DefaultImage)}
	if !h.pl.push(slot) {
		t.Fatal("pool must accept seed slot")
	}
	return slot
}

func stubAgent(t *testing.T, h *Host) {
	t.Helper()
	h.cfg.AgentCall = func(_ context.Context, _, _ string, req fastAgentRequest) (fastAgentResponse, error) {
		return fastAgentResponse{OK: true}, nil
	}
}

func TestPoolClaimFastPath(t *testing.T) {
	ctx := context.Background()
	h, driver, _ := testHost(t)
	h.pl = newPool(2)
	stubAgent(t, h)
	slot := seedSlot(t, h, resSmall)
	h.pl.target = 0 // suppress background refill for deterministic counts
	slotIP := h.vms[slot.id].guestIP

	ip, err := h.ProvisionFast(ctx, "run-1", resSmall, "", "dock-token", "/home/user/work", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ip != slotIP {
		t.Fatalf("claim must keep slot ip %s, got %s", slotIP, ip)
	}
	if driver.boots != 0 {
		t.Fatalf("claim must not boot, boots=%d", driver.boots)
	}
	got, err := h.fastVM("run-1")
	if err != nil || got.fastToken != "dock-token" || got.tap != TapFor(slot.id) {
		t.Fatalf("claimed vm = %+v %v", got, err)
	}
	if _, err := h.fastVM(slot.id); err == nil {
		t.Fatal("slot id must be gone after claim")
	}
	if byDock := h.alloc.ByDock(); byDock["run-1"] != slotIP {
		t.Fatalf("ip must move to dock, byDock=%v", byDock)
	}
}

func TestPoolMissFallsBackToSlowPath(t *testing.T) {
	ctx := context.Background()
	h, driver, _ := testHost(t)
	h.pl = newPool(2)
	stubAgent(t, h)
	h.pl.target = 0 // suppress background refill for deterministic counts
	ip, err := h.ProvisionFast(ctx, "run-1", resSmall, "", "tok", "/home/user/work", nil)
	if err != nil || ip == "" {
		t.Fatalf("miss must provision, ip=%s err=%v", ip, err)
	}
	if driver.boots != 1 {
		t.Fatalf("miss must boot once, boots=%d", driver.boots)
	}
}

func TestPoolSizeMismatchKeepsSlot(t *testing.T) {
	ctx := context.Background()
	h, driver, _ := testHost(t)
	h.pl = newPool(2)
	stubAgent(t, h)
	seedSlot(t, h, resSmall)
	h.pl.target = 0 // suppress background refill for deterministic counts
	if _, err := h.ProvisionFast(ctx, "run-1", resLarge, "", "tok", "/home/user/work", nil); err != nil {
		t.Fatal(err)
	}
	if driver.boots != 1 {
		t.Fatalf("mismatch must boot fresh, boots=%d", driver.boots)
	}
	if n := idleCount(h.pl); n != 1 {
		t.Fatalf("unmatched slot must stay idle, idle=%d", n)
	}
}

func TestPoolSkipsDeadSlots(t *testing.T) {
	ctx := context.Background()
	h, driver, _ := testHost(t)
	h.pl = newPool(2)
	calls := 0
	h.cfg.AgentCall = func(_ context.Context, _, _ string, req fastAgentRequest) (fastAgentResponse, error) {
		calls++
		if req.Op == "rekey" {
			return fastAgentResponse{}, errors.New("connection refused")
		}
		return fastAgentResponse{OK: true}, nil
	}
	seedSlot(t, h, resSmall)
	h.pl.target = 0 // suppress background refill for deterministic counts
	if _, err := h.ProvisionFast(ctx, "run-1", resSmall, "", "tok", "/home/user/work", nil); err != nil {
		t.Fatal(err)
	}
	if driver.boots != 1 {
		t.Fatalf("dead slot must fall back to boot, boots=%d", driver.boots)
	}
	if n := idleCount(h.pl); n != 0 {
		t.Fatalf("dead slot must be gone, idle=%d", n)
	}
}

func TestDestroyUsesTrackedTap(t *testing.T) {
	ctx := context.Background()
	h, _, _ := testHost(t)
	if _, err := h.Provision(ctx, "dock-1", resDefault, "", ""); err != nil {
		t.Fatal(err)
	}
	var deleted []string
	h.cfg.NetRunner = func(name string, args ...string) error {
		if name == "ip" && len(args) > 2 && args[0] == "tuntap" && args[1] == "del" {
			deleted = append(deleted, args[3])
		}
		return nil
	}
	h.mu.Lock()
	h.vms["dock-1"].tap = "dt-poolslot"
	h.mu.Unlock()
	if err := h.Destroy(ctx, "dock-1"); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "dt-poolslot" {
		t.Fatalf("destroy must delete tracked tap, got %v", deleted)
	}
}

func TestReassignKeepsIP(t *testing.T) {
	h, _, _ := testHost(t)
	ip, _, _, err := h.alloc.Assign("pool-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.alloc.Reassign("pool-1", "dock-9"); err != nil {
		t.Fatal(err)
	}
	byDock := h.alloc.ByDock()
	if byDock["dock-9"] != ip {
		t.Fatalf("reassign must keep ip %s, got %v", ip, byDock)
	}
	if _, ok := byDock["pool-1"]; ok {
		t.Fatalf("old id must be gone, got %v", byDock)
	}
	if err := h.alloc.Reassign("ghost", "dock-10"); err == nil || !strings.Contains(err.Error(), "no IP") {
		t.Fatalf("unknown id must fail, got %v", err)
	}
}

func TestPoolRefillTopsUpToTarget(t *testing.T) {
	h, _, _ := testHost(t)
	h.pl = newPool(3)
	stubAgent(t, h)
	spec := poolSpec{res: resSmall, image: apiv1.DefaultImage}
	h.refill(manifestKey(spec.res, spec.image), spec)
	deadline := time.Now().Add(10 * time.Second)
	for idleCount(h.pl) < 3 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := idleCount(h.pl); n != 3 {
		t.Fatalf("refill must top up to target, idle=%d", n)
	}
}

// idleCount reads the idle total under the pool lock.
func idleCount(p *pool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.totalIdle()
}
