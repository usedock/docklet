package host

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultCgroupWait = 5 * time.Second
	cgroupPoll        = 50 * time.Millisecond
	// freezeWait bounds how long a cgroup may take to report frozen.
	freezeWait = time.Second
	// SweepInterval is how often leaked uids are looked for.
	SweepInterval = 10 * time.Minute
)

// killCgroup kills every process left in a dock's jailer cgroup, waits for it
// to empty and removes it. It reports false when processes survive, so the
// caller keeps the dock's uid reserved instead of handing it to a new dock.
func (h *Host) killCgroup(dockID string) bool {
	if h.cfg.CgroupBase == "" {
		return true
	}
	dir := filepath.Join(h.cfg.CgroupBase, "firecracker", dockID)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return true
	}
	wait := h.cfg.CgroupWait
	if wait <= 0 {
		wait = defaultCgroupWait
	}
	kill := h.cfg.KillPID
	if kill == nil {
		kill = func(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
	}
	deadline := time.Now().Add(wait)
	v2 := false
	if _, err := os.Stat(filepath.Join(dir, "cgroup.kill")); err == nil {
		v2 = os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644) == nil
	}
	if !v2 {
		freezeKill(dir, kill, deadline)
	}
	for {
		pids, err := cgroupPids(dir)
		if err == nil && len(pids) == 0 {
			break
		}
		if !v2 {
			// cgroup v1 has no cgroup.kill: signal the listed pids.
			for _, p := range pids {
				_ = kill(p)
			}
		}
		if time.Now().After(deadline) {
			log.Printf("dock %s: cgroup %s still has processes after %s; keeping its uid reserved", dockID, dir, wait)
			return false
		}
		time.Sleep(cgroupPoll)
	}
	_ = os.RemoveAll(dir)
	if _, err := os.Stat(dir); err == nil {
		log.Printf("dock %s: cgroup %s could not be removed; keeping its uid reserved", dockID, dir)
		return false
	}
	return true
}

// freezeKill stops a cgroup on a kernel without cgroup.kill: freeze it so
// nothing can fork, signal every listed pid, then thaw so the dead can exit.
// A cgroup without cgroup.freeze (v1) is left to the plain list-and-kill loop.
func freezeKill(dir string, kill func(int) error, deadline time.Time) {
	freeze := filepath.Join(dir, "cgroup.freeze")
	if _, err := os.Stat(freeze); err != nil || os.WriteFile(freeze, []byte("1"), 0o644) != nil {
		return
	}
	limit := time.Now().Add(freezeWait)
	if limit.After(deadline) {
		limit = deadline
	}
	for !cgroupFrozen(dir) && time.Now().Before(limit) {
		time.Sleep(cgroupPoll)
	}
	pids, _ := cgroupPids(dir)
	for _, p := range pids {
		_ = kill(p)
	}
	_ = os.WriteFile(freeze, []byte("0"), 0o644)
}

func cgroupFrozen(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "cgroup.events"))
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) == "frozen 1" {
			return true
		}
	}
	return false
}

// RunSweeper frees leaked uids every interval until ctx ends.
func (h *Host) RunSweeper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.sweepUIDs()
		}
	}
}

// dockLock is one dock id's lock; refs lets the map drop idle entries.
type dockLock struct {
	mu   sync.Mutex
	refs int
}

// lockDock serializes Provision, Destroy and the uid sweep on one dock id.
func (h *Host) lockDock(id string) func() {
	e := h.dockEntry(id)
	e.mu.Lock()
	return func() { e.mu.Unlock(); h.dockDrop(id) }
}

// tryLockDock is lockDock that gives up when the dock is busy.
func (h *Host) tryLockDock(id string) (func(), bool) {
	e := h.dockEntry(id)
	if !e.mu.TryLock() {
		h.dockDrop(id)
		return nil, false
	}
	return func() { e.mu.Unlock(); h.dockDrop(id) }, true
}

func (h *Host) dockEntry(id string) *dockLock {
	h.lockMu.Lock()
	defer h.lockMu.Unlock()
	if h.dockLocks == nil {
		h.dockLocks = map[string]*dockLock{}
	}
	e := h.dockLocks[id]
	if e == nil {
		e = &dockLock{}
		h.dockLocks[id] = e
	}
	e.refs++
	return e
}

func (h *Host) dockDrop(id string) {
	h.lockMu.Lock()
	defer h.lockMu.Unlock()
	if e := h.dockLocks[id]; e != nil {
		if e.refs--; e.refs <= 0 {
			delete(h.dockLocks, id)
		}
	}
}

// sweepUIDs frees uids left reserved by a destroy that could not empty the
// dock's cgroup, or by a crash mid-destroy: the dock is untracked and has no
// jail or vms dir left. Its cgroup is killed first; a uid whose cgroup cannot
// be emptied stays reserved. Runs in New, then on a timer; a dock being
// provisioned or destroyed is skipped.
func (h *Host) sweepUIDs() {
	if h.uids == nil {
		return
	}
	for _, id := range h.uids.Docks() {
		if checkIDs(id) != nil {
			continue
		}
		if unlock, ok := h.tryLockDock(id); ok {
			h.sweepOne(id)
			unlock()
		}
	}
}

func (h *Host) sweepOne(id string) {
	h.mu.Lock()
	_, tracked := h.vms[id]
	for _, v := range h.vms {
		tracked = tracked || v.backing == id
	}
	h.mu.Unlock()
	if tracked {
		return
	}
	for _, p := range []string{h.jailRoot(id), h.jailRoot(id) + ".failed", filepath.Join(h.cfg.VMsDir, id)} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			return
		}
	}
	if !h.killCgroup(id) {
		return
	}
	log.Printf("dock %s: freeing uid left reserved by an earlier destroy", id)
	h.releaseOwner(id)
}

func cgroupPids(dir string) ([]int, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, f := range strings.Fields(string(raw)) {
		if n, err := strconv.Atoi(f); err == nil {
			pids = append(pids, n)
		}
	}
	return pids, nil
}
