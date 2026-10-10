package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func mustExit(t *testing.T, te *testEnv, want int, args ...string) {
	t.Helper()
	if got := te.run(args...); got != want {
		t.Fatalf("dock %v: exit %d, want %d\nstdout: %s\nstderr: %s", args, got, want, te.out, te.errb)
	}
}

func jsonOut(t *testing.T, te *testEnv) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(te.out.Bytes(), &v); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, te.out)
	}
	return v
}

func TestNew(t *testing.T) {
	te := newEnv(t)
	mustExit(t, te, 0, "new", "box", "--cpus", "4", "--memory", "8gb", "--disk", "40gb", "--image", "ubuntu-24.04", "--env", "prod")
	r := te.api.last()
	if r.Method != "POST" || r.URI != "/v1/docks" || r.Auth != "Bearer dock_testkey" {
		t.Fatalf("request %+v", r)
	}
	want := map[string]any{"name": "box", "cpus": 4.0, "memoryMiB": 8192.0, "diskGiB": 40.0, "image": "ubuntu-24.04", "environment": "prod", "workspaceId": wsID}
	if len(r.Body) != len(want) {
		t.Fatalf("body %v", r.Body)
	}
	for k, v := range want {
		if r.Body[k] != v {
			t.Errorf("body[%s] = %v, want %v", k, r.Body[k], v)
		}
	}
	if !strings.Contains(te.out.String(), "created box ("+dockA+")") {
		t.Fatalf("out %q", te.out)
	}
	// flags may follow the name or precede it, and --workspace skips discovery
	mustExit(t, te, 0, "new", "--workspace", "ws-explicit", "--json", "x")
	if te.api.last().Body["workspaceId"] != "ws-explicit" {
		t.Fatalf("body %v", te.api.last().Body)
	}
	if jsonOut(t, te).(map[string]any)["id"] != dockA {
		t.Fatal("json id")
	}
}

func TestNewBadSize(t *testing.T) {
	te := newEnv(t)
	mustExit(t, te, 2, "new", "--memory", "lots")
	mustExit(t, te, 2, "new", "--disk", "1000mb")
	mustExit(t, te, 2, "new", "--cpus", "0")
	mustExit(t, te, 2, "new", "a", "b")
	mustExit(t, te, 2, "new", "--bogus")
	if len(te.api.reqs) != 0 {
		t.Fatal("no request expected on usage errors")
	}
}

func TestLs(t *testing.T) {
	te := newEnv(t)
	mustExit(t, te, 0, "ls", "--all")
	if te.api.last().URI != "/v1/docks?all=true" {
		t.Fatalf("uri %s", te.api.last().URI)
	}
	for _, s := range []string{"web", "db", "ready", "archived", "4gb", "20gb"} {
		if !strings.Contains(te.out.String(), s) {
			t.Errorf("missing %q in %q", s, te.out)
		}
	}
	mustExit(t, te, 0, "ls", "--filter", "r")
	if te.api.last().URI != "/v1/docks?filter=r" {
		t.Fatalf("uri %s", te.api.last().URI)
	}
	mustExit(t, te, 0, "ls", "--json")
	if len(jsonOut(t, te).([]any)) != 2 {
		t.Fatal("json list")
	}
}

func TestExec(t *testing.T) {
	te := newEnv(t)
	mustExit(t, te, 0, "exec", dockA, "--", "echo", "hi there")
	r := te.api.last()
	if r.URI != "/v1/docks/"+dockA+"/exec" || r.Body["command"] != "echo 'hi there'" {
		t.Fatalf("request %+v", r)
	}
	if te.out.String() != "hello\n" {
		t.Fatalf("out %q", te.out)
	}
	// a name is resolved through the list
	mustExit(t, te, 0, "exec", "web", "--timeout", "5", "--cwd", "/tmp", "--", "ls -la | wc -l")
	r = te.api.last()
	if r.Body["command"] != "ls -la | wc -l" || r.Body["timeoutSeconds"] != 5.0 || r.Body["cwd"] != "/tmp" {
		t.Fatalf("body %v", r.Body)
	}
	mustExit(t, te, 0, "exec", "web", "--json", "--", "true")
	if jsonOut(t, te).(map[string]any)["exitCode"] != 0.0 {
		t.Fatal("json exit")
	}
	mustExit(t, te, 2, "exec", "web")
	mustExit(t, te, 2, "exec", "--", "ls")
}

