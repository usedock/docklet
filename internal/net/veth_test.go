package net

import (
	"errors"
	"strings"
	"testing"
)

func TestVethForStableAndShort(t *testing.T) {
	a, b := VethFor("dock-1"), VethFor("dock-1")
	if a != b || len(a) > 15 || !strings.HasPrefix(a, "dv-") {
		t.Fatalf("veth = %q, want stable dv- name <=15", a)
	}
	if VethFor("dock-2") == a {
		t.Fatal("distinct docks need distinct veth names")
	}
}

func TestEnsureVethIdempotent(t *testing.T) {
	var calls []string
	run := func(name string, args ...string) error {
		if name == "sysctl" {
			return nil
		}
		calls = append(calls, name+" "+strings.Join(args, " "))
		if args[0] == "link" && args[1] == "add" {
			return errors.New("RTNETLINK answers: File exists")
		}
		return nil
	}
	if err := EnsureVeth(run, "dv-abc12345"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %v, want add/up", calls)
	}
}

func TestEnsureVethRecoversStale(t *testing.T) {
	calls := 0
	run := func(name string, args ...string) error {
		if name == "sysctl" {
			return nil
		}
		calls++
		if calls == 1 {
			return errors.New("RTNETLINK answers: File exists")
		}
		if calls == 2 {
			return errors.New("Cannot find device")
		}
		return nil
	}
	if err := EnsureVeth(run, "dv-stale123"); err != nil {
		t.Fatal(err)
	}
	if calls != 5 {
		t.Fatalf("calls = %d, want add/up/del/add/up", calls)
	}
}

func TestDeleteVethIgnoresAbsence(t *testing.T) {
	run := func(name string, args ...string) error {
		return errors.New("Cannot find device \"dv-gone\"")
	}
	if err := DeleteVeth(run, "dv-gone"); err != nil {
		t.Fatal(err)
	}
}

func TestAttachVethMovesAndAddresses(t *testing.T) {
	var calls []string
	run := func(name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	}
	if err := AttachVeth(run, "dv-abc12345", 4242, "192.168.127.2", "192.168.127.1", "24"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{"netns 4242", "name eth0", "addr add 192.168.127.2/24", "default via 192.168.127.1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("calls = %v, want %q", calls, want)
		}
	}
}

func TestGatewayAndNATReuseForVeth(t *testing.T) {
	run := func(name string, args ...string) error { return nil }
	if err := EnsureGateway(run, "dv-abc12345", "192.168.127.1", "192.168.127.2"); err != nil {
		t.Fatalf("gateway must accept veth names: %v", err)
	}
	if err := EnsureNAT(run, "192.168.127.0/24", nil, 0); err != nil {
		t.Fatalf("NAT must accept the shared subnet: %v", err)
	}
}

func TestEnsureVethDisablesIPv6(t *testing.T) {
	var calls []string
	run := func(name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	}
	if err := EnsureVeth(run, "dv-abc12345"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(calls, "\n"), "sysctl -w net.ipv6.conf.dv-abc12345.disable_ipv6=1") {
		t.Fatalf("calls = %q", calls)
	}
}
