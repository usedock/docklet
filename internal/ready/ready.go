// Package ready gates docklet startup on hypervisor prerequisites. Every
// check reads under Root so tests run against fixture trees; production
// passes Root "/".
package ready

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Host describes the paths a readiness check inspects.
type Host struct {
	Root      string
	MinMemMiB int64
	// Runtime selects the isolation gate: "microvm" (default) requires
	// /dev/kvm; "gvisor" requires a reachable Docker daemon plus
	// cgroupsv2, with no KVM or TUN. Unknown values fail closed.
	Runtime string
	// Allow lists cpu-vulnerable:<name> reasons an operator explicitly
	// accepts (single-tenant test hosts only); empty fails closed.
	Allow []string
}

// Check verifies the runtime gate, cgroupsv2, TUN, memory, and
// CPU vulnerabilities, returning one named reason per failed prerequisite.
func Check(h Host) []string {
	var reasons []string
	needTun := true
	switch h.Runtime {
	case "", "microvm":
		if !readable(filepath.Join(h.Root, "dev/kvm")) {
			reasons = append(reasons, "no-kvm")
		}
	case "gvisor":
		if !dialable(filepath.Join(h.Root, "var/run/docker.sock")) {
			reasons = append(reasons, "no-docker")
		}
		needTun = false
	default:
		reasons = append(reasons, "unknown-runtime")
	}
	if !hasControllers(filepath.Join(h.Root, "sys/fs/cgroup/cgroup.controllers")) {
		reasons = append(reasons, "no-cgroup-v2")
	}
	if needTun && !readable(filepath.Join(h.Root, "dev/net/tun")) {
		reasons = append(reasons, "no-tun")
	}
	if memMiB(h) < h.MinMemMiB {
		reasons = append(reasons, "low-memory")
	}
	for _, r := range vulnerable(filepath.Join(h.Root, "sys/devices/system/cpu/vulnerabilities")) {
		if !allowed(h.Allow, r) {
			reasons = append(reasons, r)
		}
	}
	return reasons
}

// allowed matches a cpu-vulnerable:<name> reason against the allowlist,
// which may carry full reasons or short names.
func allowed(allow []string, reason string) bool {
	for _, a := range allow {
		if a == reason || "cpu-vulnerable:"+a == reason {
			return true
		}
	}
	return false
}

func readable(path string) bool {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// dialable checks a unix socket by connecting: sockets refuse open(2)
// with ENXIO, so readability probes always fail on them.
func dialable(path string) bool {
	conn, err := dialUnix(path)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// dialUnix connects to a unix socket with a short timeout.
func dialUnix(path string) (net.Conn, error) {
	return net.DialTimeout("unix", path, time.Second)
}

func hasControllers(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	controllers := " " + strings.TrimSpace(string(raw)) + " "
	return strings.Contains(controllers, " cpu ") && strings.Contains(controllers, " memory ") && strings.Contains(controllers, " io ")
}

func memMiB(h Host) int64 {
	raw, err := os.ReadFile(filepath.Join(h.Root, "proc/meminfo"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			var kb int64
			if _, err := fmt.Sscanf(strings.TrimPrefix(line, "MemTotal:"), "%d kB", &kb); err == nil {
				return kb / 1024
			}
		}
	}
	return 0
}

// vulnerable names CVE mitigations reporting Vulnerable, which fail closed:
// guests must not run on hosts with known-exploitable CPU flaws.
func vulnerable(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var reasons []string
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(string(raw)), "Vulnerable") {
			reasons = append(reasons, "cpu-vulnerable:"+entry.Name())
		}
	}
	return reasons
}
