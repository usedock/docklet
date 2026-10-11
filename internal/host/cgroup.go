package host

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultCgroupWait = 5 * time.Second
	cgroupPoll        = 50 * time.Millisecond
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

// sweepUIDs frees uids left reserved by a destroy that could not empty the
// dock's cgroup, or by a crash mid-destroy: the dock is untracked and has no
// jail or vms dir left. Its cgroup is killed first; a uid whose cgroup cannot
// be emptied stays reserved. Runs once in New, before any provision.
func (h *Host) sweepUIDs() {
	if h.uids == nil {
		return
	}
	h.mu.Lock()
	tracked := map[string]bool{}
	for id, v := range h.vms {
		tracked[id] = true
		if v.backing != "" {
			tracked[v.backing] = true
		}
	}
	h.mu.Unlock()
	for _, id := range h.uids.Docks() {
		if tracked[id] || checkIDs(id) != nil {
			continue
		}
		left := false
		for _, p := range []string{h.jailRoot(id), h.jailRoot(id) + ".failed", filepath.Join(h.cfg.VMsDir, id)} {
			if _, err := os.Lstat(p); !os.IsNotExist(err) {
				left = true
			}
		}
		if left || !h.killCgroup(id) {
			continue
		}
		log.Printf("dock %s: freeing uid left reserved by an earlier destroy", id)
		h.releaseOwner(id)
	}
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
