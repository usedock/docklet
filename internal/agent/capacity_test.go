package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGuestMemMiBReservesHostShare(t *testing.T) {
	cases := []struct {
		total, reserve, want int64
		known                bool
	}{
		{0, 0, 0, false},
		{8192, 0, 7168, true},
		{65536, 0, 58983, true},
		{8192, 2048, 6144, true},
		{512, 0, 0, true},
		{4096, 8192, 0, true},
	}
	for _, c := range cases {
		got, known := guestMemMiB(c.total, c.reserve)
		if got != c.want || known != c.known {
			t.Fatalf("guestMemMiB(%d,%d) = %d,%v, want %d,%v", c.total, c.reserve, got, known, c.want, c.known)
		}
	}
}

func fakeMeminfo(t *testing.T, kib string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc/meminfo"), []byte("MemTotal: "+kib+" kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCapacityMarksExhaustedMemoryNegative(t *testing.T) {
	root := fakeMeminfo(t, "4194304")
	if _, mem := Capacity(root, 8192); mem != -1 {
		t.Fatalf("mem = %d, want -1 when the reserve covers all RAM", mem)
	}
	if _, mem := Capacity(root, 1024); mem != 3072 {
		t.Fatalf("mem = %d, want 3072", mem)
	}
	if _, mem := Capacity(t.TempDir(), 0); mem != 0 {
		t.Fatalf("mem = %d, want 0 (unknown) without meminfo", mem)
	}
}

func TestRegisterBodyReportsGuestMemoryBudget(t *testing.T) {
	root := fakeMeminfo(t, "8388608")
	body := Config{Root: root, HostID: "h1", Family: "f", MemoryReserveMiB: 2048}.registerBody()
	if body.MemBudgetBytes == nil || *body.MemBudgetBytes != 6144<<20 {
		t.Fatalf("memBudgetBytes = %v, want 6144 MiB", body.MemBudgetBytes)
	}
	full := Config{Root: fakeMeminfo(t, "1048576"), HostID: "h1", Family: "f", MemoryReserveMiB: 4096}.registerBody()
	if full.MemBudgetBytes == nil || *full.MemBudgetBytes != 0 {
		t.Fatalf("memBudgetBytes = %v, want a known zero", full.MemBudgetBytes)
	}
	unknown := Config{Root: t.TempDir(), HostID: "h1", Family: "f"}.registerBody()
	if unknown.MemBudgetBytes != nil {
		t.Fatalf("memBudgetBytes = %v, want unset when RAM is unknown", unknown.MemBudgetBytes)
	}
}

func TestMaxDocksUsesDefaultSize(t *testing.T) {
	const gib = int64(1) << 30
	cases := []struct {
		vcpu      int
		mem, disk int64
		want      int
		name      string
	}{
		{16, 64 * gib, 1000 * gib, 8, "cpu bound"},
		{64, 16 * gib, 1000 * gib, 4, "memory bound"},
		{64, 64 * gib, 100 * gib, 5, "disk bound"},
		{0, 0, 0, 1, "unknown totals still advertise one slot"},
		{1, 64 * gib, 1000 * gib, 1, "sub-default host rounds up to one"},
	}
	for _, c := range cases {
		if got := maxDocks(c.vcpu, c.mem, c.disk); got != c.want {
			t.Fatalf("%s: maxDocks(%d,%d,%d) = %d, want %d", c.name, c.vcpu, c.mem, c.disk, got, c.want)
		}
	}
}
