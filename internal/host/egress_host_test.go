package host

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/usedock/docklet/api/v1"
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

// snapshotFor boots a plain dock and captures it so a test can resume it confined.
func snapshotFor(t *testing.T, h *Host) string {
	t.Helper()
	stubAgent(t, h)
	if _, err := h.ProvisionWithAgent(context.Background(), "dock-1", resSmall, "", "", nil); err != nil {
		t.Fatal(err)
	}
	snap, _, _, err := h.SnapshotCreate(context.Background(), "dock-1", nil, apiv1.SnapshotFull, false, false)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestSnapshotLoadConfinesResumedDock(t *testing.T) {
	ctx := context.Background()
	h, _, _ := testHost(t)
	h.cfg.Egress = egress.New()
	h.cfg.EgressPort = 3128
	calls := recordIptables(h)
	snap := snapshotFor(t, h)
	h.fastSpecs["dock-1"] = fastSpec{token: "tok", workspace: "/home/user/work", egress: []string{"github.com"}}
	ip, err := h.SnapshotLoad(ctx, "dock-1", resSmall, snap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "-I FORWARD -s "+ip+" -j DROP") || h.vms["dock-1"].egressGateway == "" {
		t.Fatalf("resumed dock must be confined, calls=%v", *calls)
	}
}

func TestSnapshotLoadFailsClosedWithoutProxy(t *testing.T) {
	ctx := context.Background()
	h, _, _ := testHost(t)
	snap := snapshotFor(t, h)
	h.fastSpecs["dock-1"] = fastSpec{token: "tok", workspace: "/home/user/work", egress: []string{"github.com"}}
	if _, err := h.SnapshotLoad(ctx, "dock-1", resSmall, snap); err == nil || !strings.Contains(err.Error(), "egress proxy not configured") {
		t.Fatalf("confined resume without a proxy must fail, err=%v", err)
	}
	if _, ok := h.vms["dock-1"]; ok {
		t.Fatal("refused resume must not be tracked")
	}
}

func TestSnapshotLoadWithoutAllowlistStaysOpen(t *testing.T) {
	ctx := context.Background()
	h, _, _ := testHost(t)
	h.cfg.Egress = egress.New()
	h.cfg.EgressPort = 3128
	calls := recordIptables(h)
	snap := snapshotFor(t, h)
	h.fastSpecs["dock-1"] = fastSpec{token: "tok", workspace: "/home/user/work"}
	if _, err := h.SnapshotLoad(ctx, "dock-1", resSmall, snap); err != nil {
		t.Fatal(err)
	}
	for _, c := range *calls {
		if strings.HasPrefix(c, "-I") && !strings.Contains(c, "DOCKLET-GUEST") {
			t.Fatalf("unconfined resume must get no drop rule, got %v", *calls)
		}
	}
	if h.vms["dock-1"].egressGateway != "" {
		t.Fatal("unconfined resume must not use the proxy")
	}
}

func TestContainerSnapshotLoadConfinement(t *testing.T) {
	ctx := context.Background()
	h, _, _, _ := testContainerHost(t)
	recordIptables(h)
	if _, err := h.Provision(ctx, "dock-1", resSmall, "", ""); err != nil {
		t.Fatal(err)
	}
	snap, _, _, err := h.SnapshotCreate(ctx, "dock-1", nil, "full", false, false)
	if err != nil {
		t.Fatal(err)
	}
	h.fastSpecs["dock-1"] = fastSpec{token: "tok", workspace: "/home/user/work", egress: []string{"github.com"}}
	if _, err := h.SnapshotLoad(ctx, "dock-1", resSmall, snap); err == nil || !strings.Contains(err.Error(), "egress proxy not configured") {
		t.Fatalf("confined container resume without a proxy must fail, err=%v", err)
	}
	h.cfg.Egress = egress.New()
	h.cfg.EgressPort = 3128
	if _, err := h.SnapshotLoad(ctx, "dock-1", resSmall, snap); err != nil {
		t.Fatal(err)
	}
	if h.vms["dock-1"].egressGateway == "" {
		t.Fatal("resumed container must be confined")
	}
}

func TestSnapshotLoadWithAgentConfinesWhenGivenAnAllowlist(t *testing.T) {
	ctx := context.Background()
	h, _, _ := testHost(t)
	h.cfg.Egress = egress.New()
	h.cfg.EgressPort = 3128
	calls := recordIptables(h)
	snap := snapshotFor(t, h)
	ip, err := h.SnapshotLoadWithAgent(ctx, "dock-1", resSmall, snap, []string{"github.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "-I FORWARD -s "+ip+" -j DROP") || h.vms["dock-1"].egressGateway == "" {
		t.Fatalf("resumed agent dock must be confined, calls=%v", *calls)
	}
}

// proxyVerdict asks the proxy to CONNECT from guestIP and returns its refusal text.
func proxyVerdict(p *egress.Proxy, guestIP, target string) string {
	req := httptest.NewRequest(http.MethodConnect, "http://"+target, nil)
	req.Host = target
	req.RemoteAddr = guestIP + ":40000"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec.Body.String()
}

func TestRescanReconfinesAdoptedDocks(t *testing.T) {
	ctx := context.Background()
	h, _, dir, _ := testContainerHost(t)
	h.cfg.Egress = egress.New()
	h.cfg.EgressPort = 3128
	var execEnv map[string]string
	h.cfg.AgentCall = func(_ context.Context, _, _ string, req fastAgentRequest) (fastAgentResponse, error) {
		if req.Op == "exec" {
			execEnv = req.Env
		}
		return fastAgentResponse{OK: true}, nil
	}
	confinedIP, err := h.ProvisionWithAgent(ctx, "dock-1", resSmall, "", "", []string{"github.com"})
	if err != nil {
		t.Fatal(err)
	}
	openIP, err := h.ProvisionWithAgent(ctx, "dock-2", resSmall, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"dock-1", "dock-2"} {
		if err := os.WriteFile(filepath.Join(dir, "jail", "firecracker", id, "root", "spec.json"), []byte(`{"dockId":"`+id+`","resources":{"cpus":1,"memoryMiB":2048,"diskGiB":10}}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A restarted docklet has an empty proxy and no tracked docks; the iptables rules remain.
	h.cfg.Egress = egress.New()
	h.mu.Lock()
	h.vms = map[string]*vm{}
	h.mu.Unlock()
	calls := recordIptables(h)
	h.Rescan()
	if got := proxyVerdict(h.cfg.Egress, confinedIP, "evil.example:443"); !strings.Contains(got, "denied") {
		t.Fatalf("adopted confined dock must get its policy back, proxy said %q", got)
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "-I FORWARD -s "+confinedIP+" -j DROP") {
		t.Fatalf("adopted confined dock must be re-restricted, calls=%v", *calls)
	}
	if _, err := h.FastExec(ctx, "dock-1", "true", nil, time.Second); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(execEnv["HTTPS_PROXY"], ":3128") {
		t.Fatalf("exec on an adopted confined dock must carry the proxy env, got %v", execEnv)
	}
	if got := proxyVerdict(h.cfg.Egress, openIP, "evil.example:443"); !strings.Contains(got, "no egress policy") || h.vms["dock-2"].egressGateway != "" {
		t.Fatalf("adopted open dock must stay unconfined, proxy said %q", got)
	}
}
