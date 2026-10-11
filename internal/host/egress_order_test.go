package host

import (
	"context"
	"strings"
	"testing"

	"github.com/usedock/docklet/internal/egress"
)

// dropOrder records the confinement DROP rule and the boot into one ordered log.
func dropOrder(h *Host, log *[]string, setBoot func(func())) {
	inner := h.cfg.NetRunner
	h.cfg.NetRunner = func(name string, args ...string) error {
		if name == "iptables" && len(args) > 0 && args[0] == "-I" && strings.HasSuffix(strings.Join(args, " "), "-j DROP") && !strings.Contains(strings.Join(args, " "), "DOCKLET-GUEST") {
			*log = append(*log, "drop")
		}
		if name == "iptables" && len(args) > 0 && (args[0] == "-C" || args[0] == "-D") {
			return errNoRule
		}
		return inner(name, args...)
	}
	setBoot(func() { *log = append(*log, "boot") })
}

func assertDropBeforeBoot(t *testing.T, log []string) {
	t.Helper()
	boot := -1
	for i, e := range log {
		if e == "boot" {
			boot = i
			break
		}
	}
	if boot < 1 || log[0] != "drop" {
		t.Fatalf("egress rules must land before boot, order=%v", log)
	}
}

func TestMicroVMProvisionConfinesBeforeBoot(t *testing.T) {
	h, driver, _ := testHost(t)
	h.cfg.Egress = egress.New()
	h.cfg.EgressPort = 3128
	stubAgent(t, h)
	var log []string
	dropOrder(h, &log, func(f func()) { driver.onBoot = f })
	if _, err := h.ProvisionWithAgent(context.Background(), "dock-1", resSmall, "", "", []string{"github.com"}); err != nil {
		t.Fatal(err)
	}
	assertDropBeforeBoot(t, log)
}

func TestMicroVMSnapshotLoadConfinesBeforeBoot(t *testing.T) {
	h, driver, _ := testHost(t)
	h.cfg.Egress = egress.New()
	h.cfg.EgressPort = 3128
	recordIptables(h)
	snap := snapshotFor(t, h)
	h.fastSpecs["dock-1"] = fastSpec{token: "tok", workspace: "/home/user/work", egress: []string{"github.com"}}
	var log []string
	dropOrder(h, &log, func(f func()) { driver.onBoot = f })
	if _, err := h.SnapshotLoad(context.Background(), "dock-1", resSmall, snap); err != nil {
		t.Fatal(err)
	}
	assertDropBeforeBoot(t, log)
}

func TestGVisorProvisionConfinesBeforeBoot(t *testing.T) {
	h, driver, _, _ := testContainerHost(t)
	h.cfg.Egress = egress.New()
	h.cfg.EgressPort = 3128
	stubAgent(t, h)
	var log []string
	dropOrder(h, &log, func(f func()) { driver.onBoot = f })
	if _, err := h.ProvisionWithAgent(context.Background(), "dock-1", resSmall, "", "", []string{"github.com"}); err != nil {
		t.Fatal(err)
	}
	assertDropBeforeBoot(t, log)
}

func TestGVisorSnapshotLoadConfinesBeforeBoot(t *testing.T) {
	ctx := context.Background()
	h, driver, _, _ := testContainerHost(t)
	h.cfg.Egress = egress.New()
	h.cfg.EgressPort = 3128
	stubAgent(t, h)
	recordIptables(h)
	if _, err := h.Provision(ctx, "dock-1", resSmall, "", ""); err != nil {
		t.Fatal(err)
	}
	snap, _, _, err := h.SnapshotCreate(ctx, "dock-1", nil, "full", false, false)
	if err != nil {
		t.Fatal(err)
	}
	h.fastSpecs["dock-1"] = fastSpec{token: "tok", workspace: "/home/user/work", egress: []string{"github.com"}}
	var log []string
	dropOrder(h, &log, func(f func()) { driver.onBoot = f })
	if _, err := h.SnapshotLoad(ctx, "dock-1", resSmall, snap); err != nil {
		t.Fatal(err)
	}
	assertDropBeforeBoot(t, log)
}

func TestConfinedBootAbortsBeforeBootWhenRulesFail(t *testing.T) {
	h, driver, _ := testHost(t)
	h.cfg.Egress = egress.New()
	h.cfg.EgressPort = 3128
	stubAgent(t, h)
	inner := h.cfg.NetRunner
	h.cfg.NetRunner = func(name string, args ...string) error {
		if name == "iptables" && len(args) > 0 && args[0] == "-I" && strings.Contains(strings.Join(args, " "), "-j DROP") && !strings.Contains(strings.Join(args, " "), "DOCKLET-GUEST") {
			return errNoRule
		}
		if name == "iptables" && len(args) > 0 && (args[0] == "-C" || args[0] == "-D") {
			return errNoRule
		}
		return inner(name, args...)
	}
	if _, err := h.ProvisionWithAgent(context.Background(), "dock-1", resSmall, "", "", []string{"github.com"}); err == nil {
		t.Fatal("rule failure must abort the boot")
	}
	if driver.boots != 0 {
		t.Fatalf("boots = %d, want 0", driver.boots)
	}
}
