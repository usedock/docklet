package fc

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

func argv(args ...string) []byte {
	var out []byte
	for _, a := range args {
		out = append(out, a...)
		out = append(out, 0)
	}
	return out
}

func TestCmdlineHasIDMatchesExactIDOnly(t *testing.T) {
	if !cmdlineHasID(argv("/usr/bin/firecracker", "--id", "dock-1", "--start-time-us", "1"), "dock-1", "") {
		t.Fatal("exact id must match")
	}
	if cmdlineHasID(argv("firecracker", "--id", "dock-12"), "dock-1", "") {
		t.Fatal("a prefix id must not match another dock's VM")
	}
	if cmdlineHasID(argv("firecracker", "--id", "dock-1"), "dock", "") {
		t.Fatal("a shorter id must not match")
	}
	if cmdlineHasID(argv("sleep", "--id", "dock-1"), "dock-1", "") {
		t.Fatal("a non-firecracker process must not match")
	}
	if cmdlineHasID(nil, "dock-1", "") || cmdlineHasID(argv("firecracker"), "dock-1", "") {
		t.Fatal("missing args must not match")
	}
}

func TestCmdlineHasIDHonoursConfiguredBinary(t *testing.T) {
	custom := "/opt/vmm/fc-custom"
	if !cmdlineHasID(argv(custom, "--id", "d1"), "d1", custom) {
		t.Fatal("the configured binary path must match")
	}
	if !cmdlineHasID(argv("fc-custom", "--id", "d1"), "d1", custom) {
		t.Fatal("the jailed base name of the configured binary must match")
	}
	if cmdlineHasID(argv("firecracker", "--id", "d1"), "d1", custom) {
		t.Fatal("another binary must not match a configured one")
	}
}

func fakeProc(t *testing.T, cmdlines map[int][]byte) string {
	t.Helper()
	root := t.TempDir()
	for pid, raw := range cmdlines {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestMatchPIDsUsesPidFileThenFallsBackToScan(t *testing.T) {
	proc := fakeProc(t, map[int][]byte{
		100: argv("firecracker", "--id", "dock-1"),
		200: argv("firecracker", "--id", "dock-12"),
		300: argv("firecracker", "--id", "dock-1"),
		400: argv("bash"),
	})
	jail := t.TempDir()
	if got := matchPIDs(proc, jail, "dock-1", ""); !reflect.DeepEqual(got, []int{100, 300}) {
		t.Fatalf("scan without a pid file = %v, want exact matches 100 and 300", got)
	}
	if err := os.WriteFile(filepath.Join(jail, "firecracker.pid"), []byte("300\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := matchPIDs(proc, jail, "dock-1", ""); !reflect.DeepEqual(got, []int{300}) {
		t.Fatalf("pid file hit = %v, want only 300", got)
	}
	if err := os.WriteFile(filepath.Join(jail, "firecracker.pid"), []byte("400\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := matchPIDs(proc, jail, "dock-1", ""); !reflect.DeepEqual(got, []int{100, 300}) {
		t.Fatalf("stale pid file = %v, want the scan result", got)
	}
	if got := matchPIDs(proc, jail, "dock-2", ""); len(got) != 0 {
		t.Fatalf("unknown dock = %v, want none", got)
	}
}
