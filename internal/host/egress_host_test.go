package host

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/usedock/docklet/internal/egress"
)

// recordIptables wraps the host runner and records iptables rule changes.
func recordIptables(h *Host) *[]string {
	var calls []string
	inner := h.cfg.NetRunner
	h.cfg.NetRunner = func(name string, args ...string) error {
		if name == "iptables" && (args[0] == "-I" || args[0] == "-D") {
			calls = append(calls, strings.Join(args, " "))
		}
		if name == "iptables" && args[0] == "-D" {
			return errNoRule
		}
		if name == "iptables" && args[0] == "-C" {
			return errNoRule
		}
		return inner(name, args...)
	}
	return &calls
}

type ruleErr struct{}

func (ruleErr) Error() string { return "no rule" }

var errNoRule = ruleErr{}

func TestConfinedRunDockGetsFirewallProxyPolicyAndEnv(t *testing.T) {
	ctx := context.Background()
	h, _, _ := testHost(t)
	h.cfg.Egress = egress.New()
	h.cfg.EgressPort = 3128
	calls := recordIptables(h)
	var execEnv map[string]string
	h.cfg.AgentCall = func(_ context.Context, _, _ string, req fastAgentRequest) (fastAgentResponse, error) {
		if req.Op == "exec" {
			execEnv = req.Env
		}
		return fastAgentResponse{OK: true}, nil
	}
	ip, err := h.ProvisionWithAgent(ctx, "dock-1", resSmall, "", "", []string{"github.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "-I FORWARD -s "+ip+" -j DROP") {
		t.Fatalf("confined guest must have its forwarded traffic dropped, calls=%v", *calls)
	}
	if _, err := h.FastExec(ctx, "dock-1", "true", map[string]string{"A": "b", "HTTPS_PROXY": "http://evil"}, time.Second); err != nil {
		t.Fatal(err)
	}
	if execEnv["A"] != "b" || execEnv["HTTPS_PROXY"] != "http://192.168.127.1:3128" || execEnv["no_proxy"] == "" {
		t.Fatalf("exec must carry the host proxy env over caller values, got %v", execEnv)
	}
	*calls = nil
	if err := h.Destroy(ctx, "dock-1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "-D FORWARD -s "+ip+" -j DROP") {
		t.Fatalf("destroy must release the firewall for IP reuse, calls=%v", *calls)
	}
}

func TestConfinedBootFailsClosedWithoutProxy(t *testing.T) {
	ctx := context.Background()
	h, _, _ := testHost(t)
	stubAgent(t, h)
	if _, err := h.ProvisionWithAgent(ctx, "dock-1", resSmall, "", "", []string{"github.com"}); err == nil || !strings.Contains(err.Error(), "egress proxy not configured") {
		t.Fatalf("confinement without a proxy must refuse the boot, err=%v", err)
	}
	if _, ok := h.vms["dock-1"]; ok {
		t.Fatal("refused boot must not be tracked")
	}
}

func TestPoolClaimConfinesWarmSlot(t *testing.T) {
	ctx := context.Background()
	h, _, _ := testHost(t)
	h.cfg.Egress = egress.New()
	h.cfg.EgressPort = 3128
	h.pl = newPool(2)
	stubAgent(t, h)
	slot := seedSlot(t, h, resSmall)
	h.pl.target = 0
	calls := recordIptables(h)
	ip, err := h.ProvisionFast(ctx, "run-1", resSmall, "", "tok", "/home/user/work", []string{"github.com"})
	if err != nil {
		t.Fatal(err)
	}
	_ = slot
	if !strings.Contains(strings.Join(*calls, "\n"), "-I FORWARD -s "+ip+" -j DROP") || h.vms["run-1"].egressGateway == "" {
		t.Fatalf("claimed slot must be confined, calls=%v", *calls)
	}
}

func TestUnconfinedDocksStayOpen(t *testing.T) {
	ctx := context.Background()
	h, _, _ := testHost(t)
	h.cfg.Egress = egress.New()
	h.cfg.EgressPort = 3128
	calls := recordIptables(h)
	stubAgent(t, h)
	if _, err := h.ProvisionWithAgent(ctx, "dock-1", resSmall, "", "", nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range *calls {
		if strings.HasPrefix(c, "-I") && !strings.Contains(c, "DOCKLET-GUEST") {
			t.Fatalf("unconfined dock must get no egress rules, got %v", *calls)
		}
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "-I FORWARD -s 192.168.127.0/24 -j DOCKLET-GUEST") {
		t.Fatalf("unconfined dock must be hooked to the guest deny chain, calls=%v", *calls)
	}
}
