package net

import (
	"errors"
	"strings"
	"testing"
)

func TestEnsureTAPIdempotent(t *testing.T) {
	var calls []string
	run := func(name string, args ...string) error {
		if name == "sysctl" {
			return nil
		}
		calls = append(calls, name+" "+args[0])
		if args[0] == "tuntap" {
			return errors.New("RTNETLINK answers: File exists")
		}
		return nil
	}
	if err := EnsureTAP(run, "tap-d1", 1000); err != nil {
		t.Fatal(err)
	}
	want := []string{"ip tuntap", "ip link", "ip addr"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want add/up/flush", calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
	}
}

func TestEnsureTAPRecoversStale(t *testing.T) {
	calls := 0
	run := func(name string, args ...string) error {
		if name == "sysctl" {
			return nil
		}
		calls++
		switch {
		case args[0] == "tuntap" && args[1] == "add":
			if calls == 1 {
				return errors.New("RTNETLINK answers: File exists")
			}
			return nil
		case args[0] == "link" && calls == 2:
			return errors.New("Cannot find device")
		case args[0] == "tuntap" && args[1] == "del":
			return nil
		}
		return nil
	}
	if err := EnsureTAP(run, "tap-stale", 1000); err != nil {
		t.Fatal(err)
	}
	if calls != 5 {
		t.Fatalf("calls = %d, want add/up/del/add/up", calls)
	}
}

func TestAssignStableAndRelease(t *testing.T) {
	a, err := OpenAllocator(t.TempDir(), "192.168.127.0/24")
	if err != nil {
		t.Fatal(err)
	}
	guest, gateway, mask, err := a.Assign("dock-1")
	if err != nil || guest != "192.168.127.2" || gateway != "192.168.127.1" || mask != "24" {
		t.Fatalf("assign = %s %s %s %v", guest, gateway, mask, err)
	}
	again, _, _, _ := a.Assign("dock-1")
	if again != guest {
		t.Fatal("reassign must be stable")
	}
	other, _, _, _ := a.Assign("dock-2")
	if other == guest {
		t.Fatal("second dock needs its own IP")
	}
	if err := a.Release("dock-1"); err != nil {
		t.Fatal(err)
	}
	fresh, _, _, _ := a.Assign("dock-3")
	if fresh != guest {
		t.Fatalf("released IP should recycle, got %s", fresh)
	}
}
func TestEnsureGatewayToleratesExisting(t *testing.T) {
	var calls []string
	run := func(name string, args ...string) error {
		calls = append(calls, name+" "+args[0]+" "+args[1])
		if args[0] == "addr" && args[1] == "add" {
			return errors.New("RTNETLINK answers: File exists")
		}
		return nil
	}
	if err := EnsureGateway(run, "tap-d1", "192.168.127.1", "192.168.127.2"); err != nil {
		t.Fatal(err)
	}
	want := []string{"ip addr del", "ip addr add", "ip route replace"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want del/add/route", calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
	}
}

func TestEnsureGatewayToleratesAssigned(t *testing.T) {
	run := func(name string, args ...string) error {
		if args[0] == "addr" && args[1] == "add" {
			return errors.New("Error: ipv4: Address already assigned.")
		}
		return nil
	}
	if err := EnsureGateway(run, "tap-d1", "192.168.127.1", "192.168.127.2"); err != nil {
		t.Fatal(err)
	}
}

// natRecorder fakes the runner; checkFails makes every -C report a missing rule.
func natRecorder(checkFails bool) (*[]string, func(string, ...string) error) {
	var calls []string
	return &calls, func(name string, args ...string) error {
		joined := name + " " + strings.Join(args, " ")
		calls = append(calls, joined)
		if checkFails && (strings.Contains(joined, " -C ") || strings.HasPrefix(joined, "iptables -C ")) {
			return errors.New("iptables: Bad rule")
		}
		return nil
	}
}

func TestEnsureNATSkipsHooksWhenPresent(t *testing.T) {
	calls, run := natRecorder(false)
	if err := EnsureNAT(run, "192.168.127.0/24", nil, 0); err != nil {
		t.Fatal(err)
	}
	for _, c := range *calls {
		if strings.Contains(c, " -A ") || strings.Contains(c, " -I ") {
			t.Fatalf("converged hooks and nat must not mutate, got %q", c)
		}
	}
}

func TestEnsureNATAddsWhenMissing(t *testing.T) {
	// Docker sets FORWARD to DROP, so masquerading alone strands guest egress.
	calls, run := natRecorder(true)
	if err := EnsureNAT(run, "192.168.127.0/24", nil, 0); err != nil {
		t.Fatal(err)
	}
	var added []string
	for _, c := range *calls {
		if !strings.Contains(c, "-C") && !strings.HasPrefix(c, "sh ") {
			added = append(added, c)
		}
	}
	want := []string{
		"iptables -I FORWARD -s 192.168.127.0/24 -j DOCKLET-GUEST",
		"iptables -I INPUT -s 192.168.127.0/24 -j DOCKLET-GUEST-IN",
		"iptables -t nat -A POSTROUTING -s 192.168.127.0/24 -j MASQUERADE",
		"iptables -t filter -A FORWARD -s 192.168.127.0/24 -j ACCEPT",
		"iptables -t filter -A FORWARD -d 192.168.127.0/24 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
	}
	if strings.Join(added, "|") != strings.Join(want, "|") {
		t.Fatalf("added = %q, want %q", added, want)
	}
}

