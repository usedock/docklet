package ready

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, content := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func healthy() map[string]string {
	return map[string]string{
		"dev/kvm":                          "",
		"dev/net/tun":                      "",
		"sys/fs/cgroup/cgroup.controllers": "cpuset cpu io memory hugetlb pids",
		"proc/meminfo":                     "MemTotal:       32768 kB\n",
		"sys/devices/system/cpu/vulnerabilities/mmio": "Not affected",
	}
}

func TestHealthy(t *testing.T) {
	if reasons := Check(Host{Root: fixture(t, healthy()), MinMemMiB: 16}); len(reasons) != 0 {
		t.Fatalf("reasons = %v", reasons)
	}
}

func TestMissingKvmAndCgroup(t *testing.T) {
	files := healthy()
	delete(files, "dev/kvm")
	files["sys/fs/cgroup/cgroup.controllers"] = "cpuset pids"
	reasons := Check(Host{Root: fixture(t, files), MinMemMiB: 16})
	for _, want := range []string{"no-kvm", "no-cgroup-v2"} {
		found := false
		for _, r := range reasons {
			found = found || r == want
		}
		if !found {
			t.Fatalf("reasons %v missing %s", reasons, want)
		}
	}
}

func TestVulnerableCpuFailsClosed(t *testing.T) {
	files := healthy()
	files["sys/devices/system/cpu/vulnerabilities/meltdown"] = "Vulnerable"
	reasons := Check(Host{Root: fixture(t, files), MinMemMiB: 16})
	if len(reasons) != 1 || reasons[0] != "cpu-vulnerable:meltdown" {
		t.Fatalf("reasons = %v", reasons)
	}
}

func TestAllowedVulnerabilitySkipped(t *testing.T) {
	files := healthy()
	files["sys/devices/system/cpu/vulnerabilities/gather_data_sampling"] = "Vulnerable"
	if reasons := Check(Host{Root: fixture(t, files), MinMemMiB: 16}); len(reasons) != 1 {
		t.Fatalf("reasons = %v, want the GDS finding", reasons)
	}
	if reasons := Check(Host{Root: fixture(t, files), MinMemMiB: 16, Allow: []string{"gather_data_sampling"}}); len(reasons) != 0 {
		t.Fatalf("reasons = %v, want allowlist to skip GDS", reasons)
	}
	if reasons := Check(Host{Root: fixture(t, files), MinMemMiB: 16, Allow: []string{"cpu-vulnerable:gather_data_sampling"}}); len(reasons) != 0 {
		t.Fatalf("reasons = %v, want full-reason allowlist to skip GDS", reasons)
	}
}

func TestRetiredPvmRuntimeFailsClosed(t *testing.T) {
	// PVM is retired: the runtime no longer has a gate and fails closed
	// as unknown, placing nothing.
	reasons := Check(Host{Root: fixture(t, healthy()), MinMemMiB: 16, Runtime: "pvm"})
	found := false
	for _, r := range reasons {
		found = found || r == "unknown-runtime"
	}
	if !found {
		t.Fatalf("reasons %v missing unknown-runtime", reasons)
	}
}

func TestUnknownRuntimeFailsClosed(t *testing.T) {
	reasons := Check(Host{Root: fixture(t, healthy()), MinMemMiB: 16, Runtime: "xen"})
	found := false
	for _, r := range reasons {
		found = found || r == "unknown-runtime"
	}
	if !found {
		t.Fatalf("reasons %v missing unknown-runtime", reasons)
	}
}

func gvisorHealthy(t *testing.T) string {
	t.Helper()
	files := map[string]string{
		"sys/fs/cgroup/cgroup.controllers":            "cpuset cpu io memory hugetlb pids",
		"proc/meminfo":                                "MemTotal:       32768 kB\n",
		"sys/devices/system/cpu/vulnerabilities/mmio": "Not affected",
	}
	root := fixture(t, files)
	sockListener(t, root)
	return root
}

// sockListener binds a real unix socket where the daemon check dials:
// sockets refuse open(2), so an empty fixture file would fail closed.
func sockListener(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, "var/run/docker.sock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
}

func TestGVisorRuntimeSkipsKvmAndTun(t *testing.T) {
	if reasons := Check(Host{Root: gvisorHealthy(t), MinMemMiB: 16, Runtime: "gvisor"}); len(reasons) != 0 {
		t.Fatalf("reasons = %v", reasons)
	}
}

func TestGVisorRuntimeRequiresDaemon(t *testing.T) {
	files := map[string]string{
		"sys/fs/cgroup/cgroup.controllers":            "cpuset cpu io memory hugetlb pids",
		"proc/meminfo":                                "MemTotal:       32768 kB\n",
		"sys/devices/system/cpu/vulnerabilities/mmio": "Not affected",
	}
	reasons := Check(Host{Root: fixture(t, files), MinMemMiB: 16, Runtime: "gvisor"})
	found := false
	for _, r := range reasons {
		found = found || r == "no-docker"
	}
	if !found {
		t.Fatalf("reasons %v missing no-docker", reasons)
	}
}

func TestGVisorRuntimeRequiresCgroupV2(t *testing.T) {
	root := gvisorHealthy(t)
	if err := os.WriteFile(filepath.Join(root, "sys/fs/cgroup/cgroup.controllers"), []byte("cpuset pids"), 0o644); err != nil {
		t.Fatal(err)
	}
	reasons := Check(Host{Root: root, MinMemMiB: 16, Runtime: "gvisor"})
	found := false
	for _, r := range reasons {
		found = found || r == "no-cgroup-v2"
	}
	if !found {
		t.Fatalf("reasons %v missing no-cgroup-v2", reasons)
	}
}
