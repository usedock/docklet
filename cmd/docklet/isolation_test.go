package main

import (
	"testing"

	"github.com/usedock/docklet/internal/fc"
	"github.com/usedock/docklet/internal/uid"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseIsolationDefaults(t *testing.T) {
	iso, err := parseIsolation(envOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := isolation{UIDBase: uid.DefaultBase, UIDSize: uid.DefaultSize, Limits: fc.Limits{DiskMBps: fc.LimitAuto, DiskIOPS: fc.LimitAuto, NetMBps: fc.LimitAuto, PidsLimit: fc.LimitAuto}}
	if iso != want {
		t.Fatalf("iso = %+v, want %+v", iso, want)
	}
}

func TestParseIsolationValues(t *testing.T) {
	iso, err := parseIsolation(envOf(map[string]string{
		"DOCKLET_UID_BASE": "300000", "DOCKLET_UID_SIZE": "100",
		"DOCKLET_DISK_MBPS": "50", "DOCKLET_DISK_IOPS": "0", "DOCKLET_NET_MBPS": "10", "DOCKLET_PIDS_LIMIT": "512",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := isolation{UIDBase: 300000, UIDSize: 100, Limits: fc.Limits{DiskMBps: 50, DiskIOPS: 0, NetMBps: 10, PidsLimit: 512}}
	if iso != want {
		t.Fatalf("iso = %+v, want %+v", iso, want)
	}
	legacy, err := parseIsolation(envOf(map[string]string{"DOCKLET_UID_BASE": "0"}))
	if err != nil || legacy.UIDBase != 0 {
		t.Fatalf("UID_BASE=0 = %+v, %v; want legacy shared uid", legacy, err)
	}
}

func TestParseIsolationRefusesInvalid(t *testing.T) {
	for _, env := range []map[string]string{
		{"DOCKLET_DISK_MBPS": "fast"},
		{"DOCKLET_DISK_IOPS": "-5"},
		{"DOCKLET_NET_MBPS": "1.5"},
		{"DOCKLET_PIDS_LIMIT": "-1"},
		{"DOCKLET_UID_BASE": "abc"},
		{"DOCKLET_UID_SIZE": "0"},
		{"DOCKLET_UID_BASE": "4294967290", "DOCKLET_UID_SIZE": "65536"},
	} {
		if _, err := parseIsolation(envOf(env)); err == nil {
			t.Fatalf("env %v accepted, want startup refusal", env)
		}
	}
}

func TestParseIsolationRefusesOversizedLimits(t *testing.T) {
	for _, env := range []map[string]string{
		{"DOCKLET_DISK_MBPS": "1000001"},
		{"DOCKLET_NET_MBPS": "9223372036854775807"},
		{"DOCKLET_DISK_IOPS": "10000001"},
		{"DOCKLET_PIDS_LIMIT": "4194305"},
	} {
		if _, err := parseIsolation(envOf(env)); err == nil {
			t.Fatalf("env %v accepted, want startup refusal", env)
		}
	}
	if _, err := parseIsolation(envOf(map[string]string{
		"DOCKLET_DISK_MBPS": "1000000", "DOCKLET_DISK_IOPS": "10000000", "DOCKLET_NET_MBPS": "1000000", "DOCKLET_PIDS_LIMIT": "4194304",
	})); err != nil {
		t.Fatalf("limits at the cap refused: %v", err)
	}
}
