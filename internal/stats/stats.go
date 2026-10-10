// Package stats samples host-truth resource usage per dock from cgroupfs
// and TAP counters. Guest-reported numbers never enter these samples;
// container breakdown arrives separately and is labeled untrusted.
package stats

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/usedock/docklet/api/v1"
)

// Reader samples one dock; Root prefixes host sysfs in production ("/")
// and fixtures in tests. Proc locates process cgroups, CgroupMount the
// cgroupfs they hang under; both default to /proc and /sys/fs/cgroup.
type Reader struct {
	Root        string
	Proc        string
	CgroupMount string
}

// CgroupDir resolves a microVM pid to its absolute cgroup directory.
func (r Reader) CgroupDir(pid int) (string, error) {
	rel, err := r.CgroupRel(pid)
	if err != nil {
		return "", err
	}
	mount := r.CgroupMount
	if mount == "" {
		mount = "/sys/fs/cgroup"
	}
	return filepath.Join(mount, rel), nil
}

// CgroupRel resolves the jailer cgroup path of a microVM pid via procfs.
func (r Reader) CgroupRel(pid int) (string, error) {
	proc := r.Proc
	if proc == "" {
		proc = "/proc"
	}
	raw, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		parts := strings.SplitN(line, "::", 2)
		if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" && strings.TrimSpace(parts[1]) != "/" {
			return strings.TrimSpace(parts[1]), nil
		}
	}
	return "", fmt.Errorf("no cgroup for pid %d", pid)
}

// CPUMark is a cumulative cgroup CPU reading kept between samples to derive a rate.
type CPUMark struct {
	UsageUsec int64
	At        time.Time
}

// Sample collects cpu, memory, disk, and network counters for a dock whose
// jailer cgroup lives at absolute cgroupDir and whose TAP is tap. CPUPercent
// is usage since prev as a share of the cgroup's CPU quota; it returns the
// mark the next sample should pass as prev.
func (r Reader) Sample(dockID, cgroupDir, tap string, prev CPUMark, now time.Time) (apiv1.StatsSample, CPUMark, error) {
	sample := apiv1.StatsSample{DockID: dockID, Source: "host"}
	cgroup := cgroupDir
	usec, err := cpuUsec(filepath.Join(cgroup, "cpu.stat"))
	if err != nil {
		return sample, prev, err
	}
	mark := CPUMark{UsageUsec: usec, At: now}
	sample.CPUPercent = cpuPercent(prev, mark, cpuQuota(filepath.Join(cgroup, "cpu.max")))
	if mem, err := readLong(filepath.Join(cgroup, "memory.current")); err == nil {
		sample.MemBytes = mem
	}
	if max, err := readLong(filepath.Join(cgroup, "memory.max")); err == nil {
		sample.MemLimit = max
	}
	if rx, tx, err := tapBytes(filepath.Join(r.Root, "sys/class/net", tap, "statistics")); err == nil {
		sample.RxBytes, sample.TxBytes = rx, tx
	}
	sample.DiskBytes = diskBytes(cgroup)
	return sample, mark, nil
}

// cpuPercent is 0 without a usable previous mark: first sample, clock skew,
// or a counter reset after the dock was restored into a fresh cgroup.
func cpuPercent(prev, cur CPUMark, cores float64) float64 {
	if prev.At.IsZero() || !cur.At.After(prev.At) || cur.UsageUsec < prev.UsageUsec {
		return 0
	}
	pct := float64(cur.UsageUsec-prev.UsageUsec) / (float64(cur.At.Sub(prev.At).Microseconds()) * cores) * 100
	return min(pct, 100)
}

// cpuQuota reads cpu.max ("<quota> <period>") as allotted cores; an unlimited
// or unreadable quota falls back to the whole host.
func cpuQuota(path string) float64 {
	raw, err := os.ReadFile(path)
	if err == nil {
		fields := strings.Fields(string(raw))
		if len(fields) == 2 {
			quota, qErr := strconv.ParseFloat(fields[0], 64)
			period, pErr := strconv.ParseFloat(fields[1], 64)
			if qErr == nil && pErr == nil && quota > 0 && period > 0 {
				return quota / period
			}
		}
	}
	return float64(runtime.NumCPU())
}

func cpuUsec(path string) (int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "usage_usec") {
			return strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "usage_usec")), 10, 64)
		}
	}
	return 0, fmt.Errorf("usage_usec missing in %s", path)
}

func readLong(path string) (int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
}

func tapBytes(dir string) (int64, int64, error) {
	rx, err := readLong(filepath.Join(dir, "rx_bytes"))
	if err != nil {
		return 0, 0, err
	}
	tx, err := readLong(filepath.Join(dir, "tx_bytes"))
	if err != nil {
		return 0, 0, err
	}
	return rx, tx, nil
}

// diskBytes sums io rbytes+wbytes as the disk activity counter.
func diskBytes(cgroup string) int64 {
	raw, err := os.ReadFile(filepath.Join(cgroup, "io.stat"))
	if err != nil {
		return 0
	}
	var total int64
	for _, line := range strings.Split(string(raw), "\n") {
		for _, field := range strings.Fields(line) {
			if strings.HasPrefix(field, "rbytes=") || strings.HasPrefix(field, "wbytes=") {
				if n, err := strconv.ParseInt(strings.SplitN(field, "=", 2)[1], 10, 64); err == nil {
					total += n
				}
			}
		}
	}
	return total
}
