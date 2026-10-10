package stats

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func write(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSample(t *testing.T) {
	root := t.TempDir()
	write(t, root, "cgroup/dock-1/cpu.stat", "usage_usec 500000\nuser_usec 1\n")
	write(t, root, "cgroup/dock-1/memory.current", "1048576\n")
	write(t, root, "cgroup/dock-1/memory.max", "4294967296\n")
	write(t, root, "cgroup/dock-1/io.stat", "8:0 rbytes=100 wbytes=200 rios=1 wios=2\n")
	write(t, root, "sys/class/net/tap-d1/statistics/rx_bytes", "1000\n")
	write(t, root, "sys/class/net/tap-d1/statistics/tx_bytes", "2000\n")
	write(t, root, "cgroup/dock-1/cpu.max", "200000 100000\n")
	cgroup := filepath.Join(root, "cgroup/dock-1")
	start := time.Unix(1000, 0)
	// 0.5s of CPU over 1s on a 2-core quota is 25% of the dock's allotment.
	prev := CPUMark{UsageUsec: 0, At: start.Add(-time.Second)}
	sample, mark, err := Reader{Root: root}.Sample("dock-1", cgroup, "tap-d1", prev, start)
	if err != nil {
		t.Fatal(err)
	}
	if sample.Source != "host" || sample.CPUPercent != 25 || sample.MemBytes != 1048576 || sample.MemLimit != 4294967296 {
		t.Fatalf("sample = %+v", sample)
	}
	if mark != (CPUMark{UsageUsec: 500000, At: start}) {
		t.Fatalf("mark = %+v, want the reading just taken", mark)
	}
	if sample.RxBytes != 1000 || sample.TxBytes != 2000 || sample.DiskBytes != 300 {
		t.Fatalf("io = %+v", sample)
	}
}

func TestMissingCpuFails(t *testing.T) {
	r := Reader{Root: t.TempDir()}
	if _, _, err := r.Sample("d", filepath.Join(t.TempDir(), "cgroup/missing"), "tap", CPUMark{}, time.Now()); err == nil {
		t.Fatal("missing cgroup must fail")
	}
}

func TestCPUPercentIsARateNotACounter(t *testing.T) {
	at := time.Unix(1000, 0)
	cur := CPUMark{UsageUsec: 2_000_000, At: at}
	cases := []struct {
		name string
		prev CPUMark
		want float64
	}{
		{"first sample has no rate", CPUMark{}, 0},
		{"idle dock reads zero", CPUMark{UsageUsec: 2_000_000, At: at.Add(-15 * time.Second)}, 0},
		{"one core busy for half the window", CPUMark{UsageUsec: 500_000, At: at.Add(-3 * time.Second)}, 50},
		{"counter reset after restore", CPUMark{UsageUsec: 9_000_000, At: at.Add(-time.Second)}, 0},
		{"bursts clamp to the quota", CPUMark{UsageUsec: 0, At: at.Add(-time.Second)}, 100},
	}
	for _, c := range cases {
		if got := cpuPercent(c.prev, cur, 1); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCPUQuotaUnlimitedUsesHost(t *testing.T) {
	root := t.TempDir()
	write(t, root, "cpu.max", "max 100000\n")
	if got := cpuQuota(filepath.Join(root, "cpu.max")); got != float64(runtime.NumCPU()) {
		t.Fatalf("quota = %v, want host cores %d", got, runtime.NumCPU())
	}
}

func TestAllocatedCountsBlocksNotApparentSize(t *testing.T) {
	dir := t.TempDir()
	sparse := filepath.Join(dir, "rootfs.ext4")
	f, err := os.Create(sparse)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(make([]byte, 1<<20), 0); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	got, err := Allocated(sparse)
	if err != nil {
		t.Fatal(err)
	}
	if got < 1<<20 || got >= 1<<29 {
		t.Fatalf("Allocated(sparse) = %d, want about 1 MiB of blocks, not the 1 GiB apparent size", got)
	}
}

func TestAllocatedCountsHardLinksOnceAndSkipsSymlinkTargets(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a/data", string(make([]byte, 256<<10)))
	one, err := Allocated(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(dir, "a/data"), filepath.Join(dir, "a/link")); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	write(t, outside, "big", string(make([]byte, 1<<20)))
	if err := os.Symlink(filepath.Join(outside, "big"), filepath.Join(dir, "a/sym")); err != nil {
		t.Fatal(err)
	}
	two, err := Allocated(dir)
	if err != nil {
		t.Fatal(err)
	}
	if one < 256<<10 || two-one >= 256<<10 {
		t.Fatalf("Allocated = %d then %d, want hard link and symlink target not double counted", one, two)
	}
	if _, err := Allocated(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("Allocated(missing) succeeded, want error")
	}
}
