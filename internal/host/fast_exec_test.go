package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/usedock/docklet/api/v1"
)

func TestFastExecForwardsPerCallEnv(t *testing.T) {
	ctx := context.Background()
	h, _, _ := testHost(t)
	var got fastAgentRequest
	h.cfg.AgentCall = func(_ context.Context, _, _ string, req fastAgentRequest) (fastAgentResponse, error) {
		got = req
		return fastAgentResponse{OK: true}, nil
	}
	if _, err := h.ProvisionFast(ctx, "run-1", resSmall, "", "tok", "/home/user/work", nil); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"GITHUB_CLONE_TOKEN": "ghs_x", "ANTHROPIC_API_KEY": "sk"}
	if _, err := h.FastExec(ctx, "run-1", "true", env, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if got.Op != "exec" || got.Env["GITHUB_CLONE_TOKEN"] != "ghs_x" || got.Env["ANTHROPIC_API_KEY"] != "sk" || got.TimeoutSeconds != 30 {
		t.Fatalf("exec must forward script env and budget, got %+v", got)
	}
}

func TestProvisionWithAgentEnablesExec(t *testing.T) {
	ctx := context.Background()
	h, driver, _ := testHost(t)
	var tokens []string
	h.cfg.AgentCall = func(_ context.Context, _, token string, _ fastAgentRequest) (fastAgentResponse, error) {
		tokens = append(tokens, token)
		return fastAgentResponse{OK: true}, nil
	}
	if _, err := h.ProvisionWithAgent(ctx, "dock-1", resSmall, "", "", nil); err != nil {
		t.Fatal(err)
	}
	if driver.boots != 1 {
		t.Fatalf("agent dock must boot once, boots=%d", driver.boots)
	}
	if _, err := h.FastExec(ctx, "dock-1", "true", nil, time.Second); err != nil {
		t.Fatalf("agent dock must accept exec: %v", err)
	}
	if len(tokens) < 2 || len(tokens[0]) != 64 || tokens[0] != tokens[len(tokens)-1] {
		t.Fatalf("host must mint one 32-byte agent token and reuse it, got %v", tokens)
	}
}

func TestPlainProvisionHasNoExecChannel(t *testing.T) {
	ctx := context.Background()
	h, _, _ := testHost(t)
	stubAgent(t, h)
	if _, err := h.Provision(ctx, "dock-1", resSmall, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.FastExec(ctx, "dock-1", "true", nil, time.Second); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("plain docks must not expose exec, err=%v", err)
	}
}

func TestGuestAgentCreatesArtifactRootsAtStartup(t *testing.T) {
	src := FastAgentProgramText()
	if !strings.Contains(src, "os.makedirs(_root, exist_ok=True)") || !strings.Contains(src, "os.chown(_root") {
		t.Fatal("guest agent must create and chown its roots; /run is tmpfs on VM guests")
	}
}

func TestGuestAgentDropsToDockUserOnlyAsRoot(t *testing.T) {
	// MicroVM agents run as root and must not run scripts as root; container
	// agents already run as the dock user, where runuser would refuse.
	src := FastAgentProgramText()
	if !strings.Contains(src, "if os.geteuid() == 0:\n        argv = ['/usr/sbin/runuser', '-u', 'user', '--'] + argv") {
		t.Fatal("guest agent must wrap scripts in runuser exactly when it runs as root")
	}
}