func TestExecExitCodeAndStderr(t *testing.T) {
	te := newEnv(t)
	te.api.routes["POST /v1/docks/"+dockA+"/exec"] = func(map[string]any) (int, any) {
		return 200, map[string]any{"exitCode": 7, "stdout": "o", "stderr": "e", "durationMs": 1, "truncated": true}
	}
	mustExit(t, te, 7, "exec", dockA, "--", "false")
	if te.out.String() != "o" || !strings.Contains(te.errb.String(), "e") || !strings.Contains(te.errb.String(), "truncated") {
		t.Fatalf("out %q err %q", te.out, te.errb)
	}
	mustExit(t, te, 7, "exec", dockA, "--json", "--", "false")
	jsonOut(t, te)
}

func TestSSH(t *testing.T) {
	te := newEnv(t)
	mustExit(t, te, 0, "ssh", "web", "--print")
	if got := strings.TrimSpace(te.out.String()); got != "ssh -p 2222 "+dockA+"@dock.example.com" {
		t.Fatalf("out %q", got)
	}
	mustExit(t, te, 0, "ssh", dockA, "--json")
	if jsonOut(t, te).(map[string]any)["host"] != "dock.example.com" {
		t.Fatal("json host")
	}
	var argv []string
	te.app.RunSSH = func(_ context.Context, a []string) error { argv = a; return nil }
	mustExit(t, te, 0, "ssh", dockA)
	if len(argv) != 4 || argv[0] != "ssh" || argv[3] != dockA+"@dock.example.com" {
		t.Fatalf("argv %v", argv)
	}
	te.api.routes["GET /v1/config"] = func(map[string]any) (int, any) { return 200, map[string]any{"ssh": nil} }
	mustExit(t, te, 1, "ssh", dockA)
	if !strings.Contains(te.errb.String(), "error: ssh_unavailable:") {
		t.Fatalf("err %q", te.errb)
	}
}

func TestPort(t *testing.T) {
	te := newEnv(t)
	mustExit(t, te, 0, "port", "web", "3000")
	r := te.api.last()
	if r.URI != "/v1/docks/"+dockA+"/ports" || r.Body["port"] != 3000.0 {
		t.Fatalf("req %+v", r)
	}
	if strings.TrimSpace(te.out.String()) != "https://3000-abc.ports.example.com" {
		t.Fatalf("out %q", te.out)
	}
	mustExit(t, te, 0, "port", "web", "3000", "--json")
	jsonOut(t, te)
	mustExit(t, te, 0, "port", "web", "3000", "--rm")
	if te.api.last().Method != "DELETE" {
		t.Fatal("expected DELETE")
	}
	mustExit(t, te, 0, "port", "web", "3000", "--rm", "--json")
	jsonOut(t, te)
	mustExit(t, te, 2, "port", "web", "70000")
	mustExit(t, te, 2, "port", "web")
}

func TestStopResume(t *testing.T) {
	te := newEnv(t)
	mustExit(t, te, 0, "stop", "web", "--force")
	if r := te.api.last(); r.URI != "/v1/docks/"+dockA+"/stop" || r.Body["force"] != true {
		t.Fatalf("req %+v", r)
	}
	if !strings.Contains(te.out.String(), "stopped web state=archived") {
		t.Fatalf("out %q", te.out)
	}
	mustExit(t, te, 0, "stop", "web", "--json")
	if jsonOut(t, te).(map[string]any)["state"] != "archived" {
		t.Fatal("json")
	}
	mustExit(t, te, 0, "resume", dockA)
	if !strings.Contains(te.out.String(), "resumed") || te.api.last().URI != "/v1/docks/"+dockA+"/resume" {
		t.Fatalf("out %q", te.out)
	}
	mustExit(t, te, 0, "resume", dockA, "--json")
	jsonOut(t, te)
}

func TestBranch(t *testing.T) {
	te := newEnv(t)
	mustExit(t, te, 0, "branch", "web", "--name", "w2", "--cpus", "1")
	if r := te.api.last(); r.URI != "/v1/docks/"+dockA+"/branch" || r.Body["name"] != "w2" || r.Body["cpus"] != 1.0 {
		t.Fatalf("req %+v", r)
	}
	mustExit(t, te, 0, "branch", "web", "--json")
	if _, ok := jsonOut(t, te).(map[string]any); !ok {
		t.Fatal("single branch prints an object")
	}
	n := 0
	for _, r := range te.api.reqs {
		if strings.HasSuffix(r.URI, "/branch") {
			n++
		}
	}
	mustExit(t, te, 0, "branch", "web", "--count", "3", "--json")
	if len(jsonOut(t, te).([]any)) != 3 {
		t.Fatal("count 3 prints an array of 3")
	}
	mustExit(t, te, 0, "branch", "web", "--count", "2")
	if strings.Count(te.out.String(), "branched web") != 2 {
		t.Fatalf("out %q", te.out)
	}
	mustExit(t, te, 2, "branch", "web", "--count", "0")
	mustExit(t, te, 2, "branch", "web", "--count", "2", "--name", "x")
	_ = n
}

