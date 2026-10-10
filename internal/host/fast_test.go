package host

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startAgentProgram runs the real guest-agent program on port against a
// scratch config and returns a one-shot call helper. Exec runs without the
// runuser privilege drop so the test needs no root.
func startAgentProgram(t *testing.T, port string) func(req map[string]any) map[string]any {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(configPath, []byte(`{"token":"t","workspace":".","roots":["."]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	program := strings.ReplaceAll(fastAgentProgram, "/etc/dock-fast-agent.json", configPath)
	program = strings.Replace(program, "2222", port, 1)
	programPath := filepath.Join(dir, "agent.py")
	if err := os.WriteFile(programPath, []byte(program), 0o644); err != nil {
		t.Fatal(err)
	}
	proc := exec.Command(python, programPath)
	proc.Dir = dir
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proc.Process.Kill(); _ = proc.Wait() })
	return func(req map[string]any) map[string]any {
		t.Helper()
		var conn net.Conn
		var err error
		for i := 0; i < 50; i++ {
			conn, err = net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second)
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("agent never listened: %v", err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
		raw, _ := json.Marshal(req)
		if _, err := conn.Write(append(raw, '\n')); err != nil {
			t.Fatal(err)
		}
		var res map[string]any
		if err := json.NewDecoder(conn).Decode(&res); err != nil {
			t.Fatal(err)
		}
		return res
	}
}

// TestFastAgentProgramPingRekey: ping answers, rekey rotates the token, the
// old token dies. This catches guest-side breakage that Go unit tests cannot
// see (e.g. a misplaced global statement).
func TestFastAgentProgramPingRekey(t *testing.T) {
	agent := startAgentProgram(t, "18222")
	call := func(token, op string, extra map[string]string) map[string]any {
		req := map[string]any{"token": token, "op": op}
		for k, v := range extra {
			req[k] = v
		}
		return agent(req)
	}
	if res := call("t", "ping", nil); res["ok"] != true {
		t.Fatalf("ping = %v", res)
	}
	if res := call("t", "rekey", map[string]string{"newToken": "t2"}); res["ok"] != true {
		t.Fatalf("rekey = %v", res)
	}
	if res := call("t2", "ping", nil); res["ok"] != true {
		t.Fatalf("ping with new token = %v", res)
	}
	if res := call("t", "ping", nil); res["ok"] != false {
		t.Fatalf("old token must die, got %v", res)
	}
	if res := call("t2", "rekey", nil); res["ok"] != false {
		t.Fatalf("rekey without token must fail, got %v", res)
	}
	fmt.Println("agent protocol ok")
}

func TestFastAgentProgramExec(t *testing.T) {
	agent := startAgentProgram(t, "18223")
	res := agent(map[string]any{"token": "t", "op": "exec", "script": "echo out; echo err >&2; exit 3", "timeoutSeconds": 10})
	if res["exitCode"] != float64(3) || res["stdout"] != "out\n" || res["stderr"] != "err\n" || res["truncated"] != false || res["timedOut"] != false {
		t.Fatalf("exec = %v", res)
	}
	res = agent(map[string]any{"token": "t", "op": "exec", "script": "head -c 5000 /dev/zero | tr '\\0' x", "timeoutSeconds": 10, "outputLimit": 100})
	if out, _ := res["stdout"].(string); len(out) != 100 || res["truncated"] != true {
		t.Fatalf("output must clip to the limit, got len %d truncated=%v", len(out), res["truncated"])
	}
	// A slow exec must not block other connections; the agent serves them concurrently.
	done := make(chan map[string]any, 1)
	go func() {
		done <- agent(map[string]any{"token": "t", "op": "exec", "script": "echo started; sleep 30 & sleep 30", "timeoutSeconds": 1})
	}()
	time.Sleep(200 * time.Millisecond)
	if ping := agent(map[string]any{"token": "t", "op": "ping"}); ping["ok"] != true {
		t.Fatalf("ping during exec = %v", ping)
	}
	started := time.Now()
	res = <-done
	if res["timedOut"] != true || res["exitCode"] != float64(124) || res["stdout"] != "started\n" {
		t.Fatalf("timeout = %v", res)
	}
	// The whole process group dies, so a background child cannot pin the pipes.
	if waited := time.Since(started); waited > 5*time.Second {
		t.Fatalf("timeout reply took %s", waited)
	}
}