func TestFastExecClipsOutputAndReportsTimeout(t *testing.T) {
	ctx := context.Background()
	h, _, _ := testHost(t)
	var got fastAgentRequest
	big := strings.Repeat("x", MaxExecOutput+10)
	h.cfg.AgentCall = func(_ context.Context, _, _ string, req fastAgentRequest) (fastAgentResponse, error) {
		got = req
		return fastAgentResponse{OK: true, ExitCode: 124, Stdout: big, Stderr: "e", TimedOut: true}, nil
	}
	if _, err := h.ProvisionWithAgent(ctx, "dock-1", resSmall, "", "", nil); err != nil {
		t.Fatal(err)
	}
	res, err := h.FastExec(ctx, "dock-1", "yes", nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.OutputLimit != MaxExecOutput {
		t.Fatalf("exec must ask the guest to bound output, got %d", got.OutputLimit)
	}
	if len(res.Stdout) != MaxExecOutput || res.Stderr != "e" || !res.Truncated || !res.TimedOut || res.ExitCode != 124 {
		t.Fatalf("result = len %d %q truncated=%v timedOut=%v exit=%d", len(res.Stdout), res.Stderr, res.Truncated, res.TimedOut, res.ExitCode)
	}
}

func TestExecOnPlainDockIsExecUnavailable(t *testing.T) {
	ctx := context.Background()
	h, _, _ := testHost(t)
	if _, err := h.Provision(ctx, "dock-1", resSmall, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.FastExec(ctx, "dock-1", "true", nil, time.Second); !errors.Is(err, ErrExecUnavailable) {
		t.Fatalf("err = %v, want ErrExecUnavailable", err)
	}
}

func TestSnapshotLoadWithAgentRekeysResumedDock(t *testing.T) {
	ctx := context.Background()
	h, _, _ := testHost(t)
	var tokens []string
	h.cfg.AgentCall = func(_ context.Context, _, token string, _ fastAgentRequest) (fastAgentResponse, error) {
		tokens = append(tokens, token)
		return fastAgentResponse{OK: true}, nil
	}
	if _, err := h.ProvisionWithAgent(ctx, "dock-1", resSmall, "", "", nil); err != nil {
		t.Fatal(err)
	}
	first := tokens[len(tokens)-1]
	snap, _, _, err := h.SnapshotCreate(ctx, "dock-1", nil, apiv1.SnapshotFull, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.SnapshotLoadWithAgent(ctx, "dock-1", resSmall, snap); err != nil {
		t.Fatal(err)
	}
	if _, err := h.FastExec(ctx, "dock-1", "true", nil, time.Second); err != nil {
		t.Fatalf("resumed agent dock must accept exec: %v", err)
	}
	if last := tokens[len(tokens)-1]; last == first || len(last) != 64 {
		t.Fatalf("resume must mint a fresh token, first=%s last=%s", first, last)
	}
}

func TestPlainSnapshotLoadHasNoExecChannel(t *testing.T) {
	ctx := context.Background()
	h, _, _ := testHost(t)
	stubAgent(t, h)
	if _, err := h.ProvisionWithAgent(ctx, "dock-1", resSmall, "", "", nil); err != nil {
		t.Fatal(err)
	}
	snap, _, _, err := h.SnapshotCreate(ctx, "dock-1", nil, apiv1.SnapshotFull, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.SnapshotLoad(ctx, "dock-1", resSmall, snap); err != nil {
		t.Fatal(err)
	}
	if _, err := h.FastExec(ctx, "dock-1", "true", nil, time.Second); !errors.Is(err, ErrExecUnavailable) {
		t.Fatalf("err = %v, want ErrExecUnavailable", err)
	}
}

func TestContainerAgentDockExecsAcrossResume(t *testing.T) {
	ctx := context.Background()
	h, _, dir, _ := testContainerHost(t)
	stubAgent(t, h)
	if _, err := h.ProvisionWithAgent(ctx, "dock-1", resSmall, "", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.FastExec(ctx, "dock-1", "true", nil, time.Second); err != nil {
		t.Fatalf("gvisor agent dock must accept exec: %v", err)
	}
	snap, _, _, err := h.SnapshotCreate(ctx, "dock-1", nil, "full", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.SnapshotLoadWithAgent(ctx, "dock-1", resSmall, snap); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "jail", "firecracker", "dock-1", "root", "fast-agent.json")
	if _, err := os.Stat(config); err != nil {
		t.Fatalf("resume must stage the agent config for the sidecar: %v", err)
	}
	if _, err := h.FastExec(ctx, "dock-1", "true", nil, time.Second); err != nil {
		t.Fatalf("resumed gvisor agent dock must accept exec: %v", err)
	}
	if kind := h.Running()[0].Kind; kind != "dock" {
		t.Fatalf("agent dock kind = %s, want dock", kind)
	}
}

func TestRescanRestoresExecForAgentDocksOnly(t *testing.T) {
	ctx := context.Background()
	h, _, dir, _ := testContainerHost(t)
	stubAgent(t, h)
	if _, err := h.ProvisionWithAgent(ctx, "dock-1", resSmall, "", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ProvisionFast(ctx, "run-1", resSmall, "", "tok", "/home/user/work", nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"dock-1", "run-1"} {
		if err := os.WriteFile(filepath.Join(dir, "jail", "firecracker", id, "root", "spec.json"), []byte(`{"dockId":"`+id+`","resources":{"cpus":1,"memoryMiB":2048,"diskGiB":10}}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h.mu.Lock()
	h.vms = map[string]*vm{}
	h.mu.Unlock()
	h.Rescan()
	if _, err := h.FastExec(ctx, "dock-1", "true", nil, time.Second); err != nil {
		t.Fatalf("restarted docklet must keep exec on agent docks: %v", err)
	}
	if _, err := h.FastExec(ctx, "run-1", "true", nil, time.Second); !errors.Is(err, ErrExecUnavailable) {
		t.Fatalf("ephemeral runtimes must not be re-attached, err=%v", err)
	}
	raw, err := os.Stat(filepath.Join(dir, "jail", "firecracker", "dock-1", "root", execAgentSidecar))
	if err != nil || raw.Mode().Perm() != 0o600 {
		t.Fatalf("agent sidecar must be owner-only, got %v %v", raw, err)
	}
}