func TestRm(t *testing.T) {
	te := newEnv(t)
	mustExit(t, te, 0, "rm", "web")
	r := te.api.last()
	if r.Method != "DELETE" || r.URI != "/v1/docks/"+dockA || r.Confirm != dockA {
		t.Fatalf("req %+v", r)
	}
	mustExit(t, te, 0, "rm", dockA, "--json")
	if _, ok := jsonOut(t, te).(map[string]any)["operation"]; !ok {
		t.Fatal("operation in json")
	}
}

func TestErrorEnvelope(t *testing.T) {
	te := newEnv(t)
	te.api.routes["POST /v1/docks"] = func(map[string]any) (int, any) {
		return 409, apiErr{"dock_name_taken", "Dock name already in use"}
	}
	mustExit(t, te, 1, "new", "web")
	if te.errb.String() != "error: dock_name_taken: Dock name already in use\n" {
		t.Fatalf("err %q", te.errb)
	}
	mustExit(t, te, 1, "stop", "nope")
	if !strings.HasPrefix(te.errb.String(), "error: dock_not_found:") {
		t.Fatalf("err %q", te.errb)
	}
	// even with --json the failure goes to stderr and stdout stays empty
	mustExit(t, te, 1, "rm", "missing", "--json")
	if te.out.Len() != 0 {
		t.Fatalf("stdout %q", te.out)
	}
	// non-envelope upstream failures still carry a code
	te.api.routes["GET /v1/docks"] = func(map[string]any) (int, any) { return 500, apiErr{"", ""} }
	mustExit(t, te, 1, "ls")
	if !strings.HasPrefix(te.errb.String(), "error: http_500:") {
		t.Fatalf("err %q", te.errb)
	}
}

func TestAmbiguousName(t *testing.T) {
	te := newEnv(t)
	te.api.routes["GET /v1/docks"] = func(map[string]any) (int, any) {
		return 200, []any{map[string]any{"id": dockA, "name": "dup"}, map[string]any{"id": dockB, "name": "dup"}}
	}
	mustExit(t, te, 1, "stop", "dup")
	if !strings.Contains(te.errb.String(), "ambiguous_dock") {
		t.Fatalf("err %q", te.errb)
	}
}

func TestUsage(t *testing.T) {
	te := newEnv(t)
	mustExit(t, te, 2)
	mustExit(t, te, 2, "frobnicate")
	mustExit(t, te, 0, "help")
	if !strings.Contains(te.out.String(), "not released") {
		t.Fatal("help marks the CLI as early")
	}
	mustExit(t, te, 0, "ls", "-h")
}

func TestNoAPIKey(t *testing.T) {
	te := newEnv(t)
	delete(te.env, "DOCK_API_KEY")
	mustExit(t, te, 1, "ls")
	if !strings.HasPrefix(te.errb.String(), "error: no_api_key:") {
		t.Fatalf("err %q", te.errb)
	}
}

func TestEveryJSONCommandIsCovered(t *testing.T) {
	// guards the claim that --json works everywhere by running each command once
	te := newEnv(t)
	cmds := [][]string{
		{"new", "--json"}, {"ls", "--json"}, {"exec", "web", "--json", "--", "x"}, {"ssh", "web", "--json"},
		{"port", "web", "3000", "--json"}, {"stop", "web", "--json"}, {"resume", "web", "--json"},
		{"branch", "web", "--json"}, {"rm", "web", "--json"}, {"context", "ls", "--json"},
	}
	for _, c := range cmds {
		if got := te.run(c...); got != 0 {
			t.Fatalf("%v exit %d: %s", c, got, te.errb)
		}
		var v any
		if err := json.Unmarshal(te.out.Bytes(), &v); err != nil {
			t.Fatalf("%s: %v", fmt.Sprint(c), err)
		}
	}
}