func restoreDoc(t *testing.T, calls []string) string {
	t.Helper()
	for _, c := range calls {
		if strings.HasPrefix(c, "sh -c iptables-restore --noflush") {
			return c
		}
	}
	t.Fatalf("no iptables-restore call in %q", calls)
	return ""
}

func TestEnsureNATRestoreDocument(t *testing.T) {
	calls, run := natRecorder(false)
	if err := EnsureNAT(run, "192.168.127.0/24", []string{"10.1.0.0/24"}, 3128); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"*filter",
		":DOCKLET-GUEST - [0:0]",
		":DOCKLET-GUEST-IN - [0:0]",
		"-A DOCKLET-GUEST -d 192.168.127.0/24 -j DROP",
		"-A DOCKLET-GUEST -d 10.1.0.0/24 -j RETURN",
		"-A DOCKLET-GUEST -d 169.254.0.0/16 -j DROP",
		"-A DOCKLET-GUEST -d 10.0.0.0/8 -j DROP",
		"-A DOCKLET-GUEST -d 172.16.0.0/12 -j DROP",
		"-A DOCKLET-GUEST -d 192.168.0.0/16 -j DROP",
		"-A DOCKLET-GUEST -d 100.64.0.0/10 -j DROP",
		"-A DOCKLET-GUEST -d 127.0.0.0/8 -j DROP",
		"-A DOCKLET-GUEST-IN -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
		"-A DOCKLET-GUEST-IN -p icmp --icmp-type echo-request -j ACCEPT",
		"-A DOCKLET-GUEST-IN -d 192.168.127.1 -p tcp --dport 3128 -j ACCEPT",
		"-A DOCKLET-GUEST-IN -j DROP",
		"COMMIT",
	}, "\n")
	if got := restoreDoc(t, *calls); !strings.Contains(got, "\n"+want+"\n") {
		t.Fatalf("restore = %q, want body %q", got, want)
	}
}

func TestEnsureNATRestoreDropsRemovedOptIn(t *testing.T) {
	// Restore declares the whole chain, so a removed opt-in cannot linger.
	calls, run := natRecorder(false)
	if err := EnsureNAT(run, "192.168.127.0/24", []string{"10.1.0.0/24"}, 0); err != nil {
		t.Fatal(err)
	}
	if err := EnsureNAT(run, "192.168.127.0/24", nil, 0); err != nil {
		t.Fatal(err)
	}
	var docs []string
	for _, c := range *calls {
		if strings.HasPrefix(c, "sh -c iptables-restore --noflush") {
			docs = append(docs, c)
		}
	}
	if len(docs) != 2 || !strings.Contains(docs[0], "10.1.0.0/24 -j RETURN") || strings.Contains(docs[1], "RETURN") {
		t.Fatalf("restore docs = %q", docs)
	}
	if strings.Contains(docs[1], "dport") || !strings.Contains(docs[1], ":DOCKLET-GUEST - [0:0]") {
		t.Fatalf("second doc must redeclare chains without proxy port: %q", docs[1])
	}
}

func TestEnsureNATRejectsBadCIDR(t *testing.T) {
	calls, run := natRecorder(false)
	if err := EnsureNAT(run, "192.168.127.0/24", []string{"10.0.0.0/8\nCOMMIT"}, 0); err == nil {
		t.Fatal("want error for malformed cidr")
	}
	if len(*calls) != 0 {
		t.Fatalf("no command may run, got %q", *calls)
	}
}

func TestEnsureTAPDisablesIPv6(t *testing.T) {
	calls, run := natRecorder(false)
	if err := EnsureTAP(run, "dt-abc", 1000); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "sysctl -w net.ipv6.conf.dt-abc.disable_ipv6=1") {
		t.Fatalf("calls = %q", *calls)
	}
}

func TestEnsureNATSkipsIPv6OptIns(t *testing.T) {
	calls, run := natRecorder(false)
	if err := EnsureNAT(run, "192.168.127.0/24", []string{"fd00::/8", "10.1.0.0/24"}, 0); err != nil {
		t.Fatal(err)
	}
	doc := restoreDoc(t, *calls)
	if strings.Contains(doc, "fd00") || !strings.Contains(doc, "-d 10.1.0.0/24 -j RETURN") {
		t.Fatalf("restore = %q, want IPv4 opt-in only", doc)
	}
}
